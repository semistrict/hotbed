package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/semistrict/hotbed/internal/api"
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
	if e.guest == nil {
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
			// Until it stops, at least give back the RAM it holds.
			e.guest.reclaimIdleVM(ctx, wsID)
			e.mu.Lock()
			w.vmRetryAt = e.now().Add(time.Minute)
			e.mu.Unlock()
			continue
		}
		e.guest.forgetVMCgroup(wsID)
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
	// fuse: cladefs workspaces mount on the host; vms: workspaces may run
	// in VMs (by a VM backend, or a runtime of their own in tests).
	b := e.cladefs()
	fuse := b != nil && b.Kind() == runtime.KindCladefs
	vms := (b != nil && b.Kind() == runtime.KindVM) || e.cfg.Runtime != nil
	if req.GetPlacement().GetBench() {
		if rt == runtime.KindVM {
			return "", api.Errorf(api.CodeInvalidArgs, "--bench requires cladefs runtime, not vm")
		}
		if rt != "" && rt != runtime.KindCladefs {
			return "", api.Errorf(api.CodeInvalidArgs, "unknown runtime %q (want vm or cladefs)", rt)
		}
		if fuse {
			return runtime.KindCladefs, nil
		}
		if vms {
			return "", api.Errorf(api.CodePlatformNotSupported, "--bench requires cladefs runtime, which is not enabled on this worker")
		}
		return runtime.KindCladefs, nil
	}
	if rt == "" {
		if b != nil {
			return b.DefaultRuntime(), nil
		}
		if e.cfg.Runtime != nil && e.cfg.RuntimeFor == nil {
			return e.cfg.Runtime.Kind(), nil
		}
		return runtime.KindCladefs, nil
	}
	switch rt {
	case runtime.KindVM:
		if !vms {
			return "", api.Errorf(api.CodePlatformNotSupported, "vm runtime is not enabled on this worker")
		}
		return runtime.KindVM, nil
	case runtime.KindCladefs:
		if !fuse && vms {
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
	if b := e.cladefs(); b != nil {
		b.ApplyHeartbeat(ctx, reply.GetCladefsPeers(), reply.GetDecommissionIncarnations(), reply.GetRetireBases())
	}
}

// ApplyWorkerConfig hot-reloads non-structural controller-managed worker
// configuration (VM tuning, managed kernel/guestd paths, DNS, default runtime,
// FUSE disk/peer settings) onto active backends without restarting the worker.
func (e *Engine) ApplyWorkerConfig(cfg statedir.WorkerConfig) {
	if b := e.cladefs(); b != nil {
		b.applyConfig(cfg)
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
		if c.stopping || c.cmd().State != running || !e.runsInGuest(c) {
			continue
		}
		activeWS[c.runtimeTarget()] = struct{}{}
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
