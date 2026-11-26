package worker

import (
	"fmt"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/unit"
)

// stepFor steps e every 30 seconds of its clock for d, adding to unit the
// CPU time that many cores busy would use.
func stepFor(t *testing.T, e *Engine, units *fake.Units, now *time.Time, unit string, cores float64, d time.Duration) {
	t.Helper()
	const every = 30 * time.Second
	for range d / every {
		*now = now.Add(every)
		units.AddCPU(unit, time.Duration(cores*float64(every)))
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

// wantStoppedAsWasteful waits for the background stop of command id and
// fails t unless it ended cancelled, the node's intervention saying it kept
// reserved of the node's 16 GiB for over at cores on average.
func wantStoppedAsWasteful(t *testing.T, e *Engine, id, over, reserved, cores string) {
	t.Helper()
	e.idleStops.Wait()
	want := fmt.Sprintf(`stopped by the node asahimbp: "for %s it kept %s of the 16.0 GiB this node has for work, over 25%%, while using %s CPU cores on average, under one core for each 6 GiB: stopped to free that memory for the work queued behind it (hotbed run --keep-idle keeps a command that rightly holds much memory for little CPU)". `+
		"No diagnostics: this worker takes no diagnostics: process inspection is not configured on it.", over, reserved, cores)
	if s, got := commandState(t, e, id); s != cancelled || got != want {
		t.Fatalf("%s = (%v, %q), want (cancelled, %q)", id, s, got, want)
	}
}

// A command reserving over a quarter of the node's memory for work while
// keeping less than a core busy for each 6 GiB of it for five minutes is
// stopped, its diagnostics taken first: it keeps other work queued for little.
// One busy enough runs on.
func TestACommandUsingLittleCPUForMuchMemoryIsStopped(t *testing.T) {
	for _, tc := range []struct {
		cores float64
		want  pb.CommandState
	}{
		// 12 GiB needs 2 cores.
		{2.1, running},
		{1.9, cancelled},
	} {
		t.Run(fmt.Sprintf("%.1f cores on 12 GiB", tc.cores), func(t *testing.T) {
			e, units, _, now := parkEngine(t)
			e.start(t, units, now, "test-1", "a", "test")
			// 12 GiB of the node's 16 GiB, part of it swapped out.
			units.SetAnon("hotbed-test-1", 9<<30)
			units.SetSwap("hotbed-test-1", 3<<30)

			// First seen holding it 30s from now: five minutes on is 5m30s.
			stepFor(t, e, units, now, "hotbed-test-1", tc.cores, 5*time.Minute)
			wantRunning(t, e, "test-1", fmt.Sprintf("after 4m30s at %.1f cores on 12 GiB", tc.cores))
			stepFor(t, e, units, now, "hotbed-test-1", tc.cores, 30*time.Second)
			if tc.want == running {
				stepFor(t, e, units, now, "hotbed-test-1", tc.cores, 10*time.Minute)
				e.idleStops.Wait()
				wantRunning(t, e, "test-1", fmt.Sprintf("after 15m at %.1f cores on 12 GiB", tc.cores))
				return
			}
			wantStoppedAsWasteful(t, e, "test-1", "5m0s", "12.0 GiB", "1.90")
		})
	}
}

// What counts is what it reserves, which is what makes other work queue,
// not what it holds now: the most it has held (as a VM keeps from the host
// what its guest is done with), or the memory it was guaranteed.
func TestWhatACommandReservesCounts(t *testing.T) {
	t.Run("the most it held", func(t *testing.T) {
		e, units, _, now := parkEngine(t)
		e.start(t, units, now, "tests-1", "a", "test")
		units.SetAnon("hotbed-tests-1", 10<<30)
		stepFor(t, e, units, now, "hotbed-tests-1", 4, 30*time.Second)
		units.SetAnon("hotbed-tests-1", 512<<20)
		stepFor(t, e, units, now, "hotbed-tests-1", 1, 4*time.Minute+30*time.Second)
		wantRunning(t, e, "tests-1", "4m30s after it held 10 GiB")
		stepFor(t, e, units, now, "hotbed-tests-1", 1, 30*time.Second)
		wantStoppedAsWasteful(t, e, "tests-1", "5m0s", "10.0 GiB", "1.00")
	})
	t.Run("its guarantee", func(t *testing.T) {
		e, units, _, now := parkEngine(t)
		c := e.start(t, units, now, "lean-1", "a", "test")
		e.mu.Lock()
		c.cmd().Limits.MemoryLowBytes = 6 << 30
		e.mu.Unlock()
		stepFor(t, e, units, now, "hotbed-lean-1", 0.5, 5*time.Minute)
		wantRunning(t, e, "lean-1", "after 4m30s guaranteed 6 GiB at 0.5 cores")
		stepFor(t, e, units, now, "hotbed-lean-1", 0.5, 30*time.Second)
		wantStoppedAsWasteful(t, e, "lean-1", "5m0s", "6.0 GiB", "0.50")
	})
}

// Its average is taken over the last five minutes: once one that computed
// slows down, it is stopped as soon as that average falls below the line.
func TestACommandThatStopsComputingIsStoppedOnceItsAverageFalls(t *testing.T) {
	e, units, _, now := parkEngine(t)
	e.start(t, units, now, "link-1", "a", "build")
	units.SetAnon("hotbed-link-1", 12<<30)

	stepFor(t, e, units, now, "hotbed-link-1", 2.5, 10*time.Minute)
	wantRunning(t, e, "link-1", "after 10m at 2.5 cores on 12 GiB")
	// Its marks are a minute apart, from 30s after it started: 1m after it
	// stops computing, the window from 5m30s still averages 11.25 core-minutes
	// over 5.5 minutes (2.05 cores); 30s later, from 6m30s, 8.75 over 5 (1.75).
	stepFor(t, e, units, now, "hotbed-link-1", 0, time.Minute)
	wantRunning(t, e, "link-1", "1m after it stopped computing")
	stepFor(t, e, units, now, "hotbed-link-1", 0, 30*time.Second)
	wantStoppedAsWasteful(t, e, "link-1", "5m0s", "12.0 GiB", "1.75")
}

// Reserving a quarter of the node or less does not count; nor does sitting
// idle frozen, as a dev server, an interactive command or a measurement,
// which may rightly wait on what they hold, or when its run asked to be kept.
func TestUsingLittleCPUForMuchMemoryExemptions(t *testing.T) {
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
			e.mu.Lock()
			tc.setup(units, c)
			e.mu.Unlock()
			stepFor(t, e, units, now, "hotbed-idle-1", 0, 15*time.Minute)
			e.idleStops.Wait() // any stop it began
			if s, why := commandState(t, e, "idle-1"); s != tc.want {
				t.Fatalf("idle-1 is %v after 15m idle, want %v: %q", s, tc.want, why)
			}
		})
	}
}

