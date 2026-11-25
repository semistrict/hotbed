package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

// A parked command whose own swap would never fit beside the others isn't
// parked for good: once it has waited rotateAfter it trades places with
// the newest running command (board 7422).
func TestAParkedCommandGetsItsTurn(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	old := e.start(t, units, now, "old-1", "a", "build")
	young := e.start(t, units, now, "young-1", "b", "build")
	sys.SetUsed(sys.Mem.Total * 95 / 100)
	e.step(t.Context())
	if s := young.cmd().State; s != parked {
		t.Fatalf("the newest is %v at 95%%, want parked", s)
	}
	// Its memory went to swap, more than would fit back beside the other.
	units.SetSwap("hotbed-young-1", sys.Mem.Total/2)
	sys.SetUsed(sys.Mem.Total * 40 / 100)
	*now = now.Add(rotateAfter - time.Second)
	e.step(t.Context())
	if s := young.cmd().State; s != parked {
		t.Fatalf("the parked one is %v before its turn, want still parked", s)
	}
	*now = now.Add(time.Second)
	e.step(t.Context())
	if s, o := young.cmd().State, old.cmd().State; s != running || o != parked {
		t.Fatalf("after %v the parked one is %v and the other %v; want them traded", rotateAfter, s, o)
	}
	if r := old.cmd().Reason; !strings.HasPrefix(r, "frozen to give young-1 its turn (parked 10m0s)") {
		t.Errorf("the other's reason = %q", r)
	}
	if st, _ := units.Status(t.Context(), "hotbed-young-1"); st.Frozen {
		t.Error("the thawed one's unit is still frozen")
	}
	// The one parked in its place waits its own turn, not trading straight back.
	units.SetSwap("hotbed-old-1", sys.Mem.Total/2)
	*now = now.Add(time.Second)
	e.step(t.Context())
	if s := old.cmd().State; s != parked {
		t.Errorf("the one parked in its place is %v a second later, want parked", s)
	}
}

// When multiple parked commands have all waited longer than rotateAfter, each
// thawed command must still get a full rotateAfter run quantum before rotating
// out for the next waiting command (preventing 1 Hz park/thaw thrashing).
func TestRotationDoesNotThrashMultipleLongParkedCommands(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	c1 := e.start(t, units, now, "cmd-1", "a", "build")
	c2 := e.start(t, units, now, "cmd-2", "b", "build")
	c3 := e.start(t, units, now, "cmd-3", "c", "build")

	// Park c3 then c2 under memory pressure, leaving c1 running.
	sys.SetUsed(sys.Mem.Total * 95 / 100)
	e.step(t.Context())
	*now = now.Add(time.Second)
	e.step(t.Context())
	if c1.cmd().State != running || c2.cmd().State != parked || c3.cmd().State != parked {
		t.Fatalf("states after parking = (%v, %v, %v), want (running, parked, parked)",
			c1.cmd().State, c2.cmd().State, c3.cmd().State)
	}

	// Both parked commands have large swap so neither fits alongside the running one.
	units.SetSwap("hotbed-cmd-2", sys.Mem.Total/2)
	units.SetSwap("hotbed-cmd-3", sys.Mem.Total/2)
	sys.SetUsed(sys.Mem.Total * 40 / 100)

	// Advance past rotateAfter: oldest parked (c3, parked first) trades with c1.
	*now = now.Add(rotateAfter + time.Minute)
	e.step(t.Context())
	if c3.cmd().State != running || c1.cmd().State != parked || c2.cmd().State != parked {
		t.Fatalf("after first rotation = (%v, %v, %v), want (parked, parked, running)",
			c1.cmd().State, c2.cmd().State, c3.cmd().State)
	}

	// One second later, c2 has STILL been parked > rotateAfter, but c3 has only
	// run for 1 second: c3 MUST NOT be rotated out immediately.
	units.SetSwap("hotbed-cmd-1", sys.Mem.Total/2)
	*now = now.Add(time.Second)
	e.step(t.Context())
	if c3.cmd().State != running || c2.cmd().State != parked {
		t.Fatalf("1s after thawing c3: c3=%v c2=%v, want c3 still running (no 1 Hz rotation thrash)",
			c3.cmd().State, c2.cmd().State)
	}

	// Once c3 has run for rotateAfter, c2 gets its turn.
	*now = now.Add(rotateAfter)
	e.step(t.Context())
	if c2.cmd().State != running || c3.cmd().State != parked {
		t.Fatalf("after c3 ran rotateAfter: c2=%v c3=%v, want c2 running and c3 parked",
			c2.cmd().State, c3.cmd().State)
	}
}

