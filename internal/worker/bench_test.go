package worker

import (
	"testing"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
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
