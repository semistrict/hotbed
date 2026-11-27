package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// Reviews of an exact version by reviewer agents. The implementer that wants
// one requests it (naming its exact model) and hands its number to one or more
// reviewer agents (in separate sessions or as sub-agents sharing a session).
// Each reviewer starts a pass naming its exact model, reports findings and
// finishes. The implementer reads the findings, accepts, dismisses/rejects,
// marks won't fix, marks fixed or invalid, and responds with comments; the
// user's responses come back through responses, and hotbed poll hears of them.

type reviewCmd struct {
	Request   reviewRequestCmd   `cmd:"" help:"Ask for a review of this checkout's exact version: from --base (default: where it left the remote's default branch) to --head (default: HEAD, or a snapshot of uncommitted changes). Names the implementer's exact model (--model, or inherited from the session). Prints the review; give its number to reviewer agent(s)."`
	Ls        reviewLsCmd        `cmd:"" help:"A project's reviews (default: this session's project, or the one holding this checkout's repo), newest first, with their findings counted."`
	Show      reviewShowCmd      `cmd:"" help:"A review: its version, implementer, reviewers, findings (with locations, decisions and responses) and every event."`
	Link      reviewLinkCmd      `cmd:"" help:"The review's page in the cockpit, to send the user."`
	Start     reviewStartCmd     `cmd:"" help:"Reviewer: claim/join a review with your exact model (--model); multiple reviewers (and sub-agents in the same session) may review the same review."`
	Findings  reviewFindingsCmd  `cmd:"" help:"Reviewer: report findings, as ReportFindings takes them ({\"findings\": [...]}, each with file, line, summary, failure_scenario, category, severity, short_summary, verdict), with \"locations\": [{path, start_line, end_line, excerpt}] besides; {\"findings\": []} says you found none. JSON on stdin or --file, or --from-report REPORT: a reviewer's whole report ending with that JSON in a fenced json hotbed-findings block, which the review keeps. Call it as often as you like."`
	Finish    reviewFinishCmd    `cmd:"" help:"Reviewer: finish your review pass, with a summary."`
	Fail      reviewFailCmd      `cmd:"" help:"Reviewer: end your review pass as failed, saying why."`
	Cancel    reviewCancelCmd    `cmd:"" help:"Withdraw a review that has not ended."`
	Accept    reviewAcceptCmd    `cmd:"" help:"Accept findings: you will act on them."`
	Dismiss   reviewDismissCmd   `cmd:"" aliases:"reject" help:"Dismiss/reject findings: you will not act on them, saying why."`
	WontFix   reviewWontFixCmd   `cmd:"" name:"wont-fix" help:"Mark findings won't fix: intentionally left unfixed, saying why."`
	Fixed     reviewFixedCmd     `cmd:"" help:"Mark findings fixed, in a commit (default: HEAD), with the files it changed and notes if you like. A fixed finding is handled: the user can respond to it no more."`
	Invalid   reviewInvalidCmd   `cmd:"" help:"Mark findings invalid: not real defects, with the evidence (Markdown, citing commits, files and lines as you like). The user may still dispute it."`
	Reopen    reviewReopenCmd    `cmd:"" help:"Make decided findings (accepted, dismissed, won't fix, fixed or invalid) undecided again."`
	Respond   reviewRespondCmd   `cmd:"" help:"Respond to a finding, or the review as a whole: a quick response, a comment, or both. Stamped as your bound agent session by default; pass --as-user to record a human user response."`
	Responses reviewResponsesCmd `cmd:"" help:"The responses to a review since you last asked (or --after N; --all for every one), the user's above all."`
}

// reviewID is the argument of the commands that act on one review.
type reviewID struct {
	ID int32 `arg:"" help:"The review's number."`
}

