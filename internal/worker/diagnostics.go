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
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

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
	wsID := c.runtimeTarget()
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
// a live command and streams progress plus the final report via emit. The
// final sample is kept with the command (keepSample) before it is emitted;
// host is where the call came from.
func (e *Engine) SampleCommand(ctx context.Context, req *pb.SampleCommandRequest, host string, emit inspect.SampleEmit) error {
	route, err := e.resolveLiveCommand(req.Id)
	if err != nil {
		if e.cfg.Inspector == nil && e.cfg.Runtime == nil {
			return api.Errorf(api.CodePlatformNotSupported, "process sampling is not configured on this worker")
		}
		return err
	}
	by := &pb.Stamp{At: e.nowMs(), Session: req.SessionId, Host: host}
	keep := func(ch *pb.SampleCommandChunk) error {
		if ch != nil && ch.Done && ch.Sample != nil {
			e.keepSample(req, by, ch)
		}
		return emit(ch)
	}
	if route.isVM {
		return e.cfg.Runtime.SampleCommand(ctx, route.wsID, req, e.wrapVMSampleEmit(route.wsID, keep))
	}
	if e.cfg.Inspector == nil {
		return api.Errorf(api.CodePlatformNotSupported, "process sampling is not configured on this worker")
	}
	return e.cfg.Inspector.Sample(ctx, e.hostInspectTarget(ctx, req.Id, route), req, keep)
}

// maxSampleBytes bounds a kept activity sample, as maxDiagnosticsBytes does
// diagnostics: the controller stores each in one row.
const maxSampleBytes = 1 << 20

// keepSample numbers the final sample ch carries after its command's
// others, trims it to maxSampleBytes, and saves it with the command for
// the controller (SaveCommandSample), all before ch goes to whoever asked:
// ch then carries the sample as kept and how it was kept, or why it was not.
func (e *Engine) keepSample(req *pb.SampleCommandRequest, by *pb.Stamp, ch *pb.SampleCommandChunk) {
	cs := &pb.CommandSample{
		CommandId:       req.Id,
		By:              by,
		DurationSeconds: int32(inspect.ClampSampleDuration(req.DurationSeconds)),
		Pid:             req.Pid,
		Sample:          ch.Sample,
	}
	if proto.Size(cs) > maxSampleBytes {
		cs = proto.Clone(cs).(*pb.CommandSample)
		// Headroom for the seq this numbers it with below.
		const headroom = 16
		fits := func() bool { return proto.Size(cs) <= maxSampleBytes-headroom }
		if trimmed := trimToFit(fits, cs.Sample, nil); len(trimmed) > 0 {
			cs.Truncated = "dropped " + strings.Join(trimmed, ", ") + " to fit 1 MiB"
		}
		if !fits() {
			ch.NotKept = fmt.Sprintf("it is %d bytes even trimmed, past the %d it may be", proto.Size(cs), maxSampleBytes)
			return
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[req.Id]
	if !ok || !api.Live(c.cmd().State) {
		ch.NotKept = "its command ended as it was sampled"
		return
	}
	cmd := c.cmd()
	cmd.Samples++
	cs.Seq = cmd.Samples
	e.saveCommand(c, change{what: "sampled", out: outbox{sample: cs}})
	ch.Sample = cs.Sample
	ch.Kept = proto.CloneOf(cs)
	ch.Kept.Sample = nil
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
		wsID = c.runtimeTarget()
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

// snapshotVMProfile synchronously fetches and forgets a finished VM command's
// in-guest eBPF profile outside e.mu before endBy marks the command ended, so
// fast session teardown cannot race the vsock snapshot.
func (e *Engine) snapshotVMProfile(c *command) {
	e.mu.Lock()
	if c.vmProfileDone {
		e.mu.Unlock()
		return
	}
	c.vmProfileDone = true
	cmd := c.cmd()
	wsID, cmdID, isFreshen := c.runtimeTarget(), cmd.Id, c.isFreshen()
	e.mu.Unlock()

	if isFreshen || wsID == "" || !e.usesRuntime(wsID) {
		return
	}
	pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	snap, err := e.cfg.Runtime.CommandProfile(pctx, wsID, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: cmdID},
		Limit:  500,
	})
	cancel()
	if err != nil || snap == nil || snap.TotalSamples == 0 {
		return
	}
	snap.Live = false
	e.mu.Lock()
	c.vmProfile = snap
	e.mu.Unlock()

	fctx, fcancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = e.cfg.Runtime.ForgetCommandProfile(fctx, wsID, cmdID)
	fcancel()
}

// NodeProfile queries the always-on OpenTelemetry eBPF continuous profile ring
// buffer collected across all processes on this worker node, merging in live
// in-guest eBPF profiles from any active Firecracker VM workspaces.
func (e *Engine) NodeProfile(node string, req *pb.GetProfileRequest) (*pb.CommandProfile, error) {
	host := e.cfg.Host
	if host == "" {
		host = node
	}

	// Collect active VM workspaces so their in-guest OTel eBPF profiles can be
	// merged alongside host processes (firecracker, cladefsd, etc.).
	var vmWorkspaces []string
	if e.cfg.Runtime != nil {
		e.mu.Lock()
		for wsID, w := range e.workspaces {
			st := w.ws().State
			if st == released || st == failedWS || st == preparing {
				continue
			}
			if e.usesRuntime(wsID) {
				vmWorkspaces = append(vmWorkspaces, wsID)
			}
		}
		e.mu.Unlock()
	}

	var hostProf *pb.CommandProfile
	if e.cfg.Profiler == nil {
		hostProf = &pb.CommandProfile{
			Node:       host,
			Profiler:   profiler.EngineName,
			Live:       true,
			Active:     false,
			StatusNote: "continuous profiler not configured on this worker",
		}
	} else if len(vmWorkspaces) == 0 {
		return e.cfg.Profiler.QueryNode(host, req), nil
	} else {
		// Query unfiltered stacks for the requested time window so MergeProfiles
		// applies query/comm/frame-type/limit filters uniformly across host + VMs.
		hostProf = e.cfg.Profiler.QueryNode(host, &pb.GetProfileRequest{
			Target: &pb.GetProfileRequest_Node{Node: host},
			FromMs: req.GetFromMs(),
			ToMs:   req.GetToMs(),
			Limit:  500,
		})
	}

	if len(vmWorkspaces) == 0 {
		return hostProf, nil
	}

	guestProfs := make([]*pb.CommandProfile, len(vmWorkspaces))
	var wg sync.WaitGroup
	for i, wsID := range vmWorkspaces {
		wg.Go(func() {
			pctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			gp, err := e.cfg.Runtime.CommandProfile(pctx, wsID, &pb.GetProfileRequest{
				Target: &pb.GetProfileRequest_Node{Node: "vm:" + wsID},
				FromMs: req.GetFromMs(),
				ToMs:   req.GetToMs(),
				Limit:  500,
			})
			if err == nil && gp != nil && gp.TotalSamples > 0 {
				guestProfs[i] = gp
			}
		})
	}
	wg.Wait()

	var nonNil []*pb.CommandProfile
	for _, gp := range guestProfs {
		if gp != nil {
			nonNil = append(nonNil, gp)
		}
	}
	return profiler.MergeProfiles(host, hostProf, nonNil, req), nil
}
