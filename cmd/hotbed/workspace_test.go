package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
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
	t.Chdir(agent)
	c.ok(nil, "run", "-s", S, "-r", "app", "--detach", "--mem", "1G", "--", "true")
	ws := &pb.Workspace{}
	c.ok(ws, "workspace", "show", "-s", S)

	// ls lists the session's workspaces; --all every session's.
	c.ok(nil, "session", "start", "calm-wren")
	other := c.workspace("calm-wren", nil)
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
	c.ok(shown, "workspace", "show", "-s", S)
	if shown.Id != ws.Id || shown.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE || len(shown.Repos) != 1 || shown.Repos[0].FilesChanged != 2 {
		t.Errorf("workspace show = %v", shown)
	}

	// files and file show the checkouts against the base, each path below
	// /work, starting with its repo.
	files := &pb.ListFilesResponse{}
	c.ok(files, "workspace", "files", "-s", S)
	var got []string
	for _, f := range files.Files {
		got = append(got, fmt.Sprintf("%s %s %d %s", f.Path, f.Kind, f.SizeBytes, f.Change))
	}
	slices.Sort(got)
	want := []string{
		"app dir 0 ",
		"app/.gitignore file 8 ",
		"app/src dir 0 ",
		"app/src/keep.txt file 5 ",
		"app/src/lib.txt file 7 modified",
		"app/src/new.txt file 4 added",
	}
	if !slices.Equal(got, want) {
		t.Errorf("workspace files = %q, want %q", got, want)
	}
	file := &pb.FileContent{}
	c.ok(file, "workspace", "file", "-s", S, "app/src/lib.txt")
	if file.Path != "app/src/lib.txt" || file.Text != "edited\n" || !slices.Equal(file.ChangedLines, []int32{1}) {
		t.Errorf("workspace file = %v", file)
	}
	if _, e := c.fails("workspace", "file", "-s", S, "app/src/missing.txt"); e.Code != api.CodeNotFound {
		t.Errorf("workspace file of a missing file: %+v", e)
	}
	if _, e := c.fails("workspace", "file", "-s", S, "src/lib.txt"); e.Code != api.CodeInvalidArgs {
		t.Errorf("workspace file of a path in no repo: %+v", e)
	}

	// logs --tail prints a command's last lines.
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", S, "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", "echo one; echo two; echo three")
	c.eventually("the command to end", func() bool {
		ps := &pb.ListCommandsResponse{}
		c.ok(ps, "ps", "-s", S)
		return !containsLive(ps.Commands, cmd.Id)
	})
	if code, out, errOut := c.hotbed("logs", "-s", S, "--tail", "2", cmd.Id); code != 0 || out != "two\nthree\n" {
		t.Errorf("logs --tail 2: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	// Lines past the response's 1 MiB are left out, and stderr says so. Two
	// lines of 600 KiB fit only the last; binary output arrives as UTF-8.
	big := &pb.Command{}
	c.ok(big, "run", "-s", S, "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c",
		`printf 'first\n'; head -c 614400 /dev/zero | tr '\0' a; echo; head -c 614400 /dev/zero | tr '\0' b; printf '\377\n'`)
	c.eventually("the big command to end", func() bool {
		ps := &pb.ListCommandsResponse{}
		c.ok(ps, "ps", "-s", S)
		return !containsLive(ps.Commands, big.Id)
	})
	code, out, errOut := c.hotbed("logs", "-s", S, "--tail", "3", big.Id)
	if want := strings.Repeat("b", 614400) + "�\n"; code != 0 || out != want {
		t.Errorf("logs --tail 3 of 1.2 MiB: exit %d, %d bytes of stdout starting %.20q, want the last line alone", code, len(out), out)
	}
	if want := "hotbed: only the last 1 lines fit in one response; hotbed logs " + big.Id + " without -n prints it all\n"; errOut != want {
		t.Errorf("logs --tail 3 of 1.2 MiB: stderr %q, want %q", errOut, want)
	}
	if _, e := c.fails("logs", "-s", S, "--tail", "2", "-f", cmd.Id); e.Code != api.CodeInvalidArgs {
		t.Errorf("logs --tail with -f: %+v", e)
	}
	if _, e := c.fails("logs", "-s", S, "--tail", "-1", cmd.Id); e.Code != api.CodeInvalidArgs {
		t.Errorf("logs --tail -1: %+v", e)
	}

	// release gives the workspace up; the session goes on, and its next run
	// gets another.
	released := releaseResult{}
	c.ok(&released, "workspace", "release", "-s", S)
	if released != (releaseResult{ID: ws.Id, Project: "app", Node: "asahimbp", CockpitURL: c.cockpit().workspace(ws.Id)}) {
		t.Errorf("workspace release = %+v", released)
	}
	after := &pb.ListWorkspacesResponse{}
	c.ok(after, "workspace", "ls", "-s", S)
	if len(after.Workspaces) != 0 {
		t.Errorf("workspace ls after release = %v", after.Workspaces)
	}
	if code, out, errOut := c.hotbed("run", "-s", S, "-r", "app", "--", "cat", "src/lib.txt"); code != 0 || out != "edited\n" {
		t.Fatalf("run after release: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	again := &pb.Workspace{}
	c.ok(again, "workspace", "show", "-s", S)
	if again.Id == ws.Id {
		t.Errorf("run after release went to released workspace %s", ws.Id)
	}
	// Another session's workspace is not this one's to release.
	if _, e := c.fails("workspace", "release", "-s", S, "--node", "nowhere"); e.Code != api.CodeNotFound {
		t.Errorf("release of a workspace the session has not: %+v", e)
	}
}

func workspaceIDs(res *pb.ListWorkspacesResponse) []string {
	var ids []string
	for _, w := range res.Workspaces {
		ids = append(ids, w.Id)
	}
	return ids
}

// Workspaces and commands carry their session's project and can be filtered
// with -p PROJECT.
func TestWorkspaceAndCommandProjectFiltering(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	c.ok(nil, "project", "add", "other", "--repo", origin, "--shared")

	c.ok(nil, "session", "start", "alpha-fox", "-p", "app")
	c.ok(nil, "session", "start", "beta-owl", "-p", "other")

	wsA := c.workspace("alpha-fox", nil)
	wsB := c.workspace("beta-owl", nil)
	if wsA.Project != "app" || wsB.Project != "other" {
		t.Fatalf("workspace projects = %q, %q; want app, other", wsA.Project, wsB.Project)
	}

	wsList := &pb.ListWorkspacesResponse{}
	c.ok(wsList, "workspace", "ls", "-p", "app")
	if got := workspaceIDs(wsList); !slices.Equal(got, []string{wsA.Id}) || wsList.Workspaces[0].Project != "app" {
		t.Errorf("workspace ls -p app = %v", wsList.Workspaces)
	}
	c.ok(wsList, "workspace", "ls", "-p", "other")
	if got := workspaceIDs(wsList); !slices.Equal(got, []string{wsB.Id}) || wsList.Workspaces[0].Project != "other" {
		t.Errorf("workspace ls -p other = %v", wsList.Workspaces)
	}

	cmdA := &pb.Command{}
	cmdB := &pb.Command{}
	c.ok(cmdA, "run", "-s", "alpha-fox", "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", "sleep 30")
	c.ok(cmdB, "run", "-s", "beta-owl", "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", "sleep 30")
	t.Cleanup(func() {
		c.hotbed("stop", "-s", "alpha-fox", cmdA.Id)
		c.hotbed("stop", "-s", "beta-owl", cmdB.Id)
	})

	// The sessions' commands; base builds show too (TestPsShowsBaseBuilds).
	cmdIDs := func(res *pb.ListCommandsResponse) []string {
		var out []string
		res.Commands = slices.DeleteFunc(res.Commands, func(c *pb.Command) bool { return c.Kind == "freshen" })
		for _, cmd := range res.Commands {
			out = append(out, cmd.Id)
		}
		return out
	}
	ps := &pb.ListCommandsResponse{}
	c.ok(ps, "ps", "-p", "app")
	if got := cmdIDs(ps); !slices.Equal(got, []string{cmdA.Id}) || ps.Commands[0].Project != "app" {
		t.Errorf("ps -p test = %v", ps.Commands)
	}
	c.ok(ps, "ps", "-p", "other")
	if got := cmdIDs(ps); !slices.Equal(got, []string{cmdB.Id}) || ps.Commands[0].Project != "other" {
		t.Errorf("ps -p other = %v", ps.Commands)
	}
	c.ok(ps, "ps", "--all")
	if got := cmdIDs(ps); len(got) != 2 || !slices.Contains(got, cmdA.Id) || !slices.Contains(got, cmdB.Id) {
		t.Errorf("ps --all = %v, want both commands", got)
	}
}
