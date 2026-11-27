package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/semistrict/hotbed/internal/bench"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

// prepareHost readies a Linux machine for the worker's commands: their
// slice, its priority and its memory bound, and the measurements'
// isolation.
func prepareHost(ctx context.Context, h hostPrep) error {
	// Ensure workspace commands (via the command user's kvm supplementary group)
	// can open /dev/userfaultfd read-write on Linux hosts, matching Firecracker
	// guests (cmd/hotbed-guestd/devperms.go) and persisting across reboots.
	if err := EnsureUserfaultfdAccess(ctx, h.effCfg.User); err != nil {
		return fmt.Errorf("configure /dev/userfaultfd access for %s: %w", h.effCfg.User, err)
	}
	// Lower the work slice's CPU and IO priority so normal commands yield to
	// desktop and system processes while still sharing spare CPU among
	// themselves by their per-command CPUWeight=. Measurements run in a
	// sibling slice (*_bench.slice) at normal priority.
	if err := unit.StopLeftExecs(ctx); err != nil {
		return fmt.Errorf("stop the last worker's chroot git: %w", err)
	}
	if err := unit.StartSlice(ctx, h.workSlice); err != nil {
		return fmt.Errorf("start work slice %s: %w", h.workSlice, err)
	}
	if err := unit.SetProperties(ctx, h.workSlice, "CPUWeight=idle", "IOWeight=1"); err != nil {
		return fmt.Errorf("set work slice %s priority: %w", h.workSlice, err)
	}
	// Bound what the commands take together, base builds and work alike,
	// leaving the host room to stay up (board 9128). On zram hosts, swap
	// sits in kernel RAM outside the slice's memcg, so the backstop also
	// caps MemorySwapMax and accounts for zram's RAM footprint (board 9361).
	// The engine measures its memory, and admits and parks by it, within
	// this bound. The measurement slice is its sibling, outside it.
	host, err := sysinfo.Meminfo()
	if err != nil {
		return fmt.Errorf("read the host's memory: %w", err)
	}
	swaps, err := sysinfo.Swaps()
	if err != nil {
		return fmt.Errorf("read the host's swap: %w", err)
	}
	var cladefsFloor int64
	if b := h.ecfg.cladefs(); b != nil {
		if osStats := b.SampleObjectStore(ctx); osStats != nil {
			cladefsFloor = int64(osStats.GetExtentCacheFloorBytes())
		}
	}
	cladefsReserve := unit.CladefsReserve(host.Total, cladefsFloor)
	backstop := unit.BackstopForWithReserve(host.Total, unit.Swap{Zram: swaps.ZramBytes, Disk: swaps.DiskBytes}, cladefsReserve)
	if static, err := unit.StaticMemoryLimits(ctx, h.workSlice, host.Total); err != nil {
		slog.Warn("read static work slice memory limits", "slice", h.workSlice, "err", err)
	} else {
		backstop = backstop.Clamp(static)
	}
	if err := unit.SetProperties(ctx, h.workSlice, backstop.Properties()...); err != nil {
		return fmt.Errorf("bound work slice %s's memory: %w", h.workSlice, err)
	}
	slog.Info("work slice memory bounded",
		"slice", h.workSlice,
		"high", backstop.High,
		"max", backstop.Max,
		"swapMax", backstop.SwapMax,
		"headroom", backstop.Headroom,
		"reserve", backstop.Reserve,
		"cladefsFloor", cladefsFloor,
		"zram", swaps.ZramBytes,
		"diskSwap", swaps.DiskBytes,
	)
	// Under /run: what an isolation saves of the host lasts as long as the
	// kernel's settings it changed.
	benchOpts := bench.Options{
		State: "/run/hotbed/bench", Slice: BenchSlice(h.effCfg.Slice),
		WorkSlice: h.workSlice, Systemd: Systemd{},
	}
	// Other work's writes to the volumes are kept quiet beside a
	// measurement too: capped on a block device (btrfs's), held back on a
	// ZFS pool, which no cap reaches. A worker that keeps no volumes (a VM
	// worker's) has none to quiet.
	switch s := h.store.(type) {
	case storage.Pooled:
		benchOpts.Pool = s.Dataset()
	case storage.NoVolumes:
	default:
		benchOpts.StoreDisk = s.Path("")
	}
	if h.cfg.Bench != nil {
		iso, err := bench.New(*h.cfg.Bench, benchOpts)
		if err != nil {
			return fmt.Errorf("the worker's measurements: %w", err)
		}
		h.ecfg.Bench = iso
	} else if err := bench.GiveBack(ctx, benchOpts); err != nil {
		// A worker that took measurements before its config stopped them.
		slog.Error("give back a measurement's isolation", "err", err)
	}
	return nil
}

// hostUnits runs the worker's commands as systemd units.
func hostUnits() Units { return Systemd{} }

// hostSystem measures the machine as commands in slice get it.
func hostSystem(slice string) System { return &Linux{Slice: slice} }
