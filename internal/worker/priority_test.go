package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// reclaimingUnits wraps fake.Units to simulate Linux cgroup memory.reclaim
// pushing anonymous RAM to swap, with optional error injection, partial-reclaim
// floor (leaveAnon), and physical memory accounting on sys.
type reclaimingUnits struct {
	*fake.Units
	mu         sync.Mutex
	reclaimErr error
	leaveAnon  int64
	sys        *fake.System
}

func (u *reclaimingUnits) Reclaim(cgroup string, n int64) error {
	u.mu.Lock()
	err := u.reclaimErr
	leave := u.leaveAnon
	sys := u.sys
	u.mu.Unlock()
	if err != nil {
		return err
	}
	m, merr := u.Units.Memory(cgroup)
	if merr != nil {
		return merr
	}
	reclaimable := max(int64(0), m.Anon-leave)
	moved := min(n, reclaimable)
	if moved <= 0 {
		return nil
	}
	name := strings.TrimSuffix(filepath.Base(cgroup), ".service")
	u.Units.SetAnon(name, m.Anon-moved)
	u.Units.SetSwap(name, m.Swap+moved)
	if sys != nil {
		cur, _ := sys.Meminfo()
		sys.SetUsed(max(int64(0), cur.Used()-moved))
	}
	return nil
}

func parkReclaimEngine(t *testing.T) (*Engine, *reclaimingUnits, *fake.System, *time.Time) {
	t.Helper()
	baseUnits := &fake.Units{}
	sys := fake.NewSystem()
	units := &reclaimingUnits{Units: baseUnits}
	store := &fake.Store{Root: t.TempDir()}
	now := time.Unix(1795400000, 0)
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: sys, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	for _, w := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle}, Volume: "ws/" + w}}
	}
	return e, units, sys, &now
}

func startWithLimits(t *testing.T, e *Engine, units *fake.Units, now *time.Time, id, ws, kind string, limits *pb.CommandLimits) *command {
	t.Helper()
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: id, WorkspaceId: ws, Args: []string{"sleep", "60"}, Kind: kind, Limits: limits,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+id) })
	e.step(t.Context())
	if s := e.state(id); s != running {
		t.Fatalf("%s is %v (%q), want running", id, s, e.commands[id].cmd().Reason)
	}
	return e.commands[id]
}

