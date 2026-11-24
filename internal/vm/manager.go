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
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/guestd"
)

// Instance represents one live Firecracker microVM backing a workspace.
type Instance struct {
	ID         string
	UnitName   string
	Dir        string
	CellPath   string
	VsockUDS   string
	ConsoleLog string
	GuestCID   uint32
	Net        NetAlloc
	Client     *guestd.Client
	StartedAt  time.Time

	gitAuth io.Closer
}

// vmMeta is persisted to <RunDir>/<id>/meta.json so hotbed-worker can reconnect
// to surviving Firecracker VMs across worker restarts without disrupting them.
type vmMeta struct {
	ID         string    `json:"id"`
	UnitName   string    `json:"unitName"`
	CellPath   string    `json:"cellPath"`
	VsockUDS   string    `json:"vsockUDS"`
	ConsoleLog string    `json:"consoleLog"`
	GuestCID   uint32    `json:"guestCID"`
	Net        NetAlloc  `json:"net"`
	StartedAt  time.Time `json:"startedAt"`
}

// Manager launches, tracks, reconnects, and tears down Firecracker microVMs.
// It implements runtime.GuestResolver via ClientFor.
type Manager struct {
	cfg      Config
	launcher Launcher
	net      *NetManager
	dialerFn func(vsockUDS string) guestd.DialFunc

	mu        sync.Mutex
	instances map[string]*Instance
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
		cfg:      cfg,
		launcher: SystemdLauncher{},
		net:      nm,
		dialerFn: func(uds string) guestd.DialFunc {
			return guestd.FirecrackerVsockDialer(uds)
		},
		instances: map[string]*Instance{},
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
	Drives        []any            `json:"drives"`
	MachineConfig fcMachineConfig  `json:"machine-config"`
	Vsock         fcVsock          `json:"vsock"`
	NetInterfaces []fcNetInterface `json:"network-interfaces,omitempty"`
	Balloon       *fcBalloon       `json:"balloon,omitempty"`
	Cladefs       []fcCladefs      `json:"cladefs"`
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
	bootParts := []string{
		"root=rootfs",
		"rootfstype=cladefs",
		"rw",
		"console=ttyS0",
		"reboot=k",
		"panic=1",
		"init=/sbin/hotbed-guestd",
	}
	if ipArg := netAlloc.KernelIPArg(); ipArg != "" {
		bootParts = append(bootParts, ipArg)
	}
	if extra := strings.TrimSpace(spec.ExtraBootArgs); extra != "" {
		bootParts = append(bootParts, extra)
	}

	fc := FirecrackerConfig{
		BootSource: fcBootSource{
			KernelImagePath: cfg.KernelPath,
			BootArgs:        strings.Join(bootParts, " "),
		},
		Drives: []any{},
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
	cid := m.allocCIDLocked(spec.ID)
	cfg := m.cfg
	m.mu.Unlock()

	vmDir := filepath.Join(cfg.RunDir, spec.ID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		m.releaseCID(cid)
		return nil, fmt.Errorf("vm: create dir %s: %w", vmDir, err)
	}
	// Firecracker runs as VMUser and creates/removes vsock sockets in vmDir.
	_ = chownSockForUser(vmDir, cfg.VMUser)
	_ = os.Chmod(vmDir, 0o755)

	vsockUDS := filepath.Join(vmDir, "vsock.sock")
	consoleLog := filepath.Join(vmDir, "console.log")
	configPath := filepath.Join(vmDir, "vm.json")
	unitName := "hotbed-vm-" + spec.ID

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

	cleanupOnErr := func() {
		if gaBridge != nil {
			_ = gaBridge.Close()
		}
		if m.net != nil && netAlloc.TAPName != "" {
			m.net.ReleaseTAP(context.Background(), netAlloc)
		}
		m.releaseCID(cid)
	}

	fcCfg := m.BuildFirecrackerConfig(spec, cid, vsockUDS, netAlloc)
	cfgBytes, err := json.MarshalIndent(fcCfg, "", "  ")
	if err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("vm %s: encode firecracker config: %w", spec.ID, err)
	}
	if err := os.WriteFile(configPath, cfgBytes, 0o644); err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("vm %s: write %s: %w", spec.ID, configPath, err)
	}

	now := time.Now().UTC()
	meta := vmMeta{
		ID:         spec.ID,
		UnitName:   unitName,
		CellPath:   spec.CellHostPath,
		VsockUDS:   vsockUDS,
		ConsoleLog: consoleLog,
		GuestCID:   cid,
		Net:        netAlloc,
		StartedAt:  now,
	}
	if err := writeMeta(vmDir, meta); err != nil {
		cleanupOnErr()
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
		Args:       []string{"--enable-pci", "--no-api", "--config-file", configPath},
		ConsoleLog: consoleLog,
	}
	if err := m.launcher.Start(ctx, procSpec); err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("vm %s: launch firecracker: %w", spec.ID, err)
	}

	client := guestd.NewClient(m.dialerFn(vsockUDS))
	if err := m.waitHealthy(ctx, unitName, consoleLog, client); err != nil {
		_ = m.launcher.Stop(context.Background(), unitName)
		cleanupOnErr()
		return nil, fmt.Errorf("vm %s: %w", spec.ID, err)
	}

	inst := &Instance{
		ID:         spec.ID,
		UnitName:   unitName,
		Dir:        vmDir,
		CellPath:   spec.CellHostPath,
		VsockUDS:   vsockUDS,
		ConsoleLog: consoleLog,
		GuestCID:   cid,
		Net:        netAlloc,
		Client:     client,
		StartedAt:  now,
		gitAuth:    gaBridge,
	}
	m.mu.Lock()
	m.instances[spec.ID] = inst
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
// restart, re-attaches their git-auth vsock bridges and guestd clients, and
// cleans up stale directories whose VMs have already exited.
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
		vmDir := filepath.Join(m.cfg.RunDir, ent.Name())
		meta, err := readMeta(vmDir)
		if err != nil {
			continue
		}
		st, err := m.launcher.Status(ctx, meta.UnitName)
		if err != nil || !st.Running {
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
			ID:         meta.ID,
			UnitName:   meta.UnitName,
			Dir:        vmDir,
			CellPath:   meta.CellPath,
			VsockUDS:   meta.VsockUDS,
			ConsoleLog: meta.ConsoleLog,
			GuestCID:   meta.GuestCID,
			Net:        meta.Net,
			Client:     client,
			StartedAt:  meta.StartedAt,
			gitAuth:    gaBridge,
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

// Stop gracefully shuts down the Firecracker VM for id (syncing Fxfs inside the
// guest if syncFS is true), waits for clean Firecracker exit, stops the unit,
// and cleans up TAP/bridge/socket/state files. Idempotent if not running.
func (m *Manager) Stop(ctx context.Context, id string, syncFS bool) error {
	m.mu.Lock()
	inst, ok := m.instances[id]
	if ok {
		delete(m.instances, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}

	if inst.Client != nil {
		sctx, cancel := context.WithTimeout(ctx, m.cfg.ShutdownTimeout)
		_ = inst.Client.Shutdown(sctx, guestd.ShutdownRequest{
			SyncFS:       syncFS,
			StopCommands: true,
			PowerOff:     true,
		})
		cancel()

		// Wait briefly for Firecracker to exit cleanly on its own.
		deadline := time.Now().Add(m.cfg.ShutdownTimeout)
		for time.Now().Before(deadline) {
			st, err := m.launcher.Status(ctx, inst.UnitName)
			if err != nil || !st.Running {
				break
			}
			select {
			case <-ctx.Done():
				break
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	stopErr := m.launcher.Stop(ctx, inst.UnitName)
	if inst.gitAuth != nil {
		_ = inst.gitAuth.Close()
	}
	if m.net != nil && inst.Net.TAPName != "" {
		m.net.ReleaseTAP(ctx, inst.Net)
	}
	m.releaseCID(inst.GuestCID)
	_ = os.RemoveAll(inst.Dir)
	return stopErr
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
