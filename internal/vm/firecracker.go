package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// firecrackerVMM runs each VM as a Firecracker process its Launcher starts
// (a transient systemd unit in production), on a TAP of its NetManager's
// bridge, booting hotbed-guestd from a read-only tools disk.
type firecrackerVMM struct {
	launcher    Launcher
	net         *NetManager // nil without networking
	toolsDiskFn func(cacheDir, guestdBin string) (string, error)
	// fsRoot is where PinVCPUs finds /sys and /proc ("" for /), and
	// setAffinity how it pins a thread to a CPU (nil for the kernel's):
	// tests' own.
	fsRoot      string
	setAffinity func(tid, cpu int) error

	toolsMu        sync.Mutex
	toolsCacheBin  string
	toolsCacheSize int64
	toolsCacheMod  int64
	toolsCachePath string
}

func (f *firecrackerVMM) Boot(ctx context.Context, m Machine, spec LaunchSpec, cfg Config) (Booted, error) {
	if spec.CladefsVMSock == "" {
		return Booted{}, fmt.Errorf("vm %s: LaunchSpec.CladefsVMSock is required", m.ID)
	}
	// Firecracker runs as VMUser and creates/removes vsock sockets in m.Dir.
	_ = chownSockForUser(m.Dir, cfg.VMUser)
	_ = os.Chmod(m.Dir, 0o755)
	b := Booted{VsockUDS: filepath.Join(m.Dir, "vsock.sock"), ToolsDiskPath: spec.ToolsDiskPath, FirecrackerLog: filepath.Join(m.Dir, "firecracker.log")}
	_ = os.Remove(b.VsockUDS)

	if !cfg.DisableNetwork && !spec.DisableNet && f.net != nil {
		var err error
		b.Net, err = f.net.AllocTAP(ctx, m.ID)
		if err != nil {
			return Booted{}, fmt.Errorf("vm %s: alloc network: %w", m.ID, err)
		}
	}
	freeNet := func() {
		if f.net != nil && b.Net.TAPName != "" {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			f.net.ReleaseTAP(cctx, b.Net)
			cancel()
		}
	}
	if b.ToolsDiskPath == "" {
		toolsPath, err := f.ensureToolsDisk(cfg.RunDir, cfg.GuestdBin)
		if err != nil {
			freeNet()
			return Booted{}, fmt.Errorf("vm %s: prepare tools disk: %w", m.ID, err)
		}
		b.ToolsDiskPath = toolsPath
	}
	spec.ToolsDiskPath = b.ToolsDiskPath

	fcCfg := firecrackerConfig(cfg, spec, m.CID, b.VsockUDS, b.Net)
	b.VCPUs, b.MemMiB = fcCfg.MachineConfig.VCPUCount, fcCfg.MachineConfig.MemSizeMiB
	cfgBytes, err := json.MarshalIndent(fcCfg, "", "  ")
	if err != nil {
		freeNet()
		return Booted{}, fmt.Errorf("vm %s: encode firecracker config: %w", m.ID, err)
	}
	configPath := filepath.Join(m.Dir, "vm.json")
	if err := os.WriteFile(configPath, cfgBytes, 0o644); err != nil {
		freeNet()
		return Booted{}, fmt.Errorf("vm %s: write %s: %w", m.ID, configPath, err)
	}
	// Ensure Firecracker can write console.log and firecracker.log when running as VMUser.
	for _, logPath := range []string{m.ConsoleLog, b.FirecrackerLog} {
		if log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			_ = log.Close()
			_ = chownSockForUser(logPath, cfg.VMUser)
		}
	}
	slice, props := cfg.Slice, []string(nil)
	if spec.BenchCPUs != "" {
		// A measurement's: in its slice, never swapping (Manager.benchPin).
		slice, props = cfg.BenchSlice, []string{"MemorySwapMax=0"}
	}
	if err := f.launcher.Start(ctx, ProcessSpec{
		Name:       m.Unit,
		Slice:      slice,
		Properties: props,
		User:       cfg.VMUser,
		Binary:     cfg.FirecrackerBin,
		Args: []string{
			"--id", m.ID,
			"--enable-pci",
			"--no-api",
			"--config-file", configPath,
			"--log-path", b.FirecrackerLog,
			"--level", "Info",
			"--show-level",
			"--show-log-origin",
		},
		ConsoleLog: m.ConsoleLog,
	}); err != nil {
		freeNet()
		return Booted{}, fmt.Errorf("vm %s: launch firecracker: %w", m.ID, err)
	}
	return b, nil
}

