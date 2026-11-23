package controller

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// reviews serves ReviewService: reviews of an exact version, by reviewer
// agents, for the agent that asked and the user it shows them to.
type reviews struct{ *Server }

const maxTitle = 60

var commitID = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func (s *reviews) RequestReview(ctx context.Context, req *pb.RequestReviewRequest) (*pb.Review, error) {
	if !commitID.MatchString(req.Head) {
		return nil, api.Errorf(api.CodeInvalidArgs, "head %q is not a commit: a review is of an exact version", req.Head).
			WithSuggestion("hotbed review request, in a checkout, names its HEAD (or a snapshot of its changes) itself")
	}
	for _, c := range []string{req.Base, req.SnapshotOf} {
		if c != "" && !commitID.MatchString(c) {
			return nil, api.Errorf(api.CodeInvalidArgs, "%q is not a commit", c)
		}
	}
	by, project, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = fmt.Sprintf("%s at %s", cmp.Or(req.Branch, "detached"), req.Head[:min(8, len(req.Head))])
	}
	r := &pb.Review{
		Repo: gitops.CanonicalRemote(req.Repo), Title: title, Instructions: req.Instructions,
		Base: req.Base, Head: req.Head, Branch: req.Branch, SnapshotOf: req.SnapshotOf,
		State: pb.ReviewState_REVIEW_STATE_REQUESTED, Requested: by,
	}
	if r, err = s.cfg.Store.AddReview(ctx, project.Name, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *reviews) ListReviews(ctx context.Context, req *pb.ListReviewsRequest) (*pb.ListReviewsResponse, error) {
	if req.Limit < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "limit %d: give 0 for all, or how many", req.Limit)
	}
	project, err := s.listedProject(ctx, req)
	if err != nil {
		return nil, err
	}
	rs, err := s.cfg.Store.Reviews(ctx, project, req.States, int(req.Limit))
	return &pb.ListReviewsResponse{Reviews: rs}, err
}

func (s *reviews) GetReview(ctx context.Context, req *pb.GetReviewRequest) (*pb.Review, error) {
	return s.cfg.Store.Review(ctx, req.Id, true)
}

func (s *reviews) StartReview(ctx context.Context, req *pb.StartReviewRequest) (*pb.Review, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "starting review %d names no model", req.Id).
			WithSuggestion(fmt.Sprintf("pass the reviewer's exact model ID, as its provider names it: hotbed review start %d --model claude-opus-5-5", req.Id))
	}
	by, project, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	// Claiming it again is a no-op, so a reviewer may retry a start; with
	// another model it is refused, as the review already says which did it.
	if r, err := s.cfg.Store.Review(ctx, req.Id, false); err == nil && r.State == pb.ReviewState_REVIEW_STATE_IN_PROGRESS && r.Started.GetSession() == by.Session {
		if r.Model != model {
			return nil, api.Errorf(api.CodeConflict, "review %d was started by model %s, not %s", r.Id, cmp.Or(r.Model, "unknown"), model).
				WithSuggestion("a review is one model's: cancel it and request another for a different reviewer")
		}
		return s.cfg.Store.Review(ctx, req.Id, true)
	}
	if _, err := s.cfg.Store.ChangeReview(ctx, req.Id, reviewStarted, by, func(r *pb.Review) error {
		if err := inProject("review", r.Id, r.Project, project, by); err != nil {
			return err
		}
		if r.State != pb.ReviewState_REVIEW_STATE_REQUESTED {
			return api.Errorf(api.CodeConflict, "review %d is %s%s; only a requested review can be started", r.Id, reviewStateWord(r.State), reviewer(r))
		}
		r.State, r.Started, r.Model = pb.ReviewState_REVIEW_STATE_IN_PROGRESS, by, model
		return nil
	}); err != nil {
		return nil, err
	}
	return s.cfg.Store.Review(ctx, req.Id, true)
}

func (s *reviews) AddFindings(ctx context.Context, req *pb.AddFindingsRequest) (*pb.AddFindingsResponse, error) {
	reported, err := findingsOf(req)
	if err != nil {
		return nil, err
	}
	findings := make([]*pb.Finding, 0, len(reported))
	for i, rf := range reported {
		f, err := toFinding(rf)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "finding %d of %d: %s", i+1, len(reported), err.Message).WithSuggestion(err.Suggestion)
		}
		findings = append(findings, f)
	}
	by, _, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	out, err := s.cfg.Store.AddFindings(ctx, req.ReviewId, by, func(r *pb.Review) error { return reviewerOf(r, by, "report findings") }, findings, strings.TrimSpace(req.Report))
	if err != nil {
		return nil, err
	}
	return &pb.AddFindingsResponse{Findings: out}, nil
}

