package main

import (
	"maps"
	"path/filepath"
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

// repo add -p attaches the repo to a named project; repo ls -p lists only that
// project's repos.
func TestRepoAddAndListWithProject(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "project", "add", "alpha")
	c.ok(nil, "project", "add", "beta")
	c.ok(nil, "repo", "add", "r1", "https://github.com/o/r1.git", "-p", "alpha")
	c.ok(nil, "repo", "add", "r2", "https://github.com/o/r2.git", "-p", "beta")

	if _, e := c.fails("repo", "add", "r3", "https://github.com/o/r3.git", "-p", "nosuch"); e.Code != api.CodeNotFound {
		t.Errorf("repo add -p nosuch = %v, want NOT_FOUND", e)
	}

	repoIDs := func(res *pb.ListReposResponse) []string {
		var out []string
		for _, r := range res.Repos {
			out = append(out, r.Id)
		}
		return out
	}
	res := &pb.ListReposResponse{}
	c.ok(res, "repo", "ls", "-p", "alpha")
	if got := repoIDs(res); !slices.Equal(got, []string{"r1"}) {
		t.Errorf("repo ls -p alpha = %v, want [r1]", got)
	}
	c.ok(res, "repo", "ls", "-p", "beta")
	if got := repoIDs(res); !slices.Equal(got, []string{"r2"}) {
		t.Errorf("repo ls -p beta = %v, want [r2]", got)
	}
	c.ok(res, "repo", "ls")
	if got := repoIDs(res); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Errorf("repo ls = %v, want [r1 r2]", got)
	}
}

// Every listing of a project's things takes its scope the same way, and
// the controller resolves it: --all-projects outweighs HOTBED_PROJECT, -p
// beside it is refused, neither project nor session is refused, and a
// project or session named must be one. Every request with all_projects
// is here: a new one fails the test until it is.
func TestListingsScopeAlike(t *testing.T) {
	c, _ := questionCluster(t)
	listings := map[string][]string{
		"hotbed.v1.ListQuestionsRequest": {"question", "ls"},
		"hotbed.v1.ListMemoriesRequest":  {"memory", "ls"},
		"hotbed.v1.ListSessionsRequest":  {"session", "ls"},
		"hotbed.v1.ListSchedulesRequest": {"schedule", "ls"},
		"hotbed.v1.ListReviewsRequest":   {"review", "ls"},
	}
	msgs := pb.File_hotbed_v1_hotbed_proto.Messages()
	for i := range msgs.Len() {
		m := msgs.Get(i)
		if _, ok := listings[string(m.FullName())]; m.Fields().ByName("all_projects") != nil && !ok {
			t.Errorf("%s takes all_projects, and its command is not tested here", m.FullName())
		}
	}
	// The cluster's helpers fail the test itself, so no subtests.
	for req, cmd := range listings {
		with := func(args ...string) []string { return append(slices.Clone(cmd), args...) }
		t.Setenv("HOTBED_PROJECT", "test")
		c.ok(nil, with("--all-projects")...)
		if _, e := c.fails(with("-p", "other", "--all-projects")...); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "can't be used together") {
			t.Errorf("%s -p other --all-projects = %v, want INVALID_ARGS", req, e)
		}
		if _, e := c.fails(with("-p", "nosuch")...); e.Code != api.CodeNotFound {
			t.Errorf("%s -p nosuch = %v, want NOT_FOUND", req, e)
		}
		if _, e := c.fails(with("-p", "test", "-s", "no-such-session")...); e.Code != api.CodeNotFound {
			t.Errorf("%s -s no-such-session = %v, want NOT_FOUND", req, e)
		}
		c.ok(nil, with("-s", "brave-otter")...)
		t.Setenv("HOTBED_PROJECT", "")
		if _, e := c.fails(cmd...); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "say which project") {
			t.Errorf("%s with no project or session = %v, want INVALID_ARGS", req, e)
		}
	}
}

