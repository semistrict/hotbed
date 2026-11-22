package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// reviewed is a checkout to review, and the commits in it: work's head
// over main's, which origin/HEAD names.
type reviewed struct {
	dir, main, head string
}

// reviewCluster is questionCluster (brave-otter asks for reviews,
// calm-heron reviews) in a checkout with one commit of work on main, and the
// user's own review client: the cockpit's calls.
func reviewCluster(t *testing.T) (*cluster, hotbedv1connect.ReviewServiceClient, reviewed) {
	t.Helper()
	c, _ := questionCluster(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "a.go"), "package a\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "main")
	main := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "remote", "add", "origin", "git@example.invalid:o/app.git")
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", main)
	gitIn(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	gitIn(t, dir, "checkout", "-qb", "work")
	write(t, filepath.Join(dir, "a.go"), "package a\n\nfunc A() {}\n")
	gitIn(t, dir, "commit", "-qam", "work")
	t.Chdir(dir)
	addr, err := c.cli.controller()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := api.Dial(addr, api.DefaultPort, c.cli.http)
	if err != nil {
		t.Fatal(err)
	}
	return c, hotbedv1connect.NewReviewServiceClient(conn), reviewed{dir: dir, main: main, head: gitIn(t, dir, "rev-parse", "HEAD")}
}

// twoFindings is ReportFindings' JSON, the first with locations besides.
const twoFindings = `{"findings": [
  {"file": "a.go", "line": 3, "short_summary": "A ignores its error", "summary": "A drops the error from B.",
   "failure_scenario": "B fails: A returns nil", "category": "correctness", "severity": "important", "verdict": "CONFIRMED",
   "locations": [{"path": "b.go", "start_line": 10, "end_line": 12, "excerpt": "if err != nil {\n\treturn nil\n}"}]},
  {"summary": "The package has no doc comment.", "category": "docs", "severity": "nit"}
]}`

func (c *cluster) review(id int32) *pb.Review {
	c.t.Helper()
	r := &pb.Review{}
	c.ok(r, "review", "show", fmt.Sprint(id))
	return r
}

func reviewWhats(r *pb.Review) []string {
	var out []string
	for _, e := range r.Events {
		who := e.By.Session
		if who == "" {
			who = "user"
		}
		w := e.What + " by " + who
		if e.FindingId != 0 {
			w = fmt.Sprintf("%s %d by %s", e.What, e.FindingId, who)
		}
		out = append(out, w)
	}
	return out
}

// hotbedAsync runs the CLI in the background and hands back its envelope's
// data decoded into m, or its failure: a failing call in a goroutine must
// fail the test, not leave it waiting.
func (c *cluster) hotbedAsync(m proto.Message, args ...string) <-chan error {
	done := make(chan error, 1)
	go func() {
		code, stdout, _ := c.hotbed(args...)
		var out output
		switch err := json.Unmarshal([]byte(stdout), &out); {
		case err != nil || code != 0 || !out.OK:
			done <- fmt.Errorf("hotbed %v: exit %d, %s", args, code, stdout)
		default:
			done <- protojson.Unmarshal(out.Data, m)
		}
	}()
	return done
}

