package main

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// checkout makes a git checkout whose origin is remote, and goes into it.
func checkout(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "remote", "add", "origin", remote)
	t.Chdir(dir)
	return dir
}

// A session starts in the project that holds its checkout's repo, named by
// its canonical remote; a repo no project holds becomes a project of its own
// name; a repo several projects hold needs the project named.
func TestSessionStartsInItsCheckoutsProject(t *testing.T) {
	c := newCluster(t)
	t.Setenv("HOTBED_PROJECT", "")
	checkout(t, "git@github.com:o/app.git")

	sess := &pb.Session{}
	c.ok(sess, "session", "start", "first-one")
	if sess.Project != "app" {
		t.Fatalf("session in a repo no project holds: project %q, want app, made for it", sess.Project)
	}
	ls := &pb.ListProjectsResponse{}
	c.ok(ls, "project", "ls")
	if got := projectRepos(ls, "app"); !slices.Equal(got, []string{"github.com/o/app"}) {
		t.Fatalf("project app holds %v", got)
	}

	// A project of several repos; the same repo in two projects.
	web := &pb.Project{}
	c.ok(web, "project", "add", "web", "--repo", "https://github.com/o/app", "--repo", "github.com/o/ui", "--shared")
	c.ok(web, "project", "add", "web", "--repo", "ssh://git@github.com/o/api.git")
	if !slices.Equal(web.Repos, []string{"github.com/o/api", "github.com/o/app", "github.com/o/ui"}) {
		t.Errorf("project web holds %v, want the repos of both adds", web.Repos)
	}
	// first-one is bound to this checkout: each start here replaces it.
	if _, e := c.fails("session", "start", "second-one", "--replace"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "github.com/o/app is in projects app, web") {
		t.Errorf("start in a repo two projects hold = %v, want INVALID_ARGS naming them", e)
	}
	if s := c.session("first-one"); s.EndedAt != nil {
		t.Errorf("a refused start ended the session it was to replace: %v", s)
	}
	c.ok(sess, "session", "start", "second-one", "-p", "web", "--replace")
	if sess.Project != "web" {
		t.Errorf("start -p web: project %q", sess.Project)
	}

	// The one project holding a repo is its sessions' default.
	checkout(t, "https://github.com/o/ui.git")
	c.ok(sess, "session", "start", "third-one")
	if sess.Project != "web" {
		t.Errorf("start in ui, which only web holds: project %q", sess.Project)
	}
	if s := c.session("third-one"); s.Project != "web" {
		t.Errorf("the cockpit's session = %v", s)
	}

	if _, e := c.fails("session", "start", "-p", "nosuch", "--replace"); e.Code != api.CodeNotFound {
		t.Errorf("start -p nosuch = %v, want NOT_FOUND", e)
	}
	t.Chdir(t.TempDir())
	if _, e := c.fails("session", "start"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "outside a git checkout") {
		t.Errorf("start outside a checkout without a project = %v, want INVALID_ARGS", e)
	}
	if _, e := c.fails("project", "add", "Bad_Name"); e.Code != api.CodeInvalidArgs {
		t.Errorf("project add Bad_Name = %v, want INVALID_ARGS", e)
	}

	// A repo whose own name is a project holding other repos needs naming.
	checkout(t, "git@github.com:other/web.git")
	if _, e := c.fails("session", "start"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "project web holds other repos") {
		t.Errorf("start in other/web = %v, want INVALID_ARGS", e)
	}
}

func projectRepos(ls *pb.ListProjectsResponse, name string) []string {
	for _, p := range ls.Projects {
		if p.Name == name {
			return p.Repos
		}
	}
	return nil
}

