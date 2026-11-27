package worker

import (
	"context"
	"maps"
	"testing"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"google.golang.org/grpc"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/statedir"
)

// statsCladefsd is a cladefsd whose VMs say what the test sets.
type statsCladefsd struct {
	cladefsdv1.CladefsdClient
	vms []*cladefsdv1.VmStats
}

func (f *statsCladefsd) GetStats(_ context.Context, req *cladefsdv1.GetStatsRequest, _ ...grpc.CallOption) (*cladefsdv1.GetStatsResponse, error) {
	if !req.GetLeaveOutHistograms() || req.GetLeaveOutGuests() || req.GetLeaveOutVms() {
		return nil, context.Canceled
	}
	return &cladefsdv1.GetStatsResponse{Vms: f.vms}, nil
}

func vmStats(conn, tenant, cell uint64, written uint64, answered bool) *cladefsdv1.VmStats {
	v := &cladefsdv1.VmStats{Connection: conn, Tenant: tenant, Cell: cell, Answered: answered}
	if answered {
		v.Guest = &cladefsdv1.GuestStats{Counters: map[string]uint64{"device_write_bytes": written, "device_writes": 9}}
	}
	return v
}

// VolumeWrites asks cladefsd for every VM's guest stats, and names each
// VM's writes by its workspace's volume, a VM of no workspace by its cell.
func TestVolumeWritesByWorkspace(t *testing.T) {
	api := &statsCladefsd{}
	st, err := cladefs.NewStore(t.Context(), cladefs.Config{CellRoot: t.TempDir()}, api, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := newVMBackend("", statedir.VMConfig{})
	b.store.Store(st)
	b.wsCells["m"] = cladefs.CellRef{Tenant: 5, Cell: 40}
	api.vms = []*cladefsdv1.VmStats{vmStats(1, 5, 40, 100, true), vmStats(2, 5, 99, 30, true)}
	if _, err := b.VolumeWrites(); err != nil {
		t.Fatal(err)
	}
	api.vms = []*cladefsdv1.VmStats{vmStats(1, 5, 40, 150, true), vmStats(2, 5, 99, 31, true)}
	got, err := b.VolumeWrites()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"ws/m": 50, "cell 5/99": 1}; !maps.Equal(got, want) {
		t.Errorf("VolumeWrites = %v, want %v", got, want)
	}
}

// Each volume's count only grows, from when its VM was first heard from: a
// VM that was there before counts from what it first said (not all it wrote
// since it booted), one that booted since counts all it wrote, one that
// doesn't answer counts what it said last, and one that ended still counts
// until its writes are too old to judge a measurement by.
func TestVolumeWritesOnlyGrow(t *testing.T) {
	vols := map[cellKey]string{{5, 40}: "ws/m", {5, 41}: "ws/b", {5, 42}: "ws/c"}
	at := time.Unix(1795400000, 0)
	var s vmWrites
	step := func(want map[string]int64, vms ...*cladefsdv1.VmStats) {
		t.Helper()
		at = at.Add(time.Second)
		if got := s.add(vms, vols, at); !maps.Equal(got, want) {
			t.Errorf("at %v: %v, want %v", at.Format(time.TimeOnly), got, want)
		}
	}
	// c's VM, there already, doesn't answer at first.
	step(map[string]int64{"ws/m": 0, "ws/b": 0, "ws/c": 0},
		vmStats(1, 5, 40, 100, true), vmStats(2, 5, 41, 7_000, true), vmStats(4, 5, 42, 0, false))
	step(map[string]int64{"ws/m": 0, "ws/b": 2_000, "cell 5/99": 30, "ws/c": 0},
		vmStats(1, 5, 40, 0, false), vmStats(2, 5, 41, 9_000, true), vmStats(3, 5, 99, 30, true), vmStats(4, 5, 42, 0, false))
	step(map[string]int64{"ws/m": 400, "ws/b": 2_000, "cell 5/99": 30, "ws/c": 0},
		vmStats(1, 5, 40, 500, true), vmStats(3, 5, 99, 30, true), vmStats(4, 5, 42, 50_000_000, true))
	// b's VM booted again: what it writes adds to what its last one wrote.
	step(map[string]int64{"ws/m": 400, "ws/b": 2_500, "cell 5/99": 30, "ws/c": 1_000},
		vmStats(1, 5, 40, 500, true), vmStats(3, 5, 99, 30, true), vmStats(4, 5, 42, 50_001_000, true), vmStats(5, 5, 41, 500, true))
	at = at.Add(endedWritesKept)
	step(map[string]int64{"ws/m": 400, "ws/b": 500, "cell 5/99": 30, "ws/c": 1_000},
		vmStats(1, 5, 40, 500, true), vmStats(3, 5, 99, 30, true), vmStats(4, 5, 42, 50_001_000, true), vmStats(5, 5, 41, 500, true))
}