// TestHighPriorityPreemptsLowerAndThawsOnCompletion verifies board 7907:
// a higher-priority queued command preempts (parks/freezes) a lower-priority
// running command when memory or CPU is full, runs to completion, and then
// automatically thaws the lower-priority command.
func TestHighPriorityPreemptsLowerAndThawsOnCompletion(t *testing.T) {
	t.Run("memory reservation full", func(t *testing.T) {
		e, units, _, now := parkReclaimEngine(t)
		low := startWithLimits(t, e, units.Units, now, "low-1", "a", "build", &pb.CommandLimits{Priority: -10})
		// low-1 holds 14 GiB of the 16 GiB node (past 13.76 GiB admitLimit).
		units.SetAnon("hotbed-low-1", 14<<30)
		*now = now.Add(time.Second)
		e.step(t.Context())

		// Queue a high-priority command needing 4 GiB.
		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "high-1", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "build",
			Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 4 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-1") })

		e.step(t.Context())

		if s := e.state("high-1"); s != running {
			t.Fatalf("high-1 = %v (%q), want running after preempting low-1", s, e.commands["high-1"].cmd().Reason)
		}
		gotLow, _ := e.Command("low-1")
		if gotLow.State != parked {
			t.Fatalf("low-1 = %v, want parked", gotLow.State)
		}
		if !strings.Contains(gotLow.Reason, "preempted by higher-priority command high-1 (priority 10 > -10)") {
			t.Errorf("low-1 reason = %q, want preemption message", gotLow.Reason)
		}
		if !e.frozen(t, "hotbed-low-1") {
			t.Error("low-1 unit is not frozen")
		}

		// Even across more ticks (and even past rotateAfter!) while high-1 is
		// still running, low-1 must stay parked and must NOT rotate out high-1.
		*now = now.Add(rotateAfter + time.Minute)
		e.step(t.Context())
		if sLow, sHigh := e.state("low-1"), e.state("high-1"); sLow != parked || sHigh != running {
			t.Fatalf("while high-1 runs past rotateAfter: low-1=%v high-1=%v, want parked/running", sLow, sHigh)
		}

		// high-1 completes -> low-1 automatically thaws!
		if _, err := e.Stop(t.Context(), "high-1", "done"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		e.step(t.Context())
		if s := low.cmd().State; s != running {
			t.Fatalf("after high-1 ended, low-1 = %v (%q), want thawed/running", s, low.cmd().Reason)
		}
		if e.frozen(t, "hotbed-low-1") {
			t.Error("low-1 unit is still frozen after thaw")
		}
	})

	t.Run("CPU pressure hold", func(t *testing.T) {
		e, units, sys, now := parkEngine(t)
		_ = startWithLimits(t, e, units, now, "low-cpu", "a", "build", &pb.CommandLimits{Priority: -10})

		sys.SetCPUPressure(75)
		e.Sample()
		if !e.Heartbeat("c").Admission.Holding {
			t.Fatal("expected node to hold new work at 75% CPU pressure")
		}

		// Normal-priority (0 > -10) command arrives under CPU pressure -> preempts low-cpu!
		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "norm-cpu", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "test",
			Limits: &pb.CommandLimits{Priority: 0},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-norm-cpu") })

		e.step(t.Context())
		if sNorm, sLow := e.state("norm-cpu"), e.state("low-cpu"); sNorm != running || sLow != parked {
			t.Fatalf("under CPU pressure: norm-cpu=%v low-cpu=%v, want running/parked", sNorm, sLow)
		}

		// While CPU pressure is still high and norm-cpu runs, low-cpu stays parked.
		*now = now.Add(time.Second)
		e.step(t.Context())
		if s := e.state("low-cpu"); s != parked {
			t.Fatalf("while norm-cpu runs under CPU pressure, low-cpu=%v, want parked", s)
		}

		// Once norm-cpu ends, low-cpu thaws (nothing left running).
		if _, err := e.Stop(t.Context(), "norm-cpu", "done"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		e.step(t.Context())
		if s := e.state("low-cpu"); s != running {
			t.Fatalf("after norm-cpu ended, low-cpu=%v, want running", s)
		}
	})
}

// TestPriorityPreemptionVictimOrderAndBenchProtected verifies:
//   - Lowest priority running command is parked first (leaving higher-priority
//     siblings running when freeing the lowest is enough).
//   - Bench measurements are NEVER preempted, even by urgent commands.
//   - If parking all lower-priority commands still wouldn't leave room for the
//     queued high-priority command (e.g. a bench holds RAM), lower-priority
//     commands are NOT needlessly parked.
func TestPriorityPreemptionVictimOrderAndBenchProtected(t *testing.T) {
	now := time.Unix(1795600000, 0)
	baseUnits := &fake.Units{}
	units, sys := &reclaimingUnits{Units: baseUnits}, fake.NewSystem()
	store := &fake.Store{Root: t.TempDir()}
	iso := fake.NewIsolation("4-7", 4)
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: sys,
		Bench: iso, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	for _, w := range []string{"a", "b", "c", "d"} {
		if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
			Volume:    "ws/" + w,
		}}
	}

	// Start low (-10, 8 GiB) and mid (0, 4 GiB) -> total 12 GiB of 13.76 GiB.
	_ = startWithLimits(t, e, baseUnits, &now, "mid-1", "a", "build", &pb.CommandLimits{Priority: 0})
	units.SetAnon("hotbed-mid-1", 4<<30)
	_ = startWithLimits(t, e, baseUnits, &now, "low-1", "b", "build", &pb.CommandLimits{Priority: -10})
	units.SetAnon("hotbed-low-1", 8<<30)
	now = now.Add(time.Second)
	e.step(t.Context())

	// high-1 (priority 10) needs 6 GiB: parking low-1 (8 GiB) leaves 9.76 GiB
	// (>= 6 GiB), so ONLY low-1 should be parked and mid-1 must keep running.
	now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "high-1", WorkspaceId: "c", Args: []string{"sleep", "60"}, Kind: "build",
		Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 6 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-1") })
	e.step(t.Context())

	if sLow, sMid, sHigh := e.state("low-1"), e.state("mid-1"), e.state("high-1"); sLow != parked || sMid != running || sHigh != running {
		t.Fatalf("after high-1 admission: low-1=%v mid-1=%v high-1=%v, want parked/running/running", sLow, sMid, sHigh)
	}

	// Clean up and test bench protection.
	for _, id := range []string{"low-1", "mid-1", "high-1"} {
		if _, err := e.Stop(t.Context(), id, "cleanup"); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Second)
	e.step(t.Context())

	// Start a low-priority bench measurement holding 10 GiB and a low-priority build holding 2 GiB.
	now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "bench-low", WorkspaceId: "a", Args: []string{"sleep", "60"}, Bench: true,
		Limits: &pb.CommandLimits{Priority: -10},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-bench-low") })
	e.step(t.Context())
	units.SetAnon("hotbed-bench-low", 10<<30)
	_ = startWithLimits(t, e, baseUnits, &now, "build-low", "b", "build", &pb.CommandLimits{Priority: -10})
	units.SetAnon("hotbed-build-low", 2<<30)
	now = now.Add(time.Second)
	e.step(t.Context())

	// An urgent (priority 20) command needing 6 GiB cannot fit beside the 10 GiB
	// bench even if build-low (2 GiB) were parked (13.76 - 10 = 3.76 < 6).
	// Bench MUST NOT be preempted, and build-low MUST NOT be needlessly parked.
	now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "urgent-1", WorkspaceId: "c", Args: []string{"sleep", "60"}, Kind: "build",
		Limits: &pb.CommandLimits{Priority: 20, MemoryLowBytes: 6 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-urgent-1") })
	e.step(t.Context())

	if sBench, sBuild, sUrgent := e.state("bench-low"), e.state("build-low"), e.state("urgent-1"); sBench != running || sBuild != running || sUrgent != queued {
		t.Fatalf("with bench holding 10 GiB: bench-low=%v build-low=%v urgent-1=%v, want running/running/queued", sBench, sBuild, sUrgent)
	}
}

// TestLowerPriorityParkedStaysParkedWhileHigherQueued verifies that a parked
// lower-priority command is NOT thawed while a strictly higher-priority command
// is queued waiting for room.
func TestLowerPriorityParkedStaysParkedWhileHigherQueued(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	_ = startWithLimits(t, e, units, now, "peer-1", "a", "build", &pb.CommandLimits{Priority: 10})
	low := startWithLimits(t, e, units, now, "low-1", "b", "build", &pb.CommandLimits{Priority: -10})

	// Park low-1 via memory pressure.
	sys.SetUsed(sys.Mem.Total * 95 / 100)
	*now = now.Add(time.Second)
	e.step(t.Context())
	if s := low.cmd().State; s != parked {
		t.Fatalf("low-1 at 95%% memory = %v, want parked", s)
	}

	// peer-1 holds 10 GiB; queue high-2 (priority 10) needing 6 GiB (cannot fit yet).
	units.SetAnon("hotbed-peer-1", 10<<30)
	sys.SetUsed(sys.Mem.Total * 30 / 100)
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "high-2", WorkspaceId: "c", Args: []string{"sleep", "60"}, Kind: "build",
		Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 6 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-2") })

	e.step(t.Context())
	if sHigh, sLow := e.state("high-2"), e.state("low-1"); sHigh != queued || sLow != parked {
		t.Fatalf("with high-2 queued: high-2=%v low-1=%v, want queued/parked", sHigh, sLow)
	}
}

// TestSamePriorityDoesNotPreempt verifies that commands of equal priority do
// not preempt each other and follow normal FIFO / memory rules.
func TestSamePriorityDoesNotPreempt(t *testing.T) {
	e, units, _, now := parkEngine(t)
	_ = startWithLimits(t, e, units, now, "norm-1", "a", "build", &pb.CommandLimits{Priority: 10})
	units.SetAnon("hotbed-norm-1", 14<<30)
	*now = now.Add(time.Second)
	e.step(t.Context())

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "norm-2", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "build",
		Limits: &pb.CommandLimits{Priority: 10},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-norm-2") })
	e.step(t.Context())

	if s1, s2 := e.state("norm-1"), e.state("norm-2"); s1 != running || s2 != queued {
		t.Fatalf("same priority: norm-1=%v norm-2=%v, want running/queued", s1, s2)
	}
}

// TestQueueAdmissionOrdersByPriorityThenFIFO verifies that higher-priority
// queued commands are admitted ahead of earlier-queued lower-priority commands.
func TestQueueAdmissionOrdersByPriorityThenFIFO(t *testing.T) {
	e, units, _, now := parkEngine(t)
	// Queue low first (t+1), then high (t+2), each wanting 8 GiB on a 16 GiB
	// node (admitLimit 13.76 GiB, so only one can start at once).
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "q-low", WorkspaceId: "a", Args: []string{"sleep", "60"},
		Limits: &pb.CommandLimits{Priority: -10, MemoryLowBytes: 8 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-q-low") })

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "q-high", WorkspaceId: "b", Args: []string{"sleep", "60"},
		Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 8 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-q-high") })

	e.admit(t.Context())
	if sHigh, sLow := e.state("q-high"), e.state("q-low"); sHigh != running || sLow != queued {
		t.Fatalf("queue admission order: q-high=%v q-low=%v, want running/queued", sHigh, sLow)
	}
}

// TestMemoryPressureParksLowestPriorityAndThawsHighestFirst verifies that
// memory-pressure parking and thawing respect priority ordering.
func TestMemoryPressureParksLowestPriorityAndThawsHighestFirst(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	// Start low first (older), then high (newer), then urgent (newest).
	_ = startWithLimits(t, e, units, now, "p-low", "a", "build", &pb.CommandLimits{Priority: -10})
	_ = startWithLimits(t, e, units, now, "p-norm", "b", "build", &pb.CommandLimits{Priority: 0})
	_ = startWithLimits(t, e, units, now, "p-high", "c", "build", &pb.CommandLimits{Priority: 10})

	// High memory -> lowest priority (p-low) is parked first despite being oldest!
	sys.SetUsed(sys.Mem.Total * 95 / 100)
	*now = now.Add(time.Second)
	e.step(t.Context())
	if e.state("p-low") != parked || e.state("p-norm") != running || e.state("p-high") != running {
		t.Fatalf("first park: low=%v norm=%v high=%v, want parked/running/running",
			e.state("p-low"), e.state("p-norm"), e.state("p-high"))
	}

	// Still at 95% -> p-norm is parked next, leaving p-high running.
	*now = now.Add(time.Second)
	e.step(t.Context())
	if e.state("p-norm") != parked || e.state("p-high") != running {
		t.Fatalf("second park: norm=%v high=%v, want parked/running", e.state("p-norm"), e.state("p-high"))
	}

	// Memory drops -> highest-priority parked command (p-norm, 0 > -10) thaws first!
	sys.SetUsed(sys.Mem.Total * 30 / 100)
	*now = now.Add(time.Second)
	e.step(t.Context())
	if e.state("p-norm") != running || e.state("p-low") != parked {
		t.Fatalf("first thaw: norm=%v low=%v, want running/parked", e.state("p-norm"), e.state("p-low"))
	}

	// Next step -> p-low thaws too.
	*now = now.Add(time.Second)
	e.step(t.Context())
	if e.state("p-low") != running {
		t.Fatalf("second thaw: low=%v, want running", e.state("p-low"))
	}
}

// TestPriorityDoesNotBypassHostStarvationAndRejectsOutOfRange verifies that
// urgent commands cannot preempt past host starvation and that Exec rejects
// out-of-range priorities.
func TestPriorityDoesNotBypassHostStarvationAndRejectsOutOfRange(t *testing.T) {
	e, units, _, now := parkEngine(t)

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "bad-hi", WorkspaceId: "a", Args: []string{"sleep", "1"},
		Limits: &pb.CommandLimits{Priority: 101},
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("priority 101 err = %v, want INVALID_ARGS", err)
	}
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "bad-lo", WorkspaceId: "a", Args: []string{"sleep", "1"},
		Limits: &pb.CommandLimits{Priority: -101},
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("priority -101 err = %v, want INVALID_ARGS", err)
	}

	_ = startWithLimits(t, e, units, now, "low-1", "a", "build", &pb.CommandLimits{Priority: -10})
	e.SetHostUsage(&pb.HostUsage{
		HostCpuBusy: 0.95,
		HostCpus:    8,
		SampledAt:   now.UnixMilli(),
	})

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "urgent-1", WorkspaceId: "b", Args: []string{"sleep", "60"},
		Limits: &pb.CommandLimits{Priority: 20},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-urgent-1") })
	e.step(t.Context())

	if sUrgent, sLow := e.state("urgent-1"), e.state("low-1"); sUrgent != queued || sLow != running {
		t.Fatalf("under host starvation: urgent-1=%v low-1=%v, want queued/running", sUrgent, sLow)
	}
}

