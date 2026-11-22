package worker

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/shell"
	"github.com/semistrict/hotbed/internal/unit"
)

const (
	queued    = pb.CommandState_COMMAND_STATE_QUEUED
	running   = pb.CommandState_COMMAND_STATE_RUNNING
	parked    = pb.CommandState_COMMAND_STATE_PARKED
	succeeded = pb.CommandState_COMMAND_STATE_SUCCEEDED
	failed    = pb.CommandState_COMMAND_STATE_FAILED
	cancelled = pb.CommandState_COMMAND_STATE_CANCELLED
)

// cacheEnv puts everything a build would keep in the home directory inside
// the workspace, so a base carries it and every fork starts with it.
func cacheEnv() []string {
	h := WorkPath + "/.hotbed"
	return []string{
		"RUSTUP_HOME=" + h + "/rustup",
		"CARGO_HOME=" + h + "/cargo",
		"RUSTUP_AUTO_INSTALL=1",
		"GOPATH=" + h + "/go",
		"GOMODCACHE=" + h + "/go/mod",
		"GOCACHE=" + h + "/cache/go-build",
		"XDG_CACHE_HOME=" + h + "/cache",
		"XDG_DATA_HOME=" + h + "/data",
		"XDG_STATE_HOME=" + h + "/state",
		"COREPACK_HOME=" + h + "/corepack",
		"npm_config_cache=" + h + "/cache/npm",
		"CARGO_TERM_COLOR=never",
		// Where steps put the programs they install, so forks have them.
		"HOTBED_BIN=" + h + "/bin",
	}
}

var cachePath = []string{WorkPath + "/.hotbed/bin", WorkPath + "/.hotbed/cargo/bin", WorkPath + "/.hotbed/go/bin"}

// Exec queues a command in a workspace; it starts once the node admits it.
func (e *Engine) Exec(_ context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
	if len(req.Args) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "no command to run")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[req.WorkspaceId]
	if !ok || w.ws().State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", req.WorkspaceId)
	}
	if w.ws().State == failedWS {
		return nil, api.Errorf(api.CodeConflict, "workspace %s failed: %s", req.WorkspaceId, w.ws().Reason).
			WithSuggestion("hotbed add the repo again to try a new workspace")
	}
	// The controller names the commands it forwards; a client calling this
	// worker directly leaves that to the worker.
	id := req.Id
	if id == "" {
		id = newID()
	}
	if _, dup := e.commands[id]; dup {
		return nil, api.Errorf(api.CodeConflict, "command ID %q is taken", id)
	}
	if req.Bench && e.cfg.Bench == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "%s takes no measurements: its worker's config names no bench CPUs", e.cfg.Host).
			WithSuggestion("hotbed nodes shows the nodes that do (benchCpus); hotbed add --node one of them")
	}
	kind := req.Kind
	if kind == "" {
		kind = inferKind(req.Args)
	}
	limits := &pb.CommandLimits{}
	if req.Limits != nil {
		limits = proto.Clone(req.Limits).(*pb.CommandLimits)
	}
	if limits.CpuWeight == 0 {
		limits.CpuWeight = 100
	}
	now := e.nowMs()
	c := &command{
		rec: &pb.CommandRecord{
			Command: &pb.Command{
				Id: id, WorkspaceId: req.WorkspaceId, NodeId: e.cfg.NodeID,
				Argv: shell.Join(req.Args), Args: req.Args, Kind: kind,
				State: queued, QueuedAt: now, Limits: limits, Attached: req.Attached,
				SessionId: w.ws().SessionId, Bench: req.Bench,
				Timeline: []*pb.CommandEvent{{At: now, What: "queued", Detail: "by " + w.ws().Agent}},
			},
			RepoId: w.ws().RepoId, Agent: w.ws().Agent, Volume: w.rec.Volume,
			Dir: WorkPath + "/" + w.ws().RepoId, Env: req.Env,
		},
		lastFollow: e.now(),
	}
	if req.Port > 0 {
		c.cmd().Url = fmt.Sprintf("http://%s:%d", e.cfg.Host, req.Port)
	}
	if isBuild(kind) {
		if _, units, ok := e.coldBuild(w.ws().RepoId); ok {
			c.cmd().Rebuilt = &pb.Rebuilt{Of: int32(units)}
		}
	}
	w.ws().LastActiveAt = now
	if w.ws().State != preparing {
		w.ws().State = activeWS
	}
	e.saveWorkspace(w, change{what: "active", detail: "running " + id})
	e.commands[id] = c
	e.saveCommand(c, change{what: "queued", detail: "by " + w.ws().Agent})
	e.stepSoon()
	return clone(c.cmd()), nil
}