// project ls counts each project's live sessions and questions; session ls
// lists a project's sessions.
func TestProjectsCountTheirWork(t *testing.T) {
	c, user := questionCluster(t)
	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "far-fox", "-p", "other")
	c.ask("brave-otter", storeQuestion)
	c.ask("brave-otter", storeQuestion)
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Other: "yes"}}); err != nil {
		t.Fatal(err)
	}
	c.ok(nil, "session", "end", "-s", "calm-heron")

	ls := &pb.ListProjectsResponse{}
	c.ok(ls, "project", "ls")
	counts := map[string][3]int32{}
	for _, p := range ls.Projects {
		counts[p.Name] = [3]int32{p.LiveSessions, p.OpenQuestions, p.Outbox}
	}
	if want := map[string][3]int32{"test": {1, 1, 1}, "other": {1, 0, 0}}; !maps.Equal(counts, want) {
		t.Errorf("project ls counts (live sessions, open, outbox) = %v, want %v", counts, want)
	}

	sessions := &pb.ListSessionsResponse{}
	c.ok(sessions, "session", "ls")
	if got := sessionIDs(sessions); !slices.Equal(got, []string{"brave-otter"}) {
		t.Errorf("session ls in test = %v", got)
	}
	c.ok(sessions, "session", "ls", "--ended")
	if got := sessionIDs(sessions); !slices.Contains(got, "calm-heron") || len(got) != 2 {
		t.Errorf("session ls --ended = %v, want brave-otter and calm-heron", got)
	}
	c.ok(sessions, "session", "ls", "--all-projects")
	if got := sessionIDs(sessions); len(got) != 2 || !slices.Contains(got, "far-fox") {
		t.Errorf("session ls --all-projects = %v, want brave-otter and far-fox", got)
	}
}

func sessionIDs(res *pb.ListSessionsResponse) []string {
	var out []string
	for _, s := range res.Sessions {
		out = append(out, s.Id)
	}
	return out
}

// A repo's own project takes a name made of its remote's last part, and a
// project of that name that holds no repo yet takes the repo in. A session
// whose name is taken makes no project.
func TestRepoProjectNames(t *testing.T) {
	c := newCluster(t)
	t.Setenv("HOTBED_PROJECT", "")

	checkout(t, "https://github.com/vercel/Next.js.git")
	sess := &pb.Session{}
	c.ok(sess, "session", "start", "next-one")
	if sess.Project != "next-js" {
		t.Errorf("session in vercel/Next.js: project %q, want next-js", sess.Project)
	}

	c.ok(nil, "project", "add", "app")
	checkout(t, "git@github.com:o/app.git")
	c.ok(sess, "session", "start", "app-one")
	ls := &pb.ListProjectsResponse{}
	c.ok(ls, "project", "ls")
	if sess.Project != "app" || !slices.Equal(projectRepos(ls, "app"), []string{"github.com/o/app"}) {
		t.Errorf("session in o/app with an empty project app: project %q holding %v", sess.Project, projectRepos(ls, "app"))
	}

	checkout(t, "git@github.com:o/fresh.git")
	if _, e := c.fails("session", "start", "app-one"); e.Code != api.CodeConflict {
		t.Errorf("start a taken name = %v, want CONFLICT", e)
	}
	c.ok(ls, "project", "ls")
	if projectRepos(ls, "fresh") != nil || len(ls.Projects) != 3 {
		t.Errorf("a refused start made a project: %v", ls.Projects)
	}
}

// Registering a repo auto-creates a single-repo project for it when no project
// holds it yet; adding a repo already held by a project leaves projects alone.
func TestRepoAddCreatesItsOwnProject(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "repo", "add", "cladefs", "https://github.com/semistrict/cladefs.git")

	ls := &pb.ListProjectsResponse{}
	c.ok(ls, "project", "ls")
	if got := projectRepos(ls, "cladefs"); !slices.Equal(got, []string{"github.com/semistrict/cladefs"}) {
		t.Fatalf("project cladefs after repo add = %v, want [github.com/semistrict/cladefs]", got)
	}

	// A repo already in a multi-repo project does not sprout a duplicate project.
	c.ok(nil, "project", "add", "suite", "--repo", "https://github.com/semistrict/other.git")
	c.ok(nil, "repo", "add", "other", "git@github.com:semistrict/other.git")
	c.ok(ls, "project", "ls")
	if projectRepos(ls, "other") != nil {
		t.Errorf("repo already in project suite should not make a project called other: %v", ls.Projects)
	}
}