type reviewRequestCmd struct {
	controllerClient
	sessionFlag
	Title              string `help:"What it reviews, for people (default: the branch and commit)."`
	Instructions       string `help:"What the reviewer should look at, or for."`
	InstructionsFile   string `type:"existingfile" help:"Read the instructions from this file."`
	Base               string `help:"The commit the changes are from (default: where HEAD left the remote's default branch)."`
	Head               string `help:"The commit to review (default: HEAD, or, with uncommitted changes, a snapshot of them)."`
	Whole              bool   `help:"Review the whole tree at head, not its changes from a base."`
	Model              string `env:"HOTBED_MODEL" help:"The exact model that implemented the code (e.g. gemini-4-neon, claude-opus-4-6). Defaults to the session's model."`
	Subagent           string `help:"Optional sub-agent name/role of the implementer when running inside a shared session."`
	ImplementerSession string `name:"implementer-session" help:"The session that implemented the code, if different from the requesting session."`
}

func (c *reviewRequestCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	instructions, err := textOrFile("instructions", c.Instructions, c.InstructionsFile)
	if err != nil {
		return nil, err
	}
	v, err := d.versionToReview(ctx, c.Base, c.Head, c.Whole)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.RequestReview(ctx, &pb.RequestReviewRequest{
		SessionId:          session,
		Title:              c.Title,
		Instructions:       instructions,
		Repo:               v.remote,
		Base:               v.base,
		Head:               v.head,
		Branch:             v.branch,
		SnapshotOf:         v.snapshotOf,
		Model:              strings.TrimSpace(c.Model),
		Subagent:           strings.TrimSpace(c.Subagent),
		T3CodeThreadId:     d.t3codeThreadID(),
		ImplementerSession: strings.TrimSpace(c.ImplementerSession),
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
func (h here) versionToReview(ctx context.Context, base, head string, whole bool) (reviewedVersion, error) {
	var v reviewedVersion
	root, err := h.checkoutRoot(ctx)
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
	var snap gitops.Snap
	switch {
	case head != "":
		if v.head, err = commit(head); err != nil {
			return v, err
		}
	default:
		if v.head, err = commit("HEAD"); err != nil {
			return v, err
		}
		if snap, err = gitops.Snapshot(ctx, root); err != nil {
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
	if v.snapshotOf != "" {
		if err := snap.PinSubs(ctx, "refs/hotbed/reviews/"); err != nil {
			return v, err
		}
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
	sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	var states []pb.ReviewState
	for _, w := range c.State {
		states = append(states, pb.ReviewState(pb.ReviewState_value["REVIEW_STATE_"+strings.ToUpper(strings.ReplaceAll(w, "-", "_"))]))
	}
	req := &pb.ListReviewsRequest{SessionId: sc.session, Remote: sc.remote, States: states, Limit: c.Limit}
	switch {
	case sc.all:
		req.Scope = &pb.ListReviewsRequest_AllProjects{AllProjects: true}
	case sc.project != "":
		req.Scope = &pb.ListReviewsRequest_Project{Project: sc.project}
	}
	return cl.reviews.ListReviews(ctx, req)
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
	base, err := api.ControllerBaseURL(c.Controller)
	if err != nil {
		return nil, err
	}
	return linkResult{URL: fmt.Sprintf("%s/reviews/%d", base, c.ID)}, nil
}

type reviewStartCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Model    string `required:"" help:"The exact model doing the review, as its provider names it (claude-opus-4-6, gpt-5.5-codex), not its family."`
	Subagent string `help:"Optional sub-agent name/role when spawned inside a shared session (e.g. verifying-reviewer, codex-exec)."`
	New      bool   `help:"Start a new reviewer pass even if an identical (session, model, subagent) reviewer is already in progress."`
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
	return cl.reviews.StartReview(ctx, &pb.StartReviewRequest{
		Id:             c.ID,
		SessionId:      session,
		Model:          c.Model,
		Subagent:       strings.TrimSpace(c.Subagent),
		T3CodeThreadId: d.t3codeThreadID(),
		NewReviewer:    c.New,
	})
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
	jsonInput
	FromReport string `type:"existingfile" name:"from-report" placeholder:"REPORT" help:"Read a reviewer's whole report (Markdown) from this file instead of JSON: its findings are its last fenced json hotbed-findings block, and the review keeps the report. For a reviewer that could not call hotbed itself."`
	Reviewer   int32  `help:"Which reviewer pass (#id) is reporting (optional when unambiguous for this session)."`
	Model      string `help:"Optional model filter when multiple reviewers share this session."`
	Subagent   string `help:"Optional sub-agent filter when multiple reviewers share this session."`
}

func (c *reviewFindingsCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	req, err := c.request(d)
	if err != nil {
		return nil, err
	}
	req.ReviewId, req.SessionId = c.ID, session
	req.ReviewerId = c.Reviewer
	req.Model = strings.TrimSpace(c.Model)
	req.Subagent = strings.TrimSpace(c.Subagent)
	req.T3CodeThreadId = d.t3codeThreadID()
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.AddFindings(ctx, req)
}

// request is what the reviewer reports: the findings' JSON, or a report
// the controller finds them in.
func (c *reviewFindingsCmd) request(d deps) (*pb.AddFindingsRequest, error) {
	if c.FromReport != "" {
		if c.File != "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "give the findings' JSON (--file) or a report (--from-report), not both")
		}
		b, err := os.ReadFile(c.FromReport)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(b)) == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "report %s is empty: the reviewer reported nothing, not that it found nothing", c.FromReport).
				WithSuggestion("hotbed review fail ID --reason WHY, if the reviewer did not finish")
		}
		return &pb.AddFindingsRequest{Report: string(b)}, nil
	}
	b, err := c.read(d, findingsExample)
	if err != nil {
		return nil, err
	}
	req := &pb.AddFindingsRequest{}
	if err := protojson.Unmarshal(b, req); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "read the findings' JSON: %v", err).WithSuggestion(findingsExample)
	}
	if req.ReviewId != 0 || req.SessionId != "" || req.Report != "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "the JSON holds findings alone; hotbed fills in the review and session, and --from-report gives a report").WithSuggestion(findingsExample)
	}
	return req, nil
}

type reviewFinishCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Summary  string `required:"" help:"What you found, in a few lines."`
	Reviewer int32  `help:"Which reviewer pass (#id) is finishing (optional when unambiguous for this session)."`
	Model    string `help:"Optional model filter when multiple reviewers share this session."`
	Subagent string `help:"Optional sub-agent filter when multiple reviewers share this session."`
}

func (c *reviewFinishCmd) Exec(ctx context.Context, d deps) (any, error) {
	return finishReview(ctx, d, c.controllerClient, c.sessionFlag, c.ID, c.Summary, false, c.Reviewer, c.Model, c.Subagent)
}

type reviewFailCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Reason   string `required:"" help:"Why the review could not be done."`
	Reviewer int32  `help:"Which reviewer pass (#id) failed (optional when unambiguous for this session)."`
	Model    string `help:"Optional model filter when multiple reviewers share this session."`
	Subagent string `help:"Optional sub-agent filter when multiple reviewers share this session."`
}

func (c *reviewFailCmd) Exec(ctx context.Context, d deps) (any, error) {
	return finishReview(ctx, d, c.controllerClient, c.sessionFlag, c.ID, c.Reason, true, c.Reviewer, c.Model, c.Subagent)
}

func finishReview(ctx context.Context, d deps, cc controllerClient, sf sessionFlag, id int32, summary string, failed bool, reviewer int32, model, subagent string) (any, error) {
	session, err := sf.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := cc.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.reviews.FinishReview(ctx, &pb.FinishReviewRequest{
		Id:             id,
		SessionId:      session,
		Summary:        summary,
		Failed:         failed,
		ReviewerId:     reviewer,
		Model:          strings.TrimSpace(model),
		Subagent:       strings.TrimSpace(subagent),
		T3CodeThreadId: d.t3codeThreadID(),
	})
}

type reviewCancelCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Reason   string `help:"Why it is no longer wanted."`
	Model    string `env:"HOTBED_MODEL" help:"Exact model cancelling the review (defaults to session/implementer model)."`
	Subagent string `help:"Optional sub-agent label."`
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
	return cl.reviews.CancelReview(ctx, &pb.CancelReviewRequest{
		Id:             c.ID,
		SessionId:      session,
		Reason:         c.Reason,
		Model:          strings.TrimSpace(c.Model),
		Subagent:       strings.TrimSpace(c.Subagent),
		T3CodeThreadId: d.t3codeThreadID(),
	})
}

