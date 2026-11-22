package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// events is everything that happened to a session, ended or not, as the
// controller recorded it.
func (c *cluster) events(session string) []*pb.SessionEvent {
	c.t.Helper()
	evs, err := c.store.SessionEvents(c.t.Context(), session, 0)
	if err != nil {
		c.t.Fatal(err)
	}
	return evs
}

// ends lists what ended a command in its session's events: exited or
// cancelled, with the detail.
func ends(evs []*pb.SessionEvent, cmd string) []string {
	var out []string
	for _, e := range evs {
		if e.CommandId == cmd && (e.What == "exited" || e.What == "cancelled") {
			out = append(out, fmt.Sprintf("%s %s", e.What, e.Detail))
		}
	}
	return out
}

// converged waits for the controller's view of every worker to be exactly
// the worker's own state.
func (c *cluster) converged() {
	c.t.Helper()
	var diff string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		diff = ""
		for _, nd := range c.nodes {
			mine, err := nd.eng().Checkpoint()
			if err != nil {
				c.t.Fatal(err)
			}
			seen, err := c.store.NodeView(c.t.Context(), nd.peer.NodeID)
			if err != nil {
				c.t.Fatal(err)
			}
			if d := cmp.Diff(sortedCheckpoint(mine), sortedCheckpoint(seen), protocmp.Transform()); d != "" {
				diff += fmt.Sprintf("%s (-worker +controller):\n%s", nd.peer.Hostname, d)
			}
		}
		if diff == "" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("the controller's views never caught up with the workers:\n%s", diff)
}

func sortedCheckpoint(cp *pb.Checkpoint) *pb.Checkpoint {
	cp = proto.Clone(cp).(*pb.Checkpoint)
	slices.SortFunc(cp.Bases, func(a, b *pb.Base) int { return strings.Compare(a.Id, b.Id) })
	slices.SortFunc(cp.Workspaces, func(a, b *pb.Workspace) int { return strings.Compare(a.Id, b.Id) })
	slices.SortFunc(cp.Commands, func(a, b *pb.Command) int { return strings.Compare(a.Id, b.Id) })
	return cp
}

// started is the ID of the command an attached run started, from its
// stderr.
var started = regexp.MustCompile(`hotbed: (\S+) running in `)

func (c *cluster) runAttached(session string, args ...string) (id string, code int) {
	c.t.Helper()
	code, _, stderr := c.hotbed(append([]string{"run", session, "app", "--"}, args...)...)
	m := started.FindStringSubmatch(stderr)
	if m == nil {
		c.t.Fatalf("run %v started nothing: exit %d, stderr %q", args, code, stderr)
	}
	return m[1], code
}

// A worker restarted under a running command (a deploy) picks it up again:
// its end reaches the session once, with its status.
func TestWorkerRestartMidCommand(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "warm-elk")
	c.ok(nil, "add", "warm-elk", "app")
	cmd := &pb.Command{}
	c.ok(cmd, "run", "warm-elk", "app", "--detach", "--", "sh", "-c", "sleep 2; exit 3")
	c.nodes[0].restart()
	c.eventually("the command's end", func() bool { return len(ends(c.events("warm-elk"), cmd.Id)) > 0 })
	c.converged()
	if got := ends(c.events("warm-elk"), cmd.Id); !slices.Equal(got, []string{"exited exit 3"}) {
		t.Errorf("ends of %s = %q, want one exit 3", cmd.Id, got)
	}
}

// Commands run while the controller is down, and a new controller (a
// deploy) on the same database: each end reaches its session once, and
// history has each command.
func TestControllerRestartWithEventsInFlight(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "busy-ant")
	c.ok(nil, "add", "busy-ant", "app")
	c.ctrl.setDown(true)
	var ids []string
	for i := range 3 {
		id, code := c.runAttached("busy-ant", "sh", "-c", fmt.Sprintf("exit %d", i))
		if code != i {
			t.Fatalf("run of exit %d returned %d", i, code)
		}
		ids = append(ids, id)
	}
	c.restart()
	c.ctrl.setDown(false)
	c.converged()
	evs := c.events("busy-ant")
	for i, id := range ids {
		if got := ends(evs, id); !slices.Equal(got, []string{fmt.Sprintf("exited exit %d", i)}) {
			t.Errorf("ends of %s = %q, want one exit %d", id, got, i)
		}
	}
	history, err := c.store.History(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !slices.ContainsFunc(history, func(f *pb.FinishedCommand) bool { return f.Id == id }) {
			t.Errorf("history has no %s", id)
		}
	}
}