func (f *firecrackerVMM) Stop(ctx context.Context, unit string) error {
	return f.launcher.Stop(ctx, unit)
}

func (f *firecrackerVMM) Kill(ctx context.Context, unit string) error {
	if k, ok := f.launcher.(Killer); ok {
		return k.Kill(ctx, unit)
	}
	return ErrNoKill
}

func (f *firecrackerVMM) Status(ctx context.Context, unit string) (ProcessStatus, error) {
	return f.launcher.Status(ctx, unit)
}

func (f *firecrackerVMM) List(ctx context.Context, slice string) ([]string, error) {
	return f.launcher.List(ctx, slice)
}

func (f *firecrackerVMM) Adopt(m Machine, b Booted) {
	if f.net != nil && b.Net.GuestIP != "" {
		f.net.ReserveIP(m.ID, b.Net.GuestIP)
	}
}

func (f *firecrackerVMM) Free(ctx context.Context, m Machine, b Booted) {
	if f.net == nil {
		return
	}
	if b.Net.TAPName != "" {
		f.net.ReleaseTAP(ctx, b.Net)
	} else {
		f.net.ReleaseTAPByVMID(ctx, m.ID)
	}
}

func (f *firecrackerVMM) Sweep(ctx context.Context, live map[string]bool) {
	if f.net != nil {
		f.net.SweepOrphanTAPs(ctx, live)
	}
}

// FirecrackerConfig matches Firecracker's --config-file JSON schema (including
// cladefs's virtio-extent "cladefs" device array).
type FirecrackerConfig struct {
	BootSource    fcBootSource     `json:"boot-source"`
	Drives        []fcDrive        `json:"drives"`
	Pmem          []fcPmem         `json:"pmem,omitempty"`
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

type fcPmem struct {
	ID         string `json:"id"`
	PathOnHost string `json:"path_on_host"`
	RootDevice bool   `json:"root_device"`
	ReadOnly   bool   `json:"read_only"`
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

// BuildFirecrackerConfig constructs the Firecracker JSON config for spec as
// the manager's config stands.
func (m *Manager) BuildFirecrackerConfig(spec LaunchSpec, cid uint32, vsockUDS string, netAlloc NetAlloc) FirecrackerConfig {
	return firecrackerConfig(m.configSnapshot(), spec, cid, vsockUDS, netAlloc)
}

// firecrackerConfig is the Firecracker JSON config for spec under cfg.
func firecrackerConfig(cfg Config, spec LaunchSpec, cid uint32, vsockUDS string, netAlloc NetAlloc) FirecrackerConfig {
	vcpus, memMiB := cfg.size(spec)
	diskBytes := spec.DiskBytes
	if diskBytes == 0 {
		diskBytes = cfg.DefaultDiskBytes
	}
	// Boot from the read-only host tools disk mapped via virtio-pmem
	// (/dev/pmem0, appended by Firecracker because root_device=true &
	// read_only=true), then hotbed-guestd PID 1 mounts cladefs:rootfs and
	// pivot_roots onto it. Using virtio-pmem shares the host page cache
	// across VMs with zero block-queue I/O and avoids arm64 PCI virtio-blk
	// boot hangs. Start always sets spec.ToolsDiskPath before building this.
	bootParts := []string{
		"console=ttyS0",
		"reboot=k",
		"panic=1",
		"psi=1",
		"init=/hotbed-guestd",
		"HOTBED_ROOT=cladefs:rootfs",
	}
	drives := []fcDrive{}
	pmems := []fcPmem{{
		ID:         "hotbed-tools",
		PathOnHost: spec.ToolsDiskPath,
		RootDevice: true,
		ReadOnly:   true,
	}}
	if ipArg := netAlloc.KernelIPArg(); ipArg != "" {
		bootParts = append(bootParts, ipArg)
	}
	if cfg.Nofile > 0 {
		bootParts = append(bootParts, fmt.Sprintf("HOTBED_NOFILE=%d", cfg.Nofile))
	}
	bootParts = append(bootParts, fmt.Sprintf("HOTBED_NOW=%d", time.Now().Unix()))
	if extra := strings.TrimSpace(spec.ExtraBootArgs); extra != "" {
		bootParts = append(bootParts, extra)
	}

	fc := FirecrackerConfig{
		BootSource: fcBootSource{
			KernelImagePath: cfg.KernelPath,
			BootArgs:        strings.Join(bootParts, " "),
		},
		Drives: drives,
		Pmem:   pmems,
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
func (m *firecrackerVMM) ensureToolsDisk(runDir, guestdBin string) (string, error) {
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
