package worker

import (
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/unit"
)

// stepFor steps e every 30 seconds of its clock for d, adding to unit what
// a CPU busy that share of the time would use.
func stepFor(t *testing.T, e *Engine, units *fake.Units, now *time.Time, unit string, busy float64, d time.Duration) {
	t.Helper()
	const every = 30 * time.Second
	for range d / every {
		*now = now.Add(every)
		units.AddCPU(unit, time.Duration(busy*float64(every)))
		e.step(t.Context())
	}
}

// commandState is command id's state and reason, read under e.mu: the
// stop runs in the background.
func commandState(t *testing.T, e *Engine, id string) (pb.CommandState, string) {
	t.Helper()
	cmd, ok := e.Command(id)
	if !ok {
		t.Fatalf("no command %s", id)
	}
	return cmd.State, cmd.Reason
}

// wantRunning fails t unless command id is running.
func wantRunning(t *testing.T, e *Engine, id, when string) {
	t.Helper()
	if s, why := commandState(t, e, id); s != running {
		t.Fatalf("%s is %v %s, want running: %q", id, s, when, why)
	}
}

// wantStoppedAsStuck waits for the background stop of command id and fails
// t unless it ended cancelled, the node's intervention saying why.
func wantStoppedAsStuck(t *testing.T, e *Engine, id, why string) {
	t.Helper()
	e.idleStops.Wait()
	want := `stopped by the node asahimbp: "` + why + ` (hotbed run --keep-idle keeps a command that rightly waits on much memory)". ` +
		"No diagnostics: this worker takes no diagnostics: process inspection is not configured on it."
	if s, got := commandState(t, e, id); s != cancelled || got != want {
		t.Fatalf("%s = (%v, %q), want (cancelled, %q)", id, s, got, want)
	}
}

// A command holding over a quarter of the node's memory for work, in RAM or
// swap, while barely using a CPU for five minutes is stopped as stuck, its
// diagnostics taken first: it keeps that memory from all other work.
func TestACommandIdleOnMuchMemoryIsStopped(t *testing.T) {
	e, units, _, now := parkEngine(t)
	e.start(t, units, now, "hung-1", "a", "test")
	// 5 GiB of the node's 16 GiB, part of it swapped out.
	units.SetAnon("hotbed-hung-1", 3<<30)
	units.SetSwap("hotbed-hung-1", 2<<30)

	// First seen holding it 30s from now: five minutes on is 5m30s.
	stepFor(t, e, units, now, "hotbed-hung-1", 0.01, 5*time.Minute)
	wantRunning(t, e, "hung-1", "after 4m30s idle on 5 GiB")
	stepFor(t, e, units, now, "hotbed-hung-1", 0.01, 30*time.Second)
	wantStoppedAsStuck(t, e, "hung-1",
		"it held 5.0 GiB, over 25% of the 16.0 GiB this node has for work, for 5m0s while using 0.01 CPU cores on average: stopped as stuck, to free that memory")
}

// One that computes, however much it holds, runs on; once it stops
// computing it is stopped five minutes after.
func TestACommandBusyOnMuchMemoryRuns(t *testing.T) {
	e, units, _, now := parkEngine(t)
	e.start(t, units, now, "link-1", "a", "build")
	units.SetAnon("hotbed-link-1", 12<<30)

	stepFor(t, e, units, now, "hotbed-link-1", 1, 10*time.Minute)
	wantRunning(t, e, "link-1", "after 10m busy on 12 GiB")
	// Its average over the last five minutes falls below 0.1 cores only
	// once it has been idle for all but 30s of them.
	stepFor(t, e, units, now, "hotbed-link-1", 0, 4*time.Minute+30*time.Second)
	wantRunning(t, e, "link-1", "4m30s after it stopped computing")
	stepFor(t, e, units, now, "hotbed-link-1", 0, 30*time.Second)
	wantStoppedAsStuck(t, e, "link-1",
		"it held 12.0 GiB, over 25% of the 16.0 GiB this node has for work, for 5m30s while using 0.09 CPU cores on average: stopped as stuck, to free that memory")
}

// Holding less, or holding it with a break, does not count; nor does sitting
// idle frozen, as a dev server, an interactive command or a measurement,
// which may rightly wait on what they hold, or when its run asked to be kept.
func TestIdleOnMuchMemoryExemptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(units *fake.Units, c *command)
		want  pb.CommandState
	}{
		{"a quarter of the node or less", func(units *fake.Units, c *command) {
			units.SetAnon(c.unitName(), 4<<30)
		}, running},
		{"a dev server", func(_ *fake.Units, c *command) { c.cmd().Kind = "server" }, running},
		{"an interactive command", func(_ *fake.Units, c *command) { c.cmd().TtyInput = true }, running},
		{"a measurement", func(_ *fake.Units, c *command) { c.cmd().Bench = true }, running},
		{"run with --keep-idle", func(_ *fake.Units, c *command) { c.cmd().Limits.KeepIdle = true }, running},
		{"paused by its soft time limit", func(_ *fake.Units, c *command) { c.cmd().Limits.SoftLimitSeconds = 60 }, paused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, units, _, now := parkEngine(t)
			c := e.start(t, units, now, "idle-1", "a", "test")
			units.SetAnon("hotbed-idle-1", 6<<30)
			tc.setup(units, c)
			stepFor(t, e, units, now, "hotbed-idle-1", 0, 15*time.Minute)
			e.idleStops.Wait() // any stop it began
			if s, why := commandState(t, e, "idle-1"); s != tc.want {
				t.Fatalf("idle-1 is %v after 15m idle, want %v: %q", s, tc.want, why)
			}
		})
	}

	t.Run("a break in holding it starts the five minutes over", func(t *testing.T) {
		e, units, _, now := parkEngine(t)
		e.start(t, units, now, "idle-1", "a", "test")
		units.SetAnon("hotbed-idle-1", 6<<30)
		stepFor(t, e, units, now, "hotbed-idle-1", 0, 4*time.Minute+30*time.Second)
		units.SetAnon("hotbed-idle-1", 1<<30)
		stepFor(t, e, units, now, "hotbed-idle-1", 0, 30*time.Second)
		units.SetAnon("hotbed-idle-1", 6<<30)
		// Seen holding it again at the first of these steps.
		stepFor(t, e, units, now, "hotbed-idle-1", 0, 5*time.Minute)
		wantRunning(t, e, "idle-1", "4m30s after it again held 6 GiB")
		stepFor(t, e, units, now, "hotbed-idle-1", 0, 30*time.Second)
		wantStoppedAsStuck(t, e, "idle-1",
			"it held 6.0 GiB, over 25% of the 16.0 GiB this node has for work, for 5m0s while using 0.00 CPU cores on average: stopped as stuck, to free that memory")
	})
}

