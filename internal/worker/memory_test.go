package worker

import (
	"context"
	"os"
	"path/filepath"
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
	// 17 GiB of a 16 GiB slice: past the 16.0 GiB reservations may take.
	units.SetAnon("hotbed-build-1", 12<<30)
	units.SetSwap("hotbed-build-1", 5<<30)
	e.step(t.Context())
	if got := e.Heartbeat("cl").ReservedMemoryBytes; got != 17<<30 {
		t.Fatalf("reserved = %s, want 17.0 GiB, what build-1 held", gib(got))
	}

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "next-1", WorkspaceId: "b", Args: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-next-1") })
	e.step(t.Context())
	next := e.commands["next-1"].cmd()
	if want := "waiting for memory: needs 2.0 GiB, but running commands hold or are guaranteed 17.0 GiB (limit 16.0 GiB)"; next.State != queued || next.Reason != want {
		t.Fatalf("next-1 beside 17 GiB held is %v: %q, want queued: %q", next.State, next.Reason, want)
	}
	if s := signal(e.Heartbeat("cl").Admission, "memory-reserved"); s.Value != float64(17<<30) || !s.Holding || s.Waiting != 1 {
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
	if want := "waiting for memory: needs 2.0 GiB, but running commands hold or are guaranteed 17.0 GiB (limit 16.0 GiB)"; kind != "memory" || reason != want {
		t.Fatalf("guaranteed command beside 17 GiB held = (%q, %q), want (memory, %q)", kind, reason, want)
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

	// Work holding 8 GiB leaves 8.0 GiB: g2, needing 9, waits.
	e.start(t, units, now, "work-1", "a", "build")
	units.SetAnon("hotbed-work-1", 8<<30)
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
	if want := "waiting for memory: needs 9.0 GiB, 8.0 GiB left of what running commands hold or are guaranteed"; kind != "memory" || reason != want {
		t.Fatalf("g2 beside 8 GiB of work: %s %q, want memory %q", kind, reason, want)
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
	units.SetAnon("hotbed-work-2", 6<<30)
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
		t.Fatalf("work-3 beside g2's 9 GiB guarantee and work-2's 6 GiB is %v: %q, want waiting for memory", w3.State, w3.Reason)
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

}

// Memory full or tasks stalling for it freezes nothing already running: it
// swaps, and new work waits until the pressure falls.
func TestMemoryPressureFreezesNothing(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	cmds := []*command{
		e.start(t, units, now, "old-1", "a", "build"),
		e.start(t, units, now, "mid-1", "b", "build"),
	}
	sys.SetUsed(sys.Mem.Total * 95 / 100)
	sys.Psi = 45
	for range 5 {
		*now = now.Add(30 * time.Second)
		e.step(t.Context())
	}
	for _, c := range cmds {
		if s := c.cmd().State; s != running {
			t.Fatalf("%s is %v at 95%% memory and 45%% pressure, want running", c.cmd().Id, s)
		}
	}
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "new-1", WorkspaceId: "c", Args: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-new-1") })
	e.step(t.Context())
	if w := e.commands["new-1"].cmd(); w.State != queued || w.Reason != "waiting for memory: 95% in use (0.8 GiB physically available on the node)" {
		t.Fatalf("new-1 = (%v, %q), want queued waiting for memory", w.State, w.Reason)
	}
}

func TestCommandAndShellMemoryBudgetEnv(t *testing.T) {
	const GiB = 1 << 30
	e, _, _, _ := parkEngine(t)
	e.mu.Lock()
	e.resources.MemoryBytes = 10 * GiB
	e.cfg.VM = newVMBackend("", statedir.VMConfig{MemMiB: 6144})
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

// When running reservations already equal admitLimit (room == 0), such as a
// base freshen whose guarantee was capped at admitLimit, a new command with no
// explicit --mem-low (want == 0) must NOT sneak onto the node, whatever its
// priority: it waits, and nothing running is preempted for it.
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
		wantReason := "waiting for memory: needs 2.0 GiB, but running commands hold or are guaranteed " + gib(limit) + " (limit " + gib(limit) + ")"
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

	t.Run("higher-priority want==0 waits too: nothing is preempted", func(t *testing.T) {
		e, units, sys, now := parkEngine(t)
		sys.SetUsed(0)
		e.mu.Lock()
		limit := e.admitLimit()
		e.mu.Unlock()

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

		*now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "urgent-zero", WorkspaceId: "b", Args: []string{"sleep", "60"},
			Limits: &pb.CommandLimits{Priority: 20},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-urgent-zero") })
		e.step(t.Context())
		if low.cmd().State != running {
			t.Fatalf("low-full state = %v, want still running", low.cmd().State)
		}
		if urgent := e.commands["urgent-zero"].cmd(); urgent.State != queued {
			t.Fatalf("urgent-zero state = %v (%q), want queued", urgent.State, urgent.Reason)
		}
	})
}

// A command that asked for no guarantee is charged defaultReservation until
// it holds more, so builds that start small are not all let in at once only
// to grow past the node together; what it holds beyond that is charged.
func TestAdmissionAssumesADefaultReservationThatOnlyGrows(t *testing.T) {
	e, units, sys, now := parkEngine(t)
	sys.SetUsed(0)
	// Shrink slice budget to 7.5 GiB: three default 2 GiB reservations fit (6 GiB),
	// leaving 1.5 GiB so a fourth waits.
	e.mu.Lock()
	e.resources.MemoryBytes = 7<<30 + 512<<20
	e.mu.Unlock()
	for _, id := range []string{"b-1", "b-2", "b-3"} {
		ws := map[string]string{"b-1": "a", "b-2": "b", "b-3": "c"}[id]
		e.start(t, units, now, id, ws, "build")
	}
	e.mu.Lock()
	if got := e.reserved(); got != 3*defaultReservation {
		t.Fatalf("reserved = %s, want %s", gib(got), gib(3*defaultReservation))
	}
	e.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(e.cfg.Store.Path("ws/d"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.workspaces["d"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "d", Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle}, Volume: "ws/d"}}
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "b-4", WorkspaceId: "d", Args: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-b-4") })
	e.step(t.Context())
	if w := e.commands["b-4"].cmd(); w.State != queued || w.Reason != "waiting for memory: needs 2.0 GiB, 1.5 GiB left of what running commands hold or are guaranteed" {
		t.Fatalf("b-4 = (%v, %q), want queued for memory", w.State, w.Reason)
	}

	// b-1 holds 6 GiB: its reservation grows to that, and stays there when it
	// holds less again.
	units.SetAnon("hotbed-b-1", 6<<30)
	e.step(t.Context())
	units.SetAnon("hotbed-b-1", 1<<30)
	e.step(t.Context())
	e.mu.Lock()
	defer e.mu.Unlock()
	if got := e.reservation(e.commands["b-1"]); got != 6<<30 {
		t.Fatalf("b-1 reservation = %s, want 6.0 GiB", gib(got))
	}
	if got := e.reservation(e.commands["b-2"]); got != defaultReservation {
		t.Fatalf("b-2 reservation = %s, want the default %s", gib(got), gib(defaultReservation))
	}
}

// cladefsd's DAX extent cache lives in memfd/shmem, which Linux /proc/meminfo
// excludes from MemAvailable even though cladefsd evicts everything above its
// protected floor on demand. Both host-domain admission checks and reported
// heartbeat usage must credit reclaimable DAX cache above floor, while cgroup
// slice-domain readings (which exclude cladefsd) must not double-credit it.
func TestReclaimableCladefsDAXCacheCreditedOnHostDomain(t *testing.T) {
	const GiB = int64(1 << 30)
	e, units, sys, now := parkEngine(t)

	// Simulate ubu1: 28 GiB physical RAM, 18 GiB work slice (backstop.High),
	// 27 GiB raw host used (~96.4% >= fullAt 95%) because cladefsd holds
	// 9 GiB of DAX cache above its 3.5 GiB floor (5.5 GiB reclaimable).
	e.mu.Lock()
	e.resources.MemoryBytes = 18 * GiB
	e.objectStore = &pb.ObjectStoreStats{
		ExtentCacheBytes:      uint64(9 * GiB),
		ExtentCacheFloorBytes: uint64(3*GiB + 512<<20),
		ExtentCacheLimitBytes: uint64(12 * GiB),
	}
	e.mu.Unlock()
	sys.Mem.Total = 28 * GiB
	sys.Mem.Available = 1 * GiB // 27 GiB raw used (96.4%), 6.5 GiB effective available (76.8% used)
	sys.Mem.Domain = ""
	e.Sample()

	// Heartbeat MemoryUsedBytes must project effective available RAM (6.5 GiB)
	// against the 18 GiB slice budget (18 - 6.5 = 11.5 GiB), never raw 27 GiB.
	if got, want := e.Heartbeat("cl").Usage.GetMemoryUsedBytes(), 11*GiB+512<<20; got != want {
		t.Fatalf("reported MemoryUsedBytes = %s, want %s (18 GiB slice minus 6.5 GiB effective avail)", gib(got), gib(want))
	}

	// Start one command, then admit a second with explicit --mem-low 4G:
	// raw MemAvailable is only 1 GiB, but effective available is 6.5 GiB.
	_ = e.start(t, units, now, "run-1", "a", "build")
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "run-2", WorkspaceId: "b", Args: []string{"sleep", "60"},
		Limits: &pb.CommandLimits{MemoryLowBytes: 4 * GiB},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-run-2") })
	e.step(t.Context())
	if got := e.commands["run-2"].cmd(); got.State != running {
		t.Fatalf("run-2 with 5.5 GiB reclaimable DAX cache = %v (%q), want running", got.State, got.Reason)
	}

	// Once cladefsd cache is at or below its floor, zero credit applies and
	// 96.4% host RAM used still holds new work at fullAt.
	e.mu.Lock()
	e.objectStore.ExtentCacheBytes = uint64(3 * GiB)
	e.mu.Unlock()
	e.Sample()
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "wait-floor", WorkspaceId: "c", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-wait-floor") })
	e.step(t.Context())
	if got := e.commands["wait-floor"].cmd(); got.State != queued || !strings.Contains(got.Reason, "in use") {
		t.Fatalf("wait-floor at 96%% host RAM with cache below floor = %v (%q), want queued at fullAt", got.State, got.Reason)
	}

	// On a cgroup slice domain (mem.Domain != ""), cladefsd runs outside the
	// slice so warm DAX cache above floor must NOT be subtracted from slice use.
	e.mu.Lock()
	e.objectStore.ExtentCacheBytes = uint64(9 * GiB)
	e.mu.Unlock()
	sys.Mem.Total = 18 * GiB
	sys.Mem.Available = 512 << 20 // 97.2% of slice used
	sys.Mem.Domain = "/user.slice/user-1000-hotbed.slice"
	e.Sample()
	e.step(t.Context())
	if got := e.commands["wait-floor"].cmd(); got.State != queued || !strings.Contains(got.Reason, "in use") {
		t.Fatalf("wait-floor at 97%% slice RAM = %v (%q), want queued (no DAX credit on slice domain)", got.State, got.Reason)
	}
}
