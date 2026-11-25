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

// Instance represents one live Firecracker microVM backing a workspace.
type Instance struct {
	ID               string
	UnitName         string
	Dir              string
	CellPath         string
	CladefsVMSock    string
	APISock          string
	VsockUDS         string
	ConsoleLog       string
	GuestCID         uint32
	Net              NetAlloc
	Client           *guestd.Client
	StartedAt        time.Time
	FromMemoryCell   bool
	MemoryCellPath   string
	PrevMemCellPaths []string
	ToolsDiskPath    string

	gitAuth io.Closer
}

// vmMeta is persisted to <RunDir>/<id>/meta.json so hotbed-worker can reconnect
// to surviving or suspended Firecracker VMs across worker restarts without
// disrupting them.
type vmMeta struct {
	ID               string          `json:"id"`
	UnitName         string          `json:"unitName"`
	CellPath         string          `json:"cellPath"`
	CladefsVMSock    string          `json:"cladefsVMSock,omitempty"`
	APISock          string          `json:"apiSock,omitempty"`
	VsockUDS         string          `json:"vsockUDS"`
	ConsoleLog       string          `json:"consoleLog"`
	GuestCID         uint32          `json:"guestCID"`
	Net              NetAlloc        `json:"net"`
	StartedAt        time.Time       `json:"startedAt"`
	FromMemoryCell   bool            `json:"fromMemoryCell,omitempty"`
	MemoryCellPath   string          `json:"memoryCellPath,omitempty"`
	PrevMemCellPaths []string        `json:"prevMemCellPaths,omitempty"`
	ToolsDiskPath    string          `json:"toolsDiskPath,omitempty"`
	Suspended        *SuspendedState `json:"suspended,omitempty"`
}

// Manager launches, tracks, reconnects, suspends, resumes, and tears down
// Firecracker microVMs. It implements runtime.GuestResolver via ClientFor.
type Manager struct {
	cfg         Config
	launcher    Launcher
	net         *NetManager
	dialerFn    func(vsockUDS string) guestd.DialFunc
	toolsDiskFn func(cacheDir, guestdBin string) (string, error)

	toolsMu        sync.Mutex
	toolsCacheBin  string
	toolsCacheSize int64
	toolsCacheMod  int64
	toolsCachePath string

	mu        sync.Mutex
	instances map[string]*Instance
	suspended map[string]*SuspendedState
	starting  map[string]struct{}
	vmLocks   map[string]*sync.Mutex
	usedCIDs  map[uint32]string
	nextCID   uint32
}

// ManagerOption overrides optional Manager collaborators (used in tests).
type ManagerOption func(*Manager)

// WithLauncher replaces the default SystemdLauncher.
func WithLauncher(l Launcher) ManagerOption {
	return func(m *Manager) { m.launcher = l }
}

// WithDialerFactory overrides how a VM's guestd.DialFunc is constructed from its
// vsock UDS path (default: guestd.FirecrackerVsockDialer).
func WithDialerFactory(fn func(vsockUDS string) guestd.DialFunc) ManagerOption {
	return func(m *Manager) { m.dialerFn = fn }
}

// WithToolsDiskBuilder overrides how the read-only hotbed-tools disk image is
// built from cfg.GuestdBin (default: EnsureToolsDisk).
func WithToolsDiskBuilder(fn func(cacheDir, guestdBin string) (string, error)) ManagerOption {
	return func(m *Manager) { m.toolsDiskFn = fn }
}

