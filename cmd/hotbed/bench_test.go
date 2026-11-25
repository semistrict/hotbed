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
		cfg.UnitProperties = []string{"Nice=19", "IOSchedulingClass=idle", "CPUSchedulingPolicy=idle", "DeviceAllow=/dev/kvm rw"}
	})
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "tidy-owl")
	c.workspace("tidy-owl", nil)
	return c, iso
}

// inPs is the session tidy-owl's command id as hotbed ps lists it, or nil
// while ps does not list it yet: ps answers from the controller's view of
// the node, which the worker's events reach a moment after run returns. Its
// getters read nil as a command in no state, for eventually's conditions.
func (c *cluster) inPs(id string) *pb.Command {
	c.t.Helper()
	var ls pb.ListCommandsResponse
	c.ok(&ls, "ps", "-s", "tidy-owl")
	for _, cmd := range ls.Commands {
		if cmd.Id == id {
			return cmd
		}
	}
	return nil
}

// command is inPs(id), failing the test if ps does not list it.
func (c *cluster) command(id string) *pb.Command {
	c.t.Helper()
	if cmd := c.inPs(id); cmd != nil {
		return cmd
	}
	c.t.Fatalf("hotbed ps lists no command %s", id)
	return nil
}

// detach queues a command in tidy-owl's workspace of app, with hotbed run's
// flags, and returns it.
func (c *cluster) detach(flags []string, args ...string) *pb.Command {
	c.t.Helper()
	var cmd pb.Command
	c.ok(&cmd, append(append(append([]string{"run", "-s", "tidy-owl", "-r", "app", "--detach"}, flags...), "--"), args...)...)
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
	code, e := c.fails("run", "-s", "tidy-owl", "-r", "app", "--bench", "--", "true")
	if code != exitError || e.Code != api.CodeInvalidArgs || e.Message != "no node can take the run: asahimbp has no measurement CPUs" {
		t.Fatalf("run --bench: exit %d, %+v", code, e)
	}
}

