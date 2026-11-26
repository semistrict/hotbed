package worker

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// feedOf is the engine's feed events of a kind, oldest first.
func feedOf(t *testing.T, e *Engine, kind string) []string {
	t.Helper()
	evs, err := e.Events(0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Feed != nil && ev.Feed.Kind == kind {
			out = append(out, ev.Feed.Text)
		}
	}
	return out
}

func signal(a *pb.Admission, name string) *pb.AdmissionSignal {
	for _, s := range a.Signals {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// CPU pressure reaching 60% holds new work; it starts again only once
// pressure is under 30% and has not reached 60% for 30 seconds. Every hold,
// resume and wait is an event, and the heartbeat says why work waits.
func TestCPUPressureHoldsNewWork(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)
	sys := e.cfg.System.(*fake.System)

	sys.SetCPUPressure(70)
	e.Sample()
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "c-2", WorkspaceId: "w", Args: []string{"sleep", "300"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.cfg.Units.Stop(context.WithoutCancel(t.Context()), "hotbed-c-2") })
	e.admit(t.Context())
	c2 := e.commands["c-2"].cmd()
	if want := "waiting: CPU pressure 70% (peak 70% in the last 30s); the node holds new work until it is under 30%"; c2.State != queued || c2.Reason != want {
		t.Fatalf("c-2 at 70%% CPU pressure is %v: %q, want queued: %q", c2.State, c2.Reason, want)
	}
	if got, want := feedOf(t, e, "hold"), []string{"hold new work: CPU pressure 70% (peak 70% in the last 30s); new work starts again under 30%"}; !slices.Equal(got, want) {
		t.Errorf("hold events = %q, want %q", got, want)
	}
	if got := feedOf(t, e, "wait"); len(got) != 1 || !strings.HasPrefix(got[0], "wait sleep 300 · waiting: CPU pressure 70%") {
		t.Errorf("wait events = %q, want one for c-2", got)
	}
	a := e.Heartbeat("c").Admission
	cpu := signal(a, "cpu-pressure")
	if !a.Holding || a.Queued != 1 || cpu.Value != 70 || cpu.Limit != 60 || cpu.Resume != 30 || !cpu.Holding || cpu.Waiting != 1 {
		t.Errorf("admission while held = %v", a)
	}

	// Under 30%, but 70% was reached within 30 seconds: still held.
	now = now.Add(10 * time.Second)
	sys.SetCPUPressure(25)
	e.Sample()
	e.admit(t.Context())
	if s := e.state("c-2"); s != queued {
		t.Fatalf("10s after 70%%, at 25%%, c-2 is %v, want queued", s)
	}

	// Past the window, under 30%: work starts again.
	now = now.Add(25 * time.Second)
	e.Sample()
	e.admit(t.Context())
	if s := e.state("c-2"); s != running {
		t.Fatalf("35s after 70%%, at 25%%, c-2 is %v, want running", s)
	}
	if got, want := feedOf(t, e, "unhold"), []string{"start new work again after holding it 35s (cpu)"}; !slices.Equal(got, want) {
		t.Errorf("resume events = %q, want %q", got, want)
	}
	if a := e.Heartbeat("c").Admission; a.Holding || a.Reason != "" || signal(a, "cpu-pressure").Holding {
		t.Errorf("admission after resuming = %v", a)
	}

	// Between the marks, nothing holds work that is not already held.
	now = now.Add(5 * time.Second)
	sys.SetCPUPressure(50)
	e.Sample()
	if e.cpu.holding {
		t.Error("50% CPU pressure, never at 60% since resuming, holds new work")
	}
}

// cladefsd reporting host memory full (or evicting at/near its floor) holds
// new work even when the work slice itself reports 0% memory PSI — preventing
// new VMs from piling onto a node whose DAX cache is already starved.
func TestCladefsMemoryStarvationHoldsNewWork(t *testing.T) {
	const GiB = 1 << 30
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)

	// 1. cladefsd host memory full -> holds new work despite 0% slice PSI.
	e.mu.Lock()
	e.objectStore = &pb.ObjectStoreStats{
		ExtentCacheBytes:      4 * GiB,
		ExtentCacheLimitBytes: 4 * GiB,
		ExtentCacheFloorBytes: 4 * GiB,
		MemoryPressure:        true,
		MemoryFull:            true,
	}
	e.mu.Unlock()
	e.Sample()

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "c-cladefs", WorkspaceId: "w", Args: []string{"sleep", "300"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.cfg.Units.Stop(context.WithoutCancel(t.Context()), "hotbed-c-cladefs") })
	e.admit(t.Context())

	c := e.commands["c-cladefs"].cmd()
	if c.State != queued || !strings.Contains(c.Reason, "cladefsd host memory full") {
		t.Fatalf("c-cladefs while cladefsd full = %v: %q, want queued for cladefsd host memory full", c.State, c.Reason)
	}
	a := e.Heartbeat("c").Admission
	sig := signal(a, "cladefs-memory")
	if !a.Holding || sig == nil || !sig.Holding || !sig.Floor || sig.Waiting != 1 || sig.Limit != float64(4*GiB) || sig.Resume != float64(4*GiB+512<<20) {
		t.Fatalf("admission while cladefsd full = %+v (signal=%+v)", a, sig)
	}
	if holds := feedOf(t, e, "hold"); len(holds) != 1 || !strings.Contains(holds[0], "cladefsd host memory full") {
		t.Errorf("hold events = %q, want cladefsd full hold", holds)
	}

	// 2. No longer full, but still evicting within the 12.5% hysteresis band near floor -> still holds.
	now = now.Add(10 * time.Second)
	e.mu.Lock()
	e.objectStore = &pb.ObjectStoreStats{
		ExtentCacheBytes:      4 * GiB,
		ExtentCacheLimitBytes: 4*GiB + 512<<20,
		ExtentCacheFloorBytes: 4 * GiB,
		MemoryPressure:        true,
		MemoryFull:            false,
	}
	e.mu.Unlock()
	e.Sample()
	e.admit(t.Context())
	if s := e.state("c-cladefs"); s != queued {
		t.Fatalf("c-cladefs while pressured near floor = %v, want queued", s)
	}

	// 3. Routine cold-cache trimming above the 12.5% band (e.g. 25% above floor) -> NOT starved; resumes and admits!
	now = now.Add(10 * time.Second)
	e.mu.Lock()
	e.objectStore = &pb.ObjectStoreStats{
		ExtentCacheBytes:      5 * GiB,
		ExtentCacheLimitBytes: 5 * GiB,
		ExtentCacheFloorBytes: 4 * GiB,
		MemoryPressure:        true,
		MemoryFull:            false,
	}
	e.mu.Unlock()
	e.Sample()
	e.admit(t.Context())
	if s := e.state("c-cladefs"); s != running {
		t.Fatalf("c-cladefs with warm cache above 12.5%% floor band = %v, want running", s)
	}
	if unholds := feedOf(t, e, "unhold"); len(unholds) != 1 || !strings.Contains(unholds[0], "cladefs-memory") {
		t.Errorf("unhold events = %q, want cladefs-memory unhold", unholds)
	}
	if a := e.Heartbeat("c").Admission; a.Holding || signal(a, "cladefs-memory").Holding {
		t.Errorf("admission after cladefsd recovery = %+v", a)
	}
}