// newID is ten random base32 characters, lowercase: 50 bits, so commands
// workers name on their own do not meet across a cluster's history.
func newID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.EncodeToString(b))[:10]
}

func isBuild(kind string) bool { return kind == "build" || kind == "test" || kind == "check" }

// inferKind sorts a command by what it does, for history and the cockpit.
func inferKind(args []string) string {
	s := " " + strings.Join(args, " ") + " "
	switch {
	case strings.Contains(s, " test") || strings.Contains(s, "nextest") || strings.Contains(s, " vitest"):
		return "test"
	case strings.Contains(s, " clippy") || strings.Contains(s, " check") || strings.Contains(s, " lint") || strings.Contains(s, " vet"):
		return "check"
	case strings.Contains(s, " build"):
		return "build"
	case strings.Contains(s, " dev ") || strings.Contains(s, " serve") || strings.Contains(s, " run "):
		return "server"
	default:
		return "shell"
	}
}

// Stop ends a command: a queued one never starts, a running or parked one is
// stopped. It returns the command as it ended.
func (e *Engine) Stop(ctx context.Context, id, why string) (*pb.Command, error) {
	e.mu.Lock()
	c, ok := e.commands[id]
	if !ok {
		e.mu.Unlock()
		return nil, api.Errorf(api.CodeNotFound, "no command %s on this node", id)
	}
	state := c.cmd().State
	if state == queued {
		// It ends here, under the lock: admit, starting it meanwhile, finds
		// it ended and stops its unit.
		e.end(c, cancelled, nil, why)
		defer e.mu.Unlock()
		return clone(c.cmd()), nil
	}
	c.stopping = true
	e.mu.Unlock()
	if live(state) {
		if state == parked {
			// A frozen unit cannot be stopped until it is thawed.
			if err := e.cfg.Units.Thaw(ctx, c.unitName()); err != nil {
				slog.Warn("thaw before stop", "command", id, "err", err)
			}
		}
		if err := e.cfg.Units.Stop(ctx, c.unitName()); err != nil {
			e.mu.Lock()
			c.stopping = false
			e.mu.Unlock()
			return nil, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c.stopping = false
	e.end(c, cancelled, nil, why)
	return clone(c.cmd()), nil
}

// step moves every command along: reads running units, admits queued
// commands, parks or thaws for memory, stops orphaned attached commands.
func (e *Engine) step(ctx context.Context) {
	e.mu.Lock()
	var active []*command
	for _, c := range e.commands {
		if s := c.cmd().State; s == running || s == parked {
			active = append(active, c)
		}
	}
	e.mu.Unlock()
	for _, c := range active {
		st, err := e.cfg.Units.Status(ctx, c.unitName())
		if err != nil {
			slog.Warn("read command", "command", c.cmd().Id, "err", err)
			continue
		}
		e.observe(ctx, c, st)
	}
	e.settleBench(ctx)
	e.admit(ctx)
	e.park(ctx)
	e.reap(ctx)
	e.forget()
}

func (e *Engine) observe(ctx context.Context, c *command, st unit.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cmd := c.cmd()
	if !live(cmd.State) || c.stopping {
		return
	}
	now := e.now()
	if !st.Loaded {
		e.end(c, failed, nil, "its systemd unit disappeared")
		return
	}
	if st.Cgroup != "" {
		c.rec.Cgroup = st.Cgroup
	}
	if !c.cpuAt.IsZero() && st.CPUNanos >= c.cpuNanos {
		if dt := now.Sub(c.cpuAt).Seconds(); dt > 0 {
			cmd.CpuCores = float64(st.CPUNanos-c.cpuNanos) / 1e9 / dt
		}
	}
	c.cpuNanos, c.cpuAt = st.CPUNanos, now
	if st.Memory > 0 {
		cmd.MemoryBytes = st.Memory
		c.rec.PeakMemoryBytes = max(c.rec.PeakMemoryBytes, st.Memory)
	}
	if c.rec.Cgroup != "" {
		if swap, err := e.cfg.Units.Swap(c.rec.Cgroup); err == nil {
			cmd.SwapBytes = swap
		}
	}
	// While it runs and as it ends: the node is given back only after.
	e.checkIsolation(c)
	if !st.Exited {
		return
	}
	// Stop removes the unit; its status is already read.
	if err := e.cfg.Units.Stop(ctx, c.unitName()); err != nil {
		slog.Warn("remove unit", "command", cmd.Id, "err", err)
	}
	if st.Signal != 0 {
		e.end(c, failed, nil, fmt.Sprintf("killed by signal %d", st.Signal))
		return
	}
	code := int32(st.ExitCode)
	if lost := c.rec.IsolationLost; lost != "" {
		e.end(c, failed, nil, fmt.Sprintf("exit %d, but the measurement lost its isolation: %s", code, lost))
		return
	}
	state := succeeded
	if code != 0 {
		state = failed
	}
	e.end(c, state, &code, "")
}

// admit starts queued commands, oldest first, while they fit.
func (e *Engine) admit(ctx context.Context) {
	e.mu.Lock()
	var waiting []*command
	for _, c := range e.commands {
		if c.cmd().State == queued {
			waiting = append(waiting, c)
		}
	}
	slices.SortFunc(waiting, func(a, b *command) int { return int(a.cmd().QueuedAt - b.cmd().QueuedAt) })
	e.mu.Unlock()
	// ahead is the oldest measurement still waiting, which later ones wait
	// for; holder is one that waits for room, which later work waits behind.
	var ahead, holder *command
	for _, c := range waiting {
		e.mu.Lock()
		if c.cmd().State != queued {
			e.mu.Unlock()
			continue // stopped meanwhile
		}
		if kind, reason, holds := e.admissionBlocker(c, ahead, holder); kind != "" {
			// The numbers change every step and go in heartbeats; the kind of
			// wait is an event.
			c.cmd().Reason = reason
			if kind != c.waiting {
				c.waiting = kind
				e.saveCommand(c, change{what: "waiting", detail: reason})
			}
			if c.cmd().Bench {
				ahead = cmp.Or(ahead, c)
				if holds {
					holder = cmp.Or(holder, c)
				}
			}
			e.mu.Unlock()
			continue
		}
		c.cmd().Reason = ""
		spec := e.spec(c)
		e.mu.Unlock()
		err := e.makeTmp(c.cmd().Id)
		if err != nil {
			err = fmt.Errorf("make its TMPDIR: %w", err)
		}
		if err == nil {
			err = e.stopLeftover(ctx, c)
		}
		if err == nil && c.cmd().Bench {
			if err = e.cfg.Bench.On(ctx); err != nil {
				err = fmt.Errorf("isolate the node: %w", err)
			}
		}
		if err == nil {
			err = e.cfg.Units.Start(ctx, spec)
		}
		e.mu.Lock()
		if err != nil {
			slog.Error("start command", "command", c.cmd().Id, "err", err)
			e.end(c, failed, nil, "could not start: "+err.Error())
			e.mu.Unlock()
			continue
		}
		cmd := c.cmd()
		if cmd.State != queued {
			// Stopped (ended by Stop) while it started.
			e.mu.Unlock()
			if err := e.cfg.Units.Stop(ctx, c.unitName()); err != nil {
				slog.Warn("stop a command stopped while it started", "command", cmd.Id, "err", err)
			}
			continue
		}
		now := e.nowMs()
		cmd.State = running
		cmd.StartedAt = &now
		share := fmt.Sprintf("weight %d", cmd.Limits.CpuWeight)
		if cmd.Bench {
			share = "measurement on CPUs " + e.cfg.Bench.CPUs().String()
		}
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: "admitted", Detail: fmt.Sprintf("%s · low %s · high %s", share, gib(cmd.Limits.MemoryLowBytes), gib(cmd.Limits.MemoryHighBytes))})
		ch := change{what: "admitted"}
		switch {
		case cmd.Bench:
			ch.feed = feed("admit", fmt.Sprintf("measure %s · %s on CPUs %s", cmd.Argv, cmd.WorkspaceId, e.cfg.Bench.CPUs()), ref("command", cmd.Id))
		case cmd.Kind != "freshen":
			ch.feed = feed("admit", fmt.Sprintf("admit %s · %s", cmd.Argv, cmd.WorkspaceId), ref("command", cmd.Id))
		}
		e.saveCommand(c, ch)
		go e.watchExit(ctx, spec.Slice, c.unitName())
		e.mu.Unlock()
	}
}