func (s *reviews) FinishReview(ctx context.Context, req *pb.FinishReviewRequest) (*pb.Review, error) {
	what, state := reviewFinished, pb.ReviewState_REVIEW_STATE_DONE
	if req.Failed {
		what, state = reviewFailed, pb.ReviewState_REVIEW_STATE_FAILED
		if strings.TrimSpace(req.Summary) == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "say why the review failed").WithSuggestion("hotbed review fail ID --reason WHY")
		}
	}
	by, _, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	if _, err := s.cfg.Store.ChangeReview(ctx, req.Id, what, by, func(r *pb.Review) error {
		if err := reviewerOf(r, by, "end it"); err != nil {
			return err
		}
		r.State, r.Summary, r.Ended = state, req.Summary, by
		return nil
	}); err != nil {
		return nil, err
	}
	s.sessionWakes.wake(s.requester(ctx, req.Id))
	return s.cfg.Store.Review(ctx, req.Id, true)
}

func (s *reviews) CancelReview(ctx context.Context, req *pb.CancelReviewRequest) (*pb.Review, error) {
	by, project, err := s.agentStamp(ctx, req.SessionId, req.Reason)
	if err != nil {
		return nil, err
	}
	if _, err := s.cfg.Store.ChangeReview(ctx, req.Id, reviewCancelled, by, func(r *pb.Review) error {
		if err := inProject("review", r.Id, r.Project, project, by); err != nil {
			return err
		}
		if ended(r.State) {
			return api.Errorf(api.CodeConflict, "review %d is %s already, %s", r.Id, reviewStateWord(r.State), stamped(r.Ended))
		}
		r.State, r.Summary, r.Ended = pb.ReviewState_REVIEW_STATE_CANCELLED, req.Reason, by
		return nil
	}); err != nil {
		return nil, err
	}
	return s.cfg.Store.Review(ctx, req.Id, true)
}