// NewManager creates a Firecracker VM manager rooted at cfg.RunDir.
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
	var nm *NetManager
	if !cfg.DisableNetwork {
		nm, err = NewNetManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.VMUser)
		if err != nil {
			return nil, err
		}
	}
	m := &Manager{
		cfg:         cfg,
		launcher:    SystemdLauncher{},
		net:         nm,
		toolsDiskFn: EnsureToolsDisk,
		dialerFn: func(uds string) guestd.DialFunc {
			return guestd.FirecrackerVsockDialer(uds)
		},
		instances: map[string]*Instance{},
		suspended: map[string]*SuspendedState{},
		starting:  map[string]struct{}{},
		vmLocks:   map[string]*sync.Mutex{},
		usedCIDs:  map[uint32]string{},
		nextCID:   3, // CID 0,1,2 are reserved (hypervisor, local, host)
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// FirecrackerConfig matches Firecracker's --config-file JSON schema (including
// cladefs's virtio-extent "cladefs" device array).
type FirecrackerConfig struct {
	BootSource    fcBootSource     `json:"boot-source"`
	Drives        []fcDrive        `json:"drives"`
	MachineConfig fcMachineConfig  `json:"machine-config"`
	Vsock         fcVsock          `json:"vsock"`
	NetInterfaces []fcNetInterface `json:"network-interfaces,omitempty"`
	Balloon       *fcBalloon       `json:"balloon,omitempty"`
	Cladefs       []fcCladefs      `json:"cladefs"`
}

type fcDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type fcBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type fcMachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type fcVsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type fcNetInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac,omitempty"`
	HostDevName string `json:"host_dev_name"`
}

type fcBalloon struct {
	AmountMiB            int  `json:"amount_mib"`
	DeflateOnOOM         bool `json:"deflate_on_oom"`
	StatsPollingInterval int  `json:"stats_polling_interval_s,omitempty"`
	FreePageReporting    bool `json:"free_page_reporting,omitempty"`
}

