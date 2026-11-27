package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

// isVMWorkspace reports whether workspace wsID was created with runtime="vm".
// It reads the lock-free vmWorkspaces index so it is safe to call both with and
// without e.mu held (e.g. from endBy/step/Exec as well as runtimeUnits).
func (e *Engine) isVMWorkspace(wsID string) bool {
	_, ok := e.vmWorkspaces.Load(wsID)
	return ok
}

// isVMQuiescent reports whether wsID's VM has no active workload running in
// it (no running command, no live server command, no interactive shell, no
// in-flight sync, and not preparing/releasing) so host cgroup reclaim can push
// cold VM RAM down to vmHostMinResidentBytes.
func (e *Engine) isVMQuiescent(wsID string) bool {
	if wsID == "" || !e.usesRuntime(wsID) {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if w, ok := e.workspaces[wsID]; ok && !vmWorkspaceSettled(w) {
		return false
	}
	if e.shellOpenLocked(wsID) {
		return false
	}
	for _, c := range e.commands {
		if c.runtimeTarget() != wsID || !api.Live(c.cmd().State) {
			continue
		}
		if c.cmd().State == running || c.cmd().Kind == "server" || c.admitting || c.stopping {
			return false
		}
	}
	return true
}

// vmWorkspaceSettled reports whether w is in a state its VM may be stopped or
// reclaimed in: not preparing, forking, syncing, failed or being released
// (release stops the VM itself). Call with e.mu held.
func vmWorkspaceSettled(w *workspace) bool {
	ws := w.ws()
	return !w.releasing && !w.forking && ws.State != preparing && ws.State != failedWS && ws.State != released && !isSyncInProgress(ws.Reason)
}

// shellOpenLocked reports whether an interactive shell is open in wsID.
// Call with e.mu held.
func (e *Engine) shellOpenLocked(wsID string) bool {
	for _, sh := range e.shells {
		if sh.workspaceID == wsID {
			return true
		}
	}
	return false
}

// commandInVMLocked is a command other than except that runs, is parked or
// paused, or is starting or stopping in VM wsID, or nil. Call with e.mu held.
func (e *Engine) commandInVMLocked(wsID string, except *command) *command {
	for _, c := range e.commands {
		if c == except || c.runtimeTarget() != wsID {
			continue
		}
		if started(c.cmd().State) || c.admitting || c.stopping {
			return c
		}
	}
	return nil
}

// vmBlockerLocked says why command c cannot start in its workspace's VM yet,
// or "": every command runs in a VM of its own, booted for it on the
// workspace's disk, and two VMs cannot share a disk, so c waits while another
// command holds the VM, and until the VM an earlier command ran in has
// stopped. A VM no command has run in yet (booted to prepare the workspace,
// sync a checkout or open a shell) is fresh, and c may start in it. Call with
// e.mu held.
func (e *Engine) vmBlockerLocked(c *command, w *workspace) string {
	if other := e.commandInVMLocked(w.ws().Id, c); other != nil {
		return fmt.Sprintf("waiting for its workspace: command %s runs in its VM", other.cmd().Id)
	}
	if !w.vmUsed {
		if e.shellOpenLocked(w.ws().Id) && vmSize(c) != (unit.VMSize{}) {
			// Booting it at c's size would take the shell's VM from under it.
			return "waiting for its workspace: a shell is open in its VM, and it asks a VM of its own size"
		}
		return ""
	}
	if e.shellOpenLocked(w.ws().Id) {
		return "waiting for its workspace: a shell is open in the VM an earlier command ran in"
	}
	return "waiting for its workspace: the VM an earlier command ran in is stopping"
}

// freshVMGrace is how long a VM no command has run in yet (booted to prepare
// its workspace, sync a checkout or list its files) waits for the command it
// was booted ahead of before it is stopped too: long enough for hotbed run's
// command to follow its sync. A command still queued then, waiting for room,
// boots its own VM once admitted rather than keep this one idle meanwhile.
const freshVMGrace = 30 * time.Second

// retireReasonLocked says why w's VM should stop now, or "" if it should not:
// at once when a command has run in it and nothing holds it any more (a VM
// is never reused for another command), or its workspace failed, and after
// freshVMGrace unused when no command has run in it. Call with e.mu held.
func (e *Engine) retireReasonLocked(w *workspace) string {
	ws := w.ws()
	if !e.usesRuntime(ws.Id) || (!vmWorkspaceSettled(w) && ws.State != failedWS) || w.releasing || w.forking {
		return ""
	}
	if !w.vmRetryAt.IsZero() && e.now().Before(w.vmRetryAt) {
		return ""
	}
	if e.commandInVMLocked(ws.Id, nil) != nil || e.shellOpenLocked(ws.Id) {
		return ""
	}
	if ws.State == failedWS {
		return "its workspace failed"
	}
	if w.vmUsed {
		return "the command that ran in it ended"
	}
	if ws.LastActiveAt > 0 && e.now().Sub(time.UnixMilli(ws.LastActiveAt)) < freshVMGrace {
		return ""
	}
	return "no command ran in it"
}

// retireVMs stops every workspace VM retireReasonLocked says to, keeping the
// workspace's disk: the next command boots a new VM on it.
func (e *Engine) retireVMs(ctx context.Context) {
	if e.cfg.Runtime == nil {
		return
	}
	vmUp := func(wsID string) bool {
		if r, ok := e.cfg.Runtime.(interface{ IsVMRunning(string) bool }); ok && r.IsVMRunning(wsID) {
			return true
		}
		return false
	}
	e.mu.Lock()
	var cands []*workspace
	for _, w := range e.workspaces {
		if e.retireReasonLocked(w) != "" {
			cands = append(cands, w)
		}
	}
	e.mu.Unlock()
	for _, w := range cands {
		if ctx.Err() != nil {
			return
		}
		wsID := w.ws().Id
		if !vmUp(wsID) {
			e.mu.Lock()
			w.vmUsed = false // it is gone already: the next VM is new
			e.mu.Unlock()
			continue
		}
		if !w.vm.TryLock() {
			continue // a sync, file read or shell is using it: next step
		}
		e.mu.Lock()
		why := e.retireReasonLocked(w)
		e.mu.Unlock()
		if why == "" {
			w.vm.Unlock()
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := e.cfg.Runtime.Teardown(tctx, wsID)
		cancel()
		if err != nil && !vmUp(wsID) {
			// It is gone all the same (only its guest's sync failed): the
			// next command boots a new one.
			slog.Warn("workspace VM stopped with an error", "workspace", wsID, "why", why, "err", err)
			err = nil
		}
		w.vm.Unlock()
		if err != nil {
			slog.Error("stop workspace VM", "workspace", wsID, "why", why, "err", err, "retryIn", time.Minute)
			if e.rtUnits != nil {
				// Until it stops, at least give back the RAM it holds.
				e.rtUnits.reclaimIdleVM(ctx, wsID)
			}
			e.mu.Lock()
			w.vmRetryAt = e.now().Add(time.Minute)
			e.mu.Unlock()
			continue
		}
		if e.rtUnits != nil {
			e.rtUnits.forgetVMCgroup(wsID)
		}
		e.mu.Lock()
		w.vmUsed = false
		w.vmRetryAt = time.Time{}
		e.mu.Unlock()
		slog.Info("stopped workspace VM", "workspace", wsID, "why", why)
		e.stepSoon()
	}
}

// resolveWorkspaceRuntime picks "vm" or "cladefs" for a CreateWorkspaceRequest.
func (e *Engine) resolveWorkspaceRuntime(req *pb.CreateWorkspaceRequest) (string, error) {
	rt := req.GetRuntime()
	if rt == "" && req.GetPlacement() != nil {
		rt = req.GetPlacement().GetRuntime()
	}
	if req.GetPlacement().GetBench() {
		if rt == runtime.KindVM {
			return "", api.Errorf(api.CodeInvalidArgs, "--bench requires cladefs runtime, not vm")
		}
		if rt != "" && rt != runtime.KindCladefs {
			return "", api.Errorf(api.CodeInvalidArgs, "unknown runtime %q (want vm or cladefs)", rt)
		}
		if e.cfg.CladefsFUSE != nil {
			return runtime.KindCladefs, nil
		}
		if e.cfg.VM != nil || e.cfg.Runtime != nil {
			return "", api.Errorf(api.CodePlatformNotSupported, "--bench requires cladefs runtime, which is not enabled on this worker")
		}
		return runtime.KindCladefs, nil
	}
	if rt == "" {
		if e.cfg.VM != nil {
			return e.cfg.VM.DefaultRuntime(), nil
		}
		if e.cfg.CladefsFUSE != nil {
			return e.cfg.CladefsFUSE.DefaultRuntime(), nil
		}
		if e.cfg.Runtime != nil && e.cfg.RuntimeFor == nil {
			return e.cfg.Runtime.Kind(), nil
		}
		return runtime.KindCladefs, nil
	}
	switch rt {
	case runtime.KindVM:
		if e.cfg.VM == nil && e.cfg.Runtime == nil {
			return "", api.Errorf(api.CodePlatformNotSupported, "vm runtime is not enabled on this worker")
		}
		return runtime.KindVM, nil
	case runtime.KindCladefs:
		if e.cfg.CladefsFUSE == nil && (e.cfg.VM != nil || e.cfg.Runtime != nil) {
			return "", api.Errorf(api.CodePlatformNotSupported, "cladefs host-FUSE runtime is not enabled on this worker")
		}
		return runtime.KindCladefs, nil
	default:
		return "", api.Errorf(api.CodeInvalidArgs, "unknown runtime %q (want vm or cladefs)", rt)
	}
}

// applyCladefsHeartbeat forwards controller-provided cladefs peer addresses
// and retired host incarnations to the local cladefsd backend.
func (e *Engine) applyCladefsHeartbeat(ctx context.Context, reply *pb.HeartbeatResponse) {
	if reply == nil {
		return
	}
	if e.cfg.VM != nil {
		e.cfg.VM.ApplyHeartbeat(ctx, reply.GetCladefsPeers(), reply.GetDecommissionIncarnations(), reply.GetRetireBases())
	} else if e.cfg.CladefsFUSE != nil {
		e.cfg.CladefsFUSE.ApplyHeartbeat(ctx, reply.GetCladefsPeers(), reply.GetDecommissionIncarnations(), reply.GetRetireBases())
	}
}

// ApplyWorkerConfig hot-reloads non-structural controller-managed worker
// configuration (VM tuning, managed kernel/guestd paths, DNS, default runtime,
// FUSE disk/peer settings) onto active backends without restarting the worker.
func (e *Engine) ApplyWorkerConfig(cfg statedir.WorkerConfig) {
	if e.cfg.VM != nil && cfg.VM != nil {
		e.cfg.VM.UpdateConfig(*cfg.VM)
	}
	if e.cfg.CladefsFUSE != nil && cfg.CladefsFUSE != nil {
		e.cfg.CladefsFUSE.UpdateConfig(*cfg.CladefsFUSE)
	}
	if e.freshenBudget != nil {
		e.freshenBudget.updateConfig(cfg.Freshen)
	}
}

// usesRuntime reports whether workspace wsID is executed via cfg.Runtime
// (e.g. a Firecracker microVM via hotbed-guestd) instead of host chroot.
func (e *Engine) usesRuntime(wsID string) bool {
	if e.cfg.Runtime == nil || wsID == "" {
		return false
	}
	if e.cfg.RuntimeFor != nil {
		return e.cfg.RuntimeFor(wsID)
	}
	return true
}

// vmCgroupPrefix marks synthetic cgroup identifiers for commands running inside
// a guest VM so Memory() and Reclaim() route through WorkspaceRuntime instead
// of reading or reclaiming host /sys/fs/cgroup files.
const vmCgroupPrefix = "vm:"

func makeVMCgroup(wsID, cmdID string) string {
	return vmCgroupPrefix + wsID + ":" + cmdID
}

func parseVMCgroup(cg string) (wsID, cmdID string, ok bool) {
	rest, ok := strings.CutPrefix(cg, vmCgroupPrefix)
	if !ok {
		return "", "", false
	}
	wsID, cmdID, ok = strings.Cut(rest, ":")
	return wsID, cmdID, ok && wsID != "" && cmdID != ""
}

// vmHostMinResidentBytes keeps Firecracker VMM, guest kernel, hotbed-guestd,
// and cladefsd DAX queues warm in host RAM when host-side cgroup memory.reclaim
// pushes a parked/paused VM command's cold anonymous heap out to host swap.
const vmHostMinResidentBytes int64 = 64 << 20

const (
	// guestCacheReclaimPressure triggers a non-disruptive guest-only cache
	// reclaim (sync -> drop_caches=3 -> compact_memory=1) when host memory PSI
	// (some avg10) reaches this percentage, well before admitPressure (22%) or
	// cgroup memory.high throttling.
	guestCacheReclaimPressure = 5.0
	// guestCacheReclaimMemRatio triggers guest cache reclaim once effective
	// memory usage reaches this fraction of admitLimit(), catching clean
	// cladefs page-cache buildup before it hits slice MemoryHigh/MemorySwapMax.
	guestCacheReclaimMemRatio = 0.80
	// guestCacheReclaimCooldown is the minimum interval between proactive
	// guest-cache reclaims on the same running VM.
	guestCacheReclaimCooldown = 30 * time.Second
)

type vmCacheReclaimState struct {
	inFlight bool
	lastAt   time.Time
}

// guestCacheReclaimReasonLocked returns a human-readable reason to proactively
// drop clean guest page cache on running VMs, or "" when the host has ample
// memory headroom. Call with e.mu held.
func (e *Engine) guestCacheReclaimReasonLocked(mem sysinfo.Mem, memP float64) string {
	if e.cfg.Runtime == nil {
		return ""
	}
	if memP >= guestCacheReclaimPressure {
		return fmt.Sprintf("host memory pressure %.1f%%", memP)
	}
	if e.objectStore != nil && (e.objectStore.GetMemoryFull() || e.objectStore.GetMemoryPressure()) {
		return "cladefsd memory pressure"
	}
	if limit := e.admitLimit(); limit > 0 && mem.Total > 0 {
		used := e.reportedMemUsedLocked(mem)
		if used >= int64(float64(limit)*guestCacheReclaimMemRatio) {
			return fmt.Sprintf("memory used %s/%s", gib(used), gib(limit))
		}
	}
	return ""
}

// scheduleGuestCacheReclaimLocked dispatches asynchronous guest-only Reclaim
// RPCs (sync -> drop_caches=3 -> compact_memory=1, without pausing the VM or
// pushing Firecracker into host swap) for running VM workspaces when host
// memory is under pressure. Call with e.mu held.
func (e *Engine) scheduleGuestCacheReclaimLocked(now time.Time, mem sysinfo.Mem, memP float64) {
	if e.cfg.Runtime == nil {
		return
	}
	activeWS := map[string]struct{}{}
	for _, c := range e.commands {
		if c.stopping || c.cmd().State != running {
			continue
		}
		wsID := c.cmd().WorkspaceId
		if !e.usesRuntime(wsID) {
			if cgWS, _, ok := parseVMCgroup(c.rec.Cgroup); ok && e.usesRuntime(cgWS) {
				wsID = cgWS
			} else {
				continue
			}
		}
		activeWS[wsID] = struct{}{}
	}
	for wsID, st := range e.vmCacheReclaim {
		if _, ok := activeWS[wsID]; !ok && !st.inFlight {
			delete(e.vmCacheReclaim, wsID)
		}
	}
	if len(activeWS) == 0 {
		return
	}
	reason := e.guestCacheReclaimReasonLocked(mem, memP)
	if reason == "" {
		return
	}
	if e.vmCacheReclaim == nil {
		e.vmCacheReclaim = map[string]vmCacheReclaimState{}
	}
	for wsID := range activeWS {
		st := e.vmCacheReclaim[wsID]
		if st.inFlight || (!st.lastAt.IsZero() && now.Sub(st.lastAt) < guestCacheReclaimCooldown) {
			continue
		}
		prev := st
		e.vmCacheReclaim[wsID] = vmCacheReclaimState{inFlight: true, lastAt: now}
		wsID, reason := wsID, reason
		started := e.startBgLocked(func(bgCtx context.Context) {
			defer func() {
				e.mu.Lock()
				cur := e.vmCacheReclaim[wsID]
				cur.inFlight = false
				e.vmCacheReclaim[wsID] = cur
				e.mu.Unlock()
			}()
			rctx, cancel := context.WithTimeout(bgCtx, 10*time.Second)
			err := e.cfg.Runtime.Reclaim(rctx, wsID, "", 0)
			cancel()
			if err != nil {
				if bgCtx.Err() == nil {
					slog.Warn("proactive guest page-cache reclaim failed", "workspace", wsID, "reason", reason, "err", err)
				}
				return
			}
			slog.Info("reclaimed guest page cache under host memory pressure", "workspace", wsID, "reason", reason)
		})
		if !started {
			e.vmCacheReclaim[wsID] = prev
		}
	}
}

// runtimeUnits wraps a host Units implementation (Systemd) and routes commands
// and interactive shells belonging to VM-backed workspaces through
// runtime.WorkspaceRuntime (hotbed-guestd over AF_VSOCK), while leaving chroot
// workspaces, base freshens, and --bench commands on the inner host Units.
type runtimeUnits struct {
	inner         Units
	rt            runtime.WorkspaceRuntime
	useRT         func(wsID string) bool
	isVMQuiescent func(wsID string) bool
	uid           int
	gid           int

	// pumpCtx bounds every guest log pump, which pumps run on until Close:
	// a pump stopped then resumes from the host log's end after a restart
	// (RegisterExisting). pumps counts the pumps running.
	pumpCtx   context.Context
	stopPumps context.CancelFunc
	pumps     sync.WaitGroup
	mu        sync.Mutex
	closed    bool              // Close began: no pump starts
	cmds      map[string]*rtCmd // unitName -> active/tracked guest command
	vmCgroups map[string]string // wsID -> host Firecracker unit cgroup path
}

type rtCmd struct {
	wsID    string
	cmdID   string
	logPath string
	cancel  context.CancelFunc
	done    chan struct{}
}

// newRuntimeUnits routes VM workspaces' units through rt; its log pumps run
// until ctx ends or Close.
func newRuntimeUnits(ctx context.Context, inner Units, rt runtime.WorkspaceRuntime, useRT func(string) bool, uid, gid int) *runtimeUnits {
	pumpCtx, stop := context.WithCancel(ctx)
	return &runtimeUnits{
		pumpCtx:   pumpCtx,
		stopPumps: stop,
		inner:     inner,
		rt:        rt,
		useRT:     useRT,
		uid:       uid,
		gid:       gid,
		cmds:      map[string]*rtCmd{},
		vmCgroups: map[string]string{},
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v
		}
	}
	return ""
}

// RegisterExisting re-attaches a live VM command loaded from worker.db after a
// hotbed-worker restart and resumes streaming its guest logs into logPath from
// the existing host file offset.
func (u *runtimeUnits) RegisterExisting(unitName, wsID, cmdID, logPath string) {
	if !u.useRT(wsID) {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, exists := u.cmds[unitName]; exists {
		return
	}
	u.trackLocked(unitName, wsID, cmdID, logPath)
}

// trackLocked tracks the guest command unitName and starts pumping its log
// into logPath, unless Close began. Call with u.mu held.
func (u *runtimeUnits) trackLocked(unitName, wsID, cmdID, logPath string) {
	ctx, cancel := context.WithCancel(u.pumpCtx)
	rc := &rtCmd{
		wsID:    wsID,
		cmdID:   cmdID,
		logPath: logPath,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	u.cmds[unitName] = rc
	if u.closed {
		cancel()
		close(rc.done)
		return
	}
	u.pumps.Add(1)
	go func() {
		defer u.pumps.Done()
		u.pumpLogs(ctx, rc)
	}()
}

// Close stops every log pump and waits for them to end. Guest commands run
// on: only their logs stop reaching the host until the worker restarts.
func (u *runtimeUnits) Close() {
	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()
	u.stopPumps()
	u.pumps.Wait()
}

func (u *runtimeUnits) lookup(unitName string) *rtCmd {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cmds[unitName]
}

func (u *runtimeUnits) Start(ctx context.Context, s unit.Spec) error {
	wsID := envValue(s.Env, "HOTBED_RUNTIME_TARGET")
	if wsID == "" {
		wsID = envValue(s.Env, "HOTBED_WORKSPACE")
	}
	if wsID == "" || !u.useRT(wsID) {
		return u.inner.Start(ctx, s)
	}
	cmdID := envValue(s.Env, "HOTBED_COMMAND")
	if cmdID == "" {
		cmdID = strings.TrimPrefix(s.Name, "hotbed-")
	}
	if s.Log != "" {
		if err := os.MkdirAll(filepath.Dir(s.Log), 0o755); err != nil {
			return err
		}
	}
	env := make([]string, 0, len(s.Env)+1)
	for _, kv := range s.Env {
		if strings.HasPrefix(kv, "HOTBED_RUNTIME_TARGET=") {
			continue
		}
		env = append(env, kv)
	}
	if len(s.PathPrefix) > 0 {
		env = append(env, "PATH="+strings.Join(s.PathPrefix, ":")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	err := u.rt.StartCommand(ctx, wsID, runtime.CommandSpec{
		ID:         cmdID,
		Argv:       s.Argv,
		Dir:        s.Dir,
		Env:        env,
		User:       s.User,
		UID:        u.uid,
		GID:        u.gid,
		CPUWeight:  s.CPUWeight,
		MemoryLow:  s.MemoryLow,
		MemoryHigh: s.MemoryHigh,
		LogPath:    s.Log,
		VM:         s.VM,
		TTY:        s.TTY,
		TTYInput:   s.TTYInput,
	})
	if err != nil {
		return err
	}

	u.mu.Lock()
	u.trackLocked(s.Name, wsID, cmdID, s.Log)
	u.mu.Unlock()
	return nil
}

func (u *runtimeUnits) pumpLogs(ctx context.Context, rc *rtCmd) {
	defer close(rc.done)
	if rc.logPath == "" {
		_, _ = u.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, 0, true, io.Discard)
		return
	}
	f, err := os.OpenFile(rc.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Error("open host log for VM command", "command", rc.cmdID, "path", rc.logPath, "err", err)
		return
	}
	defer f.Close()
	tw := cmdlog.NewTimestampWriter(rc.logPath, f)
	defer tw.Close()

	var offset int64
	if st, err := f.Stat(); err == nil {
		offset = st.Size()
	}
	for {
		n, err := u.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, offset, true, tw)
		offset += n
		if err == nil || ctx.Err() != nil {
			return
		}
		// Check whether the command already exited before retrying.
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		stMap, serr := u.rt.StatusMany(sctx, rc.wsID, []string{rc.cmdID})
		cancel()
		if serr == nil {
			if st, ok := stMap[rc.cmdID]; !ok || st.Exited {
				// Final non-follow drain to ensure trailing bytes are flushed.
				_, _ = u.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, offset, false, tw)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (u *runtimeUnits) liveCountInVM(wsID string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, rc := range u.cmds {
		if rc.wsID == wsID {
			n++
		}
	}
	return max(n, 1)
}

func (u *runtimeUnits) vmAdjustedMemory(ctx context.Context, wsID string, guestAnon, guestSwap int64, hostMem unit.MemoryUse, hasHost bool) unit.MemoryUse {
	anon, swap := guestAnon, guestSwap
	if hasHost {
		hostAnon := max(hostMem.Anon, 0)
		hostHeld := max(hostMem.Held(), 0)
		if anon > hostAnon {
			swap += anon - hostAnon
			anon = hostAnon
		}
		nLive := int64(u.liveCountInVM(wsID))
		// When the host Firecracker process holds more RAM than the guest
		// command's cgroup reports (e.g. guest tmpfs or guest kernel/page
		// cache), attribute the VM's excess host footprint above
		// vmHostMinResidentBytes across its live commands so admission
		// reservations and reclaim see the real host memory cost.
		vmResident := max(hostAnon-vmHostMinResidentBytes, 0) / nLive
		vmHeld := max(hostHeld-vmHostMinResidentBytes, 0) / nLive
		if vmResident > anon {
			anon = vmResident
		}
		if anon+swap < vmHeld {
			swap = vmHeld - anon
		}
	}
	return unit.MemoryUse{Anon: anon, Swap: swap}
}

func toUnitState(wsID string, st runtime.CommandStatus) unit.State {
	return unit.State{
		Loaded:     true,
		Running:    st.Running || st.Frozen,
		Frozen:     st.Frozen,
		Exited:     st.Exited,
		ExitCode:   st.ExitCode,
		Signal:     st.Signal,
		Result:     st.Result,
		OOMKilled:  st.OOMKilled,
		VMExit:     st.VMExit,
		Memory:     st.MemoryAnon,
		MemoryPeak: st.MemoryPeak,
		CPUNanos:   uint64(max(st.CPUUsec, 0)) * 1000,
		Cgroup:     makeVMCgroup(wsID, st.ID),
	}
}

func withMemoryUse(st unit.State, mem unit.MemoryUse) unit.State {
	m := mem
	st.MemoryUse = &m
	if m.Anon > st.Memory {
		st.Memory = m.Anon
	}
	return st
}

func (u *runtimeUnits) Status(ctx context.Context, name string) (unit.State, error) {
	rc := u.lookup(name)
	if rc == nil {
		return u.inner.Status(ctx, name)
	}
	m, err := u.rt.StatusMany(ctx, rc.wsID, []string{rc.cmdID})
	if err != nil {
		return unit.State{}, err
	}
	st, ok := m[rc.cmdID]
	if !ok {
		return unit.State{Loaded: false}, nil
	}
	_, hostMem, hasHost := u.vmHostMemory(ctx, rc.wsID)
	mem := u.vmAdjustedMemory(ctx, rc.wsID, st.MemoryAnon, st.MemorySwap, hostMem, hasHost)
	return withMemoryUse(toUnitState(rc.wsID, st), mem), nil
}

func (u *runtimeUnits) StatusMany(ctx context.Context, names []string) (map[string]unit.State, error) {
	var hostNames []string
	byWS := map[string][]*rtCmd{}
	byUnit := map[string]*rtCmd{}
	for _, name := range names {
		if rc := u.lookup(name); rc != nil {
			byWS[rc.wsID] = append(byWS[rc.wsID], rc)
			byUnit[name] = rc
		} else {
			hostNames = append(hostNames, name)
		}
	}
	out := make(map[string]unit.State, len(names))
	if len(hostNames) > 0 {
		hostStates, err := u.inner.StatusMany(ctx, hostNames)
		if err != nil {
			return nil, err
		}
		for k, v := range hostStates {
			out[k] = v
		}
	}
	var (
		outMu    sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	for wsID, rcs := range byWS {
		wsID, rcs := wsID, rcs
		wg.Go(func() {
			ids := make([]string, len(rcs))
			for i, rc := range rcs {
				ids[i] = rc.cmdID
			}
			vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			m, err := u.rt.StatusMany(vctx, wsID, ids)
			if err != nil {
				slog.Warn("read VM commands", "workspace", wsID, "count", len(ids), "err", err)
				outMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				outMu.Unlock()
				return
			}
			_, hostMem, hasHost := u.vmHostMemory(vctx, wsID)
			outMu.Lock()
			for name, rc := range byUnit {
				if rc.wsID != wsID {
					continue
				}
				if st, ok := m[rc.cmdID]; ok {
					mem := u.vmAdjustedMemory(vctx, wsID, st.MemoryAnon, st.MemorySwap, hostMem, hasHost)
					out[name] = withMemoryUse(toUnitState(wsID, st), mem)
				} else {
					out[name] = unit.State{Loaded: false}
				}
			}
			outMu.Unlock()
		})
	}
	wg.Wait()
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Freeze freezes the command's cgroup v2 INSIDE the guest via hotbed-guestd.
// Never freezes the host Firecracker process (which would trigger cladefsd
// quarantine timeout / exit 158).
func (u *runtimeUnits) Freeze(ctx context.Context, name string) error {
	if rc := u.lookup(name); rc != nil {
		return u.rt.FreezeCommand(ctx, rc.wsID, rc.cmdID)
	}
	return u.inner.Freeze(ctx, name)
}

func (u *runtimeUnits) Thaw(ctx context.Context, name string) error {
	if rc := u.lookup(name); rc != nil {
		return u.rt.ThawCommand(ctx, rc.wsID, rc.cmdID)
	}
	return u.inner.Thaw(ctx, name)
}

func (u *runtimeUnits) vmCgroup(ctx context.Context, wsID string) string {
	if u.inner == nil || wsID == "" {
		return ""
	}
	u.mu.Lock()
	cg := u.vmCgroups[wsID]
	u.mu.Unlock()
	if cg != "" {
		return cg
	}
	st, err := u.inner.Status(ctx, "hotbed-vm-"+wsID)
	if err != nil || !st.Loaded || st.Cgroup == "" {
		return ""
	}
	u.mu.Lock()
	if u.vmCgroups == nil {
		u.vmCgroups = map[string]string{}
	}
	u.vmCgroups[wsID] = st.Cgroup
	u.mu.Unlock()
	return st.Cgroup
}

func (u *runtimeUnits) forgetVMCgroup(wsID string) {
	u.mu.Lock()
	delete(u.vmCgroups, wsID)
	u.mu.Unlock()
}

func (u *runtimeUnits) vmHostMemory(ctx context.Context, wsID string) (cg string, mem unit.MemoryUse, ok bool) {
	for attempt := range 2 {
		cg = u.vmCgroup(ctx, wsID)
		if cg == "" {
			return "", unit.MemoryUse{}, false
		}
		m, err := u.inner.Memory(cg)
		if err == nil {
			return cg, m, true
		}
		if attempt == 0 && errors.Is(err, os.ErrNotExist) {
			u.forgetVMCgroup(wsID)
			continue
		}
		return cg, unit.MemoryUse{}, false
	}
	return "", unit.MemoryUse{}, false
}

func (u *runtimeUnits) reclaimIdleVM(ctx context.Context, wsID string) {
	gctx, gcancel := context.WithTimeout(ctx, 10*time.Second)
	_ = u.rt.Reclaim(gctx, wsID, "", 0)
	gcancel()
	hctx, hcancel := context.WithTimeout(ctx, 2*time.Second)
	vmCG, hostMem, hasHost := u.vmHostMemory(hctx, wsID)
	hcancel()
	if vmCG != "" && hasHost {
		if target := max(hostMem.Anon-vmHostMinResidentBytes, 0); target > 0 {
			if err := u.inner.Reclaim(vmCG, target); err != nil {
				slog.Warn("reclaim idle VM host cgroup failed", "workspace", wsID, "target", target, "err", err)
			}
		}
	}
}

func (u *runtimeUnits) Reclaim(cgroup string, n int64) error {
	if wsID, cmdID, ok := parseVMCgroup(cgroup); ok {
		quiescent := u.isVMQuiescent != nil && u.isVMQuiescent(wsID)

		// 1. Flush dirty buffers, drop clean page cache, and compact free
		// memory inside the guest so virtio-balloon free-page-reporting
		// returns it directly to the host without swap.
		gctx, gcancel := context.WithTimeout(context.Background(), 10*time.Second)
		guestErr := u.rt.Reclaim(gctx, wsID, cmdID, n)
		gcancel()

		// 2. Push the frozen guest command's cold anonymous RAM out of the host
		// Firecracker unit's cgroup into host zram/swap WITHOUT ever freezing
		// Firecracker (keeping cladefsd DAX unmaps and guestd responsive).
		// When the entire VM is quiescent, reclaim all host-resident anon above
		// vmHostMinResidentBytes (including guest tmpfs and guest kernel
		// memory that the guest command cgroup does not report).
		hctx, hcancel := context.WithTimeout(context.Background(), 2*time.Second)
		vmCG, hostMem, hasHostMem := u.vmHostMemory(hctx, wsID)
		if vmCG == "" {
			vmCG = u.vmCgroup(hctx, wsID)
		}
		hcancel()
		if vmCG != "" {
			var hostTarget int64
			if quiescent && hasHostMem {
				hostTarget = max(hostMem.Anon-vmHostMinResidentBytes, 0)
			} else if n > 0 {
				hostTarget = n
				if hasHostMem {
					reclaimable := max(hostMem.Anon-vmHostMinResidentBytes, 0)
					hostTarget = min(hostTarget, reclaimable)
				}
			}
			if hostTarget > 0 {
				if err := u.inner.Reclaim(vmCG, hostTarget); err != nil {
					return err
				}
				if guestErr != nil {
					slog.Warn("in-guest VM reclaim failed; host Firecracker cgroup reclaim succeeded",
						"workspace", wsID, "command", cmdID, "hostTarget", hostTarget, "err", guestErr)
				}
				return nil
			}
			if hasHostMem && guestErr == nil {
				return nil
			}
		}
		return guestErr
	}
	return u.inner.Reclaim(cgroup, n)
}

func (u *runtimeUnits) Memory(cgroup string) (unit.MemoryUse, error) {
	if wsID, cmdID, ok := parseVMCgroup(cgroup); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m, err := u.rt.StatusMany(ctx, wsID, []string{cmdID})
		if err != nil {
			return unit.MemoryUse{}, err
		}
		st, found := m[cmdID]
		if !found {
			return unit.MemoryUse{}, os.ErrNotExist
		}
		_, hostMem, hasHost := u.vmHostMemory(ctx, wsID)
		return u.vmAdjustedMemory(ctx, wsID, st.MemoryAnon, st.MemorySwap, hostMem, hasHost), nil
	}
	return u.inner.Memory(cgroup)
}

func (u *runtimeUnits) Stop(ctx context.Context, name string) error {
	rc := u.lookup(name)
	if rc == nil {
		return u.inner.Stop(ctx, name)
	}
	err := u.rt.StopCommand(ctx, rc.wsID, rc.cmdID, 3*time.Second)
	// Wait briefly for the log pump to flush final output before returning.
	select {
	case <-rc.done:
	case <-time.After(2 * time.Second):
		rc.cancel()
		<-rc.done
	case <-ctx.Done():
		rc.cancel()
	}
	u.mu.Lock()
	delete(u.cmds, name)
	u.mu.Unlock()
	return err
}

func (u *runtimeUnits) List(ctx context.Context, slice string) ([]string, error) {
	hostList, err := u.inner.List(ctx, slice)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	for name := range u.cmds {
		hostList = append(hostList, name)
	}
	u.mu.Unlock()
	return hostList, nil
}

func (u *runtimeUnits) Wait(ctx context.Context, slice, name string) error {
	if rc := u.lookup(name); rc != nil {
		select {
		case <-rc.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return u.inner.Wait(ctx, slice, name)
}

func (u *runtimeUnits) StartShell(ctx context.Context, s unit.Spec, sio unit.ShellIO) (unit.ShellHandle, error) {
	wsID := envValue(s.Env, "HOTBED_WORKSPACE")
	if wsID == "" || !u.useRT(wsID) {
		return u.inner.StartShell(ctx, s, sio)
	}
	env := append([]string(nil), s.Env...)
	if len(s.PathPrefix) > 0 {
		env = append(env, "PATH="+strings.Join(s.PathPrefix, ":")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return u.rt.StartShell(ctx, wsID, runtime.ShellSpec{
		Argv: s.Argv,
		Dir:  s.Dir,
		Env:  env,
		User: s.User,
		UID:  u.uid,
		GID:  u.gid,
	}, sio)
}

func (u *runtimeUnits) Exec(ctx context.Context, name, slice string, c unit.ExecSpec) (*exec.Cmd, error) {
	return u.inner.Exec(ctx, name, slice, c)
}

// applyViaRuntime delegates git bundle application inside a VM workspace to
// hotbed-guestd over AF_VSOCK (Connect Sync stream), without touching host paths.
func (e *Engine) applyViaRuntime(ctx context.Context, wsID, repo, bundlePath, commit, baseCommit string, subs []gitops.SubCheckout) (int, error) {
	defer e.useVM(wsID)()
	var uid, gid int
	if e.cred != nil {
		uid, gid = int(e.cred.Uid), int(e.cred.Gid)
	} else if e.cfg.BaseImage != nil {
		sp := e.cfg.BaseImage.Spec.Normalized()
		uid, gid = sp.UID, sp.GID
	}
	return e.cfg.Runtime.ApplySync(ctx, wsID, runtime.SyncSpec{
		Repo:       repo,
		BundlePath: bundlePath,
		Commit:     commit,
		BaseCommit: baseCommit,
		Subs:       subs,
		Env:        e.sandboxEnv(),
		UID:        uid,
		GID:        gid,
	})
}

// useVM holds wsID's VM up, booting it if need be, while something other
// than a command uses it, until the returned release: retireVMs stops no VM
// held so.
func (e *Engine) useVM(wsID string) (release func()) {
	e.mu.Lock()
	w, ok := e.workspaces[wsID]
	e.mu.Unlock()
	if !ok {
		return func() {}
	}
	w.vm.RLock()
	return w.vm.RUnlock
}

func (e *Engine) touchWorkspaceActive(wsID string) {
	e.mu.Lock()
	if w, ok := e.workspaces[wsID]; ok {
		w.ws().LastActiveAt = e.nowMs()
	}
	e.mu.Unlock()
}

func (e *Engine) filesViaRuntime(ctx context.Context, wsID string, repos []*pb.WorkspaceRepo) ([]*pb.FileEntry, error) {
	defer e.useVM(wsID)()
	e.touchWorkspaceActive(wsID)
	refs := make([]runtime.RepoRef, len(repos))
	for i, r := range repos {
		refs[i] = runtime.RepoRef{Repo: r.Repo, BaseCommit: r.BaseCommit}
	}
	return e.cfg.Runtime.ListFiles(ctx, wsID, refs)
}

func (e *Engine) fileViaRuntime(ctx context.Context, wsID, path string, repos []*pb.WorkspaceRepo) (*pb.FileContent, error) {
	defer e.useVM(wsID)()
	e.touchWorkspaceActive(wsID)
	r, rel, err := repoPath(path, repos)
	if err != nil {
		return nil, err
	}
	fc, err := e.cfg.Runtime.GetFile(ctx, wsID, r.Repo, r.BaseCommit, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, api.Errorf(api.CodeNotFound, "%s: %v", path, err)
		}
		return nil, api.Errorf(api.CodeInvalidArgs, "%s: %v", path, err)
	}
	fc.Path = path
	return fc, nil
}
