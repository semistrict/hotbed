package worker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
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

// hostUnits: a macOS worker runs no command on the host itself.
func hostUnits() Units { return vmOnlyUnits{} }

// hostSystem measures the Mac through sysctl(3).
func hostSystem(string) System { return darwinSystem{} }

// warmsBaseImage: a macOS worker has no chroot workspaces to keep a shared
// base rootfs for.
const warmsBaseImage = false

// errHostCommand is what a macOS worker answers a command it would run on
// the host: every workspace there is a VM's.
var errHostCommand = api.Errorf(api.CodePlatformNotSupported, "a macOS worker runs commands only in VMs, never on the host")

// vmOnlyUnits is a macOS worker's Units: it runs nothing on the host, so it
// has no units to list, wait for or stop.
type vmOnlyUnits struct{}

func (vmOnlyUnits) Start(context.Context, unit.Spec) error { return errHostCommand }
func (vmOnlyUnits) Status(context.Context, string) (unit.State, error) {
	return unit.State{}, nil
}
func (vmOnlyUnits) StatusMany(_ context.Context, names []string) (map[string]unit.State, error) {
	out := make(map[string]unit.State, len(names))
	for _, n := range names {
		out[n] = unit.State{}
	}
	return out, nil
}
func (vmOnlyUnits) Freeze(context.Context, string) error           { return errHostCommand }
func (vmOnlyUnits) Thaw(context.Context, string) error             { return errHostCommand }
func (vmOnlyUnits) Reclaim(string, int64) error                    { return errHostCommand }
func (vmOnlyUnits) Memory(string) (unit.MemoryUse, error)          { return unit.MemoryUse{}, errHostCommand }
func (vmOnlyUnits) Stop(context.Context, string) error             { return nil }
func (vmOnlyUnits) List(context.Context, string) ([]string, error) { return nil, nil }
func (vmOnlyUnits) Wait(context.Context, string, string) error     { return nil }
func (vmOnlyUnits) StartShell(context.Context, unit.Spec, unit.ShellIO) (unit.ShellHandle, error) {
	return nil, errHostCommand
}
func (vmOnlyUnits) Exec(context.Context, string, string, unit.ExecSpec) (*exec.Cmd, error) {
	return nil, errHostCommand
}

// darwinSystem measures a Mac. macOS has no pressure stall information or
// OOM kill count, and its CPU ticks are a Mach call (host_processor_info)
// that a binary without cgo cannot make: those are errors.ErrUnsupported.
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
func (darwinSystem) CPU() (sysinfo.CPUShare, error)   { return sysinfo.CPUShare{}, errors.ErrUnsupported }
func (darwinSystem) OOMKills() (int64, error)         { return 0, errors.ErrUnsupported }

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