// A unit seen frozen while its command is recorded running (mid-pause, or
// a stray freeze) uses no CPU by design: it starts the watch over.
func TestUsingLittleCPUIgnoresAFrozenUnit(t *testing.T) {
	e, units, _, now := parkEngine(t)
	c := e.start(t, units, now, "frozen-1", "a", "test")
	at := now.Add(30 * time.Second)
	e.mu.Lock()
	defer e.mu.Unlock()
	c.rec.PeakHeldBytes = 6 << 30
	e.watchIdle(c, unit.State{Loaded: true, Running: true}, at)
	if len(c.idle.marks) != 1 {
		t.Fatalf("marks of a running unit reserving 6 GiB = %d, want 1", len(c.idle.marks))
	}
	e.watchIdle(c, unit.State{Loaded: true, Running: true, Frozen: true}, at.Add(30*time.Second))
	if len(c.idle.marks) != 0 {
		t.Fatalf("marks once its unit is frozen = %d, want 0", len(c.idle.marks))
	}
}

// A CPU time that goes back (its unit replaced) starts the watch over, so
// the window never spans two counters.
func TestIdleWatchStartsOverWhenCPUTimeGoesBack(t *testing.T) {
	var w idleWatch
	t0 := time.Unix(1795400000, 0)
	mark := func(d time.Duration, cpu time.Duration) cpuMark {
		return cpuMark{at: t0.Add(d), nanos: uint64(cpu), reserved: 6 << 30}
	}
	w.see(true, mark(0, 100*time.Second))
	w.see(true, mark(4*time.Minute, 100*time.Second))
	w.see(true, mark(5*time.Minute, 0))
	if _, _, _, ok := w.wasteful(); ok {
		t.Fatal("wasteful right after its CPU time went back")
	}
	if len(w.marks) != 1 || !w.marks[0].at.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("marks after its CPU time went back = %v, want one at 5m", w.marks)
	}
	w.see(true, mark(10*time.Minute, 0))
	if over, cores, reserved, ok := w.wasteful(); !ok || over != 5*time.Minute || cores != 0 || reserved != 6<<30 {
		t.Fatalf("5m after = (%s, %.2f, %s, %v), want (5m0s, 0.00, 6.0 GiB, true)", over, cores, gib(reserved), ok)
	}
}
