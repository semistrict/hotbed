package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/bench"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/unit"
)

func benchEngine(t *testing.T, iso *fake.Isolation) *Engine {
	t.Helper()
	store := &fake.Store{Root: t.TempDir()}
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp", Bench: iso,
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// A worker that stopped mid-measurement, the measurement gone since, gives
// the node back when it starts again.
func TestRestartGivesBackAnIsolationNoMeasurementHolds(t *testing.T) {
	iso := fake.NewIsolation("7-10", 7)
	iso.SetActive(true)
	e := benchEngine(t, iso)
	e.settleBench(t.Context())
	if iso.Active() || iso.Offs() != 1 {
		t.Errorf("active %v after %d Offs, want given back once", iso.Active(), iso.Offs())
	}
	if got := e.resources.BenchCpus; got != "7-10" {
		t.Errorf("the node reports bench CPUs %q, want 7-10", got)
	}
}

// One that finds its measurement still running keeps it isolated.
func TestRestartKeepsARunningMeasurementIsolated(t *testing.T) {
	iso := fake.NewIsolation("7-10", 7)
	iso.SetActive(true)
	e := benchEngine(t, iso)
	e.commands["c-1"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-1", State: running, Bench: true}}}
	e.settleBench(t.Context())
	if !iso.Active() || iso.Offs() != 0 {
		t.Errorf("active %v after %d Offs, want kept", iso.Active(), iso.Offs())
	}
}

// An Off that fails is tried again a while later, not at every step.
func TestFailedOffIsRetriedLater(t *testing.T) {
	iso := fake.NewIsolation("7-10", 7)
	iso.SetActive(true)
	iso.SetOffErr(errors.New("cpu0's max clock reads 4001000 after giving back 5263061"))
	now := time.Unix(1795300000, 0)
	e := benchEngine(t, iso)
	e.cfg.Now = func() time.Time { return now }
	e.settleBench(t.Context())
	e.settleBench(t.Context())
	if iso.Offs() != 1 {
		t.Fatalf("%d Offs right after a failed one, want 1", iso.Offs())
	}
	iso.SetOffErr(nil)
	now = now.Add(offRetryAfter)
	e.settleBench(t.Context())
	if iso.Offs() != 2 || iso.Active() {
		t.Errorf("%d Offs, active %v, %v after a failed one, want given back on the second try", iso.Offs(), iso.Active(), offRetryAfter)
	}
}

// Dev servers run until stopped: a measurement doesn't wait for their CPUs,
// which would hold up every command behind it for good.
func TestWorkCoresLeaveOutServersAndMeasurements(t *testing.T) {
	e := benchEngine(t, fake.NewIsolation("7-10", 7))
	for id, c := range map[string]*pb.Command{
		"build":  {State: running, Kind: "build", CpuCores: 3},
		"test":   {State: running, Kind: "test", CpuCores: 1.5},
		"server": {State: running, Kind: "server", CpuCores: 8},
		"bench":  {State: running, Kind: "shell", Bench: true, CpuCores: 4},
		"parked": {State: parked, Kind: "build", CpuCores: 2},
	} {
		c.Id = id
		e.commands[id] = &command{rec: &pb.CommandRecord{Command: c}}
	}
	if got := e.workCores(); got != 4.5 {
		t.Errorf("workCores = %v, want the build's and the test's 4.5", got)
	}
}

// A unit of a command's name already there (a worker that stopped between
// starting it and recording that) is stopped before the command starts:
// left, it would run on unwatched, in a measurement's slice too.
func TestALeftoverUnitIsStoppedFirst(t *testing.T) {
	units := &fake.Units{}
	store := &fake.Store{Root: t.TempDir()}
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp", Bench: fake.NewIsolation("7-10", 7),
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if err := os.MkdirAll(filepath.Join(store.Path("v"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.workspaces["w"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "w", Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle}, Volume: "v"}}
	log := filepath.Join(t.TempDir(), "old.log")
	if err := units.Start(t.Context(), unit.Spec{Name: "hotbed-c-1", RootDir: store.Path("v"), Argv: []string{"sleep", "30"}, Log: log}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "c-1", WorkspaceId: "w", Args: []string{"sleep", "30"}, Bench: true}); err != nil {
		t.Fatal(err)
	}
	e.step(t.Context())
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-c-1") })
	if c := e.commands["c-1"].cmd(); c.State != running {
		t.Fatalf("the command is %v: %s", c.State, c.Reason)
	}
	if spec, _ := units.Spec("hotbed-c-1"); spec.Log == log || !spec.NoSwap {
		t.Errorf("the unit running is the leftover one: %+v", spec)
	}
}

// quietEngine is an engine whose volumes are on a ZFS pool, with idle
// workspaces m, b and c on volumes ws/m, ws/b and ws/c.
func quietEngine(t *testing.T, iso *fake.Isolation) (*Engine, *fake.Units) {
	t.Helper()
	iso.SetPool("rpool/hotbed")
	units := &fake.Units{}
	store := &fake.Store{Root: t.TempDir()}
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "ubu1", Bench: iso,
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	for _, w := range []string{"m", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle}, Volume: "ws/" + w}}
	}
	return e, units
}

