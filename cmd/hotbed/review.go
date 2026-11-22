package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// Reviews of an exact version by reviewer agents. The agent that wants one
// requests it and hands its number to a reviewer agent, which starts it,
// reports findings and finishes it. The requester reads the findings,
// accepts or dismisses each, and sends the user the review's link; the
// user's responses come back through responses and wait --responses.

type reviewCmd struct {
	Request   reviewRequestCmd   `cmd:"" help:"Ask for a review of this checkout's exact version: from --base (default: where it left the remote's default branch) to --head (default: HEAD, or a snapshot of uncommitted changes). Prints the review; give its number to a reviewer agent."`
	Ls        reviewLsCmd        `cmd:"" help:"A project's reviews (default: this session's project), newest first, with their findings counted."`
	Show      reviewShowCmd      `cmd:"" help:"A review: its version, findings (with locations, decisions and responses) and every event."`
	Link      reviewLinkCmd      `cmd:"" help:"The review's page in the cockpit, to send the user."`
	Start     reviewStartCmd     `cmd:"" help:"Reviewer: claim a requested review for this session; review show has what to review."`
	Findings  reviewFindingsCmd  `cmd:"" help:"Reviewer: report findings, as ReportFindings takes them ({\"findings\": [...]}, each with file, line, summary, failure_scenario, category, severity, short_summary, verdict), with \"locations\": [{path, start_line, end_line, excerpt}] besides. JSON on stdin or --file; call it as often as you like."`
	Finish    reviewFinishCmd    `cmd:"" help:"Reviewer: finish the review, with a summary."`
	Fail      reviewFailCmd      `cmd:"" help:"Reviewer: end the review as failed, saying why."`
	Cancel    reviewCancelCmd    `cmd:"" help:"Withdraw a review that has not ended."`
	Accept    reviewAcceptCmd    `cmd:"" help:"Accept findings: you will act on them."`
	Dismiss   reviewDismissCmd   `cmd:"" help:"Dismiss findings: you will not act on them, saying why."`
	Reopen    reviewReopenCmd    `cmd:"" help:"Make accepted or dismissed findings undecided again."`
	Respond   reviewRespondCmd   `cmd:"" help:"Respond to a finding, or the review as a whole: a quick response, a comment, or both. The user responds in the cockpit; use this to reply, or to pass on what the user said."`
	Responses reviewResponsesCmd `cmd:"" help:"The responses to a review since you last asked (or --after N; --all for every one), the user's above all."`
	Wait      reviewWaitCmd      `cmd:"" help:"Wait (up to two minutes) for the review to end; with --responses, for a new response from anyone but this session."`
}

// reviewID is the argument of the commands that act on one review.
type reviewID struct {
	ID int32 `arg:"" help:"The review's number."`
}

type reviewRequestCmd struct {
	controllerClient
	sessionFlag
	Title            string `help:"What it reviews, for people (default: the branch and commit)."`
	Instructions     string `help:"What the reviewer should look at, or for."`
	InstructionsFile string `type:"existingfile" help:"Read the instructions from this file."`
	Base             string `help:"The commit the changes are from (default: where HEAD left the remote's default branch)."`
	Head             string `help:"The commit to review (default: HEAD, or, with uncommitted changes, a snapshot of them)."`
	Whole            bool   `help:"Review the whole tree at head, not its changes from a base."`
}

func (c *reviewRequestCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	instructions := c.Instructions
	if c.InstructionsFile != "" {
		if instructions != "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "give the instructions with --instructions or --instructions-file, not both")
		}
		b, err := os.ReadFile(c.InstructionsFile)
		if err != nil {
			return nil, err
		}
		instructions = string(b)
	}
	v, err := versionToReview(ctx, c.Base, c.Head, c.Whole)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.RequestReview(ctx, &pb.RequestReviewRequest{
		SessionId: session, Title: c.Title, Instructions: instructions,
		Repo: v.remote, Base: v.base, Head: v.head, Branch: v.branch, SnapshotOf: v.snapshotOf,
	})
}

// reviewedVersion is what a review is of, in this checkout.
type reviewedVersion struct {
	remote, branch, base, head, snapshotOf string
}

