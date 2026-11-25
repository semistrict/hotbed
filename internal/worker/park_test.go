package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

// parkEngine is an engine with idle workspaces a, b and c, a clock the
// test moves, and a 16 GiB machine whose memory in use the test sets.
func parkEngine(t *testing.T) (*Engine, *fake.Units, *fake.System, *time.Time) {
	t.Helper()
	units, sys := &fake.Units{}, fake.NewSystem()
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

// start runs a sleep in workspace ws as command id, of kind, and steps it
// to running, a second after the one before.
func (e *Engine) start(t *testing.T, units *fake.Units, now *time.Time, id, ws, kind string) *command {
	t.Helper()
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: id, WorkspaceId: ws, Args: []string{"sleep", "60"}, Kind: kind}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+id) })
	e.commands[id].cmd().Kind = kind
	e.step(t.Context())
	if s := e.commands[id].cmd().State; s != running {
		t.Fatalf("%s is %v: %s", id, s, e.commands[id].cmd().Reason)
	}
	return e.commands[id]
}

// Under constraint, hotbed focuses on completing one session's work rather than
// interleaving across sessions: non-focus sessions are parked before the focus
// session, and when a focus session finishes a command while it already has
// more work queued on the node, it drains its queued work before switching.
func TestFocusSessionContinuityUnderConstraint(t *testing.T) {
	t.Run("focus session drains its queued work before switching, but higher priority still wins", func(t *testing.T) {
		e, units, _, now := parkEngine(t)
		e.workspaces["a"].ws().SessionId = "sess-1"
		e.workspaces["b"].ws().SessionId = "sess-2"
		e.workspaces["c"].ws().SessionId = "sess-1"

		_ = e.start(t, units, now, "a1", "a", "build")
		units.SetAnon("hotbed-a1", 14<<30) // fills 13.76 GiB admitLimit
		*now = now.Add(time.Second)
		e.step(t.Context())

		// sess-2 queues b1 first (t+1), then sess-1 queues a2 (t+2); each needs 8 GiB.
		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "b1", WorkspaceId: "b", Args: []string{"sleep", "60"},
			Limits: &pb.CommandLimits{MemoryLowBytes: 8 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-b1") })

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "a2", WorkspaceId: "c", Args: []string{"sleep", "60"},
			Limits: &pb.CommandLimits{MemoryLowBytes: 8 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-a2") })
		e.step(t.Context())
		if e.state("b1") != queued || e.state("a2") != queued {
			t.Fatalf("while a1 runs: b1=%v a2=%v, want queued/queued", e.state("b1"), e.state("a2"))
		}

		// When a1 finishes, sess-1 still has a2 queued -> a2 admits ahead of b1!
		if _, err := e.Stop(t.Context(), "a1", "done"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		e.step(t.Context())
		if e.state("a2") != running || e.state("b1") != queued {
			t.Fatalf("after a1 finished: a2=%v b1=%v, want a2 running (focus session) and b1 queued",
				e.state("a2"), e.state("b1"))
		}

		// Once a2 finishes and sess-1 has no more queued work, b1 admits immediately.
		if _, err := e.Stop(t.Context(), "a2", "done"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		e.step(t.Context())
		if e.state("b1") != running {
			t.Fatalf("after a2 finished: b1=%v, want running", e.state("b1"))
		}
	})

	t.Run("higher priority from another session still beats focus session", func(t *testing.T) {
		e, units, _, now := parkEngine(t)
		e.workspaces["a"].ws().SessionId = "sess-1"
		e.workspaces["b"].ws().SessionId = "sess-2"
		e.workspaces["c"].ws().SessionId = "sess-1"

		_ = e.start(t, units, now, "a1", "a", "build")
		units.SetAnon("hotbed-a1", 14<<30)
		*now = now.Add(time.Second)
		e.step(t.Context())

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "a2", WorkspaceId: "c", Args: []string{"sleep", "60"},
			Limits: &pb.CommandLimits{MemoryLowBytes: 8 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-a2") })

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "b-hi", WorkspaceId: "b", Args: []string{"sleep", "60"},
			Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 8 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-b-hi") })

		if _, err := e.Stop(t.Context(), "a1", "done"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		e.step(t.Context())
		if e.state("b-hi") != running || e.state("a2") != queued {
			t.Fatalf("after a1 finished: b-hi=%v a2=%v, want b-hi running (higher priority) and a2 queued",
				e.state("b-hi"), e.state("a2"))
		}
	})

	t.Run("focus session work does not jump ahead of a waiting measurement", func(t *testing.T) {
		now := time.Unix(1795400000, 0)
		units, sys := &fake.Units{}, fake.NewSystem()
		units.SetCores(6) // exceeds the 4 work CPUs left beside bench CPUs
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
		for _, w := range []string{"a", "b"} {
			if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
				t.Fatal(err)
			}
			e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{
				Workspace: &pb.Workspace{Id: w, Project: "app", SessionId: "tidy-owl", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
				Volume:    "ws/" + w,
			}}
		}

		w1 := e.start(t, units, &now, "w1", "a", "shell")
		// Seed w1's measured CPU load above the 4 work CPUs left beside bench CPUs.
		e.mu.Lock()
		w1.cmd().CpuCores = 6
		e.mu.Unlock()

		now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "bench-1", WorkspaceId: "b", Args: []string{"sleep", "60"}, Bench: true,
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-bench-1") })

		// Queue w2 in the SAME focus session ("tidy-owl") behind bench-1.
		now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "w2", WorkspaceId: "a", Args: []string{"sleep", "60"},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-w2") })

		e.step(t.Context())
		if gotB, gotW2 := e.commands["bench-1"].cmd(), e.commands["w2"].cmd(); gotB.State != queued || gotW2.State != queued || !strings.Contains(gotW2.Reason, "waiting behind measurement bench-1") {
			t.Fatalf("with bench-1 waiting for CPUs: bench-1=%v (%q), w2=%v (%q); want both queued with w2 waiting behind bench-1",
				gotB.State, gotB.Reason, gotW2.State, gotW2.Reason)
		}
	})
}

// A node with running work must not admit additional commands once physical RAM
// reaches admitMemory (86%), and memory pressure uses hysteresis (holds at 22%,
// stays held at 15%, resumes below 12%) so brief dips do not flap admission.
func TestBusyNodeHoldsAtAdmitMemoryAndMemoryPressureHysteresis(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	_ = e.start(t, units, now, "run-1", "a", "build")

	// 88% RAM used (>= admitMemory 86%, < parkAt 93%) with plenty of free swap:
	// must NOT admit a 2nd command only to park it seconds later.
	sys.SetUsed(sys.Mem.Total * 88 / 100)
	e.Sample()
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "wait-mem", WorkspaceId: "b", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-wait-mem") })
	e.admit(t.Context())
	if got := e.commands["wait-mem"].cmd(); got.State != queued || !strings.Contains(got.Reason, "while other work is running") {
		t.Fatalf("wait-mem at 88%% RAM beside run-1 = %v (%q), want queued", got.State, got.Reason)
	}

	// Memory drops to 50%, but memory PSI spiked to 25% (>= 22%) and then only
	// dipped to 15% (>= 12% resume threshold): must remain held!
	sys.SetUsed(sys.Mem.Total * 50 / 100)
	sys.Psi = 25
	e.Sample()
	sys.Psi = 15
	*now = now.Add(time.Second)
	e.Sample()
	e.admit(t.Context())
	if got := e.commands["wait-mem"].cmd(); got.State != queued || !strings.Contains(got.Reason, "memory pressure") {
		t.Fatalf("wait-mem at 15%% PSI after 25%% spike = %v (%q), want still held by hysteresis", got.State, got.Reason)
	}
	if a := e.Heartbeat("cl").Admission; !a.Holding || !signal(a, "memory-pressure").Holding {
		t.Fatalf("expected memory-pressure signal holding at 15%% PSI after 25%% spike, got %+v", a)
	}

	// Once PSI drops under 12% (e.g. 10%), admission resumes cleanly.
	sys.Psi = 10
	*now = now.Add(time.Second)
	e.Sample()
	e.admit(t.Context())
	if st := e.state("wait-mem"); st != running {
		t.Fatalf("wait-mem once PSI dropped to 10%% = %v, want running", st)
	}
}

type scriptedUnits struct {
	*fake.Units
	mu       sync.Mutex
	override map[string]unit.State
	thawErr  map[string]error
	thaws    map[string]int
}

func (u *scriptedUnits) Status(ctx context.Context, name string) (unit.State, error) {
	u.mu.Lock()
	if st, ok := u.override[name]; ok {
		u.mu.Unlock()
		return st, nil
	}
	u.mu.Unlock()
	return u.Units.Status(ctx, name)
}

func (u *scriptedUnits) StatusMany(ctx context.Context, names []string) (map[string]unit.State, error) {
	out := make(map[string]unit.State, len(names))
	for _, name := range names {
		st, err := u.Status(ctx, name)
		if err != nil {
			return nil, err
		}
		out[name] = st
	}
	return out, nil
}

func (u *scriptedUnits) Thaw(ctx context.Context, name string) error {
	u.mu.Lock()
	if u.thaws == nil {
		u.thaws = map[string]int{}
	}
	u.thaws[name]++
	if err := u.thawErr[name]; err != nil {
		u.mu.Unlock()
		return err
	}
	u.mu.Unlock()
	return u.Units.Thaw(ctx, name)
}

type hookUnits struct {
	*fake.Units
	mu       sync.Mutex
	onFreeze func(name string)
	onThaw   func(name string)
}

func (u *hookUnits) Freeze(ctx context.Context, name string) error {
	u.mu.Lock()
	fn := u.onFreeze
	u.mu.Unlock()
	if fn != nil {
		fn(name)
	}
	return u.Units.Freeze(ctx, name)
}

func (u *hookUnits) Thaw(ctx context.Context, name string) error {
	u.mu.Lock()
	fn := u.onThaw
	u.mu.Unlock()
	if fn != nil {
		fn(name)
	}
	return u.Units.Thaw(ctx, name)
}

type countingUnits struct {
	*fake.Units
	mu              sync.Mutex
	statusCalls     int
	statusManyCalls int
	lastBatch       []string
}

func (u *countingUnits) Status(ctx context.Context, name string) (unit.State, error) {
	u.mu.Lock()
	u.statusCalls++
	u.mu.Unlock()
	return u.Units.Status(ctx, name)
}

func (u *countingUnits) StatusMany(ctx context.Context, names []string) (map[string]unit.State, error) {
	u.mu.Lock()
	u.statusManyCalls++
	u.lastBatch = slices.Clone(names)
	u.mu.Unlock()
	return u.Units.StatusMany(ctx, names)
}

// Engine.step must query all live commands in one batched StatusMany call
// instead of N sequential Status calls per tick (board 8013).
func TestStepBatchesStatusMany(t *testing.T) {
	baseUnits := &fake.Units{}
	units := &countingUnits{Units: baseUnits}
	sys := fake.NewSystem()
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
		e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
			Volume:    "ws/" + w,
		}}
	}

	c1 := e.start(t, baseUnits, &now, "cmd-1", "a", "build")
	c2 := e.start(t, baseUnits, &now, "cmd-2", "b", "build")
	c3 := e.start(t, baseUnits, &now, "cmd-3", "c", "build")
	// Park c3 so running + parked are both included in the single batch.
	e.mu.Lock()
	c3.cmd().State = parked
	c3.rec.ParkedAt = e.nowMs()
	e.mu.Unlock()
	if err := baseUnits.Freeze(t.Context(), c3.unitName()); err != nil {
		t.Fatal(err)
	}
	// Reset counters after setup.
	units.mu.Lock()
	units.statusCalls = 0
	units.statusManyCalls = 0
	units.lastBatch = nil
	units.mu.Unlock()

	// Keep memory above thawBelow so park() does not thaw c3 during step.
	sys.SetUsed(sys.Mem.Total * 85 / 100)
	now = now.Add(time.Second)
	e.step(t.Context())

	units.mu.Lock()
	gotStatus := units.statusCalls
	gotBatchCalls := units.statusManyCalls
	gotBatch := slices.Clone(units.lastBatch)
	units.mu.Unlock()

	if gotStatus != 0 {
		t.Errorf("Engine.step made %d individual Status calls, want 0", gotStatus)
	}
	if gotBatchCalls != 1 {
		t.Errorf("Engine.step made %d StatusMany calls for 3 live commands, want 1", gotBatchCalls)
	}
	slices.Sort(gotBatch)
	wantBatch := []string{c1.unitName(), c2.unitName(), c3.unitName()}
	slices.Sort(wantBatch)
	if !slices.Equal(gotBatch, wantBatch) {
		t.Errorf("StatusMany batch = %q, want %q", gotBatch, wantBatch)
	}
	if c1.rec.Cgroup == "" || c2.rec.Cgroup == "" || c3.rec.Cgroup == "" {
		t.Errorf("commands were not reconciled from batched status: c1=%q c2=%q c3=%q", c1.rec.Cgroup, c2.rec.Cgroup, c3.rec.Cgroup)
	}
}

// When a slice's finite swap budget (MemorySwapMax) is exhausted (>= 95% full)
// while RAM is already above thawBelow and work is running, new non-bench
// work must stay queued rather than admitting and forcing an impossible park
// into a full swap cap. Once swap headroom returns (or the node goes idle),
// queued work admits immediately.
func TestSwapFullHoldsNewWorkWhenRAMAboveAdmitLimit(t *testing.T) {
	e, units, sys, clk := parkEngine(t)
	e.start(t, units, clk, "c-run", "a", "build")
	// 75% RAM used (>= thawBelow 70%, < admitMemory 86%) and 98% slice swap full.
	const giB = int64(1 << 30)
	sys.SetMem(sysinfo.Mem{
		Total:     16 * giB,
		Available: 4 * giB, // 75% used
		SwapTotal: 4 * giB,
		SwapFree:  80 << 20, // ~98% swap used
	})
	e.Sample()

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "c-wait", WorkspaceId: "b", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-c-wait") })

	e.admit(t.Context())
	cWait, _ := e.Command("c-wait")
	if cWait.State != queued || !strings.Contains(cWait.Reason, "swap") {
		t.Fatalf("c-wait with full slice swap = %v (%q), want queued for swap", cWait.State, cWait.Reason)
	}

	// Once swap headroom returns (e.g. an idle VM stopped or swap drained), c-wait admits.
	sys.SetMem(sysinfo.Mem{
		Total:     16 * giB,
		Available: 4 * giB,
		SwapTotal: 4 * giB,
		SwapFree:  2 * giB,
	})
	e.Sample()
	e.admit(t.Context())
	if st := e.state("c-wait"); st != running {
		t.Fatalf("c-wait after swap headroom returned = %v, want running", st)
	}
}
