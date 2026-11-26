package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
	"github.com/semistrict/hotbed/internal/guestd"
)

// ErrVMRunning is returned when an operation requires a VM unit to be stopped
// (e.g. preparing or deleting its backing cell) while the VM is still alive.
var ErrVMRunning = errors.New("vm is still running")

// IsEphemeralBuildID reports whether id belongs to a single-incarnation base
// image or project base build VM (never adopted across worker restarts).
func IsEphemeralBuildID(id string) bool {
	return strings.HasPrefix(id, "base-") || strings.HasPrefix(id, "prov-")
}

// Instance represents one live VM backing a workspace.
type Instance struct {
	ID            string
	UnitName      string
	Dir           string
	CellPath      string
	CladefsVMSock string
	VsockUDS      string
	ConsoleLog    string
	// FirecrackerLog is Firecracker's own log; "" under machined.
	FirecrackerLog string
	GuestCID       uint32
	Net            NetAlloc
	Client         *guestd.Client
	StartedAt      time.Time
	ToolsDiskPath  string
	// VCPUs and MemMiB are the size it booted at, as its guest sees it.
	VCPUs  int
	MemMiB int
	// Boot is how long each step of its boot took; zero for a VM
	// reconnected after a worker restart.
	Boot BootTimes

	gitAuth io.Closer
}

// BootTimes is how long each step of a VM's boot took, from Start's call to
// its guestd answering.
type BootTimes struct {
	// Prepare is its directory and metadata.
	Prepare time.Duration
	// Launch is its VMM's boot: a Firecracker process's network tap, tools
	// disk, config and systemd unit, or machined's machine.
	Launch time.Duration
	// Guest is from the process starting to hotbed-guestd answering: the
	// guest kernel's boot, its init, and guestd's start.
	Guest time.Duration
	// HealthChecks is how many times guestd was asked before it answered.
	HealthChecks int
}

// Total is the whole boot.
func (b BootTimes) Total() time.Duration { return b.Prepare + b.Launch + b.Guest }

func (b BootTimes) String() string {
	ms := func(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }
	return fmt.Sprintf("%s: prepare %s, launch %s, guest %s (%d health checks)", ms(b.Total()), ms(b.Prepare), ms(b.Launch), ms(b.Guest), b.HealthChecks)
}

// vmMeta is persisted to <RunDir>/<id>/meta.json so hotbed-worker can reconnect
// to surviving VMs across worker restarts without disrupting them.
type vmMeta struct {
	ID             string    `json:"id"`
	UnitName       string    `json:"unitName"`
	CellPath       string    `json:"cellPath"`
	CladefsVMSock  string    `json:"cladefsVMSock,omitempty"`
	VsockUDS       string    `json:"vsockUDS"`
	ConsoleLog     string    `json:"consoleLog"`
	FirecrackerLog string    `json:"firecrackerLog,omitempty"`
	GuestCID       uint32    `json:"guestCID"`
	Net            NetAlloc  `json:"net"`
	StartedAt      time.Time `json:"startedAt"`
	ToolsDiskPath  string    `json:"toolsDiskPath,omitempty"`
	VCPUs          int       `json:"vcpus,omitempty"`
	MemMiB         int       `json:"memMiB,omitempty"`
}

const (
	crashedDirName   = "_crashed"
	maxCrashedVMDirs = 50
)

func isReservedRunDirName(name string) bool {
	return name == toolsDirName || name == crashedDirName
}

// Manager launches, tracks, reconnects, and tears down VMs, which its VMM
// runs. It implements runtime.GuestResolver via ClientFor.
type Manager struct {
	cfg      Config
	vmm      VMM
	dialerFn func(vsockUDS string) guestd.DialFunc

	mu        sync.Mutex
	instances map[string]*Instance
	starting  map[string]struct{}
	usedCIDs  map[uint32]string
	nextCID   uint32
}

// ManagerOption overrides optional Manager collaborators.
type ManagerOption func(*managerOptions)

type managerOptions struct {
	vmm         VMM
	launcher    Launcher
	dialerFn    func(vsockUDS string) guestd.DialFunc
	toolsDiskFn func(cacheDir, guestdBin string) (string, error)
}

// WithVMM runs the manager's VMs on v (machined's, Machined) instead of
// Firecracker; WithLauncher and WithToolsDiskBuilder are then Firecracker's,
// unused.
func WithVMM(v VMM) ManagerOption {
	return func(o *managerOptions) { o.vmm = v }
}

// WithLauncher replaces Firecracker's default SystemdLauncher.
func WithLauncher(l Launcher) ManagerOption {
	return func(o *managerOptions) { o.launcher = l }
}

