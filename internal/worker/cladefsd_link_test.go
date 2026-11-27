package worker

import (
	"context"
	"slices"
	"testing"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/statedir"
)

// fakeLinkCladefsd is a cladefsd whose incarnation the test changes, as a
// restart does, and that records what the worker applies to it.
type fakeLinkCladefsd struct {
	cladefsdv1.CladefsdClient
	incarnation  uint64
	down         bool
	setPeers     [][]string
	decommission []uint64
}

func (f *fakeLinkCladefsd) GetStats(context.Context, *cladefsdv1.GetStatsRequest, ...grpc.CallOption) (*cladefsdv1.GetStatsResponse, error) {
	if f.down {
		return nil, status.Error(codes.Unavailable, "connection refused")
	}
	return &cladefsdv1.GetStatsResponse{Incarnation: f.incarnation}, nil
}

func (f *fakeLinkCladefsd) SetPeers(_ context.Context, req *cladefsdv1.SetPeersRequest, _ ...grpc.CallOption) (*cladefsdv1.SetPeersResponse, error) {
	if f.down {
		return nil, status.Error(codes.Unavailable, "connection refused")
	}
	f.setPeers = append(f.setPeers, slices.Clone(req.GetAddresses()))
	return &cladefsdv1.SetPeersResponse{}, nil
}

func (f *fakeLinkCladefsd) DecommissionHost(_ context.Context, req *cladefsdv1.DecommissionHostRequest, _ ...grpc.CallOption) (*cladefsdv1.DecommissionHostResponse, error) {
	f.decommission = append(f.decommission, req.GetIncarnation())
	return &cladefsdv1.DecommissionHostResponse{}, nil
}

// A cladefsd that restarts behind the worker's back (systemd after it died,
// or an upgrade whose reconnect gave up before it answered) comes back with
// a new incarnation and no peers: the next heartbeat must report the new
// incarnation and apply the mesh again, and decommission again what only
// the old daemon had done.
func TestCladefsdLinkFollowsARestartedDaemon(t *testing.T) {
	ctx := t.Context()
	api := &fakeLinkCladefsd{incarnation: 7}
	st, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: t.TempDir()}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const me = "10.0.0.1:9185"
	peers := []string{"10.0.0.2:9185", me}
	b := newVMBackend("", statedir.VMConfig{CladefsPeerAddr: me})
	b.link = cladefsdLink{incarnation: 7}
	b.store.Store(st)

	b.ApplyHeartbeat(ctx, peers, []uint64{3}, nil)
	b.ApplyHeartbeat(ctx, peers, []uint64{3}, nil)
	want := [][]string{{me, "10.0.0.2:9185"}}
	if !slices.EqualFunc(api.setPeers, want, slices.Equal) || !slices.Equal(api.decommission, []uint64{3}) {
		t.Fatalf("before the restart: SetPeers %v, DecommissionHost %v; want %v once and [3] once", api.setPeers, api.decommission, want)
	}

	// Down for a while: nothing is forgotten, nothing applied.
	api.down = true
	b.ApplyHeartbeat(ctx, peers, nil, nil)
	if _, inc, _, _ := b.HeartbeatMeta(); inc != 7 || len(api.setPeers) != 1 {
		t.Fatalf("while down: incarnation %d, SetPeers %v; want 7 and no new call", inc, api.setPeers)
	}

	api.down, api.incarnation = false, 8
	b.ApplyHeartbeat(ctx, peers, []uint64{3}, nil)
	if _, inc, _, _ := b.HeartbeatMeta(); inc != 8 {
		t.Fatalf("incarnation after the restart = %d, want 8", inc)
	}
	want = append(want, want[0])
	if !slices.EqualFunc(api.setPeers, want, slices.Equal) || !slices.Equal(api.decommission, []uint64{3, 3}) {
		t.Fatalf("after the restart: SetPeers %v, DecommissionHost %v; want %v and [3 3]", api.setPeers, api.decommission, want)
	}
	if got := b.link.Peers(); !slices.Equal(got, want[0]) {
		t.Fatalf("applied peers = %v, want %v", got, want[0])
	}
}
