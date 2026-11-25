package worker

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
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
	if want := "waiting for memory: running and parked commands hold or are guaranteed 14.0 GiB (limit 13.8 GiB)"; next.State != queued || next.Reason != want {
		t.Fatalf("next-1 beside 14 GiB held is %v: %q, want queued: %q", next.State, next.Reason, want)
	}
	if s := signal(e.Heartbeat("cl").Admission, "memory-reserved"); s.Value != float64(14<<30) || !s.Holding || s.Waiting != 1 {
		t.Errorf("memory-reserved signal = %v", s)
	}

	// A command with an explicit memory guarantee waiting while reservations
	// already exceed admitLimit also reports the over-budget state cleanly.
	withGuarantee := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id:          "next-guaranteed",
			WorkspaceId: "c",
			Limits:      &pb.CommandLimits{MemoryLowBytes: 2 << 30},
		},
	})
	e.mu.Lock()
	kind, reason := e.room(withGuarantee)
	e.mu.Unlock()
	if want := "waiting for memory: needs 2.0 GiB, but running and parked commands hold or are guaranteed 14.0 GiB (limit 13.8 GiB)"; kind != "memory" || reason != want {
		t.Fatalf("guaranteed command beside 14 GiB held = (%q, %q), want (memory, %q)", kind, reason, want)
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
	g2, err := e.startFreshen(t.Context(), project, 2, nil, "no_previous_base", h, "r2")
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

// A base freshen's guarantee comes from an earlier build's peak held memory
// (anon + swap), capped at admitLimit (86% of RAM), so other hotbed work
// leaves it room. Even on a completely idle node, OS/kernel/ZFS ARC overhead
// or a shared host's desktop routinely keeps MemAvailable below 86% of RAM
// (and fake.NewSystem itself has 12 GiB available of 16 GiB, below its
// 13.76 GiB admitLimit). The freshen must wait while other commands hold
// reservations, admit once the node drains despite want > MemAvailable, and
// still hold when the node is genuinely past parkAt (93%) or admitPressure
// (board 11078).
func TestFreshenAdmitsWhenIdleEvenIfGuaranteeExceedsMemAvailable(t *testing.T) {
	t.Run("freshen capped at admitLimit admits on idle node with lower MemAvailable", func(t *testing.T) {
		e, units, sys, now := parkEngine(t)
		// Default fake.NewSystem: 16 GiB total, 12 GiB available (25% used),
		// admitLimit = 13.76 GiB. Previous freshen peaked at 18 GiB (anon + swap).
		project := &pb.HostedProject{Name: "cladefs", Repos: []*pb.Repo{{Id: "cladefs", Url: "https://example.com/cladefs.git"}}}
		h := heads{"cladefs": strings.Repeat("b", 40)}

		g1 := &pb.Base{Id: baseSnapshot("cladefs", 1), Project: "cladefs", NodeId: "n", Generation: 1, State: warm, Command: "f-cladefs-g1", PeakMemoryBytes: 18 << 30}
		e.mu.Lock()
		e.bases[g1.Id] = g1
		e.mu.Unlock()

		// While other work holds memory, the freshen waits for the node to drain.
		e.start(t, units, now, "work-1", "a", "build")
		units.SetAnon("hotbed-work-1", 4<<30)
		e.step(t.Context())

		g2, err := e.startFreshen(t.Context(), project, 2, nil, "no_previous_base", h, "r2")
		if err != nil {
			t.Fatal(err)
		}
		f := e.commands[g2.Command]
		f.cmd().Args = []string{"sleep", "60"}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+g2.Command) })

		e.step(t.Context())
		if s := f.cmd().State; s != queued {
			t.Fatalf("freshen beside 4 GiB work is %v, want queued", s)
		}

		// Once work-1 ends, the node is idle (0 reserved, 12 GiB physically
		// available < 13.76 GiB guarantee): the freshen MUST admit.
		if _, err := e.Stop(t.Context(), "work-1", "done"); err != nil {
			t.Fatal(err)
		}
		e.step(t.Context())
		if s := f.cmd().State; s != running {
			t.Fatalf("freshen on drained node (needs %s, %s MemAvailable) is %v: %q, want running",
				gib(f.cmd().Limits.MemoryLowBytes), gib(sys.Mem.Available), s, f.cmd().Reason)
		}
	})

	t.Run("freshen below admitLimit but above MemAvailable admits on shared host", func(t *testing.T) {
		e, units, sys, _ := parkEngine(t)
		// Simulate a shared desktop host (e.g. asahimbp): 65% used (5.6 GiB
		// available of 16 GiB), 0% PSI, previous freshen held 9 GiB.
		sys.SetUsed(sys.Mem.Total * 65 / 100)
		project := &pb.HostedProject{Name: "app", Repos: []*pb.Repo{{Id: "app", Url: "https://example.com/app.git"}}}
		h := heads{"app": strings.Repeat("c", 40)}

		g1 := &pb.Base{Id: baseSnapshot("app", 1), Project: "app", NodeId: "n", Generation: 1, State: warm, Command: "f-app-g1", PeakMemoryBytes: 9 << 30}
		e.mu.Lock()
		e.bases[g1.Id] = g1
		e.mu.Unlock()

		g2, err := e.startFreshen(t.Context(), project, 2, nil, "no_previous_base", h, "r2")
		if err != nil {
			t.Fatal(err)
		}
		f := e.commands[g2.Command]
		f.cmd().Args = []string{"sleep", "60"}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+g2.Command) })

		e.step(t.Context())
		if s := f.cmd().State; s != running {
			t.Fatalf("9 GiB freshen on idle 65%%-used node is %v: %q, want running", s, f.cmd().Reason)
		}
	})

	t.Run("freshen still waits when node is past parkAt or under memory pressure", func(t *testing.T) {
		e, units, sys, _ := parkEngine(t)
		project := &pb.HostedProject{Name: "app", Repos: []*pb.Repo{{Id: "app", Url: "https://example.com/app.git"}}}
		h := heads{"app": strings.Repeat("d", 40)}

		g1 := &pb.Base{Id: baseSnapshot("app", 1), Project: "app", NodeId: "n", Generation: 1, State: warm, Command: "f-app-g1", PeakMemoryBytes: 18 << 30}
		e.mu.Lock()
		e.bases[g1.Id] = g1
		e.mu.Unlock()

		sys.SetUsed(sys.Mem.Total * 95 / 100)
		g2, err := e.startFreshen(t.Context(), project, 2, nil, "no_previous_base", h, "r2")
		if err != nil {
			t.Fatal(err)
		}
		f := e.commands[g2.Command]
		f.cmd().Args = []string{"sleep", "60"}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+g2.Command) })

		e.step(t.Context())
		if s := f.cmd().State; s != queued || !strings.HasPrefix(f.cmd().Reason, "waiting for memory") {
			t.Fatalf("freshen at 95%% memory used is %v: %q, want queued waiting for memory", s, f.cmd().Reason)
		}

		// Memory use drops under parkAt, but PSI is still past admitPressure: waits.
		sys.SetUsed(sys.Mem.Total * 30 / 100)
		sys.Psi = 30
		e.Sample()
		e.step(t.Context())
		if s := f.cmd().State; s != queued || !strings.Contains(f.cmd().Reason, "memory pressure") {
			t.Fatalf("freshen at 30%% memory PSI is %v: %q, want queued waiting for memory pressure", s, f.cmd().Reason)
		}

		// Pressure clears: freshen admits.
		sys.Psi = 0
		e.Sample()
		e.step(t.Context())
		if s := f.cmd().State; s != running {
			t.Fatalf("freshen once memory and pressure cleared is %v: %q, want running", s, f.cmd().Reason)
		}
	})

	t.Run("explicit --mem-low command still enforces physical headroom", func(t *testing.T) {
		e, units, sys, _ := parkEngine(t)
		// 10 GiB used of 16 GiB (62.5%, under parkAt) -> 6 GiB physically available.
		sys.SetUsed(10 << 30)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "explicit-8g", WorkspaceId: "a", Args: []string{"sleep", "60"},
			Limits: &pb.CommandLimits{MemoryLowBytes: 8 << 30},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-explicit-8g") })

		e.step(t.Context())
		cmd := e.commands["explicit-8g"].cmd()
		if cmd.State != queued || !strings.Contains(cmd.Reason, "physically available") {
			t.Fatalf("explicit 8 GiB command with 6 GiB available is %v: %q, want queued waiting for physically available memory", cmd.State, cmd.Reason)
		}
	})

	t.Run("cached sample keeps memory total and used from the same domain", func(t *testing.T) {
		e, _, sys, _ := parkEngine(t)
		// Simulate a cgroup-capped worker (10 GiB slice) whose fuller sampled
		// domain is the 18 GiB host with 12 GiB used (67% full, 6 GiB available).
		e.mu.Lock()
		e.resources.MemoryBytes = 10 << 30
		e.mu.Unlock()
		sys.Mem.Total = 18 << 30
		sys.Mem.Available = 6 << 30
		e.Sample()

		c := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "c-dom", Kind: "build", Limits: &pb.CommandLimits{MemoryLowBytes: 2 << 30}},
		})
		e.mu.Lock()
		kind, reason := e.room(c)
		e.mu.Unlock()
		if kind != "" {
			t.Fatalf("room() with 10 GiB slice and 12/18 GiB host sample = %q (%q), want admitted", kind, reason)
		}
	})

	t.Run("VM workspace commands are not deadlocked by host parkAt when PSI is low", func(t *testing.T) {
		e, _, sys, _ := parkEngine(t)
		e.cfg.Runtime = &trackingRuntime{}
		e.cfg.RuntimeFor = e.isVMWorkspace
		e.vmWorkspaces.Store("vm-ws", struct{}{})
		// Host at 96% used (past parkAt=93%) because the VM's own guest RAM is
		// resident, but zero memory pressure.
		sys.SetUsed(sys.Mem.Total * 96 / 100)
		sys.Psi = 0
		e.Sample()

		c := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "c-vm", WorkspaceId: "vm-ws", Kind: "shell"},
		})
		e.mu.Lock()
		kind, reason := e.room(c)
		e.mu.Unlock()
		if kind != "" {
			t.Fatalf("VM command at 96%% host memory and 0%% PSI = %q (%q), want admitted", kind, reason)
		}

		// Real memory pressure (PSI >= admitPressure) still holds VM commands.
		sys.Psi = 30
		e.Sample()
		e.mu.Lock()
		kind, reason = e.room(c)
		e.mu.Unlock()
		if kind != "pressure" {
			t.Fatalf("VM command at 30%% PSI = %q (%q), want pressure hold", kind, reason)
		}

		// When another command is already running on the node, a new VM command
		// in a different workspace must NOT bypass host parkAt (96% used).
		sys.Psi = 0
		e.Sample()
		e.mu.Lock()
		e.commands["other-run"] = e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "other-run", WorkspaceId: "other-ws", State: running},
		})
		kind, reason = e.room(c)
		e.mu.Unlock()
		if kind != "memory" {
			t.Fatalf("VM command at 96%% host memory with another running command = %q (%q), want memory hold", kind, reason)
		}
	})

	t.Run("queued same-or-lower priority work waits while work is already parked for memory", func(t *testing.T) {
		e, units, sys, now := parkEngine(t)
		_ = e.start(t, units, now, "run-1", "a", "build")
		p := e.start(t, units, now, "parked-1", "b", "build")

		// Park parked-1 due to memory pressure.
		sys.SetUsed(sys.Mem.Total * 95 / 100)
		e.step(t.Context())
		if p.cmd().State != parked {
			t.Fatalf("parked-1 = %v, want parked", p.cmd().State)
		}

		// Memory usage sits at 85% (above thawBelow=78%, below parkAt=93%) with 0% PSI:
		// parked-1 cannot thaw yet, so a brand-new same-priority command with 0 reservation
		// must wait in queued rather than piling on and getting immediately parked.
		sys.SetUsed(sys.Mem.Total * 85 / 100)
		sys.Psi = 0
		e.Sample()
		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "new-same", WorkspaceId: "c", Args: []string{"sleep", "60"}, Kind: "build",
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-new-same") })
		e.step(t.Context())

		if got := e.commands["new-same"].cmd(); got.State != queued || !strings.Contains(got.Reason, "parked") {
			t.Fatalf("new-same while parked-1 is parked = %v (%q), want queued waiting behind parked work", got.State, got.Reason)
		}

		// A strictly higher-priority command or a base freshen is NOT blocked by parked normal work.
		e.mu.Lock()
		high := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "high-q", WorkspaceId: "c", Kind: "build", Limits: &pb.CommandLimits{Priority: 10}},
		})
		highKind, highReason := e.room(high)
		freshen := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "f-q", Kind: "freshen"},
		})
		freshenKind, freshenReason := e.room(freshen)
		e.mu.Unlock()
		if highKind != "" {
			t.Errorf("high-priority command blocked by parked normal work: %q (%q)", highKind, highReason)
		}
		if freshenKind != "" {
			t.Errorf("base freshen blocked by parked normal work: %q (%q)", freshenKind, freshenReason)
		}
	})
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
	// Hysteresis: dipping to 18% (above 12% resume threshold) still holds.
	sys.Psi = 18
	*now = now.Add(time.Second)
	e.step(t.Context())
	if s := mid.cmd().State; s != parked {
		t.Fatalf("a parked one is %v at 18%% memory pressure (hysteresis), want still parked", s)
	}
	sys.Psi = 5
	*now = now.Add(time.Second)
	e.step(t.Context())
	if s := mid.cmd().State; s != running {
		t.Fatalf("the oldest-started parked command (mid) is %v once pressure fell, want thawed", s)
	}
	if s := young.cmd().State; s != parked {
		t.Fatalf("young is %v, want still parked behind older-started mid", s)
	}
}

