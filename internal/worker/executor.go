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
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/unit"
)

// commandExecutor runs the engine's commands, and reads and controls them,
// where they run: as systemd units on the host (unitExecutor) or in a
// workspace's or base build's VM, through the node's WorkspaceRuntime
// (guestExecutor). Engine.executorFor says which runs a command.
type commandExecutor interface {
	Start(ctx context.Context, l launch) error
	Status(ctx context.Context, r cmdRef) (unit.State, error)
	// StatusMany keys its states by cmdRef.unit; sampledAt is when it read
	// the CPU counters, or zero when it cannot say.
	StatusMany(ctx context.Context, refs []cmdRef) (states map[string]unit.State, sampledAt time.Time, err error)
	Freeze(ctx context.Context, r cmdRef) error
	Thaw(ctx context.Context, r cmdRef) error
	Stop(ctx context.Context, r cmdRef) error
	// Wait returns once the command's processes have all exited (or ctx is
	// done), so the engine notices an exit without waiting for its tick.
	Wait(ctx context.Context, r cmdRef) error
	// Memory is the command's own memory (anonymous and swapped), without
	// the file cache its cgroup holds too.
	Memory(ctx context.Context, r cmdRef) (unit.MemoryUse, error)
	// Reclaim pushes up to n bytes of the command's memory to swap.
	Reclaim(ctx context.Context, r cmdRef, n int64) error
}

// cmdRef names a command to its executor.
type cmdRef struct {
	unit   string // its unit name, hotbed-<id>
	slice  string // the slice its unit runs in, for Wait (unitExecutor)
	cgroup string // its unit's host cgroup, once seen (unitExecutor)
	target string // what it runs in: its workspace, or a base build's VM (guestExecutor)
	id     string // its command ID
}

// launch is how to start a command: as a systemd unit (host) on the host,
// or as a guest command (guest) in ref.target.
type launch struct {
	ref   cmdRef
	host  unit.Spec
	guest runtime.CommandSpec
}

// ref names c to its executor. It reads only what never changes of c, so
// needs no lock, and leaves slice and cgroup to callers that hold e.mu.
func (e *Engine) ref(c *command) cmdRef {
	return cmdRef{unit: c.unitName(), target: c.runtimeTarget(), id: c.cmd().Id}
}

// runsInGuest reports whether c runs in a VM, through the node's
// WorkspaceRuntime (e.guest), rather than on the host: a command of a VM
// workspace, or a base build's freshen in its VM.
func (e *Engine) runsInGuest(c *command) bool {
	return e.guest != nil && (c.isVMFreshen() || e.usesRuntime(c.runtimeTarget()))
}

// executorFor is the executor that runs c.
func (e *Engine) executorFor(c *command) commandExecutor {
	if e.runsInGuest(c) {
		return e.guest
	}
	return e.host
}

// errNoHostUnits is what a worker that runs nothing on the host (a macOS
// worker: every workspace there is a VM's) answers a command it would run
// there.
var errNoHostUnits = api.Errorf(api.CodePlatformNotSupported, "this worker runs commands only in VMs, never on the host")

// unitExecutor runs commands as systemd units on the host, through units
// (Systemd, or fake.Units in tests); with no units, the worker runs none
// there (errNoHostUnits).
type unitExecutor struct {
	units Units
	// wallClock says the engine keeps wall-clock time, so the CPU sample
	// times units report (statusSampler) are on its clock.
	wallClock bool
}

func (u unitExecutor) Start(ctx context.Context, l launch) error {
	if u.units == nil {
		return errNoHostUnits
	}
	return u.units.Start(ctx, l.host)
}

func (u unitExecutor) Status(ctx context.Context, r cmdRef) (unit.State, error) {
	if u.units == nil {
		return unit.State{}, errNoHostUnits
	}
	return u.units.Status(ctx, r.unit)
}

func (u unitExecutor) StatusMany(ctx context.Context, refs []cmdRef) (map[string]unit.State, time.Time, error) {
	if u.units == nil {
		return nil, time.Time{}, errNoHostUnits
	}
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.unit
	}
	if s, ok := u.units.(statusSampler); ok && u.wallClock {
		return s.StatusManyAt(ctx, names)
	}
	states, err := u.units.StatusMany(ctx, names)
	return states, time.Time{}, err
}

func (u unitExecutor) Freeze(ctx context.Context, r cmdRef) error {
	if u.units == nil {
		return errNoHostUnits
	}
	return u.units.Freeze(ctx, r.unit)
}