// A base build is parked last, and thawed first, at once: every workspace
// wanting its base waits on it.
func TestABaseBuildIsParkedLastAndThawedFirst(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	work := e.start(t, units, now, "work-1", "a", "build")
	base := e.start(t, units, now, "f-app-g2", "b", "freshen")
	sys.SetUsed(sys.Mem.Total * 95 / 100)
	e.step(t.Context())
	if s, b := work.cmd().State, base.cmd().State; s != parked || b != running {
		t.Fatalf("at 95%% the work is %v and the newer base build %v; want the work parked", s, b)
	}

	// One parked before (an older worker's) is thawed first, trading places
	// with the work at once.
	e2, units2, sys2, now2 := parkEngine(t)
	base2 := e2.start(t, units2, now2, "f-app-g3", "a", "freshen")
	other := e2.start(t, units2, now2, "test-1", "b", "test")
	sys2.SetUsed(sys2.Mem.Total * 95 / 100)
	e2.mu.Lock()
	base2.cmd().State = parked
	base2.rec.ParkedAt = e2.nowMs()
	e2.mu.Unlock()
	if err := units2.Freeze(t.Context(), "hotbed-f-app-g3"); err != nil {
		t.Fatal(err)
	}
	units2.SetSwap("hotbed-f-app-g3", sys2.Mem.Total/2)
	sys2.SetUsed(sys2.Mem.Total * 40 / 100)
	*now2 = now2.Add(time.Second)
	e2.step(t.Context())
	if b, o := base2.cmd().State, other.cmd().State; b != running || o != parked {
		t.Errorf("a parked base build is %v and the work %v; want the base build's turn at once", b, o)
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

// A parked unit that systemd is deactivating (e.g. OOMPolicy=stop while frozen)
// must not trigger a 1 Hz ERROR thaw loop or block other parked commands from
// thawing, and a unit that exits between Status and Thaw settles immediately
// without logging an error (board 7695).
func TestDeactivatingOrExitedParkedUnitDoesNotSpinThawErrors(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	baseUnits := &fake.Units{}
	units := &scriptedUnits{
		Units:    baseUnits,
		override: map[string]unit.State{},
		thawErr:  map[string]error{},
	}
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
	for _, w := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle}, Volume: "ws/" + w}}
	}

	first := e.start(t, baseUnits, &now, "f-app-g14", "a", "freshen")
	second := e.start(t, baseUnits, &now, "work-2", "b", "build")

	// Park both commands manually (base build sorts ahead of work).
	e.mu.Lock()
	first.cmd().State = parked
	first.rec.ParkedAt = e.nowMs()
	second.cmd().State = parked
	second.rec.ParkedAt = e.nowMs()
	e.mu.Unlock()
	if err := baseUnits.Freeze(t.Context(), first.unitName()); err != nil {
		t.Fatal(err)
	}
	if err := baseUnits.Freeze(t.Context(), second.unitName()); err != nil {
		t.Fatal(err)
	}

	// Simulate systemd ActiveState=deactivating (Loaded=true, Running=false, Exited=false)
	// after OOMPolicy=stop killed a process inside the frozen unit.
	units.mu.Lock()
	units.override[first.unitName()] = unit.State{Loaded: true, Running: false, Frozen: true, Exited: false}
	units.thawErr[first.unitName()] = errors.New("systemctl thaw: exit status 1: Failed to thaw unit: Unit is not active")
	units.mu.Unlock()

	sys.SetUsed(sys.Mem.Total * 30 / 100)
	for range 5 {
		now = now.Add(time.Second)
		e.step(t.Context())
	}

	units.mu.Lock()
	firstThaws := units.thaws[first.unitName()]
	units.mu.Unlock()
	if firstThaws != 0 {
		t.Errorf("Thaw called %d times on deactivating unit, want 0", firstThaws)
	}
	if second.cmd().State != running {
		t.Errorf("second parked command state = %v, want running (should not be blocked by deactivating unit)", second.cmd().State)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("unexpected ERROR logs while unit deactivating:\n%s", logs.String())
	}

	// Now simulate systemd finishing deactivation with OOM kill: command settles.
	units.mu.Lock()
	units.override[first.unitName()] = unit.State{Loaded: true, Exited: true, Result: "oom-kill", OOMKilled: true, Signal: 9}
	units.mu.Unlock()
	now = now.Add(time.Second)
	e.step(t.Context())
	if first.cmd().State != failed {
		t.Fatalf("first command state = %v after OOM exit, want failed", first.cmd().State)
	}

	// Race case: a parked unit exits between Status and Thaw — thawOne reconciles
	// immediately and emits no ERROR log.
	e.mu.Lock()
	second.cmd().State = parked
	second.rec.ParkedAt = e.nowMs()
	e.mu.Unlock()
	units.mu.Lock()
	units.override[second.unitName()] = unit.State{Loaded: true, Exited: true, ExitCode: 1}
	units.thawErr[second.unitName()] = errors.New("systemctl thaw: exit status 1: Unit is not active")
	units.mu.Unlock()
	e.thawOne(t.Context(), second, "")
	if second.cmd().State != failed {
		t.Errorf("second command after race-exit thawOne = %v, want failed", second.cmd().State)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("unexpected ERROR log on race-exit thawOne:\n%s", logs.String())
	}
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