// A measurement runs in the isolation's slice, isolated from before it
// starts until after it ends, never swapping, and at the default nice.
func TestBenchRunsIsolated(t *testing.T) {
	c, iso := benchCluster(t)
	code, out, stderr := c.hotbed("run", "-s", "tidy-owl", "-r", "app", "--bench", "--", "sh", "-c", "echo measured")
	if code != 0 || out != "measured\n" {
		t.Fatalf("run --bench: exit %d, %q, stderr %q", code, out, stderr)
	}
	if iso.Ons() != 1 {
		t.Errorf("isolated %d times, want once", iso.Ons())
	}
	c.eventually("the isolation to be given back", func() bool { return iso.Offs() == 1 && !iso.Active() })
	var ls pb.ListCommandsResponse
	c.ok(&ls, "ps", "-s", "tidy-owl")
	i := slices.IndexFunc(ls.Commands, func(cmd *pb.Command) bool { return cmd.Bench })
	if i < 0 {
		t.Fatalf("ps lists no measurement: %v", ls.Commands)
	}
	cmd := ls.Commands[i]
	if cmd.State != okState || cmd.Timeline[1].What != "admitted" || cmd.Timeline[1].Detail != "measurement on CPUs 4-7 · low none · high none" {
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

// Normal (non-measurement) commands default to low CPU and IO priority so
// they yield to desktop and system processes.
func TestNormalWorkRunsAtLowPriority(t *testing.T) {
	c := newCluster(t, func(cfg *worker.EngineConfig) {
		cfg.UnitProperties = []string{"DeviceAllow=/dev/kvm rw"}
	})
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "tidy-owl")
	code, out, stderr := c.hotbed("run", "-s", "tidy-owl", "-r", "app", "--", "sh", "-c", "echo normal")
	if code != 0 || out != "normal\n" {
		t.Fatalf("run: exit %d, %q, stderr %q", code, out, stderr)
	}
	var ls pb.ListCommandsResponse
	c.ok(&ls, "ps", "-s", "tidy-owl")
	i := slices.IndexFunc(ls.Commands, func(cmd *pb.Command) bool { return !cmd.Bench && cmd.Kind != "freshen" })
	if i < 0 {
		t.Fatalf("ps lists no command: %v", ls.Commands)
	}
	spec, ok := c.nodes[0].units.Spec("hotbed-" + ls.Commands[i].Id)
	if !ok {
		t.Fatal("no unit was started for the command")
	}
	want := []string{"DeviceAllow=/dev/kvm rw", "Nice=19", "IOSchedulingClass=idle"}
	if !slices.Equal(spec.Properties, want) {
		t.Errorf("normal command properties = %q, want %q", spec.Properties, want)
	}
}

// A measurement whose isolation was lost mid-run fails, whatever its
// command's status.
func TestBenchThatLostItsIsolationFails(t *testing.T) {
	c, iso := benchCluster(t)
	iso.SetLost(errors.New("the measurement's slice is not a partition of 4-7: root invalid, its CPUs 0-7"))
	code, out, stderr := c.hotbed("run", "-s", "tidy-owl", "-r", "app", "--bench", "--", "true")
	var ls pb.ListCommandsResponse
	c.ok(&ls, "ps", "-s", "tidy-owl")
	cmd := ls.Commands[len(ls.Commands)-1]
	want := "hotbed: " + cmd.Id + " running in " + cmd.WorkspaceId + " on asahimbp (hotbed stop -s tidy-owl " + cmd.Id + ")\n" +
		"hotbed: INTERNAL: " + cmd.Id + " failed: exit 0, but the measurement lost its isolation: the measurement's slice is not a partition of 4-7: root invalid, its CPUs 0-7\n"
	if code != exitError || out != "" || stderr != want {
		t.Fatalf("run --bench: exit %d, %q, stderr %q", code, out, stderr)
	}
	if cmd.Timeline[2].What != "isolation lost" {
		t.Errorf("timeline %v, want the isolation's loss after its admission", cmd.Timeline)
	}
	c.eventually("the isolation to be given back", func() bool { return !iso.Active() })
}

// Measurements take turns: the second waits for the first to end, without
// holding up work queued behind it.
func TestMeasurementsTakeTurns(t *testing.T) {
	c, iso := benchCluster(t)
	g := newGate(t)
	b1 := c.detach([]string{"--bench"}, g.until("b1")...)
	c.eventually("the first measurement to run", func() bool { return c.inPs(b1.Id).GetState() == runningState })
	b2 := c.detach([]string{"--bench"}, g.until("b2")...)
	w := c.detach(nil, g.until("w")...)
	c.eventually("work queued after the second to run beside the first", func() bool { return c.inPs(w.Id).GetState() == runningState })
	if got := c.command(b2.Id); got.State != queuedState || got.Reason != "waiting for measurement "+b1.Id+" to finish" {
		t.Errorf("the second measurement is %v: %q", got.State, got.Reason)
	}
	g.open(t, "b1")
	c.eventually("the second measurement to run", func() bool { return c.inPs(b2.Id).GetState() == runningState })
	if iso.Ons() != 2 || iso.Offs() != 1 {
		t.Errorf("isolated %d times and gave it back %d, want 2 and 1", iso.Ons(), iso.Offs())
	}
	g.open(t, "b2")
	g.open(t, "w")
	c.eventually("the second measurement to end, its isolation given back", func() bool {
		return c.inPs(b2.Id).GetState() == okState && !iso.Active() && iso.Offs() == 2
	})
}

// A measurement waits until running work fits in the CPUs it leaves them,
// and work queued after it waits behind it, so that the work ahead drains.
func TestBenchWaitsForRoomAndHoldsTheLine(t *testing.T) {
	c, iso := benchCluster(t)
	c.nodes[0].units.SetCores(6) // each command keeps 6 CPUs busy
	g := newGate(t)
	w1 := c.detach(nil, g.until("w1")...)
	var last *pb.Command
	t.Cleanup(func() {
		if t.Failed() && last != nil {
			t.Logf("w1 when the wait ended: state %v, reason %q, cpu %.2f, started %d", last.State, last.Reason, last.CpuCores, last.GetStartedAt())
		}
	})
	c.eventually("the work to run on 6 CPUs", func() bool {
		if cmd, ok := c.engine().Command(w1.Id); ok && cmd.GetCpuCores() > 5.9 {
			c.heartbeat()
		}
		last = c.inPs(w1.Id)
		return last.GetCpuCores() > 5.9
	})
	t.Logf("w1 at 6 CPUs: %v", last)
	b := c.detach([]string{"--bench"}, g.until("b")...)
	w2 := c.detach(nil, g.until("w2")...)
	c.eventually("the measurement and the work behind it to say why they wait", func() bool {
		return c.inPs(b.Id).GetReason() == "waiting for CPUs: running commands keep 6.0 busy, more than the 4 a measurement leaves them" &&
			c.inPs(w2.Id).GetReason() == "waiting behind measurement "+b.Id+", which waits for room"
	})
	if iso.Ons() != 0 {
		t.Errorf("isolated %d times while the measurement waited", iso.Ons())
	}
	g.open(t, "w1")
	c.eventually("the measurement, then the work behind it, to run", func() bool {
		return c.inPs(b.Id).GetState() == runningState && c.inPs(w2.Id).GetState() == runningState
	})
	g.open(t, "b")
	g.open(t, "w2")
}