type fcCladefs struct {
	ID                 string `json:"id"`
	Tag                string `json:"tag"`
	PathOnHost         string `json:"path_on_host"`
	Capacity           uint64 `json:"capacity"`
	CladefsdSocket     string `json:"cladefsd_socket"`
	DAXWindowSize      uint64 `json:"dax_window_size,omitempty"`
	DAXPrivateMaxBytes uint64 `json:"dax_private_max_bytes,omitempty"`
	CacheType          string `json:"cache_type,omitempty"`
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

// BuildFirecrackerConfig constructs the Firecracker JSON config for spec.
func (m *Manager) BuildFirecrackerConfig(spec LaunchSpec, cid uint32, vsockUDS string, netAlloc NetAlloc) FirecrackerConfig {
	cfg := m.configSnapshot()
	vcpus := spec.VCPUs
	if vcpus <= 0 {
		vcpus = cfg.DefaultVCPUs
	}
	memMiB := spec.MemMiB
	if memMiB <= 0 {
		memMiB = cfg.DefaultMemMiB
	}
	diskBytes := spec.DiskBytes
	if diskBytes == 0 {
		diskBytes = cfg.DefaultDiskBytes
	}
	var bootParts []string
	drives := []fcDrive{}
	if spec.ToolsDiskPath != "" {
		// Boot from the read-only host tools disk (/dev/vda, appended by
		// Firecracker because is_root_device=true & is_read_only=true), then
		// hotbed-guestd PID 1 mounts cladefs:rootfs and pivot_roots onto it.
		bootParts = []string{
			"console=ttyS0",
			"reboot=k",
			"panic=1",
			"psi=1",
			"init=/hotbed-guestd",
			"HOTBED_ROOT=cladefs:rootfs",
		}
		drives = []fcDrive{{
			DriveID:      "hotbed-tools",
			PathOnHost:   spec.ToolsDiskPath,
			IsRootDevice: true,
			IsReadOnly:   true,
		}}
	} else {
		bootParts = []string{
			"root=rootfs",
			"rootfstype=cladefs",
			"rw",
			"console=ttyS0",
			"reboot=k",
			"panic=1",
			"psi=1",
			"init=/sbin/hotbed-guestd",
		}
	}
	if ipArg := netAlloc.KernelIPArg(); ipArg != "" {
		bootParts = append(bootParts, ipArg)
	}
	if cfg.Nofile > 0 {
		bootParts = append(bootParts, fmt.Sprintf("HOTBED_NOFILE=%d", cfg.Nofile))
	}
	if extra := strings.TrimSpace(spec.ExtraBootArgs); extra != "" {
		bootParts = append(bootParts, extra)
	}

	fc := FirecrackerConfig{
		BootSource: fcBootSource{
			KernelImagePath: cfg.KernelPath,
			BootArgs:        strings.Join(bootParts, " "),
		},
		Drives: drives,
		MachineConfig: fcMachineConfig{
			VCPUCount:  vcpus,
			MemSizeMiB: memMiB,
		},
		Vsock: fcVsock{
			GuestCID: cid,
			UDSPath:  vsockUDS,
		},
		Balloon: &fcBalloon{
			AmountMiB:            cfg.BalloonTargetMiB,
			DeflateOnOOM:         true,
			StatsPollingInterval: 5,
			FreePageReporting:    true,
		},
		Cladefs: []fcCladefs{{
			ID:                 "rootfs",
			Tag:                "rootfs",
			PathOnHost:         spec.CellHostPath,
			Capacity:           diskBytes,
			CladefsdSocket:     spec.CladefsVMSock,
			DAXWindowSize:      cfg.DAXWindowBytes,
			DAXPrivateMaxBytes: cfg.DAXPrivateMaxBytes,
			CacheType:          cfg.CacheType,
		}},
	}
	if netAlloc.TAPName != "" {
		fc.NetInterfaces = []fcNetInterface{{
			IfaceID:     "eth0",
			GuestMAC:    netAlloc.GuestMAC,
			HostDevName: netAlloc.TAPName,
		}}
	}
	return fc
}

// ensureToolsDisk builds or returns the cached read-only ext4 disk image for
// guestdBin. Stat metadata is cached in memory so repeated VM boots avoid
// re-hashing the binary while still rebuilding whenever guestdBin changes.
func (m *Manager) ensureToolsDisk(runDir, guestdBin string) (string, error) {
	m.toolsMu.Lock()
	defer m.toolsMu.Unlock()

	cacheDir := filepath.Join(runDir, toolsDirName)
	if info, err := os.Stat(guestdBin); err == nil {
		if m.toolsCachePath != "" &&
			m.toolsCacheBin == guestdBin &&
			m.toolsCacheSize == info.Size() &&
			m.toolsCacheMod == info.ModTime().UnixNano() {
			if st, serr := os.Stat(m.toolsCachePath); serr == nil && st.Size() > 0 {
				return m.toolsCachePath, nil
			}
		}
		p, err := m.toolsDiskFn(cacheDir, guestdBin)
		if err != nil {
			return "", err
		}
		m.toolsCacheBin = guestdBin
		m.toolsCacheSize = info.Size()
		m.toolsCacheMod = info.ModTime().UnixNano()
		m.toolsCachePath = p
		return p, nil
	}
	return m.toolsDiskFn(cacheDir, guestdBin)
}

// Start boots a Firecracker microVM for spec, waits for hotbed-guestd to report
// healthy over AF_VSOCK, and registers the live Instance.
func (m *Manager) Start(ctx context.Context, spec LaunchSpec) (*Instance, error) {
	if spec.ID == "" {
		return nil, errors.New("vm: LaunchSpec.ID is required")
	}
	if spec.CellHostPath == "" {
		return nil, errors.New("vm: LaunchSpec.CellHostPath is required")
	}
	if spec.CladefsVMSock == "" {
		return nil, errors.New("vm: LaunchSpec.CladefsVMSock is required")
	}

	m.mu.Lock()
	if existing, ok := m.instances[spec.ID]; ok {
		m.mu.Unlock()
		return existing, nil
	}
	if m.suspended[spec.ID] != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("vm %s: already suspended (call Resume or Stop first): %w", spec.ID, ErrVMRunning)
	}
	if _, busy := m.starting[spec.ID]; busy {
		m.mu.Unlock()
		return nil, fmt.Errorf("vm %s: already starting: %w", spec.ID, ErrVMRunning)
	}
	m.starting[spec.ID] = struct{}{}
	cid := m.allocCIDLocked(spec.ID)
	cfg := m.cfg
	m.mu.Unlock()

	startedOK := false
	defer func() {
		if !startedOK {
			m.mu.Lock()
			delete(m.starting, spec.ID)
			m.mu.Unlock()
		}
	}()

	vmDir := filepath.Join(cfg.RunDir, spec.ID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		m.releaseCID(cid)
		return nil, fmt.Errorf("vm: create dir %s: %w", vmDir, err)
	}
	// Firecracker runs as VMUser and creates/removes vsock sockets in vmDir.
	_ = chownSockForUser(vmDir, cfg.VMUser)
	_ = os.Chmod(vmDir, 0o755)

	apiSock := filepath.Join(vmDir, "fc.sock")
	vsockUDS := filepath.Join(vmDir, "vsock.sock")
	consoleLog := filepath.Join(vmDir, "console.log")
	configPath := filepath.Join(vmDir, "vm.json")
	unitName := "hotbed-vm-" + spec.ID

	_ = os.Remove(apiSock)
	_ = os.Remove(vsockUDS)

	var netAlloc NetAlloc
	if !cfg.DisableNetwork && !spec.DisableNet && m.net != nil {
		var err error
		netAlloc, err = m.net.AllocTAP(ctx, spec.ID)
		if err != nil {
			m.releaseCID(cid)
			return nil, fmt.Errorf("vm %s: alloc network: %w", spec.ID, err)
		}
	}

	var gaBridge *gitAuthBridge
	if cfg.GitAuthHostSock != "" {
		var err error
		gaBridge, err = startGitAuthBridge(vsockUDS, cfg.GitAuthHostSock, cfg.VMUser)
		if err != nil {
			if m.net != nil {
				m.net.ReleaseTAP(ctx, netAlloc)
			}
			m.releaseCID(cid)
			return nil, fmt.Errorf("vm %s: start git-auth bridge: %w", spec.ID, err)
		}
	}

	cleanupOnErr := func(removeDir bool) {
		if gaBridge != nil {
			_ = gaBridge.Close()
		}
		if m.net != nil && netAlloc.TAPName != "" {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			m.net.ReleaseTAP(cctx, netAlloc)
			cancel()
		}
		m.releaseCID(cid)
		if removeDir {
			_ = os.RemoveAll(vmDir)
		}
	}

	if spec.ToolsDiskPath == "" {
		toolsPath, err := m.ensureToolsDisk(cfg.RunDir, cfg.GuestdBin)
		if err != nil {
			cleanupOnErr(true)
			return nil, fmt.Errorf("vm %s: prepare tools disk: %w", spec.ID, err)
		}
		spec.ToolsDiskPath = toolsPath
	}

	fcCfg := m.BuildFirecrackerConfig(spec, cid, vsockUDS, netAlloc)
	cfgBytes, err := json.MarshalIndent(fcCfg, "", "  ")
	if err != nil {
		cleanupOnErr(true)
		return nil, fmt.Errorf("vm %s: encode firecracker config: %w", spec.ID, err)
	}
	if err := os.WriteFile(configPath, cfgBytes, 0o644); err != nil {
		cleanupOnErr(true)
		return nil, fmt.Errorf("vm %s: write %s: %w", spec.ID, configPath, err)
	}

	now := time.Now().UTC()
	meta := vmMeta{
		ID:            spec.ID,
		UnitName:      unitName,
		CellPath:      spec.CellHostPath,
		CladefsVMSock: spec.CladefsVMSock,
		APISock:       apiSock,
		VsockUDS:      vsockUDS,
		ConsoleLog:    consoleLog,
		GuestCID:      cid,
		Net:           netAlloc,
		StartedAt:     now,
		ToolsDiskPath: spec.ToolsDiskPath,
	}
	if err := writeMeta(vmDir, meta); err != nil {
		cleanupOnErr(true)
		return nil, fmt.Errorf("vm %s: persist metadata: %w", spec.ID, err)
	}

	// Ensure Firecracker can write console.log when running as VMUser.
	if f, err := os.OpenFile(consoleLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_ = f.Close()
		_ = chownSockForUser(consoleLog, cfg.VMUser)
	}

	procSpec := ProcessSpec{
		Name:       unitName,
		Slice:      cfg.Slice,
		User:       cfg.VMUser,
		Binary:     cfg.FirecrackerBin,
		Args:       []string{"--enable-pci", "--api-sock", apiSock, "--config-file", configPath},
		ConsoleLog: consoleLog,
	}
	if err := m.launcher.Start(ctx, procSpec); err != nil {
		cleanupOnErr(true)
		return nil, fmt.Errorf("vm %s: launch firecracker: %w", spec.ID, err)
	}

	client := guestd.NewClient(m.dialerFn(vsockUDS))
	if err := m.waitHealthy(ctx, unitName, consoleLog, client); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
		stopErr := m.forceStopUnit(stopCtx, unitName)
		cancel()
		if stopErr != nil {
			// Unit may still be alive: preserve vmDir, CID, and TAP/IP so
			// orphan reconciliation can stop it cleanly without leaking metadata.
			if gaBridge != nil {
				_ = gaBridge.Close()
			}
			slog.Error("vm start failed and unit failed to stop; preserving metadata for orphan reaper",
				"vm", spec.ID, "unit", unitName, "startErr", err, "stopErr", stopErr)
			return nil, fmt.Errorf("vm %s: %w (cleanup stop failed: %v)", spec.ID, err, stopErr)
		}
		cleanupOnErr(true)
		return nil, fmt.Errorf("vm %s: %w", spec.ID, err)
	}

	inst := &Instance{
		ID:            spec.ID,
		UnitName:      unitName,
		Dir:           vmDir,
		CellPath:      spec.CellHostPath,
		CladefsVMSock: spec.CladefsVMSock,
		APISock:       apiSock,
		VsockUDS:      vsockUDS,
		ConsoleLog:    consoleLog,
		GuestCID:      cid,
		Net:           netAlloc,
		Client:        client,
		StartedAt:     now,
		ToolsDiskPath: spec.ToolsDiskPath,
		gitAuth:       gaBridge,
	}
	m.mu.Lock()
	m.instances[spec.ID] = inst
	delete(m.starting, spec.ID)
	startedOK = true
	m.mu.Unlock()
	return inst, nil
}

