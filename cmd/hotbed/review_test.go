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
		code, stdout, _ := c.hotbed(append([]string{"--json"}, args...)...)
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
// responses, each step kept with the session and exact model that took it.
func TestReviewRoundTrip(t *testing.T) {
	c, user, v := reviewCluster(t)
	// Requesting a review without an implementer model (and on a session with none) is refused.
	if _, e := c.fails("review", "request", "-s", "brave-otter", "--title", "No model"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "no implementer model") {
		t.Errorf("request without model = %v, want INVALID_ARGS naming no implementer model", e)
	}
	r := &pb.Review{}
	c.ok(r, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon", "--title", "Add A", "--instructions", "look for dropped errors")
	if r.Id != 1 || r.State != pb.ReviewState_REVIEW_STATE_REQUESTED || r.Project != "test" || r.Repo != "example.invalid/o/app" ||
		r.Base != v.main || r.Head != v.head || r.Branch != "work" || r.SnapshotOf != "" ||
		r.Requested.Session != "brave-otter" || r.ImplementerModel != "gemini-4-neon" || r.Implementer.GetModel() != "gemini-4-neon" ||
		r.Implementer.GetActorKind() != pb.ActorKind_ACTOR_KIND_AGENT || r.Instructions != "look for dropped errors" {
		t.Fatalf("requested review = %v", r)
	}
	if pinned := gitIn(t, v.dir, "rev-parse", "refs/hotbed/reviews/"+v.head); pinned != v.head {
		t.Errorf("refs/hotbed/reviews/%s = %s", v.head, pinned)
	}
	if evs := c.events("brave-otter"); len(evs) != 0 {
		t.Fatalf("requesting a review emitted events to its requester: %v", evs)
	}

	// Findings before a start are refused, with a start that works.
	if _, e := c.failsIn(twoFindings, "review", "findings", "1", "-s", "calm-heron"); e.Code != api.CodeConflict || !strings.Contains(e.Suggestion, "hotbed review start 1 --model") {
		t.Errorf("findings before a start = %v, want CONFLICT suggesting a start with --model", e)
	}
	// A start names the exact model reviewing: the review keeps it (board 7788).
	for _, model := range []string{"", "  "} {
		if _, e := c.fails("review", "start", "1", "-s", "calm-heron", "--model", model); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "names no model") {
			t.Errorf("start with model %q = %v, want INVALID_ARGS naming no model", model, e)
		}
	}
	if code, _, stderr := c.hotbed("review", "start", "1", "-s", "calm-heron"); code == 0 || !strings.Contains(stderr, "--model") {
		t.Errorf("start without --model: exit %d, stderr %q; want it refused for want of --model", code, stderr)
	}
	c.ok(r, "review", "start", "1", "-s", "calm-heron", "--model", "claude-opus-5-5")
	c.ok(r, "review", "start", "1", "-s", "calm-heron", "--model", "claude-opus-5-5") // again: a no-op
	if r.State != pb.ReviewState_REVIEW_STATE_IN_PROGRESS || r.Started.Session != "calm-heron" || r.Model != "claude-opus-5-5" || len(r.Reviewers) != 1 {
		t.Fatalf("started review = %v", r)
	}
	c.ok(r, "review", "show", "1")
	if r.Model != "claude-opus-5-5" || r.ImplementerModel != "gemini-4-neon" {
		t.Errorf("review show 1: model=%q impl=%q", r.Model, r.ImplementerModel)
	}
	if code, stdout, stderr := c.hotbed("review", "ls", "-s", "brave-otter"); code != 0 || !strings.Contains(stdout, "claude-opus-5-5") || !strings.Contains(stdout, "gemini-4-neon") {
		t.Errorf("review ls: exit %d, stdout %q (stderr %q); want both implementer and reviewer models", code, stdout, stderr)
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
	if f.Id != 1 || f.ReviewerId != 1 || f.Title != "A ignores its error" || f.Severity != pb.FindingSeverity_FINDING_SEVERITY_IMPORTANT || f.Verdict != pb.FindingVerdict_FINDING_VERDICT_CONFIRMED ||
		len(f.Locations) != 2 || f.Locations[0].Path != "a.go" || f.Locations[0].StartLine != 3 || f.Locations[1].Path != "b.go" || f.Locations[1].EndLine != 12 ||
		f.Decision != pb.FindingDecision_FINDING_DECISION_OPEN || f.Found.Session != "calm-heron" || f.Found.Model != "claude-opus-5-5" {
		t.Errorf("finding 1 = %v", f)
	}
	if n := found.Findings[1]; n.Title != "The package has no doc comment." || len(n.Locations) != 0 || n.Severity != pb.FindingSeverity_FINDING_SEVERITY_NIT {
		t.Errorf("finding 2, with no location = %v", n)
	}

	// The requester polls; the reviewer finishes.
	ev := &pb.SessionEvents{}
	done := c.hotbedAsync(ev, "poll", "-s", "brave-otter")
	start := time.Now()
	c.ok(nil, "review", "finish", "1", "-s", "calm-heron", "--summary", "One dropped error.\nOne nit.")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("the poll took %v: the end did not wake it", took)
	}
	if len(ev.Events) != 1 || ev.Events[0].GetReview().GetWhat() != pb.ReviewUpdate_WHAT_FINISHED || ev.Events[0].GetReview().GetReviewId() != 1 ||
		ev.Events[0].Detail != "review 1 (Add A) is done: One dropped error." {
		t.Errorf("brave-otter's poll = %v, want the review done", ev.Events)
	}

	decided := &pb.DecideFindingsResponse{}
	c.ok(decided, "review", "accept", "1", "1", "-s", "brave-otter", "--reason", "will fix")
	if d := decided.Findings[0]; d.Decided.GetModel() != "gemini-4-neon" || d.Decided.GetActorKind() != pb.ActorKind_ACTOR_KIND_AGENT {
		t.Errorf("accepted finding stamp = %v, want implementer model gemini-4-neon + AGENT", d.Decided)
	}
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
	if _, err := user.Respond(t.Context(), &pb.RespondRequest{ReviewId: 1, FindingId: 1, Quick: pb.QuickResponse_QUICK_RESPONSE_FIX_IF_VERIFIED, Text: "and add a test", ActorKind: pb.ActorKind_ACTOR_KIND_USER}); err != nil {
		t.Fatal(err)
	}
	if _, err := user.Respond(t.Context(), &pb.RespondRequest{ReviewId: 1, Text: "thanks", ActorKind: pb.ActorKind_ACTOR_KIND_USER}); err != nil {
		t.Fatal(err)
	}
	responses := &pb.ListResponsesResponse{}
	c.ok(responses, "review", "responses", "1")
	if len(responses.Responses) != 2 || responses.Responses[0].Quick != pb.QuickResponse_QUICK_RESPONSE_FIX_IF_VERIFIED ||
		responses.Responses[0].FindingId != 1 || responses.Responses[0].By.Session != "" ||
		responses.Responses[0].By.ActorKind != pb.ActorKind_ACTOR_KIND_USER || responses.Responses[1].Text != "thanks" {
		t.Fatalf("responses = %v", responses.Responses)
	}
	c.ok(responses, "review", "responses", "1")
	if len(responses.Responses) != 0 {
		t.Errorf("responses again = %v, want none new", responses.Responses)
	}
	c.ok(ev, "poll", "-s", "brave-otter", "--timeout", "5")
	if !slices.ContainsFunc(ev.Events, func(e *pb.SessionEvent) bool {
		return e.GetReview().GetWhat() == pb.ReviewUpdate_WHAT_RESPONDED && e.Detail == `the user on finding 1 (A ignores its error) of review 1: fix if verified; "and add a test"`
	}) {
		t.Errorf("brave-otter's events = %v, want the user's response", ev.Events)
	}

	// The requester replies; its own reply is no news to its poll.
	c.ok(nil, "review", "respond", "1", "--finding", "1", "--comment", "verified and fixed", "-s", "brave-otter")
	c.ok(ev, "poll", "-s", "brave-otter", "--timeout", "1")
	if len(ev.Events) != 0 {
		t.Errorf("poll after the session's own reply = %v, want nothing", ev.Events)
	}

	r = c.review(1)
	if got := r.Findings[0]; got.Quick != pb.QuickResponse_QUICK_RESPONSE_FIX_IF_VERIFIED ||
		got.QuickBy.GetActorKind() != pb.ActorKind_ACTOR_KIND_USER ||
		got.Responses != 2 || got.Decision != pb.FindingDecision_FINDING_DECISION_ACCEPTED {
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
	c.ok(r, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon")
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

	c.ok(r, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon", "--whole", "--head", "HEAD")
	if r.Base != "" || r.Head != v.head || r.SnapshotOf != "" {
		t.Errorf("--whole --head HEAD = %v", r)
	}
}

// A review fails, or is cancelled, with why; its requester hears of a
// failure; ends are final.
func TestReviewEnds(t *testing.T) {
	c, _, _ := reviewCluster(t)
	c.ok(nil, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon")
	c.ok(nil, "review", "start", "1", "-s", "calm-heron", "--model", "claude-opus-5-5")
	if _, e := c.fails("review", "fail", "1", "-s", "calm-heron", "--reason", " "); e.Code != api.CodeInvalidArgs {
		t.Errorf("fail without a reason = %v, want INVALID_ARGS", e)
	}
	r := &pb.Review{}
	c.ok(r, "review", "fail", "1", "-s", "calm-heron", "--reason", "head does not build")
	if r.State != pb.ReviewState_REVIEW_STATE_FAILED || r.Summary != "head does not build" || r.Ended.Session != "calm-heron" {
		t.Errorf("failed review = %v", r)
	}
	ev := &pb.SessionEvents{}
	c.ok(ev, "poll", "-s", "brave-otter", "--timeout", "5")
	if !slices.ContainsFunc(ev.Events, func(e *pb.SessionEvent) bool {
		return e.GetReview().GetWhat() == pb.ReviewUpdate_WHAT_FAILED && e.GetReview().GetReviewId() == 1
	}) {
		t.Errorf("brave-otter's events = %v, want the failure", ev.Events)
	}
	if _, e := c.fails("review", "finish", "1", "-s", "calm-heron", "--summary", "after all"); e.Code != api.CodeConflict {
		t.Errorf("finish a failed review = %v, want CONFLICT", e)
	}

	c.ok(nil, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon")
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
	if _, e := c.fails("review", "start", "2", "-s", "calm-heron", "--model", "claude-opus-5-5"); e.Code != api.CodeConflict {
		t.Errorf("start a cancelled review = %v, want CONFLICT", e)
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
	c.ok(nil, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon")
	c.ok(nil, "review", "start", "1", "-s", "calm-heron", "--model", "claude-opus-5-5")

	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "far-fox", "-p", "other")
	c.ok(nil, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon")
	if _, e := c.fails("review", "start", "2", "-s", "far-fox", "--model", "claude-opus-5-5"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "is project test's") {
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

	if _, e := c.fails("review", "request", "-s", "brave-otter", "--model", "gemini-4-neon", "--whole", "--base", "HEAD"); e.Code != api.CodeInvalidArgs {
		t.Errorf("--whole --base = %v, want INVALID_ARGS", e)
	}
	if _, e := c.fails("review", "request", "-s", "brave-otter", "--model", "gemini-4-neon", "--base", v.head, "--head", v.head); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "no changes") {
		t.Errorf("head at base = %v, want INVALID_ARGS", e)
	}
	gitIn(t, v.dir, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	if _, e := c.fails("review", "request", "-s", "brave-otter", "--model", "gemini-4-neon"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "no origin/HEAD") {
		t.Errorf("no origin/HEAD = %v, want INVALID_ARGS", e)
	}
	t.Chdir(t.TempDir())
	if _, e := c.fails("review", "request", "-s", "brave-otter", "--model", "gemini-4-neon"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "not in one") {
		t.Errorf("request outside a checkout = %v, want INVALID_ARGS", e)
	}
}

// A reviewer that found nothing says so: no findings, and an event that
// says it reported none.
func TestReviewFindingsNoneFound(t *testing.T) {
	c, _, _ := reviewCluster(t)
	c.ok(nil, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon")
	c.ok(nil, "review", "start", "1", "-s", "calm-heron", "--model", "claude-opus-5-5")
	found := &pb.AddFindingsResponse{}
	c.okIn(`{"findings": []}`, found, "review", "findings", "1", "-s", "calm-heron")
	if len(found.Findings) != 0 {
		t.Errorf("findings of an empty report = %v, want none", found.Findings)
	}
	// No JSON at all is still no report.
	if _, e := c.failsIn(" \n", "review", "findings", "1", "-s", "calm-heron"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "no JSON") {
		t.Errorf("findings with no JSON = %v, want INVALID_ARGS", e)
	}
	c.ok(nil, "review", "finish", "1", "-s", "calm-heron", "--summary", "nothing survived")
	r := c.review(1)
	if len(r.Findings) != 0 || r.Report != "" {
		t.Errorf("review = %v, want no findings and no report", r)
	}
	want := []string{"requested by brave-otter", "started by calm-heron", "found none by calm-heron", "finished by calm-heron"}
	if got := reviewWhats(r); !slices.Equal(got, want) {
		t.Errorf("events = %v\nwant %v", got, want)
	}
}

// reviewerReport is what a reviewer in a network-isolated sandbox writes:
// prose that quotes the convention, then its findings block, last.
const reviewerReport = "# Review of work\n\nA drops an error; the package lacks a doc comment.\n\n" +
	"The instructions' example, quoted:\n\n````markdown\n```json hotbed-findings\n{\"findings\": []}\n```\n````\n\n" +
	"```json hotbed-findings\n" + twoFindings + "\n```\n"

// A reviewer that cannot reach hotbed ends its report with its findings in
// a json hotbed-findings block; its caller records them, and the review
// keeps the whole report.
func TestReviewFindingsFromReport(t *testing.T) {
	c, _, v := reviewCluster(t)
	c.ok(nil, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon")
	c.ok(nil, "review", "start", "1", "-s", "calm-heron", "--model", "claude-opus-5-5")
	file := func(name, text string) string {
		p := filepath.Join(t.TempDir(), name)
		write(t, p, text)
		return p
	}

	refusals := map[string]struct{ report, says string }{
		"no block":                 {"# Review\n\nno findings\n", "no ```json hotbed-findings block"},
		"unclosed block":           {"# Review\n\n```json hotbed-findings\n{\"findings\": []}\n", "never closed"},
		"not findings":             {"```json hotbed-findings\n[1, 2]\n```\n", `is not {"findings": [...]}`},
		"bad finding":              {"```json hotbed-findings\n{\"findings\": [{\"summary\": \"x\", \"severity\": \"huge\"}]}\n```\n", `severity "huge"`},
		"only a quoted block":      {"# Review\n\n````markdown\n```json hotbed-findings\n{\"findings\": []}\n```\n````\n", "no ```json hotbed-findings block"},
		"a quoted block in tildes": {"~~~\n```json hotbed-findings\n{\"findings\": []}\n```\n~~~\n", "no ```json hotbed-findings block"},
		"only blanks":              {" \n\n", "is empty"},
	}
	for name, tc := range refusals {
		if _, e := c.fails("review", "findings", "1", "-s", "calm-heron", "--from-report", file("report.md", tc.report)); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, tc.says) {
			t.Errorf("report with %s = %v, want INVALID_ARGS saying %q", name, e, tc.says)
		}
	}
	report := file("report.md", reviewerReport+"\n## Appendix: the convention\n\n````\n```json hotbed-findings\n{\"findings\": []}\n```\n````\n")
	if _, e := c.fails("review", "findings", "1", "-s", "calm-heron", "--from-report", report, "--file", file("f.json", twoFindings)); e.Code != api.CodeInvalidArgs {
		t.Errorf("--from-report with --file = %v, want INVALID_ARGS", e)
	}
	if r := c.review(1); len(r.Findings) != 0 || r.Report != "" {
		t.Fatalf("refused reports changed the review: %v", r)
	}

	found := &pb.AddFindingsResponse{}
	c.ok(found, "review", "findings", "1", "-s", "calm-heron", "--from-report", report)
	if len(found.Findings) != 2 || found.Findings[0].Title != "A ignores its error" || len(found.Findings[0].Locations) != 2 || found.Findings[1].Severity != pb.FindingSeverity_FINDING_SEVERITY_NIT {
		t.Fatalf("findings of the report = %v, want its last block's two", found.Findings)
	}
	r := c.review(1)
	if !strings.HasPrefix(r.Report, strings.TrimSpace(reviewerReport)) || !strings.HasSuffix(r.Report, "````") {
		t.Errorf("kept report = %q, want the whole report", r.Report)
	}
	want := []string{"requested by brave-otter", "started by calm-heron", "reported by calm-heron", "found 1 by calm-heron", "found 2 by calm-heron"}
	if got := reviewWhats(r); !slices.Equal(got, want) {
		t.Errorf("events = %v\nwant %v", got, want)
	}
	ls := &pb.ListReviewsResponse{}
	c.ok(ls, "review", "ls")
	if len(ls.Reviews) != 1 || ls.Reviews[0].Report != "" || ls.Reviews[0].Important != 1 {
		t.Errorf("review ls = %v, want the review without its report", ls.Reviews)
	}

	// A report of nothing found: kept, and none found.
	c.ok(nil, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon", "--base", v.main)
	c.ok(nil, "review", "start", "2", "-s", "calm-heron", "--model", "claude-opus-5-5")
	none := "No findings survived.\n\n```json hotbed-findings\n{\"findings\": []}\n```\n"
	c.ok(found, "review", "findings", "2", "-s", "calm-heron", "--from-report", file("none.md", none))
	if r := c.review(2); len(r.Findings) != 0 || r.Report != strings.TrimSpace(none) ||
		!slices.Equal(reviewWhats(r), []string{"requested by brave-otter", "started by calm-heron", "reported by calm-heron", "found none by calm-heron"}) {
		t.Errorf("review of an empty report = %v (events %v)", r, reviewWhats(r))
	}
}

// A finding fixed names the commit that fixed it, in full, and is handled:
// it takes no more responses until it is reopened. One invalid carries its
// evidence, and may still be disputed.
func TestReviewFindingsFixedAndInvalid(t *testing.T) {
	c, user, v := reviewCluster(t)
	c.ok(nil, "review", "request", "-s", "brave-otter", "--model", "gemini-4-neon")
	c.ok(nil, "review", "start", "1", "-s", "calm-heron", "--model", "claude-opus-5-5")
	c.okIn(twoFindings, nil, "review", "findings", "1", "-s", "calm-heron")
	c.ok(nil, "review", "accept", "1", "1", "-s", "brave-otter", "--reason", "fixing")

	decided := &pb.DecideFindingsResponse{}
	c.ok(decided, "review", "fixed", "1", "1", "-s", "brave-otter", "--commit", "work",
		"--where", "a.go:3-5", "--where", "a_test.go", "--notes", "Returns B's error; `TestA` covers it.")
	f := decided.Findings[0]
	want := &pb.Fix{Commit: v.head, Notes: "Returns B's error; `TestA` covers it.", Locations: []*pb.Location{{Path: "a.go", StartLine: 3, EndLine: 5}, {Path: "a_test.go"}}}
	if f.Decision != pb.FindingDecision_FINDING_DECISION_FIXED || !proto.Equal(f.Fix, want) || f.Decided.Session != "brave-otter" {
		t.Fatalf("fixed finding = %v, want fixed in %s", f, v.head)
	}

	// Handled: no more responses to it, from the user or an agent, and no
	// other decision but reopening.
	_, err := user.Respond(t.Context(), &pb.RespondRequest{ReviewId: 1, FindingId: 1, Quick: pb.QuickResponse_QUICK_RESPONSE_VERIFY})
	if api.CodeOf(err) != api.CodeConflict || !strings.Contains(err.Error(), "takes no more responses") {
		t.Errorf("the user's response to a fixed finding = %v, want CONFLICT", err)
	}
	if _, e := c.fails("review", "respond", "1", "--finding", "1", "--comment", "hm", "-s", "brave-otter"); e.Code != api.CodeConflict {
		t.Errorf("an agent's response to a fixed finding = %v, want CONFLICT", e)
	}
	if _, e := c.fails("review", "dismiss", "1", "1", "-s", "brave-otter", "--reason", "x"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "is fixed") {
		t.Errorf("dismiss a fixed finding = %v, want CONFLICT", e)
	}
	c.ok(nil, "review", "respond", "1", "--comment", "the review as a whole still takes them", "-s", "brave-otter")

	// The commit in full; a location a real one.
	for name, args := range map[string][]string{
		"no such commit":   {"--commit", "nosuch"},
		"a line backwards": {"--where", "a.go:5-3"},
		"no path":          {"--where", ":3"},
	} {
		if _, e := c.fails(append([]string{"review", "fixed", "1", "2", "-s", "brave-otter"}, args...)...); e.Code != api.CodeInvalidArgs {
			t.Errorf("fixed with %s = %v, want INVALID_ARGS", name, e)
		}
	}
	if _, err := user.DecideFindings(t.Context(), &pb.DecideFindingsRequest{ReviewId: 1, FindingIds: []int32{2}, Decision: pb.FindingDecision_FINDING_DECISION_FIXED, Fix: &pb.Fix{Commit: v.head[:8]}, SessionId: "brave-otter"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("fixed in an abbreviated commit = %v, want INVALID_ARGS", err)
	}

	// Invalid needs its evidence, and still takes a dispute.
	if _, e := c.fails("review", "invalid", "1", "2", "-s", "brave-otter"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "evidence") {
		t.Errorf("invalid without evidence = %v, want INVALID_ARGS", e)
	}
	evidence := "The package comment is in `doc.go` at " + v.head[:8] + "."
	c.ok(decided, "review", "invalid", "1", "2", "-s", "brave-otter", "--evidence", evidence)
	if g := decided.Findings[0]; g.Decision != pb.FindingDecision_FINDING_DECISION_INVALID || g.Evidence != evidence || g.Fix != nil {
		t.Errorf("invalid finding = %v", g)
	}
	if _, e := c.fails("review", "fixed", "1", "2", "-s", "brave-otter"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "reopen it") {
		t.Errorf("fixed after invalid = %v, want CONFLICT", e)
	}
	if _, err := user.Respond(t.Context(), &pb.RespondRequest{ReviewId: 1, FindingId: 2, Text: "doc.go is empty"}); err != nil {
		t.Errorf("dispute an invalid finding: %v", err)
	}

	// Reopened, a fixed finding takes responses again, its fix gone.
	c.ok(decided, "review", "reopen", "1", "1", "-s", "brave-otter")
	if g := decided.Findings[0]; g.Decision != pb.FindingDecision_FINDING_DECISION_OPEN || g.Fix != nil {
		t.Errorf("reopened finding = %v", g)
	}
	if _, err := user.Respond(t.Context(), &pb.RespondRequest{ReviewId: 1, FindingId: 1, Text: "still broken"}); err != nil {
		t.Errorf("respond to a reopened finding: %v", err)
	}
	r := c.review(1)
	wantEvents := []string{
		"requested by brave-otter", "started by calm-heron", "found 1 by calm-heron", "found 2 by calm-heron", "accepted 1 by brave-otter",
		"fixed 1 by brave-otter", "responded by brave-otter", "invalid 2 by brave-otter", "responded 2 by user", "reopened 1 by brave-otter", "responded 1 by user",
	}
	if got := reviewWhats(r); !slices.Equal(got, wantEvents) {
		t.Errorf("events = %v\nwant %v", got, wantEvents)
	}
}

// Multiple reviewers (including sub-agents sharing one parent session and
// inheriting the same T3CODE_THREAD_ID) can review the same review with
// distinct models; each finding keeps its reviewer's exact model, and
// implementer decisions (including won't fix, reject alias) and user vs agent
// quick responses are attributed unambiguously.
func TestReviewMultiReviewerSubagentsAndAttribution(t *testing.T) {
	c, user, _ := reviewCluster(t)
	t.Setenv("T3CODE_THREAD_ID", "thread-parent-123")

	// Start a session with a model and T3 thread ID; review request inherits its model.
	sess := &pb.Session{}
	c.ok(sess, "session", "start", "impl-owl", "-p", "test", "--model", "gemini-4-neon")
	if sess.Model != "gemini-4-neon" || sess.T3CodeThreadId != "thread-parent-123" {
		t.Fatalf("started session = %v", sess)
	}
	search := &pb.SearchSessionsResponse{}
	c.ok(search, "session", "search", "thread-parent-123")
	if len(search.Sessions) != 1 || search.Sessions[0].Model != "gemini-4-neon" {
		t.Fatalf("session search = %v", search.Sessions)
	}

	// Request inherits implementer model from the bound session (impl-owl).
	r := &pb.Review{}
	c.ok(r, "review", "request", "--title", "Multi-reviewer test")
	if r.ImplementerModel != "gemini-4-neon" || r.Implementer.GetSession() != "impl-owl" ||
		r.Implementer.GetT3CodeThreadId() != "thread-parent-123" {
		t.Fatalf("requested review = %v", r)
	}

	// Two sub-agents inside the SAME parent session (impl-owl) start reviewing with different models.
	c.ok(r, "review", "start", "1", "--model", "claude-opus-4-6", "--subagent", "claude-adv")
	c.ok(r, "review", "start", "1", "--model", "gpt-5.5-codex", "--subagent", "codex-adv")
	if len(r.Reviewers) != 2 || r.Model != "claude-opus-4-6, gpt-5.5-codex" {
		t.Fatalf("reviewers = %v, model=%q", r.Reviewers, r.Model)
	}

	// Ambiguous findings call in the shared session without --reviewer/--model/--subagent fails hard.
	if _, e := c.failsIn(`{"findings": [{"summary": "ambiguous", "severity": "nit"}]}`, "review", "findings", "1"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "2 active reviewers") {
		t.Errorf("ambiguous findings = %v, want CONFLICT naming 2 active reviewers", e)
	}

	// Each sub-agent submits findings attributed to its exact reviewer and model.
	f1 := &pb.AddFindingsResponse{}
	c.okIn(`{"findings": [{"summary": "Race on lock", "severity": "important", "verdict": "CONFIRMED", "file": "a.go", "line": 3}]}`, f1,
		"review", "findings", "1", "--reviewer", "1")
	if f1.Findings[0].ReviewerId != 1 || f1.Findings[0].Found.GetModel() != "claude-opus-4-6" || f1.Findings[0].Found.GetSubagent() != "claude-adv" {
		t.Fatalf("finding from reviewer 1 = %v", f1.Findings[0])
	}
	f2 := &pb.AddFindingsResponse{}
	c.okIn(`{"findings": [{"summary": "Unbounded buffer", "severity": "pre_existing", "file": "a.go", "line": 4}]}`, f2,
		"review", "findings", "1", "--model", "gpt-5.5-codex")
	if f2.Findings[0].ReviewerId != 2 || f2.Findings[0].Found.GetModel() != "gpt-5.5-codex" || f2.Findings[0].Found.GetSubagent() != "codex-adv" {
		t.Fatalf("finding from reviewer 2 = %v", f2.Findings[0])
	}

	// Reviewer 1 finishes while reviewer 2 is still running: overall review stays IN_PROGRESS.
	c.ok(r, "review", "finish", "1", "--reviewer", "1", "--summary", "Found one race")
	if r.State != pb.ReviewState_REVIEW_STATE_IN_PROGRESS || r.Reviewers[0].State != pb.ReviewerState_REVIEWER_STATE_DONE || r.Reviewers[1].State != pb.ReviewerState_REVIEWER_STATE_IN_PROGRESS {
		t.Fatalf("after reviewer 1 finish = %v", r)
	}

	// Reviewer 2 finishes (only remaining active reviewer in this session, so no selector needed): review becomes DONE.
	c.ok(r, "review", "finish", "1", "--summary", "Found one pre-existing issue")
	if r.State != pb.ReviewState_REVIEW_STATE_DONE || r.Reviewers[1].State != pb.ReviewerState_REVIEWER_STATE_DONE ||
		r.Reviewers[0].FindingsCount != 1 || r.Reviewers[1].FindingsCount != 1 {
		t.Fatalf("after reviewer 2 finish = %v", r)
	}

	// A third reviewer in a separate session can still join a DONE review and finish.
	c.ok(r, "review", "start", "1", "-s", "calm-heron", "--model", "o4-mini")
	if r.State != pb.ReviewState_REVIEW_STATE_IN_PROGRESS || len(r.Reviewers) != 3 {
		t.Fatalf("after 3rd reviewer start = %v", r)
	}
	c.ok(r, "review", "finish", "1", "-s", "calm-heron", "--summary", "Looks clean")
	if r.State != pb.ReviewState_REVIEW_STATE_DONE || r.Model != "claude-opus-4-6, gpt-5.5-codex, o4-mini" {
		t.Fatalf("after 3rd reviewer finish = %v", r)
	}

	// Implementer marks finding 1 won't fix and finding 2 rejected (alias for dismiss).
	dec := &pb.DecideFindingsResponse{}
	c.ok(dec, "review", "wont-fix", "1", "1", "--reason", "accepted tradeoff")
	if d := dec.Findings[0]; d.Decision != pb.FindingDecision_FINDING_DECISION_WONT_FIX ||
		d.Decided.GetActorKind() != pb.ActorKind_ACTOR_KIND_AGENT ||
		d.Decided.GetModel() != "gemini-4-neon" || d.Decided.GetReason() != "accepted tradeoff" {
		t.Fatalf("wont-fix finding = %v", d)
	}
	c.ok(dec, "review", "reject", "1", "2", "--reason", "out of scope")
	if d := dec.Findings[0]; d.Decision != pb.FindingDecision_FINDING_DECISION_DISMISSED ||
		d.Decided.GetActorKind() != pb.ActorKind_ACTOR_KIND_AGENT || d.Decided.GetModel() != "gemini-4-neon" {
		t.Fatalf("rejected finding = %v", d)
	}

	// Agent calling `review respond` WITHOUT -s uses its bound session (ACTOR_KIND_AGENT), NEVER user!
	c.ok(nil, "review", "respond", "1", "--finding", "1", "--quick", "wont-fix", "--comment", "agent proposing wont-fix")
	r = c.review(1)
	if r.Findings[0].Quick != pb.QuickResponse_QUICK_RESPONSE_WONT_FIX ||
		r.Findings[0].QuickBy.GetActorKind() != pb.ActorKind_ACTOR_KIND_AGENT ||
		r.Findings[0].QuickBy.GetSession() != "impl-owl" ||
		r.Findings[0].QuickBy.GetModel() != "gemini-4-neon" {
		t.Fatalf("agent quick response attribution = %v", r.Findings[0].QuickBy)
	}

	// User clicking "Verify" in the cockpit (or CLI --as-user) clearly overrides quick_by with ACTOR_KIND_USER.
	if _, err := user.Respond(t.Context(), &pb.RespondRequest{ReviewId: 1, FindingId: 1, Quick: pb.QuickResponse_QUICK_RESPONSE_VERIFY, Text: "please double check", ActorKind: pb.ActorKind_ACTOR_KIND_USER}); err != nil {
		t.Fatal(err)
	}
	r = c.review(1)
	if r.Findings[0].Quick != pb.QuickResponse_QUICK_RESPONSE_VERIFY ||
		r.Findings[0].QuickBy.GetActorKind() != pb.ActorKind_ACTOR_KIND_USER ||
		r.Findings[0].QuickBy.GetSession() != "" {
		t.Fatalf("user quick response attribution = %v", r.Findings[0].QuickBy)
	}

	// CLI --as-user also stamps ACTOR_KIND_USER even when a session is bound.
	c.ok(nil, "review", "respond", "1", "--finding", "2", "--quick", "wont-fix", "--as-user")
	r = c.review(1)
	if r.Findings[1].Quick != pb.QuickResponse_QUICK_RESPONSE_WONT_FIX ||
		r.Findings[1].QuickBy.GetActorKind() != pb.ActorKind_ACTOR_KIND_USER ||
		r.Findings[1].QuickBy.GetSession() != "" {
		t.Fatalf("--as-user quick response attribution = %v", r.Findings[1].QuickBy)
	}
}
