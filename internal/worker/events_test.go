package worker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

// newEngine is a worker engine on fakes, with its state in dir.
func newEngine(t *testing.T, dir string) *worker.Engine {
	t.Helper()
	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: dir, NodeID: "nAsahi", Host: "asahimbp",
		Store: &fake.Store{Root: filepath.Join(dir, "volumes")}, Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// engine is a running engine, stopped and closed when the test ends.
func engine(t *testing.T, dir string) *worker.Engine {
	t.Helper()
	eng := newEngine(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		eng.Close()
	})
	return eng
}

// origin is a repo with one commit.
func origin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-qm", "First"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(dir, "lib.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Every change to a worker's bases, workspaces and commands is an event,
// numbered in order, and folding the events gives exactly the state the
// worker keeps: what a checkpoint holds.
func TestEveryChangeIsAnEvent(t *testing.T) {
	eng := engine(t, t.TempDir())
	repo := &pb.Repo{Id: "app", Url: origin(t), Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}}}
	ctx := t.Context()
	if _, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", RepoConfig: repo}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the workspace to be ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})
	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "ws1", Args: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Id) != 10 {
		t.Errorf("a command run without an ID got %q, want ten characters the worker picked", cmd.Id)
	}
	eventually(t, "the command to end", func() bool {
		c, ok := eng.Command(cmd.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})
	if err := eng.ReleaseWorkspace(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}

	events, err := eng.Events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	whats := map[string][]string{} // entity -> what happened to it, in order
	var freshen string
	for i, ev := range events {
		if i > 0 && ev.Seq <= events[i-1].Seq {
			t.Fatalf("event %d has seq %d after %d", i, ev.Seq, events[i-1].Seq)
		}
		switch c := ev.Change.(type) {
		case *pb.Event_Base:
			whats["base "+c.Base.Base.Id] = append(whats["base "+c.Base.Base.Id], c.Base.What)
		case *pb.Event_Workspace:
			whats["workspace "+c.Workspace.Workspace.Id] = append(whats["workspace "+c.Workspace.Workspace.Id], c.Workspace.What)
		case *pb.Event_Command:
			id := c.Command.Command.Id
			if c.Command.Command.Kind == "freshen" {
				freshen, id = id, "freshen"
			}
			whats["command "+id] = append(whats["command "+id], c.Command.What)
			ended := c.Command.What == "exited" || c.Command.What == "cancelled"
			if ended != (c.Command.Finished != nil) && c.Command.Command.Kind != "freshen" {
				t.Errorf("%s %s carries history %v", c.Command.What, id, c.Command.Finished)
			}
		default:
			t.Fatalf("event %d changes nothing: %v", ev.Seq, ev)
		}
	}
	want := map[string][]string{
		"base app-g1":       {"building", "warm"},
		"command freshen":   {"queued", "admitted", "exited"},
		"workspace ws1":     {"created", "ready", "active", "idle", "released"},
		"command " + cmd.Id: {"queued", "admitted", "exited"},
	}
	if diff := cmp.Diff(want, whats); diff != "" {
		t.Errorf("events per entity (-want +got):\n%s", diff)
	}
	if freshen == "" {
		t.Fatal("no freshen command")
	}

	cp, err := eng.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if cp.Seq != events[len(events)-1].Seq {
		t.Errorf("checkpoint at seq %d, want the last event's %d", cp.Seq, events[len(events)-1].Seq)
	}
	if diff := cmp.Diff(sorted(cp), sorted(fold(events)), protocmp.Transform()); diff != "" {
		t.Errorf("checkpoint against the folded events (-checkpoint +fold):\n%s", diff)
	}
}

// fold is the state the events leave: each entity as its last event has it,
// without those the worker deleted.
func fold(events []*pb.Event) *pb.Checkpoint {
	bases, workspaces, commands := map[string]*pb.Base{}, map[string]*pb.Workspace{}, map[string]*pb.Command{}
	cp := &pb.Checkpoint{}
	for _, ev := range events {
		cp.Seq = ev.Seq
		switch c := ev.Change.(type) {
		case *pb.Event_Base:
			if c.Base.What == "collected" {
				delete(bases, c.Base.Base.Id)
			} else {
				bases[c.Base.Base.Id] = c.Base.Base
			}
		case *pb.Event_Workspace:
			if c.Workspace.What == "forgotten" {
				delete(workspaces, c.Workspace.Workspace.Id)
			} else {
				workspaces[c.Workspace.Workspace.Id] = c.Workspace.Workspace
			}
		case *pb.Event_Command:
			if c.Command.What == "forgotten" {
				delete(commands, c.Command.Command.Id)
			} else {
				commands[c.Command.Command.Id] = c.Command.Command
			}
		}
	}
	for _, b := range bases {
		cp.Bases = append(cp.Bases, b)
	}
	for _, w := range workspaces {
		cp.Workspaces = append(cp.Workspaces, w)
	}
	for _, c := range commands {
		cp.Commands = append(cp.Commands, c)
	}
	return cp
}

func sorted(cp *pb.Checkpoint) *pb.Checkpoint {
	cp = proto.Clone(cp).(*pb.Checkpoint)
	slices.SortFunc(cp.Bases, func(a, b *pb.Base) int { return strings.Compare(a.Id, b.Id) })
	slices.SortFunc(cp.Workspaces, func(a, b *pb.Workspace) int { return strings.Compare(a.Id, b.Id) })
	slices.SortFunc(cp.Commands, func(a, b *pb.Command) int { return strings.Compare(a.Id, b.Id) })
	return cp
}

// A queued command's wait is an event when its kind changes, not each time
// its numbers do: those go in heartbeats.
func TestWaitingIsAnEventWhenItsKindChanges(t *testing.T) {
	eng := engine(t, t.TempDir())
	repo := &pb.Repo{Id: "app", Url: origin(t), Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}}}
	ctx := t.Context()
	if _, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{Id: "ws1", Repo: "app", SessionId: "s1", RepoConfig: repo}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the workspace to be ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})
	big, err := eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "ws1", Args: []string{"true"}, Limits: &pb.CommandLimits{MemoryLowBytes: 15 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	// Another command running and ending changes how much memory is left.
	small, err := eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "ws1", Args: []string{"sleep", "1"}, Limits: &pb.CommandLimits{MemoryLowBytes: 1 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the small command to end", func() bool {
		c, ok := eng.Command(small.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})
	events, err := eng.Events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var waits []string
	for _, ev := range events {
		if c := ev.GetCommand(); c != nil && c.Command.Id == big.Id && c.What == "waiting" {
			waits = append(waits, c.Detail)
		}
	}
	if len(waits) != 1 || !strings.HasPrefix(waits[0], "waiting for memory") {
		t.Errorf("waiting events of the big command = %q, want one, for memory", waits)
	}
	if h := eng.Heartbeat("c"); len(h.Commands) != 1 || !strings.HasPrefix(h.Commands[0].Reason, "waiting for memory: needs 15.0 GiB") {
		t.Errorf("heartbeat's commands = %v, want the big one with its reason", h.Commands)
	}
}