// WithDialerFactory overrides how a VM's guestd.DialFunc is constructed from its
// vsock UDS path (default: guestd.FirecrackerVsockDialer).
func WithDialerFactory(fn func(vsockUDS string) guestd.DialFunc) ManagerOption {
	return func(o *managerOptions) { o.dialerFn = fn }
}

// WithToolsDiskBuilder overrides how Firecracker's read-only hotbed-tools
// disk image is built from cfg.GuestdBin (default: EnsureToolsDisk).
func WithToolsDiskBuilder(fn func(cacheDir, guestdBin string) (string, error)) ManagerOption {
	return func(o *managerOptions) { o.toolsDiskFn = fn }
}

// NewManager creates a VM manager rooted at cfg.RunDir, whose VMs are
// Firecracker's unless WithVMM says otherwise.
func NewManager(cfg Config, opts ...ManagerOption) (*Manager, error) {
	cfg = cfg.Normalized()
	if cfg.RunDir == "" {
		return nil, errors.New("vm: RunDir is required")
	}
	absRunDir, err := filepath.Abs(cfg.RunDir)
	if err != nil {
		return nil, fmt.Errorf("vm: resolve RunDir: %w", err)
	}
	cfg.RunDir = absRunDir
	if err := os.MkdirAll(cfg.RunDir, 0o755); err != nil {
		return nil, fmt.Errorf("vm: create RunDir %s: %w", cfg.RunDir, err)
	}
	o := managerOptions{
		launcher:    SystemdLauncher{},
		toolsDiskFn: EnsureToolsDisk,
		dialerFn: func(uds string) guestd.DialFunc {
			return guestd.FirecrackerVsockDialer(uds)
		},
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.vmm == nil {
		fc := &firecrackerVMM{launcher: o.launcher, toolsDiskFn: o.toolsDiskFn}
		if !cfg.DisableNetwork {
			fc.net, err = NewNetManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.VMUser)
			if err != nil {
				return nil, err
			}
		}
		o.vmm = fc
	}
	return &Manager{
		cfg:       cfg,
		vmm:       o.vmm,
		dialerFn:  o.dialerFn,
		instances: map[string]*Instance{},
		starting:  map[string]struct{}{},
		usedCIDs:  map[uint32]string{},
		nextCID:   3, // CID 0,1,2 are reserved (hypervisor, local, host)
	}, nil
}

// unitName is VM id's unit, its name to the VMM.
func unitName(id string) string { return "hotbed-vm-" + id }

// machine is how m names inst's VM to its VMM.
func (inst *Instance) machine() Machine {
	return Machine{ID: inst.ID, Unit: inst.UnitName, Dir: inst.Dir, ConsoleLog: inst.ConsoleLog, CID: inst.GuestCID}
}

// booted is what inst's boot took.
func (inst *Instance) booted() Booted {
	return Booted{VsockUDS: inst.VsockUDS, Net: inst.Net, ToolsDiskPath: inst.ToolsDiskPath, FirecrackerLog: inst.FirecrackerLog, VCPUs: inst.VCPUs, MemMiB: inst.MemMiB}
}

func (meta vmMeta) machine(dir string) Machine {
	return Machine{ID: meta.ID, Unit: meta.UnitName, Dir: dir, ConsoleLog: meta.ConsoleLog, CID: meta.GuestCID}
}

func (meta vmMeta) booted() Booted {
	return Booted{VsockUDS: meta.VsockUDS, Net: meta.Net, ToolsDiskPath: meta.ToolsDiskPath, FirecrackerLog: meta.FirecrackerLog, VCPUs: meta.VCPUs, MemMiB: meta.MemMiB}
}

// configSnapshot returns a copy of the manager's current Config under lock.
func (m *Manager) configSnapshot() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// UpdateConfig hot-reloads VM launch settings (kernel, guestd, firecracker
// binary, vCPUs, memory, disk, DAX window, cache type, DNS) for all subsequent
// VM boots without disrupting running Firecracker instances. Structural manager
// fields (RunDir, Slice, BridgeName, BridgeCIDR, DisableNetwork, GitAuthHostSock)
// are preserved when unset in cfg.
func (m *Manager) UpdateConfig(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg.RunDir = m.cfg.RunDir
	if cfg.Slice == "" {
		cfg.Slice = m.cfg.Slice
	}
	if cfg.BridgeName == "" {
		cfg.BridgeName = m.cfg.BridgeName
	}
	if cfg.BridgeCIDR == "" {
		cfg.BridgeCIDR = m.cfg.BridgeCIDR
	}
	if cfg.GitAuthHostSock == "" {
		cfg.GitAuthHostSock = m.cfg.GitAuthHostSock
	}
	cfg.DisableNetwork = m.cfg.DisableNetwork
	if cfg.BootTimeout <= 0 {
		cfg.BootTimeout = m.cfg.BootTimeout
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = m.cfg.ShutdownTimeout
	}
	if cfg.SyncShutdownTimeout <= 0 {
		cfg.SyncShutdownTimeout = m.cfg.SyncShutdownTimeout
	}
	m.cfg = cfg.Normalized()
}

