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
	"github.com/semistrict/hotbed/internal/vm"
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
	if ran := w.rec.ThrowawayCommand; ran != "" {
		return nil, api.Errorf(api.CodeConflict, "workspace %s is throwaway: command %s ran in it, and it goes when that ends", req.WorkspaceId, ran).
			WithSuggestion("hotbed run --fork gets each command a throwaway workspace of its own")
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
	if err := e.checkVMRequestLocked(req); err != nil {
		return nil, err
	}
	tty, ttyInput, err := e.ttyFor(req)
	if err != nil {
		return nil, err
	}
	estimate, err := checkEstimate(req.Estimate)
	if err != nil {
		return nil, err
	}
	now := e.nowMs()
	c := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id: id, WorkspaceId: req.WorkspaceId, NodeId: e.cfg.NodeID,
			Argv: shell.Join(req.Args), Args: req.Args, Kind: kind,
			State: queued, QueuedAt: now, Limits: limits, Attached: req.Attached,
			SessionId: w.ws().SessionId, Bench: req.Bench, Repo: req.Repo,
			VmCpus: req.VmCpus, VmMemoryBytes: req.VmMemoryBytes,
			Tty: tty, TtyInput: ttyInput, Estimate: estimate,
			Runtime:  e.workspaceRuntime(w),
			Timeline: []*pb.CommandEvent{{At: now, What: "queued", Detail: "by " + w.ws().Agent}},
		},
		RepoId: req.Repo, Project: w.ws().Project, Agent: w.ws().Agent, Volume: w.rec.Volume,
		Dir: dir, Env: req.Env,
	})
	if req.Port > 0 {
		if e.usesRuntime(c.runtimeTarget()) {
			if p, err := startPortProxy(fmt.Sprintf(":%d", req.Port), c.runtimeTarget(), uint16(req.Port), e.cfg.Runtime); err != nil {
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
	if kind != "server" {
		e.recordUserActivityLocked(e.now())
	}
	w.ws().LastActiveAt = now
	if w.ws().State != preparing {
		w.ws().State = activeWS
	}
	if w.ws().Throwaway {
		w.rec.ThrowawayCommand = id
	}
	e.saveWorkspace(w, change{what: "active", detail: "running " + id})
	e.commands[id] = c
	e.saveCommand(c, change{what: "queued", detail: "by " + w.ws().Agent})
	e.stepSoon()
	return clone(c.cmd()), nil
}

// minVMMemory is the least memory a command may ask its VM's guest to see:
// less does not boot the guest and its tools.
const minVMMemory = 512 << 20

// checkVMRequestLocked refuses a VM size its workspace or this node cannot
// give: one outside the vm runtime, or larger than the node. Call with e.mu
// held.
func (e *Engine) checkVMRequestLocked(req *pb.RunCommandRequest) error {
	if req.VmCpus == 0 && req.VmMemoryBytes == 0 {
		return nil
	}
	if !e.usesRuntime(req.WorkspaceId) {
		return api.Errorf(api.CodeInvalidArgs, "workspace %s runs commands on the host: --vm-cpus and --vm-mem need the vm runtime", req.WorkspaceId).
			WithSuggestion("run with --runtime vm")
	}
	if cpus := e.resources.GetCpus(); req.VmCpus < 0 || req.VmCpus > cpus {
		return api.Errorf(api.CodeInvalidArgs, "--vm-cpus %d: %s has %d CPUs", req.VmCpus, e.cfg.Host, cpus).
			WithSuggestion(fmt.Sprintf("ask for 1 to %d, or pick a bigger node with --node (hotbed nodes lists them)", cpus))
	}
	if mem := e.resources.GetMemoryBytes(); req.VmMemoryBytes != 0 && (req.VmMemoryBytes < minVMMemory || req.VmMemoryBytes > mem) {
		return api.Errorf(api.CodeInvalidArgs, "--vm-mem %s: a VM on %s takes %s to %s", gib(req.VmMemoryBytes), e.cfg.Host, gib(minVMMemory), gib(mem)).
			WithSuggestion("ask for memory in that range, or pick a bigger node with --node (hotbed nodes lists them)")
	}
	return nil
}

// ttyFor is whether req's output goes to a terminal, and its input comes
// from it: as it asks (--tty, both), or, by default, the output of a
// detached command in a VM, whose output no one prints as it goes, so tools
// may draw for a person as they would in a shell (queries of its output
// settle what they redraw), its input still /dev/null, so a prompt fails at
// once rather than wait for an answer no one gives. Call with e.mu held.
func (e *Engine) ttyFor(req *pb.RunCommandRequest) (tty, input bool, err error) {
	vm := e.usesRuntime(req.WorkspaceId)
	switch req.Tty {
	case pb.TtyMode_TTY_MODE_ON:
		if !vm {
			return false, false, api.Errorf(api.CodeInvalidArgs, "workspace %s runs commands on the host: a terminal needs the vm runtime", req.WorkspaceId).
				WithSuggestion("drop --tty, or run with --runtime vm")
		}
		return true, true, nil
	case pb.TtyMode_TTY_MODE_OFF:
		return false, false, nil
	default:
		return vm && !req.Attached, false, nil
	}
}

// checkEstimate is a run's estimate as kept with it, or why it is not one.
func checkEstimate(est *pb.CommandEstimate) (*pb.CommandEstimate, error) {
	if est == nil || (est.MemoryBytes == 0 && est.Seconds == 0) {
		return nil, nil
	}
	if est.MemoryBytes < 0 || est.Seconds < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "an estimate of %d bytes and %d seconds: neither may be negative", est.MemoryBytes, est.Seconds)
	}
	out := proto.Clone(est).(*pb.CommandEstimate)
	if out.Source == "" {
		out.Source = "given"
	}
	return out, nil
}

