package worker

import (
	"errors"
	"testing"
	"time"

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