// stopLeftover stops a unit of c's name already there: one a worker that
// stopped between starting it and recording that left, running on
// unwatched, in a measurement's slice too.
func (e *Engine) stopLeftover(ctx context.Context, c *command) error {
	st, err := e.cfg.Units.Status(ctx, c.unitName())
	if err != nil || !st.Loaded {
		return err
	}
	slog.Warn("stop a unit left by a worker that stopped as it started it", "command", c.cmd().Id)
	if err := e.cfg.Units.Stop(ctx, c.unitName()); err != nil {
		return fmt.Errorf("stop the unit a worker before left: %w", err)
	}
	return nil
}

// admissionBlocker says why c cannot start now, as a kind (preparing,
// measurement, behind-measurement, memory, pressure, disk) and a reason
// with the numbers as they are, or "" if it can; and whether a
// measurement's reason holds up the work queued after it: it waits for room
// that work would take. ahead is the oldest measurement still waiting,
// holder one that holds up work. Call with e.mu held.
func (e *Engine) admissionBlocker(c, ahead, holder *command) (kind, reason string, holds bool) {
	if w, ok := e.workspaces[c.cmd().WorkspaceId]; ok && w.ws().State == preparing {
		return "preparing", w.ws().Reason, false
	}
	switch {
	case c.cmd().Bench:
		if r := e.benchBlocker(ahead); r != "" {
			return "measurement", r, false
		}
	case holder != nil:
		return "behind-measurement", fmt.Sprintf("waiting behind measurement %s, which waits for room", holder.cmd().Id), false
	}
	if k, r := e.room(c); r != "" {
		return k, r, true
	}
	if c.cmd().Bench {
		if r := e.benchRoom(); r != "" {
			return "measurement", r, true
		}
	}
	return "", "", false
}