// A repo another project holds goes into a second only when shared, since
// then sessions starting in its checkouts must name their project; taking it
// out again restores the default. Every change is kept, with who made it.
func TestProjectRepoIsSharedOnlyOnPurpose(t *testing.T) {
	c := newCluster(t)
	t.Setenv("HOTBED_PROJECT", "")
	t.Setenv("HOTBED_SESSION", "")
	t.Setenv("T3CODE_THREAD_ID", "")
	checkout(t, "git@github.com:o/app.git")
	c.ok(nil, "session", "start", "first-one") // makes project app, holding o/app

	if _, e := c.fails("project", "add", "web", "--repo", "https://github.com/o/app.git"); e.Code != api.CodeConflict ||
		!strings.Contains(e.Message, "github.com/o/app is in project app already") || !strings.Contains(e.Suggestion, "--shared") {
		t.Fatalf("add a held repo = %v, want CONFLICT naming app and --shared", e)
	}
	ls := &pb.ListProjectsResponse{}
	c.ok(ls, "project", "ls")
	if projectRepos(ls, "web") != nil || len(ls.Projects) != 2 {
		t.Errorf("a refused add changed the projects: %v", ls.Projects)
	}

	c.ok(nil, "project", "add", "web", "--repo", "https://github.com/o/app.git", "--shared", "-s", "first-one")
	checkout(t, "git@github.com:o/app.git") // another checkout: first-one is bound to the first
	if _, e := c.fails("session", "start", "second-one"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "is in projects app, web") {
		t.Errorf("start in a shared repo = %v, want INVALID_ARGS", e)
	}

	web := &pb.Project{}
	c.ok(web, "project", "rm-repo", "web", "--repo", "github.com/o/app")
	if len(web.Repos) != 0 {
		t.Errorf("web after rm-repo holds %v", web.Repos)
	}
	sess := &pb.Session{}
	c.ok(sess, "session", "start", "second-one")
	if sess.Project != "app" {
		t.Errorf("start after rm-repo: project %q, want app again", sess.Project)
	}
	if _, e := c.fails("project", "rm-repo", "web", "--repo", "github.com/o/app"); e.Code != api.CodeNotFound || !strings.Contains(e.Message, "does not hold github.com/o/app") {
		t.Errorf("rm-repo of a repo it lacks = %v, want NOT_FOUND", e)
	}
	if _, e := c.fails("project", "rm-repo", "nosuch", "--repo", "github.com/o/app"); e.Code != api.CodeNotFound {
		t.Errorf("rm-repo from no such project = %v, want NOT_FOUND", e)
	}

	c.ok(web, "project", "show", "web")
	var got []string
	for _, ch := range web.History {
		who := ch.By.Session
		if who == "" {
			who = "person on " + ch.By.Host
		}
		got = append(got, strings.TrimSpace(ch.What+" "+ch.Repo)+" by "+who)
	}
	want := []string{"created by first-one", "added github.com/o/app by first-one", "removed github.com/o/app by person on laptop"}
	if !slices.Equal(got, want) {
		t.Errorf("web's history = %v\nwant %v", got, want)
	}
	app := &pb.Project{}
	c.ok(app, "project", "show", "app")
	if len(app.History) != 2 || app.History[0].What != "created" || app.History[0].By.Session != "" || !strings.Contains(app.History[0].By.Reason, "which a session started in and no project held") {
		t.Errorf("app's history = %v, want it made by the controller, saying why", app.History)
	}
	if _, e := c.fails("project", "show", "nosuch"); e.Code != api.CodeNotFound {
		t.Errorf("show no such project = %v, want NOT_FOUND", e)
	}
}