// versionToReview finds the exact version to review in this checkout: head
// (given, or HEAD, or a snapshot commit of uncommitted changes on HEAD),
// pinned under refs/hotbed/reviews so git keeps it, and the base its
// changes are from.
func versionToReview(ctx context.Context, base, head string, whole bool) (reviewedVersion, error) {
	var v reviewedVersion
	root, err := gitCheckoutRoot(ctx, ".")
	if err != nil {
		return v, api.Errorf(api.CodeInvalidArgs, "a review is of a version in a git checkout; this is not in one").
			WithSuggestion("run hotbed review request in the checkout to review")
	}
	g := gitops.Git{Dir: root}
	commit := func(ref string) (string, error) {
		c, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err != nil || c == "" {
			return "", api.Errorf(api.CodeInvalidArgs, "%s is not a commit here", ref)
		}
		return c, nil
	}
	if remotes := gitRemoteURLs(ctx, root); len(remotes) > 0 {
		v.remote = remotes[0]
	}
	v.branch, _ = g.Run(ctx, "branch", "--show-current") // empty when detached
	switch {
	case head != "":
		if v.head, err = commit(head); err != nil {
			return v, err
		}
	default:
		if v.head, err = commit("HEAD"); err != nil {
			return v, err
		}
		snap, err := gitops.Snapshot(ctx, root)
		if err != nil {
			return v, err
		}
		if tree, err := g.Run(ctx, "rev-parse", "HEAD^{tree}"); err != nil {
			return v, err
		} else if snap.Tree != tree {
			v.snapshotOf, v.head = v.head, snap.Commit
		}
	}
	switch {
	case whole && base != "":
		return v, api.Errorf(api.CodeInvalidArgs, "--whole reviews the tree, not changes from a base: give one or the other")
	case whole:
	case base != "":
		if v.base, err = commit(base); err != nil {
			return v, err
		}
	default:
		upstream, err := commit("refs/remotes/origin/HEAD")
		if err != nil {
			return v, api.Errorf(api.CodeInvalidArgs, "no origin/HEAD to find where this work started").
				WithSuggestion("pass --base REF, or --whole; git remote set-head origin --auto records origin/HEAD")
		}
		if v.base, err = g.Run(ctx, "merge-base", v.head, upstream); err != nil {
			return v, err
		}
	}
	if v.base == v.head {
		return v, api.Errorf(api.CodeInvalidArgs, "head %s is base: there are no changes to review", v.head[:8]).
			WithSuggestion("pass --base REF, or --whole for the tree as it is")
	}
	// A snapshot is a commit no branch holds: pin it so gc keeps it while
	// a reviewer may look.
	if _, err := g.Run(ctx, "update-ref", "refs/hotbed/reviews/"+v.head, v.head); err != nil {
		return v, err
	}
	return v, nil
}

type reviewLsCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	AllProjects bool     `help:"Every project's reviews."`
	State       []string `enum:"requested,in-progress,done,failed,cancelled" help:"Only reviews in this state: requested, in-progress, done, failed or cancelled. Repeatable (default: all)."`
	Limit       int32    `help:"At most this many (default: all)."`
}

func (c *reviewLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	project, err := c.listed(ctx, d, cl, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	var states []pb.ReviewState
	for _, w := range c.State {
		states = append(states, pb.ReviewState(pb.ReviewState_value["REVIEW_STATE_"+strings.ToUpper(strings.ReplaceAll(w, "-", "_"))]))
	}
	return cl.reviews.ListReviews(ctx, &pb.ListReviewsRequest{Project: project, States: states, Limit: c.Limit})
}

type reviewShowCmd struct {
	controllerClient
	reviewID
}

func (c *reviewShowCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.GetReview(ctx, &pb.GetReviewRequest{Id: c.ID})
}

type reviewLinkCmd struct {
	controllerClient
	reviewID
}

type linkResult struct {
	URL string `json:"url"`
}

func (c *reviewLinkCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	// The review must be there for the link to show anything.
	if _, err := cl.reviews.GetReview(ctx, &pb.GetReviewRequest{Id: c.ID}); err != nil {
		return nil, err
	}
	hostPort, err := api.ControllerHostPort(c.Controller)
	if err != nil {
		return nil, err
	}
	return linkResult{URL: fmt.Sprintf("http://%s/reviews/%d", hostPort, c.ID)}, nil
}

type reviewStartCmd struct {
	controllerClient
	sessionFlag
	reviewID
}

func (c *reviewStartCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.StartReview(ctx, &pb.StartReviewRequest{Id: c.ID, SessionId: session})
}

const findingsExample = `hotbed review findings 12 <<'EOF'
{"findings": [{"short_summary": "Lost update on concurrent edits", "summary": "...", "failure_scenario": "...",
  "category": "correctness", "severity": "important", "verdict": "CONFIRMED", "file": "store.go", "line": 120,
  "locations": [{"path": "server.go", "start_line": 40, "end_line": 44, "excerpt": "..."}]}]}
EOF`

type reviewFindingsCmd struct {
	controllerClient
	sessionFlag
	reviewID
	questionInput
}

func (c *reviewFindingsCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	b, err := c.read(d, findingsExample)
	if err != nil {
		return nil, err
	}
	req := &pb.AddFindingsRequest{}
	if err := protojson.Unmarshal(b, req); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "read the findings' JSON: %v", err).WithSuggestion(findingsExample)
	}
	if req.ReviewId != 0 || req.SessionId != "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "the JSON names its review or session; hotbed fills those in").WithSuggestion(findingsExample)
	}
	req.ReviewId, req.SessionId = c.ID, session
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.AddFindings(ctx, req)
}

type reviewFinishCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Summary string `required:"" help:"What you found, in a few lines."`
}

func (c *reviewFinishCmd) Exec(ctx context.Context, d deps) (any, error) {
	return finishReview(ctx, d, c.controllerClient, c.sessionFlag, c.ID, c.Summary, false)
}

type reviewFailCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Reason string `required:"" help:"Why the review could not be done."`
}