// room says why the node has no room for c now, as a kind and a reason, or
// "". Call with e.mu held.
func (e *Engine) room(c *command) (kind, reason string) {
	var reserved int64
	for _, o := range e.commands {
		if s := o.cmd().State; s == running || s == parked {
			reserved += o.cmd().Limits.MemoryLowBytes
		}
	}
	want := c.cmd().Limits.MemoryLowBytes
	room := int64(float64(e.resources.MemoryBytes)*admitMemory) - reserved
	switch {
	case want > room:
		return "memory", fmt.Sprintf("waiting for memory: needs %s guaranteed, %s left", gib(want), gib(max(0, room)))
	case e.usage.MemoryPressure >= admitPressure:
		return "pressure", fmt.Sprintf("waiting: memory pressure %.0f%%", e.usage.MemoryPressure)
	case e.diskBytes > 0 && float64(e.disk.FreeBytes) < float64(e.diskBytes)*minFreeDisk:
		return "disk", fmt.Sprintf("waiting for disk: %s free", gib(e.disk.FreeBytes))
	}
	return "", ""
}

// spec is the unit for c. Call with e.mu held.
func (e *Engine) spec(c *command) unit.Spec {
	cmd := c.cmd()
	env := append(cacheEnv(), c.rec.Env...)
	env = append(env, "HOTBED_WORKSPACE="+cmd.WorkspaceId, "HOTBED_COMMAND="+cmd.Id, "TMPDIR="+e.tmpDir(cmd.Id))
	props := e.cfg.UnitProperties
	if cmd.Bench {
		// At the default nice: on CPUs of its own it competes only with the
		// kernel's per-CPU threads, which a niced measurement yields to.
		props = slices.DeleteFunc(slices.Clone(props), func(p string) bool { return strings.HasPrefix(p, "Nice=") })
	}
	return unit.Spec{
		Name:       c.unitName(),
		Slice:      e.sliceOf(c),
		Argv:       cmd.Args,
		Dir:        c.rec.Dir,
		Binds:      []unit.Bind{{Source: e.cfg.Store.Path(c.rec.Volume), Target: WorkPath}},
		User:       e.cfg.User,
		Env:        env,
		PathPrefix: cachePath,
		PathSuffix: []string{e.home + "/.cargo/bin", e.home + "/.local/bin", e.home + "/go/bin", e.home + "/.local/go/bin", "/usr/local/go/bin"},
		CPUWeight:  int(cmd.Limits.CpuWeight),
		MemoryLow:  cmd.Limits.MemoryLowBytes,
		MemoryHigh: cmd.Limits.MemoryHighBytes,
		Log:        e.logPath(cmd.Id),
		NoSwap:     cmd.Bench,
		Properties: props,
	}
}

