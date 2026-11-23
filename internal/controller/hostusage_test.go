package controller

import (
	"context"
	"math"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

func TestHostStarvationThresholds(t *testing.T) {
	tests := []struct {
		name        string
		guest       *pb.NodeUsage
		host        *pb.HostUsage
		wantHold    bool
		wantHolding []string
	}{
		{
			name:        "healthy VM is not holding",
			guest:       &pb.NodeUsage{CpuBusy: 0.40},
			host:        &pb.HostUsage{VmCpuBusy: 0.35, HostCpuBusy: 0.45, HostLoad_1M: 4.0, HostCpus: 12, HostMemoryPressure: 0},
			wantHold:    false,
			wantHolding: nil,
		},
		{
			name:        "host CPU busy >= 85% holds",
			guest:       &pb.NodeUsage{CpuBusy: 0.20},
			host:        &pb.HostUsage{VmCpuBusy: 0.20, HostCpuBusy: 0.85, HostLoad_1M: 6.0, HostCpus: 12, HostMemoryPressure: 0},
			wantHold:    true,
			wantHolding: []string{"host-cpu-starvation"},
		},
		{
			name:        "host load >= 1.25 * CPUs holds",
			guest:       &pb.NodeUsage{CpuBusy: 0.20},
			host:        &pb.HostUsage{VmCpuBusy: 0.20, HostCpuBusy: 0.60, HostLoad_1M: 15.0, HostCpus: 12, HostMemoryPressure: 0},
			wantHold:    true,
			wantHolding: []string{"host-cpu-starvation"},
		},
		{
			name:        "guest busy >= 50% with >= 35% CPU steal holds",
			guest:       &pb.NodeUsage{CpuBusy: 0.80},
			host:        &pb.HostUsage{VmCpuBusy: 0.20, HostCpuBusy: 0.55, HostLoad_1M: 5.0, HostCpus: 12, HostMemoryPressure: 0},
			wantHold:    true,
			wantHolding: []string{"host-cpu-starvation"},
		},
		{
			name:        "macOS memory pressure warn (50) holds",
			guest:       &pb.NodeUsage{CpuBusy: 0.10},
			host:        &pb.HostUsage{VmCpuBusy: 0.10, HostCpuBusy: 0.30, HostLoad_1M: 2.0, HostCpus: 12, HostMemoryPressure: 50},
			wantHold:    true,
			wantHolding: []string{"host-memory-pressure"},
		},
		{
			name:        "CPU steal and critical memory pressure both hold",
			guest:       &pb.NodeUsage{CpuBusy: 0.90},
			host:        &pb.HostUsage{VmCpuBusy: 0.15, HostCpuBusy: 0.95, HostLoad_1M: 18.0, HostCpus: 12, HostMemoryPressure: 100},
			wantHold:    true,
			wantHolding: []string{"host-cpu-starvation", "host-memory-pressure"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			holding, reason, signals := HostStarvation(tc.guest, tc.host)
			if holding != tc.wantHold {
				t.Fatalf("holding = %v (reason %q), want %v", holding, reason, tc.wantHold)
			}
			if holding && reason == "" {
				t.Error("expected non-empty reason when holding")
			}
			var gotHolding []string
			for _, s := range signals {
				if s.Holding {
					gotHolding = append(gotHolding, s.Name)
				}
			}
			if !slices.Equal(gotHolding, tc.wantHolding) {
				t.Errorf("holding signals = %v, want %v", gotHolding, tc.wantHolding)
			}
		})
	}
}

func TestReportHostUsageFlowAndExpiry(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctrlSelf := tailnet.Self{
		Peer:  tailnet.Peer{NodeID: "nCtrl", Hostname: "ctrl", Login: "ramon@"},
		Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	}
	vmPeer := tailnet.Peer{NodeID: "nVM", Hostname: "mac-worker", Login: "ramon@"}
	bmPeer := tailnet.Peer{NodeID: "nBM", Hostname: "ubu1", Login: "ramon@"}
	hostPeer := tailnet.Peer{NodeID: "nMacHost", Hostname: "ramon-mbp", Login: "ramon@"}

	tn := tailnettest.New(ctrlSelf)
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	srv := NewServer(ServerConfig{
		Store:      store,
		Identifier: tn,
		Self:       ctrlSelf.Addrs,
		Cluster:    statedir.ControllerConfig{ClusterID: "c1", Owner: "ramon@", Port: 7120},
		Now:        clock.now,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	addr := strings.TrimPrefix(httpSrv.URL, "http://")

	vmClient, _, err := api.Controller(addr, tn.Client(vmPeer))
	if err != nil {
		t.Fatal(err)
	}
	bmClient, _, err := api.Controller(addr, tn.Client(bmPeer))
	if err != nil {
		t.Fatal(err)
	}
	hostClient, _, err := api.Controller(addr, tn.Client(hostPeer))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := vmClient.Join(ctx, &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := bmClient.Join(ctx, &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}

	// Open a real Standby stream from hostPeer so mac-worker is actively
	// supervised by hostPeer.
	standbyCtx, cancelStandby := context.WithCancel(ctx)
	t.Cleanup(cancelStandby)
	stream, err := hostClient.Standby(standbyCtx, &pb.StandbyRequest{
		Node:              "mac-worker",
		Running:           true,
		SleepAfterSeconds: 1800,
	})
	if err != nil {
		t.Fatalf("Standby: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Standby initial receive: %v", err)
	}

	// Validation errors.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("empty request code = %s, want INVALID_ARGS", api.CodeOf(err))
	}
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "mac-worker"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("missing usage code = %s, want INVALID_ARGS", api.CodeOf(err))
	}

	// Host supervisor reports severe CPU steal (guest 85%, VM only 15%, host 92%) + warn memory pressure.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:          0.15,
			HostCpuBusy:        0.92,
			HostLoad_1M:        16.5,
			HostCpus:           12,
			HostMemoryPressure: 50,
			SampledAt:          clock.now().UnixMilli(),
		},
	}); err != nil {
		t.Fatalf("ReportHostUsage: %v", err)
	}

	// Heartbeat from mac-worker (still claiming Admission.Holding == false) receives HostUsage in reply.
	hbResp, err := vmClient.Heartbeat(ctx, &pb.HeartbeatRequest{
		ClusterId: "c1",
		Resources: &pb.Resources{Arch: "arm64", Cpus: 7, MemoryBytes: 16 << 30},
		Usage:     &pb.NodeUsage{CpuBusy: 0.85, MemoryUsedBytes: 4 << 30},
		Admission: &pb.Admission{Holding: false, Signals: []*pb.AdmissionSignal{
			{Name: "cpu-pressure", Value: 5, Limit: 60, Unit: "%", Holding: false},
		}},
	})
	if err != nil {
		t.Fatalf("VM Heartbeat: %v", err)
	}
	if hbResp.HostUsage == nil {
		t.Fatal("HeartbeatResponse.HostUsage = nil, want fresh HostUsage")
	}
	if hbResp.HostUsage.VmCpuBusy != 0.15 || hbResp.HostUsage.HostCpuBusy != 0.92 || hbResp.HostUsage.HostMemoryPressure != 50 {
		t.Fatalf("HeartbeatResponse.HostUsage = %+v", hbResp.HostUsage)
	}
	if hbResp.HostUsage.RemainingTtlMs != hostUsageTTL.Milliseconds() {
		t.Fatalf("HeartbeatResponse.HostUsage.RemainingTtlMs = %d, want %d", hbResp.HostUsage.RemainingTtlMs, hostUsageTTL.Milliseconds())
	}

	// Bare-metal worker gets nil HostUsage.
	bmResp, err := bmClient.Heartbeat(ctx, &pb.HeartbeatRequest{
		ClusterId: "c1",
		Resources: &pb.Resources{Arch: "amd64", Cpus: 16, MemoryBytes: 64 << 30},
		Usage:     &pb.NodeUsage{CpuBusy: 0.30, MemoryUsedBytes: 12 << 30},
		Admission: &pb.Admission{Holding: false},
	})
	if err != nil {
		t.Fatalf("BM Heartbeat: %v", err)
	}
	if bmResp.HostUsage != nil {
		t.Fatalf("bare-metal HeartbeatResponse.HostUsage = %+v, want nil", bmResp.HostUsage)
	}

	// ListNodes exposes Node.HostUsage and immediately marks Admission.Holding == true on starvation.
	nodesResp, err := hostClient.ListNodes(ctx, &pb.ListNodesRequest{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	var vmNode, bmNode *pb.Node
	for _, n := range nodesResp.Nodes {
		switch n.Hostname {
		case "mac-worker":
			vmNode = n
		case "ubu1":
			bmNode = n
		}
	}
	if vmNode == nil || vmNode.HostUsage == nil {
		t.Fatalf("ListNodes mac-worker HostUsage missing: %+v", vmNode)
	}
	if !vmNode.Admission.GetHolding() {
		t.Fatalf("ListNodes mac-worker Admission.Holding = false, want true (reason=%q)", vmNode.Admission.GetReason())
	}
	if bmNode == nil || bmNode.HostUsage != nil || bmNode.Admission.GetHolding() {
		t.Fatalf("bare-metal node unexpectedly affected: hostUsage=%+v admission=%+v", bmNode.HostUsage, bmNode.Admission)
	}

	// GetSnapshot exposes Node.HostUsage, Admission.Holding == true, and overlays VmCpuBusy/HostCpuBusy onto Series.
	snap, err := hostClient.GetSnapshot(ctx, &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	vmSamples := snap.Series["nVM"].GetSamples()
	if len(vmSamples) == 0 {
		t.Fatal("GetSnapshot missing nVM series samples")
	}
	lastSample := vmSamples[len(vmSamples)-1]
	if lastSample.VmCpuBusy == nil || *lastSample.VmCpuBusy != 0.15 {
		t.Errorf("Sample.VmCpuBusy = %v, want 0.15", lastSample.VmCpuBusy)
	}
	if lastSample.HostCpuBusy == nil || *lastSample.HostCpuBusy != 0.92 {
		t.Errorf("Sample.HostCpuBusy = %v, want 0.92", lastSample.HostCpuBusy)
	}
	bmSamples := snap.Series["nBM"].GetSamples()
	if len(bmSamples) > 0 && (bmSamples[len(bmSamples)-1].VmCpuBusy != nil || bmSamples[len(bmSamples)-1].HostCpuBusy != nil) {
		t.Errorf("bare-metal sample unexpectedly has host CPU fields: %+v", bmSamples[len(bmSamples)-1])
	}

	// Sanitization clamps NaN / out-of-range values cleanly over the wire.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:          math.NaN(),
			HostCpuBusy:        2.5,
			HostLoad_1M:        -3,
			HostCpus:           -2,
			HostMemoryPressure: 200,
		},
	}); err != nil {
		t.Fatalf("ReportHostUsage sanitize: %v", err)
	}
	gotSanitized := srv.hostUsage("mac-worker", clock.now())
	if gotSanitized.VmCpuBusy != 0 || gotSanitized.HostCpuBusy != 1 || gotSanitized.HostLoad_1M != 0 || gotSanitized.HostCpus != 0 || gotSanitized.HostMemoryPressure != 100 {
		t.Errorf("sanitized HostUsage = %+v", gotSanitized)
	}

	// Stale HostUsage past hostUsageTTL expires and stops holding.
	clock.advance(hostUsageTTL + time.Second)
	hbResp2, err := vmClient.Heartbeat(ctx, &pb.HeartbeatRequest{
		ClusterId: "c1",
		Resources: &pb.Resources{Arch: "arm64", Cpus: 7, MemoryBytes: 16 << 30},
		Usage:     &pb.NodeUsage{CpuBusy: 0.85, MemoryUsedBytes: 4 << 30},
		Admission: &pb.Admission{Holding: false},
	})
	if err != nil {
		t.Fatalf("VM Heartbeat after expiry: %v", err)
	}
	if hbResp2.HostUsage != nil {
		t.Fatalf("expired HostUsage still returned in HeartbeatResponse: %+v", hbResp2.HostUsage)
	}
	detail, err := hostClient.GetNode(ctx, &pb.GetNodeRequest{Node: "mac-worker"})
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if detail.Node.HostUsage != nil {
		t.Errorf("expired HostUsage still present on GetNode: %+v", detail.Node.HostUsage)
	}
	if detail.Node.Admission.GetHolding() {
		t.Errorf("expired HostUsage still holding Admission on GetNode: %+v", detail.Node.Admission)
	}
}