func TestCommandAndShellMemoryBudgetEnv(t *testing.T) {
	const GiB = 1 << 30
	e, _, _, _ := parkEngine(t)
	e.mu.Lock()
	e.resources.MemoryBytes = 10 * GiB
	e.cfg.VM = &VMBackend{cfg: statedir.VMConfig{MemMiB: 6144}}
	e.cfg.Runtime = &trackingRuntime{}
	e.cfg.RuntimeFor = e.isVMWorkspace
	e.vmWorkspaces.Store("ws-vm", struct{}{})

	// Host-FUSE / chroot command (including base freshen) gets slice ceiling.
	hostCmd := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "c-host", Kind: "freshen", Args: []string{"true"}, Limits: &pb.CommandLimits{CpuWeight: 100}},
		Dir:     WorkPath,
		Volume:  "v-host",
	})
	hostSpec, err := e.spec(hostCmd)
	if err != nil {
		e.mu.Unlock()
		t.Fatalf("spec(hostCmd): %v", err)
	}
	if got, want := envValue(hostSpec.Env, "HOTBED_MEMORY_BYTES"), strconv.FormatInt(10*GiB, 10); got != want {
		e.mu.Unlock()
		t.Fatalf("host command HOTBED_MEMORY_BYTES = %q, want %q", got, want)
	}

	// Explicit --mem-high tighter than the slice tightens HOTBED_MEMORY_BYTES.
	cappedCmd := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "c-cap", Kind: "build", Args: []string{"true"}, Limits: &pb.CommandLimits{CpuWeight: 100, MemoryHighBytes: 4 * GiB}},
		Dir:     WorkPath,
		Volume:  "v-host",
	})
	cappedSpec, err := e.spec(cappedCmd)
	if err != nil {
		e.mu.Unlock()
		t.Fatalf("spec(cappedCmd): %v", err)
	}
	if got, want := envValue(cappedSpec.Env, "HOTBED_MEMORY_BYTES"), strconv.FormatInt(4*GiB, 10); got != want {
		e.mu.Unlock()
		t.Fatalf("capped command HOTBED_MEMORY_BYTES = %q, want %q", got, want)
	}

	// VM command and VM freshen get the Firecracker guest RAM size.
	vmCmd := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "c-vm", WorkspaceId: "ws-vm", Kind: "build", Args: []string{"true"}, Limits: &pb.CommandLimits{CpuWeight: 100}},
		Dir:     WorkPath,
		Volume:  "v-vm",
	})
	vmSpec, err := e.spec(vmCmd)
	if err != nil {
		e.mu.Unlock()
		t.Fatalf("spec(vmCmd): %v", err)
	}
	if got, want := envValue(vmSpec.Env, "HOTBED_MEMORY_BYTES"), strconv.FormatInt(6*GiB, 10); got != want {
		e.mu.Unlock()
		t.Fatalf("VM command HOTBED_MEMORY_BYTES = %q, want %q", got, want)
	}

	vmFreshen := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "f-vm", Kind: "freshen", Args: []string{"true"}, Limits: &pb.CommandLimits{CpuWeight: 100}},
		Dir:     WorkPath,
	})
	vmFreshen.managedFreshen = &managedFreshen{vmID: "base-vm-1"}
	vmFreshenSpec, err := e.spec(vmFreshen)
	if err != nil {
		e.mu.Unlock()
		t.Fatalf("spec(vmFreshen): %v", err)
	}
	if got, want := envValue(vmFreshenSpec.Env, "HOTBED_MEMORY_BYTES"), strconv.FormatInt(6*GiB, 10); got != want {
		e.mu.Unlock()
		t.Fatalf("VM freshen HOTBED_MEMORY_BYTES = %q, want %q", got, want)
	}

	// Shells receive the same budget.
	hostWS := &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "ws-host", SessionId: "s1"}, Volume: "v-host"}}
	hostShellSpec, _ := e.shellSpec("sh1", hostWS, "", []string{"bash"}, nil, nil, false)
	if got, want := envValue(hostShellSpec.Env, "HOTBED_MEMORY_BYTES"), strconv.FormatInt(10*GiB, 10); got != want {
		e.mu.Unlock()
		t.Fatalf("host shell HOTBED_MEMORY_BYTES = %q, want %q", got, want)
	}
	vmWS := &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "ws-vm", SessionId: "s1"}, Volume: "v-vm"}}
	vmShellSpec, _ := e.shellSpec("sh2", vmWS, "", []string{"bash"}, nil, nil, false)
	e.mu.Unlock()
	if got, want := envValue(vmShellSpec.Env, "HOTBED_MEMORY_BYTES"), strconv.FormatInt(6*GiB, 10); got != want {
		t.Fatalf("VM shell HOTBED_MEMORY_BYTES = %q, want %q", got, want)
	}
}