// park freezes the newest running command when memory runs short, and
// thaws the oldest parked one when there is room for it again. Nothing is
// killed for memory. Parking frees memory for the commands left running,
// so it never freezes the last of them, and with none left running it
// thaws the oldest whatever memory says: else every command could sit
// frozen, waiting for room that only running ones could make. A
// measurement is never parked, but counts among those left running.
func (e *Engine) park(ctx context.Context) {
	mem, err := e.cfg.System.Meminfo()
	if err != nil {
		slog.Warn("read memory", "err", err)
		return
	}
	e.mu.Lock()
	var runningCmds, parkedCmds []*command
	measuring := 0
	for _, c := range e.commands {
		switch s := c.cmd().State; {
		case s == running && c.cmd().Bench:
			measuring++
		case s == running && c.cmd().Kind != "server":
			runningCmds = append(runningCmds, c)
		case s == parked:
			parkedCmds = append(parkedCmds, c)
		}
	}
	e.mu.Unlock()
	total := float64(mem.Total)
	if float64(mem.Used()) > total*parkAt && len(runningCmds) > 0 && len(runningCmds)+measuring > 1 {
		slices.SortFunc(runningCmds, func(a, b *command) int { return int(b.cmd().GetStartedAt() - a.cmd().GetStartedAt()) })
		victim := runningCmds[0]
		if err := e.cfg.Units.Freeze(ctx, victim.unitName()); err != nil {
			slog.Error("park", "command", victim.cmd().Id, "err", err)
			return
		}
		if victim.rec.Cgroup != "" {
			// Push the frozen command's memory to swap so others can use it.
			if err := e.cfg.Units.Reclaim(victim.rec.Cgroup, victim.cmd().MemoryBytes); err != nil {
				slog.Warn("reclaim parked command", "command", victim.cmd().Id, "err", err)
			}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		cmd := victim.cmd()
		pct := 100 * float64(mem.Used()) / total
		of := "memory"
		if mem.Domain != "" {
			of = "of " + mem.Domain + "'s memory"
		}
		cmd.State = parked
		cmd.CpuCores = 0
		cmd.Reason = fmt.Sprintf("frozen at %.0f%% %s in use; its memory goes to swap until there is room", pct, of)
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: "parked", Detail: cmd.Reason})
		victim.rec.Parks++
		victim.rec.ParkedAt = e.nowMs()
		e.saveCommand(victim, change{
			what: "parked", detail: cmd.Reason,
			feed: feed("park", fmt.Sprintf("park %s · %s at %.0f%%", cmd.Argv, cmp.Or(mem.Domain, "memory"), pct), ref("command", cmd.Id)),
		})
		return
	}
	if len(parkedCmds) == 0 {
		return
	}
	slices.SortFunc(parkedCmds, func(a, b *command) int { return int(a.rec.ParkedAt - b.rec.ParkedAt) })
	oldest := parkedCmds[0]
	if len(runningCmds)+measuring > 0 && float64(mem.Used()+oldest.cmd().SwapBytes) > total*thawBelow {
		return
	}
	if err := e.cfg.Units.Thaw(ctx, oldest.unitName()); err != nil {
		slog.Error("thaw", "command", oldest.cmd().Id, "err", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cmd := oldest.cmd()
	slept := (time.Duration(e.nowMs()-oldest.rec.ParkedAt) * time.Millisecond).Round(time.Second)
	cmd.State = running
	cmd.Reason = ""
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: "thawed", Detail: "after " + slept.String()})
	e.saveCommand(oldest, change{
		what: "thawed", detail: "after " + slept.String(),
		feed: feed("thaw", fmt.Sprintf("thaw %s after %s", cmd.Argv, slept), ref("command", cmd.Id)),
	})
}

