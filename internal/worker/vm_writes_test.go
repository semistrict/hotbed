package worker

import (
	"context"
	"maps"
	"testing"

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

// VolumeWrites names each VM's writes by its workspace's volume, a VM of
// no workspace by its cell, and counts what a VM said last while it
// doesn't answer, so a measurement's window never sees it fall to nothing
// and rise again.
func TestVolumeWritesByWorkspace(t *testing.T) {
	api := &statsCladefsd{}
	st, err := cladefs.NewStore(t.Context(), cladefs.Config{CellRoot: t.TempDir()}, api, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := newVMBackend("", statedir.VMConfig{})
	b.store.Store(st)
	b.wsCells["m"] = cladefs.CellRef{Tenant: 5, Cell: 40}
	b.wsCells["b"] = cladefs.CellRef{Tenant: 5, Cell: 41}

	api.vms = []*cladefsdv1.VmStats{vmStats(1, 5, 40, 100, true), vmStats(2, 5, 41, 7_000, true), vmStats(3, 5, 99, 30, true), vmStats(4, 5, 41, 0, false)}
	got, err := b.VolumeWrites()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"ws/m": 100, "ws/b": 7_000, "cell 5/99": 30}; !maps.Equal(got, want) {
		t.Errorf("VolumeWrites = %v, want %v", got, want)
	}

	api.vms = []*cladefsdv1.VmStats{vmStats(1, 5, 40, 0, false), vmStats(2, 5, 41, 9_000, true)}
	got, err = b.VolumeWrites()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"ws/m": 100, "ws/b": 9_000}; !maps.Equal(got, want) {
		t.Errorf("VolumeWrites with m's VM not answering = %v, want %v", got, want)
	}
}