// Start boots a VM for spec on the manager's VMM, waits for hotbed-guestd to
// report healthy over its vsock, and registers the live Instance.
func (m *Manager) Start(ctx context.Context, spec LaunchSpec) (*Instance, error) {
	if spec.ID == "" {
		return nil, errors.New("vm: LaunchSpec.ID is required")
	}
	if spec.CellHostPath == "" {
		return nil, errors.New("vm: LaunchSpec.CellHostPath is required")
	}

	m.mu.Lock()
	if existing, ok := m.instances[spec.ID]; ok {
		m.mu.Unlock()
		return existing, nil
	}
	if _, busy := m.starting[spec.ID]; busy {
		m.mu.Unlock()
		return nil, fmt.Errorf("vm %s: already starting: %w", spec.ID, ErrVMRunning)
	}
	m.starting[spec.ID] = struct{}{}
	cid := m.allocCIDLocked(spec.ID)
	cfg := m.cfg
	m.mu.Unlock()

	began := time.Now()
	var boot BootTimes
	startedOK := false
	defer func() {
		if !startedOK {
			m.mu.Lock()
			delete(m.starting, spec.ID)
			m.mu.Unlock()
		}
	}()

	mach := Machine{ID: spec.ID, Unit: unitName(spec.ID), Dir: filepath.Join(cfg.RunDir, spec.ID), CID: cid}
	mach.ConsoleLog = filepath.Join(mach.Dir, "console.log")
	if err := os.MkdirAll(mach.Dir, 0o755); err != nil {
		m.releaseCID(cid)
		return nil, fmt.Errorf("vm: create dir %s: %w", mach.Dir, err)
	}
	cleanupOnErr := func(b Booted) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		m.vmm.Free(cctx, mach, b)
		cancel()
		m.releaseCID(cid)
		_ = os.RemoveAll(mach.Dir)
	}

	now := time.Now().UTC()
	meta := vmMeta{
		ID:            spec.ID,
		UnitName:      mach.Unit,
		CellPath:      spec.CellHostPath,
		CladefsVMSock: spec.CladefsVMSock,
		ConsoleLog:    mach.ConsoleLog,
		GuestCID:      cid,
		StartedAt:     now,
	}
	// Written before the boot too, so a worker that dies during it leaves
	// the orphan reaper what to stop.
	if err := writeMeta(mach.Dir, meta); err != nil {
		cleanupOnErr(Booted{})
		return nil, fmt.Errorf("vm %s: persist metadata: %w", spec.ID, err)
	}

	launching := time.Now()
	boot.Prepare = launching.Sub(began)
	b, err := m.vmm.Boot(ctx, mach, spec, cfg)
	if err != nil {
		cleanupOnErr(Booted{})
		return nil, err
	}
	launched := time.Now()
	boot.Launch = launched.Sub(launching)
	meta.VsockUDS, meta.Net, meta.ToolsDiskPath, meta.FirecrackerLog, meta.VCPUs, meta.MemMiB = b.VsockUDS, b.Net, b.ToolsDiskPath, b.FirecrackerLog, b.VCPUs, b.MemMiB

	// Until the VM is confirmed gone, keep its metadata for the orphan
	// reaper: stop it on any failure from here on.
	stopOnErr := func(err error) (*Instance, error) {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
		// How it ended, before stopping it clears that (systemd's
		// reset-failed).
		preSt, _ := m.vmm.Status(stopCtx, mach.Unit)
		stopErr := m.forceStopUnit(stopCtx, mach.Unit)
		cancel()
		if stopErr != nil {
			slog.Error("vm start failed and its VM failed to stop; preserving metadata for orphan reaper",
				"vm", spec.ID, "unit", mach.Unit, "startErr", err, "stopErr", stopErr)
			return nil, fmt.Errorf("vm %s: %w (cleanup stop failed: %v)", spec.ID, err, stopErr)
		}
		if preSt.DiedAbnormally() {
			m.preserveCrashedLogs(spec.ID, mach.Unit, mach.Dir, preSt, "start failed", err)
		}
		cleanupOnErr(b)
		return nil, fmt.Errorf("vm %s: %w", spec.ID, err)
	}
	if err := writeMeta(mach.Dir, meta); err != nil {
		return stopOnErr(fmt.Errorf("persist metadata: %w", err))
	}
	var gaBridge *gitAuthBridge
	if cfg.GitAuthHostSock != "" {
		gaBridge, err = startGitAuthBridge(b.VsockUDS, cfg.GitAuthHostSock, cfg.VMUser)
		if err != nil {
			return stopOnErr(fmt.Errorf("start git-auth bridge: %w", err))
		}
	}

	client := guestd.NewClient(m.dialerFn(b.VsockUDS))
	checks, err := m.waitHealthy(ctx, mach.Unit, mach.ConsoleLog, b.FirecrackerLog, client)
	boot.Guest, boot.HealthChecks = time.Since(launched), checks
	if err != nil {
		if gaBridge != nil {
			_ = gaBridge.Close()
		}
		return stopOnErr(err)
	}

	inst := &Instance{
		ID:             spec.ID,
		UnitName:       mach.Unit,
		Dir:            mach.Dir,
		CellPath:       spec.CellHostPath,
		CladefsVMSock:  spec.CladefsVMSock,
		VsockUDS:       b.VsockUDS,
		ConsoleLog:     mach.ConsoleLog,
		FirecrackerLog: b.FirecrackerLog,
		GuestCID:       cid,
		Net:            b.Net,
		Client:         client,
		StartedAt:      now,
		ToolsDiskPath:  b.ToolsDiskPath,
		VCPUs:          b.VCPUs,
		MemMiB:         b.MemMiB,
		Boot:           boot,
		gitAuth:        gaBridge,
	}
	slog.Info("vm booted", "vm", spec.ID, "vcpus", b.VCPUs, "mem_mib", b.MemMiB, "total", boot.Total(),
		"prepare", boot.Prepare, "launch", boot.Launch, "guest", boot.Guest, "health_checks", boot.HealthChecks)
	m.mu.Lock()
	m.instances[spec.ID] = inst
	delete(m.starting, spec.ID)
	startedOK = true
	m.mu.Unlock()
	return inst, nil
}