// reap stops attached commands whose client went away: no one has followed
// their output for orphanAfter.
func (e *Engine) reap(ctx context.Context) {
	e.mu.Lock()
	var orphans []string
	for _, c := range e.commands {
		if c.cmd().Attached && live(c.cmd().State) && c.followers == 0 && e.now().Sub(c.lastFollow) > orphanAfter {
			orphans = append(orphans, c.cmd().Id)
		}
	}
	e.mu.Unlock()
	for _, id := range orphans {
		if _, err := e.Stop(ctx, id, "its client went away"); err != nil {
			slog.Warn("stop orphaned command", "command", id, "err", err)
		}
	}
}

// forget drops commands that ended a while ago, which history keeps, and
// workspaces released a while ago.
func (e *Engine) forget() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forgetReleased()
	for id, c := range e.commands {
		if live(c.cmd().State) || c.cmd().EndedAt == nil {
			continue
		}
		if time.Duration(e.nowMs()-c.cmd().GetEndedAt())*time.Millisecond > keepEnded {
			delete(e.commands, id)
			e.deleteCommand(c)
			if e.cfg.Profiler != nil {
				e.cfg.Profiler.Forget(id)
			}
		}
	}
}

// end records how c ended and tells the controller. Call with e.mu held.
func (e *Engine) end(c *command, state pb.CommandState, code *int32, why string) {
	cmd := c.cmd()
	if !live(cmd.State) {
		return
	}
	now := e.nowMs()
	cmd.State = state
	cmd.EndedAt = &now
	cmd.ExitCode = code
	cmd.CpuCores = 0
	cmd.Reason = why
	what, detail := "exited", "exit "+codeText(code)
	if state == cancelled {
		what, detail = "cancelled", why
	}
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: what, Detail: detail})
	// Its processes are gone: the unit has exited or been stopped.
	go e.removeTmp(cmd.Id)

	started := cmd.QueuedAt
	if cmd.StartedAt != nil {
		started = *cmd.StartedAt
	}
	seconds := float64(now-started) / 1000
	units, failure := scanLog(e.logPath(cmd.Id), state, code, cmd.Kind)
	if cmd.Rebuilt != nil {
		cmd.Rebuilt.Units = int32(units)
	}
	if cold, _, ok := e.coldBuild(c.rec.RepoId); ok && isBuild(cmd.Kind) {
		// From nothing, this command would have had to build everything
		// first: the base's cold freshen, then itself.
		total := cold + seconds
		cmd.ColdSeconds = &total
	}
	if cmd.Kind == "freshen" {
		e.saveCommand(c, change{what: what, detail: detail})
		e.freshened(c, state == succeeded, seconds, units)
		return
	}
	f := &pb.FinishedCommand{
		Id: cmd.Id, RepoId: c.rec.RepoId, NodeId: cmd.NodeId, WorkspaceId: cmd.WorkspaceId, Agent: c.rec.Agent,
		Argv: cmd.Argv, Kind: cmd.Kind, State: state, ExitCode: code, Failure: failure,
		EndedAt: now, QueuedSeconds: float64(started-cmd.QueuedAt) / 1000, Seconds: seconds,
		ColdSeconds: cmd.ColdSeconds, PeakMemoryBytes: c.rec.PeakMemoryBytes, Parks: c.rec.Parks,
		SessionId: cmd.SessionId, Reason: why, Bench: cmd.Bench,
	}
	if cmd.Rebuilt != nil {
		units, of := cmd.Rebuilt.Units, cmd.Rebuilt.Of
		f.Rebuilt, f.Of = &units, &of
	}
	r := ref("command", cmd.Id)
	var line *pb.WorkerEvent
	switch state {
	case succeeded:
		text := fmt.Sprintf("ok %s · %s · %s", cmd.Argv, c.rec.RepoId, dur(seconds))
		if cmd.ColdSeconds != nil {
			text += fmt.Sprintf(" (cold %s, %.0f×)", dur(*cmd.ColdSeconds), *cmd.ColdSeconds/max(seconds, 0.1))
		}
		line = feed("finish", text, r)
	case failed:
		line = feed("fail", fmt.Sprintf("fail %s · %s · %s", cmd.Argv, c.rec.RepoId, orDefault(why, "exit "+codeText(code))), r)
	default:
		line = feed("finish", fmt.Sprintf("cancelled %s · %s", cmd.Argv, why), r)
	}
	var prof *pb.CommandProfile
	if e.cfg.Profiler != nil {
		if snap := e.cfg.Profiler.Snapshot(cmd.Id); snap != nil && snap.TotalSamples > 0 {
			if err := profiler.SaveProfile(e.profilePath(cmd.Id), snap); err != nil {
				slog.Warn("save command profile", "command", cmd.Id, "err", err)
			}
			prof = snap
		}
	}
	e.saveCommand(c, change{what: what, detail: detail, finished: f, feed: line, profile: prof})
	if w, ok := e.workspaces[cmd.WorkspaceId]; ok {
		w.ws().LastActiveAt = now
		if w.ws().State == activeWS && !e.workspaceBusy(w.ws().Id) {
			w.ws().State = idle
		}
		wsWhat := "active"
		if w.ws().State == idle {
			wsWhat = "idle"
		}
		e.saveWorkspace(w, change{what: wsWhat, detail: cmd.Id + " ended"})
	}
}

