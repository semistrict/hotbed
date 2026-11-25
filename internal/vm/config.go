// Package vm manages one Firecracker microVM per hotbed workspace, backed by
// cladefsd Fxfs cells and controlled over AF_VSOCK via hotbed-guestd.
package vm

import (
	"fmt"
	"runtime"
	"time"
)

const (
	DefaultFirecrackerBin     = "/usr/local/bin/firecracker"
	DefaultKernelPath         = "/var/lib/cladefs/vmlinux"
	DefaultGuestdBin          = "/usr/local/bin/hotbed-guestd"
	DefaultSlice              = "hotbed.slice"
	DefaultVMUser             = "cladefsd"
	DefaultBridgeName         = "hb-vm0"
	DefaultBridgeCIDR         = "10.199.0.1/16"
	DefaultDiskBytes          = 32 << 30
	// 16 GiB virtual DAX window (PROT_NONE | MAP_NORESERVE on host, lazy per-128 MiB
	// section memremap_pages in guest): cladefs splits the window 50/50 between
	// dax->pool and dax->region_pool, so 16 GiB gives dax->pool 8 GiB (larger than
	// cladefsd's 6 GiB extent cache) with zero extra host RAM.
	DefaultDAXWindowBytes     = 16 << 30
	DefaultDAXPrivateMaxBytes = 512 << 20
	DefaultCacheType          = "Unsafe"
	DefaultMemMiB             = 4096
	DefaultNofile             = 65536
	DefaultBootTimeout        = 30 * time.Second
	DefaultShutdownTimeout    = 5 * time.Second

	// Firecracker exit codes defined by cladefs's Firecracker build.
	ExitCodeCladefsdLost = 158
	ExitCodeDAXFault     = 159
)

// Config configures the host Firecracker VM manager.
type Config struct {
	FirecrackerBin     string        // path to cladefs-enabled firecracker binary
	KernelPath         string        // path to cladefs guest vmlinux kernel
	GuestdBin          string        // path to static Linux hotbed-guestd binary (for base image builds)
	RunDir             string        // directory for per-VM state (vm.json, meta.json, vsock UDS, console.log)
	Slice              string        // systemd slice for VM units (default hotbed.slice)
	VMUser             string        // non-root host user running Firecracker (default cladefsd)
	DefaultVCPUs       int           // vCPUs per workspace VM
	DefaultMemMiB      int           // guest RAM in MiB per workspace VM
	DefaultDiskBytes   uint64        // Fxfs rootfs capacity in bytes
	DAXWindowBytes     uint64        // virtio-extent DAX window size in bytes
	DAXPrivateMaxBytes uint64        // per-VM private DAX page budget in bytes
	DisableDAX         bool          // omit DAX window (required on 16 KiB host pages until upstream DAX support)
	CacheType          string        // virtio-extent cache_type: "Unsafe" (default, fsync returns once ordered) or "Writeback"
	BalloonTargetMiB   int           // optional initial balloon target (0 = deflate on OOM only)
	BridgeName         string        // host bridge for VM TAPs (default hb-vm0)
	BridgeCIDR         string        // gateway IP/mask on BridgeName (default 10.199.0.1/16)
	DNSServers         []string      // nameservers written to guest /etc/resolv.conf
	Nofile             uint64        // RLIMIT_NOFILE soft/hard limit inside guest VMs (0 = guestd default 65536)
	GitAuthHostSock    string        // host path to hotbed gitAuthServer Unix socket (bridged via vsock port 5002)
	BootTimeout        time.Duration // max wait for hotbed-guestd /v1/health after VM launch
	ShutdownTimeout    time.Duration // max wait for clean Firecracker exit after /v1/shutdown
	DisableNetwork     bool          // skip TAP/bridge setup (for isolated tests)
}

// Normalized returns a copy of c with defaults applied.
func (c Config) Normalized() Config {
	if c.FirecrackerBin == "" {
		c.FirecrackerBin = DefaultFirecrackerBin
	}
	if c.KernelPath == "" {
		c.KernelPath = DefaultKernelPath
	}
	if c.GuestdBin == "" {
		c.GuestdBin = DefaultGuestdBin
	}
	if c.Slice == "" {
		c.Slice = DefaultSlice
	}
	if c.VMUser == "" {
		c.VMUser = DefaultVMUser
	}
	if c.DefaultVCPUs <= 0 {
		c.DefaultVCPUs = min(max(runtime.NumCPU(), 2), 8)
	}
	if c.DefaultMemMiB <= 0 {
		c.DefaultMemMiB = DefaultMemMiB
	}
	if c.DefaultDiskBytes == 0 {
		c.DefaultDiskBytes = DefaultDiskBytes
	}
	if c.DisableDAX {
		c.DAXWindowBytes = 0
		c.DAXPrivateMaxBytes = 0
	} else {
		if c.DAXWindowBytes == 0 {
			c.DAXWindowBytes = DefaultDAXWindowBytes
		}
		if c.DAXPrivateMaxBytes == 0 {
			c.DAXPrivateMaxBytes = DefaultDAXPrivateMaxBytes
		}
	}
	if c.CacheType == "" {
		c.CacheType = DefaultCacheType
	}
	if c.BridgeName == "" {
		c.BridgeName = DefaultBridgeName
	}
	if c.BridgeCIDR == "" {
		c.BridgeCIDR = DefaultBridgeCIDR
	}
	if len(c.DNSServers) == 0 {
		c.DNSServers = []string{"1.1.1.1", "8.8.8.8"}
	}
	if c.BootTimeout <= 0 {
		c.BootTimeout = DefaultBootTimeout
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	return c
}

// LaunchSpec describes one Firecracker microVM to boot for a workspace or base build.
type LaunchSpec struct {
	ID            string // workspace or build ID (e.g. ws-3f2a01)
	CellHostPath  string // host path to the cladefsd cell directory
	CladefsVMSock string // host path to cladefsd VM socket (tenant or default)
	VCPUs         int    // 0 -> Config.DefaultVCPUs
	MemMiB        int    // 0 -> Config.DefaultMemMiB
	DiskBytes     uint64 // 0 -> Config.DefaultDiskBytes
	ExtraBootArgs string // optional extra kernel cmdline tokens
	DisableNet    bool   // skip TAP allocation for this VM
}

// ExitReason returns a human-readable explanation for a Firecracker exit code.
func ExitReason(code int) string {
	switch code {
	case 0:
		return "clean shutdown"
	case ExitCodeCladefsdLost:
		return fmt.Sprintf("firecracker exited %d: lost connection to cladefsd (or VM process was frozen past quarantine deadline)", code)
	case ExitCodeDAXFault:
		return fmt.Sprintf("firecracker exited %d: cladefs DAX fault", code)
	default:
		return fmt.Sprintf("firecracker exited with code %d", code)
	}
}
