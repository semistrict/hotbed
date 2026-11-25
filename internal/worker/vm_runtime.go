package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/vm"
)

// isVMWorkspace reports whether workspace wsID was created with runtime="vm".
// It reads the lock-free vmWorkspaces index so it is safe to call both with and
// without e.mu held (e.g. from endBy/step/Exec as well as runtimeUnits).
func (e *Engine) isVMWorkspace(wsID string) bool {
	_, ok := e.vmWorkspaces.Load(wsID)
	return ok
}

func (e *Engine) vmIdleSuspendAfter() time.Duration {
	if e.cfg.VMIdleSuspendAfter < 0 {
		return 0
	}
	if e.cfg.VMIdleSuspendAfter > 0 {
		return e.cfg.VMIdleSuspendAfter
	}
	return defaultVMIdleSuspendAfter
}

// canSuspendVM reports whether wsID's VM is completely quiescent and safe to
// suspend to a CladeFS memory cell: no running command, no live server command,
// no active interactive shell, no in-flight sync, and not preparing/releasing.
func (e *Engine) canSuspendVM(wsID string) bool {
	if wsID == "" || !e.usesRuntime(wsID) {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.canSuspendVMLocked(wsID, false)
}

func (e *Engine) canSuspendVMLocked(wsID string, requireNoLiveCommands bool) bool {
	if w, ok := e.workspaces[wsID]; ok {
		ws := w.ws()
		if w.releasing || w.forking || ws.State == preparing || ws.State == failedWS || ws.State == released || isSyncInProgress(ws.Reason) {
			return false
		}
		if requireNoLiveCommands && ws.State != idle {
			return false
		}
	}
	for _, sh := range e.shells {
		if sh.workspaceID == wsID {
			return false
		}
	}
	for _, c := range e.commands {
		if c.runtimeTarget() != wsID {
			continue
		}
		st := c.cmd().State
		if !api.Live(st) {
			continue
		}
		if requireNoLiveCommands {
			return false
		}
		if st == running || c.cmd().Kind == "server" || c.admitting || c.stopping {
			return false
		}
	}
	return true
}

// vmPressureIdleSuspendAfter tightens idle VM hibernation/shutdown when the
// node is already under memory tightness or has commands parked waiting for
// room, so completely idle VMs vacate RAM/swap promptly instead of squatting
// for two minutes while active work stalls.
const vmPressureIdleSuspendAfter = 15 * time.Second

func (e *Engine) vmMemoryTightLocked() bool {
	for _, c := range e.commands {
		if !c.stopping && !c.deactivating && c.cmd().State == parked && c.rec.HeldFor == "" {
			return true
		}
	}
	if e.memPressureHolding || e.usage.GetMemoryPressure() >= admitPressure {
		return true
	}
	if e.lastMem.Total > 0 && float64(e.lastMem.Used()) >= float64(e.lastMem.Total)*admitMemory {
		return true
	}
	return false
}

// hibernateIdleVMs suspends VM workspaces that have remained completely idle
// (no live commands, shells, or syncs) for vmIdleSuspendAfter into CladeFS
// memory cells so idle workspaces hold zero host RAM and zero swap while
// staying fast to wake on the next sync, command, file query, or shell.
// When memory-cell suspend is unsupported for an idle VM (e.g. pre-upgrade
// Firecracker or --no-api VMs), it cleanly shuts down the idle VM via Teardown
// (preserving the CladeFS rootfs cell so EnsureVM reboots it transparently on
// the next command or sync) instead of letting idle VMs pin host RAM and swap.
func (e *Engine) hibernateIdleVMs(ctx context.Context) {
	sr, ok := e.cfg.Runtime.(runtime.SuspendableRuntime)
	if !ok {
		return
	}
	idleAfter := e.vmIdleSuspendAfter()
	if idleAfter <= 0 {
		return
	}
	runningCheck, hasRunningCheck := e.cfg.Runtime.(interface{ IsVMRunning(string) bool })
	now := e.now()
	e.mu.Lock()
	if e.vmMemoryTightLocked() {
		idleAfter = min(idleAfter, vmPressureIdleSuspendAfter)
	}
	var cands []*workspace
	for _, w := range e.workspaces {
		ws := w.ws()
		if !e.usesRuntime(ws.Id) {
			continue
		}
		if !e.canSuspendVMLocked(ws.Id, true) {
			continue
		}
		if !w.suspendRetryAt.IsZero() && now.Before(w.suspendRetryAt) {
			continue
		}
		if ws.LastActiveAt <= 0 || now.Sub(time.UnixMilli(ws.LastActiveAt)) < idleAfter {
			continue
		}
		if susp, _ := sr.IsSuspended(ws.Id); susp {
			continue
		}
		if hasRunningCheck && !runningCheck.IsVMRunning(ws.Id) {
			continue
		}
		cands = append(cands, w)
	}
	e.mu.Unlock()

	for _, w := range cands {
		if ctx.Err() != nil {
			return
		}
		if !w.sync.TryLock() {
			continue
		}
		wsID := w.ws().Id
		e.mu.Lock()
		curIdleAfter := e.vmIdleSuspendAfter()
		if e.vmMemoryTightLocked() {
			curIdleAfter = min(curIdleAfter, vmPressureIdleSuspendAfter)
		}
		stillIdle := curIdleAfter > 0 && e.canSuspendVMLocked(wsID, true) &&
			(w.suspendRetryAt.IsZero() || !e.now().Before(w.suspendRetryAt)) &&
			e.now().Sub(time.UnixMilli(w.ws().LastActiveAt)) >= curIdleAfter
		e.mu.Unlock()
		if !stillIdle {
			w.sync.Unlock()
			continue
		}
		if susp, _ := sr.IsSuspended(wsID); susp {
			w.sync.Unlock()
			continue
		}
		if hasRunningCheck && !runningCheck.IsVMRunning(wsID) {
			w.sync.Unlock()
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := sr.SuspendWorkspace(sctx, wsID, 0)
		cancel()
		if err != nil {
			if errors.Is(err, vm.ErrSuspendUnsupported) {
				// No live commands, shells, or syncs remain: stop the idle
				// Firecracker VM cleanly (SyncFs + PowerOff + StopVM) while
				// keeping its CladeFS rootfs cell intact. EnsureVM boots a
				// fresh VM on that cell transparently on the next use.
				tctx, tcancel := context.WithTimeout(ctx, 15*time.Second)
				terr := e.cfg.Runtime.Teardown(tctx, wsID)
				tcancel()
				w.sync.Unlock()
				if terr == nil {
					if e.rtUnits != nil {
						e.rtUnits.forgetVMCgroup(wsID)
					}
					e.mu.Lock()
					w.suspendRetryAt = time.Time{}
					e.mu.Unlock()
					slog.Info("stopped idle VM workspace to free host RAM (memory-cell suspend unsupported)", "workspace", wsID)
					continue
				}
				if e.rtUnits != nil {
					e.rtUnits.reclaimIdleVM(ctx, wsID)
				}
				e.mu.Lock()
				w.suspendRetryAt = e.now().Add(5 * time.Minute)
				e.mu.Unlock()
				slog.Warn("stop idle VM workspace failed; fell back to host reclaim", "workspace", wsID, "suspendErr", err, "stopErr", terr)
				continue
			}
			w.sync.Unlock()
			e.mu.Lock()
			w.suspendRetryAt = e.now().Add(time.Minute)
			e.mu.Unlock()
			slog.Warn("hibernate idle VM workspace failed", "workspace", wsID, "retryIn", time.Minute, "err", err)
			continue
		}
		w.sync.Unlock()
		if e.rtUnits != nil {
			e.rtUnits.forgetVMCgroup(wsID)
		}
		slog.Info("hibernated idle VM workspace to cladefs memory cell", "workspace", wsID)
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
		e.cfg.VM.ApplyHeartbeat(ctx, reply.GetCladefsPeers(), reply.GetDecommissionIncarnations())
	} else if e.cfg.CladefsFUSE != nil {
		e.cfg.CladefsFUSE.ApplyHeartbeat(ctx, reply.GetCladefsPeers(), reply.GetDecommissionIncarnations())
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

// runtimeUnits wraps a host Units implementation (Systemd) and routes commands
// and interactive shells belonging to VM-backed workspaces through
// runtime.WorkspaceRuntime (hotbed-guestd over AF_VSOCK), while leaving chroot
// workspaces, base freshens, and --bench commands on the inner host Units.
type runtimeUnits struct {
	inner      Units
	rt         runtime.WorkspaceRuntime
	useRT      func(wsID string) bool
	canSuspend func(wsID string) bool
	uid        int
	gid        int

	mu        sync.Mutex
	cmds      map[string]*rtCmd         // unitName -> active/tracked guest command
	vmCgroups map[string]string         // wsID -> host Firecracker unit cgroup path
	wakeCh    map[string]chan struct{}  // wsID -> closed & replaced whenever VM resumes
}

type rtCmd struct {
	wsID    string
	cmdID   string
	logPath string
	cancel  context.CancelFunc
	done    chan struct{}
}

func newRuntimeUnits(inner Units, rt runtime.WorkspaceRuntime, useRT func(string) bool, uid, gid int) *runtimeUnits {
	return &runtimeUnits{
		inner:     inner,
		rt:        rt,
		useRT:     useRT,
		uid:       uid,
		gid:       gid,
		cmds:      map[string]*rtCmd{},
		vmCgroups: map[string]string{},
		wakeCh:    map[string]chan struct{}{},
	}
}

func (u *runtimeUnits) isSuspended(wsID string) (bool, int64) {
	if sr, ok := u.rt.(runtime.SuspendableRuntime); ok {
		return sr.IsSuspended(wsID)
	}
	return false, 0
}

func (u *runtimeUnits) wakeWaiter(wsID string) <-chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	ch, ok := u.wakeCh[wsID]
	if !ok {
		ch = make(chan struct{})
		u.wakeCh[wsID] = ch
	}
	return ch
}

func (u *runtimeUnits) notifyResumed(wsID string) {
	u.mu.Lock()
	if ch, ok := u.wakeCh[wsID]; ok {
		close(ch)
		delete(u.wakeCh, wsID)
	}
	u.mu.Unlock()
}

// Drop cancels the log pump and forgets tracking for unitName without calling
// StopCommand inside the guest (used when releasing a workspace whose VM is
// already suspended and being discarded without resuming).
func (u *runtimeUnits) Drop(name string) {
	u.mu.Lock()
	rc := u.cmds[name]
	delete(u.cmds, name)
	u.mu.Unlock()
	if rc != nil {
		rc.cancel()
		<-rc.done
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
func (u *runtimeUnits) RegisterExisting(ctx context.Context, unitName, wsID, cmdID, logPath string) {
	if !u.useRT(wsID) {
		return
	}
	u.mu.Lock()
	if _, exists := u.cmds[unitName]; exists {
		u.mu.Unlock()
		return
	}
	pumpCtx, cancel := context.WithCancel(ctx)
	rc := &rtCmd{
		wsID:    wsID,
		cmdID:   cmdID,
		logPath: logPath,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	u.cmds[unitName] = rc
	u.mu.Unlock()

	go u.pumpLogs(pumpCtx, rc)
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
	})
	if err != nil {
		return err
	}

	pumpCtx, cancel := context.WithCancel(context.Background())
	rc := &rtCmd{
		wsID:    wsID,
		cmdID:   cmdID,
		logPath: s.Log,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	u.mu.Lock()
	u.cmds[s.Name] = rc
	u.mu.Unlock()

	go u.pumpLogs(pumpCtx, rc)
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

	var offset int64
	if st, err := f.Stat(); err == nil {
		offset = st.Size()
	}
	for {
		n, err := u.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, offset, true, f)
		offset += n
		if err == nil || ctx.Err() != nil {
			return
		}
		// While the workspace VM is intentionally suspended in a CladeFS memory
		// cell, keep the log pump alive without waking Firecracker and resume
		// streaming from the exact same offset once the VM wakes.
		if susp, _ := u.isSuspended(rc.wsID); susp {
			wake := u.wakeWaiter(rc.wsID)
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		// Check whether the command already exited before retrying.
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		stMap, serr := u.rt.StatusMany(sctx, rc.wsID, []string{rc.cmdID})
		cancel()
		if serr == nil {
			if st, ok := stMap[rc.cmdID]; !ok || st.Exited {
				// Final non-follow drain to ensure trailing bytes are flushed.
				_, _ = u.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, offset, false, f)
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
	if susp, wsBytes := u.isSuspended(wsID); susp {
		return unit.MemoryUse{Anon: 0, Swap: wsBytes}
	}
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
		// command's cgroup reports (e.g. guest zram pool, guest tmpfs, or
		// guest kernel/page cache), attribute the VM's excess host footprint
		// above vmHostMinResidentBytes across its live commands so admission
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
		if err := u.rt.ThawCommand(ctx, rc.wsID, rc.cmdID); err != nil {
			return err
		}
		u.notifyResumed(rc.wsID)
		return nil
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
	gctx, gcancel := context.WithTimeout(ctx, 3*time.Second)
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
		quiescent := u.canSuspend != nil && u.canSuspend(wsID)
		// Preferred path: when the entire workspace VM is quiescent (all live
		// commands parked/paused, no active shell or dev server), snapshot the
		// VM straight into a CladeFS memory cell and stop Firecracker so it
		// holds zero host RAM and zero swap.
		if sr, ok := u.rt.(runtime.SuspendableRuntime); ok && quiescent {
			if susp, _ := sr.IsSuspended(wsID); susp {
				return nil
			}
			workingSet := n
			hctx, hcancel := context.WithTimeout(context.Background(), 2*time.Second)
			if _, hostMem, hasHost := u.vmHostMemory(hctx, wsID); hasHost {
				workingSet = max(workingSet, hostMem.Anon)
			}
			hcancel()
			sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := sr.SuspendWorkspace(sctx, wsID, workingSet)
			scancel()
			if err == nil {
				u.forgetVMCgroup(wsID)
				return nil
			}
			if errors.Is(err, vm.ErrSuspendUnsupported) {
				slog.Warn("VM memory-cell suspend unsupported; falling back to cgroup reclaim",
					"workspace", wsID, "command", cmdID, "err", err)
			} else {
				slog.Error("VM memory-cell suspend failed; falling back to cgroup reclaim",
					"workspace", wsID, "command", cmdID, "err", err)
			}
		}

		// Fallback path (another command/shell/server is still running in the
		// same VM, or pre-upgrade VMM/cladefsd does not support suspend):
		// 1. Reclaim clean page cache inside the guest so virtio-balloon
		// free-page-reporting returns it directly to the host without swap.
		gctx, gcancel := context.WithTimeout(context.Background(), 3*time.Second)
		guestErr := u.rt.Reclaim(gctx, wsID, cmdID, n)
		gcancel()

		// 2. Push the frozen guest command's cold anonymous RAM out of the host
		// Firecracker unit's cgroup into host zram/swap WITHOUT ever freezing
		// Firecracker (keeping cladefsd DAX unmaps and guestd responsive).
		// When the entire VM is quiescent, reclaim all host-resident anon above
		// vmHostMinResidentBytes (including guest zram, tmpfs, and guest kernel
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
		if susp, wsBytes := u.isSuspended(wsID); susp {
			// A VM suspended to a CladeFS memory cell holds 0 bytes of host
			// RAM and 0 bytes of host swap; report its pre-suspend working set
			// in Swap so thaw estimation and PeakHeldBytes stay accurate while
			// parkedReservationForHigherPriority charges 0 resident RAM.
			return unit.MemoryUse{Anon: 0, Swap: wsBytes}, nil
		}
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

func (e *Engine) touchWorkspaceActive(wsID string) {
	e.mu.Lock()
	if w, ok := e.workspaces[wsID]; ok {
		w.ws().LastActiveAt = e.nowMs()
	}
	e.mu.Unlock()
}

func (e *Engine) filesViaRuntime(ctx context.Context, wsID string, repos []*pb.WorkspaceRepo) ([]*pb.FileEntry, error) {
	e.touchWorkspaceActive(wsID)
	refs := make([]runtime.RepoRef, len(repos))
	for i, r := range repos {
		refs[i] = runtime.RepoRef{Repo: r.Repo, BaseCommit: r.BaseCommit}
	}
	return e.cfg.Runtime.ListFiles(ctx, wsID, refs)
}

func (e *Engine) fileViaRuntime(ctx context.Context, wsID, path string, repos []*pb.WorkspaceRepo) (*pb.FileContent, error) {
	e.touchWorkspaceActive(wsID)
	clean := strings.TrimPrefix(filepath.Clean("/"+path), "/")
	name, rel, _ := strings.Cut(clean, "/")
	var r *pb.WorkspaceRepo
	for _, o := range repos {
		if o.Repo == name {
			r = o
		}
	}
	if r == nil || rel == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "path %q is in none of the workspace's repos", path).
			WithSuggestion("paths start with the repo, such as hotbed/go.mod; hotbed workspace files lists them")
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

// portProxy forwards TCP connections on a host port to a dev server listening
// on loopback inside a VM workspace via WorkspaceRuntime.DialPort (vsock 5003).
type portProxy struct {
	ln   net.Listener
	port uint16
	wsID string
	rt   runtime.WorkspaceRuntime
}

func startPortProxy(listenAddr string, wsID string, guestPort uint16, rt runtime.WorkspaceRuntime) (*portProxy, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen dev server proxy %s -> %s:%d: %w", listenAddr, wsID, guestPort, err)
	}
	p := &portProxy{ln: ln, port: guestPort, wsID: wsID, rt: rt}
	go p.serve()
	return p, nil
}

func (p *portProxy) Addr() string { return p.ln.Addr().String() }

func (p *portProxy) serve() {
	for {
		src, err := p.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			dst, err := p.rt.DialPort(ctx, p.wsID, p.port)
			cancel()
			if err != nil {
				return
			}
			defer dst.Close()
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(dst, c)
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				done <- struct{}{}
			}()
			go func() {
				_, _ = io.Copy(c, dst)
				if cw, ok := c.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				done <- struct{}{}
			}()
			<-done
			<-done
		}(src)
	}
}

func (p *portProxy) Close() error { return p.ln.Close() }