// A VM command is judged on what its own processes hold, not the share of
// its VM's footprint it is charged for admission: stopping it frees no more
// than its own.
func TestIdleOnMuchMemoryCountsAVMCommandsOwn(t *testing.T) {
	e, units, _, now := parkEngine(t)
	c := e.start(t, units, now, "vm-1", "a", "test")
	charged := unit.MemoryUse{Anon: 8 << 30}
	at := *now
	for i := range 12 {
		at = at.Add(30 * time.Second)
		st := unit.State{Loaded: true, Running: true, OwnMemoryUse: &unit.MemoryUse{Anon: 20 << 20}}
		e.mu.Lock()
		e.watchIdle(c, st, charged, true, at)
		marks := len(c.idle.marks)
		e.mu.Unlock()
		if marks != 0 {
			t.Fatalf("sample %d: watched as holding much, with 20 MiB of its own and 8 GiB charged", i)
		}
	}
	e.mu.Lock()
	e.watchIdle(c, unit.State{Loaded: true, Running: true, OwnMemoryUse: &unit.MemoryUse{Anon: 6 << 30}}, charged, true, at)
	marks, held := len(c.idle.marks), c.idle.held
	e.mu.Unlock()
	if marks != 1 || held != 6<<30 {
		t.Fatalf("with 6 GiB of its own: marks %d, held %s; want 1 mark of 6.0 GiB", marks, gib(held))
	}
}

// A VM command's status carries what its guest cgroup holds as its own,
// beside the share of its VM's footprint admission charges it.
func TestAVMCommandsStatusCarriesItsOwnMemory(t *testing.T) {
	st := toUnitState("ws", runtime.CommandStatus{ID: "c", Running: true, MemoryAnon: 20 << 20, MemorySwap: 4 << 20})
	if st.OwnMemoryUse == nil || *st.OwnMemoryUse != (unit.MemoryUse{Anon: 20 << 20, Swap: 4 << 20}) {
		t.Fatalf("own memory = %v, want 20 MiB anon, 4 MiB swap", st.OwnMemoryUse)
	}
}

// A unit seen frozen while its command is recorded running (mid-pause, or
// a stray freeze) uses no CPU by design: it starts the watch over.
func TestIdleOnMuchMemoryIgnoresAFrozenUnit(t *testing.T) {
	e, units, _, now := parkEngine(t)
	c := e.start(t, units, now, "frozen-1", "a", "test")
	at := now.Add(30 * time.Second)
	mem := unit.MemoryUse{Anon: 6 << 30}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.watchIdle(c, unit.State{Loaded: true, Running: true}, mem, true, at)
	if len(c.idle.marks) != 1 {
		t.Fatalf("marks of a running unit holding 6 GiB = %d, want 1", len(c.idle.marks))
	}
	e.watchIdle(c, unit.State{Loaded: true, Running: true, Frozen: true}, mem, true, at.Add(30*time.Second))
	if len(c.idle.marks) != 0 {
		t.Fatalf("marks once its unit is frozen = %d, want 0", len(c.idle.marks))
	}
}

// A CPU time that goes back (its unit replaced) starts the watch over, so
// the window never spans two counters.
func TestIdleWatchStartsOverWhenCPUTimeGoesBack(t *testing.T) {
	var w idleWatch
	t0 := time.Unix(1795400000, 0)
	w.see(true, 6<<30, t0, uint64(100*time.Second))
	w.see(true, 6<<30, t0.Add(4*time.Minute), uint64(100*time.Second))
	w.see(true, 6<<30, t0.Add(5*time.Minute), 0)
	if _, _, ok := w.stuck(); ok {
		t.Fatal("stuck right after its CPU time went back")
	}
	if len(w.marks) != 1 || !w.marks[0].at.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("marks after its CPU time went back = %v, want one at 5m", w.marks)
	}
	w.see(true, 6<<30, t0.Add(10*time.Minute), 0)
	if over, cores, ok := w.stuck(); !ok || over != 5*time.Minute || cores != 0 {
		t.Fatalf("stuck 5m after = (%s, %.2f, %v), want (5m0s, 0.00, true)", over, cores, ok)
	}
}
