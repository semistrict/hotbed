package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Agents report problems with hotbed itself once, find them again, and +1
// them; the user archives one dealt with, and a session's start tells
// agents how.
func TestIssues(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)
	c.addRepo(upstream(t))
	var first pb.Session
	c.ok(&first, "session", "start", "sore-elk")
	if !strings.Contains(first.GetReminder(), "hotbed issue report") || !strings.Contains(first.GetReminder(), "hotbed issue upvote") {
		t.Fatalf("session start reminder %q, want it to say how to report and +1 issues", first.GetReminder())
	}
	c.ok(nil, "session", "start", "sly-fox")

	var reported pb.Issue
	c.ok(&reported, "issue", "report", "-s", "sore-elk", "run --wait counts time queued", "-m", "a run queued 2m was detached before it started")
	if reported.Id == 0 || reported.State != pb.IssueState_ISSUE_STATE_OPEN || reported.Votes != 1 || reported.Reported.GetSession() != "sore-elk" || reported.Project != "app" {
		t.Fatalf("report = %v", &reported)
	}
	if want := fmt.Sprintf("%s/issues/%d", c.cockpit().base, reported.Id); reported.CockpitUrl != want {
		t.Errorf("issue links to %q, want %s", reported.CockpitUrl, want)
	}
	var other pb.Issue
	c.ok(&other, "issue", "report", "-s", "sly-fox", "logs -f misses the last line")

	// Another agent finds it by its words, and +1s it once.
	var found pb.ListIssuesResponse
	c.ok(&found, "issue", "ls", "-q", "WAIT queued")
	if len(found.Issues) != 1 || found.Issues[0].Id != reported.Id {
		t.Fatalf("issue ls -q = %v, want only #%d", found.Issues, reported.Id)
	}
	var up pb.Issue
	c.ok(&up, "issue", "upvote", "-s", "sly-fox", fmt.Sprint(reported.Id), "-m", "hit it too")
	if up.Votes != 2 || len(up.Upvotes) != 1 || up.Upvotes[0].By.GetSession() != "sly-fox" || up.Upvotes[0].Comment != "hit it too" {
		t.Fatalf("upvote = %v", &up)
	}
	for _, s := range []string{"sly-fox", "sore-elk"} {
		if code, e := c.fails("issue", "upvote", "-s", s, fmt.Sprint(reported.Id)); code != exitError || e.Code != api.CodeConflict {
			t.Errorf("%s +1 again: exit %d, %+v; want CONFLICT, each counts once", s, code, e)
		}
	}

	// Most +1'd first.
	var all pb.ListIssuesResponse
	c.ok(&all, "issue", "ls")
	if len(all.Issues) != 2 || all.Issues[0].Id != reported.Id || all.Issues[1].Id != other.Id {
		t.Fatalf("issue ls = %v, want #%d then #%d", all.Issues, reported.Id, other.Id)
	}
	code, text, stderr := c.hotbed("issue", "ls")
	if want := fmt.Sprintf("#%-4d  open      +2    run --wait counts time queued\n", reported.Id); code != exitOK || !strings.HasPrefix(text, want) {
		t.Fatalf("issue ls text: exit %d, %q (stderr %q), want it to start %q", code, text, stderr, want)
	}

	// The user archives it: it leaves the open list, no one +1s it, and it
	// comes back reopened.
	var archived pb.Issue
	c.ok(&archived, "issue", "archive", fmt.Sprint(reported.Id), "-m", "fixed in 5a9531ef")
	if archived.State != pb.IssueState_ISSUE_STATE_ARCHIVED || archived.Archived.GetReason() != "fixed in 5a9531ef" {
		t.Fatalf("archive = %v", &archived)
	}
	c.ok(&all, "issue", "ls")
	if len(all.Issues) != 1 || all.Issues[0].Id != other.Id {
		t.Fatalf("open issues after archiving = %v, want #%d alone", all.Issues, other.Id)
	}
	c.ok(&all, "issue", "ls", "--archived")
	if len(all.Issues) != 1 || all.Issues[0].Id != reported.Id {
		t.Fatalf("archived issues = %v, want #%d", all.Issues, reported.Id)
	}
	if code, e := c.fails("issue", "upvote", "-s", "sly-fox", fmt.Sprint(other.Id+100)); code != exitError || e.Code != api.CodeNotFound {
		t.Errorf("+1 of no issue: exit %d, %+v; want NOT_FOUND", code, e)
	}
	c.ok(nil, "session", "start", "new-owl")
	if code, e := c.fails("issue", "upvote", "-s", "new-owl", fmt.Sprint(reported.Id)); code != exitError || e.Code != api.CodeConflict || !strings.Contains(e.Message, "fixed in 5a9531ef") {
		t.Errorf("+1 of an archived issue: exit %d, %+v; want CONFLICT saying why it was archived", code, e)
	}
	var reopened pb.Issue
	c.ok(&reopened, "issue", "reopen", fmt.Sprint(reported.Id))
	if reopened.State != pb.IssueState_ISSUE_STATE_OPEN || reopened.Archived != nil {
		t.Fatalf("reopen = %v", &reopened)
	}
	var shown pb.Issue
	c.ok(&shown, "issue", "show", fmt.Sprint(reported.Id))
	if shown.Votes != 2 || shown.Body != "a run queued 2m was detached before it started" {
		t.Fatalf("issue show = %v", &shown)
	}
	var snap pb.Snapshot
	c.ok(&snap, "snapshot")
	if snap.OpenIssues != 2 {
		t.Errorf("snapshot open issues = %d, want 2", snap.OpenIssues)
	}
}
