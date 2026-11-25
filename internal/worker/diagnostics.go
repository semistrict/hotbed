package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/profiler"
)

func (e *Engine) profilePath(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return filepath.Join(e.cfg.Dir, "profiles", id+".pb.gz"), nil
}

// pruneProfiles removes the profile files of commands the engine forgot
// that are older than keepProfiles: those the controller may never have
// got, which forget leaves (a worker restarting, the controller out of
// reach when it forgot them).
func (e *Engine) pruneProfiles() {
	e.mu.Lock()
	known := make(map[string]bool, len(e.commands))
	for id := range e.commands {
		known[id] = true
	}
	e.mu.Unlock()
	if err := removeStaleProfiles(filepath.Join(e.cfg.Dir, "profiles"), e.now().Add(-keepProfiles), known); err != nil {
		slog.Error("prune command profiles", "err", err)
	}
}

// removeStaleProfiles removes the profile files in dir last written before
// before, except those of known commands.
func removeStaleProfiles(dir string, before time.Time, known map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, ent := range entries {
		id, ok := strings.CutSuffix(ent.Name(), ".pb.gz")
		if !ok || known[id] {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if info.ModTime().Before(before) {
			if err := os.Remove(filepath.Join(dir, ent.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

type liveCommandRoute struct {
	wsID     string
	isVM     bool
	unitName string
	cgroup   string
}

// resolveLiveCommand validates that id refers to a live (started, non-ended)
// command and reports whether it runs inside a guest VM workspace.
func (e *Engine) resolveLiveCommand(id string) (liveCommandRoute, error) {
	if err := validateID(id); err != nil {
		return liveCommandRoute{}, err
	}
	e.mu.Lock()
	c, ok := e.commands[id]
	if !ok {
		e.mu.Unlock()
		return liveCommandRoute{}, notFound("command", id)
	}
	cmd := c.cmd()
	state := cmd.State
	wsID := cmd.WorkspaceId
	unitName := c.unitName()
	cgroup := c.rec.Cgroup
	e.mu.Unlock()

	isVM := wsID != "" && e.usesRuntime(wsID)
	if state == queued {
		return liveCommandRoute{}, api.Errorf(api.CodeConflict, "command %s is still queued and has not started yet", id)
	}
	if !api.Live(state) {
		return liveCommandRoute{}, api.Errorf(api.CodeConflict, "command %s has already ended (%s); live inspection only applies while running", id, state).
			WithSuggestion("use `hotbed profile " + id + "` to query its always-on OpenTelemetry eBPF profile")
	}
	return liveCommandRoute{
		wsID:     wsID,
		isVM:     isVM,
		unitName: unitName,
		cgroup:   cgroup,
	}, nil
}

// hostInspectTarget builds an inspect.Target for a live host/chroot command.
func (e *Engine) hostInspectTarget(ctx context.Context, id string, route liveCommandRoute) inspect.Target {
	cgroup := route.cgroup
	var mainPID int
	if st, err := e.cfg.Units.Status(ctx, route.unitName); err == nil && st.Loaded {
		mainPID = st.PID
		if st.Cgroup != "" {
			cgroup = st.Cgroup
		}
	}
	t := inspect.Target{
		CommandID: id,
		UnitName:  route.unitName,
		Cgroup:    cgroup,
		MainPID:   mainPID,
		Hostname:  e.cfg.Host,
		Arch:      runtime.GOARCH,
	}
	if e.cfg.Profiler != nil {
		t.ProfileWindow = func(fromMs, toMs int64) *pb.CommandProfile {
			return e.cfg.Profiler.Query(id, &pb.GetProfileRequest{
				Target: &pb.GetProfileRequest_CommandId{CommandId: id},
				FromMs: fromMs,
				ToMs:   toMs,
				Limit:  100,
			}, true)
		}
	}
	return t
}

func (e *Engine) wrapVMSampleEmit(wsID string, emit inspect.SampleEmit) inspect.SampleEmit {
	if e.cfg.Host == "" {
		return emit
	}
	label := fmt.Sprintf("%s (vm:%s)", e.cfg.Host, wsID)
	return func(ch *pb.SampleCommandChunk) error {
		if ch != nil && ch.Sample != nil {
			ch.Sample.Hostname = label
		}
		return emit(ch)
	}
}

// Inspect takes an opt-in snapshot of a live command's processes, open files,
// cgroup metrics, kernel stacks, memory maps, and limits.
func (e *Engine) Inspect(ctx context.Context, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	route, err := e.resolveLiveCommand(req.Id)
	if err != nil {
		if e.cfg.Inspector == nil && e.cfg.Runtime == nil {
			return nil, api.Errorf(api.CodePlatformNotSupported, "process inspection is not configured on this worker")
		}
		return nil, err
	}
	if route.isVM {
		return e.cfg.Runtime.InspectCommand(ctx, route.wsID, req)
	}
	if e.cfg.Inspector == nil {
		return nil, api.Errorf(api.CodePlatformNotSupported, "process inspection is not configured on this worker")
	}
	return e.cfg.Inspector.Inspect(ctx, e.hostInspectTarget(ctx, req.Id, route), req)
}

// Trace runs an opt-in active Linux trace (strace, perf, stacks, bpftrace) on
// a live command and streams progress/results via emit.
func (e *Engine) Trace(ctx context.Context, req *pb.TraceCommandRequest, emit inspect.Emit) error {
	route, err := e.resolveLiveCommand(req.Id)
	if err != nil {
		if e.cfg.Inspector == nil && e.cfg.Runtime == nil {
			return api.Errorf(api.CodePlatformNotSupported, "process tracing is not configured on this worker")
		}
		return err
	}
	if route.isVM {
		return e.cfg.Runtime.TraceCommand(ctx, route.wsID, req, emit)
	}
	if e.cfg.Inspector == nil {
		return api.Errorf(api.CodePlatformNotSupported, "process tracing is not configured on this worker")
	}
	return e.cfg.Inspector.Trace(ctx, e.hostInspectTarget(ctx, req.Id, route), req, emit)
}

// SampleCommand takes a comprehensive Activity Monitor-style Linux activity sample of
// a live command and streams progress plus the final report via emit.
func (e *Engine) SampleCommand(ctx context.Context, req *pb.SampleCommandRequest, emit inspect.SampleEmit) error {
	route, err := e.resolveLiveCommand(req.Id)
	if err != nil {
		if e.cfg.Inspector == nil && e.cfg.Runtime == nil {
			return api.Errorf(api.CodePlatformNotSupported, "process sampling is not configured on this worker")
		}
		return err
	}
	if route.isVM {
		return e.cfg.Runtime.SampleCommand(ctx, route.wsID, req, e.wrapVMSampleEmit(route.wsID, emit))
	}
	if e.cfg.Inspector == nil {
		return api.Errorf(api.CodePlatformNotSupported, "process sampling is not configured on this worker")
	}
	return e.cfg.Inspector.Sample(ctx, e.hostInspectTarget(ctx, req.Id, route), req, emit)
}

// CommandProfile queries the always-on OpenTelemetry eBPF continuous profile
// for a live or finished command on this worker.
func (e *Engine) CommandProfile(id string, req *pb.GetProfileRequest) (*pb.CommandProfile, error) {
	path, err := e.profilePath(id)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	c, ok := e.commands[id]
	isLive := ok && api.Live(c.cmd().State)
	wsID := ""
	if ok {
		wsID = c.cmd().WorkspaceId
	}
	e.mu.Unlock()
	isVM := wsID != "" && e.usesRuntime(wsID)

	if isLive {
		if isVM {
			pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			prof, err := e.cfg.Runtime.CommandProfile(pctx, wsID, req)
			if err == nil {
				return prof, nil
			}
			return &pb.CommandProfile{
				CommandId:  id,
				Profiler:   profiler.EngineName,
				Live:       true,
				Active:     false,
				StatusNote: err.Error(),
			}, nil
		}
		if e.cfg.Profiler == nil {
			return &pb.CommandProfile{
				CommandId:  id,
				Profiler:   profiler.EngineName,
				Live:       true,
				Active:     false,
				StatusNote: "continuous profiler not configured on this worker",
			}, nil
		}
		return e.cfg.Profiler.Query(id, req, true), nil
	}

	// Command ended (or forgotten): check saved profile on disk first.
	if saved, err := profiler.LoadProfile(path); err == nil {
		saved.Live = false
		return profiler.QueryProfile(saved, req), nil
	} else if !profiler.IsNotExist(err) {
		return nil, err
	}

	if ok && isVM {
		pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if prof, err := e.cfg.Runtime.CommandProfile(pctx, wsID, req); err == nil {
			prof.Live = false
			return prof, nil
		} else {
			return &pb.CommandProfile{
				CommandId:  id,
				Profiler:   profiler.EngineName,
				Live:       false,
				Active:     false,
				StatusNote: err.Error(),
			}, nil
		}
	}
	// Command recently ended and may still have samples in memory.
	if ok && e.cfg.Profiler != nil {
		return e.cfg.Profiler.Query(id, req, false), nil
	}
	if ok {
		return &pb.CommandProfile{
			CommandId:  id,
			Profiler:   profiler.EngineName,
			Live:       false,
			Active:     false,
			StatusNote: "continuous profiler not configured on this worker",
		}, nil
	}
	return nil, notFound("command", id)
}

// NodeProfile queries the always-on OpenTelemetry eBPF continuous profile ring
// buffer collected across all processes on this worker node.
func (e *Engine) NodeProfile(node string, req *pb.GetProfileRequest) (*pb.CommandProfile, error) {
	host := e.cfg.Host
	if host == "" {
		host = node
	}
	if e.cfg.Profiler == nil {
		return &pb.CommandProfile{
			Node:       host,
			Profiler:   profiler.EngineName,
			Live:       true,
			Active:     false,
			StatusNote: "continuous profiler not configured on this worker",
		}, nil
	}
	return e.cfg.Profiler.QueryNode(host, req), nil
}