func (e *Engine) exec(t *testing.T, units *fake.Units, id, ws string, bench bool) *command {
	t.Helper()
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: id, WorkspaceId: ws, Args: []string{"sleep", "30"}, Bench: bench}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+id) })
	return e.commands[id]
}

// On a ZFS pool, which no cgroup's cap reaches: the commands of a volume
// writing to the pool past the bound are held from just before a
// measurement starts until it ends, and other volumes' queued commands
// wait for it; the measurement's own volume goes on.
func TestAMeasurementKeepsThePoolQuiet(t *testing.T) {
	iso := fake.NewIsolation("4-7", 4)
	e, units := quietEngine(t, iso)
	writer := e.exec(t, units, "w-1", "b", false)
	e.step(t.Context())
	if s := writer.cmd().State; s != running {
		t.Fatalf("the writer is %v", s)
	}
	iso.SetLoud(bench.Writer{Volume: "ws/b", Rate: 40e6})
	m := e.exec(t, units, "m-1", "m", true)
	e.step(t.Context())
	if s := m.cmd().State; s != running {
		t.Fatalf("the measurement is %v: %s", s, m.cmd().Reason)
	}
	if s, held := writer.cmd().State, writer.rec.HeldFor; s != parked || held != "m-1" {
		t.Fatalf("the writer is %v, held for %q; want parked for m-1", s, held)
	}
	if st, _ := units.Status(t.Context(), "hotbed-w-1"); !st.Frozen {
		t.Error("the writer's unit isn't frozen")
	}
	want := "held: its volume wrote 40.0 MB/s to rpool/hotbed beside measurement m-1, past its bound; thawed once it ends"
	if got := writer.cmd().Reason; got != want {
		t.Errorf("the writer's reason = %q, want %q", got, want)
	}
	if iso.Samples() == 0 {
		t.Error("no samples taken")
	}

	iso.SetLoud()
	other := e.exec(t, units, "o-1", "c", false)
	own := e.exec(t, units, "s-1", "m", false)
	e.step(t.Context())
	if s, r := other.cmd().State, other.cmd().Reason; s != queued || r != "waiting for measurement m-1 to finish: it keeps rpool/hotbed quiet, which no cap can do on ZFS" {
		t.Errorf("a command in another workspace is %v: %q", s, r)
	}
	if s := own.cmd().State; s != running {
		t.Errorf("a command in the measurement's workspace is %v: %s", s, own.cmd().Reason)
	}
	// Memory to spare doesn't thaw it: it's held for the measurement.
	if s := writer.cmd().State; s != parked {
		t.Errorf("the writer is %v during the measurement", s)
	}
	if got := iso.HeldOwn(); got != "ws/m" {
		t.Errorf("Held asked about own volume %q, want ws/m", got)
	}

	if _, err := e.Stop(t.Context(), "m-1", "done"); err != nil {
		t.Fatal(err)
	}
	e.step(t.Context())
	if s, held := writer.cmd().State, writer.rec.HeldFor; s != running || held != "" {
		t.Errorf("after the measurement the writer is %v, held for %q; want running", s, held)
	}
	if st, _ := units.Status(t.Context(), "hotbed-w-1"); st.Frozen {
		t.Error("the writer's unit is still frozen")
	}
	if s := other.cmd().State; s != running {
		t.Errorf("after the measurement the other command is %v: %s", s, other.cmd().Reason)
	}
}

// Where the volumes aren't on ZFS, the kernel caps work's writes: nothing
// is held, and work in other workspaces starts beside a measurement.
func TestWithoutAPoolNothingIsHeld(t *testing.T) {
	iso := fake.NewIsolation("4-7", 4)
	e, units := quietEngine(t, iso)
	iso.SetPool("")
	iso.SetLoud(bench.Writer{Volume: "ws/b", Rate: 40e6})
	writer := e.exec(t, units, "w-1", "b", false)
	e.step(t.Context())
	m := e.exec(t, units, "m-1", "m", true)
	other := e.exec(t, units, "o-1", "c", false)
	e.step(t.Context())
	for _, c := range []*command{writer, m, other} {
		if s := c.cmd().State; s != running {
			t.Errorf("%s is %v: %s", c.cmd().Id, s, c.cmd().Reason)
		}
	}
}

