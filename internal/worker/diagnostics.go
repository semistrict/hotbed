package worker

import (
	"context"
	"path/filepath"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/profiler"
)

func (e *Engine) profilePath(id string) string {
	return filepath.Join(e.cfg.Dir, "profiles", id+".pb.gz")
}

// commandTarget resolves a live command's systemd unit, cgroup, and main PID
// for on-demand Linux inspection or active tracing.
func (e *Engine) commandTarget(ctx context.Context, id string) (inspect.Target, error) {
	e.mu.Lock()
	c, ok := e.commands[id]
	if !ok {
		e.mu.Unlock()
		return inspect.Target{}, notFound("command", id)
	}
	state := c.cmd().State
	unitName := c.unitName()
	cgroup := c.rec.Cgroup
	e.mu.Unlock()

	if state == queued {
		return inspect.Target{}, api.Errorf(api.CodeConflict, "command %s is still queued and has not started yet", id)
	}
	if !live(state) {
		return inspect.Target{}, api.Errorf(api.CodeConflict, "command %s has already ended (%s); live inspection only applies while running", id, state).
			WithSuggestion("use `hotbed profile " + id + "` to query its always-on OpenTelemetry eBPF profile")
	}

	var mainPID int
	if st, err := e.cfg.Units.Status(ctx, unitName); err == nil && st.Loaded {
		mainPID = st.PID
		if st.Cgroup != "" {
			cgroup = st.Cgroup
		}
	}
	return inspect.Target{
		CommandID: id,
		UnitName:  unitName,
		Cgroup:    cgroup,
		MainPID:   mainPID,
	}, nil
}

// Inspect takes an opt-in snapshot of a live command's processes, open files,
// cgroup metrics, kernel stacks, memory maps, and limits.
func (e *Engine) Inspect(ctx context.Context, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	if e.cfg.Inspector == nil {
		return nil, api.Errorf(api.CodePlatformNotSupported, "process inspection is not configured on this worker")
	}
	target, err := e.commandTarget(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return e.cfg.Inspector.Inspect(ctx, target, req)
}

// Trace runs an opt-in active Linux trace (strace, perf, stacks, bpftrace) on
// a live command and streams progress/results via emit.
func (e *Engine) Trace(ctx context.Context, req *pb.TraceCommandRequest, emit inspect.Emit) error {
	if e.cfg.Inspector == nil {
		return api.Errorf(api.CodePlatformNotSupported, "process tracing is not configured on this worker")
	}
	target, err := e.commandTarget(ctx, req.Id)
	if err != nil {
		return err
	}
	return e.cfg.Inspector.Trace(ctx, target, req, emit)
}

// CommandProfile queries the always-on OpenTelemetry eBPF continuous profile
// for a live or finished command on this worker.
func (e *Engine) CommandProfile(req *pb.GetCommandProfileRequest) (*pb.CommandProfile, error) {
	e.mu.Lock()
	c, ok := e.commands[req.Id]
	isLive := ok && live(c.cmd().State)
	e.mu.Unlock()

	if isLive {
		if e.cfg.Profiler == nil {
			return &pb.CommandProfile{
				CommandId:  req.Id,
				Profiler:   profiler.EngineName,
				Live:       true,
				Active:     false,
				StatusNote: "continuous profiler not configured on this worker",
			}, nil
		}
		return e.cfg.Profiler.Query(req.Id, req, true), nil
	}

	// Command ended (or forgotten): check saved profile on disk first.
	if saved, err := profiler.LoadProfile(e.profilePath(req.Id)); err == nil {
		saved.Live = false
		return profiler.QueryProfile(saved, req), nil
	} else if !profiler.IsNotExist(err) {
		return nil, err
	}

	// Command recently ended and may still have samples in memory.
	if ok && e.cfg.Profiler != nil {
		return e.cfg.Profiler.Query(req.Id, req, false), nil
	}
	if ok {
		return &pb.CommandProfile{
			CommandId:  req.Id,
			Profiler:   profiler.EngineName,
			Live:       false,
			Active:     false,
			StatusNote: "continuous profiler not configured on this worker",
		}, nil
	}
	return nil, notFound("command", req.Id)
}
