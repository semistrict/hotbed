package worker

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/sysinfo"
)

// TestHostCPUStarvationHoldsWork verifies board 9281: when the Linux guest
// looks busy (92%) with low guest PSI (31%, below cpuHoldAt 60%) while the
// macOS host gives the VM only 20% of its vCPUs (host 95% busy), the worker
// holds new work — including measurements — reports host signals in admission,
// and admits queued work once the host recovers.
func TestHostCPUStarvationHoldsWork(t *testing.T) {
	now := time.Unix(1795500000, 0)
	units, sys := &fake.Units{}, fake.NewSystem()
	store := &fake.Store{Root: t.TempDir()}
	iso := fake.NewIsolation("4-7", 4)
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "vm1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: sys,
		Bench: iso, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if err := os.MkdirAll(filepath.Join(store.Path("ws/w"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.workspaces["w"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "w", Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
		Volume:    "ws/w",
	}}

	// Guest reports 92% CPU busy with 31% guest PSI (below 60% guest hold).
	sys.SetCPU(sysinfo.CPUShare{Busy: 0.92})
	sys.SetCPUPressure(31)
	e.Sample()
	if a := e.Heartbeat("c").Admission; a.Holding {
		t.Fatalf("without host usage, guest PSI 31%% should not hold: %v", a)
	}

	// Host supervisor reports the VM only gets 20% while host is 95% busy.
	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.20,
		HostCpuBusy:        0.95,
		HostLoad_1M:        14.0,
		HostCpus:           10,
		HostMemoryPressure: 0,
		SampledAt:          now.UnixMilli(),
	})

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "work-1", WorkspaceId: "w", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-work-1") })
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "bench-1", WorkspaceId: "w", Args: []string{"sleep", "60"}, Bench: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-bench-1") })

	e.admit(t.Context())

	w1, _ := e.Command("work-1")
	if w1.State != queued || !strings.Contains(w1.Reason, "host CPU starvation") {
		t.Fatalf("work-1 under host CPU starvation = %v (%q), want queued with host CPU starvation", w1.State, w1.Reason)
	}
	b1, _ := e.Command("bench-1")
	if b1.State != queued || !strings.Contains(b1.Reason, "host CPU starvation") {
		t.Fatalf("bench-1 under host CPU starvation = %v (%q), want queued with host CPU starvation", b1.State, b1.Reason)
	}

	a := e.Heartbeat("c").Admission
	if !a.Holding || !strings.Contains(a.Reason, "host CPU starvation") || a.Queued != 2 {
		t.Fatalf("admission while host starved = %+v", a)
	}
	hcpu := signal(a, "host-cpu-starvation")
	hmem := signal(a, "host-memory-pressure")
	if hcpu == nil || !hcpu.Holding || hcpu.Unit != "%" || hcpu.Waiting != 2 {
		t.Errorf("host-cpu-starvation signal = %+v, want holding with waiting=2", hcpu)
	}
	if hmem == nil || hmem.Holding || hmem.Limit != 100 || hmem.Unit != "%" {
		t.Errorf("host-memory-pressure signal = %+v, want present, not holding, limit 100%%", hmem)
	}

	// Hysteresis: still above resume threshold (host CPU 78% > 70%), stays held.
	now = now.Add(5 * time.Second)
	sys.SetCPU(sysinfo.CPUShare{Busy: 0.40})
	e.Sample()
	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.35,
		HostCpuBusy:        0.78,
		HostLoad_1M:        6.0,
		HostCpus:           10,
		HostMemoryPressure: 0,
		SampledAt:          now.UnixMilli(),
	})
	e.admit(t.Context())
	if s := e.state("work-1"); s != queued {
		t.Fatalf("at 78%% host CPU (hysteresis band), work-1 is %v, want queued", s)
	}

	// Host recovers cleanly -> hold clears, stepSoon is signalled, work admits.
	now = now.Add(5 * time.Second)
	// Drain any pending stepc notification before SetHostUsage.
	select {
	case <-e.stepc:
	default:
	}
	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.35,
		HostCpuBusy:        0.40,
		HostLoad_1M:        3.0,
		HostCpus:           10,
		HostMemoryPressure: 0,
		SampledAt:          now.UnixMilli(),
	})
	select {
	case <-e.stepc:
	default:
		t.Error("SetHostUsage clearing a hold did not wake stepc for immediate admission")
	}

	e.admit(t.Context())
	if s := e.state("work-1"); s != running {
		t.Fatalf("after host recovery, work-1 is %v, want running", s)
	}
	if a := e.Heartbeat("c").Admission; a.Holding || signal(a, "host-cpu-starvation").Holding {
		t.Errorf("admission after host recovery = %+v", a)
	}
	if unholds := feedOf(t, e, "unhold"); len(unholds) != 1 || !strings.Contains(unholds[0], "host-cpu") {
		t.Errorf("unhold events = %q, want one for host-cpu", unholds)
	}
}