func (s *reviews) DecideFindings(ctx context.Context, req *pb.DecideFindingsRequest) (*pb.DecideFindingsResponse, error) {
	what, ok := map[pb.FindingDecision]string{
		pb.FindingDecision_FINDING_DECISION_ACCEPTED:  findingAccepted,
		pb.FindingDecision_FINDING_DECISION_DISMISSED: findingDismiss,
		pb.FindingDecision_FINDING_DECISION_FIXED:     findingFixed,
		pb.FindingDecision_FINDING_DECISION_INVALID:   findingInvalid,
		pb.FindingDecision_FINDING_DECISION_OPEN:      findingReopened,
	}[req.Decision]
	if !ok {
		return nil, api.Errorf(api.CodeInvalidArgs, "decide a finding accepted, dismissed, fixed, invalid or open")
	}
	if len(req.FindingIds) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "name the findings").WithSuggestion(fmt.Sprintf("hotbed review show %d", req.ReviewId))
	}
	if err := checkDecision(req); err != nil {
		return nil, err
	}
	by, project, err := s.agentStamp(ctx, req.SessionId, req.Reason)
	if err != nil {
		return nil, err
	}
	fs, err := s.cfg.Store.ChangeFindings(ctx, req.ReviewId, req.FindingIds, what, by, func(r *pb.Review, f *pb.Finding) error {
		if err := inProject("review", r.Id, r.Project, project, by); err != nil {
			return err
		}
		switch {
		case f.Decision == req.Decision:
			if f.Decided == nil {
				return api.Errorf(api.CodeConflict, "finding %d is %s already", f.Id, decisionWord(f.Decision))
			}
			return api.Errorf(api.CodeConflict, "finding %d is %s already, %s", f.Id, decisionWord(f.Decision), stamped(f.Decided))
		case f.Decision == pb.FindingDecision_FINDING_DECISION_FIXED && req.Decision != pb.FindingDecision_FINDING_DECISION_OPEN:
			return api.Errorf(api.CodeConflict, "finding %d is fixed, in %s: it is handled", f.Id, f.Fix.GetCommit()).
				WithSuggestion(fmt.Sprintf("hotbed review reopen %d %d, if the fix did not hold", r.Id, f.Id))
		case req.Decision == pb.FindingDecision_FINDING_DECISION_FIXED && (f.Decision == pb.FindingDecision_FINDING_DECISION_DISMISSED || f.Decision == pb.FindingDecision_FINDING_DECISION_INVALID):
			return api.Errorf(api.CodeConflict, "finding %d is %s: reopen it before you mark it fixed", f.Id, decisionWord(f.Decision)).
				WithSuggestion(fmt.Sprintf("hotbed review reopen %d %d", r.Id, f.Id))
		}
		f.Decision, f.Decided, f.Fix, f.Evidence = req.Decision, by, req.Fix, req.Evidence
		if req.Decision == pb.FindingDecision_FINDING_DECISION_OPEN {
			f.Decided = nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &pb.DecideFindingsResponse{Findings: fs}, nil
}

// fullCommit is a commit named in full, SHA-1 or SHA-256.
var fullCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// checkDecision holds a decision to what it needs: a fix its commit, in
// full, and its locations real ones; invalid its evidence; and neither
// given to another decision.
func checkDecision(req *pb.DecideFindingsRequest) error {
	fixed := req.Decision == pb.FindingDecision_FINDING_DECISION_FIXED
	invalid := req.Decision == pb.FindingDecision_FINDING_DECISION_INVALID
	switch {
	case req.Fix != nil && !fixed:
		return api.Errorf(api.CodeInvalidArgs, "a fix is given only with fixed")
	case req.Evidence != "" && !invalid:
		return api.Errorf(api.CodeInvalidArgs, "evidence is given only with invalid")
	case invalid && strings.TrimSpace(req.Evidence) == "":
		return api.Errorf(api.CodeInvalidArgs, "say why the findings are invalid: the evidence that they are not real").
			WithSuggestion("hotbed review invalid ID FINDING --evidence TEXT (or --evidence-file FILE)")
	case fixed && !fullCommit.MatchString(req.Fix.GetCommit()):
		return api.Errorf(api.CodeInvalidArgs, "fixed in %q: name the commit that fixed them in full (40 or 64 hex digits)", req.Fix.GetCommit()).
			WithSuggestion("hotbed review fixed ID FINDING --commit REF resolves REF in this checkout")
	}
	for _, l := range req.Fix.GetLocations() {
		switch {
		case strings.TrimSpace(l.Path) == "":
			return api.Errorf(api.CodeInvalidArgs, "a fix's location needs its path")
		case l.StartLine < 0 || l.EndLine < 0 || (l.EndLine != 0 && l.EndLine < l.StartLine):
			return api.Errorf(api.CodeInvalidArgs, "%s: lines %d to %d are not lines of a file", l.Path, l.StartLine, l.EndLine)
		}
	}
	return nil
}

func (s *reviews) Respond(ctx context.Context, req *pb.RespondRequest) (*pb.ReviewEvent, error) {
	if req.Quick == pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED && strings.TrimSpace(req.Text) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "a response gives a quick response, a comment, or both")
	}
	if req.Quick != pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED && req.FindingId == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "a quick response is to a finding: name it")
	}
	by, project, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	e := &pb.ReviewEvent{ReviewId: req.ReviewId, FindingId: req.FindingId, By: by, Quick: req.Quick, Text: req.Text}
	if e, err = s.cfg.Store.AddResponse(ctx, e, req.After, func(r *pb.Review, f *pb.Finding) error {
		if err := inProject("review", r.Id, r.Project, project, by); err != nil {
			return err
		}
		if f.GetDecision() == pb.FindingDecision_FINDING_DECISION_FIXED {
			return api.Errorf(api.CodeConflict, "finding %d is fixed, in %s: it is handled, and takes no more responses", f.Id, f.Fix.GetCommit()).
				WithSuggestion(fmt.Sprintf("hotbed review reopen %d %d, if the fix did not hold; or respond to the review as a whole", r.Id, f.Id))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	s.sessionWakes.wake(s.requester(ctx, req.ReviewId))
	return e, nil
}

func (s *reviews) ListResponses(ctx context.Context, req *pb.ListResponsesRequest) (*pb.ListResponsesResponse, error) {
	if _, err := s.cfg.Store.Review(ctx, req.ReviewId, false); err != nil {
		return nil, err
	}
	// Read all review events after req.After in one query so Cursor advances
	// past non-response events (e.g. accepted/dismissed), staying aligned with
	// Respond --after which checks all review events.
	events, err := s.cfg.Store.ReviewEvents(ctx, req.ReviewId, req.After, false)
	if err != nil {
		return nil, err
	}
	var rs []*pb.ReviewEvent
	cursor := req.After
	for _, e := range events {
		if e.Seq > cursor {
			cursor = e.Seq
		}
		if e.What == reviewResponded {
			rs = append(rs, e)
		}
	}
	return &pb.ListResponsesResponse{Responses: rs, Cursor: cursor}, nil
}

// requester is the session that requested review id, to wake; empty if
// there is none to wake.
func (s *reviews) requester(ctx context.Context, id int32) string {
	r, err := s.cfg.Store.Review(ctx, id, false)
	if err != nil {
		return ""
	}
	return r.Requested.GetSession()
}

// reviewerOf refuses a change only a review's reviewer may make, by anyone
// else, or once the review is not in progress.
func reviewerOf(r *pb.Review, by *pb.Stamp, doing string) error {
	switch {
	case r.State == pb.ReviewState_REVIEW_STATE_REQUESTED:
		return api.Errorf(api.CodeConflict, "review %d is not started: start it before you %s", r.Id, doing).
			WithSuggestion(fmt.Sprintf("hotbed review start %d --model MODEL (your exact model ID, such as claude-opus-5-5)", r.Id))
	case r.State != pb.ReviewState_REVIEW_STATE_IN_PROGRESS:
		return api.Errorf(api.CodeConflict, "review %d is %s, %s", r.Id, reviewStateWord(r.State), stamped(r.Ended))
	case r.Started.GetSession() != by.Session:
		return api.Errorf(api.CodeConflict, "review %d is %s's to review; session %s may not %s", r.Id, r.Started.GetSession(), by.Session, doing)
	}
	return nil
}

func ended(st pb.ReviewState) bool {
	return st == pb.ReviewState_REVIEW_STATE_DONE || st == pb.ReviewState_REVIEW_STATE_FAILED || st == pb.ReviewState_REVIEW_STATE_CANCELLED
}

// reviewer says who reviews r, if anyone: " by brave-otter".
func reviewer(r *pb.Review) string {
	if s := r.Started.GetSession(); s != "" {
		return " by " + s
	}
	return ""
}

func reviewStateWord(st pb.ReviewState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(st.String(), "REVIEW_STATE_")), "_", " ")
}

