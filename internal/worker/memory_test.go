package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// The node admits no more work than the memory its commands hold fits:
// a command with no guarantee reserves the most it has held of its own
// (anon and swap, not cache), and keeps reserving it once it holds less,
// until it ends (board 9128).
func TestAdmissionReservesWhatCommandsHeld(t *testing.T) {
	e, units, _, now := parkEngine(t)
	e.start(t, units, now, "build-1", "a", "build")
	// 14 GiB of a 16 GiB node: past the 86% (13.8 GiB) reservations may take.
	units.SetAnon("hotbed-build-1", 10<<30)
	units.SetSwap("hotbed-build-1", 4<<30)
	e.step(t.Context())
	if got := e.Heartbeat("cl").ReservedMemoryBytes; got != 14<<30 {
		t.Fatalf("reserved = %s, want 14.0 GiB, what build-1 held", gib(got))
	}

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "next-1", WorkspaceId: "b", Args: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-next-1") })
	e.step(t.Context())
	next := e.commands["next-1"].cmd()
	if want := "waiting for memory: needs 0.0 GiB, 0.0 GiB left of what running and parked commands hold or are guaranteed"; next.State != queued || next.Reason != want {
		t.Fatalf("next-1 beside 14 GiB held is %v: %q, want queued: %q", next.State, next.Reason, want)
	}
	if s := signal(e.Heartbeat("cl").Admission, "memory-reserved"); s.Value != float64(14<<30) || !s.Holding || s.Waiting != 1 {
		t.Errorf("memory-reserved signal = %v", s)
	}

	// Between links a build holds little, but needs it all again at the next.
	units.SetAnon("hotbed-build-1", 1<<30)
	units.SetSwap("hotbed-build-1", 0)
	e.step(t.Context())
	if s := next.State; s != queued {
		t.Fatalf("next-1 is %v once build-1 holds 1 GiB, want still queued behind what it held", s)
	}

	if _, err := e.Stop(t.Context(), "build-1", "done"); err != nil {
		t.Fatal(err)
	}
	e.step(t.Context())
	if s := next.State; s != running {
		t.Fatalf("next-1 is %v once build-1 ended: %q, want running", s, next.Reason)
	}
}

// A base build is guaranteed what the project's last one on the node
// held, so work admitted beside it leaves it room from its first step,
// before it holds any; and it waits itself while work holds too much for
// it to fit. Its guarantee is never more than reservations may take, so
// it can always start once the node drains (board 9128).
func TestABaseBuildIsGuaranteedWhatTheLastOneHeld(t *testing.T) {
	e, units, _, now := parkEngine(t)
	project := &pb.HostedProject{Name: "app", Repos: []*pb.Repo{{Id: "app", Url: "https://example.com/app.git"}}}
	h := heads{"app": strings.Repeat("a", 40)}

	// g1's freshen held 9 GiB; its base remembers, failed or not.
	g1 := &pb.Base{Id: baseSnapshot("app", 1), Project: "app", NodeId: "n", Generation: 1, State: building, Command: "f-app-g1"}
	c1 := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "f-app-g1", Kind: "freshen", State: failed},
		Project: "app", BaseId: g1.Id, PeakHeldBytes: 9 << 30,
	})
	e.mu.Lock()
	e.bases[g1.Id] = g1
	e.mu.Unlock()
	e.freshened(t.Context(), c1, false, 1, 0)
	if got := g1.PeakMemoryBytes; got != 9<<30 {
		t.Fatalf("g1's peak memory = %s, want 9.0 GiB, what its freshen held", gib(got))
	}

	// Work holding 5 GiB leaves 8.8 GiB: g2, needing 9, waits.
	e.start(t, units, now, "work-1", "a", "build")
	units.SetAnon("hotbed-work-1", 5<<30)
	e.step(t.Context())
	g2, err := e.startFreshen(t.Context(), project, 2, nil, h, "r2")
	if err != nil {
		t.Fatal(err)
	}
	f := e.commands[g2.Command]
	f.cmd().Args = []string{"sleep", "60"} // its script would fetch the repo
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+g2.Command) })
	if got := f.cmd().Limits.MemoryLowBytes; got != 9<<30 {
		t.Fatalf("g2's freshen is guaranteed %s, want 9.0 GiB, what g1's held", gib(got))
	}
	e.mu.Lock()
	kind, reason := e.room(f)
	e.mu.Unlock()
	if want := "waiting for memory: needs 9.0 GiB, 8.8 GiB left of what running and parked commands hold or are guaranteed"; kind != "memory" || reason != want {
		t.Fatalf("g2 beside 5 GiB of work: %s %q, want memory %q", kind, reason, want)
	}

	// Once the work holds less than it did it still reserves its peak; a
	// node that drains admits the base build, and then no more work than
	// fits beside its guarantee.
	if _, err := e.Stop(t.Context(), "work-1", "done"); err != nil {
		t.Fatal(err)
	}
	e.step(t.Context())
	if s := f.cmd().State; s != running {
		t.Fatalf("g2 on a drained node is %v: %q, want running", s, f.cmd().Reason)
	}
	work2 := e.start(t, units, now, "work-2", "b", "build")
	units.SetAnon("hotbed-work-2", 5<<30)
	e.step(t.Context())
	if s := work2.cmd().State; s != running {
		t.Fatalf("work-2 is %v", s)
	}
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "work-3", WorkspaceId: "c", Args: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-work-3") })
	e.step(t.Context())
	if w3 := e.commands["work-3"].cmd(); w3.State != queued || !strings.HasPrefix(w3.Reason, "waiting for memory") {
		t.Fatalf("work-3 beside g2's 9 GiB guarantee and work-2's 5 GiB is %v: %q, want waiting for memory", w3.State, w3.Reason)
	}

	// A guarantee past what reservations may take is cut to it.
	e.mu.Lock()
	g1.PeakMemoryBytes = 40 << 30
	got, limit := e.freshenGuarantee("app"), e.admitLimit()
	e.mu.Unlock()
	if got != limit {
		t.Errorf("guarantee after a 40 GiB build = %s, want %s, all reservations may take", gib(got), gib(limit))
	}
}