func TestReportHostUsageAuthorizationAndPruning(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctrlSelf := tailnet.Self{
		Peer:  tailnet.Peer{NodeID: "nCtrl", Hostname: "ctrl", Login: "ramon@"},
		Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	}
	vmPeer := tailnet.Peer{NodeID: "nVM", Hostname: "mac-worker", Login: "ramon@"}
	bmPeer := tailnet.Peer{NodeID: "nBM", Hostname: "ubu1", Login: "ramon@"}
	retiredPeer := tailnet.Peer{NodeID: "nOld", Hostname: "old-vm", Login: "ramon@"}
	hostPeer := tailnet.Peer{NodeID: "nMacHost", Hostname: "ramon-mbp", Login: "ramon@"}
	wrongPeer := tailnet.Peer{NodeID: "nOther", Hostname: "other-mac", Login: "ramon@"}

	tn := tailnettest.New(ctrlSelf)
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	srv := NewServer(ServerConfig{
		Store:      store,
		Identifier: tn,
		Self:       ctrlSelf.Addrs,
		Cluster:    statedir.ControllerConfig{ClusterID: "c1", Owner: "ramon@", Port: 7120},
		Now:        clock.now,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	addr := strings.TrimPrefix(httpSrv.URL, "http://")

	vmClient, _, err := api.Controller(addr, tn.Client(vmPeer))
	if err != nil {
		t.Fatal(err)
	}
	bmClient, _, err := api.Controller(addr, tn.Client(bmPeer))
	if err != nil {
		t.Fatal(err)
	}
	retiredClient, _, err := api.Controller(addr, tn.Client(retiredPeer))
	if err != nil {
		t.Fatal(err)
	}
	hostClient, _, err := api.Controller(addr, tn.Client(hostPeer))
	if err != nil {
		t.Fatal(err)
	}
	wrongClient, _, err := api.Controller(addr, tn.Client(wrongPeer))
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		fn   func() error
	}{
		{"vm", func() error { _, err := vmClient.Join(ctx, &pb.JoinRequest{}); return err }},
		{"bm", func() error { _, err := bmClient.Join(ctx, &pb.JoinRequest{}); return err }},
		{"retired", func() error { _, err := retiredClient.Join(ctx, &pb.JoinRequest{}); return err }},
	} {
		if err := c.fn(); err != nil {
			t.Fatalf("join %s: %v", c.name, err)
		}
	}
	if err := store.RetireNode(ctx, retiredPeer.NodeID, clock.now()); err != nil {
		t.Fatalf("RetireNode: %v", err)
	}

	sample := func() *pb.HostUsage {
		return &pb.HostUsage{
			VmCpuBusy:          0.25,
			HostCpuBusy:        0.90,
			HostLoad_1M:        14.0,
			HostCpus:           12,
			HostMemoryPressure: 50,
			SampledAt:          clock.now().UnixMilli(),
		}
	}
	mapSizes := func() (int, int) {
		srv.hostUsages.mu.Lock()
		defer srv.hostUsages.mu.Unlock()
		return len(srv.hostUsages.latest), len(srv.hostUsages.history)
	}

	// 1. Unknown hostname is rejected with NOT_FOUND without allocating map state.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "ghost-node", Usage: sample()}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("unknown node code = %s, want NOT_FOUND (err=%v)", api.CodeOf(err), err)
	}
	if l, h := mapSizes(); l != 0 || h != 0 {
		t.Fatalf("unknown node allocated map state: latest=%d history=%d", l, h)
	}

	// 2. Retired node is rejected with NOT_FOUND without allocating map state.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "old-vm", Usage: sample()}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("retired node code = %s, want NOT_FOUND (err=%v)", api.CodeOf(err), err)
	}
	if l, h := mapSizes(); l != 0 || h != 0 {
		t.Fatalf("retired node allocated map state: latest=%d history=%d", l, h)
	}

	// 3. Unsupervised bare-metal node is rejected with CONFLICT without allocating map state.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "ubu1", Usage: sample()}); api.CodeOf(err) != api.CodeConflict {
		t.Fatalf("unsupervised bare-metal node code = %s, want CONFLICT (err=%v)", api.CodeOf(err), err)
	}
	if l, h := mapSizes(); l != 0 || h != 0 {
		t.Fatalf("unsupervised node allocated map state: latest=%d history=%d", l, h)
	}

	// 4. Registered VM before Standby connects is also rejected with CONFLICT.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "mac-worker", Usage: sample()}); api.CodeOf(err) != api.CodeConflict {
		t.Fatalf("unsupervised VM code = %s, want CONFLICT (err=%v)", api.CodeOf(err), err)
	}
	if l, h := mapSizes(); l != 0 || h != 0 {
		t.Fatalf("unsupervised VM allocated map state: latest=%d history=%d", l, h)
	}

	// Supervise mac-worker from hostPeer.
	release := srv.power.holdBy("mac-worker", hostPeer, true, 30*time.Minute)

	// 5. Wrong supervising peer is rejected with NOT_AUTHORIZED without allocating map state.
	if _, err := wrongClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "mac-worker", Usage: sample()}); api.CodeOf(err) != api.CodeNotAuthorized {
		t.Fatalf("wrong supervisor code = %s, want NOT_AUTHORIZED (err=%v)", api.CodeOf(err), err)
	}
	if l, h := mapSizes(); l != 0 || h != 0 {
		t.Fatalf("wrong supervisor allocated map state: latest=%d history=%d", l, h)
	}

	// 6. Direct call without authenticated peer context is also rejected.
	if _, err := srv.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "mac-worker", Usage: sample()}); api.CodeOf(err) != api.CodeNotAuthorized {
		t.Fatalf("missing peer code = %s, want NOT_AUTHORIZED (err=%v)", api.CodeOf(err), err)
	}

	// 7. Registered + supervised VM node reported by its supervising peer succeeds.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "mac-worker", Usage: sample()}); err != nil {
		t.Fatalf("authorized supervisor ReportHostUsage: %v", err)
	}
	if l, h := mapSizes(); l != 1 || h != 1 {
		t.Fatalf("after valid report map sizes = (%d, %d), want (1, 1)", l, h)
	}

	// Still authorized during standbyGrace after Standby stream ends, rejected once grace expires.
	release()
	clock.advance(standbyGrace - time.Second)
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "mac-worker", Usage: sample()}); err != nil {
		t.Fatalf("within standbyGrace ReportHostUsage: %v", err)
	}
	clock.advance(time.Second)
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{Node: "mac-worker", Usage: sample()}); api.CodeOf(err) != api.CodeConflict {
		t.Fatalf("after standbyGrace code = %s, want CONFLICT (err=%v)", api.CodeOf(err), err)
	}

	// 8. Expired latest entry is pruned at hostUsageTTL; expired history entry is pruned after liveWindow + hostUsageTTL.
	clock.advance(hostUsageTTL)
	if got := srv.hostUsage("mac-worker", clock.now()); got != nil {
		t.Fatalf("hostUsage at TTL boundary = %+v, want nil", got)
	}
	if l, h := mapSizes(); l != 0 || h != 1 {
		t.Fatalf("after hostUsageTTL map sizes = (%d, %d), want (0, 1)", l, h)
	}
	clock.advance(liveWindow)
	if err := srv.ReapIdle(ctx); err != nil {
		t.Fatalf("ReapIdle: %v", err)
	}
	if l, h := mapSizes(); l != 0 || h != 0 {
		t.Fatalf("after liveWindow + hostUsageTTL prune map sizes = (%d, %d), want (0, 0)", l, h)
	}
}