// TestIdleNestedVMIsNotHostCPUStarvation: an idle Firecracker microVM
// inside the worker VM keeps its vCPU threads in KVM_RUN, so the guest
// kernel counts them busy while the macOS host, seeing them wait, gives the
// VM little. That gap is not steal: with the guest 98% busy, 80% of it in a
// nested VM, and the host giving the VM 22%, new work must still start. The
// same guest busy outside nested VMs is starvation.
func TestIdleNestedVMIsNotHostCPUStarvation(t *testing.T) {
	e, units, sys, clk := parkEngine(t)
	starved := &pb.HostUsage{
		VmCpuBusy:   0.22,
		HostCpuBusy: 0.24,
		HostLoad_1M: 6.3,
		HostCpus:    15,
		SampledAt:   clk.UnixMilli(),
	}

	sys.SetCPU(sysinfo.CPUShare{Busy: 0.98, Nested: 0.80})
	e.Sample()
	e.SetHostUsage(starved)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "nested-1", WorkspaceId: "a", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-nested-1") })
	e.admit(t.Context())
	if c, _ := e.Command("nested-1"); c.State != running {
		t.Fatalf("with an idle nested VM, nested-1 is %v (%q), want running", c.State, c.Reason)
	}
	if hcpu := signal(e.Heartbeat("c").Admission, "host-cpu-starvation"); hcpu.Holding {
		t.Errorf("host-cpu-starvation with an idle nested VM = %+v, want not holding", hcpu)
	}

	sys.SetCPU(sysinfo.CPUShare{Busy: 0.98})
	e.Sample()
	e.SetHostUsage(starved)
	hcpu := signal(e.Heartbeat("c").Admission, "host-cpu-starvation")
	if !hcpu.Holding || math.Round(hcpu.Value) != 76 {
		t.Errorf("host-cpu-starvation with the guest's own work 98%% busy = %+v, want holding at 76%% steal", hcpu)
	}
}

// TestHostMemoryPressureHoldsWork verifies that macOS warn (50%) does NOT
// hard-hold new work (leaving soft placement preference to the controller),
// while critical host_memory_pressure (100%) holds new work with signal
// "host-memory-pressure" (limit 100, unit "%") and recovers when pressure drops.
func TestHostMemoryPressureHoldsWork(t *testing.T) {
	now := time.Unix(1795500000, 0)
	e, units, _, clk := parkEngine(t)
	*clk = now

	// macOS warn (50%) must NOT hard-hold new work.
	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.10,
		HostCpuBusy:        0.20,
		HostLoad_1M:        2.0,
		HostCpus:           8,
		HostMemoryPressure: 50, // macOS warn
		SampledAt:          clk.UnixMilli(),
	})
	if a := e.Heartbeat("c").Admission; a.Holding {
		t.Fatalf("macOS warn (50%%) must not hard-hold admission: %+v", a)
	} else if hmem := signal(a, "host-memory-pressure"); hmem == nil || hmem.Holding || hmem.Value != 50 || hmem.Limit != 100 {
		t.Fatalf("warn host-memory-pressure signal = %+v, want value=50 limit=100 holding=false", hmem)
	}

	// macOS critical (100%) MUST hard-hold new work.
	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.10,
		HostCpuBusy:        0.20,
		HostLoad_1M:        2.0,
		HostCpus:           8,
		HostMemoryPressure: 100, // macOS critical
		SampledAt:          clk.UnixMilli(),
	})

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "mem-1", WorkspaceId: "a", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-mem-1") })

	e.admit(t.Context())
	c, _ := e.Command("mem-1")
	if c.State != queued || !strings.Contains(c.Reason, "host memory pressure") {
		t.Fatalf("mem-1 under host memory pressure 100%% = %v (%q), want queued", c.State, c.Reason)
	}
	a := e.Heartbeat("c").Admission
	hmem := signal(a, "host-memory-pressure")
	if !a.Holding || hmem == nil || !hmem.Holding || hmem.Value != 100 || hmem.Limit != 100 || hmem.Unit != "%" || hmem.Waiting != 1 {
		t.Fatalf("admission under host memory pressure = %+v (signal=%+v)", a, hmem)
	}

	// Host memory pressure drops back to warn (50%) -> hold clears and command starts.
	*clk = clk.Add(10 * time.Second)
	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.10,
		HostCpuBusy:        0.20,
		HostLoad_1M:        2.0,
		HostCpus:           8,
		HostMemoryPressure: 50,
		SampledAt:          clk.UnixMilli(),
	})
	e.admit(t.Context())
	if s := e.state("mem-1"); s != running {
		t.Fatalf("after host memory pressure cleared from critical, mem-1 is %v, want running", s)
	}
}

