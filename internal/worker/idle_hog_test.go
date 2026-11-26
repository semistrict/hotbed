package worker

import (
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
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

// A command holding over a quarter of the node's memory, in RAM or swap,
// while barely using a CPU for five minutes is stopped as stuck: it keeps
// that memory from all other work.
func TestACommandIdleOnMuchMemoryIsStopped(t *testing.T) {
	e, units, _, now := parkEngine(t)
	c := e.start(t, units, now, "hung-1", "a", "test")
	// 5 GiB of the 16 GiB node, part of it swapped out.
	units.SetAnon("hotbed-hung-1", 3<<30)
	units.SetSwap("hotbed-hung-1", 2<<30)

	// First seen holding it 30s from now: five minutes on is 5m30s.
	stepFor(t, e, units, now, "hotbed-hung-1", 0.01, 5*time.Minute)
	if s := c.cmd().State; s != running {
		t.Fatalf("hung-1 is %v after 4m30s idle on 5 GiB, want running: %q", s, c.cmd().Reason)
	}
	stepFor(t, e, units, now, "hotbed-hung-1", 0.01, 30*time.Second)
	want := "it held 5.0 GiB, over 25% of the node's 16.0 GiB, for 5m0s while using 0.01 CPU cores on average: stopped as stuck, to free that memory"
	if got := c.cmd(); got.State != cancelled || got.Reason != want {
		t.Fatalf("hung-1 after 5m idle on 5 GiB = (%v, %q), want (cancelled, %q)", got.State, got.Reason, want)
	}
}

// One that computes, however much it holds, runs on; once it stops
// computing it is stopped five minutes after.
func TestACommandBusyOnMuchMemoryRuns(t *testing.T) {
	e, units, _, now := parkEngine(t)
	c := e.start(t, units, now, "link-1", "a", "build")
	units.SetAnon("hotbed-link-1", 12<<30)

	stepFor(t, e, units, now, "hotbed-link-1", 1, 10*time.Minute)
	if s := c.cmd().State; s != running {
		t.Fatalf("link-1 is %v after 10m busy on 12 GiB, want running: %q", s, c.cmd().Reason)
	}
	// Its average over the last five minutes falls below 0.1 cores only
	// once it has been idle for all but 30s of them.
	stepFor(t, e, units, now, "hotbed-link-1", 0, 4*time.Minute+30*time.Second)
	if s := c.cmd().State; s != running {
		t.Fatalf("link-1 is %v 4m30s after it stopped computing, want running: %q", s, c.cmd().Reason)
	}
	stepFor(t, e, units, now, "hotbed-link-1", 0, 30*time.Second)
	want := "it held 12.0 GiB, over 25% of the node's 16.0 GiB, for 5m30s while using 0.09 CPU cores on average: stopped as stuck, to free that memory"
	if got := c.cmd(); got.State != cancelled || got.Reason != want {
		t.Fatalf("link-1 5m after it stopped computing = (%v, %q), want (cancelled, %q)", got.State, got.Reason, want)
	}
}

// Holding less, or holding it with a break, does not count; nor does sitting
// idle frozen, or as a dev server or an interactive command, which may
// rightly wait on what they hold.
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
		{"paused by its soft time limit", func(_ *fake.Units, c *command) { c.cmd().Limits.SoftLimitSeconds = 60 }, paused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, units, _, now := parkEngine(t)
			c := e.start(t, units, now, "idle-1", "a", "test")
			units.SetAnon("hotbed-idle-1", 6<<30)
			tc.setup(units, c)
			stepFor(t, e, units, now, "hotbed-idle-1", 0, 15*time.Minute)
			if s := c.cmd().State; s != tc.want {
				t.Fatalf("idle-1 is %v after 15m idle, want %v: %q", s, tc.want, c.cmd().Reason)
			}
		})
	}

	t.Run("a break in holding it starts the five minutes over", func(t *testing.T) {
		e, units, _, now := parkEngine(t)
		c := e.start(t, units, now, "idle-1", "a", "test")
		units.SetAnon("hotbed-idle-1", 6<<30)
		stepFor(t, e, units, now, "hotbed-idle-1", 0, 4*time.Minute+30*time.Second)
		units.SetAnon("hotbed-idle-1", 1<<30)
		stepFor(t, e, units, now, "hotbed-idle-1", 0, 30*time.Second)
		units.SetAnon("hotbed-idle-1", 6<<30)
		// Seen holding it again at the first of these steps.
		stepFor(t, e, units, now, "hotbed-idle-1", 0, 5*time.Minute)
		if s := c.cmd().State; s != running {
			t.Fatalf("idle-1 is %v 4m30s after it again held 6 GiB, want running: %q", s, c.cmd().Reason)
		}
		stepFor(t, e, units, now, "hotbed-idle-1", 0, 30*time.Second)
		if s := c.cmd().State; s != cancelled {
			t.Fatalf("idle-1 is %v 5m after it again held 6 GiB, want cancelled", s)
		}
	})
}