// TestPreemptionKeepsChargingUnreclaimedOrPartialResidentMemory verifies
// Finding #210 (board 9661): a parked strictly lower-priority command MUST keep
// charging its resident/unreclaimed memory when Reclaim fails, is partial, or
// is skipped (e.g. I/O-held beside a measurement), and physical headroom is
// verified before admitting a preemptor.
func TestPreemptionKeepsChargingUnreclaimedOrPartialResidentMemory(t *testing.T) {
	t.Run("reclaim error keeps 15 GiB charged and blocks 4 GiB preemptor", func(t *testing.T) {
		e, units, sys, now := parkReclaimEngine(t)
		units.sys = sys
		units.reclaimErr = errors.New("memory.reclaim: EPERM")

		_ = startWithLimits(t, e, units.Units, now, "low-1", "a", "build", &pb.CommandLimits{Priority: -10})
		units.SetAnon("hotbed-low-1", 15<<30)
		sys.SetUsed(15 << 30)
		*now = now.Add(time.Second)
		e.step(t.Context())

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "high-1", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "build",
			Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 4 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-1") })

		e.step(t.Context())
		if sHigh, sLow := e.state("high-1"), e.state("low-1"); sHigh != queued || sLow != parked {
			t.Fatalf("when reclaim fails: high-1=%v (%q) low-1=%v, want queued/parked",
				sHigh, e.commands["high-1"].cmd().Reason, sLow)
		}
		e.mu.Lock()
		reserved := e.reservedFor(10)
		e.mu.Unlock()
		if reserved < 15<<30 {
			t.Fatalf("reservedFor(10) after failed reclaim = %s, want >= 15.0 GiB", gib(reserved))
		}
	})

	t.Run("partial reclaim leaving 11 GiB resident blocks 4 GiB preemptor", func(t *testing.T) {
		e, units, sys, now := parkReclaimEngine(t)
		units.sys = sys
		units.leaveAnon = 11 << 30 // 13.76 - 11 = 2.76 GiB < 4 GiB

		_ = startWithLimits(t, e, units.Units, now, "low-1", "a", "build", &pb.CommandLimits{Priority: -10})
		units.SetAnon("hotbed-low-1", 15<<30)
		sys.SetUsed(15 << 30)
		*now = now.Add(time.Second)
		e.step(t.Context())

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "high-1", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "build",
			Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 4 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-1") })

		e.step(t.Context())
		if sHigh, sLow := e.state("high-1"), e.state("low-1"); sHigh != queued || sLow != parked {
			t.Fatalf("after partial reclaim (11 GiB left): high-1=%v (%q) low-1=%v, want queued/parked",
				sHigh, e.commands["high-1"].cmd().Reason, sLow)
		}
		e.mu.Lock()
		reserved := e.reservedFor(10)
		e.mu.Unlock()
		if reserved != 11<<30 {
			t.Fatalf("reservedFor(10) after partial reclaim = %s, want 11.0 GiB", gib(reserved))
		}
	})

	t.Run("skipped reclaim on I/O-held parked command keeps charging full reservation", func(t *testing.T) {
		e, units, _, now := parkReclaimEngine(t)
		low := startWithLimits(t, e, units.Units, now, "low-held", "a", "build", &pb.CommandLimits{Priority: -10})
		units.SetAnon("hotbed-low-held", 12<<30)
		*now = now.Add(time.Second)
		e.step(t.Context())

		// Simulate holdLoud: frozen without calling Reclaim (reclaimed=false).
		e.mu.Lock()
		low.cmd().State = parked
		low.rec.HeldFor = "bench-1"
		low.rec.ParkedAt = e.nowMs()
		low.reclaimed = false
		e.mu.Unlock()

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "high-1", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "build",
			Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 4 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-1") })

		e.admit(t.Context())
		if s := e.state("high-1"); s != queued {
			t.Fatalf("high-1 beside unreclaimed I/O-held 12 GiB victim = %v, want queued", s)
		}
	})

	t.Run("reclaim dropping resident to 512 MiB admits 4 GiB preemptor", func(t *testing.T) {
		e, units, sys, now := parkReclaimEngine(t)
		units.sys = sys
		units.leaveAnon = 512 << 20

		_ = startWithLimits(t, e, units.Units, now, "low-1", "a", "build", &pb.CommandLimits{Priority: -10})
		units.SetAnon("hotbed-low-1", 15<<30)
		sys.SetUsed(15 << 30)
		*now = now.Add(time.Second)
		e.step(t.Context())

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "high-1", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "build",
			Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 4 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-1") })

		e.step(t.Context())
		if sHigh, sLow := e.state("high-1"), e.state("low-1"); sHigh != running || sLow != parked {
			t.Fatalf("after reclaim dropped resident to 512 MiB: high-1=%v (%q) low-1=%v, want running/parked",
				sHigh, e.commands["high-1"].cmd().Reason, sLow)
		}
		e.mu.Lock()
		reservedLow := e.commands["low-1"].parkedReservationForHigherPriority()
		e.mu.Unlock()
		if reservedLow != 512<<20 {
			t.Fatalf("low-1 parked charge after reclaim = %s, want 0.5 GiB", gib(reservedLow))
		}
	})
}

