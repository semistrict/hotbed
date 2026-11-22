package controller

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	s.wakeReview(r.Id)
	return r, nil
}

func (s *reviews) ListReviews(ctx context.Context, req *pb.ListReviewsRequest) (*pb.ListReviewsResponse, error) {
	if req.Limit < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "limit %d: give 0 for all, or how many", req.Limit)
	}
	rs, err := s.cfg.Store.Reviews(ctx, req.Project, req.States, int(req.Limit))
	return &pb.ListReviewsResponse{Reviews: rs}, err
}

func (s *reviews) GetReview(ctx context.Context, req *pb.GetReviewRequest) (*pb.Review, error) {
	return s.cfg.Store.Review(ctx, req.Id, true)
}

func (s *reviews) StartReview(ctx context.Context, req *pb.StartReviewRequest) (*pb.Review, error) {
	by, project, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	// Claiming it again is a no-op, so a reviewer may retry a start.
	if r, err := s.cfg.Store.Review(ctx, req.Id, false); err == nil && r.State == pb.ReviewState_REVIEW_STATE_IN_PROGRESS && r.Started.GetSession() == by.Session {
		return s.cfg.Store.Review(ctx, req.Id, true)
	}
	if _, err := s.change(ctx, req.Id, reviewStarted, by, func(r *pb.Review) error {
		if err := inProject("review", r.Id, r.Project, project, by); err != nil {
			return err
		}
		if r.State != pb.ReviewState_REVIEW_STATE_REQUESTED {
			return api.Errorf(api.CodeConflict, "review %d is %s%s; only a requested review can be started", r.Id, reviewStateWord(r.State), reviewer(r))
		}
		r.State, r.Started = pb.ReviewState_REVIEW_STATE_IN_PROGRESS, by
		return nil
	}); err != nil {
		return nil, err
	}
	return s.cfg.Store.Review(ctx, req.Id, true)
}

func (s *reviews) AddFindings(ctx context.Context, req *pb.AddFindingsRequest) (*pb.AddFindingsResponse, error) {
	if len(req.Findings) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "no findings").
			WithSuggestion(`{"findings": [{"summary": "...", "failure_scenario": "...", "severity": "important", "short_summary": "...", "file": "a.go", "line": 12}]}`)
	}
	findings := make([]*pb.Finding, 0, len(req.Findings))
	for i, rf := range req.Findings {
		f, err := toFinding(rf)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "finding %d of %d: %s", i+1, len(req.Findings), err.Message).WithSuggestion(err.Suggestion)
		}
		findings = append(findings, f)
	}
	by, _, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	out, err := s.cfg.Store.AddFindings(ctx, req.ReviewId, by, func(r *pb.Review) error { return reviewerOf(r, by, "report findings") }, findings)
	if err != nil {
		return nil, err
	}
	s.wakeReview(req.ReviewId)
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
	if _, err := s.change(ctx, req.Id, what, by, func(r *pb.Review) error {
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
	if _, err := s.change(ctx, req.Id, reviewCancelled, by, func(r *pb.Review) error {
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
		pb.FindingDecision_FINDING_DECISION_OPEN:      findingReopened,
	}[req.Decision]
	if !ok {
		return nil, api.Errorf(api.CodeInvalidArgs, "decide a finding accepted, dismissed or open")
	}
	if len(req.FindingIds) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "name the findings").WithSuggestion(fmt.Sprintf("hotbed review show %d", req.ReviewId))
	}
	by, project, err := s.agentStamp(ctx, req.SessionId, req.Reason)
	if err != nil {
		return nil, err
	}
	fs, err := s.cfg.Store.ChangeFindings(ctx, req.ReviewId, req.FindingIds, what, by, func(r *pb.Review, f *pb.Finding) error {
		if err := inProject("review", r.Id, r.Project, project, by); err != nil {
			return err
		}
		if f.Decision == req.Decision {
			return api.Errorf(api.CodeConflict, "finding %d is %s already, %s", f.Id, decisionWord(f.Decision), stamped(f.Decided))
		}
		f.Decision, f.Decided = req.Decision, by
		if req.Decision == pb.FindingDecision_FINDING_DECISION_OPEN {
			f.Decided = nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.wakeReview(req.ReviewId)
	return &pb.DecideFindingsResponse{Findings: fs}, nil
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
	if e, err = s.cfg.Store.AddResponse(ctx, e, func(r *pb.Review) error {
		return inProject("review", r.Id, r.Project, project, by)
	}); err != nil {
		return nil, err
	}
	s.wakeReview(req.ReviewId)
	s.sessionWakes.wake(s.requester(ctx, req.ReviewId))
	return e, nil
}

func (s *reviews) ListResponses(ctx context.Context, req *pb.ListResponsesRequest) (*pb.ListResponsesResponse, error) {
	if _, err := s.cfg.Store.Review(ctx, req.ReviewId, false); err != nil {
		return nil, err
	}
	rs, err := s.cfg.Store.ReviewEvents(ctx, req.ReviewId, req.After, true)
	if err != nil {
		return nil, err
	}
	res := &pb.ListResponsesResponse{Responses: rs, Cursor: req.After}
	if n := len(rs); n > 0 {
		res.Cursor = rs[n-1].Seq
	}
	return res, nil
}

// WaitReview returns once the review has ended, or, waiting for responses,
// once someone other than the caller has responded after the cursor; or
// when its time is up, with the review as it is.
func (s *reviews) WaitReview(ctx context.Context, req *pb.WaitReviewRequest) (*pb.Review, error) {
	wait := maxWait
	if t := time.Duration(req.TimeoutSeconds) * time.Second; t > 0 && t < wait {
		wait = t
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	key := strconv.Itoa(int(req.Id))
	for {
		woken := s.reviewWakes.wait(key)
		r, err := s.cfg.Store.Review(ctx, req.Id, true)
		if err != nil {
			return nil, err
		}
		if s.waited(r, req) {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return r, nil
		case <-woken:
		}
	}
}

// waited says whether what req waits for has come to r.
func (s *reviews) waited(r *pb.Review, req *pb.WaitReviewRequest) bool {
	if !req.Responses {
		return ended(r.State)
	}
	for _, e := range r.Events {
		if e.What == reviewResponded && e.Seq > req.After && e.By.GetSession() != req.SessionId {
			return true
		}
	}
	return false
}

// change makes one change to a review and wakes those waiting on it.
func (s *reviews) change(ctx context.Context, id int32, what string, by *pb.Stamp, edit func(*pb.Review) error) (*pb.Review, error) {
	r, err := s.cfg.Store.ChangeReview(ctx, id, what, by, edit)
	if err != nil {
		return nil, err
	}
	s.wakeReview(id)
	return r, nil
}

func (s *Server) wakeReview(id int32) { s.reviewWakes.wake(strconv.Itoa(int(id))) }

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
			WithSuggestion(fmt.Sprintf("hotbed review start %d", r.Id))
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