// waitHealthy waits for the guestd of the VM unitName runs to answer, and
// says how many times it asked.
func (m *Manager) waitHealthy(ctx context.Context, unitName, consoleLog, fcLog string, client *guestd.Client) (int, error) {
	deadline := time.Now().Add(m.cfg.BootTimeout)
	var lastErr error
	checks := 0
	for {
		if ctx.Err() != nil {
			return checks, ctx.Err()
		}
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := client.Health(hctx)
		cancel()
		checks++
		if err == nil {
			return checks, nil
		}
		lastErr = err

		// Fail fast if the VM already exited (e.g. Firecracker's exit
		// 158/159 or a bad config).
		if st, serr := m.vmm.Status(ctx, unitName); serr == nil && st.Loaded && st.Exited {
			return checks, formatBootFailure(st.Why(), consoleLog, fcLog)
		}

		if time.Now().After(deadline) {
			return checks, formatBootFailure(fmt.Sprintf("timed out waiting for hotbed-guestd after %s (last err: %v)", m.cfg.BootTimeout, lastErr), consoleLog, fcLog)
		}
		select {
		case <-ctx.Done():
			return checks, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func formatBootFailure(base, consoleLog, fcLog string) error {
	var b strings.Builder
	b.WriteString(base)
	if tail := readTail(consoleLog, 2048); tail != "" {
		b.WriteString("\nconsole tail:\n")
		b.WriteString(tail)
	}
	if tail := readTail(fcLog, 2048); tail != "" {
		b.WriteString("\nfirecracker log tail:\n")
		b.WriteString(tail)
	}
	return errors.New(b.String())
}

// Reconnect scans cfg.RunDir for VMs that survived a hotbed-worker
// restart, re-attaches their git-auth vsock bridges and guestd clients, stops
// any ephemeral base/prov build VMs from a prior worker incarnation, and cleans
// up stale directories whose VMs are verified dead.
func (m *Manager) Reconnect(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(m.cfg.RunDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("vm reconnect: read %s: %w", m.cfg.RunDir, err)
	}
	reconnected := 0
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		id := ent.Name()
		if isReservedRunDirName(id) {
			continue
		}
		vmDir := filepath.Join(m.cfg.RunDir, id)
		meta, err := readMeta(vmDir)
		if err != nil {
			// Only remove a corrupt/incomplete dir if its unit is confirmed dead.
			if st, serr := m.vmm.Status(ctx, unitName(id)); serr == nil && !st.Alive() {
				m.vmm.Free(ctx, Machine{ID: id, Unit: unitName(id), Dir: vmDir}, Booted{})
				_ = os.RemoveAll(vmDir)
			}
			continue
		}
		mach, booted := meta.machine(vmDir), meta.booted()
		st, err := m.vmm.Status(ctx, meta.UnitName)
		if IsEphemeralBuildID(meta.ID) {
			// Ephemeral base-build / provisioning VMs belong to a single worker
			// incarnation; never adopt them across restarts.
			if err != nil || st.Alive() {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
				stopErr := m.forceStopUnit(stopCtx, meta.UnitName)
				cancel()
				if stopErr != nil {
					slog.Warn("vm reconnect: stop leftover build VM failed; preserving dir for reaper",
						"vm", meta.ID, "unit", meta.UnitName, "err", stopErr)
					continue
				}
			}
			m.vmm.Free(ctx, mach, booted)
			_ = os.RemoveAll(vmDir)
			slog.Info("vm reconnect: reaped leftover build VM", "vm", meta.ID, "unit", meta.UnitName)
			continue
		}
		if err != nil {
			slog.Warn("vm reconnect: status check failed; preserving dir", "vm", meta.ID, "unit", meta.UnitName, "err", err)
			continue
		}
		if st.Deactivating {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
			stopErr := m.forceStopUnit(stopCtx, meta.UnitName)
			cancel()
			if stopErr != nil {
				slog.Warn("vm reconnect: finish stopping deactivating VM failed", "vm", meta.ID, "err", stopErr)
				continue
			}
			m.vmm.Free(ctx, mach, booted)
			_ = os.RemoveAll(vmDir)
			continue
		}
		if !st.Running {
			if st.DiedAbnormally() {
				m.preserveCrashedLogs(meta.ID, meta.UnitName, vmDir, st, "dead on reconnect", nil)
			}
			m.vmm.Free(ctx, mach, booted)
			_ = os.RemoveAll(vmDir)
			continue
		}

		var gaBridge *gitAuthBridge
		if m.cfg.GitAuthHostSock != "" {
			if b, berr := startGitAuthBridge(meta.VsockUDS, m.cfg.GitAuthHostSock, m.cfg.VMUser); berr == nil {
				gaBridge = b
			} else {
				slog.Warn("vm reconnect: git-auth bridge failed", "vm", meta.ID, "err", berr)
			}
		}

		client := guestd.NewClient(m.dialerFn(meta.VsockUDS))
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, herr := client.Health(hctx)
		cancel()
		if herr != nil {
			slog.Warn("vm reconnect: running VM failed health check", "vm", meta.ID, "err", herr)
		}

		inst := &Instance{
			ID:             meta.ID,
			UnitName:       meta.UnitName,
			Dir:            vmDir,
			CellPath:       meta.CellPath,
			CladefsVMSock:  meta.CladefsVMSock,
			VsockUDS:       meta.VsockUDS,
			ConsoleLog:     meta.ConsoleLog,
			FirecrackerLog: meta.FirecrackerLog,
			GuestCID:       meta.GuestCID,
			Net:            meta.Net,
			Client:         client,
			StartedAt:      meta.StartedAt,
			ToolsDiskPath:  meta.ToolsDiskPath,
			VCPUs:          meta.VCPUs,
			MemMiB:         meta.MemMiB,
			gitAuth:        gaBridge,
		}
		m.mu.Lock()
		m.instances[meta.ID] = inst
		if meta.GuestCID >= 3 {
			m.usedCIDs[meta.GuestCID] = meta.ID
			if meta.GuestCID >= m.nextCID {
				m.nextCID = meta.GuestCID + 1
			}
		}
		m.mu.Unlock()
		m.vmm.Adopt(mach, booted)
		reconnected++
	}
	return reconnected, nil
}

func (m *Manager) stopTimeout() time.Duration {
	cfg := m.configSnapshot()
	return max(cfg.ShutdownTimeout*3, 15*time.Second)
}

func (m *Manager) killWaitTimeout() time.Duration {
	cfg := m.configSnapshot()
	return min(max(cfg.ShutdownTimeout, time.Second), 5*time.Second)
}

// forceStopUnit stops unitName, escalates to the VMM's Kill (SIGKILL) if it
// stays alive or Stop errors, and only returns nil once the VMM's Status
// confirms the unit is no longer alive.
func (m *Manager) forceStopUnit(ctx context.Context, unitName string) error {
	stopErr := m.vmm.Stop(ctx, unitName)
	st, stErr := m.vmm.Status(ctx, unitName)
	if stErr == nil && !st.Alive() {
		return nil
	}

	killErr := m.vmm.Kill(ctx, unitName)
	if !errors.Is(killErr, ErrNoKill) {
		deadline := time.Now().Add(m.killWaitTimeout())
	waitDead:
		for time.Now().Before(deadline) {
			st, stErr = m.vmm.Status(ctx, unitName)
			if stErr == nil && !st.Alive() {
				_ = m.vmm.Stop(ctx, unitName)
				return nil
			}
			select {
			case <-ctx.Done():
				break waitDead
			case <-time.After(25 * time.Millisecond):
			}
		}
		st, stErr = m.vmm.Status(ctx, unitName)
		if stErr == nil && !st.Alive() {
			_ = m.vmm.Stop(ctx, unitName)
			return nil
		}
	}

	if stErr != nil {
		return fmt.Errorf("verify unit %s stopped: %w (stop=%v, kill=%v)", unitName, stErr, stopErr, killErr)
	}
	return fmt.Errorf("unit %s is still alive after stop/kill (stop=%v, kill=%v)", unitName, stopErr, killErr)
}

// Stop gracefully shuts down the VM for id (syncing Fxfs inside the
// guest if syncFS is true), verifies the Firecracker unit actually exited
// (escalating to SIGKILL on failure/timeout), and ONLY THEN cleans up
// tracking, TAP, CID, and inst.Dir. If the unit cannot be confirmed dead,
// metadata and tracking are preserved for retry/reconciliation and an error is
// returned. When syncFS is true, Stop gives guestd SyncShutdownTimeout to flush
// dirty page cache synchronously and returns an error (after still stopping and
// cleaning up Firecracker) if the guest sync could not complete — preventing
// callers from snapshotting a half-flushed cell to R2. Idempotent if not tracked.
func (m *Manager) Stop(ctx context.Context, id string, syncFS bool) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return nil
	}

	var syncErr error
	if syncFS {
		switch {
		case ctx.Err() != nil:
			syncErr = fmt.Errorf("guest sync skipped: %w", ctx.Err())
		case inst.Client == nil:
			syncErr = errors.New("guest sync skipped: no guestd client")
		default:
			rpcTimeout := max(m.cfg.SyncShutdownTimeout, m.cfg.ShutdownTimeout)
			sctx, cancel := context.WithTimeout(ctx, rpcTimeout)
			if err := inst.Client.Shutdown(sctx, &guestdv1.ShutdownRequest{
				SyncFs:       true,
				StopCommands: true,
				PowerOff:     true,
			}); err != nil {
				syncErr = fmt.Errorf("guest shutdown/sync failed: %w", err)
				slog.Error("vm guest sync/shutdown failed before stop", "vm", id, "timeout", rpcTimeout, "err", err)
			}
			cancel()
		}
	} else if inst.Client != nil && ctx.Err() == nil {
		sctx, cancel := context.WithTimeout(ctx, m.cfg.ShutdownTimeout)
		_ = inst.Client.Shutdown(sctx, &guestdv1.ShutdownRequest{
			SyncFs:       false,
			StopCommands: true,
			PowerOff:     true,
		})
		cancel()
	}

	if syncErr == nil && inst.Client != nil && ctx.Err() == nil {
		// Wait briefly for the VM to exit cleanly on its own after PowerOff.
		deadline := time.Now().Add(m.cfg.ShutdownTimeout)
	waitLoop:
		for time.Now().Before(deadline) {
			st, err := m.vmm.Status(ctx, inst.UnitName)
			if err != nil || !st.Alive() {
				break
			}
			select {
			case <-ctx.Done():
				break waitLoop
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	// Always execute the stop/kill verification on a bounded non-cancelled
	// context so a cancelled caller context never skips stopping Firecracker or
	// strands a zombie VM without metadata.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
	defer cancel()

	// Capture exit status before forceStopUnit runs `systemctl reset-failed`,
	// which clears systemd's Result / ExecMainCode / ExecMainStatus fields.
	preSt, _ := m.vmm.Status(stopCtx, inst.UnitName)

	if err := m.forceStopUnit(stopCtx, inst.UnitName); err != nil {
		slog.Error("vm stop failed; preserving instance tracking and metadata",
			"vm", id, "unit", inst.UnitName, "dir", inst.Dir, "err", err)
		return fmt.Errorf("vm %s: stop failed: %w", id, err)
	}

	if preSt.DiedAbnormally() || syncErr != nil {
		m.preserveCrashedLogs(id, inst.UnitName, inst.Dir, preSt, "stop", syncErr)
	}

	m.mu.Lock()
	if cur := m.instances[id]; cur == inst {
		delete(m.instances, id)
	}
	m.mu.Unlock()

	if inst.gitAuth != nil {
		_ = inst.gitAuth.Close()
	}
	m.vmm.Free(stopCtx, inst.machine(), inst.booted())
	m.releaseCID(inst.GuestCID)
	_ = os.RemoveAll(inst.Dir)
	if syncErr != nil {
		return fmt.Errorf("vm %s: %w", id, syncErr)
	}
	return nil
}

// EnsureNotRunning verifies no live VM or in-flight launch holds
// id. If a tracked instance already exited, its stale state is cleaned up.
func (m *Manager) EnsureNotRunning(ctx context.Context, id string) error {
	m.mu.Lock()
	_, starting := m.starting[id]
	tracked := m.instances[id] != nil
	m.mu.Unlock()
	if starting {
		return fmt.Errorf("vm %s is currently starting: %w", id, ErrVMRunning)
	}
	unit := unitName(id)
	if st, err := m.vmm.Status(ctx, unit); err == nil && st.Alive() {
		return fmt.Errorf("vm %s (%s): %w", id, unit, ErrVMRunning)
	}
	if tracked {
		return m.Stop(ctx, id, false)
	}
	return nil
}

// ReconcileOrphans discovers every hotbed-vm-* unit its VMM runs, every
// tracked instance, and every RunDir entry not in ownedIDs (or currently
// starting), force-stops it, releases its TAP (even when RunDir/meta.json was
// already deleted), removes its RunDir, and sweeps orphaned TAPs on the bridge.
// Returns the reaped VM IDs and any recovered backing cell paths.
func (m *Manager) ReconcileOrphans(ctx context.Context, ownedIDs map[string]bool) (reapedIDs []string, reapedCellPaths []string) {
	cfg := m.configSnapshot()
	candidates := map[string]struct{}{}

	if units, err := m.vmm.List(ctx, cfg.Slice); err != nil {
		slog.Warn("vm reconcile orphans: list units failed", "slice", cfg.Slice, "err", err)
	} else {
		for _, u := range units {
			if id, ok := strings.CutPrefix(u, "hotbed-vm-"); ok && id != "" {
				candidates[id] = struct{}{}
			}
		}
	}

	m.mu.Lock()
	for id := range m.instances {
		candidates[id] = struct{}{}
	}
	m.mu.Unlock()

	if entries, err := os.ReadDir(cfg.RunDir); err == nil {
		for _, ent := range entries {
			if ent.IsDir() && ent.Name() != "" && !isReservedRunDirName(ent.Name()) {
				candidates[ent.Name()] = struct{}{}
			}
		}
	}

	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	stillAlive := map[string]bool{}
	for _, id := range ids {
		if ownedIDs[id] {
			continue
		}
		m.mu.Lock()
		if _, starting := m.starting[id]; starting {
			m.mu.Unlock()
			continue
		}
		inst := m.instances[id]
		m.mu.Unlock()

		vmDir := filepath.Join(cfg.RunDir, id)
		meta, metaErr := readMeta(vmDir)
		cellPath := ""
		if inst != nil && inst.CellPath != "" {
			cellPath = inst.CellPath
		} else if metaErr == nil && meta.CellPath != "" {
			cellPath = meta.CellPath
		}

		if inst != nil {
			if err := m.Stop(ctx, id, false); err != nil {
				slog.Warn("reap tracked orphan VM failed", "vm", id, "err", err)
				stillAlive[id] = true
				continue
			}
		} else {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
			preSt, _ := m.vmm.Status(stopCtx, unitName(id))
			err := m.forceStopUnit(stopCtx, unitName(id))
			if err != nil {
				cancel()
				slog.Warn("reap untracked orphan VM failed", "vm", id, "unit", unitName(id), "err", err)
				stillAlive[id] = true
				continue
			}
			if preSt.DiedAbnormally() {
				m.preserveCrashedLogs(id, unitName(id), vmDir, preSt, "orphan reap", nil)
			}
			var booted Booted
			if metaErr == nil {
				booted = meta.booted()
			}
			m.vmm.Free(stopCtx, Machine{ID: id, Unit: unitName(id), Dir: vmDir}, booted)
			cancel()
			_ = os.RemoveAll(vmDir)
		}

		reapedIDs = append(reapedIDs, id)
		if cellPath != "" {
			reapedCellPaths = append(reapedCellPaths, cellPath)
		}
		slog.Info("reaped orphan VM", "vm", id, "unit", unitName(id))
	}

	liveIDs := make(map[string]bool, len(ownedIDs)+len(stillAlive))
	for id := range ownedIDs {
		liveIDs[id] = true
	}
	for id := range stillAlive {
		liveIDs[id] = true
	}
	m.mu.Lock()
	for id := range m.instances {
		liveIDs[id] = true
	}
	for id := range m.starting {
		liveIDs[id] = true
	}
	m.mu.Unlock()
	m.vmm.Sweep(ctx, liveIDs)
	return reapedIDs, reapedCellPaths
}

// SetGitAuthHostSock sets the host Unix socket path for git-auth vsock
// bridging (port 5002) for subsequently launched VMs.
func (m *Manager) SetGitAuthHostSock(sock string) {
	m.mu.Lock()
	m.cfg.GitAuthHostSock = sock
	m.mu.Unlock()
}

// Get returns the live Instance for id, or nil if none is running.
func (m *Manager) Get(id string) *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[id]
}

// ClientFor implements runtime.GuestResolver.
func (m *Manager) ClientFor(workspaceID string) (*guestd.Client, error) {
	m.mu.Lock()
	inst, ok := m.instances[workspaceID]
	m.mu.Unlock()
	if !ok || inst.Client == nil {
		return nil, fmt.Errorf("vm: no running VM for workspace %s", workspaceID)
	}
	return inst.Client, nil
}

// StopVM implements runtime.GuestResolver.
func (m *Manager) StopVM(ctx context.Context, workspaceID string) error {
	return m.Stop(ctx, workspaceID, true)
}

// Status reports the status of id's VM, as its VMM has it.
func (m *Manager) Status(ctx context.Context, id string) (ProcessStatus, error) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return ProcessStatus{}, nil
	}
	return m.vmm.Status(ctx, inst.UnitName)
}