// When a failing --bench command exits, systemd tears down the empty bench
// slice so reading cpuset.cpus.partition returns ENOENT. Post-exit observation
// must not mask the command's real exit code as lost isolation (board 7935),
// while losing isolation while running MUST still be caught and reported.
func TestBenchFailingCommandKeepsExitCodeWhenSliceVanishesOnExit(t *testing.T) {
	sliceENOENT := &os.PathError{
		Op:   "open",
		Path: "/sys/fs/cgroup/hotbed_bench.slice/cpuset.cpus.partition",
		Err:  os.ErrNotExist,
	}

	t.Run("slice gone on exit keeps real exit code", func(t *testing.T) {
		iso := fake.NewIsolation("4-7", 4)
		e := benchEngine(t, iso)
		c := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "b-fail", State: running, Bench: true, Argv: "false"},
			Volume:  "ws/m",
		})
		e.commands[c.cmd().Id] = c

		// Running tick: isolation holds.
		e.observe(t.Context(), c, unit.State{Loaded: true, Running: true}, e.now())
		if c.rec.IsolationLost != "" {
			t.Fatalf("unexpected isolation lost while running: %q", c.rec.IsolationLost)
		}

		// Command exits 2 and systemd removes the empty bench slice.
		iso.SetLost(sliceENOENT)
		e.observe(t.Context(), c, unit.State{Loaded: true, Exited: true, ExitCode: 2}, e.now())

		cmd := c.cmd()
		if cmd.State != failed || cmd.GetExitCode() != 2 || cmd.Reason != "" {
			t.Fatalf("cmd = state %v exit %v reason %q; want failed exit 2 with empty reason", cmd.State, cmd.ExitCode, cmd.Reason)
		}
		if c.rec.IsolationLost != "" {
			t.Errorf("IsolationLost = %q, want empty", c.rec.IsolationLost)
		}
		for _, ev := range cmd.Timeline {
			if ev.What == "isolation lost" {
				t.Errorf("unexpected isolation lost timeline event: %v", cmd.Timeline)
			}
		}
	})

	t.Run("isolation lost while running is reported even if slice vanishes on exit", func(t *testing.T) {
		iso := fake.NewIsolation("4-7", 4)
		e := benchEngine(t, iso)
		c := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "b-lost", State: running, Bench: true, Argv: "bench"},
			Volume:  "ws/m",
		})
		e.commands[c.cmd().Id] = c

		// Isolation breaks while the command is still running.
		iso.SetLost(errors.New("the measurement's slice is not a partition of 4-7: member, its CPUs 0-7"))
		e.observe(t.Context(), c, unit.State{Loaded: true, Running: true}, e.now())
		if c.rec.IsolationLost == "" {
			t.Fatal("expected IsolationLost to be recorded while running")
		}

		// Even if the slice disappears by the time exit is observed, the recorded
		// mid-run isolation loss is preserved and reported.
		iso.SetLost(sliceENOENT)
		e.observe(t.Context(), c, unit.State{Loaded: true, Exited: true, ExitCode: 2}, e.now())

		cmd := c.cmd()
		want := "exit 2, but the measurement lost its isolation: the measurement's slice is not a partition of 4-7: member, its CPUs 0-7"
		if cmd.State != failed || cmd.Reason != want {
			t.Fatalf("cmd = state %v reason %q; want failed %q", cmd.State, cmd.Reason, want)
		}
	})

	t.Run("slice gone while still running is reported as lost isolation", func(t *testing.T) {
		iso := fake.NewIsolation("4-7", 4)
		e := benchEngine(t, iso)
		c := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "b-gone-live", State: running, Bench: true, Argv: "bench"},
			Volume:  "ws/m",
		})
		e.commands[c.cmd().Id] = c

		iso.SetLost(sliceENOENT)
		e.observe(t.Context(), c, unit.State{Loaded: true, Running: true}, e.now())
		if c.rec.IsolationLost == "" {
			t.Fatal("expected ENOENT while still running to record IsolationLost")
		}
	})

	t.Run("real bench.Isolation still runs remaining Held checks when slice vanishes on exit", func(t *testing.T) {
		root := t.TempDir()
		writeFile := func(rel, content string) {
			t.Helper()
			p := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		writeFile("sys/devices/system/cpu/online", "0-7\n")
		writeFile("sys/fs/cgroup/hotbed_bench.slice/cpuset.cpus.exclusive", "\n")
		writeFile("sys/fs/cgroup/hotbed_bench.slice/cpuset.cpus", "\n")
		writeFile("sys/fs/cgroup/hotbed_bench.slice/cpuset.cpus.partition", "member\n")
		writeFile("sys/fs/cgroup/hotbed_bench.slice/cpuset.cpus.effective", "4-7\n")
		writeFile("sys/devices/virtual/workqueue/cpumask", "ff\n")
		writeFile("proc/irq/default_smp_affinity", "ff\n")

		iso, err := bench.New(bench.Config{CPUs: "4-7"}, bench.Options{
			Root:      root,
			State:     filepath.Join(t.TempDir(), "bench"),
			Slice:     "hotbed_bench.slice",
			WorkSlice: "hotbed.slice",
			Systemd:   noopBenchSystemd{},
			Rmdir:     os.RemoveAll,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := iso.On(t.Context()); err != nil {
			t.Fatal(err)
		}

		store := &fake.Store{Root: t.TempDir()}
		e, err := NewEngine(t.Context(), EngineConfig{
			Dir: t.TempDir(), NodeID: "n", Host: "asahimbp", Bench: iso,
			Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.Close() })

		// Case A: slice cpuset files vanish on exit, workqueue still isolated -> real exit code preserved.
		c1 := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "b-real-clean", State: running, Bench: true, Argv: "false"},
			Volume:  "ws/m",
		})
		e.commands[c1.cmd().Id] = c1
		if err := os.Remove(filepath.Join(root, "sys/fs/cgroup/hotbed_bench.slice/cpuset.cpus.partition")); err != nil {
			t.Fatal(err)
		}
		e.observe(t.Context(), c1, unit.State{Loaded: true, Exited: true, ExitCode: 2}, e.now())
		if c1.cmd().State != failed || c1.cmd().GetExitCode() != 2 || c1.rec.IsolationLost != "" {
			t.Fatalf("clean exit with vanished slice: state=%v exit=%v lost=%q", c1.cmd().State, c1.cmd().ExitCode, c1.rec.IsolationLost)
		}

		// Case B: slice cpuset files vanished on exit AND workqueue mask was corrupted -> isolation loss still caught!
		c2 := e.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "b-real-wq", State: running, Bench: true, Argv: "bench"},
			Volume:  "ws/m",
		})
		e.commands[c2.cmd().Id] = c2
		writeFile("sys/devices/virtual/workqueue/cpumask", "ff\n")
		e.observe(t.Context(), c2, unit.State{Loaded: true, Exited: true, ExitCode: 0}, e.now())
		if c2.cmd().State != failed || c2.rec.IsolationLost == "" {
			t.Fatalf("exit with vanished slice + bad workqueue: state=%v reason=%q lost=%q, want failed with IsolationLost", c2.cmd().State, c2.cmd().Reason, c2.rec.IsolationLost)
		}
	})
}