func (c *reviewFailCmd) Exec(ctx context.Context, d deps) (any, error) {
	return finishReview(ctx, d, c.controllerClient, c.sessionFlag, c.ID, c.Reason, true)
}

func finishReview(ctx context.Context, d deps, cc controllerClient, sf sessionFlag, id int32, summary string, failed bool) (any, error) {
	session, err := sf.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := cc.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.FinishReview(ctx, &pb.FinishReviewRequest{Id: id, SessionId: session, Summary: summary, Failed: failed})
}

type reviewCancelCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Reason string `help:"Why it is no longer wanted."`
}

func (c *reviewCancelCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.CancelReview(ctx, &pb.CancelReviewRequest{Id: c.ID, SessionId: session, Reason: c.Reason})
}

// decideCmd is what accept, dismiss and reopen take.
type decideCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Findings []int32 `arg:"" name:"finding" help:"The findings' numbers."`
}

func (c decideCmd) decide(ctx context.Context, d deps, decision pb.FindingDecision, reason string) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.DecideFindings(ctx, &pb.DecideFindingsRequest{ReviewId: c.ID, FindingIds: c.Findings, Decision: decision, Reason: reason, SessionId: session})
}

type reviewAcceptCmd struct {
	decideCmd
	Reason string `help:"What you will do."`
}

func (c *reviewAcceptCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.decide(ctx, d, pb.FindingDecision_FINDING_DECISION_ACCEPTED, c.Reason)
}

type reviewDismissCmd struct {
	decideCmd
	Reason string `required:"" help:"Why you will not act on them."`
}

func (c *reviewDismissCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.decide(ctx, d, pb.FindingDecision_FINDING_DECISION_DISMISSED, c.Reason)
}

type reviewReopenCmd struct {
	decideCmd
}

func (c *reviewReopenCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.decide(ctx, d, pb.FindingDecision_FINDING_DECISION_OPEN, "")
}

type reviewRespondCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Finding int32  `help:"The finding it answers (default: the review as a whole)."`
	Quick   string `enum:",verify,fix-if-verified,wont-fix" default:"" help:"A quick response to the finding: verify, fix-if-verified or wont-fix."`
	Comment string `help:"What you say."`
}

func (c *reviewRespondCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	quick := pb.QuickResponse(pb.QuickResponse_value["QUICK_RESPONSE_"+strings.ToUpper(strings.ReplaceAll(c.Quick, "-", "_"))])
	// The session named, if any: a response passed on from the user is
	// the user's (see sessionFlag.named).
	return cl.reviews.Respond(ctx, &pb.RespondRequest{ReviewId: c.ID, FindingId: c.Finding, Quick: quick, Text: c.Comment, SessionId: c.named()})
}

type reviewResponsesCmd struct {
	controllerClient
	reviewID
	After int64 `default:"-1" help:"Responses after this cursor (default: after the last ones this machine read)."`
	All   bool  `help:"Every response, from the first."`
}

func (c *reviewResponsesCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	read, err := readCursor(d, c.ID)
	if err != nil {
		return nil, err
	}
	after := c.After
	switch {
	case c.All:
		after = 0
	case after < 0:
		after = read
	}
	res, err := cl.reviews.ListResponses(ctx, &pb.ListResponsesRequest{ReviewId: c.ID, After: after})
	if err != nil {
		return nil, err
	}
	if res.Cursor > read {
		writeCursor(d, c.ID, res.Cursor)
	}
	return res, nil
}

type reviewWaitCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Responses bool  `help:"Wait for a response after those you last read (hotbed review responses), from anyone but this session, rather than for the review to end."`
	Timeout   int32 `default:"120" help:"Seconds to wait, at most 120."`
}

func (c *reviewWaitCmd) Exec(ctx context.Context, d deps) (any, error) {
	stream, err := c.clients(d.stream)
	if err != nil {
		return nil, err
	}
	req := &pb.WaitReviewRequest{Id: c.ID, TimeoutSeconds: c.Timeout}
	if c.Responses {
		if req.After, err = readCursor(d, c.ID); err != nil {
			return nil, err
		}
		// This session's own responses are not news to it; with no session
		// named or bound here, every response is.
		session, err := c.resolve(ctx, d)
		if err != nil && api.FromConnect(err).Code != api.CodeInvalidArgs {
			return nil, err
		}
		req.Responses, req.SessionId = true, session
	}
	return stream.reviews.WaitReview(ctx, req)
}

// The cursor of the responses this machine has read, per review.
func cursorPath(d deps, id int32) string {
	return filepath.Join(d.cache, "reviews", strconv.Itoa(int(id))+".cursor")
}

func readCursor(d deps, id int32) (int64, error) {
	b, err := os.ReadFile(cursorPath(d, id))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil // none read yet
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read the responses cursor %s: %w (delete it to read every response again)", cursorPath(d, id), err)
	}
	return n, nil
}

func writeCursor(d deps, id int32, cursor int64) {
	p := cursorPath(d, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		err = os.WriteFile(p, []byte(strconv.FormatInt(cursor, 10)), 0o644)
		if err != nil {
			fmt.Fprintf(d.stderr, "hotbed: could not save the responses cursor: %v\n", err)
		}
	}
}