// estimateNote sets what a command was expected to need beside what it
// took: its peak memory and how long it ran.
func estimateNote(est *pb.CommandEstimate, peak int64, seconds float64) string {
	var parts []string
	if est.MemoryBytes > 0 {
		parts = append(parts, fmt.Sprintf("peak %s of %s estimated", gib(peak), gib(est.MemoryBytes)))
	}
	if est.Seconds > 0 {
		parts = append(parts, fmt.Sprintf("took %s of %s expected", dur(seconds), dur(float64(est.Seconds))))
	}
	return strings.Join(parts, " · ") + " (" + est.Source + ")"
}

// vmSize is the size of the VM c runs in, as its unit asks it.
func vmSize(c *command) unit.VMSize {
	return unit.VMSize{VCPUs: int(c.cmd().VmCpus), MemMiB: int((c.cmd().VmMemoryBytes + 1<<20 - 1) >> 20)}
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

// Stop ends a command: a queued one never starts, a started one is stopped. It returns the command as it ended.
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
	// A frozen unit cannot be stopped until it is thawed: one held or
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
// commands, holds them to their time limits, stops those that sit idle on
// much of the node's memory, stops orphaned attached commands.
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
	e.mu.Lock()
	measuring := e.measuring() != nil
	e.mu.Unlock()
	if !measuring {
		e.releaseHeld(ctx)
	}
	e.settleBench(ctx)
	e.admit(ctx)
	e.limit(ctx)
	e.stopIdleHogs()
	e.retireVMs(ctx)
	e.releaseSpentThrowaways()
	e.maybeStartPendingFreshens(ctx)
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
// frozen (held beside a measurement, or paused).
func started(s pb.CommandState) bool { return s == running || s == parked || s == paused }

const (
	minPriority = -100
	maxPriority = 100
)

func (c *command) priority() int32 {
	return c.cmd().GetLimits().GetPriority()
}

// reserves says whether a command in state s holds its guaranteed memory
// (memory.low) on the node: one running or held beside a measurement does, to
// come back to; one paused does not, its memory gone to swap until an agent,
// who may never come, waits on it.
func reserves(s pb.CommandState) bool { return s == running || s == parked }

// reservation is the memory the node keeps for c while it reserves: its
// guarantee, or as much as it has held of its own, if more. One that asked
// for no guarantee is charged its estimate, if it gave one; else, attached,
// quickReservation, and detached, defaultReservation (each at most
// admitLimit, so it can always run alone): a build grows to gigabytes well after it
// starts, by when admission would have let others in beside it. From there
// it only grows: a build's need recurs (each link wants its memory again), so
// what it has held is what it may hold again, whatever it asked for. Never
// the RAM its VM was given: that is a configured ceiling a guest rarely
// fills, so counting it would leave the node idle. Call with e.mu held.
func (e *Engine) reservation(c *command) int64 {
	base := c.cmd().Limits.GetMemoryLowBytes()
	switch {
	case base > 0:
	case c.cmd().GetEstimate().GetMemoryBytes() > 0:
		base = min(c.cmd().GetEstimate().GetMemoryBytes(), e.admitLimit())
	case c.cmd().Attached:
		base = min(quickReservation, e.admitLimit())
	default:
		base = min(defaultReservation, e.admitLimit())
	}
	return max(base, c.rec.PeakHeldBytes)
}

// reserved is the memory the node keeps for the commands that reserve it,
// base builds and work alike. Call with e.mu held.
func (e *Engine) reserved() int64 {
	var n int64
	for _, c := range e.commands {
		if reserves(c.cmd().State) {
			n += e.reservation(c)
		}
	}
	return n
}

// admitLimit is the memory reservations may take: the work slice's memory
// budget (e.resources.MemoryBytes, which on Linux is already backstop.High
// after deducting host headroom, cladefsd reserve, zram footprint, and any
// static slice drop-in clamp). Call with e.mu held.
func (e *Engine) admitLimit() int64 {
	return e.resources.GetMemoryBytes()
}

// reclaimableCladefsCacheBytes returns the bytes of cladefsd's DAX extent cache
// held above its protected floor. That cache lives in shmem/memfd, which Linux
// /proc/meminfo excludes from MemAvailable even though cladefsd's memory
// governor evicts everything above ExtentCacheFloorBytes on demand.
func reclaimableCladefsCacheBytes(os *pb.ObjectStoreStats) int64 {
	if os == nil {
		return 0
	}
	used := int64(os.GetExtentCacheBytes())
	floor := int64(os.GetExtentCacheFloorBytes())
	if floor <= 0 || used <= floor {
		return 0
	}
	return used - floor
}

// effectiveMemUsedLocked returns non-reclaimable memory used in mem's domain.
// On the host domain (mem.Domain == ""), cladefsd's evictable DAX extent cache
// above its floor is credited back so warm DAX caching does not look like
// unavailable RAM; on a cgroup slice domain (mem.Domain != ""), cladefsd runs
// outside the slice so mem.Used() is returned unchanged. Call with e.mu held.
func (e *Engine) effectiveMemUsedLocked(mem sysinfo.Mem) int64 {
	used := mem.Used()
	if mem.Domain == "" {
		used = max(0, used-reclaimableCladefsCacheBytes(e.objectStore))
	}
	return used
}

// reportedMemUsedLocked returns MemoryUsedBytes for heartbeats and controller
// placement, paired against e.resources.MemoryBytes. When mem comes from a
// wider host domain (mem.Total > e.resources.MemoryBytes), it projects the
// host's effective available RAM onto the slice budget so controller placement
// (free = resources.MemoryBytes - max(used, reserved)) and cockpit gauges do
// not double-subtract OS headroom, cladefsd's reserve, or evictable DAX cache.
// Call with e.mu held.
func (e *Engine) reportedMemUsedLocked(mem sysinfo.Mem) int64 {
	if mem.Total <= 0 {
		return 0
	}
	used := e.effectiveMemUsedLocked(mem)
	avail := max(0, mem.Total-used)
	if r := e.resources.GetMemoryBytes(); r > 0 && mem.Total > r {
		return max(0, r-min(r, avail))
	}
	return used
}

// fitsReservation reports whether a command reserving want bytes fits alongside
// reserved bytes already held against a node admitLimit of limit. When existing
// reservations already fill or exceed limit (reserved > 0 && room <= 0), even a
// want == 0 command cannot start: every real command consumes memory, and a
// reservation equal to limit (such as a base freshen capped at admitLimit) claims
// the whole node until it finishes.
func fitsReservation(want, reserved, limit int64) bool {
	room := limit - reserved
	if want > room {
		return false
	}
	if reserved > 0 && room <= 0 {
		return false
	}
	return true
}

// memoryBudgetBytes returns the effective memory ceiling in bytes for a
// command or shell running in this worker: Firecracker guest RAM for VM
// runtimes (vmMemory when the command sized its VM, else the node's default;
// bounded by the host slice), or the work slice's memory limit
// (e.resources.MemoryBytes) for host runtimes, further capped by any explicit
// per-command MemoryHigh limit. Call with e.mu held.
func (e *Engine) memoryBudgetBytes(isVM bool, vmMemory, memHigh int64) int64 {
	budget := e.resources.GetMemoryBytes()
	if isVM && vmMemory <= 0 && e.cfg.VM != nil {
		if mib := e.cfg.VM.configSnapshot().MemMiB; mib > 0 {
			vmMemory = int64(mib) << 20
		}
	}
	if isVM && vmMemory > 0 && (budget <= 0 || vmMemory < budget) {
		budget = vmMemory
	}
	if memHigh > 0 && (budget <= 0 || memHigh < budget) {
		budget = memHigh
	}
	return budget
}

func (e *Engine) observe(ctx context.Context, c *command, st unit.State, sampledAt time.Time) {
	// Read memory OUTSIDE e.mu so slow cgroup reads or VM vsock RPCs never
	// stall heartbeats, RPCs, or Stop calls across the worker. When StatusMany
	// already populated st.MemoryUse (e.g. VM commands), reuse it directly
	// without making a second RPC.
	var (
		mem    unit.MemoryUse
		hasMem bool
		memErr error
	)
	if st.MemoryUse != nil {
		mem = *st.MemoryUse
		hasMem = true
	} else {
		cg := st.Cgroup
		if cg == "" {
			e.mu.Lock()
			if api.Live(c.cmd().State) && !c.stopping {
				cg = c.rec.Cgroup
			}
			e.mu.Unlock()
		}
		if cg != "" {
			mem, memErr = e.cfg.Units.Memory(cg)
			hasMem = memErr == nil
		}
	}

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
	c.rec.PeakMemoryBytes = max(c.rec.PeakMemoryBytes, st.MemoryPeak)
	if c.rec.Cgroup != "" {
		if hasMem {
			cmd.SwapBytes = mem.Swap
			if mem.Anon > cmd.MemoryBytes {
				cmd.MemoryBytes = mem.Anon
				c.rec.PeakMemoryBytes = max(c.rec.PeakMemoryBytes, mem.Anon)
			}
			c.rec.PeakHeldBytes = max(c.rec.PeakHeldBytes, mem.Held())
		} else if memErr != nil && !st.Exited {
			slog.Warn("read a command's memory", "command", cmd.Id, "err", memErr)
		}
	}
	// While it runs and as it ends: the node is given back only after.
	e.checkIsolation(c, st.Exited)
	c.deactivating = st.Loaded && !st.Exited && !st.Running
	if !st.Exited {
		e.watchIdle(c, st, mem, c.rec.Cgroup != "" && hasMem, sampledAt)
		return
	}
	// removeUnit has removed the unit and read the rest of its output.
	if st.VMExit != nil {
		c.failure = "vm"
		e.end(c, failed, nil, vmExitReason(*st.VMExit))
		return
	}
	if st.OOMKilled || st.Result == "oom-kill" {
		var code *int32
		if st.Signal == 0 && st.ExitCode != 0 {
			ec := int32(st.ExitCode)
			code = &ec
		}
		c.failure = "oom"
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

// vmExitReason says how the VM a command ran in ended under it: the command
// never reported an end of its own, so the reason is the VM's, never a
// signal or exit the command did not have.
func vmExitReason(ex unit.VMExit) string {
	switch {
	case ex.Gone:
		return "its VM stopped while it ran"
	case ex.OOMKilled || ex.Result == "oom-kill":
		return "its VM was killed by the host's OOM killer"
	case ex.Signal != 0:
		return fmt.Sprintf("its VM was killed by signal %d", ex.Signal)
	default:
		return "its VM ended: " + vm.ExitReason(ex.ExitCode)
	}
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

// admit starts queued commands, highest priority first, then base builds
// (which workspaces wait on), then measurements (so waiting measurements
// hold the line ahead of other work), then attached ones (quick runs a
// client waits on, the fast lane), then the focus session's work, then
// oldest queued first, while they fit. What it starts runs until it ends:
// nothing is frozen to make room.
func (e *Engine) admit(ctx context.Context) {
	e.mu.Lock()
	e.weighHold(e.now())
	focus := e.focusSessionLocked()
	var waiting []*command
	for _, c := range e.commands {
		if c.cmd().State == queued {
			waiting = append(waiting, c)
		}
	}
	slices.SortFunc(waiting, func(a, b *command) int {
		return cmp.Or(
			cmp.Compare(b.priority(), a.priority()),
			cmpBool(b.isFreshen(), a.isFreshen()),
			cmpBool(b.cmd().Bench, a.cmd().Bench),
			cmpBool(b.cmd().Attached, a.cmd().Attached),
			cmpBool(isFocusCommand(b, focus), isFocusCommand(a, focus)),
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
		kind, reason, holds := e.admissionBlockerFor(c, ahead, holder, mem, hasMem)
		// A command sizing its VM may boot it again at its size: nothing else
		// may use the VM meanwhile, until its unit has started.
		var heldVM *sync.RWMutex
		if kind == "" {
			if w, ok := e.workspaces[c.runtimeTarget()]; ok && e.usesRuntime(w.ws().Id) && vmSize(c) != (unit.VMSize{}) {
				if w.vm.TryLock() {
					heldVM = &w.vm
				} else {
					kind, reason = "workspace-busy", "waiting for its workspace: a sync, file read or shell is using its VM"
				}
			}
		}
		releaseVM := func() {
			if heldVM != nil {
				heldVM.Unlock()
				heldVM = nil
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
		if w, ok := e.workspaces[c.runtimeTarget()]; ok && e.usesRuntime(w.ws().Id) {
			w.vmUsed = true // the next command needs a VM of its own
		}
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
				releaseVM()
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
					releaseVM()
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
		var booted string
		if err == nil {
			startCall := time.Now()
			err = e.cfg.Units.Start(ctx, spec)
			if err == nil && e.cfg.VM != nil {
				if b, ok := e.cfg.VM.BootSince(c.cmd().WorkspaceId, startCall); ok {
					booted = " · VM booted in " + b.String()
				}
			}
		}
		releaseVM()
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
		if isFocusEligible(c) {
			e.lastFocusSession = e.focusSessionLocked()
		}
		finishAdmit()
		share := fmt.Sprintf("weight %d", cmd.Limits.CpuWeight)
		if cmd.Bench {
			share = "measurement on CPUs " + e.cfg.Bench.CPUs().String()
		}
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: "admitted", Detail: fmt.Sprintf("%s · low %s · high %s%s", share, limit(cmd.Limits.MemoryLowBytes), limit(cmd.Limits.MemoryHighBytes), booted)})
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
		// Refresh physical memory after starting a command (which may have
		// booted a Firecracker VM or faulted RAM) before checking the next
		// queued command in the same tick.
		if m, err := e.cfg.System.Meminfo(); err == nil && m.Total > 0 {
			mem, hasMem = m, true
		}
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

// isFocusEligible reports whether c participates in session-focus scheduling:
// regular session-owned work (not benchmarks, dev servers, or base builds).
func isFocusEligible(c *command) bool {
	cmd := c.cmd()
	return cmd.SessionId != "" && !cmd.Bench && cmd.Kind != "server" && !c.isFreshen()
}

func isFocusCommand(c *command, focus string) bool {
	return focus != "" && isFocusEligible(c) && c.cmd().SessionId == focus
}

// focusSessionLocked returns the session currently focused for completion on
// this node, or "" if no session-scoped work is active:
//  1. The session of the highest-priority, oldest-started running command.
//  2. Otherwise, e.lastFocusSession if it still has a queued command at the
//     highest queued priority (so a session finishes its already-queued work
//     in its warm workspace/VM before switching).
//  3. Otherwise, the session of the highest-priority, oldest-queued command.
//
// Call with e.mu held.
func (e *Engine) focusSessionLocked() string {
	var bestRun, bestQueue *command
	var maxPendingPrio int32
	hasPending := false
	for _, c := range e.commands {
		if c.stopping || c.deactivating || !isFocusEligible(c) {
			continue
		}
		switch c.cmd().State {
		case running:
			if bestRun == nil ||
				c.priority() > bestRun.priority() ||
				(c.priority() == bestRun.priority() && cmp.Or(cmp.Compare(c.cmd().GetStartedAt(), bestRun.cmd().GetStartedAt()), cmp.Compare(c.cmd().Id, bestRun.cmd().Id)) < 0) {
				bestRun = c
			}
		case queued:
			if !hasPending || c.priority() > maxPendingPrio {
				maxPendingPrio = c.priority()
				hasPending = true
			}
			if bestQueue == nil ||
				c.priority() > bestQueue.priority() ||
				(c.priority() == bestQueue.priority() && cmp.Or(cmp.Compare(c.cmd().QueuedAt, bestQueue.cmd().QueuedAt), cmp.Compare(c.cmd().Id, bestQueue.cmd().Id)) < 0) {
				bestQueue = c
			}
		}
	}
	if bestRun != nil {
		return bestRun.cmd().SessionId
	}
	if e.lastFocusSession != "" && hasPending {
		for _, c := range e.commands {
			if c.stopping || c.deactivating || !isFocusEligible(c) {
				continue
			}
			if c.cmd().State == queued && c.cmd().SessionId == e.lastFocusSession && c.priority() == maxPendingPrio {
				return e.lastFocusSession
			}
		}
	}
	if bestQueue != nil {
		return bestQueue.cmd().SessionId
	}
	return ""
}

// admissionBlocker says why c cannot start now, as a kind (preparing,
// measurement, behind-measurement, quiet, host-memory, host-cpu, disk, memory,
// pressure, cpu) and a reason with the numbers as they are, or "" if it can;
// and whether a measurement's reason holds up the work queued after it: it
// waits for room that work would take. ahead is the oldest measurement still
// waiting, holder one that holds up work. Call with e.mu held.
func (e *Engine) admissionBlocker(c, ahead, holder *command) (kind, reason string, holds bool) {
	return e.admissionBlockerFor(c, ahead, holder, sysinfo.Mem{}, false)
}

func (e *Engine) admissionBlockerFor(c, ahead, holder *command, mem sysinfo.Mem, hasMem bool) (kind, reason string, holds bool) {
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
		if e.usesRuntime(c.runtimeTarget()) {
			if r := e.vmBlockerLocked(c, w); r != "" {
				return "workspace-busy", r, false
			}
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
	if k, r := e.roomFor(c, mem, hasMem); r != "" {
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
	return e.roomFor(c, sysinfo.Mem{}, false)
}

// roomFor checks node capacity for c, measuring physical memory by mem when
// hasMem: host starvation, disk, memory reservations, physical memory headroom,
// and memory and CPU pressure. Call with e.mu held.
func (e *Engine) roomFor(c *command, mem sysinfo.Mem, hasMem bool) (kind, reason string) {
	hu := e.freshHostUsage(e.now())
	if memHeld, memReason := hostMemoryStarved(hu); memHeld {
		return "host-memory", "waiting: " + memReason
	}
	if cpuHeld, _, _, _, cpuReason, _ := e.hostCPUStatus(hu); cpuHeld {
		return "host-cpu", "waiting: " + cpuReason
	}
	if cladefsHeld, cladefsReason := e.cladefsMemoryStarved(); cladefsHeld {
		return "cladefs-memory", "waiting: " + cladefsReason
	}
	if e.diskLow() {
		return "disk", fmt.Sprintf("waiting for disk: %s free", gib(e.disk.FreeBytes))
	}
	want := e.reservation(c)
	limit := e.admitLimit()
	reserved := e.reserved()
	room := limit - reserved
	if !fitsReservation(want, reserved, limit) {
		if room <= 0 {
			if want > 0 {
				return "memory", fmt.Sprintf("waiting for memory: needs %s, but running commands hold or are guaranteed %s (limit %s)", gib(want), gib(reserved), gib(limit))
			}
			return "memory", fmt.Sprintf("waiting for memory: running commands hold or are guaranteed %s (limit %s)", gib(reserved), gib(limit))
		}
		return "memory", fmt.Sprintf("waiting for memory: needs %s, %s left of what running commands hold or are guaranteed", gib(want), gib(room))
	}
	var memTotal, memUsed, swapTotal, swapFree int64
	if hasMem {
		memTotal, memUsed = mem.Total, e.effectiveMemUsedLocked(mem)
		swapTotal, swapFree = mem.SwapTotal, mem.SwapFree
	} else if e.lastMem.Total > 0 {
		memTotal, memUsed = e.lastMem.Total, e.effectiveMemUsedLocked(e.lastMem)
		swapTotal, swapFree = e.lastMem.SwapTotal, e.lastMem.SwapFree
	} else if t := e.resources.GetMemoryBytes(); t > 0 && e.usage.GetMemoryUsedBytes() > 0 {
		memTotal, memUsed = t, e.usage.GetMemoryUsedBytes()
	}
	// A solo VM workspace on an otherwise idle node can hold host memory above
	// fullAt purely from its own already-faulted guest RAM while memory PSI is
	// low; bypass fullAt/physRoom ONLY when no other started command exists on
	// the node so a single VM never self-deadlocks, without letting new VMs
	// pile onto an already-full multi-command node.
	vmSoloBypass := e.usesRuntime(c.runtimeTarget()) && !e.hasOtherStartedCommandLocked(c.runtimeTarget())
	if memTotal > 0 && !vmSoloBypass {
		physRoom := memTotal - memUsed
		if memUsed >= int64(float64(memTotal)*fullAt) {
			return "memory", fmt.Sprintf("waiting for memory: %.0f%% in use (%s physically available on the node)", 100*float64(memUsed)/float64(memTotal), gib(max(0, physRoom)))
		}
		// When the work slice's swap budget is saturated (>= 95% used) and RAM
		// is already above swapFullRAM with work running, nothing more can
		// spill to swap: queue new work instead of piling into a permanent
		// memory.high PSI livelock.
		if !c.cmd().Bench && swapTotal > 0 && float64(swapTotal-swapFree) >= float64(swapTotal)*0.95 &&
			float64(memUsed) >= float64(memTotal)*swapFullRAM && e.hasRunningWorkLocked() {
			return "memory", fmt.Sprintf("waiting for memory: swap %.0f%% full (%s free) while RAM is %.0f%% in use",
				100*float64(swapTotal-swapFree)/float64(swapTotal), gib(max(0, swapFree)), 100*float64(memUsed)/float64(memTotal))
		}
		// A freshen's MemoryLowBytes is an intra-hotbed concurrency reservation
		// sized from an earlier build's PeakHeldBytes (anon + swap, capped at
		// admitLimit), not a requirement that all of it sit free in physical
		// RAM before starting: OS/kernel/ZFS ARC overhead or a shared host
		// routinely keeps idle MemAvailable below admitLimit, so requiring
		// want <= physRoom would deadlock base builds on idle nodes
		// (board 11078). Explicit caller --mem-low guarantees, and what a
		// command has already held, still verify physical headroom; the
		// default reservation does not: it is an estimate for admission.
		if held := max(c.cmd().Limits.GetMemoryLowBytes(), c.rec.PeakHeldBytes); !c.isFreshen() && held > physRoom {
			return "memory", fmt.Sprintf("waiting for memory: needs %s, %s physically available on the node", gib(held), gib(max(0, physRoom)))
		}
	}
	switch {
	case e.memPressureHolding || e.usage.GetMemoryPressure() >= admitPressure:
		return "pressure", fmt.Sprintf("waiting: memory pressure %.0f%% (resumes under %.0f%%)", e.usage.GetMemoryPressure(), memPressureResumeAt)
	case e.cpu.holding && !c.cmd().Bench:
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
	if mem := e.memoryBudgetBytes(vmFreshen || e.usesRuntime(c.runtimeTarget()), cmd.VmMemoryBytes, cmd.Limits.GetMemoryHighBytes()); mem > 0 {
		env = append(env, "HOTBED_MEMORY_BYTES="+strconv.FormatInt(mem, 10))
	}
	if t := c.runtimeTarget(); t != cmd.WorkspaceId {
		env = append(env, "HOTBED_RUNTIME_TARGET="+t)
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
		VM:           vmSize(c),
		TTY:          cmd.Tty,
		TTYInput:     cmd.TtyInput,
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

func (c *command) isFreshen() bool { return c.cmd().Kind == "freshen" }

// hasOtherStartedCommandLocked reports whether any running or held command
// outside wsID currently exists on the node. Call with e.mu held.
func (e *Engine) hasOtherStartedCommandLocked(wsID string) bool {
	for _, other := range e.commands {
		if other.stopping || other.deactivating {
			continue
		}
		st := other.cmd().State
		if st != running && st != parked {
			continue
		}
		if wsID == "" || other.runtimeTarget() != wsID {
			return true
		}
	}
	return false
}

// hasRunningWorkLocked reports whether any non-stopping command is currently
// running on the node. Call with e.mu held.
func (e *Engine) hasRunningWorkLocked() bool {
	for _, c := range e.commands {
		if !c.stopping && !c.deactivating && c.cmd().State == running {
			return true
		}
	}
	return false
}

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

// pause freezes c, which ran its soft time limit without ending, and then
// pushes its memory to swap: no one may come back for it.
func (e *Engine) pause(ctx context.Context, c *command) {
	_, err := e.pauseUnit(ctx, c, func(was pb.CommandState) bool {
		cmd := c.cmd()
		soft := cmd.Limits.GetSoftLimitSeconds()
		// A stop that began as it froze, an intervention, a measurement hold,
		// or an agent that began waiting on it meanwhile (WaitOn saw it running):
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
// (held or paused), then, under e.mu, asks still whether it is to be
// paused after all, given the state it was in. If so, record records it
// paused, under e.mu, and its memory is pushed to swap; if not, the unit is
// thawed again ONLY if this call transitioned it from running and the command
// is still in running state (never thawing a command that another transition
// already paused or held). It says whether c was paused.
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
	cgroup, memory := c.rec.Cgroup, c.cmd().MemoryBytes
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
		// Exclude time spent held beside a measurement from the soft time
		// limit budget so it gets its full soft-limit running time once thawed.
		base := max(cmd.GetStartedAt(), c.rec.WaitedAt)
		if parkedMs := now - max(c.rec.ParkedAt, base); parkedMs > 0 {
			c.rec.WaitedAt = base + parkedMs
		}
	}
	cmd.State = running
	cmd.Reason = ""
	c.rec.HeldFor = ""
	if isFocusEligible(c) {
		e.lastFocusSession = e.focusSessionLocked()
	}
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: pt.ch.what, Detail: pt.ch.detail})
	e.saveCommand(c, pt.ch)
}

// thawLocked runs c, frozen (paused or held), again, saying why in ch. It
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
	if cmd.StartedAt != nil && isFocusEligible(c) {
		e.lastFocusSession = cmd.SessionId
	}
	cmd.State = state
	cmd.EndedAt = &now
	cmd.ExitCode = code
	cmd.CpuCores = 0
	// An ended command's memory is its peak, as history keeps it.
	if c.rec.PeakMemoryBytes > 0 {
		cmd.MemoryBytes = c.rec.PeakMemoryBytes
	}
	cmd.Reason = why
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
	if state == failed && c.failure != "" {
		failure = c.failure
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
		e.saveCommand(c, change{what: what, detail: detail, out: outbox{diagnostics: by.diagnostics()}})
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
	if est := cmd.Estimate; est != nil {
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: "estimate", Detail: estimateNote(est, c.rec.PeakMemoryBytes, seconds)})
	}
	f := &pb.FinishedCommand{
		Id: cmd.Id, RepoId: c.rec.RepoId, NodeId: cmd.NodeId, WorkspaceId: cmd.WorkspaceId, Agent: c.rec.Agent,
		Argv: cmd.Argv, Kind: cmd.Kind, State: state, ExitCode: code, Failure: failure,
		EndedAt: now, QueuedSeconds: float64(started-cmd.QueuedAt) / 1000, Seconds: seconds,
		ColdSeconds: cmd.ColdSeconds, PeakMemoryBytes: c.rec.PeakMemoryBytes, Parks: c.rec.Parks,
		SessionId: cmd.SessionId, Reason: why, Bench: cmd.Bench, Runtime: e.commandRuntime(c),
		Samples: cmd.Samples, Estimate: clone(cmd.Estimate),
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
	isVM := cmd.WorkspaceId != "" && e.usesRuntime(c.runtimeTarget())
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
	e.saveCommand(c, change{what: what, detail: detail, finished: f, feed: line, profile: prof, out: outbox{diagnostics: by.diagnostics()}})
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
		if cmd.Kind != "server" {
			e.recordUserActivityLocked(e.now())
		}
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

// unusedThrowawayGrace is how long a ready throwaway workspace waits, from
// its last sync, for the command it was made for: past it, the client that
// made it is taken to have given up on it.
const unusedThrowawayGrace = 10 * time.Minute

// throwawayReleaseRetry is how long after starting to release a throwaway
// workspace its release is started again, should the first fail.
const throwawayReleaseRetry = time.Minute

// releaseSpentThrowaways releases, in the background, each throwaway
// workspace that is done with (throwawaySpentLocked), and again a minute
// after a release that failed; one a worker restart cut short is done
// with all the same.
func (e *Engine) releaseSpentThrowaways() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for id, w := range e.workspaces {
		if !w.ws().Throwaway || w.ws().State == released || now.Before(w.releaseRetryAt) {
			continue
		}
		if why := e.throwawaySpentLocked(w); why != "" {
			slog.Info("release throwaway workspace", "workspace", id, "why", why)
			w.releaseRetryAt = now.Add(throwawayReleaseRetry)
			e.releaseWorkspaceAsyncLocked(id)
		}
	}
}

// throwawaySpentLocked says why throwaway workspace w is done with, or ""
// while it is not: its command has ended (or been forgotten, long ended),
// or, with none run in it, it has sat ready, or failed, unused past
// unusedThrowawayGrace. Call with e.mu held.
func (e *Engine) throwawaySpentLocked(w *workspace) string {
	if ran := w.rec.ThrowawayCommand; ran != "" {
		if c, ok := e.commands[ran]; ok && api.Live(c.cmd().State) {
			return ""
		}
		return "command " + ran + " ended"
	}
	ws := w.ws()
	if (ws.State != idle && ws.State != failedWS) || isSyncInProgress(ws.Reason) || e.workspaceBusy(ws.Id) {
		return ""
	}
	if e.now().Sub(time.UnixMilli(ws.LastActiveAt)) < unusedThrowawayGrace {
		return ""
	}
	return fmt.Sprintf("no command ran in it for %s", unusedThrowawayGrace)
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
