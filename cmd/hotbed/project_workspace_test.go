package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

// A project's workspace has every repo it holds checked out side by side
// at /work/<repo>, built by the project's steps, which run in /work. A run
// syncs the checkout it is run in into its repo's directory and starts
// there; one run outside any checkout starts in /work. Any repo's branch
// moving freshens the next base once the optional freshen cooldown passes.
func TestProjectWorkspaceHasEveryRepo(t *testing.T) {
	wclock := &clock{}
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Now = wclock.Now })
	app, lib := upstream(t), upstream(t)
	write(t, filepath.Join(lib, "src/lib.txt"), "lib\n")
	gitIn(t, lib, "commit", "--quiet", "-am", "Lib's own")
	c.ok(nil, "project", "add", "duo", "--repo", app, "--repo", lib, "--shared")
	c.ok(nil, "repo", "add", "app", app)
	c.ok(nil, "repo", "add", "lib", lib)
	t.Setenv("HOTBED_PROJECT", "duo")
	// One step builds across both repos, from /work.
	project := &pb.Project{}
	c.ok(project, "step", "set", "-p", "duo", "link", "--every-pull", "--", `cat app/src/lib.txt lib/src/lib.txt > linked.txt`)
	if len(project.Steps) != 1 || project.Steps[0].Name != "link" {
		t.Fatalf("project after step set = %v", project)
	}

	const S = "twin-owl"
	c.ok(nil, "session", "start", S)
	ws := c.workspace(S, nil)
	var repos []string
	for _, r := range ws.Repos {
		repos = append(repos, r.Repo)
	}
	if ws.Project != "duo" || ws.BaseId != "duo-g1" || !slices.Equal(repos, []string{"app", "lib"}) {
		t.Fatalf("workspace = %v, want duo-g1 with app and lib", ws)
	}

	// Outside any checkout, a run starts in /work, beside every repo and
	// what the steps built there.
	t.Chdir(t.TempDir())
	if code, out, errOut := c.hotbed("run", "-s", S, "--", "sh", "-c", "ls; cat linked.txt"); code != 0 || out != "app\nlib\nlinked.txt\nbase\nlib\n" {
		t.Fatalf("run outside a checkout: exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	// From a checkout of lib, a run syncs it into /work/lib and starts there,
	// leaving app as the base has it.
	agent := filepath.Join(t.TempDir(), "lib")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", lib, agent)
	write(t, filepath.Join(agent, "src/lib.txt"), "lib edited\n")
	t.Chdir(agent)
	code, out, errOut := c.hotbed("run", "-s", S, "--", "sh", "-c", `basename "$PWD"; cat src/lib.txt ../app/src/lib.txt`)
	if code != 0 || out != "lib\nlib edited\nbase\n" || !strings.Contains(errOut, "synced 1 changed files") {
		t.Fatalf("run from lib's checkout: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	shown := &pb.Workspace{}
	c.ok(shown, "workspace", "show", "-s", S)
	got := map[string]string{}
	for _, r := range shown.Repos {
		got[r.Repo] = fmt.Sprintf("changed %d from %q", r.FilesChanged, r.SyncDir)
	}
	root := gitIn(t, agent, "rev-parse", "--show-toplevel")
	if want := map[string]string{"app": `changed 0 from ""`, "lib": fmt.Sprintf("changed 1 from %q", root)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("workspace repos = %v, want %v", got, want)
	}
	files := &pb.ListFilesResponse{}
	c.ok(files, "workspace", "files", "-s", S)
	var changed []string
	for _, f := range files.Files {
		if f.Change != "" {
			changed = append(changed, f.Path+" "+f.Change)
		}
	}
	if !slices.Equal(changed, []string{"lib/src/lib.txt modified"}) {
		t.Errorf("changed files = %v, want lib's edit alone", changed)
	}

	// Either repo moving freshens the next base once cooldown elapses, running
	// the every-pull step over both.
	write(t, filepath.Join(app, "src/lib.txt"), "app moved\n")
	gitIn(t, app, "commit", "--quiet", "-am", "Move app")
	wclock.advance(31 * time.Minute)
	c.ok(nil, "session", "start", "next-owl")
	next := c.workspace("next-owl", nil)
	g2 := c.base(next.BaseId)
	heads := map[string]string{}
	for _, r := range g2.Repos {
		heads[r.Repo] = r.Commit
	}
	if g2.Id != "duo-g2" || g2.Cold || heads["app"] != gitIn(t, app, "rev-parse", "HEAD") || heads["lib"] != gitIn(t, lib, "rev-parse", "HEAD") {
		t.Fatalf("base after app moved = %v, want duo-g2, incremental, at both heads", g2)
	}
	t.Chdir(t.TempDir())
	if code, out, errOut := c.hotbed("run", "-s", "next-owl", "--", "cat", "linked.txt"); code != 0 || out != "app moved\nlib\n" {
		t.Errorf("the step's output in the new base: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// A run of a repo its session's project does not hold is refused, saying
// how to add it: its checkout would be in none of the project's workspaces.
func TestRunOfARepoOutsideTheProjectIsRefused(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	other := upstream(t)
	c.ok(nil, "project", "add", "elsewhere", "--repo", other)
	c.ok(nil, "repo", "add", "stray", other)
	c.ok(nil, "session", "start", "lost-elk")
	code, e := c.fails("run", "-s", "lost-elk", "-r", "stray", "--", "true")
	if code != exitError || e.Message != "session lost-elk works on project app, which holds no repo stray" {
		t.Errorf("run of another project's repo: exit %d, %+v", code, e)
	}
}

// project hosts says which nodes keep a project's bases: runs go only to
// them, and bases elsewhere go once unused.
func TestProjectHostsKeepItsBases(t *testing.T) {
	c := newFleet(t, 2)
	c.addRepo(upstream(t))
	project := &pb.Project{}
	c.ok(project, "project", "hosts", "app", "mini")
	if !slices.Equal(project.Hosts, []string{"mini"}) {
		t.Fatalf("project hosts = %v, want [mini]", project.Hosts)
	}
	c.ok(nil, "session", "start", "far-fox")
	if ws := c.workspace("far-fox", nil); ws.Node != "mini" {
		t.Errorf("workspace on %s, want mini, the one node keeping app's bases", ws.Node)
	}
	if _, e := c.tryWorkspace("far-fox", &pb.Placement{Node: "asahimbp"}); e == nil || e.Message != "no node can take the run: asahimbp keeps no bases of the project" {
		t.Errorf("a run sent to asahimbp: %v, want refused: it keeps no bases of app", e)
	}
	c.ok(project, "project", "hosts", "app")
	if len(project.Hosts) != 0 {
		t.Errorf("project hosts with none = %v, want every worker", project.Hosts)
	}
}
