package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/unit"
)

func TestBenchSlice(t *testing.T) {
	for slice, want := range map[string]string{
		"user-1000-hotbed.slice": "user-1000-hotbed_bench.slice",
		"":                       "hotbed_bench.slice",
	} {
		if got := BenchSlice(slice); got != want {
			t.Errorf("BenchSlice(%q) = %q, want %q", slice, got, want)
		}
	}
}

func benchEngine(t *testing.T, iso *fake.Isolation) *Engine {
	t.Helper()
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp", Bench: iso,
		Store: &fake.Store{Root: t.TempDir()}, Units: &fake.Units{}, System: fake.NewSystem(),
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
		Store: store, Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if err := os.MkdirAll(filepath.Join(store.Path("v"), "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.workspaces["w"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "w", RepoId: "app", State: idle}, Volume: "v"}}
	log := filepath.Join(t.TempDir(), "old.log")
	if err := units.Start(t.Context(), unit.Spec{Name: "hotbed-c-1", Argv: []string{"sleep", "30"}, Log: log}); err != nil {
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