// A worker whose database is lost starts a new epoch. Its session hears
// that its command and workspace are lost, and a new workspace works.
func TestLostWorkerDatabase(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "poor-cod")
	first := &pb.Workspace{}
	c.ok(first, "add", "poor-cod", "app")
	cmd := &pb.Command{}
	c.ok(cmd, "run", "poor-cod", "app", "--detach", "--", "sleep", "30")
	c.eventually("the command to run", func() bool { return c.commandStates("poor-cod")[cmd.Id] == pb.CommandState_COMMAND_STATE_RUNNING })
	c.nodes[0].wipe()
	c.eventually("the loss to reach the session", func() bool { return len(ends(c.events("poor-cod"), cmd.Id)) > 0 })
	c.converged()
	evs := c.events("poor-cod")
	if got := ends(evs, cmd.Id); len(got) != 1 || !strings.HasPrefix(got[0], "cancelled lost") {
		t.Errorf("ends of %s = %q, want one: cancelled, lost", cmd.Id, got)
	}
	if !slices.ContainsFunc(evs, func(e *pb.SessionEvent) bool {
		return e.WorkspaceId == first.Id && e.CommandId == "" && e.What == "failed" && strings.HasPrefix(e.Detail, "lost")
	}) {
		t.Errorf("no failed event for workspace %s in %v", first.Id, evs)
	}
	again := &pb.Workspace{}
	c.ok(again, "add", "poor-cod", "app")
	if again.Id == first.Id {
		t.Fatalf("add after the loss returned the lost workspace %s", first.Id)
	}
	if code, out, stderr := c.hotbed("run", "poor-cod", "app", "--", "cat", "src/lib.txt"); code != 0 || out != "base\n" {
		t.Errorf("run in the new workspace: exit %d, %q, stderr %q", code, out, stderr)
	}
}

// A worker cut off from the controller keeps serving clients; its events
// and the release of an ended session's workspace go through once it is
// back. Other workers are not held up.
func TestPartitionedWorker(t *testing.T) {
	c := newFleet(t, 2)
	c.addRepo(upstream(t))
	for _, s := range []string{"near-yak", "far-gnu", "gone-emu"} {
		c.ok(nil, "session", "start", s)
	}
	c.ok(nil, "add", "near-yak", "app", "--node", "asahimbp")
	c.ok(nil, "add", "far-gnu", "app", "--node", "mini")
	gone := &pb.Workspace{}
	c.ok(gone, "add", "gone-emu", "app", "--node", "mini")

	c.nodes[1].cut.Store(true)
	far, code := c.runAttached("far-gnu", "sh", "-c", "exit 5")
	if code != 5 {
		t.Fatalf("run on the cut-off worker returned %d", code)
	}
	near, code := c.runAttached("near-yak", "true")
	if code != 0 {
		t.Fatalf("run on the other worker returned %d", code)
	}
	c.eventually("the other worker's command to reach its session", func() bool { return len(ends(c.events("near-yak"), near)) == 1 })
	if got := ends(c.events("far-gnu"), far); len(got) != 0 {
		t.Errorf("the cut-off worker's command reached its session: %q", got)
	}
	c.ok(nil, "session", "end", "gone-emu")

	c.nodes[1].cut.Store(false)
	c.converged()
	if got := ends(c.events("far-gnu"), far); !slices.Equal(got, []string{"exited exit 5"}) {
		t.Errorf("ends of %s = %q, want one exit 5", far, got)
	}
	c.eventually("the ended session's workspace to be released", func() bool {
		ws, err := c.nodes[1].eng().Workspace(gone.Id)
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED
	})
}