// Concurrent Stop() or exit while park() / parkOne() / thawOne() are blocked
// inside Freeze or Thaw must never race on command fields or resurrect a
// terminal command with "parked" or "thawed" events (board 7884).
func TestParkConcurrentStopAndExitDuringFreezeAndThaw(t *testing.T) {
	newHookedEngine := func(t *testing.T) (*Engine, *hookUnits, *fake.System, *time.Time) {
		t.Helper()
		baseUnits := &fake.Units{}
		units := &hookUnits{Units: baseUnits}
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
		for _, w := range []string{"a", "b"} {
			if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
				t.Fatal(err)
			}
			e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{
				Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
				Volume:    "ws/" + w,
			}}
		}
		return e, units, sys, &now
	}

	t.Run("stop completes during Freeze", func(t *testing.T) {
		e, units, sys, now := newHookedEngine(t)
		_ = e.start(t, units.Units, now, "old-1", "a", "build")
		young := e.start(t, units.Units, now, "young-1", "b", "build")

		// Seed cgroup and memory and mutate them concurrently under e.mu during
		// Freeze to verify -race catches any unlocked reads in park/parkOne.
		var wg sync.WaitGroup
		units.onFreeze = func(name string) {
			if name != young.unitName() {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.mu.Lock()
				young.rec.Cgroup = "/hotbed.slice/hotbed-young-1.service"
				young.cmd().MemoryBytes = 1 << 30
				young.cmd().SwapBytes = 512 << 20
				e.mu.Unlock()
			}()
			if _, err := e.Stop(t.Context(), "young-1", "cancelled during freeze"); err != nil {
				t.Errorf("Stop during Freeze: %v", err)
			}
		}

		sys.SetUsed(sys.Mem.Total * 95 / 100)
		e.park(t.Context())
		wg.Wait()

		got, ok := e.Command("young-1")
		if !ok {
			t.Fatal("young-1 missing")
		}
		if got.State != cancelled {
			t.Fatalf("state after Stop during Freeze = %v, want cancelled (resurrected!)", got.State)
		}
		for _, ev := range got.Timeline {
			if ev.What == "parked" {
				t.Errorf("unexpected parked timeline event after Stop during Freeze: %+v", got.Timeline)
			}
		}
	})

	t.Run("natural exit during Freeze", func(t *testing.T) {
		e, units, sys, now := newHookedEngine(t)
		_ = e.start(t, units.Units, now, "old-1", "a", "build")
		young := e.start(t, units.Units, now, "young-1", "b", "build")

		units.onFreeze = func(name string) {
			if name != young.unitName() {
				return
			}
			code := int32(0)
			e.mu.Lock()
			e.end(young, succeeded, &code, "")
			e.mu.Unlock()
		}

		sys.SetUsed(sys.Mem.Total * 95 / 100)
		e.park(t.Context())

		got, ok := e.Command("young-1")
		if !ok {
			t.Fatal("young-1 missing")
		}
		if got.State != succeeded {
			t.Fatalf("state after exit during Freeze = %v, want succeeded", got.State)
		}
		if last := got.Timeline[len(got.Timeline)-1]; last.What != "exited" {
			t.Errorf("last timeline event = %+v, want exited", last)
		}
	})

	t.Run("stop completes during Thaw", func(t *testing.T) {
		e, units, sys, now := newHookedEngine(t)
		_ = e.start(t, units.Units, now, "old-1", "a", "build")
		young := e.start(t, units.Units, now, "young-1", "b", "build")

		sys.SetUsed(sys.Mem.Total * 95 / 100)
		e.park(t.Context())
		if s := e.state("young-1"); s != parked {
			t.Fatalf("young-1 state = %v, want parked", s)
		}

		var wg sync.WaitGroup
		units.onThaw = func(name string) {
			if name != young.unitName() {
				return
			}
			// Clear hook before Stop's own thaw-before-stop runs.
			units.mu.Lock()
			units.onThaw = nil
			units.mu.Unlock()

			wg.Add(1)
			go func() {
				defer wg.Done()
				e.mu.Lock()
				young.cmd().SwapBytes = 256 << 20
				e.mu.Unlock()
			}()
			if _, err := e.Stop(t.Context(), "young-1", "cancelled during thaw"); err != nil {
				t.Errorf("Stop during Thaw: %v", err)
			}
		}

		sys.SetUsed(sys.Mem.Total * 30 / 100)
		e.park(t.Context())
		wg.Wait()

		got, ok := e.Command("young-1")
		if !ok {
			t.Fatal("young-1 missing")
		}
		if got.State != cancelled {
			t.Fatalf("state after Stop during Thaw = %v, want cancelled (resurrected!)", got.State)
		}
		// Thaw is published only after Units.Thaw succeeds (board 9800); since
		// Stop completed before Units.Thaw ran, no bogus "thawed" entry is
		// recorded and the timeline ends parked, cancelled.
		var whats []string
		for _, ev := range got.Timeline {
			whats = append(whats, ev.What)
		}
		if n := len(whats); !slices.Equal(whats[n-2:], []string{"parked", "cancelled"}) || slices.Contains(whats, "thawed") {
			t.Errorf("timeline after Stop during Thaw = %q, want it to end parked, cancelled with no thawed entry", whats)
		}
	})
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
// while RAM is already above admitMemory and work is running, new non-bench
// work must stay queued rather than admitting and forcing an impossible park
// into a full swap cap. Once swap headroom returns (or the node goes idle),
// queued work admits immediately.
func TestSwapFullHoldsNewWorkWhenRAMAboveAdmitLimit(t *testing.T) {
	e, units, sys, clk := parkEngine(t)
	e.start(t, units, clk, "c-run", "a", "build")
	// 88% RAM used (>= admitMemory 86%, < parkAt 93%) and 98% slice swap full.
	const giB = int64(1 << 30)
	sys.SetMem(sysinfo.Mem{
		Total:     16 * giB,
		Available: 16*giB - 14*giB - 100<<20, // ~88.1% used
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
		Available: 16*giB - 14*giB - 100<<20,
		SwapTotal: 4 * giB,
		SwapFree:  2 * giB,
	})
	e.Sample()
	e.admit(t.Context())
	if st := e.state("c-wait"); st != running {
		t.Fatalf("c-wait after swap headroom returned = %v, want running", st)
	}
}
