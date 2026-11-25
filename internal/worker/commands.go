package worker

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/shell"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

const (
	queued    = pb.CommandState_COMMAND_STATE_QUEUED
	running   = pb.CommandState_COMMAND_STATE_RUNNING
	parked    = pb.CommandState_COMMAND_STATE_PARKED
	succeeded = pb.CommandState_COMMAND_STATE_SUCCEEDED
	failed    = pb.CommandState_COMMAND_STATE_FAILED
	cancelled = pb.CommandState_COMMAND_STATE_CANCELLED
	paused    = pb.CommandState_COMMAND_STATE_PAUSED
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
	if err := validateID(req.WorkspaceId); err != nil {
		return nil, err
	}
	if req.Id != "" {
		if err := validateID(req.Id); err != nil {
			return nil, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[req.WorkspaceId]
	if !ok || w.ws().State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED || w.releasing {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", req.WorkspaceId)
	}
	if w.ws().State == failedWS {
		return nil, api.Errorf(api.CodeConflict, "workspace %s failed: %s", req.WorkspaceId, w.ws().Reason).
			WithSuggestion("run again: the controller releases a failed workspace and gives the session a new one")
	}
	dir := WorkPath
	if req.Repo != "" {
		if repoOf(w.ws(), req.Repo) == nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "workspace %s of project %s has no repo %s", req.WorkspaceId, w.ws().Project, req.Repo).
				WithSuggestion("hotbed project ls shows each project's repos; hotbed repo add puts a repo in one")
		}
		dir += "/" + req.Repo
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
			WithSuggestion("hotbed nodes shows the nodes that do (benchCpus); hotbed run --bench goes only to them unless --node names another")
	}
	kind := req.Kind
	if kind == "" {
		if req.Port > 0 {
			kind = "server"
		} else {
			kind = "shell"
		}
	}
	limits := &pb.CommandLimits{}
	if req.Limits != nil {
		limits = proto.Clone(req.Limits).(*pb.CommandLimits)
	}
	if limits.Priority < minPriority || limits.Priority > maxPriority {
		return nil, api.Errorf(api.CodeInvalidArgs, "priority %d out of range [%d, %d]", limits.Priority, minPriority, maxPriority).
			WithSuggestion("pass low (-10), normal (0), high (10), urgent (20), or an integer from -100 to 100")
	}
	if limits.CpuWeight == 0 {
		limits.CpuWeight = 100
	}
	now := e.nowMs()
	c := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id: id, WorkspaceId: req.WorkspaceId, NodeId: e.cfg.NodeID,
			Argv: shell.Join(req.Args), Args: req.Args, Kind: kind,
			State: queued, QueuedAt: now, Limits: limits, Attached: req.Attached,
			SessionId: w.ws().SessionId, Bench: req.Bench, Repo: req.Repo,
			Runtime:  e.workspaceRuntime(w),
			Timeline: []*pb.CommandEvent{{At: now, What: "queued", Detail: "by " + w.ws().Agent}},
		},
		RepoId: req.Repo, Project: w.ws().Project, Agent: w.ws().Agent, Volume: w.rec.Volume,
		Dir: dir, Env: req.Env,
	})
	if req.Port > 0 {
		if e.usesRuntime(req.WorkspaceId) {
			if p, err := startPortProxy(fmt.Sprintf(":%d", req.Port), req.WorkspaceId, uint16(req.Port), e.cfg.Runtime); err != nil {
				slog.Warn("start VM dev-server port proxy", "command", id, "port", req.Port, "err", err)
			} else {
				e.portProxies[id] = p
			}
		}
		c.cmd().Url = fmt.Sprintf("http://%s:%d", e.cfg.Host, req.Port)
	}
	if isBuild(kind) {
		if _, units, ok := e.coldBuild(w.ws().Project); ok {
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

func (e *Engine) workspaceRuntime(w *workspace) string {
	if w != nil {
		if rt := w.ws().Runtime; rt != "" {
			return rt
		}
		if e.usesRuntime(w.ws().Id) && e.cfg.Runtime != nil {
			return e.cfg.Runtime.Kind()
		}
	}
	return ""
}

func (e *Engine) commandRuntime(c *command) string {
	if c == nil {
		return ""
	}
	if rt := c.cmd().Runtime; rt != "" {
		return rt
	}
	if c.isVMFreshen() {
		return runtime.KindVM
	}
	if c.isManagedFreshen() {
		return runtime.KindCladefs
	}
	if c.cmd().Kind == "freshen" {
		return runtime.KindCladefs
	}
	return e.workspaceRuntime(e.workspaces[c.cmd().WorkspaceId])
}

// Stop ends a command: a queued one never starts, a running or parked one is
// stopped. It returns the command as it ended.
func (e *Engine) Stop(ctx context.Context, id, why string) (*pb.Command, error) {
	return e.stop(ctx, id, why, nil)
}

// stop is Stop, of a command someone stops by hand (by) when it is one.
func (e *Engine) stop(ctx context.Context, id, why string, by *byHand) (*pb.Command, error) {
	e.mu.Lock()
	c, ok := e.commands[id]
	if !ok {
		e.mu.Unlock()
		if err := validateID(id); err != nil {
			return nil, err
		}
		return nil, api.Errorf(api.CodeNotFound, "no command %s on this node", id)
	}
	state := c.cmd().State
	if state == queued {
		admitDone := c.admitDone
		if admitDone == nil {
			// It ends here, under the lock: admit has not started it.
			e.endBy(c, cancelled, nil, why, by)
			out := clone(c.cmd())
			e.mu.Unlock()
			return out, nil
		}
		// admit is currently creating tmpDir / starting the unit outside e.mu.
		// Signal it via c.stopping and wait for it to finish or roll back; if
		// admit started the unit and Units.Stop fails, the command stays running
		// and Stop returns that error so a retry can stop the unit.
		c.stopping = true
		c.stopWhy = why
		c.stopBy = by
		e.mu.Unlock()
		select {
		case <-admitDone:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := c.admitStopErr; err != nil {
			return nil, err
		}
		return clone(c.cmd()), nil
	}
	if !api.Live(state) {
		out := clone(c.cmd())
		e.mu.Unlock()
		return out, nil
	}
	var cancelDiag context.CancelFunc
	if by == nil && c.cancelDiag != nil {
		cancelDiag = c.cancelDiag
	}
	c.stopping = true
	e.mu.Unlock()
	if cancelDiag != nil {
		cancelDiag()
	}
	// A frozen unit cannot be stopped until it is thawed: one parked or
	// paused, or that pause froze as this stop began. Its unit says.
	if st, err := e.cfg.Units.Status(ctx, c.unitName()); err != nil {
		slog.Warn("read the unit before stop", "command", id, "err", err)
	} else if st.Frozen {
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
	e.finishOutput(c)
	e.mu.Lock()
	defer e.mu.Unlock()
	c.stopping = false
	e.endBy(c, cancelled, nil, why, by)
	return clone(c.cmd()), nil
}

// statusSampler is implemented by Units (both production Systemd and fake.Units)
// that return the wall-clock timestamp captured when CPU counters were sampled.
type statusSampler interface {
	StatusManyAt(ctx context.Context, names []string) (map[string]unit.State, time.Time, error)
}

var _ statusSampler = Systemd{}

func (Systemd) StatusManyAt(ctx context.Context, names []string) (map[string]unit.State, time.Time, error) {
	return unit.StatusManyAt(ctx, names)
}

// minCPUSampleWindow avoids computing CPU cores over tiny stepSoon() bursts
// where quantized counters or scheduling jitter would distort the rate.
const minCPUSampleWindow = 100 * time.Millisecond

// step moves every command along: reads running units, admits queued
// commands, parks or thaws for memory, stops orphaned attached commands.
func (e *Engine) step(ctx context.Context) {
	e.mu.Lock()
	var active []*command
	for _, c := range e.commands {
		if started(c.cmd().State) {
			active = append(active, c)
		}
	}
	e.mu.Unlock()
	if e.cfg.Bench != nil {
		e.cfg.Bench.Sample()
	}
	// Writers are held before the measurement's isolation is checked, so
	// what this sample saw is held now.
	e.quiet(ctx)
	states := make([]*unit.State, len(active))
	var sampledAt time.Time
	if len(active) > 0 {
		names := make([]string, len(active))
		for i, c := range active {
			names[i] = c.unitName()
		}
		var (
			byName map[string]unit.State
			err    error
		)
		// StatusManyAt timestamps against wall-clock time.Now(); when a test
		// injects a synthetic clock (e.cfg.Now != nil), stay on StatusMany +
		// e.now() so sample timestamps match the synthetic clock.
		if s, ok := e.cfg.Units.(statusSampler); ok && e.cfg.Now == nil {
			byName, sampledAt, err = s.StatusManyAt(ctx, names)
		} else {
			byName, err = e.cfg.Units.StatusMany(ctx, names)
			sampledAt = e.now()
		}
		if err != nil {
			slog.Warn("read commands", "count", len(active), "err", err)
		} else {
			for i, c := range active {
				if st, ok := byName[c.unitName()]; ok {
					st := st
					states[i] = &st
				}
			}
		}
	}
	// Each command's output at once, so one that wrote a lot holds up the
	// others no longer than itself.
	var wg sync.WaitGroup
	for i, c := range active {
		if st := states[i]; st != nil {
			wg.Go(func() {
				if st.Loaded && !st.Exited {
					c.out.catchUp()
					e.compactLog(c)
				} else {
					e.removeUnit(ctx, c, *st)
				}
			})
		}
	}
	wg.Wait()
	for i, c := range active {
		if st := states[i]; st != nil {
			if c.isFreshen() {
				step, steps, stepName, startMs, done, gitSec, gitDone := c.out.freshenLive()
				e.mu.Lock()
				if step != c.lastFreshenStep || done != c.lastFreshenDone || gitDone != c.lastFreshenGitDone {
					c.lastFreshenStep, c.lastFreshenDone, c.lastFreshenGitDone = step, done, gitDone
					if b, ok := e.bases[c.rec.BaseId]; ok && b.State == building && b.Command == c.cmd().Id {
						b.Steps = c.out.snapshotLive(b.Steps)
						if steps > 0 {
							b.TotalSteps = int32(steps)
							if done {
								b.Progress = 1
								b.CurrentStep = ""
								b.CurrentStepIndex = 0
								b.CurrentStepStartedAt = 0
							} else {
								b.Progress = float64(step-1) / float64(steps)
								b.CurrentStep = stepName
								b.CurrentStepIndex = int32(step)
								b.CurrentStepStartedAt = startMs
							}
						}
						if gitDone {
							b.GitSeconds = gitSec
						}
						detail := "git done"
						if done {
							detail = "steps done"
						} else if step > 0 {
							detail = fmt.Sprintf("step %d/%d %s", step, steps, stepName)
						}
						e.saveBase(b, change{what: "progress", detail: detail})
					} else {
						e.notify()
					}
				}
				e.mu.Unlock()
			}
			e.observe(ctx, c, *st, sampledAt)
		}
	}
	e.settleBench(ctx)
	e.admit(ctx)
	e.limit(ctx)
	e.park(ctx)
	e.hibernateIdleVMs(ctx)
	e.reap(ctx)
	e.forget()
}

// compactLog keeps c's log to the engine's limit and says on c how much it
// dropped. The command's next
// saved change (its end, at the latest) records it.
func (e *Engine) compactLog(c *command) {
	d, err := c.out.compact(e.cfg.LogLimit)
	if err != nil {
		slog.Error("compact command log", "command", c.cmd().Id, "err", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c.cmd().DroppedOutputBytes = d.Bytes()
}

// removeUnit stops a unit whose command exited, which takes any process it
// left behind with it, so its output is complete: then it reads the rest.
// It leaves a unit Stop is stopping to Stop.
func (e *Engine) removeUnit(ctx context.Context, c *command, st unit.State) {
	e.mu.Lock()
	ours := api.Live(c.cmd().State) && !c.stopping
	e.mu.Unlock()
	if !ours {
		return
	}
	// Its status is already read.
	if st.Exited {
		if err := e.cfg.Units.Stop(ctx, c.unitName()); err != nil {
			slog.Warn("remove unit", "command", c.cmd().Id, "err", err)
		}
	}
	e.finishOutput(c)
}

// finishOutput reads the rest of the output of c, whose processes are gone:
// compacted first, so what is left to read is at most the log's limit
// however much it wrote at the end. For VM commands it also synchronously
// snapshots the guest's eBPF profile before endBy marks the command ended.
func (e *Engine) finishOutput(c *command) {
	e.compactLog(c)
	c.out.finish()
	e.snapshotVMProfile(c)
}

// started says whether a command in state s has a unit: it runs, or is
// frozen (parked or paused).
func started(s pb.CommandState) bool { return s == running || s == parked || s == paused }

const (
	minPriority = -100
	maxPriority = 100
)

func (c *command) priority() int32 {
	return c.cmd().GetLimits().GetPriority()
}

// reserves says whether a command in state s holds its guaranteed memory
// (memory.low) on the node: one running or parked does, to come back to;
// one paused does not, its memory gone to swap until an agent, who may never
// come, waits on it (park makes room again should it come back).
func reserves(s pb.CommandState) bool { return s == running || s == parked }

// reservation is the memory the node keeps for c while it reserves: its
// guarantee, or as much as it has held of its own, if more. A build's
// need recurs (each link wants its memory again), so what it has held is
// what it may hold again, whatever it asked for.
func (c *command) reservation() int64 {
	return max(c.cmd().Limits.GetMemoryLowBytes(), c.rec.PeakHeldBytes)
}

// reserved is the memory the node keeps for the commands that reserve it,
// base builds and work alike. Call with e.mu held.
func (e *Engine) reserved() int64 {
	var n int64
	for _, c := range e.commands {
		if reserves(c.cmd().State) {
			n += c.reservation()
		}
	}
	return n
}

// parkedReservationForHigherPriority is the memory a parked command still
// charges against strictly higher-priority work: once Reclaim succeeded and
// its post-reclaim resident anonymous RAM was measured, charge that remaining
// resident RAM (so partial reclaim still charges what stayed in RAM). If
// Reclaim failed, was skipped (e.g. I/O-held beside a measurement), or could
// not be measured, keep charging its full reservation. Call with e.mu held.
func (c *command) parkedReservationForHigherPriority() int64 {
	if c.reclaimed && c.residentKnown {
		return c.residentBytes
	}
	return max(c.reservation(), c.residentBytes)
}

// reservedFor is the memory reserved against a command at priority: every
// running command, parked commands whose priority is at least priority, plus
// the still-resident (or unreclaimed) memory of strictly lower-priority parked
// commands (board 7907, board 9661). Call with e.mu held.
func (e *Engine) reservedFor(priority int32) int64 {
	var n int64
	for _, c := range e.commands {
		switch c.cmd().State {
		case running:
			n += c.reservation()
		case parked:
			if c.priority() >= priority {
				n += c.reservation()
			} else {
				n += c.parkedReservationForHigherPriority()
			}
		}
	}
	return n
}

// admitLimit is the memory reservations may take: admitMemory of RAM.
// Call with e.mu held.
func (e *Engine) admitLimit() int64 {
	return int64(float64(e.resources.GetMemoryBytes()) * admitMemory)
}

func (e *Engine) observe(ctx context.Context, c *command, st unit.State, sampledAt time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cmd := c.cmd()
	if !api.Live(cmd.State) || c.stopping {
		return
	}
	if sampledAt.IsZero() {
		sampledAt = e.now()
	}
	if !st.Loaded {
		e.end(c, failed, nil, "its systemd unit disappeared")
		return
	}
	if st.Cgroup != "" {
		c.rec.Cgroup = st.Cgroup
	}
	if c.cpuAt.IsZero() || st.CPUNanos < c.cpuNanos {
		c.cpuNanos, c.cpuAt = st.CPUNanos, sampledAt
	} else if dt := sampledAt.Sub(c.cpuAt); dt >= minCPUSampleWindow {
		cmd.CpuCores = float64(st.CPUNanos-c.cpuNanos) / 1e9 / dt.Seconds()
		c.cpuNanos, c.cpuAt = st.CPUNanos, sampledAt
	}
	if st.Memory > 0 {
		cmd.MemoryBytes = st.Memory
		c.rec.PeakMemoryBytes = max(c.rec.PeakMemoryBytes, st.Memory)
	}
	if st.MemoryPeak > 0 {
		c.rec.PeakMemoryBytes = max(c.rec.PeakMemoryBytes, st.MemoryPeak)
		if st.Exited && cmd.MemoryBytes == 0 {
			cmd.MemoryBytes = st.MemoryPeak
		}
	}
	if c.rec.Cgroup != "" {
		if m, err := e.cfg.Units.Memory(c.rec.Cgroup); err == nil {
			cmd.SwapBytes = m.Swap
			c.residentBytes = m.Anon
			c.residentKnown = true
			if cmd.State == parked && m.Anon == 0 && e.rtUnits != nil {
				if susp, _ := e.rtUnits.isSuspended(c.runtimeTarget()); susp {
					c.reclaimed = true
				}
			}
			c.rec.PeakHeldBytes = max(c.rec.PeakHeldBytes, m.Held())
		} else if !st.Exited {
			slog.Warn("read a command's memory", "command", cmd.Id, "err", err)
		}
	}
	// While it runs and as it ends: the node is given back only after.
	e.checkIsolation(c, st.Exited)
	c.deactivating = st.Loaded && !st.Exited && !st.Running
	if !st.Exited {
		return
	}
	// removeUnit has removed the unit and read the rest of its output.
	if st.OOMKilled || st.Result == "oom-kill" {
		var code *int32
		if st.Signal == 0 && st.ExitCode != 0 {
			ec := int32(st.ExitCode)
			code = &ec
		}
		e.end(c, failed, code, oomReason(st, c.rec.PeakMemoryBytes))
		return
	}
	if st.Signal != 0 {
		why := fmt.Sprintf("killed by signal %d", st.Signal)
		if st.Result != "" && st.Result != "signal" && st.Result != "core-dump" && st.Result != "success" {
			why = fmt.Sprintf("killed by signal %d (systemd result: %s)", st.Signal, st.Result)
		}
		e.end(c, failed, nil, why)
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

// oomReason explains an OOM kill clearly, including when systemd's default
// OOMPolicy=stop turned a child's OOM kill into SIGTERM on the main process.
func oomReason(st unit.State, peakBytes int64) string {
	var parts []string
	switch {
	case st.Signal == 15:
		parts = append(parts, "systemd stopped the unit with signal 15")
	case st.Signal != 0:
		parts = append(parts, fmt.Sprintf("signal %d", st.Signal))
	case st.ExitCode != 0:
		parts = append(parts, fmt.Sprintf("exit %d", st.ExitCode))
	}
	if peakBytes > 0 {
		parts = append(parts, "peak "+gib(peakBytes))
	}
	if len(parts) == 0 {
		return "killed by the OOM killer"
	}
	return fmt.Sprintf("killed by the OOM killer (%s)", strings.Join(parts, "; "))
}

// admit starts queued commands, highest priority first then oldest first,
// while they fit, preempting strictly lower-priority running commands when
// needed (board 7907).
func (e *Engine) admit(ctx context.Context) {
	e.mu.Lock()
	e.weighHold(e.now())
	var waiting []*command
	for _, c := range e.commands {
		if c.cmd().State == queued {
			waiting = append(waiting, c)
		}
	}
	slices.SortFunc(waiting, func(a, b *command) int {
		return cmp.Or(
			cmp.Compare(b.priority(), a.priority()),
			cmp.Compare(a.cmd().QueuedAt, b.cmd().QueuedAt),
			cmp.Compare(a.cmd().Id, b.cmd().Id),
		)
	})
	e.mu.Unlock()
	var mem sysinfo.Mem
	var hasMem bool
	if len(waiting) > 0 {
		if m, err := e.cfg.System.Meminfo(); err == nil && m.Total > 0 {
			mem, hasMem = m, true
		}
	}
	// ahead is the oldest measurement still waiting, which later ones wait
	// for; holder is one that waits for room, which later work waits behind.
	var ahead, holder *command
	for _, c := range waiting {
		e.mu.Lock()
		if c.cmd().State != queued || c.stopping || c.intervening {
			e.mu.Unlock()
			continue // stopped meanwhile
		}
		if state, why, terminal := e.terminalWorkspaceForCommand(c); terminal {
			e.end(c, state, nil, why)
			e.mu.Unlock()
			continue
		}
		kind, reason, holds := e.admissionBlockerFor(c, ahead, holder, false, mem, hasMem)
		if isPreemptibleBlocker(kind) {
			if victims := e.preemptCandidatesLocked(c); len(victims) > 0 {
				cID, cPrio := c.cmd().Id, c.priority()
				e.mu.Unlock()
				parkedAny := false
				for _, v := range victims {
					e.mu.Lock()
					if c.cmd().State != queued || c.stopping || c.intervening {
						e.mu.Unlock()
						break
					}
					vArgv, vPrio := v.cmd().Argv, v.priority()
					e.mu.Unlock()
					pReason := fmt.Sprintf("preempted by higher-priority command %s (priority %d > %d)", cID, cPrio, vPrio)
					pFeed := fmt.Sprintf("park %s · preempted by %s (priority %d > %d)", vArgv, cID, cPrio, vPrio)
					if e.parkOne(ctx, v, pReason, pFeed, cID) {
						parkedAny = true
						if m, err := e.cfg.System.Meminfo(); err == nil && m.Total > 0 {
							mem, hasMem = m, true
						}
					}
					e.mu.Lock()
					if c.cmd().State != queued || c.stopping || c.intervening {
						e.mu.Unlock()
						break
					}
					kind, reason, holds = e.admissionBlockerFor(c, ahead, holder, parkedAny, mem, hasMem)
					e.mu.Unlock()
					if kind == "" || !isPreemptibleBlocker(kind) {
						break
					}
				}
				e.mu.Lock()
				if c.cmd().State != queued || c.stopping || c.intervening {
					e.mu.Unlock()
					continue
				}
				kind, reason, holds = e.admissionBlockerFor(c, ahead, holder, parkedAny, mem, hasMem)
			}
		}
		if kind != "" {
			// The numbers change every step and go in heartbeats; the kind of
			// wait is an event.
			c.cmd().Reason = reason
			if kind != c.waiting {
				c.waiting = kind
				ch := change{what: "waiting", detail: reason}
				if kind != "preparing" {
					ch.feed = feed(feedWait, fmt.Sprintf("wait %s · %s", c.cmd().Argv, reason), ref("command", c.cmd().Id))
				}
				e.saveCommand(c, ch)
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
		spec, err := e.spec(c)
		volPath := e.commandVolPath(c)
		cmdID := c.cmd().Id
		isBench := c.cmd().Bench
		useRT := e.usesRuntime(c.runtimeTarget()) || c.isVMFreshen()
		var beforeStart func(context.Context) error
		var onStartFail func()
		if c.managedFreshen != nil {
			beforeStart = c.managedFreshen.beforeStart
			onStartFail = c.managedFreshen.onStartFail
		}
		var admitDone chan struct{}
		if err == nil {
			c.admitting = true
			c.admitStopErr = nil
			admitDone = make(chan struct{})
			c.admitDone = admitDone
		}
		e.mu.Unlock()
		finishAdmit := func() {
			if admitDone != nil {
				c.admitting = false
				c.admitDone = nil
				close(admitDone)
			}
		}
		madeTmp := false
		if err == nil && !useRT {
			if err = e.makeTmp(c); err != nil {
				err = fmt.Errorf("make its tmp: %w", err)
			} else {
				madeTmp = true
			}
		}
		if err == nil {
			// If Stop cancelled c or its workspace became terminal before or
			// during makeTmp, remove the newly created tmpDir and skip starting
			// the unit.
			e.mu.Lock()
			termState, termWhy, termWS := e.terminalWorkspaceForCommand(c)
			cancelledEarly := c.cmd().State != queued || c.stopping || termWS
			e.mu.Unlock()
			if cancelledEarly {
				if madeTmp {
					removeTmp(volPath, cmdID)
				}
				e.mu.Lock()
				stopping, stopBy := c.stopping, c.stopBy
				c.stopping, c.stopBy = false, nil
				if api.Live(c.cmd().State) {
					if termWS && !stopping {
						e.end(c, termState, nil, termWhy)
					} else {
						e.endBy(c, cancelled, nil, c.stopWhy, stopBy)
					}
				}
				finishAdmit()
				e.mu.Unlock()
				continue
			}
		}
		ranBeforeStart := false
		if err == nil && beforeStart != nil {
			if err = beforeStart(ctx); err == nil {
				ranBeforeStart = true
				e.mu.Lock()
				cancelledAfterBefore := c.cmd().State != queued || c.stopping
				e.mu.Unlock()
				if cancelledAfterBefore {
					if onStartFail != nil {
						onStartFail()
					}
					if madeTmp {
						removeTmp(volPath, cmdID)
					}
					e.mu.Lock()
					stopBy := c.stopBy
					c.stopping, c.stopBy = false, nil
					if api.Live(c.cmd().State) {
						e.endBy(c, cancelled, nil, c.stopWhy, stopBy)
					}
					finishAdmit()
					e.mu.Unlock()
					continue
				}
			}
		}
		if err == nil {
			err = e.stopLeftover(ctx, c)
		}
		if err == nil && isBench {
			e.holdLoud(ctx, c)
			if err = e.cfg.Bench.On(ctx); err != nil {
				err = fmt.Errorf("isolate the node: %w", err)
			}
		}
		if err == nil {
			err = e.cfg.Units.Start(ctx, spec)
		}
		if err != nil && ranBeforeStart && onStartFail != nil {
			onStartFail()
		}
		e.mu.Lock()
		if err != nil {
			slog.Error("start command", "command", cmdID, "err", err)
			stopping := c.stopping
			alreadyEnded := !api.Live(c.cmd().State)
			if !alreadyEnded && !stopping {
				e.end(c, failed, nil, "could not start: "+err.Error())
				finishAdmit()
				e.mu.Unlock()
				continue
			}
			e.mu.Unlock()
			if madeTmp {
				removeTmp(volPath, cmdID)
			}
			e.mu.Lock()
			stopBy := c.stopBy
			c.stopping, c.stopBy = false, nil
			if stopping && api.Live(c.cmd().State) {
				e.endBy(c, cancelled, nil, c.stopWhy, stopBy)
			}
			finishAdmit()
			e.mu.Unlock()
			continue
		}
		cmd := c.cmd()
		if cmd.State != queued || c.stopping {
			// Stopped while it started.
			e.mu.Unlock()
			if stopErr := e.cfg.Units.Stop(ctx, c.unitName()); stopErr != nil {
				slog.Warn("stop a command stopped while it started", "command", cmdID, "err", stopErr)
				// Keep the command running (non-terminal) and preserve its tmpDir
				// so a subsequent Stop retry calls Units.Stop again and cleans up
				// once the unit actually stops.
				e.mu.Lock()
				c.stopping, c.stopBy = false, nil
				c.admitStopErr = stopErr
				now := e.nowMs()
				cmd.State = running
				cmd.StartedAt = &now
				detail := "stop failed while starting: " + stopErr.Error()
				cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: "admitted", Detail: detail})
				e.saveCommand(c, change{what: "admitted", detail: detail})
				finishAdmit()
				e.watchExitBgLocked(ctx, spec.Slice, c.unitName())
				e.mu.Unlock()
				continue
			}
			if ranBeforeStart && onStartFail != nil {
				onStartFail()
			}
			if madeTmp {
				removeTmp(volPath, cmdID)
			}
			e.mu.Lock()
			stopBy := c.stopBy
			c.stopping, c.stopBy = false, nil
			c.admitStopErr = nil
			if api.Live(cmd.State) {
				e.endBy(c, cancelled, nil, c.stopWhy, stopBy)
			}
			finishAdmit()
			e.mu.Unlock()
			continue
		}
		now := e.nowMs()
		cmd.State = running
		cmd.StartedAt = &now
		finishAdmit()
		share := fmt.Sprintf("weight %d", cmd.Limits.CpuWeight)
		if cmd.Bench {
			share = "measurement on CPUs " + e.cfg.Bench.CPUs().String()
		}
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: "admitted", Detail: fmt.Sprintf("%s · low %s · high %s", share, limit(cmd.Limits.MemoryLowBytes), limit(cmd.Limits.MemoryHighBytes))})
		ch := change{what: "admitted"}
		switch {
		case cmd.Bench:
			ch.feed = feed(feedAdmit, fmt.Sprintf("measure %s · %s on CPUs %s", cmd.Argv, cmd.WorkspaceId, e.cfg.Bench.CPUs()), ref("command", cmd.Id))
		case cmd.Kind != "freshen":
			ch.feed = feed(feedAdmit, fmt.Sprintf("admit %s · %s", cmd.Argv, cmd.WorkspaceId), ref("command", cmd.Id))
		}
		e.saveCommand(c, ch)
		e.watchExitBgLocked(ctx, spec.Slice, c.unitName())
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

// terminalWorkspaceForCommand reports whether c belongs to a workspace that
// can never run it (failed, released, or missing) and the terminal command
// state and reason to end c with. Call with e.mu held.
func (e *Engine) terminalWorkspaceForCommand(c *command) (pb.CommandState, string, bool) {
	wsID := c.cmd().WorkspaceId
	if wsID == "" {
		return 0, "", false // freshen commands run on a base, not a workspace
	}
	w, ok := e.workspaces[wsID]
	if !ok {
		return failed, fmt.Sprintf("its workspace %s no longer exists", wsID), true
	}
	switch w.ws().State {
	case failedWS:
		return failed, "its workspace failed: " + w.ws().Reason, true
	case released:
		return cancelled, "workspace released", true
	default:
		return 0, "", false
	}
}

// isPreemptibleBlocker reports whether a blocker kind can be cleared by
// parking strictly lower-priority running commands (freeing memory and CPU).
// Host starvation and low disk are never bypassed by preemption.
func isPreemptibleBlocker(kind string) bool {
	return kind == "memory" || kind == "pressure" || kind == "cpu"
}

// preemptCandidatesLocked returns strictly lower-priority running (non-bench,
// non-server) commands ordered lowest priority first, work before base builds,
// then newest first. Returns nil if there are no candidates or if parking all
// of them would still not leave enough memory reservation room for c.
// Call with e.mu held.
func (e *Engine) preemptCandidatesLocked(c *command) []*command {
	cPrio := c.priority()
	var victims []*command
	var reclaimable int64
	for _, v := range e.commands {
		if v.stopping || v.deactivating || v.intervening || v.cmd().State != running {
			continue
		}
		if v.cmd().Bench || v.cmd().Kind == "server" {
			continue
		}
		if v.priority() < cPrio {
			victims = append(victims, v)
			reclaimable += v.reservation()
		}
	}
	if len(victims) == 0 {
		return nil
	}
	// Do not disturb running work if c could not fit even after parking every
	// lower-priority victim (e.g. a bench or higher-priority command holds RAM).
	if c.reservation() > e.admitLimit()-(e.reservedFor(cPrio)-reclaimable) {
		return nil
	}
	slices.SortFunc(victims, compareParkVictim)
	return victims
}

// compareParkVictim orders running commands by which should be parked first:
// lowest priority first, work before base builds, newest first.
func compareParkVictim(a, b *command) int {
	return cmp.Or(
		cmp.Compare(a.priority(), b.priority()),
		cmpBool(a.isFreshen(), b.isFreshen()),
		cmp.Compare(b.cmd().GetStartedAt(), a.cmd().GetStartedAt()),
		cmp.Compare(b.cmd().Id, a.cmd().Id),
	)
}

// compareThawOrder orders parked commands by which should thaw first:
// highest priority first, base builds before work, longest parked first.
func compareThawOrder(a, b *command) int {
	return cmp.Or(
		cmp.Compare(b.priority(), a.priority()),
		cmpBool(b.isFreshen(), a.isFreshen()),
		cmp.Compare(a.rec.ParkedAt, b.rec.ParkedAt),
		cmp.Compare(a.cmd().Id, b.cmd().Id),
	)
}

// admissionBlocker says why c cannot start now, as a kind (preparing,
// measurement, behind-measurement, quiet, host-memory, host-cpu, disk, memory,
// pressure, cpu) and a reason with the numbers as they are, or "" if it can;
// and whether a measurement's reason holds up the work queued after it: it
// waits for room that work would take. ahead is the oldest measurement still
// waiting, holder one that holds up work. Call with e.mu held.
func (e *Engine) admissionBlocker(c, ahead, holder *command) (kind, reason string, holds bool) {
	return e.admissionBlockerFor(c, ahead, holder, false, sysinfo.Mem{}, false)
}

func (e *Engine) admissionBlockerFor(c, ahead, holder *command, cpuPreempted bool, mem sysinfo.Mem, hasMem bool) (kind, reason string, holds bool) {
	if e.systemUpgrading {
		return "upgrade", "waiting: node is upgrading cladefs", true
	}
	if wsID := c.cmd().WorkspaceId; wsID != "" {
		w, ok := e.workspaces[wsID]
		if !ok {
			return "workspace-missing", fmt.Sprintf("its workspace %s no longer exists", wsID), false
		}
		switch w.ws().State {
		case failedWS:
			return "workspace-failed", "its workspace failed: " + w.ws().Reason, false
		case released:
			return "workspace-released", "workspace released", false
		case preparing:
			return "preparing", w.ws().Reason, false
		}
		if w.releasing {
			return "releasing", "workspace is being released", false
		}
		if isSyncInProgress(w.ws().Reason) {
			return "syncing", "workspace is syncing", false
		}
	}
	switch {
	case c.cmd().Bench:
		if r := e.benchBlocker(ahead); r != "" {
			return "measurement", r, false
		}
	case holder != nil:
		return "behind-measurement", fmt.Sprintf("waiting behind measurement %s, which waits for room", holder.cmd().Id), false
	default:
		if r := e.quietBlocker(c); r != "" {
			return "quiet", r, false
		}
	}
	if k, r := e.roomFor(c, cpuPreempted, mem, hasMem); r != "" {
		// Node-wide holds (host starvation, full disk) already block every
		// command on their own; do not mislabel subsequent work as waiting
		// behind a measurement.
		holds := k != "host-cpu" && k != "host-memory" && k != "disk"
		return k, r, holds
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
	return e.roomFor(c, false, sysinfo.Mem{}, false)
}

// roomFor checks node capacity for c. When cpuPreempted is true, c has just
// parked a strictly lower-priority running command to yield CPU, so a stale
// guest CPU pressure hold does not keep c waiting behind the work it just
// froze. Memory pressure is NEVER bypassed by preemption: memory PSI reflects
// host-wide thrashing (including servers or external workloads) and admission
// must wait for a fresh sample under admitPressure. Host starvation, disk,
// memory reservations (including unreclaimed/resident pages of parked victims),
// and physical memory headroom still apply. Call with e.mu held.
func (e *Engine) roomFor(c *command, cpuPreempted bool, mem sysinfo.Mem, hasMem bool) (kind, reason string) {
	hu := e.freshHostUsage(e.now())
	if memHeld, memReason := hostMemoryStarved(hu); memHeld {
		return "host-memory", "waiting: " + memReason
	}
	if cpuHeld, _, _, _, cpuReason, _ := e.hostCPUStatus(hu); cpuHeld {
		return "host-cpu", "waiting: " + cpuReason
	}
	if e.diskLow() {
		return "disk", fmt.Sprintf("waiting for disk: %s free", gib(e.disk.FreeBytes))
	}
	want := c.reservation()
	limit := e.admitLimit()
	reserved := e.reservedFor(c.priority())
	room := limit - reserved
	if want > room {
		if room < 0 {
			if want > 0 {
				return "memory", fmt.Sprintf("waiting for memory: needs %s, but running and parked commands hold or are guaranteed %s (limit %s)", gib(want), gib(reserved), gib(limit))
			}
			return "memory", fmt.Sprintf("waiting for memory: running and parked commands hold or are guaranteed %s (limit %s)", gib(reserved), gib(limit))
		}
		return "memory", fmt.Sprintf("waiting for memory: needs %s, %s left of what running and parked commands hold or are guaranteed", gib(want), gib(room))
	}
	var memTotal, memUsed int64
	if hasMem {
		memTotal, memUsed = mem.Total, mem.Used()
	} else if e.lastMem.Total > 0 {
		memTotal, memUsed = e.lastMem.Total, e.lastMem.Used()
	} else if t := e.resources.GetMemoryBytes(); t > 0 && e.usage.GetMemoryUsedBytes() > 0 {
		memTotal, memUsed = t, e.usage.GetMemoryUsedBytes()
	}
	// Commands targeting a VM workspace run inside an already-allocated guest
	// whose RAM is bounded by its Firecracker memMiB. Once the guest faults in
	// its RAM, its own resident pages can hold host memory above parkAt even
	// when the node is idle with zero memory pressure; blocking new commands in
	// that VM on host parkAt/physRoom would permanently self-deadlock the VM.
	// Host memory starvation (hostMemoryStarved), real memory pressure
	// (admitPressure), and intra-hotbed reservations (want > room) still apply.
	if memTotal > 0 && !e.usesRuntime(c.cmd().WorkspaceId) {
		physRoom := memTotal - memUsed
		if float64(memUsed) > float64(memTotal)*parkAt {
			return "memory", fmt.Sprintf("waiting for memory: %.0f%% in use (%s physically available on the node)", 100*float64(memUsed)/float64(memTotal), gib(max(0, physRoom)))
		}
		// A freshen's MemoryLowBytes is an intra-hotbed concurrency reservation
		// sized from an earlier build's PeakHeldBytes (anon + swap, capped at
		// admitLimit), not a requirement that all of it sit free in physical
		// RAM before starting: OS/kernel/ZFS ARC overhead or a shared host
		// routinely keeps idle MemAvailable below admitMemory of RAM, so
		// requiring want <= physRoom would deadlock base builds on idle nodes
		// (board 11078). Explicit caller --mem-low guarantees still verify
		// physical headroom.
		if !c.isFreshen() && want > physRoom {
			return "memory", fmt.Sprintf("waiting for memory: needs %s, %s physically available on the node", gib(want), gib(max(0, physRoom)))
		}
	}
	switch {
	case e.usage.GetMemoryPressure() >= admitPressure:
		return "pressure", fmt.Sprintf("waiting: memory pressure %.0f%%", e.usage.GetMemoryPressure())
	case !cpuPreempted && e.cpu.holding && !c.cmd().Bench:
		return "cpu", fmt.Sprintf("waiting: CPU pressure %.0f%% (peak %.0f%% in the last %s); the node holds new work until it is under %.0f%%", e.usage.GetCpuPressure(), e.cpu.peak(), cpuWindow, cpuResumeAt)
	}
	return "", ""
}

// spec is the unit for c. Call with e.mu held.
func (e *Engine) spec(c *command) (unit.Spec, error) {
	cmd := c.cmd()
	logPath, err := e.logPath(cmd.Id)
	if err != nil {
		return unit.Spec{}, err
	}
	vmFreshen := c.managedFreshen != nil && c.managedFreshen.vmID != ""
	var tmpDir, runDir string
	if !vmFreshen {
		if tmpDir, err = e.tmpDir(c); err != nil {
			return unit.Spec{}, err
		}
		if runDir, err = e.runtimeDir(c); err != nil {
			return unit.Spec{}, err
		}
	}
	volPath := e.commandVolPath(c)
	runtimeTarget := e.runtimeChrootDir()
	env := append(cacheEnv(), "XDG_RUNTIME_DIR="+runtimeTarget)
	env = append(env, c.rec.Env...)
	env = append(env, "HOTBED_WORKSPACE="+cmd.WorkspaceId, "HOTBED_COMMAND="+cmd.Id)
	if vmFreshen {
		env = append(env, "HOTBED_RUNTIME_TARGET="+c.managedFreshen.vmID)
	}
	props := slices.Clone(e.cfg.UnitProperties)
	if cmd.Bench {
		// Measurements run at normal CPU and IO priority: on CPUs of their
		// own they compete only with the kernel's per-CPU threads, which a
		// niced or idle-scheduled measurement yields to.
		props = slices.DeleteFunc(props, isPriorityProperty)
	} else {
		// Normal work yields to desktop and system processes, unless the
		// worker's config overrides a property explicitly.
		if !hasProperty(props, "Nice=") {
			props = append(props, "Nice=19")
		}
		if !hasProperty(props, "IOSchedulingClass=") {
			props = append(props, "IOSchedulingClass=idle")
		}
	}
	spec := unit.Spec{
		Name:         c.unitName(),
		Slice:        e.sliceOf(c),
		Argv:         cmd.Args,
		Dir:          c.rec.Dir,
		Tmp:          tmpDir,
		User:         e.cfg.User,
		Env:          env,
		PathPrefix:   cachePath,
		CPUWeight:    int(cmd.Limits.CpuWeight),
		MemoryLow:    cmd.Limits.MemoryLowBytes,
		MemoryHigh:   cmd.Limits.MemoryHighBytes,
		Log:          logPath,
		NoSwap:       cmd.Bench,
		Capabilities: []string{"CAP_PERFMON"},
		TraceFS:      true,
		Properties:   props,
	}
	if !vmFreshen {
		spec.Binds = []unit.Bind{{Source: runDir, Target: runtimeTarget}}
		e.inRootfs(&spec, volPath)
	}
	return spec, nil
}

// inRootfs makes spec run chrooted in volPath: its home there, the scratch
// disk at its own path, the worker's containers.conf defaults, and the worker's
// git credentials bound read-only.
func (e *Engine) inRootfs(spec *unit.Spec, volPath string) {
	spec.Env = append(spec.Env, "HOME="+e.chrootHome())
	spec.RootDir = volPath
	if d := e.cfg.ScratchDisk; d != "" {
		spec.Binds = append(spec.Binds, unit.Bind{Source: d, Target: d})
	}
	if e.containersConf != "" {
		spec.ReadOnlyBinds = append(spec.ReadOnlyBinds, unit.Bind{Source: e.containersConf, Target: containersConfChrootPath})
	}
	if e.gitAuth != nil {
		spec.ReadOnlyBinds = append(spec.ReadOnlyBinds, unit.Bind{Source: e.gitAuth.Dir(), Target: gitAuthChrootDir})
		spec.Env = append(e.gitAuth.chrootEnv(), spec.Env...)
		spec.PathPrefix = append([]string{gitAuthChrootDir + "/bin"}, spec.PathPrefix...)
	}
}

func (e *Engine) runtimeChrootDir() string {
	if e.cred != nil {
		return fmt.Sprintf("/run/user/%d", e.cred.Uid)
	}
	if e.cfg.BaseImage != nil {
		return fmt.Sprintf("/run/user/%d", e.cfg.BaseImage.Spec.Normalized().UID)
	}
	return "/run/user/1000"
}

func (e *Engine) chrootHome() string {
	if e.cfg.BaseImage != nil {
		return e.cfg.BaseImage.Spec.Normalized().Home
	}
	return WorkPath + "/.hotbed/home"
}

func isPriorityProperty(p string) bool {
	return strings.HasPrefix(p, "Nice=") ||
		strings.HasPrefix(p, "IOSchedulingClass=") ||
		strings.HasPrefix(p, "IOSchedulingPriority=") ||
		strings.HasPrefix(p, "CPUSchedulingPolicy=")
}

func hasProperty(props []string, prefix string) bool {
	return slices.ContainsFunc(props, func(p string) bool { return strings.HasPrefix(p, prefix) })
}

// park freezes the newest running command when memory runs short, and
// thaws the parked command first in line when there is room for it again.
// Memory runs short when too much of it is in use, or when tasks stall
// waiting for it (memory pressure past parkPressure) while what is in use
// looks fine: the kernel counts file pages as available that linkers and
// compilers have mapped and fault straight back in, so the node thrashes
// with a quarter of its memory "available" (board 9128). While tasks stall
// so, nothing is thawed either. Nothing is killed for memory. Parking frees memory for the commands left
// running, so it never freezes the last of them, and with none left running
// it thaws the first in line whatever memory says: else every command could
// sit frozen, waiting for room that only running ones could make. A
// measurement is never parked, but counts among those left running.
//
// A base build is parked last and thawed first: every workspace that wants
// its base waits on it. And nothing waits for good: one first in line
// longer than rotateAfter (a base build at once) trades places with the
// newest running command, though its own swap (what parking it pushed out)
// would never fit beside the others (board 7422).
func (e *Engine) park(ctx context.Context) {
	mem, err := e.cfg.System.Meminfo()
	if err != nil {
		slog.Warn("read memory", "err", err)
		return
	}
	total := float64(mem.Total)
	pct := 100 * float64(mem.Used()) / total
	of := "memory"
	if mem.Domain != "" {
		of = "of " + mem.Domain + "'s memory"
	}
	pressure, err := e.cfg.System.Pressure("memory")
	if err != nil {
		slog.Warn("read memory pressure", "err", err)
	}

	e.mu.Lock()
	var runningCmds, parkedCmds []*command
	measuring := 0
	for _, c := range e.commands {
		if c.stopping || c.deactivating || c.intervening {
			continue
		}
		switch s := c.cmd().State; {
		case s == running && c.cmd().Bench:
			measuring++
		case s == running && c.cmd().Kind != "server":
			runningCmds = append(runningCmds, c)
		case s == parked && c.rec.HeldFor == "":
			// One held beside a measurement is thawed once it ends.
			parkedCmds = append(parkedCmds, c)
		}
	}
	// Victims first: lowest priority, work before base builds, newest first.
	slices.SortFunc(runningCmds, compareParkVictim)
	// First in line: highest priority, base builds before work, longest parked.
	slices.SortFunc(parkedCmds, compareThawOrder)
	nowTime := e.now()
	now := nowTime.UnixMilli()

	canPark := len(runningCmds) > 0 && len(runningCmds)+measuring > 1
	// Pressure is an average over 10 s: one park shows in it only after
	// that, so the next waits for pressureSettle.
	stalling := pressure >= parkPressure && nowTime.Sub(e.pressureParkedAt) >= pressureSettle
	var parkVictim, thawTarget *command
	var parkReason, parkFeed, thawWhy string
	switch {
	case float64(mem.Used()) > total*parkAt && canPark:
		parkVictim = runningCmds[0]
		parkReason = fmt.Sprintf("frozen at %.0f%% %s in use; its memory goes to swap until there is room", pct, of)
		parkFeed = fmt.Sprintf("park %s · %s at %.0f%%", parkVictim.cmd().Argv, cmp.Or(mem.Domain, "memory"), pct)
	case stalling && canPark:
		parkVictim = runningCmds[0]
		e.pressureParkedAt = nowTime
		parkReason = fmt.Sprintf("frozen at memory pressure %.0f%% (%.0f%% %s in use); its memory goes to swap until there is room", pressure, pct, of)
		parkFeed = fmt.Sprintf("park %s · memory pressure %.0f%%", parkVictim.cmd().Argv, pressure)
	case len(parkedCmds) > 0 && len(runningCmds)+measuring == 0:
		for _, c := range parkedCmds {
			if !e.thawBlockedByPriority(c, true, mem) {
				thawTarget = c
				break
			}
		}
	case len(parkedCmds) > 0 && pressure < admitPressure && !e.cpu.holding && !e.isHostStarved(nowTime):
		var first *command
		for _, c := range parkedCmds {
			if !e.thawBlockedByPriority(c, false, mem) {
				first = c
				break
			}
		}
		if first != nil {
			if float64(mem.Used()+first.cmd().SwapBytes) <= total*thawBelow &&
				e.reservedFor(first.priority()) <= e.admitLimit() {
				thawTarget = first
			} else if len(runningCmds) > 0 {
				victim := runningCmds[0]
				// Never rotate out a strictly higher-priority running command,
				// nor a same-priority base build. A strictly higher-priority
				// parked command (or a base build) takes its turn at once.
				canRotateVictim := victim.priority() < first.priority() ||
					(victim.priority() == first.priority() && !victim.isFreshen())
				wait := rotateAfter
				if first.isFreshen() || victim.priority() < first.priority() {
					wait = 0
				}
				if canRotateVictim && time.Duration(now-first.rec.ParkedAt)*time.Millisecond >= wait {
					parkVictim = victim
					thawTarget = first
					waited := (time.Duration(now-first.rec.ParkedAt) * time.Millisecond).Round(time.Second)
					parkReason = fmt.Sprintf("frozen to give %s its turn (parked %s); its memory goes to swap until there is room", first.cmd().Id, waited)
					parkFeed = fmt.Sprintf("park %s · %s's turn", parkVictim.cmd().Argv, first.cmd().Id)
					thawWhy = fmt.Sprintf(", its turn: %s frozen in its place", parkVictim.cmd().Id)
				}
			}
		}
	}
	e.mu.Unlock()

	if parkVictim != nil {
		if !e.parkOne(ctx, parkVictim, parkReason, parkFeed, "") {
			return
		}
	}
	if thawTarget != nil {
		e.thawOne(ctx, thawTarget, thawWhy)
	}
}

// canQueuedMakeProgress reports whether queued command q can realistically be
// admitted on this node rather than deadlocking lower-priority thaws behind an
// impossible request (e.g. MemoryLowBytes > admitLimit, or on an idle node
// where q still cannot be admitted even with 0 running commands).
// Call with e.mu held.
func (e *Engine) canQueuedMakeProgress(q *command, idle bool, mem sysinfo.Mem) bool {
	if q.cmd().State != queued || q.stopping {
		return false
	}
	if wsID := q.cmd().WorkspaceId; wsID != "" {
		w, ok := e.workspaces[wsID]
		if !ok || w.releasing || w.ws().State == preparing || w.ws().State == failedWS || w.ws().State == released || isSyncInProgress(w.ws().Reason) {
			return false
		}
	}
	if lim := e.admitLimit(); lim > 0 && q.reservation() > lim {
		return false
	}
	if idle {
		kind, _, _ := e.admissionBlockerFor(q, nil, nil, false, mem, mem.Total > 0)
		// With 0 running commands, only transient guest pressure or an
		// immediately admittable command should keep the last runnable parked
		// command frozen; impossible memory, disk, or host holds must not
		// leave the node idle with its last runnable command frozen forever.
		return kind == "" || kind == "pressure" || kind == "cpu"
	}
	return true
}

// hasHigherPriorityQueued reports whether any queued command that can make
// progress has strictly higher priority than prio. Call with e.mu held.
func (e *Engine) hasHigherPriorityQueued(prio int32, idle bool, mem sysinfo.Mem) bool {
	for _, c := range e.commands {
		if c.priority() <= prio {
			continue
		}
		if e.canQueuedMakeProgress(c, idle, mem) {
			return true
		}
	}
	return false
}

// thawBlockedByPriority reports whether parked command c must remain parked
// because the higher-priority command that preempted it is still running or
// queued and able to make progress, or because another strictly higher-priority
// command that can make progress is queued. Call with e.mu held.
func (e *Engine) thawBlockedByPriority(c *command, idle bool, mem sysinfo.Mem) bool {
	if c.preemptedBy != "" {
		if p, ok := e.commands[c.preemptedBy]; ok {
			if p.cmd().State == running {
				return true
			}
			if p.cmd().State == queued && e.canQueuedMakeProgress(p, false, mem) {
				return true
			}
		}
		c.preemptedBy = ""
		if err := e.db.deletePreemption(c.cmd().Id); err != nil {
			slog.Warn("clear stale preemption record", "command", c.cmd().Id, "err", err)
		}
	}
	return e.hasHigherPriorityQueued(c.priority(), idle, mem)
}

// rotateAfter is how long a parked command waits for room before it trades
// places with the newest running one.
const rotateAfter = 10 * time.Minute

func (c *command) isFreshen() bool { return c.cmd().Kind == "freshen" }

// cmpBool orders false before true.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

// reconcileInactiveUnit checks whether c's unit is no longer active (gone,
// exited, or deactivating) after Freeze or Thaw failed. When it is, it settles
// an exited/gone unit or marks c deactivating and returns true so callers do
// not log a spurious error or retry in a loop (board 7695).
func (e *Engine) reconcileInactiveUnit(ctx context.Context, c *command) bool {
	st, err := e.cfg.Units.Status(ctx, c.unitName())
	sampledAt := e.now()
	if err != nil || st.Running {
		return false
	}
	if !st.Loaded || st.Exited {
		e.removeUnit(ctx, c, st)
		e.observe(ctx, c, st, sampledAt)
		return true
	}
	e.mu.Lock()
	if api.Live(c.cmd().State) && !c.stopping {
		c.deactivating = true
	}
	e.mu.Unlock()
	return true
}

// parkOne freezes victim and pushes its memory to swap, saying why, and
// says whether it did. preemptedBy, if non-empty, names the higher-priority
// command that preempted victim.
func (e *Engine) parkOne(ctx context.Context, victim *command, reason, feedText, preemptedBy string) bool {
	victim.freezeMu.Lock()
	defer victim.freezeMu.Unlock()
	e.mu.Lock()
	if victim.stopping || victim.intervening || victim.cmd().State != running {
		e.mu.Unlock()
		return false
	}
	unitName, id := victim.unitName(), victim.cmd().Id
	e.mu.Unlock()
	if err := e.cfg.Units.Freeze(ctx, unitName); err != nil {
		if !e.reconcileInactiveUnit(ctx, victim) {
			slog.Error("park", "command", id, "err", err)
		}
		return false
	}
	e.mu.Lock()
	cmd := victim.cmd()
	if victim.stopping || cmd.State != running {
		// Stopped or exited while Freeze was in flight: never resurrect it,
		// and never thaw a command that transitioned to paused/parked.
		shouldThaw := victim.stopping && cmd.State == running
		e.mu.Unlock()
		if shouldThaw {
			if err := e.cfg.Units.Thaw(ctx, unitName); err != nil {
				slog.Warn("thaw a command no longer to park", "command", id, "err", err)
			}
		}
		return false
	}
	now := e.nowMs()
	cmd.State = parked
	cmd.CpuCores = 0
	cmd.Reason = reason
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: "parked", Detail: reason})
	victim.rec.Parks++
	victim.rec.ParkedAt = now
	victim.preemptedBy = preemptedBy
	victim.reclaimed = false
	e.saveCommand(victim, change{what: "parked", detail: reason, feed: feed(feedPark, feedText, ref("command", cmd.Id))})
	cgroup, reclaimTarget, residentKnown := victim.rec.Cgroup, max(cmd.MemoryBytes, victim.residentBytes), victim.residentKnown
	e.mu.Unlock()
	if cgroup == "" {
		if st, err := e.cfg.Units.Status(ctx, unitName); err == nil && st.Cgroup != "" {
			cgroup = st.Cgroup
			reclaimTarget = max(reclaimTarget, st.Memory)
			e.mu.Lock()
			if victim.rec.Cgroup == "" {
				victim.rec.Cgroup = cgroup
			}
			e.mu.Unlock()
		}
	}
	if cgroup != "" {
		if !residentKnown {
			if m, err := e.cfg.Units.Memory(cgroup); err == nil {
				reclaimTarget = max(reclaimTarget, m.Anon)
			}
		}
		// Push the frozen command's memory to swap so others can use it, then
		// measure what actually remains resident in RAM so higher-priority
		// admission never drops the charge for unreclaimed or partial pages.
		if err := e.cfg.Units.Reclaim(cgroup, reclaimTarget); err != nil {
			slog.Warn("reclaim parked command", "command", id, "err", err)
		} else if m, err := e.cfg.Units.Memory(cgroup); err != nil {
			slog.Warn("read parked command memory after reclaim", "command", id, "err", err)
		} else {
			e.mu.Lock()
			if victim.cmd().State == parked {
				victim.cmd().SwapBytes = m.Swap
				victim.residentBytes = m.Anon
				victim.residentKnown = true
				victim.reclaimed = true
				victim.rec.PeakHeldBytes = max(victim.rec.PeakHeldBytes, m.Held())
			}
			e.mu.Unlock()
		}
	}
	return true
}

// thawOne thaws a parked command; why, if not "", says more of why now.
func (e *Engine) thawOne(ctx context.Context, c *command, why string) {
	c.freezeMu.Lock()
	defer c.freezeMu.Unlock()
	e.mu.Lock()
	if c.stopping || c.intervening || c.cmd().State != parked {
		e.mu.Unlock()
		return
	}
	cmd := c.cmd()
	slept := (time.Duration(e.nowMs()-c.rec.ParkedAt) * time.Millisecond).Round(time.Second)
	detail := "after " + slept.String() + why
	if err := e.thawLocked(ctx, c, change{
		what: "thawed", detail: detail,
		feed: feed(feedThaw, fmt.Sprintf("thaw %s %s", cmd.Argv, detail), ref("command", cmd.Id)),
	}); err != nil {
		slog.Error("thaw", "command", cmd.Id, "err", err)
	}
}

// reap stops attached commands whose client went away: no one has followed
// their output for orphanAfter. A paused one is left to its hard limit: its
// client went because it was paused, and may wait on it again.
func (e *Engine) reap(ctx context.Context) {
	e.mu.Lock()
	var orphans []string
	for _, c := range e.commands {
		if c.cmd().Attached && api.Live(c.cmd().State) && c.cmd().State != paused && !c.intervening && c.followers == 0 && e.now().Sub(c.lastFollow) > orphanAfter {
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

// limit holds commands to their time limits. One past its hard limit is
// stopped (even if a hand intervention or diagnostic capture is in flight:
// hard-limit expiry takes precedence and cancels in-flight diagnostics). One
// that ran its soft limit without ending, counted from its start or from when
// an agent last began waiting on it, is paused: frozen, its memory pushed to
// swap, until an agent waits on it again (WaitOn). A measurement is never
// paused.
func (e *Engine) limit(ctx context.Context) {
	e.mu.Lock()
	now := e.nowMs()
	var stop, pause []*command
	var cancelDiags []context.CancelFunc
	for _, c := range e.commands {
		cmd := c.cmd()
		if !started(cmd.State) || cmd.StartedAt == nil || c.stopping {
			continue
		}
		if hard := cmd.Limits.GetHardLimitSeconds(); hard > 0 && now-cmd.GetStartedAt() >= hard*1000 {
			if c.cancelDiag != nil {
				cancelDiags = append(cancelDiags, c.cancelDiag)
			}
			stop = append(stop, c)
			continue
		}
		if c.intervening {
			continue
		}
		if soft := cmd.Limits.GetSoftLimitSeconds(); soft > 0 && cmd.State == running && !cmd.Bench && !c.deactivating &&
			now-max(cmd.GetStartedAt(), c.rec.WaitedAt) >= soft*1000 {
			pause = append(pause, c)
		}
	}
	e.mu.Unlock()
	for _, cancel := range cancelDiags {
		cancel()
	}
	for _, c := range stop {
		hard := time.Duration(c.cmd().Limits.GetHardLimitSeconds()) * time.Second
		if _, err := e.Stop(ctx, c.cmd().Id, fmt.Sprintf("its hard time limit of %s passed", hard)); err != nil {
			slog.Error("stop a command past its hard time limit", "command", c.cmd().Id, "err", err)
		}
	}
	for _, c := range pause {
		e.pause(ctx, c)
	}
}

// thawStrays thaws the units of commands recorded as running that are
// frozen: a worker that stopped between freezing one (to park or pause it)
// and recording that left it so, and nothing else would ever thaw it.
func (e *Engine) thawStrays(ctx context.Context) {
	e.mu.Lock()
	var runs []*command
	for _, c := range e.commands {
		if c.cmd().State == running {
			runs = append(runs, c)
		}
	}
	e.mu.Unlock()
	for _, c := range runs {
		st, err := e.cfg.Units.Status(ctx, c.unitName())
		if err != nil {
			slog.Warn("read a running command's unit", "command", c.cmd().Id, "err", err)
			continue
		}
		if !st.Frozen {
			continue
		}
		slog.Warn("thaw a running command's unit, frozen by a worker that stopped as it froze it", "command", c.cmd().Id)
		if err := e.cfg.Units.Thaw(ctx, c.unitName()); err != nil {
			slog.Error("thaw a running command's unit", "command", c.cmd().Id, "err", err)
		}
	}
}

// sessionScopedCmd formats a session-scoped hotbed CLI command (such as
// "logs -f" or "stop"), including "-s <session>" whenever the command belongs
// to a session so the hint works verbatim from any thread or checkout.
func sessionScopedCmd(subcmd, sessionID, cmdID string) string {
	if sessionID != "" {
		return fmt.Sprintf("hotbed %s -s %s %s", subcmd, sessionID, cmdID)
	}
	return fmt.Sprintf("hotbed %s %s", subcmd, cmdID)
}

// pause freezes c, which ran its soft time limit without ending (one
// parked for memory is frozen already), and then pushes its memory to swap:
// no one may come back for it.
func (e *Engine) pause(ctx context.Context, c *command) {
	_, err := e.pauseUnit(ctx, c, func(was pb.CommandState) bool {
		cmd := c.cmd()
		soft := cmd.Limits.GetSoftLimitSeconds()
		// A stop that began as it froze, an intervention, a concurrent park, or
		// an agent that began waiting on it meanwhile (WaitOn saw it running):
		// it is not paused after all.
		return !c.stopping && !c.intervening && cmd.State == was && was == running &&
			e.nowMs()-max(cmd.GetStartedAt(), c.rec.WaitedAt) >= soft*1000
	}, func() {
		cmd := c.cmd()
		limit := time.Duration(cmd.Limits.GetSoftLimitSeconds()) * time.Second
		cmd.State = paused
		cmd.CpuCores = 0
		cmd.Reason = fmt.Sprintf("paused: it ran %s, its soft time limit, without ending; waiting on it again (%s) resumes it",
			limit, sessionScopedCmd("logs -f", cmd.SessionId, cmd.Id))
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: "paused", Detail: "after its soft time limit of " + limit.String()})
		c.rec.HeldFor = ""
		c.preemptedBy = ""
		c.reclaimed = false
		e.saveCommand(c, change{
			what: "paused", detail: cmd.Reason,
			feed: feed(feedPause, fmt.Sprintf("pause %s after %s, its soft time limit", cmd.Argv, limit), ref("command", cmd.Id)),
		})
	})
	if err != nil {
		slog.Error("pause", "command", c.cmd().Id, "err", err)
	}
}

// pauseUnit pauses c: it freezes its unit, unless that is frozen already
// (parked or paused), then, under e.mu, asks still whether it is to be
// paused after all, given the state it was in. If so, record records it
// paused, under e.mu, and its memory is pushed to swap; if not, the unit is
// thawed again ONLY if this call transitioned it from running and the command
// is still in running state (never thawing a command that another transition
// already paused or parked). It says whether c was paused.
func (e *Engine) pauseUnit(ctx context.Context, c *command, still func(was pb.CommandState) bool, record func()) (bool, error) {
	c.freezeMu.Lock()
	defer c.freezeMu.Unlock()
	e.mu.Lock()
	was := c.cmd().State
	unitName, id := c.unitName(), c.cmd().Id
	if c.stopping || (was != running && was != parked && was != paused) {
		e.mu.Unlock()
		return false, nil
	}
	e.mu.Unlock()
	frozeUnit := false
	if was == running {
		if err := e.cfg.Units.Freeze(ctx, unitName); err != nil {
			if e.reconcileInactiveUnit(ctx, c) {
				return false, nil
			}
			return false, err
		}
		frozeUnit = true
	}
	e.mu.Lock()
	if !still(was) {
		shouldThaw := frozeUnit && c.cmd().State == running
		e.mu.Unlock()
		if shouldThaw {
			if err := e.cfg.Units.Thaw(ctx, unitName); err != nil {
				slog.Warn("thaw a command no longer to pause", "command", id, "err", err)
			}
		}
		return false, nil
	}
	record()
	cgroup, memory := c.rec.Cgroup, max(c.cmd().MemoryBytes, c.residentBytes)
	e.mu.Unlock()
	if cgroup == "" {
		if st, err := e.cfg.Units.Status(ctx, unitName); err == nil && st.Cgroup != "" {
			cgroup = st.Cgroup
			memory = max(memory, st.Memory)
			e.mu.Lock()
			if c.rec.Cgroup == "" {
				c.rec.Cgroup = cgroup
			}
			e.mu.Unlock()
		}
	}
	if cgroup != "" {
		if memory <= 0 {
			if m, err := e.cfg.Units.Memory(cgroup); err == nil {
				memory = max(memory, m.Anon)
			}
		}
		if err := e.cfg.Units.Reclaim(cgroup, memory); err != nil {
			slog.Warn("reclaim paused command", "command", id, "err", err)
		} else if m, err := e.cfg.Units.Memory(cgroup); err == nil {
			e.mu.Lock()
			if c.cmd().State == paused {
				c.cmd().SwapBytes = m.Swap
				c.residentBytes = m.Anon
				c.residentKnown = true
				c.reclaimed = true
				c.rec.PeakHeldBytes = max(c.rec.PeakHeldBytes, m.Held())
			}
			e.mu.Unlock()
		}
	}
	return true, nil
}

// WaitOn notes that an agent waits on command id (FollowLog with wait): its
// soft time limit counts from now, and if that limit paused it, it resumes.
// One paused by hand stays paused until resumed by hand (Resume). A resumed
// command is no longer its first client's, who went when it paused: it is
// left to its limits, as a detached one is, whatever becomes of the agent
// now waiting on it.
func (e *Engine) WaitOn(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	e.mu.Lock()
	c, ok := e.commands[id]
	if !ok || !api.Live(c.cmd().State) {
		e.mu.Unlock()
		return nil
	}
	c.rec.WaitedAt = e.nowMs()
	c.lastFollow = e.now()
	cmd := c.cmd()
	if cmd.State != paused || c.stopping || c.intervening || api.PausedByHand(cmd) {
		e.saveCommand(c, change{what: "waited"})
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	c.freezeMu.Lock()
	defer c.freezeMu.Unlock()
	e.mu.Lock()
	cmd = c.cmd()
	if !api.Live(cmd.State) || cmd.State != paused || c.stopping || c.intervening || api.PausedByHand(cmd) {
		if api.Live(cmd.State) {
			e.saveCommand(c, change{what: "waited"})
		}
		e.mu.Unlock()
		return nil
	}
	if err := e.thawLockedBy(ctx, c, change{
		what: "resumed", detail: "an agent waits on it",
		feed: feed(feedResume, "resume "+cmd.Argv, ref("command", cmd.Id)),
	}, nil, true); err != nil {
		return fmt.Errorf("resume %s: %w", id, err)
	}
	return nil
}

type pendingThaw struct {
	ch            change
	by            *byHand
	clearAttached bool
}

// commitPendingThawLocked applies c.pendingThaw to c and publishes its event.
// Call with e.mu held.
func (e *Engine) commitPendingThawLocked(c *command) {
	pt := c.pendingThaw
	if pt == nil {
		return
	}
	c.pendingThaw = nil
	cmd := c.cmd()
	if !api.Live(cmd.State) {
		return
	}
	wasState := cmd.State
	now := e.nowMs()
	if pt.by != nil {
		pt.by.record(cmd)
		cmd.Attached = false
		c.rec.WaitedAt = now
	} else if pt.clearAttached {
		cmd.Attached = false
	} else if wasState == parked && c.rec.ParkedAt > 0 {
		// Exclude time spent parked by hotbed from the soft time limit budget
		// so a command frozen for memory pressure gets its full soft-limit
		// running time once thawed.
		base := max(cmd.GetStartedAt(), c.rec.WaitedAt)
		if parkedMs := now - max(c.rec.ParkedAt, base); parkedMs > 0 {
			c.rec.WaitedAt = base + parkedMs
		}
	}
	cmd.State = running
	cmd.Reason = ""
	c.rec.HeldFor = ""
	c.preemptedBy = ""
	c.reclaimed = false
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: pt.ch.what, Detail: pt.ch.detail})
	e.saveCommand(c, pt.ch)
}

// thawLocked runs c, frozen (paused or parked), again, saying why in ch. It
// calls Units.Thaw before publishing the resumed/thawed event so a failed thaw
// never leaves a bogus resumed event or intervention in the timeline/stream.
// Call it with c.freezeMu and e.mu held; it returns with e.mu released.
func (e *Engine) thawLocked(ctx context.Context, c *command, ch change) error {
	return e.thawLockedBy(ctx, c, ch, nil, false)
}

// thawLockedBy is thawLocked, recording a hand resume (by) only once
// Units.Thaw succeeds (or observe sees the unit exit right after thawing).
// Call with c.freezeMu and e.mu held; it returns with e.mu released.
func (e *Engine) thawLockedBy(ctx context.Context, c *command, ch change, by *byHand, clearAttached bool) error {
	cmd := c.cmd()
	from, reason := cmd.State, cmd.Reason
	unitName := c.unitName()
	c.pendingThaw = &pendingThaw{ch: ch, by: by, clearAttached: clearAttached}
	e.mu.Unlock()

	err := e.cfg.Units.Thaw(ctx, unitName)
	if err == nil {
		e.mu.Lock()
		// If observe() already saw the unit exit immediately after Thaw and
		// committed pendingThaw before ending c, pendingThaw is nil now.
		if c.pendingThaw != nil && !c.stopping && api.Live(cmd.State) {
			e.commitPendingThawLocked(c)
		} else {
			c.pendingThaw = nil
		}
		e.mu.Unlock()
		return nil
	}

	// Thaw failed: clear pendingThaw before reconcileInactiveUnit so an
	// already-dead unit never emits a bogus "resumed"/"thawed" event.
	e.mu.Lock()
	c.pendingThaw = nil
	e.mu.Unlock()

	if e.reconcileInactiveUnit(ctx, c) {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if cmd.State == from && !c.stopping {
		what := map[pb.CommandState]string{paused: "paused", parked: "parked"}[from]
		if what == "" {
			what = stateName(from)
		}
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: what, Detail: "it could not be thawed: " + err.Error()})
		e.saveCommand(c, change{what: what, detail: reason})
	}
	return err
}

// forget drops commands that ended a while ago, which history keeps, and
// workspaces released a while ago.
func (e *Engine) forget() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forgetReleased()
	for id, c := range e.commands {
		if api.Live(c.cmd().State) || c.cmd().EndedAt == nil {
			continue
		}
		if time.Duration(e.nowMs()-c.cmd().GetEndedAt())*time.Millisecond > keepEnded {
			delete(e.commands, id)
			e.deleteCommand(c)
			if e.cfg.Profiler != nil {
				e.cfg.Profiler.Forget(id)
			}
			// Once the controller has its profile, it serves it; the file
			// was for until then.
			if c.profileSeq > 0 && c.profileSeq <= e.applied.Load() {
				if path, err := e.profilePath(id); err == nil {
					if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
						slog.Error("remove a delivered command profile", "command", id, "err", err)
					}
				}
			}
		}
	}
}

// end records how c ended and tells the controller. Call with e.mu held.
func (e *Engine) end(c *command, state pb.CommandState, code *int32, why string) {
	e.endBy(c, state, code, why, nil)
}

// endBy is end, of a command someone stopped by hand (by) when it is one.
// Call with e.mu held.
func (e *Engine) endBy(c *command, state pb.CommandState, code *int32, why string, by *byHand) {
	cmd := c.cmd()
	if !api.Live(cmd.State) {
		c.pendingThaw = nil
		return
	}
	if c.pendingThaw != nil {
		if state != cancelled {
			e.commitPendingThawLocked(c)
		} else {
			c.pendingThaw = nil
		}
	}
	by.record(cmd)
	now := e.nowMs()
	cmd.State = state
	cmd.EndedAt = &now
	cmd.ExitCode = code
	cmd.CpuCores = 0
	cmd.Reason = why
	c.preemptedBy = ""
	c.reclaimed = false
	what, detail := "exited", "exit "+codeText(code)
	if state == cancelled {
		what, detail = "cancelled", why
		if by != nil {
			detail = conciseInterventionNote(cmd, by.iv)
		}
	} else if why != "" {
		detail = why
	}
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: what, Detail: detail})
	// Its processes are gone: the unit has exited or been stopped.
	if p, ok := e.portProxies[cmd.Id]; ok {
		delete(e.portProxies, cmd.Id)
		_ = p.Close()
	}
	if !e.usesRuntime(c.runtimeTarget()) && !c.isVMFreshen() {
		e.dropTmp(c)
	}

	started := cmd.QueuedAt
	if cmd.StartedAt != nil {
		started = *cmd.StartedAt
	}
	seconds := float64(now-started) / 1000
	units, failure := c.out.result(state, code, cmd.Kind)
	if state == failed && strings.HasPrefix(why, "killed by the OOM killer") {
		failure = "oom"
	}
	if cmd.Rebuilt != nil {
		cmd.Rebuilt.Units = int32(units)
	}
	if cold, _, ok := e.coldBuild(c.rec.Project); ok && isBuild(cmd.Kind) {
		// From nothing, this command would have had to build everything
		// first: the base's cold freshen, then itself.
		total := cold + seconds
		cmd.ColdSeconds = &total
	}
	if cmd.Kind == "freshen" {
		e.saveCommand(c, change{what: what, detail: detail, diagnostics: by.diagnostics()})
		if mf := c.managedFreshen; mf != nil {
			// Managed VM / cladefs-FUSE base builds finalize their R2 cell and
			// Base record inside EnsureProjectBase once tmp cleanup finishes.
			gone, done := c.tmpGone, mf.done
			if gone == nil {
				close(done)
			} else {
				go func() {
					<-gone
					close(done)
				}()
			}
			return
		}
		// Its tmp would be baked into the base: snapshot once it is gone.
		gone, ok := c.tmpGone, state == succeeded
		e.startBgLocked(func(ctx context.Context) {
			select {
			case <-gone:
			case <-ctx.Done():
				return
			}
			e.freshened(ctx, c, ok, seconds, units)
		})
		return
	}
	f := &pb.FinishedCommand{
		Id: cmd.Id, RepoId: c.rec.RepoId, NodeId: cmd.NodeId, WorkspaceId: cmd.WorkspaceId, Agent: c.rec.Agent,
		Argv: cmd.Argv, Kind: cmd.Kind, State: state, ExitCode: code, Failure: failure,
		EndedAt: now, QueuedSeconds: float64(started-cmd.QueuedAt) / 1000, Seconds: seconds,
		ColdSeconds: cmd.ColdSeconds, PeakMemoryBytes: c.rec.PeakMemoryBytes, Parks: c.rec.Parks,
		SessionId: cmd.SessionId, Reason: why, Bench: cmd.Bench, Runtime: e.commandRuntime(c),
	}
	if len(cmd.Interventions) > 0 {
		f.Interventions = make([]*pb.CommandIntervention, len(cmd.Interventions))
		for i, iv := range cmd.Interventions {
			f.Interventions[i] = clone(iv)
		}
	}
	if cmd.Rebuilt != nil {
		units, of := cmd.Rebuilt.Units, cmd.Rebuilt.Of
		f.Rebuilt, f.Of = &units, &of
	}
	r := ref("command", cmd.Id)
	var line *pb.WorkerEvent
	switch state {
	case succeeded:
		text := fmt.Sprintf("ok %s · %s · %s", cmd.Argv, ranIn(c.rec), dur(seconds))
		if cmd.ColdSeconds != nil {
			text += fmt.Sprintf(" (cold %s, %.0f×)", dur(*cmd.ColdSeconds), *cmd.ColdSeconds/max(seconds, 0.1))
		}
		line = feed(feedFinish, text, r)
	case failed:
		line = feed(feedFail, fmt.Sprintf("fail %s · %s · %s", cmd.Argv, ranIn(c.rec), orDefault(why, "exit "+codeText(code))), r)
	default:
		line = feed(feedFinish, fmt.Sprintf("cancelled %s · %s", cmd.Argv, why), r)
	}
	var prof *pb.CommandProfile
	isVM := cmd.WorkspaceId != "" && e.usesRuntime(cmd.WorkspaceId)
	var snap *pb.CommandProfile
	if isVM {
		snap = c.vmProfile
		c.vmProfile = nil
	} else if e.cfg.Profiler != nil {
		snap = e.cfg.Profiler.Snapshot(cmd.Id)
	}
	if snap != nil && snap.TotalSamples > 0 {
		if path, err := e.profilePath(cmd.Id); err != nil {
			slog.Warn("save command profile", "command", cmd.Id, "err", err)
		} else if err := profiler.SaveProfile(path, snap); err != nil {
			slog.Warn("save command profile", "command", cmd.Id, "err", err)
		}
		prof = snap
	}
	e.saveCommand(c, change{what: what, detail: detail, finished: f, feed: line, profile: prof, diagnostics: by.diagnostics()})
	if prof != nil && !e.isDirty() {
		// The event just written carries the profile to the controller.
		if _, newest, err := e.db.eventRange(); err != nil {
			slog.Error("read the event that carries a profile", "command", cmd.Id, "err", err)
		} else {
			c.profileSeq = newest
		}
	}
	if w, ok := e.workspaces[cmd.WorkspaceId]; ok {
		w.ws().LastActiveAt = now
		if w.ws().State == activeWS && !e.workspaceBusy(w.ws().Id) {
			w.ws().State = idle
		}
		wsWhat := "active"
		switch w.ws().State {
		case idle:
			wsWhat = "idle"
		case failedWS:
			wsWhat = "failed"
		case released:
			wsWhat = "released"
		case preparing:
			wsWhat = "preparing"
		}
		e.saveWorkspace(w, change{what: wsWhat, detail: cmd.Id + " ended"})
		if w.ws().State == idle && e.usesRuntime(w.ws().Id) {
			wsID := w.ws().Id
			e.startBgLocked(func(bgCtx context.Context) {
				rctx, cancel := context.WithTimeout(bgCtx, 3*time.Second)
				defer cancel()
				_ = e.cfg.Runtime.Reclaim(rctx, wsID, "", 0)
			})
		}
	}
	e.stepSoon()
}

func (e *Engine) workspaceBusy(id string) bool {
	for _, c := range e.commands {
		if c.cmd().WorkspaceId == id && api.Live(c.cmd().State) {
			return true
		}
	}
	for _, sh := range e.shells {
		if sh.workspaceID == id {
			return true
		}
	}
	return false
}

// ranIn names where a command ran: its repo, or its project when it ran in
// /work itself.
func ranIn(r *pb.CommandRecord) string { return cmp.Or(r.RepoId, r.Project) }

// coldBuild is the project's most recent from-nothing freshen on this node:
// how long it took and how many units it compiled. Call with e.mu held.
func (e *Engine) coldBuild(project string) (seconds float64, units int, ok bool) {
	var best *pb.Base
	for _, b := range e.bases {
		if b.Project == project && b.Cold && (b.State == pb.BaseState_BASE_STATE_WARM || b.State == pb.BaseState_BASE_STATE_STALE) {
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

func (e *Engine) logPath(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return filepath.Join(e.logsDir(), id+".log"), nil
}

func codeText(code *int32) string {
	if code == nil {
		return "signal"
	}
	return strconv.Itoa(int(*code))
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// limit reads a cgroup memory limit, where 0 sets none.
func limit(b int64) string {
	if b == 0 {
		return "none"
	}
	return gib(b)
}

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