// List returns IDs of all tracked running VMs.
func (m *Manager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) allocCIDLocked(id string) uint32 {
	for {
		cid := m.nextCID
		m.nextCID++
		if m.nextCID < 3 {
			m.nextCID = 3
		}
		if _, used := m.usedCIDs[cid]; !used {
			m.usedCIDs[cid] = id
			return cid
		}
	}
}

func (m *Manager) releaseCID(cid uint32) {
	if cid < 3 {
		return
	}
	m.mu.Lock()
	delete(m.usedCIDs, cid)
	m.mu.Unlock()
}

func writeMeta(vmDir string, meta vmMeta) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(vmDir, "meta.json.tmp")
	dst := filepath.Join(vmDir, "meta.json")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func readMeta(vmDir string) (vmMeta, error) {
	b, err := os.ReadFile(filepath.Join(vmDir, "meta.json"))
	if err != nil {
		return vmMeta{}, err
	}
	var meta vmMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return vmMeta{}, err
	}
	if meta.ID == "" || meta.UnitName == "" {
		return vmMeta{}, errors.New("incomplete vm metadata")
	}
	return meta, nil
}

func readTail(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return ""
	}
	start := st.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, maxBytes))
	return strings.TrimSpace(string(b))
}

type crashedVMRecord struct {
	VMID       string        `json:"vmId"`
	UnitName   string        `json:"unitName"`
	CapturedAt time.Time     `json:"capturedAt"`
	Why        string        `json:"why"`
	SyncErr    string        `json:"syncErr,omitempty"`
	Status     ProcessStatus `json:"status"`
}

