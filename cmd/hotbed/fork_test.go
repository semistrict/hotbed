package main

import (
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// hotbed run --fork runs in a throwaway workspace of its own, forked from
// the project's base: it sees nothing the session's workspace holds, what
// it leaves goes with it when it ends, and the session's runs beside it are
// in the session's workspace. Each --fork gets a new one.
func TestRunForkRunsInAThrowawayWorkspace(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "fork-elk")
	ws := c.workspace("fork-elk", nil)

	if code, _, stderr := c.hotbed("run", "-s", "fork-elk", "-r", "app", "--", "sh", "-c", "echo session > mark"); code != 0 {
		t.Fatalf("run in the session's workspace: exit %d, stderr %q", code, stderr)
	}
	code, out, stderr := c.hotbed("run", "-s", "fork-elk", "-r", "app", "--fork", "--", "sh", "-c", "cat mark 2>/dev/null || echo none; echo fork > mark")
	if code != 0 || out != "none\n" {
		t.Fatalf("run --fork: exit %d, out %q (stderr %q); want it to see none of the session's workspace", code, out, stderr)
	}
	m := regexp.MustCompile(`running in throwaway workspace (\S+) on `).FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("run --fork said %q; want it to name the throwaway workspace it ran in", stderr)
	}
	attached := m[1]

	var detached pb.Command
	c.ok(&detached, "run", "-s", "fork-elk", "-r", "app", "--fork", "--detach", "--mem", "1G", "--", "sleep", "60")
	if forks := []string{attached, detached.WorkspaceId}; slices.Contains(forks, ws.Id) || attached == detached.WorkspaceId {
		t.Errorf("forks ran in %v; want each in a new workspace, neither the session's %s", forks, ws.Id)
	}
	// While a fork runs, the session's runs are still in its own workspace.
	code, out, stderr = c.hotbed("run", "-s", "fork-elk", "-r", "app", "--", "cat", "mark")
	if code != 0 || out != "session\n" {
		t.Errorf("run beside a fork: exit %d, out %q (stderr %q); want the session's workspace, as it left it", code, out, stderr)
	}
	// The session's workspace is still the one hotbed workspace finds.
	var shown pb.Workspace
	c.ok(&shown, "workspace", "show", "-s", "fork-elk")
	if shown.Id != ws.Id {
		t.Errorf("workspace show beside a fork = %s; want the session's %s", shown.Id, ws.Id)
	}
	c.ok(nil, "stop", "-s", "fork-elk", detached.Id)

	// Each throwaway workspace goes once its command has ended.
	var ids []string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		list, err := c.workspaces().ListWorkspaces(t.Context(), &pb.ListWorkspacesRequest{SessionId: "fork-elk"})
		if err != nil {
			t.Fatal(err)
		}
		ids = ids[:0]
		for _, w := range list.Workspaces {
			ids = append(ids, w.Id)
		}
		if slices.Equal(ids, []string{ws.Id}) {
			break
		}
	}
	if !slices.Equal(ids, []string{ws.Id}) {
		t.Fatalf("the session's workspaces after its forks ended = %v; want only %s", ids, ws.Id)
	}
}

// A --fork run before the session has a workspace gives it none: the
// session's first plain run gets a workspace of its own, not the fork.
func TestRunForkFirstLeavesTheSessionWithoutAWorkspace(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "fork-owl")

	var fork pb.Command
	c.ok(&fork, "run", "-s", "fork-owl", "-r", "app", "--fork", "--detach", "--mem", "1G", "--", "sleep", "60")
	var plain pb.Command
	c.ok(&plain, "run", "-s", "fork-owl", "-r", "app", "--detach", "--mem", "1G", "--", "true")
	if plain.WorkspaceId == fork.WorkspaceId {
		t.Errorf("the session's first plain run went to the fork's workspace %s; want one of its own", fork.WorkspaceId)
	}
	c.ok(nil, "stop", "-s", "fork-owl", fork.Id)
}

// With the controller out of reach a --fork run fails: it cannot fall back
// to the workspace this machine last used, which is the session's own.
func TestRunForkNeedsTheController(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "fork-yak")
	c.workspace("fork-yak", nil)
	c.ctrl.setDown(true)

	if _, e := c.fails("run", "-s", "fork-yak", "-r", "app", "--fork", "--", "true"); e.Code != api.CodeControllerUnreachable {
		t.Errorf("run --fork with the controller down = %+v; want %s", e, api.CodeControllerUnreachable)
	}
}