// decideCmd is what the commands that decide findings take.
type decideCmd struct {
	controllerClient
	sessionFlag
	reviewID
	Findings []int32 `arg:"" name:"finding" help:"The findings' numbers."`
	Model    string  `env:"HOTBED_MODEL" help:"Exact model ID of the deciding agent (defaults to session/implementer model)."`
	Subagent string  `help:"Optional sub-agent name/role of the deciding agent."`
	AsUser   bool    `name:"as-user" help:"Record this decision as coming from the human user rather than an agent session."`
}

func (c decideCmd) decide(ctx context.Context, d deps, decision pb.FindingDecision, reason string) (any, error) {
	return c.send(ctx, d, &pb.DecideFindingsRequest{Decision: decision, Reason: reason})
}

// send sends req, a decision, of the findings named, as this session (or user with --as-user).
func (c decideCmd) send(ctx context.Context, d deps, req *pb.DecideFindingsRequest) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	req.ReviewId, req.FindingIds = c.ID, c.Findings
	if c.AsUser {
		req.ActorKind = pb.ActorKind_ACTOR_KIND_USER
		req.SessionId = ""
	} else {
		session, err := c.resolve(ctx, d)
		if err != nil {
			return nil, err
		}
		req.ActorKind = pb.ActorKind_ACTOR_KIND_AGENT
		req.SessionId = session
		req.Model = strings.TrimSpace(c.Model)
		req.Subagent = strings.TrimSpace(c.Subagent)
		req.T3CodeThreadId = d.t3codeThreadID()
	}
	return cl.reviews.DecideFindings(ctx, req)
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

type reviewWontFixCmd struct {
	decideCmd
	Reason string `required:"" help:"Why you will not fix them."`
}

func (c *reviewWontFixCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.decide(ctx, d, pb.FindingDecision_FINDING_DECISION_WONT_FIX, c.Reason)
}

type reviewFixedCmd struct {
	decideCmd
	Commit    string   `default:"HEAD" help:"The commit that fixed them, resolved in this checkout (outside one, give it in full)."`
	Where     []string `name:"where" placeholder:"PATH[:LINE[-END]]" help:"A file the fix changed, and its lines if you like. Repeatable."`
	Notes     string   `help:"What you say of the fix (Markdown)."`
	NotesFile string   `type:"existingfile" help:"Read the notes from this file."`
}

func (c *reviewFixedCmd) Exec(ctx context.Context, d deps) (any, error) {
	commit, err := d.fixingCommit(ctx, c.Commit)
	if err != nil {
		return nil, err
	}
	notes, err := textOrFile("notes", c.Notes, c.NotesFile)
	if err != nil {
		return nil, err
	}
	fix := &pb.Fix{Commit: commit, Notes: notes}
	for _, w := range c.Where {
		l, err := parseWhere(w)
		if err != nil {
			return nil, err
		}
		fix.Locations = append(fix.Locations, l)
	}
	return c.send(ctx, d, &pb.DecideFindingsRequest{Decision: pb.FindingDecision_FINDING_DECISION_FIXED, Fix: fix})
}

// fixingCommit is ref in full: resolved in this checkout, or, outside one,
// as given.
func (h here) fixingCommit(ctx context.Context, ref string) (string, error) {
	root, err := h.checkoutRoot(ctx)
	if err != nil {
		return ref, nil // the controller holds it to a full commit
	}
	c, err := gitops.Git{Dir: root}.Run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || c == "" {
		return "", api.Errorf(api.CodeInvalidArgs, "%s is not a commit here", ref).WithSuggestion("pass --commit REF: the commit that fixed them")
	}
	return c, nil
}