func (u unitExecutor) Thaw(ctx context.Context, r cmdRef) error {
	if u.units == nil {
		return errNoHostUnits
	}
	return u.units.Thaw(ctx, r.unit)
}

func (u unitExecutor) Stop(ctx context.Context, r cmdRef) error {
	if u.units == nil {
		return errNoHostUnits
	}
	return u.units.Stop(ctx, r.unit)
}

func (u unitExecutor) Wait(ctx context.Context, r cmdRef) error {
	if u.units == nil {
		return errNoHostUnits
	}
	return u.units.Wait(ctx, r.slice, r.unit)
}

// cgroup is r's unit's cgroup: the one the engine saw, or else the one its
// unit has now, or "" while it has none.
func (u unitExecutor) cgroup(ctx context.Context, r cmdRef) string {
	if r.cgroup != "" {
		return r.cgroup
	}
	st, err := u.units.Status(ctx, r.unit)
	if err != nil {
		slog.Warn("read a unit's cgroup", "unit", r.unit, "err", err)
		return ""
	}
	return st.Cgroup
}

func (u unitExecutor) Memory(ctx context.Context, r cmdRef) (unit.MemoryUse, error) {
	if u.units == nil {
		return unit.MemoryUse{}, errNoHostUnits
	}
	cg := u.cgroup(ctx, r)
	if cg == "" {
		return unit.MemoryUse{}, fmt.Errorf("unit %s has no cgroup: %w", r.unit, os.ErrNotExist)
	}
	return u.units.Memory(cg)
}

// Reclaim does nothing for a unit with no cgroup yet: it holds nothing.
func (u unitExecutor) Reclaim(ctx context.Context, r cmdRef, n int64) error {
	if u.units == nil {
		return errNoHostUnits
	}
	cg := u.cgroup(ctx, r)
	if cg == "" {
		return nil
	}
	return u.units.Reclaim(cg, n)
}

// Exec makes the command that runs c chrooted in a scope of its own, name,
// under slice: the worker's own work in a workspace's rootfs.
func (u unitExecutor) Exec(ctx context.Context, name, slice string, c unit.ExecSpec) (*exec.Cmd, error) {
	if u.units == nil {
		return nil, errNoHostUnits
	}
	return u.units.Exec(ctx, name, slice, c)
}

// StartShell runs a shell as a systemd unit on the host.
func (u unitExecutor) StartShell(ctx context.Context, s unit.Spec, sio unit.ShellIO) (unit.ShellHandle, error) {
	if u.units == nil {
		return nil, errNoHostUnits
	}
	return u.units.StartShell(ctx, s, sio)
}

// guestExecutor runs commands in VMs through rt (hotbed-guestd over
// AF_VSOCK), pumping each one's guest log into its host log file. A command
// it neither started nor was told of (register) is none of its own: it
// reads as not loaded, and nothing it asks of rt for it boots a VM.
type guestExecutor struct {
	rt runtime.WorkspaceRuntime
	// host reads the memory of the systemd units that run the VMs
	// themselves (hotbed-vm-<id>), and pushes it to swap; nil where VMs
	// are not systemd units (machined's).
	host Units
	// isVMQuiescent reports whether nothing runs in a VM now, so all its
	// host memory above vmHostMinResidentBytes may go to swap.
	isVMQuiescent func(target string) bool
	uid, gid      int

	// pumpCtx bounds every guest log pump, which pumps run on until Close:
	// a pump stopped then resumes from the host log's end after a restart
	// (register). pumps counts the pumps running.
	pumpCtx   context.Context
	stopPumps context.CancelFunc
	pumps     sync.WaitGroup
	mu        sync.Mutex
	closed    bool              // Close began: no pump starts
	cmds      map[string]*rtCmd // unit name -> guest command it runs
	vmCgroups map[string]string // target -> host cgroup of its VM's unit
}

type rtCmd struct {
	wsID    string
	cmdID   string
	logPath string
	cancel  context.CancelFunc
	done    chan struct{}
}

// newGuestExecutor runs commands in VMs through rt as uid:gid; its log
// pumps run until ctx ends or Close.
func newGuestExecutor(ctx context.Context, rt runtime.WorkspaceRuntime, host Units, uid, gid int) *guestExecutor {
	pumpCtx, stop := context.WithCancel(ctx)
	return &guestExecutor{
		rt:        rt,
		host:      host,
		uid:       uid,
		gid:       gid,
		pumpCtx:   pumpCtx,
		stopPumps: stop,
		cmds:      map[string]*rtCmd{},
		vmCgroups: map[string]string{},
	}
}