func (m *Manager) waitHealthy(ctx context.Context, unitName, consoleLog string, client *guestd.Client) error {
	deadline := time.Now().Add(m.cfg.BootTimeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := client.Health(hctx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		// Fail fast if Firecracker already exited (e.g. exit 158/159 or bad config).
		if st, serr := m.launcher.Status(ctx, unitName); serr == nil && st.Loaded && st.Exited {
			tail := readTail(consoleLog, 2048)
			if tail != "" {
				return fmt.Errorf("%s\nconsole tail:\n%s", ExitReason(st.ExitCode), tail)
			}
			return errors.New(ExitReason(st.ExitCode))
		}

		if time.Now().After(deadline) {
			tail := readTail(consoleLog, 2048)
			if tail != "" {
				return fmt.Errorf("timed out waiting for hotbed-guestd after %s (last err: %v)\nconsole tail:\n%s", m.cfg.BootTimeout, lastErr, tail)
			}
			return fmt.Errorf("timed out waiting for hotbed-guestd after %s (last err: %v)", m.cfg.BootTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Reconnect scans cfg.RunDir for Firecracker VMs that survived a hotbed-worker
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
		if id == toolsDirName {
			continue
		}
		vmDir := filepath.Join(m.cfg.RunDir, id)
		meta, err := readMeta(vmDir)
		if err != nil {
			// Only remove a corrupt/incomplete dir if its unit is confirmed dead.
			if st, serr := m.launcher.Status(ctx, "hotbed-vm-"+id); serr == nil && !st.Alive() {
				if m.net != nil {
					m.net.ReleaseTAPByVMID(ctx, id)
				}
				_ = os.RemoveAll(vmDir)
			}
			continue
		}
		st, err := m.launcher.Status(ctx, meta.UnitName)
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
			if m.net != nil {
				if meta.Net.TAPName != "" {
					m.net.ReleaseTAP(ctx, meta.Net)
				} else {
					m.net.ReleaseTAPByVMID(ctx, meta.ID)
				}
			}
			_ = os.RemoveAll(vmDir)
			slog.Info("vm reconnect: reaped leftover build VM", "vm", meta.ID, "unit", meta.UnitName)
			continue
		}
		if meta.Suspended != nil {
			// Suspended VM persisted before worker restart. If the paused
			// Firecracker unit is still alive (worker crashed mid-suspend before
			// forceStopUnit completed), stop it now without touching the cell.
			if err == nil && st.Alive() {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
				stopErr := m.forceStopUnit(stopCtx, meta.UnitName)
				cancel()
				if stopErr != nil {
					slog.Warn("vm reconnect: stop mid-suspend VM failed; preserving dir", "vm", meta.ID, "err", stopErr)
					continue
				}
				if m.net != nil && meta.Net.TAPName != "" {
					rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					m.net.ReleaseTAPKeepIP(rctx, meta.Net)
					rcancel()
				}
			}
			susp := meta.Suspended.clone()
			if susp.Dir == "" {
				susp.Dir = vmDir
			}
			if susp.ID == "" {
				susp.ID = meta.ID
			}
			m.mu.Lock()
			m.suspended[meta.ID] = susp
			if meta.GuestCID >= 3 {
				m.usedCIDs[meta.GuestCID] = meta.ID
				if meta.GuestCID >= m.nextCID {
					m.nextCID = meta.GuestCID + 1
				}
			}
			m.mu.Unlock()
			if m.net != nil && meta.Net.GuestIP != "" {
				m.net.ReserveIP(meta.ID, meta.Net.GuestIP)
			}
			reconnected++
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
			if m.net != nil && meta.Net.TAPName != "" {
				m.net.ReleaseTAP(ctx, meta.Net)
			}
			_ = os.RemoveAll(vmDir)
			continue
		}
		if !st.Running {
			if m.net != nil && meta.Net.TAPName != "" {
				m.net.ReleaseTAP(ctx, meta.Net)
			}
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
			ID:               meta.ID,
			UnitName:         meta.UnitName,
			Dir:              vmDir,
			CellPath:         meta.CellPath,
			CladefsVMSock:    meta.CladefsVMSock,
			APISock:          meta.APISock,
			VsockUDS:         meta.VsockUDS,
			ConsoleLog:       meta.ConsoleLog,
			GuestCID:         meta.GuestCID,
			Net:              meta.Net,
			Client:           client,
			StartedAt:        meta.StartedAt,
			FromMemoryCell:   meta.FromMemoryCell,
			MemoryCellPath:   meta.MemoryCellPath,
			PrevMemCellPaths: slices.Clone(meta.PrevMemCellPaths),
			ToolsDiskPath:    meta.ToolsDiskPath,
			gitAuth:          gaBridge,
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
		if m.net != nil && meta.Net.GuestIP != "" {
			m.net.ReserveIP(meta.ID, meta.Net.GuestIP)
		}
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

// forceStopUnit stops unitName, escalates to Killer.Kill (SIGKILL) if it stays
// alive or Stop errors, and only returns nil once launcher.Status confirms the
// unit is no longer alive.
func (m *Manager) forceStopUnit(ctx context.Context, unitName string) error {
	stopErr := m.launcher.Stop(ctx, unitName)
	st, stErr := m.launcher.Status(ctx, unitName)
	if stErr == nil && !st.Alive() {
		return nil
	}

	var killErr error
	if killer, ok := m.launcher.(Killer); ok {
		killErr = killer.Kill(ctx, unitName)
		deadline := time.Now().Add(m.killWaitTimeout())
	waitDead:
		for time.Now().Before(deadline) {
			st, stErr = m.launcher.Status(ctx, unitName)
			if stErr == nil && !st.Alive() {
				_ = m.launcher.Stop(ctx, unitName)
				return nil
			}
			select {
			case <-ctx.Done():
				break waitDead
			case <-time.After(25 * time.Millisecond):
			}
		}
		st, stErr = m.launcher.Status(ctx, unitName)
		if stErr == nil && !st.Alive() {
			_ = m.launcher.Stop(ctx, unitName)
			return nil
		}
	}

	if stErr != nil {
		return fmt.Errorf("verify unit %s stopped: %w (stop=%v, kill=%v)", unitName, stErr, stopErr, killErr)
	}
	return fmt.Errorf("unit %s is still alive after stop/kill (stop=%v, kill=%v)", unitName, stopErr, killErr)
}

// Stop gracefully shuts down the Firecracker VM for id (syncing Fxfs inside the
// guest if syncFS is true), verifies the Firecracker unit actually exited
// (escalating to SIGKILL on failure/timeout), and ONLY THEN cleans up
// tracking, TAP, CID, and inst.Dir. If the unit cannot be confirmed dead,
// metadata and tracking are preserved for retry/reconciliation and an error is
// returned. When syncFS is true, Stop gives guestd SyncShutdownTimeout to flush
// dirty page cache synchronously and returns an error (after still stopping and
// cleaning up Firecracker) if the guest sync could not complete — preventing
// callers from snapshotting a half-flushed cell to R2. Idempotent if not tracked.
func (m *Manager) Stop(ctx context.Context, id string, syncFS bool) error {
	lock := m.vmLock(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	inst, ok := m.instances[id]
	susp := m.suspended[id]
	m.mu.Unlock()
	if !ok {
		if susp == nil {
			return nil
		}
		// Discarding a suspended VM requires no Firecracker resume or guest
		// sync: release its reserved IP/CID and remove its snapshot dir.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
		defer cancel()
		if m.net != nil {
			if susp.Net.TAPName != "" {
				m.net.ReleaseTAP(stopCtx, susp.Net)
			} else {
				m.net.ReleaseTAPByVMID(stopCtx, id)
			}
		}
		m.releaseCID(susp.GuestCID)
		_ = os.RemoveAll(susp.Dir)
		m.mu.Lock()
		if cur := m.suspended[id]; cur == susp {
			delete(m.suspended, id)
		}
		m.mu.Unlock()
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
		// Wait briefly for Firecracker to exit cleanly on its own after PowerOff.
		deadline := time.Now().Add(m.cfg.ShutdownTimeout)
	waitLoop:
		for time.Now().Before(deadline) {
			st, err := m.launcher.Status(ctx, inst.UnitName)
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

	if err := m.forceStopUnit(stopCtx, inst.UnitName); err != nil {
		slog.Error("vm stop failed; preserving instance tracking and metadata",
			"vm", id, "unit", inst.UnitName, "dir", inst.Dir, "err", err)
		return fmt.Errorf("vm %s: stop failed: %w", id, err)
	}

	m.mu.Lock()
	if cur := m.instances[id]; cur == inst {
		delete(m.instances, id)
	}
	m.mu.Unlock()

	if inst.gitAuth != nil {
		_ = inst.gitAuth.Close()
	}
	if m.net != nil {
		if inst.Net.TAPName != "" {
			m.net.ReleaseTAP(stopCtx, inst.Net)
		} else {
			m.net.ReleaseTAPByVMID(stopCtx, id)
		}
	}
	m.releaseCID(inst.GuestCID)
	_ = os.RemoveAll(inst.Dir)
	if syncErr != nil {
		return fmt.Errorf("vm %s: %w", id, syncErr)
	}
	return nil
}

// EnsureNotRunning verifies no live Firecracker unit or in-flight launch holds
// id. If a tracked instance already exited, its stale state is cleaned up.
func (m *Manager) EnsureNotRunning(ctx context.Context, id string) error {
	m.mu.Lock()
	_, starting := m.starting[id]
	tracked := m.instances[id] != nil || m.suspended[id] != nil
	m.mu.Unlock()
	if starting {
		return fmt.Errorf("vm %s is currently starting: %w", id, ErrVMRunning)
	}
	unitName := "hotbed-vm-" + id
	if st, err := m.launcher.Status(ctx, unitName); err == nil && st.Alive() {
		return fmt.Errorf("vm %s (%s): %w", id, unitName, ErrVMRunning)
	}
	if tracked {
		return m.Stop(ctx, id, false)
	}
	return nil
}

// ReconcileOrphans discovers every active hotbed-vm-* unit in systemd, every
// tracked instance, and every RunDir entry not in ownedIDs (or currently
// starting), force-stops it, releases its TAP (even when RunDir/meta.json was
// already deleted), removes its RunDir, and sweeps orphaned TAPs on the bridge.
// Returns the reaped VM IDs and any recovered backing cell paths.
func (m *Manager) ReconcileOrphans(ctx context.Context, ownedIDs map[string]bool) (reapedIDs []string, reapedCellPaths []string) {
	cfg := m.configSnapshot()
	candidates := map[string]struct{}{}

	if units, err := m.launcher.List(ctx, cfg.Slice); err != nil {
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
	for id := range m.suspended {
		candidates[id] = struct{}{}
	}
	m.mu.Unlock()

	if entries, err := os.ReadDir(cfg.RunDir); err == nil {
		for _, ent := range entries {
			if ent.IsDir() && ent.Name() != "" && ent.Name() != toolsDirName {
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
		susp := m.suspended[id]
		m.mu.Unlock()

		vmDir := filepath.Join(cfg.RunDir, id)
		meta, metaErr := readMeta(vmDir)
		cellPath := ""
		if inst != nil && inst.CellPath != "" {
			cellPath = inst.CellPath
		} else if susp != nil && susp.CellPath != "" {
			cellPath = susp.CellPath
		} else if metaErr == nil && meta.CellPath != "" {
			cellPath = meta.CellPath
		}

		if inst != nil || susp != nil {
			if err := m.Stop(ctx, id, false); err != nil {
				slog.Warn("reap tracked orphan Firecracker VM failed", "vm", id, "err", err)
				stillAlive[id] = true
				continue
			}
		} else {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
			err := m.forceStopUnit(stopCtx, "hotbed-vm-"+id)
			if err != nil {
				cancel()
				slog.Warn("reap untracked orphan Firecracker VM failed", "vm", id, "unit", "hotbed-vm-"+id, "err", err)
				stillAlive[id] = true
				continue
			}
			if m.net != nil {
				if metaErr == nil && meta.Net.TAPName != "" {
					m.net.ReleaseTAP(stopCtx, meta.Net)
				}
				m.net.ReleaseTAPByVMID(stopCtx, id)
			}
			cancel()
			_ = os.RemoveAll(vmDir)
		}

		reapedIDs = append(reapedIDs, id)
		if cellPath != "" {
			reapedCellPaths = append(reapedCellPaths, cellPath)
		}
		slog.Info("reaped orphan Firecracker VM", "vm", id, "unit", "hotbed-vm-"+id)
	}

	if m.net != nil {
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
		for id := range m.suspended {
			liveIDs[id] = true
		}
		for id := range m.starting {
			liveIDs[id] = true
		}
		m.mu.Unlock()
		m.net.SweepOrphanTAPs(ctx, liveIDs)
	}
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

// Status reports the underlying Firecracker VMM process status for id.
func (m *Manager) Status(ctx context.Context, id string) (ProcessStatus, error) {
	m.mu.Lock()
	inst, ok := m.instances[id]
	m.mu.Unlock()
	if !ok {
		return ProcessStatus{}, nil
	}
	return m.launcher.Status(ctx, inst.UnitName)
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

func (m *Manager) vmLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vmLocks == nil {
		m.vmLocks = map[string]*sync.Mutex{}
	}
	l, ok := m.vmLocks[id]
	if !ok {
		l = &sync.Mutex{}
		m.vmLocks[id] = l
	}
	return l
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