// A listing that names no project, in a checkout bound to no session, is of
// the one project that holds the checkout's repo, as session start's
// project is.
func TestListingsFallBackToTheCheckoutsProject(t *testing.T) {
	c := newCluster(t)
	t.Setenv("HOTBED_PROJECT", "")
	t.Setenv("HOTBED_SESSION", "")
	c.ok(nil, "project", "add", "app", "--repo", "github.com/o/app")
	c.ok(nil, "project", "add", "web", "--repo", "github.com/o/ui")
	// Outside a checkout, so bound to none.
	c.ok(nil, "session", "start", "app-one", "-p", "app")
	c.ok(nil, "session", "start", "web-one", "-p", "web")
	c.ask("app-one", storeQuestion)
	c.ask("web-one", storeQuestion)

	dir := checkout(t, "git@github.com:o/app.git")
	write(t, filepath.Join(dir, "a.go"), "package a\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "a")
	c.ok(nil, "review", "request", "-s", "app-one", "--whole")

	reviews := &pb.ListReviewsResponse{}
	c.ok(reviews, "review", "ls")
	if len(reviews.Reviews) != 1 || reviews.Reviews[0].Project != "app" {
		t.Errorf("review ls in o/app = %v, want app's one review", reviews.Reviews)
	}
	if got := c.listed(); !slices.Equal(got, []int32{1}) {
		t.Errorf("question ls in o/app = %v, want app's question 1", got)
	}
	outbox := &pb.ListQuestionsResponse{}
	c.ok(outbox, "question", "outbox")
	if len(outbox.Questions) != 0 {
		t.Errorf("question outbox in o/app = %v, want none answered", outbox.Questions)
	}
	sessions := &pb.ListSessionsResponse{}
	c.ok(sessions, "session", "ls")
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].Id != "app-one" {
		t.Errorf("session ls in o/app = %v, want app-one", sessions.Sessions)
	}
	c.ok(nil, "schedule", "ls")

	// -p, --all-projects and a session still say whose.
	if got := c.listed("-p", "web"); !slices.Equal(got, []int32{2}) {
		t.Errorf("question ls -p web = %v", got)
	}
	if got := c.listed("-s", "web-one"); !slices.Equal(got, []int32{2}) {
		t.Errorf("question ls -s web-one = %v", got)
	}
	c.ok(reviews, "review", "ls", "-s", "web-one")
	if len(reviews.Reviews) != 0 {
		t.Errorf("review ls -s web-one = %v, want web's none", reviews.Reviews)
	}

	// A repo two projects hold, or none, needs naming.
	c.ok(nil, "project", "add", "web", "--repo", "github.com/o/app", "--shared")
	for _, cmd := range [][]string{{"review", "ls"}, {"question", "ls"}, {"session", "ls"}, {"schedule", "ls"}} {
		if _, e := c.fails(cmd...); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "github.com/o/app, this checkout's repo, is in projects app, web") {
			t.Errorf("%v in a repo two projects hold = %v, want INVALID_ARGS naming them", cmd, e)
		}
	}
	checkout(t, "git@github.com:o/nobody.git")
	if _, e := c.fails("review", "ls"); e.Code != api.CodeNotFound || !strings.Contains(e.Message, "no project holds github.com/o/nobody") {
		t.Errorf("review ls in a repo no project holds = %v, want NOT_FOUND", e)
	}
	t.Chdir(t.TempDir())
	if _, e := c.fails("review", "ls"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "say which project") {
		t.Errorf("review ls outside a checkout = %v, want INVALID_ARGS", e)
	}
}

