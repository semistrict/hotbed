package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/semistrict/hotbed/internal/api"
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
		if what, id := api.EventWhat(e); e.GetCommand() != nil && id == cmd && (what == "exited" || what == "cancelled") {
			out = append(out, fmt.Sprintf("%s %s", what, e.Detail))
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
		time.Sleep(25 * time.Millisecond)
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
	code, _, stderr := c.hotbed(append([]string{"run", "-s", session, "-r", "app", "--"}, args...)...)
	m := started.FindStringSubmatch(stderr)
	if m == nil {
		c.t.Fatalf("run %v started nothing: exit %d, stderr %q", args, code, stderr)
	}
	return m[1], code
}

// A worker restarted under a running command (a deploy) picks it up again:
// its end reaches the session once, with its status.
func TestWorkerRestartMidCommand(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "warm-elk")
	c.workspace("warm-elk", nil)
	flag := filepath.Join(t.TempDir(), "go")
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "warm-elk", "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", waitFor(flag, "exit 3"))
	c.eventually("the command to run", func() bool {
		got, ok := c.engine().Command(cmd.Id)
		return ok && got.State == runningState
	})
	c.nodes[0].restart()
	write(t, flag, "")
	c.eventually("the command's end", func() bool { return len(ends(c.events("warm-elk"), cmd.Id)) > 0 })
	c.converged()
	if got := ends(c.events("warm-elk"), cmd.Id); !slices.Equal(got, []string{"exited exit 3"}) {
		t.Errorf("ends of %s = %q, want one exit 3", cmd.Id, got)
	}
}

// A worker whose database is lost starts a new epoch. Its session hears
// that its command and workspace are lost, and a new workspace works.
func TestLostWorkerDatabase(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "poor-cod")
	first := c.workspace("poor-cod", nil)
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "poor-cod", "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "30")
	c.eventually("the command to run", func() bool { return c.commandStates("poor-cod")[cmd.Id] == pb.CommandState_COMMAND_STATE_RUNNING })
	c.nodes[0].wipe()
	c.eventually("the loss to reach the session", func() bool { return len(ends(c.events("poor-cod"), cmd.Id)) > 0 })
	c.converged()
	evs := c.events("poor-cod")
	if got := ends(evs, cmd.Id); len(got) != 1 || !strings.HasPrefix(got[0], "cancelled lost") {
		t.Errorf("ends of %s = %q, want one: cancelled, lost", cmd.Id, got)
	}
	if !slices.ContainsFunc(evs, func(e *pb.SessionEvent) bool {
		w := e.GetWorkspace()
		return w.GetWorkspaceId() == first.Id && w.GetWhat() == pb.WorkspaceUpdate_WHAT_FAILED && strings.HasPrefix(e.Detail, "lost")
	}) {
		t.Errorf("no failed event for workspace %s in %v", first.Id, evs)
	}
	again := c.workspace("poor-cod", nil)
	if again.Id == first.Id {
		t.Fatalf("add after the loss returned the lost workspace %s", first.Id)
	}
	if code, out, stderr := c.hotbed("run", "-s", "poor-cod", "-r", "app", "--", "cat", "src/lib.txt"); code != 0 || out != "base\n" {
		t.Errorf("run in the new workspace: exit %d, %q, stderr %q", code, out, stderr)
	}
}

// With its worker cut off, the controller answers for a command from the
// worker's view, as it last saw it, rather than failing (board 7027).
func TestCutOffWorkersCommandIsAnsweredFromItsView(t *testing.T) {
	t.Parallel()
	c := newMixedArchFleet(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "far-gnu")
	c.workspace("far-gnu", &pb.Placement{Arch: "amd64"})
	g := newGate(t)
	var cmd pb.Command
	c.ok(&cmd, append([]string{"run", "-s", "far-gnu", "-r", "app", "--detach", "--mem", "1G", "--"}, g.until("far")...)...)
	c.eventually("the command to run", func() bool {
		got, err := c.workspaces().GetCommand(t.Context(), &pb.GetCommandRequest{Id: cmd.Id})
		return err == nil && got.State == runningState
	})
	c.converged()

	c.nodes[1].cut.Store(true)
	got, err := c.workspaces().GetCommand(t.Context(), &pb.GetCommandRequest{Id: cmd.Id})
	if err != nil || got.Id != cmd.Id || got.State != runningState {
		t.Errorf("GetCommand of the cut-off worker's command = %v, %v; want it running, from the view", got, err)
	}
	c.nodes[1].cut.Store(false)
	g.open(t, "far")
}

// A second run of a session while its node is still making the session's
// workspace waits for it and gets it, rather than failing NOT_FOUND from a
// node that does not have it yet (board 7349).
func TestRunsWaitForAWorkspaceBeingMade(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "slow-elk")
	gate := make(chan struct{})
	c.nodes[0].createGate.Store(&gate)
	openGate := sync.OnceFunc(func() {
		close(gate)
		c.nodes[0].createGate.Store(nil)
	})
	defer openGate() // a failure before the gate opens must not leave the node stuck
	type made struct {
		ws  *pb.Workspace
		err error
	}
	first := make(chan made, 1)
	go func() {
		ws, err := c.workspaces().CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{SessionId: "slow-elk", Repo: "app"})
		first <- made{ws, err}
	}()
	c.eventually("the first run's workspace to be claimed", func() bool {
		sess, err := c.store.Session(t.Context(), "slow-elk")
		return err == nil && len(sess.WorkspaceIds) == 1
	})
	second := make(chan made, 1)
	go func() {
		ws, err := c.workspaces().CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{SessionId: "slow-elk", Repo: "app"})
		second <- made{ws, err}
	}()
	select {
	case m := <-second:
		t.Fatalf("the second run returned while the workspace was being made: %v, %v", m.ws, m.err)
	case <-time.After(300 * time.Millisecond):
	}
	openGate()
	a, b := <-first, <-second
	if a.err != nil || b.err != nil || a.ws.Id != b.ws.Id {
		t.Fatalf("runs got %v (%v) and %v (%v), want the same workspace", a.ws.GetId(), a.err, b.ws.GetId(), b.err)
	}
	if code, out, stderr := c.hotbed("run", "-s", "slow-elk", "-r", "app", "--", "cat", "src/lib.txt"); code != 0 || out != "base\n" {
		t.Errorf("run in it: exit %d, %q, stderr %q", code, out, stderr)
	}
}

// A session's workspace its node does not have (a record the node never
// made) is forgotten, and the next run gets another instead of failing
// NOT_FOUND forever (board 7349).
func TestWorkspaceItsNodeLacksIsReplaced(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "lost-elk")
	if err := c.store.AddWorkspace(t.Context(), "ghost", "lost-elk", c.nodes[0].peer.NodeID, "laptop", time.Now()); err != nil {
		t.Fatal(err)
	}
	ws := c.workspace("lost-elk", nil)
	if ws.Id == "ghost" {
		t.Fatal("the run got the workspace its node does not have")
	}
	if code, out, stderr := c.hotbed("run", "-s", "lost-elk", "-r", "app", "--", "cat", "src/lib.txt"); code != 0 || out != "base\n" {
		t.Errorf("run: exit %d, %q, stderr %q", code, out, stderr)
	}
	if sess, err := c.store.Session(t.Context(), "lost-elk"); err != nil || !slices.Equal(sess.WorkspaceIds, []string{ws.Id}) {
		t.Errorf("session workspaces %v (%v), want only %s", sess.GetWorkspaceIds(), err, ws.Id)
	}
}
