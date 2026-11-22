package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

const (
	queuedState  = pb.CommandState_COMMAND_STATE_QUEUED
	runningState = pb.CommandState_COMMAND_STATE_RUNNING
	parkedState  = pb.CommandState_COMMAND_STATE_PARKED
	okState      = pb.CommandState_COMMAND_STATE_SUCCEEDED
)

// benchCluster is a cluster whose worker takes measurements on CPUs 4-7 of
// 8, with a session tidy-owl that has a workspace of app.
func benchCluster(t *testing.T) (*cluster, *fake.Isolation) {
	t.Helper()
	iso := fake.NewIsolation("4-7", 4)
	c := newCluster(t, func(cfg *worker.EngineConfig) {
		cfg.Bench = iso
		cfg.UnitProperties = []string{"Nice=19", "DeviceAllow=/dev/kvm rw"}
	})
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "tidy-owl")
	c.ok(nil, "add", "tidy-owl", "app")
	return c, iso
}

// command is the session tidy-owl's command id, as hotbed ps lists it.
func (c *cluster) command(id string) *pb.Command {
	c.t.Helper()
	var ls pb.ListCommandsResponse
	c.ok(&ls, "ps", "tidy-owl")
	for _, cmd := range ls.Commands {
		if cmd.Id == id {
			return cmd
		}
	}
	c.t.Fatalf("hotbed ps lists no command %s", id)
	return nil
}

// detach queues a command in tidy-owl's workspace of app, with hotbed run's
// flags, and returns it.
func (c *cluster) detach(flags []string, args ...string) *pb.Command {
	c.t.Helper()
	var cmd pb.Command
	c.ok(&cmd, append(append(append([]string{"run", "tidy-owl", "app", "--detach"}, flags...), "--"), args...)...)
	return &cmd
}

// gate is a command that runs until the test touches its file.
type gate string

func newGate(t *testing.T) gate { return gate(t.TempDir()) }

func (g gate) until(name string) []string {
	return []string{"sh", "-c", `while [ ! -e "$1" ]; do sleep 0.05; done`, "sh", filepath.Join(string(g), name)}
}