// session start and everything that creates a project or adds a repo to one
// returns a text reminder (in both plain text and --json) to add setup steps
// so common dev commands run without downloading or compiling anything.
// Read/list endpoints and storage never retain that transient reminder.
func TestSetupStepsReminder(t *testing.T) {
	c := newCluster(t)
	t.Setenv("HOTBED_PROJECT", "")
	t.Setenv("HOTBED_SESSION", "")
	t.Setenv("T3CODE_THREAD_ID", "")

	checkReminder := func(t *testing.T, label, text, project string, hasSteps bool) {
		t.Helper()
		for _, want := range []string{"Reminder:", "setup steps", "toolchain", "without downloading or compiling anything"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: missing %q in %q", label, want, text)
			}
		}
		if project != "" && !strings.Contains(text, project) {
			t.Errorf("%s: missing project %q in %q", label, project, text)
		}
		noSteps := strings.Contains(text, "has no setup steps yet")
		if hasSteps && noSteps {
			t.Errorf("%s: says no setup steps yet even though project %q has steps: %q", label, project, text)
		}
		if !hasSteps && project != "" && !noSteps {
			t.Errorf("%s: want no-setup-steps wording for %q, got %q", label, project, text)
		}
	}

	// 1. project add (plain text) creating a new project with no steps yet.
	code, stdout, stderr := c.hotbed("project", "add", "suite", "--repo", "https://github.com/o/app.git")
	if code != 0 {
		t.Fatalf("project add suite failed (%d): %s", code, stderr)
	}
	if strings.Contains(stdout, `\n`) {
		t.Errorf("project add (plain) contains literal \\n: %q", stdout)
	}
	checkReminder(t, "project add (plain)", stdout, "suite", false)

	// Adding a repo to an existing project (JSON), before and after adding a step.
	proj := &pb.Project{}
	c.ok(proj, "project", "add", "suite", "--repo", "https://github.com/o/api.git")
	checkReminder(t, "project add repo (json, no steps)", proj.GetReminder(), "suite", false)

	c.ok(nil, "step", "set", "-p", "suite", "build", "--every-pull", "--", "true")
	proj = &pb.Project{}
	c.ok(proj, "project", "add", "suite", "--repo", "https://github.com/o/web.git")
	checkReminder(t, "project add repo (json, with steps)", proj.GetReminder(), "suite", true)

	// project show / ls do not carry a reminder.
	shown := &pb.Project{}
	c.ok(shown, "project", "show", "suite")
	if shown.Reminder != nil {
		t.Errorf("project show carried reminder: %q", shown.GetReminder())
	}
	lsProj := &pb.ListProjectsResponse{}
	c.ok(lsProj, "project", "ls")
	for _, p := range lsProj.Projects {
		if p.Reminder != nil {
			t.Errorf("project ls %s carried reminder: %q", p.Name, p.GetReminder())
		}
	}

	// 2. repo add auto-creating a project (plain text) and attaching to -p (JSON).
	code, stdout, stderr = c.hotbed("repo", "add", "fresh", "https://github.com/o/fresh.git")
	if code != 0 {
		t.Fatalf("repo add fresh failed (%d): %s", code, stderr)
	}
	if strings.Contains(stdout, `\n`) {
		t.Errorf("repo add (plain) contains literal \\n: %q", stdout)
	}
	checkReminder(t, "repo add auto-project (plain)", stdout, "fresh", false)

	repo := &pb.Repo{}
	c.ok(repo, "repo", "add", "extra", "https://github.com/o/extra.git", "-p", "suite")
	checkReminder(t, "repo add -p suite (json)", repo.GetReminder(), "suite", true)

	// repo ls does not carry a persisted reminder.
	lsRepos := &pb.ListReposResponse{}
	c.ok(lsRepos, "repo", "ls")
	for _, r := range lsRepos.Repos {
		if r.Reminder != nil {
			t.Errorf("repo ls %s carried reminder: %q", r.Id, r.GetReminder())
		}
	}

	// 3. session start (plain text auto-creating a project, JSON in a project
	// with steps, and --resume).
	checkout(t, "git@github.com:o/brandnew.git")
	code, stdout, stderr = c.hotbed("session", "start", "new-fox")
	if code != 0 {
		t.Fatalf("session start new-fox failed (%d): %s", code, stderr)
	}
	if strings.Contains(stdout, `\n`) {
		t.Errorf("session start (plain) contains literal \\n: %q", stdout)
	}
	checkReminder(t, "session start auto-project (plain)", stdout, "brandnew", false)

	sess := &pb.Session{}
	c.ok(sess, "session", "start", "warm-owl", "-p", "suite", "--replace")
	checkReminder(t, "session start existing project with steps (json)", sess.GetReminder(), "suite", true)

	c.ok(nil, "session", "end", "-s", "warm-owl")
	resumed := &pb.Session{}
	c.ok(resumed, "session", "start", "warm-owl", "--resume")
	checkReminder(t, "session start --resume (json)", resumed.GetReminder(), "suite", true)

	// session end / ls do not carry a reminder.
	ended := &pb.Session{}
	c.ok(ended, "session", "end", "-s", "warm-owl")
	if ended.Reminder != nil {
		t.Errorf("session end carried reminder: %q", ended.GetReminder())
	}
	lsSess := &pb.ListSessionsResponse{}
	c.ok(lsSess, "session", "ls", "-p", "suite", "--ended")
	for _, s := range lsSess.Sessions {
		if s.Reminder != nil {
			t.Errorf("session ls %s carried reminder: %q", s.Id, s.GetReminder())
		}
	}

	// 4. schedule add returns no reminder when its repo is already in a
	// project, and returns one when it re-adds an orphaned repo to a project.
	sched := &pb.ScheduledCommand{}
	c.ok(sched, "schedule", "add", "nightly-fresh", "-r", "fresh", "--cron", "0 2 * * *", "--", "true")
	if sched.Reminder != nil {
		t.Errorf("schedule add for repo already in project carried reminder: %q", sched.GetReminder())
	}
	c.ok(nil, "project", "rm-repo", "fresh", "--repo", "https://github.com/o/fresh.git")
	sched = &pb.ScheduledCommand{}
	c.ok(sched, "schedule", "add", "nightly-fresh-reattach", "-r", "fresh", "--cron", "0 3 * * *", "--", "true")
	checkReminder(t, "schedule add re-attaching repo to project (json)", sched.GetReminder(), "fresh", false)
	shownSched := &pb.ScheduledCommand{}
	c.ok(shownSched, "schedule", "show", "nightly-fresh-reattach")
	if shownSched.Reminder != nil {
		t.Errorf("schedule show persisted reminder: %q", shownSched.GetReminder())
	}
}