func TestHostUsageRemainingTTLAndChartOverlayDeadline(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctrlSelf := tailnet.Self{
		Peer:  tailnet.Peer{NodeID: "nCtrl", Hostname: "ctrl", Login: "ramon@"},
		Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	}
	vmPeer := tailnet.Peer{NodeID: "nVM", Hostname: "mac-worker", Login: "ramon@"}
	hostPeer := tailnet.Peer{NodeID: "nMacHost", Hostname: "ramon-mbp", Login: "ramon@"}

	tn := tailnettest.New(ctrlSelf)
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	srv := NewServer(ServerConfig{
		Store:      store,
		Identifier: tn,
		Self:       ctrlSelf.Addrs,
		Cluster:    statedir.ControllerConfig{ClusterID: "c1", Owner: "ramon@", Port: 7120},
		Now:        clock.now,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	addr := strings.TrimPrefix(httpSrv.URL, "http://")

	vmClient, _, err := api.Controller(addr, tn.Client(vmPeer))
	if err != nil {
		t.Fatal(err)
	}
	hostClient, _, err := api.Controller(addr, tn.Client(hostPeer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vmClient.Join(ctx, &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	defer srv.power.holdBy("mac-worker", hostPeer, true, 30*time.Minute)()

	sendHB := func() *pb.HeartbeatResponse {
		t.Helper()
		resp, err := vmClient.Heartbeat(ctx, &pb.HeartbeatRequest{
			ClusterId: "c1",
			Resources: &pb.Resources{Arch: "arm64", Cpus: 7, MemoryBytes: 16 << 30},
			Usage:     &pb.NodeUsage{CpuBusy: 0.70, MemoryUsedBytes: 4 << 30},
		})
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		return resp
	}

	// Report at t = 0s.
	t0 := clock.now().UnixMilli()
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:          0.15,
			HostCpuBusy:        0.92,
			HostLoad_1M:        15.0,
			HostCpus:           12,
			HostMemoryPressure: 50,
			SampledAt:          t0,
		},
	}); err != nil {
		t.Fatalf("ReportHostUsage t0: %v", err)
	}

	hb0 := sendHB()
	if hb0.HostUsage == nil || hb0.HostUsage.RemainingTtlMs != 45_000 {
		t.Fatalf("t=0s HeartbeatResponse.HostUsage = %+v, want RemainingTtlMs=45000", hb0.HostUsage)
	}

	// At t = 44s, RemainingTtlMs shrinks to 1000ms (and Heartbeat must NOT synthesize a fresh history point).
	clock.advance(44 * time.Second)
	t44 := clock.now().UnixMilli()
	hb44 := sendHB()
	if hb44.HostUsage == nil || hb44.HostUsage.RemainingTtlMs != 1_000 {
		t.Fatalf("t=44s HeartbeatResponse.HostUsage = %+v, want RemainingTtlMs=1000", hb44.HostUsage)
	}

	// At t = 45s (exact original deadline), HostUsage expires -> nil.
	clock.advance(1 * time.Second)
	t45 := clock.now().UnixMilli()
	hb45 := sendHB()
	if hb45.HostUsage != nil {
		t.Fatalf("t=45s HeartbeatResponse.HostUsage = %+v, want nil", hb45.HostUsage)
	}

	// At t = 60s (16s after the 44s heartbeat, 60s after the only host report), still nil.
	clock.advance(15 * time.Second)
	t60 := clock.now().UnixMilli()
	hb60 := sendHB()
	if hb60.HostUsage != nil {
		t.Fatalf("t=60s HeartbeatResponse.HostUsage = %+v, want nil", hb60.HostUsage)
	}

	// Reporting resumes at t = 70s with healthy values.
	clock.advance(10 * time.Second)
	t70 := clock.now().UnixMilli()
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:   0.40,
			HostCpuBusy: 0.45,
			HostLoad_1M: 4.0,
			HostCpus:    12,
			SampledAt:   t70,
		},
	}); err != nil {
		t.Fatalf("ReportHostUsage t70: %v", err)
	}
	sendHB()

	snap, err := hostClient.GetSnapshot(ctx, &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	byT := map[int64]*pb.Sample{}
	for _, sp := range snap.Series["nVM"].GetSamples() {
		byT[sp.T] = sp
	}
	assertOverlay := func(ts int64, label string, wantPresent bool, wantVM, wantHost float64) {
		t.Helper()
		sp := byT[ts]
		if sp == nil {
			t.Fatalf("%s: missing sample at t=%d", label, ts)
		}
		if !wantPresent {
			if sp.VmCpuBusy != nil || sp.HostCpuBusy != nil {
				t.Errorf("%s: got VmCpuBusy=%v HostCpuBusy=%v, want nil gap after deadline", label, sp.VmCpuBusy, sp.HostCpuBusy)
			}
			return
		}
		if sp.VmCpuBusy == nil || *sp.VmCpuBusy != wantVM || sp.HostCpuBusy == nil || *sp.HostCpuBusy != wantHost {
			t.Errorf("%s: got VmCpuBusy=%v HostCpuBusy=%v, want (%.2f, %.2f)", label, sp.VmCpuBusy, sp.HostCpuBusy, wantVM, wantHost)
		}
	}
	assertOverlay(t0, "t=0s", true, 0.15, 0.92)
	assertOverlay(t44, "t=44s", true, 0.15, 0.92)
	assertOverlay(t45, "t=45s", false, 0, 0)
	assertOverlay(t60, "t=60s", false, 0, 0)
	assertOverlay(t70, "t=70s", true, 0.40, 0.45)
}

