package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

// A node saturated with CPU pressure holds new work, and the CLI shows all
// of it: the node's admission with every signal against its limit, the
// work it held and made wait, and the placements that weighed it.
func TestNodeShowsWhyItHoldsWork(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "busy-owl")
	c.workspace("busy-owl", nil)
	n := c.nodes[0]
	n.system.SetCPUPressure(75)
	n.engine.Sample()

	var d pb.NodeDetail
	c.eventually("the node to say it holds new work", func() bool {
		c.ok(&d, "node", "show", "asahimbp")
		return d.Node.GetAdmission().GetHolding()
	})
	a := d.Node.Admission
	if want := "CPU pressure 75% (peak 75% in the last 30s); new work starts again under 30%"; a.Reason != want {
		t.Errorf("admission reason = %q, want %q", a.Reason, want)
	}
	var names []string
	for _, s := range a.Signals {
		names = append(names, s.Name)
		if s.Name == "cpu-pressure" && (s.Value != 75 || s.Limit != 60 || s.Resume != 30 || !s.Holding) {
			t.Errorf("cpu-pressure signal = %v", s)
		}
	}
	if want := []string{"cpu-pressure", "memory-pressure", "memory-reserved", "disk-free"}; !slices.Equal(names, want) {
		t.Errorf("signals = %q, want %q", names, want)
	}

	var cmd pb.Command
	c.ok(&cmd, "run", "-s", "busy-owl", "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "60")
	c.eventually("the command to wait for the CPUs", func() bool {
		var ls pb.ListCommandsResponse
		c.ok(&ls, "ps", "-s", "busy-owl")
		i := slices.IndexFunc(ls.Commands, func(x *pb.Command) bool { return x.Id == cmd.Id })
		return i >= 0 && strings.HasPrefix(ls.Commands[i].Reason, "waiting: CPU pressure 75%")
	})

	var evs pb.ListEventsResponse
	c.eventually("the hold and the wait to reach the controller", func() bool {
		c.ok(&evs, "events", "--node", "asahimbp", "--kind", "hold", "--kind", "wait")
		return len(evs.Events) == 2
	})
	if e := evs.Events[0]; e.Kind != "wait" || e.Hostname != "asahimbp" || !strings.HasPrefix(e.Text, "wait sleep 60 · waiting: CPU pressure 75%") {
		t.Errorf("newest event = %v, want the command's wait", e)
	}
	if e := evs.Events[1]; e.Kind != "hold" || e.Text != "hold new work: "+a.Reason {
		t.Errorf("older event = %v, want the hold", e)
	}

	var ps pb.ListPlacementsResponse
	c.ok(&ps, "placements", "--node", "asahimbp", "--session", "busy-owl")
	if len(ps.Placements) == 0 {
		t.Fatal("no placement weighed asahimbp for busy-owl")
	}
	p := ps.Placements[0]
	if p.Hostname != "asahimbp" || p.SessionId != "busy-owl" || len(p.Candidates) != 1 || !p.Candidates[0].Chosen || len(p.Candidates[0].Parts) == 0 {
		t.Errorf("placement = %v", p)
	}

	// Plain text says the same, as a person reads it.
	code, out, stderr := c.hotbed("node", "show", "asahimbp")
	for _, want := range []string{
		"holding new work: CPU pressure 75%",
		"cpu-pressure      75% (holds at 60%, resumes under 30%)  HOLDING",
		"held work:",
		"placements:",
	} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("hotbed node show: exit %d, stderr %q; want %q in:\n%s", code, stderr, want, out)
		}
	}
}

// An attached hotbed run whose command is held in QUEUED tells the caller why
// on stderr rather than sitting silent with zero output.
func TestAttachedRunReportsQueuedWaitReason(t *testing.T) {
	origAfter, origPoll := followWaitNoticeAfter, followWaitPollInterval
	followWaitNoticeAfter = 50 * time.Millisecond
	followWaitPollInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		followWaitNoticeAfter = origAfter
		followWaitPollInterval = origPoll
	})

	// The worker's own clock, so releasing the pressure need not wait out the
	// CPU gate's window in real time.
	workerClock := &clock{}
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Now = workerClock.Now })
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "wait-owl")
	c.workspace("wait-owl", nil)
	n := c.nodes[0]
	n.system.SetCPUPressure(75)
	n.engine.Sample()

	type runRes struct {
		code   int
		out    string
		stderr string
	}
	done := make(chan runRes, 1)
	go func() {
		code, out, stderr := c.hotbed("run", "-s", "wait-owl", "-r", "app", "--", "echo", "released-ok")
		done <- runRes{code, out, stderr}
	}()

	c.eventually("the attached run to wait in QUEUED", func() bool {
		var ls pb.ListCommandsResponse
		c.ok(&ls, "ps", "-s", "wait-owl")
		return len(ls.Commands) == 1 && ls.Commands[0].State == pb.CommandState_COMMAND_STATE_QUEUED &&
			strings.HasPrefix(ls.Commands[0].Reason, "waiting: CPU pressure 75%")
	})
	// Give the follow watcher a couple of ticks to emit the queued status line.
	time.Sleep(250 * time.Millisecond)

	// Release admission pressure so the command runs and finishes cleanly: the
	// gate starts work again once a whole window has passed under its peak.
	n.system.SetCPUPressure(0)
	workerClock.advance(31 * time.Second)
	n.engine.Sample()

	res := <-done
	if res.code != 0 || !strings.Contains(res.out, "released-ok") {
		t.Fatalf("attached run: exit %d out=%q stderr=%q", res.code, res.out, res.stderr)
	}
	if !strings.Contains(res.stderr, "queued: waiting: CPU pressure 75%") {
		t.Errorf("attached run stderr missing queued wait notice: %q", res.stderr)
	}
}