func (g gate) open(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(string(g), name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBenchOnANodeThatTakesNoMeasurementsFails(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "tidy-owl")
	c.ok(nil, "add", "tidy-owl", "app")
	code, e := c.fails("run", "tidy-owl", "app", "--bench", "--", "true")
	if code != exitError || e.Code != api.CodeInvalidArgs || e.Message != "asahimbp takes no measurements: its worker's config names no bench CPUs" {
		t.Fatalf("run --bench: exit %d, %+v", code, e)
	}
}

// A measurement runs in the isolation's slice, isolated from before it
// starts until after it ends, never swapping, and at the default nice.
func TestBenchRunsIsolated(t *testing.T) {
	c, iso := benchCluster(t)
	code, out, stderr := c.hotbed("run", "tidy-owl", "app", "--bench", "--", "sh", "-c", "echo measured")
	if code != 0 || out != "measured\n" {
		t.Fatalf("run --bench: exit %d, %q, stderr %q", code, out, stderr)
	}
	if iso.Ons() != 1 {
		t.Errorf("isolated %d times, want once", iso.Ons())
	}
	c.eventually("the isolation to be given back", func() bool { return iso.Offs() == 1 && !iso.Active() })
	var ls pb.ListCommandsResponse
	c.ok(&ls, "ps", "tidy-owl")
	i := slices.IndexFunc(ls.Commands, func(cmd *pb.Command) bool { return cmd.Bench })
	if i < 0 {
		t.Fatalf("ps lists no measurement: %v", ls.Commands)
	}
	cmd := ls.Commands[i]
	if cmd.State != okState || cmd.Timeline[1].What != "admitted" || cmd.Timeline[1].Detail != "measurement on CPUs 4-7 · low 0.0 GiB · high 0.0 GiB" {
		t.Errorf("the measurement is %v, timeline %v", cmd.State, cmd.Timeline)
	}
	spec, ok := c.nodes[0].units.Spec("hotbed-" + cmd.Id)
	if !ok {
		t.Fatal("no unit was started for the measurement")
	}
	if spec.Slice != "hotbed_bench.slice" || !spec.NoSwap || !slices.Equal(spec.Properties, []string{"DeviceAllow=/dev/kvm rw"}) {
		t.Errorf("the measurement's unit: slice %q, no swap %v, properties %q", spec.Slice, spec.NoSwap, spec.Properties)
	}
}

// A measurement whose isolation was lost mid-run fails, whatever its
// command's status.
func TestBenchThatLostItsIsolationFails(t *testing.T) {
	c, iso := benchCluster(t)
	iso.SetLost(errors.New("the measurement's slice is not a partition of 4-7: root invalid, its CPUs 0-7"))
	code, out, stderr := c.hotbed("run", "tidy-owl", "app", "--bench", "--", "true")
	var ls pb.ListCommandsResponse
	c.ok(&ls, "ps", "tidy-owl")
	cmd := ls.Commands[len(ls.Commands)-1]
	want := "hotbed: " + cmd.Id + " running in " + cmd.WorkspaceId + " (hotbed stop tidy-owl " + cmd.Id + ")\n" +
		"hotbed: INTERNAL: " + cmd.Id + " failed: exit 0, but the measurement lost its isolation: the measurement's slice is not a partition of 4-7: root invalid, its CPUs 0-7\n"
	if code != exitError || out != "" || stderr != want {
		t.Fatalf("run --bench: exit %d, %q, stderr %q", code, out, stderr)
	}
	if cmd.Timeline[2].What != "isolation lost" {
		t.Errorf("timeline %v, want the isolation's loss after its admission", cmd.Timeline)
	}
	c.eventually("the isolation to be given back", func() bool { return !iso.Active() })
}

// Memory runs short with a measurement and other work running: the work
// is parked, the measurement never, though it is the newest.
func TestBenchIsNeverParked(t *testing.T) {
	c, _ := benchCluster(t)
	g := newGate(t)
	w := c.detach(nil, g.until("w")...)
	c.eventually("the work to run", func() bool { return c.command(w.Id).State == runningState })
	b := c.detach([]string{"--bench"}, g.until("b")...)
	c.eventually("the measurement to run", func() bool { return c.command(b.Id).State == runningState })
	c.nodes[0].system.SetUsed(c.nodes[0].system.Mem.Total / 100 * 97)
	c.eventually("the work to be parked", func() bool { return c.command(w.Id).State == parkedState })
	if got := c.command(b.Id); got.State != runningState {
		t.Errorf("the measurement is %v: %s", got.State, got.Reason)
	}
	c.nodes[0].system.SetUsed(0)
	g.open(t, "w")
	g.open(t, "b")
}

// Measurements take turns: the second waits for the first to end, without
// holding up work queued behind it.
func TestMeasurementsTakeTurns(t *testing.T) {
	c, iso := benchCluster(t)
	g := newGate(t)
	b1 := c.detach([]string{"--bench"}, g.until("b1")...)
	c.eventually("the first measurement to run", func() bool { return c.command(b1.Id).State == runningState })
	b2 := c.detach([]string{"--bench"}, g.until("b2")...)
	w := c.detach(nil, g.until("w")...)
	c.eventually("work queued after the second to run beside the first", func() bool { return c.command(w.Id).State == runningState })
	if got := c.command(b2.Id); got.State != queuedState || got.Reason != "waiting for measurement "+b1.Id+" to finish" {
		t.Errorf("the second measurement is %v: %q", got.State, got.Reason)
	}
	g.open(t, "b1")
	c.eventually("the second measurement to run", func() bool { return c.command(b2.Id).State == runningState })
	if iso.Ons() != 2 || iso.Offs() != 1 {
		t.Errorf("isolated %d times and gave it back %d, want 2 and 1", iso.Ons(), iso.Offs())
	}
	g.open(t, "b2")
	g.open(t, "w")
	c.eventually("the second measurement to end, its isolation given back", func() bool {
		return c.command(b2.Id).State == okState && !iso.Active() && iso.Offs() == 2
	})
}

// A measurement waits until running work fits in the CPUs it leaves them,
// and work queued after it waits behind it, so that the work ahead drains.
func TestBenchWaitsForRoomAndHoldsTheLine(t *testing.T) {
	c, iso := benchCluster(t)
	c.nodes[0].units.SetCores(6) // each command keeps 6 CPUs busy
	g := newGate(t)
	w1 := c.detach(nil, g.until("w1")...)
	c.eventually("the work to run on 6 CPUs", func() bool { return c.command(w1.Id).CpuCores > 5.9 })
	b := c.detach([]string{"--bench"}, g.until("b")...)
	w2 := c.detach(nil, g.until("w2")...)
	c.eventually("the measurement and the work behind it to say why they wait", func() bool {
		return c.command(b.Id).Reason == "waiting for CPUs: running commands keep 6.0 busy, more than the 4 a measurement leaves them" &&
			c.command(w2.Id).Reason == "waiting behind measurement "+b.Id+", which waits for room"
	})
	if iso.Ons() != 0 {
		t.Errorf("isolated %d times while the measurement waited", iso.Ons())
	}
	g.open(t, "w1")
	c.eventually("the measurement, then the work behind it, to run", func() bool {
		return c.command(b.Id).State == runningState && c.command(w2.Id).State == runningState
	})
	g.open(t, "b")
	g.open(t, "w2")
}