func (e *Engine) workspaceBusy(id string) bool {
	for _, c := range e.commands {
		if c.cmd().WorkspaceId == id && live(c.cmd().State) {
			return true
		}
	}
	return false
}

// coldBuild is the repo's most recent from-nothing freshen on this node:
// how long it took and how many units it compiled. Call with e.mu held.
func (e *Engine) coldBuild(repo string) (seconds float64, units int, ok bool) {
	var best *pb.Base
	for _, b := range e.bases {
		if b.RepoId == repo && b.Cold && (b.State == pb.BaseState_BASE_STATE_WARM || b.State == pb.BaseState_BASE_STATE_STALE) {
			if best == nil || b.Generation > best.Generation {
				best = b
			}
		}
	}
	if best == nil {
		return 0, 0, false
	}
	return best.BuildSeconds, int(best.Units), true
}

func (e *Engine) logPath(id string) string { return filepath.Join(e.cfg.Dir, "logs", id+".log") }

var (
	compiling   = regexp.MustCompile(`^\s+Compiling \S+ v`)
	rustError   = regexp.MustCompile(`^error(\[E\d+\])?: `)
	testFailure = regexp.MustCompile(`(^test result: FAILED|^\s+FAIL \[|^--- FAIL:|^FAIL\s|Tests:\s+\d+ failed)`)
)

// scanLog counts the units cargo compiled and sorts a failure into a kind
// people recognise, from the command's output.
func scanLog(path string, state pb.CommandState, code *int32, kind string) (units int, failure string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ""
	}
	defer f.Close()
	var compileErr, testFail bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case compiling.MatchString(line):
			units++
		case rustError.MatchString(line):
			compileErr = true
		case testFailure.MatchString(line):
			testFail = true
		}
	}
	if state != failed {
		return units, ""
	}
	switch {
	case code == nil:
		return units, "signal"
	case testFail:
		return units, "test"
	case compileErr && kind == "check":
		return units, "lint"
	case compileErr:
		return units, "compile"
	default:
		return units, "exit"
	}
}

func codeText(code *int32) string {
	if code == nil {
		return "signal"
	}
	return strconv.Itoa(int(*code))
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

func dur(s float64) string {
	if s < 60 {
		return fmt.Sprintf("%.1f s", s)
	}
	return time.Duration(s * float64(time.Second)).Round(time.Second).String()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