type noopBenchSystemd struct{}

func (noopBenchSystemd) StartSlice(context.Context, string) error { return nil }
func (noopBenchSystemd) SetProperties(context.Context, string, ...string) error {
	return nil
}
func (noopBenchSystemd) Show(context.Context, string, ...string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// observe computes CpuCores from the timestamp captured when CPUNanos was read
// (not wall clock after log/cleanup work), and ignores sub-100ms stepSoon
// bursts so quantized counters do not distort the rate (board 7967).
func TestObserveUsesSampleTimestampAndIgnoresMicroIntervals(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := benchEngine(t, fake.NewIsolation("4-7", 4))
	e.cfg.Now = func() time.Time { return now }
	c := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "c-cpu", State: running},
		Volume:  "ws/w",
	})
	e.commands[c.cmd().Id] = c

	t0 := now
	// Simulate 50ms of post-StatusMany delay before observe runs.
	now = t0.Add(50 * time.Millisecond)
	e.observe(t.Context(), c, unit.State{Loaded: true, Running: true, CPUNanos: 0}, t0)

	t1 := t0.Add(time.Second)
	// Simulate 250ms of post-StatusMany delay (e.g. another unit stopping).
	now = t1.Add(250 * time.Millisecond)
	e.observe(t.Context(), c, unit.State{Loaded: true, Running: true, CPUNanos: 6 * uint64(time.Second)}, t1)
	if got := c.cmd().CpuCores; got < 5.99 || got > 6.01 {
		t.Fatalf("CpuCores = %.4f, want 6.0 (must use sample timestamp, not delayed e.now)", got)
	}

	// A stepSoon() 5ms later with zero counter advancement must not overwrite CpuCores.
	t2 := t1.Add(5 * time.Millisecond)
	now = t2
	e.observe(t.Context(), c, unit.State{Loaded: true, Running: true, CPUNanos: 6 * uint64(time.Second)}, t2)
	if got := c.cmd().CpuCores; got < 5.99 || got > 6.01 {
		t.Fatalf("CpuCores after 5ms burst = %.4f, want preserved 6.0", got)
	}
}