// guestPath is the PATH of a guest command or shell: pathPrefix first.
func guestPath(pathPrefix []string) string {
	return "PATH=" + strings.Join(append(pathPrefix[:len(pathPrefix):len(pathPrefix)], "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"), ":")
}

func (g *guestExecutor) Start(ctx context.Context, l launch) error {
	spec := l.guest
	spec.UID, spec.GID = g.uid, g.gid
	if spec.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o755); err != nil {
			return err
		}
	}
	if err := g.rt.StartCommand(ctx, l.ref.target, spec); err != nil {
		return err
	}
	g.mu.Lock()
	g.trackLocked(l.ref.unit, l.ref.target, l.ref.id, spec.LogPath)
	g.mu.Unlock()
	return nil
}

// register re-attaches a guest command loaded from worker.db after a
// hotbed-worker restart and resumes streaming its guest log into logPath
// from the host file's end.
func (g *guestExecutor) register(r cmdRef, logPath string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.cmds[r.unit]; exists {
		return
	}
	g.trackLocked(r.unit, r.target, r.id, logPath)
}

// trackLocked tracks the guest command unitName and starts pumping its log
// into logPath, unless Close began. Call with g.mu held.
func (g *guestExecutor) trackLocked(unitName, wsID, cmdID, logPath string) {
	ctx, cancel := context.WithCancel(g.pumpCtx)
	rc := &rtCmd{
		wsID:    wsID,
		cmdID:   cmdID,
		logPath: logPath,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	g.cmds[unitName] = rc
	if g.closed {
		cancel()
		close(rc.done)
		return
	}
	g.pumps.Add(1)
	go func() {
		defer g.pumps.Done()
		g.pumpLogs(ctx, rc)
	}()
}

// Close stops every log pump and waits for them to end. Guest commands run
// on: only their logs stop reaching the host until the worker restarts.
func (g *guestExecutor) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.stopPumps()
	g.pumps.Wait()
}

func (g *guestExecutor) lookup(unitName string) *rtCmd {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cmds[unitName]
}

// errNotInGuest is what is asked of a guest command the executor does not
// run (register).
func errNotInGuest(r cmdRef) error {
	return fmt.Errorf("command %s runs in no VM of this worker's: %w", r.id, os.ErrNotExist)
}

