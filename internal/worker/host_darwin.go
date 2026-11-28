package worker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
)

// prepareHost checks a Mac can be the worker it is configured as: every
// command runs in a VM of machined's, which macOS gives its memory and CPU
// as it does any process, so there is no slice to make or bound.
func prepareHost(_ context.Context, h hostPrep) error {
	if h.ecfg.VM == nil {
		return api.Errorf(api.CodePlatformNotSupported, "a macOS worker runs every command in a VM of machined's: enable vm with machinedSock in hotbed.json")
	}
	if h.cfg.Bench != nil {
		return api.Errorf(api.CodePlatformNotSupported, "measurements isolate a Linux machine's CPUs, IRQs and disks: a macOS worker takes none").
			WithSuggestion("remove bench from this worker's hotbed.json")
	}
	return nil
}

// hostUnits: a macOS worker runs no command on the host itself, so it has
// no units (its unitExecutor answers errNoHostUnits): every command runs in
// a VM of machined's, through its guestExecutor.
func hostUnits() Units { return nil }

// hostSystem measures the Mac through sysctl(3).
func hostSystem(string) System { return darwinSystem{} }

// darwinSystem measures a Mac. macOS has no pressure stall information or
// OOM kill count (errors.ErrUnsupported); CPU load comes from sysctl
// vm.loadavg divided by logical CPUs (clamped to [0, 1]) so placement sees
// host and VM load without requiring cgo/Mach traps.
type darwinSystem struct{}

func (darwinSystem) Resources() (*pb.Resources, error) {
	mem, err := darwinMemory()
	if err != nil {
		return nil, err
	}
	release, err := unix.Sysctl("kern.osrelease")
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.osrelease: %w", err)
	}
	machine, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return nil, fmt.Errorf("sysctl machdep.cpu.brand_string: %w", err)
	}
	return &pb.Resources{
		Os:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Cpus:        int32(runtime.NumCPU()),
		MemoryBytes: mem.Total,
		SwapBytes:   mem.SwapTotal,
		Kernel:      "Darwin " + release,
		Machine:     strings.TrimPrefix(machine, "Apple "),
	}, nil
}

func (darwinSystem) Meminfo() (sysinfo.Mem, error)    { return darwinMemory() }
func (darwinSystem) Pressure(string) (float64, error) { return 0, errors.ErrUnsupported }
func (darwinSystem) CPU() (sysinfo.CPUShare, error)   { return darwinCPU() }
func (darwinSystem) OOMKills() (int64, error)         { return 0, errors.ErrUnsupported }

// darwinCPU estimates host CPU utilization from the 1-minute load average
// (sysctl vm.loadavg) divided by logical CPU count, clamped to [0, 1].
func darwinCPU() (sysinfo.CPUShare, error) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return sysinfo.CPUShare{}, fmt.Errorf("sysctl vm.loadavg: %w", err)
	}
	busy, err := parseDarwinLoadavg(raw, runtime.NumCPU())
	if err != nil {
		return sysinfo.CPUShare{}, err
	}
	return sysinfo.CPUShare{Busy: busy}, nil
}

// parseDarwinLoadavg parses macOS struct loadavg ({ uint32 ldavg[3]; uint32 pad;
// int64 fscale; } on 64-bit darwin) and returns ldavg[0] / fscale / cpus,
// clamped to [0, 1].
func parseDarwinLoadavg(raw []byte, cpus int) (float64, error) {
	if len(raw) < 24 {
		return 0, fmt.Errorf("sysctl vm.loadavg: short payload (%d bytes)", len(raw))
	}
	ld0 := binary.LittleEndian.Uint32(raw[0:4])
	fscale := int64(binary.LittleEndian.Uint64(raw[16:24]))
	if fscale <= 0 {
		return 0, fmt.Errorf("sysctl vm.loadavg: invalid fscale %d", fscale)
	}
	if cpus <= 0 {
		cpus = 1
	}
	busy := (float64(ld0) / float64(fscale)) / float64(cpus)
	if busy < 0 {
		busy = 0
	} else if busy > 1 {
		busy = 1
	}
	return busy, nil
}

// darwinMemory is the Mac's memory as Activity Monitor counts it: used is
// apps' memory (anonymous pages, less purgeable ones), wired memory, and
// what the compressor holds; the rest is available.
func darwinMemory() (sysinfo.Mem, error) {
	var m sysinfo.Mem
	read := func(name string) (uint64, error) {
		if v, err := unix.SysctlUint64(name); err == nil {
			return v, nil
		}
		v, err := unix.SysctlUint32(name)
		if err != nil {
			return 0, fmt.Errorf("sysctl %s: %w", name, err)
		}
		return uint64(v), nil
	}
	var total, page, internal, purgeable, wired, compressed uint64
	for _, f := range []struct {
		name string
		dst  *uint64
	}{
		{"hw.memsize", &total},
		{"vm.pagesize", &page},
		{"vm.page_pageable_internal_count", &internal},
		{"vm.page_purgeable_count", &purgeable},
		{"vm.page_wired_count", &wired},
		{"vm.compressor_bytes_used", &compressed},
	} {
		v, err := read(f.name)
		if err != nil {
			return m, err
		}
		*f.dst = v
	}
	used := (internal-min(purgeable, internal)+wired)*page + compressed
	m.Total = int64(total)
	m.Available = int64(total - min(used, total))
	// struct xsw_usage: total, avail, used (uint64s), page size, encrypted.
	if raw, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(raw) >= 16 {
		m.SwapTotal = int64(binary.LittleEndian.Uint64(raw[0:8]))
		m.SwapFree = int64(binary.LittleEndian.Uint64(raw[8:16]))
	} else if err != nil {
		return m, fmt.Errorf("sysctl vm.swapusage: %w", err)
	}
	return m, nil
}