// Memory pressure parks the newest work although memory in use looks
// fine: the kernel counts as available the file pages a thrashing build
// faults straight back in, and ubu1 froze at 73% "in use" (board 9128).
// One park waits on the one before to show in pressure, and nothing is
// thawed while tasks still stall.
func TestMemoryPressureParks(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	old := e.start(t, units, now, "old-1", "a", "build")
	mid := e.start(t, units, now, "mid-1", "b", "build")
	young := e.start(t, units, now, "young-1", "c", "build")
	sys.SetUsed(sys.Mem.Total * 70 / 100)
	sys.Psi = 45
	e.step(t.Context())
	if s := young.cmd().State; s != parked {
		t.Fatalf("the newest is %v at 45%% memory pressure, want parked", s)
	}
	if want := "frozen at memory pressure 45% (70% memory in use); its memory goes to swap until there is room"; young.cmd().Reason != want {
		t.Errorf("reason = %q, want %q", young.cmd().Reason, want)
	}

	*now = now.Add(pressureSettle - time.Second)
	e.step(t.Context())
	if s := mid.cmd().State; s != running {
		t.Fatalf("the next is %v before the park could show in pressure, want running", s)
	}
	*now = now.Add(time.Second)
	e.step(t.Context())
	if s := mid.cmd().State; s != parked {
		t.Fatalf("the next is %v once pressure stayed at 45%% for %s, want parked", s, pressureSettle)
	}
	// The last one running is never parked.
	*now = now.Add(pressureSettle)
	e.step(t.Context())
	if s := old.cmd().State; s != running {
		t.Fatalf("the last one running is %v, want running", s)
	}

	// Memory fits, but tasks still stall: no thaw.
	sys.SetUsed(sys.Mem.Total * 30 / 100)
	sys.Psi = 30
	*now = now.Add(time.Second)
	e.step(t.Context())
	if s := mid.cmd().State; s != parked {
		t.Fatalf("a parked one is %v at 30%% memory pressure, want still parked", s)
	}
	sys.Psi = 5
	*now = now.Add(time.Second)
	e.step(t.Context())
	if s := young.cmd().State; s != running {
		t.Fatalf("the first in line is %v once pressure fell, want thawed", s)
	}
}