// TestPreemptionDoesNotBypassMemoryPressure verifies Finding #211 (board 9661):
// parking a small idle low-priority command while a server or external workload
// drives memory PSI >= admitPressure MUST NOT bypass the memory-pressure hold.
// Admission waits until a fresh sample shows memory PSI < admitPressure.
func TestPreemptionDoesNotBypassMemoryPressure(t *testing.T) {
	e, units, sys, now := parkReclaimEngine(t)
	_ = startWithLimits(t, e, units.Units, now, "srv-1", "a", "server", &pb.CommandLimits{Priority: 0})
	_ = startWithLimits(t, e, units.Units, now, "low-idle", "b", "build", &pb.CommandLimits{Priority: -10})

	// External / server workload drives memory PSI to 55% (>= 22% admitPressure).
	sys.Psi = 55
	e.Sample()
	if !e.Heartbeat("c").Admission.Holding {
		t.Fatal("expected node to hold new work at 55% memory PSI")
	}

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "urgent-1", WorkspaceId: "c", Args: []string{"sleep", "60"}, Kind: "build",
		Limits: &pb.CommandLimits{Priority: 20},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-urgent-1") })

	// Parking low-idle must NOT admit urgent-1 while memory PSI stays at 55%.
	e.step(t.Context())
	if sUrg, sLow := e.state("urgent-1"), e.state("low-idle"); sUrg != queued || sLow != parked {
		t.Fatalf("at 55%% memory PSI after parking low-idle: urgent-1=%v (%q) low-idle=%v, want queued/parked",
			sUrg, e.commands["urgent-1"].cmd().Reason, sLow)
	}
	if !strings.Contains(e.commands["urgent-1"].cmd().Reason, "memory pressure") {
		t.Errorf("urgent-1 reason = %q, want memory pressure", e.commands["urgent-1"].cmd().Reason)
	}

	// Still at 55% on a subsequent tick: urgent-1 stays queued and low-idle stays parked.
	*now = now.Add(time.Second)
	e.Sample()
	e.step(t.Context())
	if sUrg, sLow := e.state("urgent-1"), e.state("low-idle"); sUrg != queued || sLow != parked {
		t.Fatalf("still at 55%% memory PSI: urgent-1=%v low-idle=%v, want queued/parked", sUrg, sLow)
	}

	// Once a fresh sample shows memory PSI < admitPressure (10% < 22%), urgent-1 admits!
	sys.Psi = 10
	*now = now.Add(time.Second)
	e.Sample()
	e.step(t.Context())
	if sUrg, sLow := e.state("urgent-1"), e.state("low-idle"); sUrg != running || sLow != parked {
		t.Fatalf("after fresh PSI=10%% sample: urgent-1=%v (%q) low-idle=%v, want running/parked",
			sUrg, e.commands["urgent-1"].cmd().Reason, sLow)
	}
}