func decisionWord(d pb.FindingDecision) string {
	return strings.ToLower(strings.TrimPrefix(d.String(), "FINDING_DECISION_"))
}

var (
	severities = map[string]pb.FindingSeverity{
		"important": pb.FindingSeverity_FINDING_SEVERITY_IMPORTANT, "pre_existing": pb.FindingSeverity_FINDING_SEVERITY_PRE_EXISTING, "nit": pb.FindingSeverity_FINDING_SEVERITY_NIT,
	}
	verdicts = map[string]pb.FindingVerdict{
		"": pb.FindingVerdict_FINDING_VERDICT_UNSPECIFIED, "CONFIRMED": pb.FindingVerdict_FINDING_VERDICT_CONFIRMED, "PLAUSIBLE": pb.FindingVerdict_FINDING_VERDICT_PLAUSIBLE,
	}
)

// toFinding holds a reported finding to ReportFindings' rules, and makes it
// a finding: file and line, when given, are its first location.
func toFinding(rf *pb.ReportedFinding) (*pb.Finding, *api.Error) {
	summary := strings.TrimSpace(rf.Summary)
	if summary == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "a finding needs its summary: the defect")
	}
	severity, ok := severities[rf.Severity]
	if !ok {
		return nil, api.Errorf(api.CodeInvalidArgs, "severity %q: give important, pre_existing or nit", rf.Severity)
	}
	verdict, ok := verdicts[rf.Verdict]
	if !ok {
		return nil, api.Errorf(api.CodeInvalidArgs, "verdict %q: give CONFIRMED, PLAUSIBLE, or none", rf.Verdict)
	}
	title := strings.TrimSpace(rf.ShortSummary)
	if title == "" {
		title = firstLine(summary)
	}
	if utf8.RuneCountInString(title) > maxTitle {
		if rf.ShortSummary != "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "short_summary %q is longer than %d characters", rf.ShortSummary, maxTitle)
		}
		title = string([]rune(title)[:maxTitle-1]) + "…"
	}
	var locations []*pb.Location
	if rf.File != "" {
		locations = append(locations, &pb.Location{Path: rf.File, StartLine: rf.Line})
	} else if rf.Line != 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "line %d names no file", rf.Line)
	}
	for _, l := range rf.Locations {
		switch {
		case strings.TrimSpace(l.Path) == "":
			return nil, api.Errorf(api.CodeInvalidArgs, "a location needs its path")
		case l.StartLine < 0 || l.EndLine < 0:
			return nil, api.Errorf(api.CodeInvalidArgs, "%s: lines count from 1", l.Path)
		case l.EndLine != 0 && l.EndLine < l.StartLine:
			return nil, api.Errorf(api.CodeInvalidArgs, "%s: lines %d to %d run backwards", l.Path, l.StartLine, l.EndLine)
		}
		if len(locations) == 1 && rf.File == l.Path && (rf.Line == 0 || rf.Line == l.StartLine) {
			locations[0] = l // file and line said again, with more
			continue
		}
		locations = append(locations, l)
	}
	return &pb.Finding{
		Title: title, Summary: summary, FailureScenario: rf.FailureScenario, Category: rf.Category,
		Severity: severity, Verdict: verdict, Locations: locations,
	}, nil
}