// TestHostLoadStarvationHoldsWork verifies that high host 1-minute load
// (>= 1.25 * host_cpus) triggers host-cpu admission hold even when host_cpu_busy
// is below 85%.
func TestHostLoadStarvationHoldsWork(t *testing.T) {
	e, units, _, clk := parkEngine(t)

	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.20,
		HostCpuBusy:        0.65,
		HostLoad_1M:        10.5, // 1.31x of 8 CPUs >= 1.25x
		HostCpus:           8,
		HostMemoryPressure: 0,
		SampledAt:          clk.UnixMilli(),
	})

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "load-1", WorkspaceId: "a", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-load-1") })
	e.admit(t.Context())
	if s := e.state("load-1"); s != queued {
		t.Fatalf("load-1 at host load 10.5/8 = %v, want queued", s)
	}
	if a := e.Heartbeat("c").Admission; !a.Holding || !signal(a, "host-cpu-starvation").Holding {
		t.Fatalf("admission at high host load = %+v", a)
	}
}

// TestStaleHostUsageStopsHolding verifies that a host starvation signal older
// than hostUsageTTL (45s) is treated as stale, stops holding, disappears from
// admission signals, and allows queued commands to start.
func TestStaleHostUsageStopsHolding(t *testing.T) {
	e, units, _, clk := parkEngine(t)

	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.10,
		HostCpuBusy:        0.95,
		HostLoad_1M:        16.0,
		HostCpus:           8,
		HostMemoryPressure: 100,
		SampledAt:          clk.UnixMilli(),
	})

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "stale-1", WorkspaceId: "a", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-stale-1") })
	e.admit(t.Context())
	if s := e.state("stale-1"); s != queued {
		t.Fatalf("stale-1 with fresh starved host usage = %v, want queued", s)
	}

	// Still within TTL (40s <= 45s): still holding.
	*clk = clk.Add(40 * time.Second)
	e.admit(t.Context())
	if s := e.state("stale-1"); s != queued {
		t.Fatalf("at 40s (within TTL), stale-1 = %v, want queued", s)
	}

	// Past TTL (46s > 45s): stops holding immediately even before next SetHostUsage.
	*clk = clk.Add(6 * time.Second)
	if a := e.Heartbeat("c").Admission; a.Holding || signal(a, "host-cpu-starvation") != nil || signal(a, "host-memory-pressure") != nil {
		t.Fatalf("past TTL, Heartbeat admission should not hold or include stale host signals: %+v", a)
	}
	e.admit(t.Context())
	if s := e.state("stale-1"); s != running {
		t.Fatalf("past TTL, stale-1 = %v, want running", s)
	}
	if unholds := feedOf(t, e, "unhold"); len(unholds) != 1 {
		t.Errorf("unhold events after TTL expiry = %q, want 1", unholds)
	}
}

type fakeHostHeartbeatCluster struct {
	hotbedv1connect.ClusterServiceClient
	reply *pb.HeartbeatResponse
}

func (f *fakeHostHeartbeatCluster) Heartbeat(_ context.Context, _ *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return f.reply, nil
}

// TestHeartbeatOnceAppliesHostUsage verifies that HeartbeatOnce feeds
// HeartbeatResponse.HostUsage into Engine.SetHostUsage.
func TestHeartbeatOnceAppliesHostUsage(t *testing.T) {
	e, _, _, clk := parkEngine(t)
	cl := &fakeHostHeartbeatCluster{
		reply: &pb.HeartbeatResponse{
			HostUsage: &pb.HostUsage{
				HostCpuBusy:        0.92,
				HostMemoryPressure: 100,
				SampledAt:          clk.UnixMilli(),
			},
		},
	}
	if err := HeartbeatOnce(t.Context(), e, cl, "cluster-1", nil); err != nil {
		t.Fatal(err)
	}
	a := e.Heartbeat("cluster-1").Admission
	if !a.Holding || signal(a, "host-memory-pressure") == nil || !signal(a, "host-memory-pressure").Holding {
		t.Fatalf("admission after HeartbeatOnce with starved HostUsage = %+v", a)
	}

	// Heartbeat with nil HostUsage clears it.
	cl.reply = &pb.HeartbeatResponse{}
	if err := HeartbeatOnce(t.Context(), e, cl, "cluster-1", nil); err != nil {
		t.Fatal(err)
	}
	a = e.Heartbeat("cluster-1").Admission
	if a.Holding || signal(a, "host-memory-pressure") != nil {
		t.Fatalf("admission after HeartbeatOnce with nil HostUsage = %+v", a)
	}
}