// A review goes from request to findings to decisions and the user's
// responses, each step kept with the session that took it.
func TestReviewRoundTrip(t *testing.T) {
	c, user, v := reviewCluster(t)
	r := &pb.Review{}
	c.ok(r, "review", "request", "-s", "brave-otter", "--title", "Add A", "--instructions", "look for dropped errors")
	if r.Id != 1 || r.State != pb.ReviewState_REVIEW_STATE_REQUESTED || r.Project != "test" || r.Repo != "example.invalid/o/app" ||
		r.Base != v.main || r.Head != v.head || r.Branch != "work" || r.SnapshotOf != "" || r.Requested.Session != "brave-otter" || r.Instructions != "look for dropped errors" {
		t.Fatalf("requested review = %v", r)
	}
	if pinned := gitIn(t, v.dir, "rev-parse", "refs/hotbed/reviews/"+v.head); pinned != v.head {
		t.Errorf("refs/hotbed/reviews/%s = %s", v.head, pinned)
	}
	c.ok(r, "review", "wait", "1", "--timeout", "1")
	if r.State != pb.ReviewState_REVIEW_STATE_REQUESTED {
		t.Errorf("wait that timed out = %v", r.State)
	}

	c.ok(r, "review", "start", "1", "-s", "calm-heron")
	c.ok(r, "review", "start", "1", "-s", "calm-heron") // again: a no-op
	if r.State != pb.ReviewState_REVIEW_STATE_IN_PROGRESS || r.Started.Session != "calm-heron" {
		t.Fatalf("started review = %v", r)
	}
	if _, e := c.fails("review", "start", "1", "-s", "brave-otter"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "in progress by calm-heron") {
		t.Errorf("start a review in progress = %v, want CONFLICT", e)
	}
	if _, e := c.failsIn(twoFindings, "review", "findings", "1", "-s", "brave-otter"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "calm-heron's to review") {
		t.Errorf("findings from another session = %v, want CONFLICT", e)
	}

	found := &pb.AddFindingsResponse{}
	c.okIn(twoFindings, found, "review", "findings", "1", "-s", "calm-heron")
	if len(found.Findings) != 2 {
		t.Fatalf("findings = %v", found)
	}
	f := found.Findings[0]
	if f.Id != 1 || f.Title != "A ignores its error" || f.Severity != pb.FindingSeverity_FINDING_SEVERITY_IMPORTANT || f.Verdict != pb.FindingVerdict_FINDING_VERDICT_CONFIRMED ||
		len(f.Locations) != 2 || f.Locations[0].Path != "a.go" || f.Locations[0].StartLine != 3 || f.Locations[1].Path != "b.go" || f.Locations[1].EndLine != 12 ||
		f.Decision != pb.FindingDecision_FINDING_DECISION_OPEN || f.Found.Session != "calm-heron" {
		t.Errorf("finding 1 = %v", f)
	}
	if n := found.Findings[1]; n.Title != "The package has no doc comment." || len(n.Locations) != 0 || n.Severity != pb.FindingSeverity_FINDING_SEVERITY_NIT {
		t.Errorf("finding 2, with no location = %v", n)
	}

	// The requester waits; the reviewer finishes.
	waited := &pb.Review{}
	done := c.hotbedAsync(waited, "review", "wait", "1", "-s", "brave-otter")
	start := time.Now()
	c.ok(nil, "review", "finish", "1", "-s", "calm-heron", "--summary", "One dropped error.\nOne nit.")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if waited.State != pb.ReviewState_REVIEW_STATE_DONE || waited.Summary != "One dropped error.\nOne nit." || len(waited.Findings) != 2 {
		t.Errorf("waited review = %v", waited)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("the wait took %v: the end did not wake it", took)
	}
	ev := &pb.WatchSessionResponse{}
	c.ok(ev, "wait", "-s", "brave-otter", "--timeout", "5")
	if !slices.ContainsFunc(ev.Events, func(e *pb.SessionEvent) bool {
		return e.What == "reviewed" && e.ReviewId == 1 && e.Detail == "review 1 (Add A) is done: One dropped error."
	}) {
		t.Errorf("brave-otter's events = %v, want the review done", ev.Events)
	}

	decided := &pb.DecideFindingsResponse{}
	c.ok(decided, "review", "accept", "1", "1", "-s", "brave-otter", "--reason", "will fix")
	c.ok(decided, "review", "dismiss", "1", "2", "-s", "brave-otter", "--reason", "not worth it")
	if d := decided.Findings[0]; d.Decision != pb.FindingDecision_FINDING_DECISION_DISMISSED || d.Decided.Reason != "not worth it" || d.Decided.Session != "brave-otter" {
		t.Errorf("dismissed = %v", d)
	}
	if _, e := c.fails("review", "dismiss", "1", "2", "-s", "brave-otter", "--reason", "again"); e.Code != api.CodeConflict {
		t.Errorf("dismiss twice = %v, want CONFLICT", e)
	}
	c.ok(decided, "review", "reopen", "1", "2", "-s", "brave-otter")
	if d := decided.Findings[0]; d.Decision != pb.FindingDecision_FINDING_DECISION_OPEN || d.Decided != nil {
		t.Errorf("reopened = %v", d)
	}

	link := struct{ URL string }{}
	c.ok(&link, "review", "link", "1")
	addr, _ := c.cli.controller()
	if link.URL != "http://"+addr+"/reviews/1" {
		t.Errorf("link = %q", link.URL)
	}

	// The user responds in the cockpit; the requester reads it.
	if _, err := user.Respond(t.Context(), &pb.RespondRequest{ReviewId: 1, FindingId: 1, Quick: pb.QuickResponse_QUICK_RESPONSE_FIX_IF_VERIFIED, Text: "and add a test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := user.Respond(t.Context(), &pb.RespondRequest{ReviewId: 1, Text: "thanks"}); err != nil {
		t.Fatal(err)
	}
	c.ok(r, "review", "wait", "1", "--responses", "-s", "brave-otter", "--timeout", "5")
	responses := &pb.ListResponsesResponse{}
	c.ok(responses, "review", "responses", "1")
	if len(responses.Responses) != 2 || responses.Responses[0].Quick != pb.QuickResponse_QUICK_RESPONSE_FIX_IF_VERIFIED ||
		responses.Responses[0].FindingId != 1 || responses.Responses[0].By.Session != "" || responses.Responses[1].Text != "thanks" {
		t.Fatalf("responses = %v", responses.Responses)
	}
	c.ok(responses, "review", "responses", "1")
	if len(responses.Responses) != 0 {
		t.Errorf("responses again = %v, want none new", responses.Responses)
	}
	c.ok(ev, "wait", "-s", "brave-otter", "--timeout", "5")
	if !slices.ContainsFunc(ev.Events, func(e *pb.SessionEvent) bool {
		return e.What == "responded" && e.Detail == `the user on finding 1 (A ignores its error) of review 1: fix if verified; "and add a test"`
	}) {
		t.Errorf("brave-otter's events = %v, want the user's response", ev.Events)
	}

	// The requester replies; its own reply is no news to its wait.
	c.ok(nil, "review", "respond", "1", "--finding", "1", "--comment", "verified and fixed", "-s", "brave-otter")
	start = time.Now()
	c.ok(r, "review", "wait", "1", "--responses", "-s", "brave-otter", "--timeout", "1")
	if time.Since(start) < 900*time.Millisecond {
		t.Errorf("wait --responses returned at once for the session's own reply")
	}

	r = c.review(1)
	if got := r.Findings[0]; got.Quick != pb.QuickResponse_QUICK_RESPONSE_FIX_IF_VERIFIED || got.Responses != 2 || got.Decision != pb.FindingDecision_FINDING_DECISION_ACCEPTED {
		t.Errorf("finding 1 = %v", got)
	}
	want := []string{
		"requested by brave-otter", "started by calm-heron", "found 1 by calm-heron", "found 2 by calm-heron", "finished by calm-heron",
		"accepted 1 by brave-otter", "dismissed 2 by brave-otter", "reopened 2 by brave-otter",
		"responded 1 by user", "responded by user", "responded 1 by brave-otter",
	}
	if got := reviewWhats(r); !slices.Equal(got, want) {
		t.Errorf("events = %v\nwant %v", got, want)
	}
	ls := &pb.ListReviewsResponse{}
	c.ok(ls, "review", "ls")
	if len(ls.Reviews) != 1 || ls.Reviews[0].Important != 1 || ls.Reviews[0].Nits != 1 || ls.Reviews[0].Open != 1 || ls.Reviews[0].Responses != 3 || ls.Reviews[0].Findings != nil {
		t.Errorf("review ls = %v", ls.Reviews)
	}
	projects := &pb.ListProjectsResponse{}
	c.ok(projects, "project", "ls")
	if p := projects.Projects[0]; p.Name != "test" || p.ActiveReviews != 0 || p.ReviewsToDecide != 1 {
		t.Errorf("project = %v, want one review to decide", p)
	}
}

// Uncommitted work is reviewed as a snapshot commit on HEAD, which git
// keeps.
func TestReviewSnapshotsUncommittedWork(t *testing.T) {
	c, _, v := reviewCluster(t)
	write(t, filepath.Join(v.dir, "a.go"), "package a\n\nfunc A() { panic(1) }\n")
	write(t, filepath.Join(v.dir, "new.go"), "package a\n")
	r := &pb.Review{}
	c.ok(r, "review", "request", "-s", "brave-otter")
	if r.SnapshotOf != v.head || r.Head == v.head || r.Base != v.main || r.Title != "work at "+r.Head[:8] {
		t.Fatalf("review of uncommitted work = %v", r)
	}
	if got := gitIn(t, v.dir, "show", r.Head+":a.go"); got != "package a\n\nfunc A() { panic(1) }" {
		t.Errorf("the snapshot's a.go = %q", got)
	}
	if got := gitIn(t, v.dir, "rev-parse", "refs/hotbed/reviews/"+r.Head); got != r.Head {
		t.Errorf("snapshot pinned at %q", got)
	}
	if status := gitIn(t, v.dir, "status", "--porcelain"); status != "M a.go\n?? new.go" && status != " M a.go\n?? new.go" {
		t.Errorf("the request changed the checkout: %q", status)
	}

	c.ok(r, "review", "request", "-s", "brave-otter", "--whole", "--head", "HEAD")
	if r.Base != "" || r.Head != v.head || r.SnapshotOf != "" {
		t.Errorf("--whole --head HEAD = %v", r)
	}
}

// A review fails, or is cancelled, with why; its requester hears of a
// failure; ends are final.
func TestReviewEnds(t *testing.T) {
	c, _, _ := reviewCluster(t)
	c.ok(nil, "review", "request", "-s", "brave-otter")
	c.ok(nil, "review", "start", "1", "-s", "calm-heron")
	if _, e := c.fails("review", "fail", "1", "-s", "calm-heron", "--reason", " "); e.Code != api.CodeInvalidArgs {
		t.Errorf("fail without a reason = %v, want INVALID_ARGS", e)
	}
	r := &pb.Review{}
	c.ok(r, "review", "fail", "1", "-s", "calm-heron", "--reason", "head does not build")
	if r.State != pb.ReviewState_REVIEW_STATE_FAILED || r.Summary != "head does not build" || r.Ended.Session != "calm-heron" {
		t.Errorf("failed review = %v", r)
	}
	ev := &pb.WatchSessionResponse{}
	c.ok(ev, "wait", "-s", "brave-otter", "--timeout", "5")
	if !slices.ContainsFunc(ev.Events, func(e *pb.SessionEvent) bool { return e.What == "review failed" && e.ReviewId == 1 }) {
		t.Errorf("brave-otter's events = %v, want the failure", ev.Events)
	}
	if _, e := c.fails("review", "finish", "1", "-s", "calm-heron", "--summary", "after all"); e.Code != api.CodeConflict {
		t.Errorf("finish a failed review = %v, want CONFLICT", e)
	}

	c.ok(nil, "review", "request", "-s", "brave-otter")
	if _, e := c.failsIn(twoFindings, "review", "findings", "2", "-s", "calm-heron"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "not started") {
		t.Errorf("findings before start = %v, want CONFLICT", e)
	}
	c.ok(r, "review", "cancel", "2", "-s", "brave-otter", "--reason", "merged already")
	if r.State != pb.ReviewState_REVIEW_STATE_CANCELLED || r.Summary != "merged already" {
		t.Errorf("cancelled review = %v", r)
	}
	if _, e := c.fails("review", "cancel", "2", "-s", "brave-otter"); e.Code != api.CodeConflict {
		t.Errorf("cancel twice = %v, want CONFLICT", e)
	}
	ls := &pb.ListReviewsResponse{}
	c.ok(ls, "review", "ls", "--state", "failed")
	if len(ls.Reviews) != 1 || ls.Reviews[0].Id != 1 {
		t.Errorf("ls --state failed = %v", ls.Reviews)
	}
}

// Reviews stay in their project and require a real version to review.
func TestReviewRefusals(t *testing.T) {
	c, _, v := reviewCluster(t)
	c.ok(nil, "review", "request", "-s", "brave-otter")
	c.ok(nil, "review", "start", "1", "-s", "calm-heron")

	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "far-fox", "-p", "other")
	c.ok(nil, "review", "request", "-s", "brave-otter")
	if _, e := c.fails("review", "start", "2", "-s", "far-fox"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "is project test's") {
		t.Errorf("start another project's review = %v, want CONFLICT", e)
	}
	if _, e := c.fails("review", "respond", "1", "--comment", "wrong project", "-s", "far-fox"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "is project test's") {
		t.Errorf("respond to another project's review = %v, want CONFLICT", e)
	}
	ls := &pb.ListReviewsResponse{}
	c.ok(ls, "review", "ls", "-p", "other")
	if len(ls.Reviews) != 0 {
		t.Errorf("other's reviews = %v", ls.Reviews)
	}
	c.ok(ls, "review", "ls", "--all-projects", "--limit", "1")
	if len(ls.Reviews) != 1 || ls.Reviews[0].Id != 2 {
		t.Errorf("ls --all-projects --limit 1 = %v", ls.Reviews)
	}

	if _, e := c.fails("review", "request", "-s", "brave-otter", "--whole", "--base", "HEAD"); e.Code != api.CodeInvalidArgs {
		t.Errorf("--whole --base = %v, want INVALID_ARGS", e)
	}
	if _, e := c.fails("review", "request", "-s", "brave-otter", "--base", v.head, "--head", v.head); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "no changes") {
		t.Errorf("head at base = %v, want INVALID_ARGS", e)
	}
	gitIn(t, v.dir, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	if _, e := c.fails("review", "request", "-s", "brave-otter"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "no origin/HEAD") {
		t.Errorf("no origin/HEAD = %v, want INVALID_ARGS", e)
	}
	t.Chdir(t.TempDir())
	if _, e := c.fails("review", "request", "-s", "brave-otter"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "not in one") {
		t.Errorf("request outside a checkout = %v, want INVALID_ARGS", e)
	}
}