func (g *guestExecutor) pumpLogs(ctx context.Context, rc *rtCmd) {
	defer close(rc.done)
	if rc.logPath == "" {
		if _, err := g.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, 0, true, io.Discard); err != nil && ctx.Err() == nil {
			slog.Warn("follow a VM command's log", "command", rc.cmdID, "err", err)
		}
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
		n, err := g.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, offset, true, tw)
		offset += n
		if err == nil || ctx.Err() != nil {
			return
		}
		// Check whether the command already exited before retrying.
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		stMap, serr := g.rt.StatusMany(sctx, rc.wsID, []string{rc.cmdID})
		cancel()
		if serr == nil {
			if st, ok := stMap[rc.cmdID]; !ok || st.Exited {
				// Final non-follow drain to ensure trailing bytes are flushed.
				_, _ = g.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, offset, false, tw)
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

func (g *guestExecutor) liveCountInVM(wsID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, rc := range g.cmds {
		if rc.wsID == wsID {
			n++
		}
	}
	return max(n, 1)
}

func (g *guestExecutor) vmAdjustedMemory(wsID string, guestAnon, guestSwap int64, hostMem unit.MemoryUse, hasHost bool) unit.MemoryUse {
	anon, swap := guestAnon, guestSwap
	if hasHost {
		hostAnon := max(hostMem.Anon, 0)
		hostHeld := max(hostMem.Held(), 0)
		if anon > hostAnon {
			swap += anon - hostAnon
			anon = hostAnon
		}
		nLive := int64(g.liveCountInVM(wsID))
		// When the host VM process holds more RAM than the guest command's
		// cgroup reports (e.g. guest tmpfs or guest kernel/page cache),
		// attribute the VM's excess host footprint above
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

// guestState is a guest command's status as the engine reads a unit's. It
// has no cgroup: the guest's cgroups are not the host's.
func guestState(st runtime.CommandStatus, mem unit.MemoryUse) unit.State {
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
		Memory:     max(st.MemoryAnon, mem.Anon),
		MemoryPeak: st.MemoryPeak,
		MemoryUse:  &mem,
		CPUNanos:   uint64(max(st.CPUUsec, 0)) * 1000,
	}
}

func (g *guestExecutor) Status(ctx context.Context, r cmdRef) (unit.State, error) {
	states, _, err := g.StatusMany(ctx, []cmdRef{r})
	if err != nil {
		return unit.State{}, err
	}
	return states[r.unit], nil
}

func (g *guestExecutor) StatusMany(ctx context.Context, refs []cmdRef) (map[string]unit.State, time.Time, error) {
	out := make(map[string]unit.State, len(refs))
	byWS := map[string]map[string]*rtCmd{} // target -> unit -> command
	for _, r := range refs {
		rc := g.lookup(r.unit)
		if rc == nil {
			out[r.unit] = unit.State{Loaded: false}
			continue
		}
		if byWS[rc.wsID] == nil {
			byWS[rc.wsID] = map[string]*rtCmd{}
		}
		byWS[rc.wsID][r.unit] = rc
	}
	var (
		outMu    sync.Mutex
		firstErr error
		answered int
		wg       sync.WaitGroup
	)
	for wsID, units := range byWS {
		wg.Go(func() {
			ids := make([]string, 0, len(units))
			for _, rc := range units {
				ids = append(ids, rc.cmdID)
			}
			vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			m, err := g.rt.StatusMany(vctx, wsID, ids)
			if err != nil {
				slog.Warn("read VM commands", "workspace", wsID, "count", len(ids), "err", err)
				outMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				outMu.Unlock()
				return
			}
			_, hostMem, hasHost := g.vmHostMemory(vctx, wsID)
			outMu.Lock()
			defer outMu.Unlock()
			answered++
			for name, rc := range units {
				st, ok := m[rc.cmdID]
				if !ok {
					out[name] = unit.State{Loaded: false}
					continue
				}
				out[name] = guestState(st, g.vmAdjustedMemory(wsID, st.MemoryAnon, st.MemorySwap, hostMem, hasHost))
			}
		})
	}
	wg.Wait()
	if firstErr != nil && answered == 0 {
		// Not one VM could be asked: say so rather than leave its commands
		// out unexplained.
		return nil, time.Time{}, firstErr
	}
	return out, time.Time{}, nil
}

// Freeze freezes the command's cgroup v2 INSIDE the guest via hotbed-guestd.
// Never freezes the host VM process (which would trigger cladefsd
// quarantine timeout / exit 158).
func (g *guestExecutor) Freeze(ctx context.Context, r cmdRef) error {
	rc := g.lookup(r.unit)
	if rc == nil {
		return errNotInGuest(r)
	}
	return g.rt.FreezeCommand(ctx, rc.wsID, rc.cmdID)
}

func (g *guestExecutor) Thaw(ctx context.Context, r cmdRef) error {
	rc := g.lookup(r.unit)
	if rc == nil {
		return errNotInGuest(r)
	}
	return g.rt.ThawCommand(ctx, rc.wsID, rc.cmdID)
}

// Stop stops the command and waits briefly for its log pump to flush its
// last output. One it does not run is stopped already.
func (g *guestExecutor) Stop(ctx context.Context, r cmdRef) error {
	rc := g.lookup(r.unit)
	if rc == nil {
		return nil
	}
	err := g.rt.StopCommand(ctx, rc.wsID, rc.cmdID, 3*time.Second)
	select {
	case <-rc.done:
	case <-time.After(2 * time.Second):
		rc.cancel()
		<-rc.done
	case <-ctx.Done():
		rc.cancel()
	}
	g.mu.Lock()
	delete(g.cmds, r.unit)
	g.mu.Unlock()
	return err
}

func (g *guestExecutor) Wait(ctx context.Context, r cmdRef) error {
	rc := g.lookup(r.unit)
	if rc == nil {
		return nil
	}
	select {
	case <-rc.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// vmCgroup is the host cgroup of the systemd unit running target's VM, or
// "" where there is none.
func (g *guestExecutor) vmCgroup(ctx context.Context, wsID string) string {
	if g.host == nil || wsID == "" {
		return ""
	}
	g.mu.Lock()
	cg := g.vmCgroups[wsID]
	g.mu.Unlock()
	if cg != "" {
		return cg
	}
	st, err := g.host.Status(ctx, "hotbed-vm-"+wsID)
	if err != nil || !st.Loaded || st.Cgroup == "" {
		return ""
	}
	g.mu.Lock()
	g.vmCgroups[wsID] = st.Cgroup
	g.mu.Unlock()
	return st.Cgroup
}

func (g *guestExecutor) forgetVMCgroup(wsID string) {
	g.mu.Lock()
	delete(g.vmCgroups, wsID)
	g.mu.Unlock()
}

func (g *guestExecutor) vmHostMemory(ctx context.Context, wsID string) (cg string, mem unit.MemoryUse, ok bool) {
	for attempt := range 2 {
		cg = g.vmCgroup(ctx, wsID)
		if cg == "" {
			return "", unit.MemoryUse{}, false
		}
		m, err := g.host.Memory(cg)
		if err == nil {
			return cg, m, true
		}
		if attempt == 0 && errors.Is(err, os.ErrNotExist) {
			g.forgetVMCgroup(wsID)
			continue
		}
		return cg, unit.MemoryUse{}, false
	}
	return "", unit.MemoryUse{}, false
}

// reclaimIdleVM gives back what an idle VM holds: its guest's page cache,
// then its host memory above vmHostMinResidentBytes.
func (g *guestExecutor) reclaimIdleVM(ctx context.Context, wsID string) {
	gctx, gcancel := context.WithTimeout(ctx, 10*time.Second)
	if err := g.rt.Reclaim(gctx, wsID, "", 0); err != nil {
		slog.Warn("reclaim an idle VM's guest memory", "workspace", wsID, "err", err)
	}
	gcancel()
	hctx, hcancel := context.WithTimeout(ctx, 2*time.Second)
	vmCG, hostMem, hasHost := g.vmHostMemory(hctx, wsID)
	hcancel()
	if vmCG != "" && hasHost {
		if target := max(hostMem.Anon-vmHostMinResidentBytes, 0); target > 0 {
			if err := g.host.Reclaim(vmCG, target); err != nil {
				slog.Warn("reclaim idle VM host cgroup failed", "workspace", wsID, "target", target, "err", err)
			}
		}
	}
}

func (g *guestExecutor) Reclaim(ctx context.Context, r cmdRef, n int64) error {
	wsID, cmdID := r.target, r.id
	quiescent := g.isVMQuiescent != nil && g.isVMQuiescent(wsID)

	// 1. Flush dirty buffers, drop clean page cache, and compact free memory
	// inside the guest so virtio-balloon free-page-reporting returns it
	// directly to the host without swap.
	gctx, gcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	guestErr := g.rt.Reclaim(gctx, wsID, cmdID, n)
	gcancel()

	// 2. Push the frozen guest command's cold anonymous RAM out of the host
	// VM unit's cgroup into host zram/swap WITHOUT ever freezing the VM
	// (keeping cladefsd DAX unmaps and guestd responsive). When the entire
	// VM is quiescent, reclaim all host-resident anon above
	// vmHostMinResidentBytes (including guest tmpfs and guest kernel memory
	// that the guest command cgroup does not report).
	hctx, hcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	vmCG, hostMem, hasHostMem := g.vmHostMemory(hctx, wsID)
	if vmCG == "" {
		vmCG = g.vmCgroup(hctx, wsID)
	}
	hcancel()
	if vmCG != "" {
		var hostTarget int64
		if quiescent && hasHostMem {
			hostTarget = max(hostMem.Anon-vmHostMinResidentBytes, 0)
		} else if n > 0 {
			hostTarget = n
			if hasHostMem {
				hostTarget = min(hostTarget, max(hostMem.Anon-vmHostMinResidentBytes, 0))
			}
		}
		if hostTarget > 0 {
			if err := g.host.Reclaim(vmCG, hostTarget); err != nil {
				return err
			}
			if guestErr != nil {
				slog.Warn("in-guest VM reclaim failed; host VM cgroup reclaim succeeded",
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

func (g *guestExecutor) Memory(ctx context.Context, r cmdRef) (unit.MemoryUse, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	m, err := g.rt.StatusMany(ctx, r.target, []string{r.id})
	if err != nil {
		return unit.MemoryUse{}, err
	}
	st, found := m[r.id]
	if !found {
		return unit.MemoryUse{}, os.ErrNotExist
	}
	_, hostMem, hasHost := g.vmHostMemory(ctx, r.target)
	return g.vmAdjustedMemory(r.target, st.MemoryAnon, st.MemorySwap, hostMem, hasHost), nil
}

// StartShell runs a shell in target's VM.
func (g *guestExecutor) StartShell(ctx context.Context, target string, spec runtime.ShellSpec, sio unit.ShellIO) (unit.ShellHandle, error) {
	spec.UID, spec.GID = g.uid, g.gid
	return g.rt.StartShell(ctx, target, spec, sio)
}
