package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestWorkspaceCommands(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	const S = "keen-lynx"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	write(t, filepath.Join(agent, "src/lib.txt"), "edited\n")
	write(t, filepath.Join(agent, "src/new.txt"), "new\n")
	ws := &pb.Workspace{}
	c.ok(ws, "add", "-s", S, "-r", "app", "--sync", agent)

	// ls lists the session's workspaces; --all every session's.
	c.ok(nil, "session", "start", "calm-wren")
	other := &pb.Workspace{}
	c.ok(other, "add", "-s", "calm-wren", "-r", "app")
	mine := &pb.ListWorkspacesResponse{}
	c.ok(mine, "workspace", "ls", "-s", S)
	if got := workspaceIDs(mine); !slices.Equal(got, []string{ws.Id}) {
		t.Errorf("workspace ls = %q, want [%s]", got, ws.Id)
	}
	all := &pb.ListWorkspacesResponse{}
	c.ok(all, "workspace", "ls", "--all")
	if got := workspaceIDs(all); !slices.Equal(got, []string{ws.Id, other.Id}) {
		t.Errorf("workspace ls --all = %q, want [%s %s]", got, ws.Id, other.Id)
	}

	shown := &pb.Workspace{}
	c.ok(shown, "workspace", "show", "-s", S, "-r", "app")
	if shown.Id != ws.Id || shown.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE || shown.FilesChanged != 2 {
		t.Errorf("workspace show = %v", shown)
	}

	// files and file show the checkout against its base.
	files := &pb.ListFilesResponse{}
	c.ok(files, "workspace", "files", "-s", S, "-r", "app")
	var got []string
	for _, f := range files.Files {
		got = append(got, fmt.Sprintf("%s %s %d %s", f.Path, f.Kind, f.SizeBytes, f.Change))
	}
	slices.Sort(got)
	want := []string{
		".gitignore file 8 ",
		"src dir 0 ",
		"src/keep.txt file 5 ",
		"src/lib.txt file 7 modified",
		"src/new.txt file 4 added",
	}
	if !slices.Equal(got, want) {
		t.Errorf("workspace files = %q, want %q", got, want)
	}
	file := &pb.FileContent{}
	c.ok(file, "workspace", "file", "-s", S, "-r", "app", "src/lib.txt")
	if file.Path != "src/lib.txt" || file.Text != "edited\n" || !slices.Equal(file.ChangedLines, []int32{1}) {
		t.Errorf("workspace file = %v", file)
	}
	if _, e := c.fails("workspace", "file", "-s", S, "-r", "app", "src/missing.txt"); e.Code != api.CodeNotFound {
		t.Errorf("workspace file of a missing file: %+v", e)
	}

	// logs --tail prints a command's last lines.
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", S, "-r", "app", "--detach", "--", "sh", "-c", "echo one; echo two; echo three")
	c.eventually("the command to end", func() bool {
		ps := &pb.ListCommandsResponse{}
		c.ok(ps, "ps", "-s", S)
		return !containsLive(ps.Commands, cmd.Id)
	})
	if code, out, errOut := c.hotbed("logs", "-s", S, "--tail", "2", cmd.Id); code != 0 || out != "two\nthree\n" {
		t.Errorf("logs --tail 2: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if _, e := c.fails("logs", "-s", S, "--tail", "2", "-f", cmd.Id); e.Code != api.CodeInvalidArgs {
		t.Errorf("logs --tail with -f: %+v", e)
	}
	if _, e := c.fails("logs", "-s", S, "--tail", "-1", cmd.Id); e.Code != api.CodeInvalidArgs {
		t.Errorf("logs --tail -1: %+v", e)
	}

	// release gives the workspace up; the session goes on and can add another.
	released := releaseResult{}
	c.ok(&released, "workspace", "release", "-s", S, "-r", "app")
	if released != (releaseResult{ID: ws.Id, Repo: "app"}) {
		t.Errorf("workspace release = %+v", released)
	}
	after := &pb.ListWorkspacesResponse{}
	c.ok(after, "workspace", "ls", "-s", S)
	if len(after.Workspaces) != 0 {
		t.Errorf("workspace ls after release = %v", after.Workspaces)
	}
	if _, e := c.fails("run", "-s", S, "-r", "app", "--", "true"); e.Code != api.CodeNotFound {
		t.Errorf("run after release: %+v", e)
	}
	if k, err := loadKnown(c.cli, S); err != nil || k["app"].ID != "" {
		t.Errorf("this machine still knows the released workspace: %v, %v", k, err)
	}
	again := &pb.Workspace{}
	c.ok(again, "add", "-s", S, "-r", "app")
	if again.Id == ws.Id {
		t.Errorf("add after release gave back released workspace %s", ws.Id)
	}
	// Another session's workspace is not this one's to release.
	if _, e := c.fails("workspace", "release", "-s", S, "-r", other.Id); e.Code != api.CodeNotFound {
		t.Errorf("release of another session's workspace: %+v", e)
	}
}

func workspaceIDs(res *pb.ListWorkspacesResponse) []string {
	var ids []string
	for _, w := range res.Workspaces {
		ids = append(ids, w.Id)
	}
	return ids
}
