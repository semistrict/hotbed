package worker_test

import (
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// A throwaway workspace (hotbed run --fork) runs one command, refuses a
// second one, and is released once that command ends, however it ends,
// while a workspace that is not throwaway stays.
func TestThrowawayWorkspaceRunsOneCommandAndGoesWhenItEnds(t *testing.T) {
	eng := engine(t, t.TempDir())
	project := &pb.HostedProject{Name: "app", Repos: []*pb.Repo{{Id: "app", Url: origin(t)}}}
	ctx := t.Context()
	for _, req := range []*pb.CreateWorkspaceRequest{
		{Id: "kept", Repo: "app", SessionId: "s1", Agent: "a", Project: project},
		{Id: "fork1", Repo: "app", SessionId: "s1", Agent: "a", Project: project, Throwaway: true},
		{Id: "fork2", Repo: "app", SessionId: "s1", Agent: "a", Project: project, Throwaway: true},
	} {
		if _, err := eng.CreateWorkspace(ctx, req); err != nil {
			t.Fatal(err)
		}
		eventually(t, req.Id+" to be ready", func() bool {
			ws, err := eng.Workspace(req.Id)
			return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
		})
	}
	released := func(id string) bool {
		ws, err := eng.Workspace(id)
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED
	}

	// One that exits.
	kept, err := eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "kept", Args: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	ran, err := eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "fork1", Args: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "fork1's command to end and fork1 to be released", func() bool {
		c, ok := eng.Command(ran.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED && released("fork1")
	})
	eventually(t, "kept's command to end", func() bool {
		c, ok := eng.Command(kept.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})
	if ws, err := eng.Workspace("kept"); err != nil || ws.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE {
		t.Errorf("kept after its command ended = %v, %v; want it idle, not released", ws, err)
	}

	// One that is stopped, after a second command was refused.
	long, err := eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "fork2", Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "fork2", Args: []string{"true"}})
	want := "workspace fork2 is throwaway: command " + long.Id + " ran in it, and it goes when that ends"
	if e := api.FromConnect(err); api.CodeOf(err) != api.CodeConflict || e.Message != want {
		t.Errorf("a second command in fork2 = %v; want CONFLICT %q", err, want)
	}
	if released("fork2") {
		t.Fatal("fork2 was released while its command ran")
	}
	if _, err := eng.Stop(ctx, long.Id, "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fork2 to be released once its command was stopped", func() bool { return released("fork2") })
}