// TestImpossibleHighPriorityRequestDoesNotDeadlockLowerThaw verifies
// Finding #212 (board 9661): a queued high-priority command whose memory
// guarantee exceeds admitLimit() (e.g. 15 GiB on a 16 GiB worker) can never
// fit and must NOT veto thawing a lower-priority parked command when the last
// runner exits. Also verifies the last-runnable-command guarantee when a
// feasible high-priority command is blocked by a node-wide disk hold.
func TestImpossibleHighPriorityRequestDoesNotDeadlockLowerThaw(t *testing.T) {
	t.Run("impossible memory guarantee > admitLimit", func(t *testing.T) {
		e, units, sys, now := parkReclaimEngine(t)
		_ = startWithLimits(t, e, units.Units, now, "peer-1", "a", "build", &pb.CommandLimits{Priority: 0})
		low := startWithLimits(t, e, units.Units, now, "low-1", "b", "build", &pb.CommandLimits{Priority: -10})

		// Park low-1 via memory pressure.
		sys.SetUsed(sys.Mem.Total * 95 / 100)
		*now = now.Add(time.Second)
		e.step(t.Context())
		if s := low.cmd().State; s != parked {
			t.Fatalf("low-1 = %v, want parked", s)
		}

		// Memory recovers, and a high-priority command requesting 15 GiB
		// (exceeds 13.76 GiB admitLimit on 16 GiB node) is queued.
		sys.SetUsed(sys.Mem.Total * 30 / 100)
		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "high-15g", WorkspaceId: "c", Args: []string{"sleep", "60"}, Kind: "build",
			Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 15 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-15g") })

		// Last runner exits -> low-1 MUST thaw instead of deadlocking behind high-15g.
		if _, err := e.Stop(t.Context(), "peer-1", "done"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		e.step(t.Context())
		if sLow, sHigh := e.state("low-1"), e.state("high-15g"); sLow != running || sHigh != queued {
			t.Fatalf("after peer-1 exited with impossible high-15g queued: low-1=%v high-15g=%v, want running/queued", sLow, sHigh)
		}
	})

	t.Run("idle node with high-priority blocked by disk hold still thaws last parked", func(t *testing.T) {
		e, units, sys, now := parkReclaimEngine(t)
		_ = startWithLimits(t, e, units.Units, now, "peer-1", "a", "build", &pb.CommandLimits{Priority: 0})
		low := startWithLimits(t, e, units.Units, now, "low-1", "b", "build", &pb.CommandLimits{Priority: -10})

		sys.SetUsed(sys.Mem.Total * 95 / 100)
		*now = now.Add(time.Second)
		e.step(t.Context())
		if s := low.cmd().State; s != parked {
			t.Fatalf("low-1 = %v, want parked", s)
		}

		// Disk is full so high-disk cannot be admitted even on an idle node.
		sys.SetUsed(sys.Mem.Total * 30 / 100)
		e.mu.Lock()
		e.diskBytes = 100 << 30
		e.disk = &pb.DiskUsage{FreeBytes: 1 << 30}
		e.mu.Unlock()

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "high-disk", WorkspaceId: "c", Args: []string{"sleep", "60"}, Kind: "build",
			Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 2 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-high-disk") })

		if _, err := e.Stop(t.Context(), "peer-1", "done"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		e.step(t.Context())
		if sLow, sHigh := e.state("low-1"), e.state("high-disk"); sLow != running || sHigh != queued {
			t.Fatalf("on idle node with disk-blocked high-disk: low-1=%v high-disk=%v, want running/queued", sLow, sHigh)
		}
	})
}

// TestPreemptedByPersistsAcrossRestart verifies Finding #213 (board 9661):
// a victim's preemptedBy relationship is persisted in worker.db across worker
// restarts so the victim stays parked while its preemptor is still live, and
// thaws once the preemptor exits (or immediately if the preemptor already ended
// before restart).
func TestPreemptedByPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: t.TempDir()}
	baseUnits := &fake.Units{}
	units := &reclaimingUnits{Units: baseUnits}
	sys := fake.NewSystem()
	now := time.Unix(1795400000, 0)

	newEng := func() *Engine {
		e, err := NewEngine(t.Context(), EngineConfig{
			Dir: dir, NodeID: "n", Host: "asahimbp",
			Store: store, BaseImage: fake.BaseImage(store), Units: units, System: sys,
			Now: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	e1 := newEng()
	for _, w := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		ws := &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
			Volume:    "ws/" + w,
		}}
		e1.mu.Lock()
		e1.workspaces[w] = ws
		e1.saveWorkspace(ws, change{what: "idle"})
		e1.mu.Unlock()
	}

	_ = startWithLimits(t, e1, baseUnits, &now, "low-cpu", "a", "build", &pb.CommandLimits{Priority: -10})

	// High CPU pressure -> high-cpu preempts low-cpu.
	sys.SetCPUPressure(75)
	e1.Sample()
	now = now.Add(time.Second)
	if _, err := e1.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "high-cpu", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "build",
		Limits: &pb.CommandLimits{Priority: 10},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { baseUnits.Stop(context.WithoutCancel(t.Context()), "hotbed-high-cpu") })

	e1.step(t.Context())
	if sHigh, sLow := e1.state("high-cpu"), e1.state("low-cpu"); sHigh != running || sLow != parked {
		t.Fatalf("before restart: high-cpu=%v low-cpu=%v, want running/parked", sHigh, sLow)
	}

	// Restart the Engine against the same worker.db (live units keep running).
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := newEng()
	t.Cleanup(func() { e2.Close() })

	e2.mu.Lock()
	restoredPreemptedBy := e2.commands["low-cpu"].preemptedBy
	e2.mu.Unlock()
	if restoredPreemptedBy != "high-cpu" {
		t.Fatalf("after restart: low-cpu.preemptedBy = %q, want %q", restoredPreemptedBy, "high-cpu")
	}

	// Even with low CPU/memory usage and past rotateAfter, low-cpu MUST stay
	// parked while high-cpu is still running!
	sys.SetCPUPressure(0)
	sys.SetUsed(sys.Mem.Total * 20 / 100)
	e2.Sample()
	now = now.Add(rotateAfter + time.Minute)
	e2.step(t.Context())
	if sHigh, sLow := e2.state("high-cpu"), e2.state("low-cpu"); sHigh != running || sLow != parked {
		t.Fatalf("after restart while high-cpu runs: high-cpu=%v low-cpu=%v, want running/parked", sHigh, sLow)
	}

	// Once high-cpu exits, low-cpu thaws and preemptedBy is cleared (also in DB).
	if _, err := e2.Stop(t.Context(), "high-cpu", "done"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	e2.step(t.Context())
	if sLow := e2.state("low-cpu"); sLow != running {
		t.Fatalf("after high-cpu ended: low-cpu=%v, want running", sLow)
	}
	e2.mu.Lock()
	afterThaw := e2.commands["low-cpu"].preemptedBy
	e2.mu.Unlock()
	if afterThaw != "" {
		t.Fatalf("after thaw: low-cpu.preemptedBy = %q, want empty", afterThaw)
	}
	preemptions, err := e2.db.loadPreemptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(preemptions) != 0 {
		t.Fatalf("persisted preemptions after thaw = %v, want empty", preemptions)
	}
	if got := slices.ContainsFunc(e2.commands["low-cpu"].cmd().Timeline, func(ev *pb.CommandEvent) bool {
		return ev.What == "thawed"
	}); !got {
		t.Errorf("expected thawed timeline event on low-cpu after restart + preemptor exit")
	}

	// Also verify reconcileLoaded clears a stale persisted preemptedBy record
	// if the named preemptor is no longer live on restart.
	e2.mu.Lock()
	lowCmd := e2.commands["low-cpu"]
	lowCmd.cmd().State = parked
	lowCmd.rec.ParkedAt = e2.nowMs()
	lowCmd.preemptedBy = "already-gone"
	e2.saveCommand(lowCmd, change{what: "parked", detail: "stale preemptor test"})
	e2.mu.Unlock()
	if err := baseUnits.Freeze(t.Context(), "hotbed-low-cpu"); err != nil {
		t.Fatal(err)
	}
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}

	e3 := newEng()
	t.Cleanup(func() { e3.Close() })
	e3.mu.Lock()
	cleared := e3.commands["low-cpu"].preemptedBy
	e3.mu.Unlock()
	if cleared != "" {
		t.Fatalf("after restart with missing preemptor: low-cpu.preemptedBy = %q, want empty", cleared)
	}
	preemptions, err = e3.db.loadPreemptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(preemptions) != 0 {
		t.Fatalf("persisted preemptions after stale reconcile = %v, want empty", preemptions)
	}
	now = now.Add(time.Second)
	e3.step(t.Context())
	if s := e3.state("low-cpu"); s != running {
		t.Fatalf("after restart with stale preemptor cleared, low-cpu = %v, want running", s)
	}
}