// preserveCrashedLogs copies its VMM's log (Firecracker's), console output, and metadata
// from vmDir into <RunDir>/_crashed/<timestamp>-<id>/ before vmDir is removed,
// keeping at most maxCrashedVMDirs entries.
func (m *Manager) preserveCrashedLogs(id, unitName, vmDir string, st ProcessStatus, why string, syncErr error) string {
	cfg := m.configSnapshot()
	if cfg.RunDir == "" {
		return ""
	}
	crashRoot := filepath.Join(cfg.RunDir, crashedDirName)
	now := time.Now().UTC()
	dstDir := filepath.Join(crashRoot, fmt.Sprintf("%s-%s", now.Format("20060102T150405.000Z"), id))
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		slog.Warn("preserve crashed VM logs: mkdir failed", "vm", id, "dir", dstDir, "err", err)
		return ""
	}

	for _, name := range []string{"firecracker.log", "console.log", "vm.json", "meta.json"} {
		_ = copyCrashFile(filepath.Join(vmDir, name), filepath.Join(dstDir, name))
	}

	rec := crashedVMRecord{
		VMID:       id,
		UnitName:   unitName,
		CapturedAt: now,
		Why:        why,
		Status:     st,
	}
	if syncErr != nil {
		rec.SyncErr = syncErr.Error()
	}
	if b, err := json.MarshalIndent(rec, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dstDir, "exit.json"), b, 0o644)
	}

	pruneCrashedDirs(crashRoot, maxCrashedVMDirs)
	slog.Warn("preserved crashed VM logs",
		"vm", id, "unit", unitName, "dir", dstDir, "why", why,
		"signal", st.Signal, "exitCode", st.ExitCode, "result", st.Result, "oomKilled", st.OOMKilled)
	return dstDir
}

func copyCrashFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(out, in)
	closeErr := out.Close()
	if cpErr != nil {
		return cpErr
	}
	return closeErr
}

func pruneCrashedDirs(crashRoot string, keep int) {
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(crashRoot)
	if err != nil || len(entries) <= keep {
		return
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			names = append(names, ent.Name())
		}
	}
	slices.Sort(names)
	if len(names) <= keep {
		return
	}
	for _, old := range names[:len(names)-keep] {
		_ = os.RemoveAll(filepath.Join(crashRoot, old))
	}
}