// parseWhere is PATH, PATH:LINE or PATH:LINE-END.
func parseWhere(w string) (*pb.Location, error) {
	bad := api.Errorf(api.CodeInvalidArgs, "--where %q: give PATH, PATH:LINE or PATH:LINE-END", w)
	path, lines, hasLines := strings.Cut(w, ":")
	if path == "" {
		return nil, bad
	}
	l := &pb.Location{Path: path}
	if !hasLines {
		return l, nil
	}
	start, end, hasEnd := strings.Cut(lines, "-")
	n, err := strconv.ParseInt(start, 10, 32)
	if err != nil || n < 1 {
		return nil, bad
	}
	l.StartLine = int32(n)
	if hasEnd {
		m, err := strconv.ParseInt(end, 10, 32)
		if err != nil || m < n {
			return nil, bad
		}
		l.EndLine = int32(m)
	}
	return l, nil
}

// textOrFile is the text a flag gives, or its file's; not both.
func textOrFile(what, text, file string) (string, error) {
	if file == "" {
		return text, nil
	}
	if text != "" {
		return "", api.Errorf(api.CodeInvalidArgs, "give the %s with --%s or --%s-file, not both", what, what, what)
	}
	b, err := os.ReadFile(file)
	return string(b), err
}

type reviewInvalidCmd struct {
	decideCmd
	Evidence     string `help:"Why they are not real defects (Markdown): what you checked, citing commits, files and lines."`
	EvidenceFile string `type:"existingfile" help:"Read the evidence from this file."`
}

func (c *reviewInvalidCmd) Exec(ctx context.Context, d deps) (any, error) {
	evidence, err := textOrFile("evidence", c.Evidence, c.EvidenceFile)
	if err != nil {
		return nil, err
	}
	return c.send(ctx, d, &pb.DecideFindingsRequest{Decision: pb.FindingDecision_FINDING_DECISION_INVALID, Evidence: evidence})
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
	Finding  int32  `help:"The finding it answers (default: the review as a whole)."`
	Quick    string `enum:",verify,fix-if-verified,wont-fix" default:"" help:"A quick response to the finding: verify, fix-if-verified or wont-fix."`
	Comment  string `help:"What you say."`
	After    int64  `help:"Refuse with CONFLICT if the review already has events after this seq."`
	Model    string `env:"HOTBED_MODEL" help:"Exact model ID of the responding agent (defaults to session/implementer model)."`
	Subagent string `help:"Optional sub-agent name/role of the responding agent."`
	AsUser   bool   `name:"as-user" help:"Record this response as coming from the human user rather than an agent session."`
}

func (c *reviewRespondCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	quick := pb.QuickResponse(pb.QuickResponse_value["QUICK_RESPONSE_"+strings.ToUpper(strings.ReplaceAll(c.Quick, "-", "_"))])
	req := &pb.RespondRequest{
		ReviewId:  c.ID,
		FindingId: c.Finding,
		Quick:     quick,
		Text:      c.Comment,
		After:     c.After,
	}
	if c.AsUser {
		req.ActorKind = pb.ActorKind_ACTOR_KIND_USER
	} else {
		session, err := c.optional(ctx, d)
		if err != nil {
			return nil, err
		}
		if session != "" {
			req.ActorKind = pb.ActorKind_ACTOR_KIND_AGENT
			req.SessionId = session
			req.Model = strings.TrimSpace(c.Model)
			req.Subagent = strings.TrimSpace(c.Subagent)
			req.T3CodeThreadId = d.t3codeThreadID()
		} else {
			req.ActorKind = pb.ActorKind_ACTOR_KIND_USER
		}
	}
	return cl.reviews.Respond(ctx, req)
}

type reviewResponsesCmd struct {
	controllerClient
	reviewID
	After int64 `default:"-1" help:"Responses after this cursor (default: after the last ones this machine read)."`
	All   bool  `help:"Every response, from the first."`
}

func reviewCursor(d deps, id int32) cursorFile {
	return cursorFile{
		path:       filepath.Join(d.cache, "reviews", strconv.Itoa(int(id))+".cursor"),
		label:      "responses cursor",
		suggestion: "delete the file to read every response again",
	}
}

func (c *reviewResponsesCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	cur := reviewCursor(d, c.ID)
	read, err := cur.read()
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
		if err := cur.write(res.Cursor); err != nil {
			fmt.Fprintf(d.stderr, "hotbed: %v\n", err)
		}
	}
	return res, nil
}