// When running or parked reservations already equal admitLimit (room == 0),
// such as a base freshen whose guarantee was capped at admitLimit, a new
// command with no explicit --mem-low (want == 0) must NOT sneak onto the node:
// same-priority work waits, higher-priority work preempts (parks) the
// lower-priority holder instead of piling on top of it, preemption does not
// disturb lower-priority work when non-reclaimable reservations already fill
// admitLimit, and a parked want == 0 command cannot thaw alongside a full-node
// reservation.
func TestFullAdmitLimitReservationBlocksZeroReservationWork(t *testing.T) {
	t.Run("same-priority want==0 waits behind reservation == admitLimit", func(t *testing.T) {
		e, units, _, now := parkEngine(t)
		project := &pb.HostedProject{Name: "cladefs", Repos: []*pb.Repo{{Id: "cladefs", Url: "https://example.com/cladefs.git"}}}
		h := heads{"cladefs": strings.Repeat("d", 40)}

		// Previous freshen peaked past admitLimit (13.76 GiB on 16 GiB node),
		// so the next freshen reserves exactly admitLimit (room == 0).
		g1 := &pb.Base{Id: baseSnapshot("cladefs", 1), Project: "cladefs", NodeId: "n", Generation: 1, State: warm, Command: "f-cladefs-g1", PeakMemoryBytes: 18 << 30}
		e.mu.Lock()
		e.bases[g1.Id] = g1
		limit := e.admitLimit()
		e.mu.Unlock()

		g2, err := e.startFreshen(t.Context(), project, 2, nil, "no_previous_base", h, "r2")
		if err != nil {
			t.Fatal(err)
		}
		f := e.commands[g2.Command]
		f.cmd().Args = []string{"sleep", "60"}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+g2.Command) })
		e.step(t.Context())
		if f.cmd().State != running {
			t.Fatalf("freshen state = %v (%q), want running", f.cmd().State, f.cmd().Reason)
		}
		if got := f.cmd().Limits.MemoryLowBytes; got != limit {
			t.Fatalf("freshen MemoryLowBytes = %s, want admitLimit %s", gib(got), gib(limit))
		}

		// Queue a same-priority command with want == 0 (no --mem-low).
		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "work-zero", WorkspaceId: "a", Args: []string{"sleep", "60"}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-work-zero") })
		e.step(t.Context())
		w := e.commands["work-zero"].cmd()
		wantReason := "waiting for memory: running and parked commands hold or are guaranteed " + gib(limit) + " (limit " + gib(limit) + ")"
		if w.State != queued || w.Reason != wantReason {
			t.Fatalf("work-zero beside full-admitLimit freshen = (%v, %q), want (queued, %q)", w.State, w.Reason, wantReason)
		}

		// Once the freshen ends, work-zero admits cleanly.
		if _, err := e.Stop(t.Context(), g2.Command, "done"); err != nil {
			t.Fatal(err)
		}
		e.step(t.Context())
		if w.State != running {
			t.Fatalf("work-zero after freshen ended = %v (%q), want running", w.State, w.Reason)
		}
	})

	t.Run("higher-priority want==0 preempts lower-priority reservation == admitLimit", func(t *testing.T) {
		e, units, sys, now := parkEngine(t)
		sys.SetUsed(0)
		e.mu.Lock()
		limit := e.admitLimit()
		e.mu.Unlock()

		// Low-priority command reserves exactly admitLimit (room == 0).
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "low-full", WorkspaceId: "a", Args: []string{"sleep", "60"},
			Limits: &pb.CommandLimits{Priority: 0, MemoryLowBytes: limit},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-low-full") })
		e.step(t.Context())
		low := e.commands["low-full"]
		if low.cmd().State != running {
			t.Fatalf("low-full state = %v (%q), want running", low.cmd().State, low.cmd().Reason)
		}

		// Urgent command with want == 0 must park low-full rather than piling
		// onto the node concurrently with it.
		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "urgent-zero", WorkspaceId: "b", Args: []string{"sleep", "60"},
			Limits: &pb.CommandLimits{Priority: 20},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-urgent-zero") })
		e.step(t.Context())
		if low.cmd().State != parked {
			t.Fatalf("low-full state = %v, want parked by urgent-zero", low.cmd().State)
		}
		if urgent := e.commands["urgent-zero"].cmd(); urgent.State != running {
			t.Fatalf("urgent-zero state = %v (%q), want running", urgent.State, urgent.Reason)
		}
	})

	t.Run("preemptCandidatesLocked skips victims when non-reclaimable reservation == admitLimit", func(t *testing.T) {
		e, _, _, _ := parkEngine(t)
		e.mu.Lock()
		defer e.mu.Unlock()
		limit := e.admitLimit()

		// Non-reclaimable server holds exactly admitLimit; a low-priority build
		// holds 0; a high-priority want == 0 command must not preempt the build
		// since it still cannot fit beside the server.
		e.commands["srv-full"] = e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "srv-full", Kind: "server", State: running, Limits: &pb.CommandLimits{Priority: 0, MemoryLowBytes: limit}},
		})
		e.commands["low-build"] = e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "low-build", Kind: "build", State: running, Limits: &pb.CommandLimits{Priority: 0}},
		})
		high := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "high-zero", Kind: "build", State: queued, Limits: &pb.CommandLimits{Priority: 10}},
		})
		if victims := e.preemptCandidatesLocked(high); len(victims) != 0 {
			t.Fatalf("preemptCandidatesLocked returned %d victim(s), want 0 when non-reclaimable reservation == admitLimit", len(victims))
		}
	})

	t.Run("canThawAlongsideLocked blocks want==0 parked command beside reservation == admitLimit", func(t *testing.T) {
		e, _, sys, _ := parkEngine(t)
		e.mu.Lock()
		defer e.mu.Unlock()
		limit := e.admitLimit()

		runner := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "run-full", Kind: "freshen", State: running, Limits: &pb.CommandLimits{Priority: 0, MemoryLowBytes: limit}},
		})
		parkedCmd := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "parked-zero", Kind: "build", State: parked, Limits: &pb.CommandLimits{Priority: 0}},
		})
		e.commands["run-full"] = runner
		e.commands["parked-zero"] = parkedCmd
		if e.canThawAlongsideLocked(parkedCmd, []*command{runner}, sys.Mem) {
			t.Fatal("canThawAlongsideLocked = true, want false when running command already reserves admitLimit")
		}
	})
}