func TestReportHostUsageOrderingAndClockSkew(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctrlSelf := tailnet.Self{
		Peer:  tailnet.Peer{NodeID: "nCtrl", Hostname: "ctrl", Login: "ramon@"},
		Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	}
	vmPeer := tailnet.Peer{NodeID: "nVM", Hostname: "mac-worker", Login: "ramon@"}
	hostPeer := tailnet.Peer{NodeID: "nMacHost", Hostname: "ramon-mbp", Login: "ramon@"}

	tn := tailnettest.New(ctrlSelf)
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	srv := NewServer(ServerConfig{
		Store:      store,
		Identifier: tn,
		Self:       ctrlSelf.Addrs,
		Cluster:    statedir.ControllerConfig{ClusterID: "c1", Owner: "ramon@", Port: 7120},
		Now:        clock.now,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	addr := strings.TrimPrefix(httpSrv.URL, "http://")

	vmClient, _, err := api.Controller(addr, tn.Client(vmPeer))
	if err != nil {
		t.Fatal(err)
	}
	hostClient, _, err := api.Controller(addr, tn.Client(hostPeer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vmClient.Join(ctx, &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	defer srv.power.holdBy("mac-worker", hostPeer, true, 30*time.Minute)()

	// 1. Already-expired sample is rejected with INVALID_ARGS.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:   0.10,
			HostCpuBusy: 0.20,
			SampledAt:   clock.now().Add(-hostUsageTTL).UnixMilli(),
		},
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("expired sample at TTL boundary code = %s, want INVALID_ARGS", api.CodeOf(err))
	}
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:   0.10,
			HostCpuBusy: 0.20,
			SampledAt:   clock.now().Add(-hostUsageTTL - 15*time.Second).UnixMilli(),
		},
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("expired sample code = %s, want INVALID_ARGS", api.CodeOf(err))
	}

	// 2. Sample too far in the future (> maxHostClockSkew) is rejected with INVALID_ARGS.
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:   0.10,
			HostCpuBusy: 0.20,
			SampledAt:   clock.now().Add(maxHostClockSkew + time.Second).UnixMilli(),
		},
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("future-skew sample code = %s, want INVALID_ARGS", api.CodeOf(err))
	}

	// 3. A 20s-delayed report receives reduced remaining TTL (hostUsageTTL - 20s = 25s).
	delayedSampleAt := clock.now().Add(-20 * time.Second).UnixMilli()
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:          0.20,
			HostCpuBusy:        0.95,
			HostMemoryPressure: 100,
			SampledAt:          delayedSampleAt,
		},
	}); err != nil {
		t.Fatalf("20s-delayed ReportHostUsage: %v", err)
	}
	got := srv.hostUsage("mac-worker", clock.now())
	if got == nil || got.RemainingTtlMs != 25_000 {
		t.Fatalf("20s-delayed HostUsage = %+v, want RemainingTtlMs=25000", got)
	}

	// 4. Delayed older healthy report (SampledAt < newer pressure report) must NOT overwrite latest or reset TTL.
	clock.advance(5 * time.Second)
	olderHealthyAt := delayedSampleAt - 5_000
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:          0.50,
			HostCpuBusy:        0.20,
			HostMemoryPressure: 0,
			SampledAt:          olderHealthyAt,
		},
	}); err != nil {
		t.Fatalf("out-of-order ReportHostUsage: %v", err)
	}
	gotAfterOld := srv.hostUsage("mac-worker", clock.now())
	if gotAfterOld == nil {
		t.Fatal("hostUsage = nil after out-of-order older report")
	}
	if gotAfterOld.HostMemoryPressure != 100 || gotAfterOld.HostCpuBusy != 0.95 || gotAfterOld.SampledAt != delayedSampleAt {
		t.Fatalf("out-of-order older healthy report overwrote newer pressure report: %+v", gotAfterOld)
	}
	if gotAfterOld.RemainingTtlMs != 20_000 {
		t.Fatalf("RemainingTtlMs after out-of-order report = %d, want 20000 (unchanged deadline)", gotAfterOld.RemainingTtlMs)
	}

	// And the 20s-delayed report expires at its reduced deadline (20s more from now).
	clock.advance(20 * time.Second)
	if expired := srv.hostUsage("mac-worker", clock.now()); expired != nil {
		t.Fatalf("20s-delayed report still active at reduced deadline: %+v", expired)
	}

	// 5. Slightly-future report within maxHostClockSkew is accepted, capped at full hostUsageTTL (never extended).
	futureSkewAt := clock.now().Add(5 * time.Second).UnixMilli()
	if _, err := hostClient.ReportHostUsage(ctx, &pb.ReportHostUsageRequest{
		Node: "mac-worker",
		Usage: &pb.HostUsage{
			VmCpuBusy:   0.30,
			HostCpuBusy: 0.40,
			SampledAt:   futureSkewAt,
		},
	}); err != nil {
		t.Fatalf("within-skew future ReportHostUsage: %v", err)
	}
	gotSkew := srv.hostUsage("mac-worker", clock.now())
	if gotSkew == nil || gotSkew.RemainingTtlMs != hostUsageTTL.Milliseconds() {
		t.Fatalf("within-skew future HostUsage = %+v, want RemainingTtlMs=%d", gotSkew, hostUsageTTL.Milliseconds())
	}
}