// TestHostUsageHonorsRemainingTTLMs verifies Finding #215 (board 9661):
// worker admission expires a HostUsage sample at its controller-supplied
// RemainingTtlMs deadline (capped at hostUsageTTL) rather than resetting a
// fresh 45s TTL on every heartbeat.
func TestHostUsageHonorsRemainingTTLMs(t *testing.T) {
	e, units, _, clk := parkEngine(t)

	// Controller says only 1000ms (1s) remain on this starved host sample.
	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.10,
		HostCpuBusy:        0.95,
		HostLoad_1M:        16.0,
		HostCpus:           8,
		HostMemoryPressure: 50,
		SampledAt:          clk.UnixMilli(),
		RemainingTtlMs:     1000,
	})

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "ttl-1", WorkspaceId: "a", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-ttl-1") })
	e.admit(t.Context())
	if s := e.state("ttl-1"); s != queued {
		t.Fatalf("at t=0 with RemainingTtlMs=1000, ttl-1 = %v, want queued", s)
	}

	// At +900ms, still within the 1s remaining TTL: still holding.
	*clk = clk.Add(900 * time.Millisecond)
	e.admit(t.Context())
	if s := e.state("ttl-1"); s != queued {
		t.Fatalf("at +900ms (within 1s remaining TTL), ttl-1 = %v, want queued", s)
	}

	// Another heartbeat arrives at +900ms with RemainingTtlMs=100 (original
	// controller deadline is still +1000ms, NOT reset to +45s!).
	e.SetHostUsage(&pb.HostUsage{
		VmCpuBusy:          0.10,
		HostCpuBusy:        0.95,
		HostLoad_1M:        16.0,
		HostCpus:           8,
		HostMemoryPressure: 50,
		SampledAt:          clk.Add(-44 * time.Second).UnixMilli(),
		RemainingTtlMs:     100,
	})
	*clk = clk.Add(50 * time.Millisecond) // +950ms total
	e.admit(t.Context())
	if s := e.state("ttl-1"); s != queued {
		t.Fatalf("at +950ms (50ms left), ttl-1 = %v, want queued", s)
	}

	// At +1001ms total (past 1s original deadline, well before 45s): expired!
	*clk = clk.Add(51 * time.Millisecond)
	if a := e.Heartbeat("c").Admission; a.Holding || signal(a, "host-memory-pressure") != nil || signal(a, "host-cpu-starvation") != nil {
		t.Fatalf("past RemainingTtlMs deadline (+1001ms), admission should not hold or include stale host signals: %+v", a)
	}
	e.admit(t.Context())
	if s := e.state("ttl-1"); s != running {
		t.Fatalf("past RemainingTtlMs deadline (+1001ms), ttl-1 = %v, want running", s)
	}

	// Negative RemainingTtlMs immediately clears any stored host usage.
	e.SetHostUsage(&pb.HostUsage{
		HostMemoryPressure: 100,
		SampledAt:          clk.UnixMilli(),
		RemainingTtlMs:     5000,
	})
	if !e.Heartbeat("c").Admission.Holding {
		t.Fatal("expected hold with RemainingTtlMs=5000")
	}
	e.SetHostUsage(&pb.HostUsage{
		HostMemoryPressure: 100,
		SampledAt:          clk.UnixMilli(),
		RemainingTtlMs:     -1,
	})
	if a := e.Heartbeat("c").Admission; a.Holding || signal(a, "host-memory-pressure") != nil {
		t.Fatalf("after negative RemainingTtlMs, admission = %+v, want cleared", a)
	}

	// RemainingTtlMs larger than hostUsageTTL is clamped to hostUsageTTL (45s).
	e.SetHostUsage(&pb.HostUsage{
		HostMemoryPressure: 100,
		SampledAt:          clk.UnixMilli(),
		RemainingTtlMs:     int64((5 * time.Minute).Milliseconds()),
	})
	*clk = clk.Add(hostUsageTTL + time.Second)
	if a := e.Heartbeat("c").Admission; a.Holding || signal(a, "host-memory-pressure") != nil {
		t.Fatalf("RemainingTtlMs > hostUsageTTL should still expire after hostUsageTTL: %+v", a)
	}
}
