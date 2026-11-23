package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Reviews are kept as they are now (reviews, review_findings) and as every
// change left them (review_events, which also hold the responses), each
// change in one transaction with its event, so the events always say who
// brought a review to where it is.

// What review events are called.
const (
	reviewRequested = "requested"
	reviewStarted   = "started"
	reviewFound     = "found"
	reviewNoneFound = "found none"
	reviewReported  = "reported"
	reviewFinished  = "finished"
	reviewFailed    = "failed"
	reviewCancelled = "cancelled"
	findingAccepted = "accepted"
	findingDismiss  = "dismissed"
	findingFixed    = "fixed"
	findingInvalid  = "invalid"
	findingReopened = "reopened"
	reviewResponded = "responded"
)

// AddReview records a review requested in project, with its event.
func (s *Store) AddReview(ctx context.Context, project string, r *pb.Review) (*pb.Review, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	pid, err := projectID(ctx, q, project)
	if err != nil {
		return nil, err
	}
	res, err := q.InsertReview(ctx, controllerdb.InsertReviewParams{
		ProjectID: pid, State: r.State.String(), RequestedBy: r.Requested.Session,
		RequestedAt: r.Requested.At, UpdatedAt: r.Requested.At,
	})
	if err != nil {
		return nil, fmt.Errorf("add review: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	r.Id, r.Project, r.UpdatedAt = int32(id), project, r.Requested.At
	if err := saveReview(ctx, q, r, reviewRequested, r.Requested); err != nil {
		return nil, err
	}
	return r, tx.Commit()
}

// ChangeReview makes one change to review id: edit changes it in place, or
// refuses with an error, and the change is saved with its event. A review
// that ends is an event of its requesting session too.
func (s *Store) ChangeReview(ctx context.Context, id int32, what string, by *pb.Stamp, edit func(*pb.Review) error) (*pb.Review, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	r, err := reviewIn(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if err := edit(r); err != nil {
		return nil, err
	}
	r.UpdatedAt = by.At
	if err := saveReview(ctx, q, r, what, by); err != nil {
		return nil, err
	}
	if what == reviewFinished || what == reviewFailed {
		line := fmt.Sprintf("review %d (%s) %s", r.Id, r.Title, map[string]string{reviewFinished: "is done", reviewFailed: "failed"}[what])
		if r.Summary != "" {
			line += ": " + firstLine(r.Summary)
		}
		w := map[string]pb.ReviewUpdate_What{reviewFinished: pb.ReviewUpdate_WHAT_FINISHED, reviewFailed: pb.ReviewUpdate_WHAT_FAILED}[what]
		if _, err := tellSession(ctx, q, r.Requested.GetSession(), by.At, reviewUpdate(w, r.Id, line)); err != nil {
			return nil, err
		}
	}
	return r, tx.Commit()
}

// AddFindings adds findings to review id, once check allows it, each with
// its event; none is an event of its own, that the reviewer found nothing.
// A report, when given, is kept with the review, with its event first.
func (s *Store) AddFindings(ctx context.Context, id int32, by *pb.Stamp, check func(*pb.Review) error, findings []*pb.Finding, report string) ([]*pb.Finding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	r, err := reviewIn(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if err := check(r); err != nil {
		return nil, err
	}
	if report != "" {
		r.Report, r.UpdatedAt = report, by.At
		if err := saveReview(ctx, q, r, reviewReported, by); err != nil {
			return nil, err
		}
	}
	if len(findings) == 0 {
		if _, err := addReviewEvent(ctx, q, &pb.ReviewEvent{ReviewId: id, What: reviewNoneFound, By: by}, ""); err != nil {
			return nil, err
		}
	}
	for _, f := range findings {
		res, err := q.InsertReviewFinding(ctx, int64(id))
		if err != nil {
			return nil, fmt.Errorf("add finding to review %d: %w", id, err)
		}
		fid, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		f.Id, f.ReviewId, f.Found, f.Decision = int32(fid), id, by, pb.FindingDecision_FINDING_DECISION_OPEN
		if err := saveFinding(ctx, q, f, reviewFound, by); err != nil {
			return nil, err
		}
	}
	if err := touchReview(ctx, q, id, by.At); err != nil {
		return nil, err
	}
	return findings, tx.Commit()
}

// ChangeFindings makes the same change to each of ids, findings of review
// id: all of them or, if edit refuses one, none.
func (s *Store) ChangeFindings(ctx context.Context, id int32, ids []int32, what string, by *pb.Stamp, edit func(*pb.Review, *pb.Finding) error) ([]*pb.Finding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	r, err := reviewIn(ctx, q, id)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Finding, 0, len(ids))
	for _, fid := range ids {
		f, err := findingIn(ctx, q, id, fid)
		if err != nil {
			return nil, err
		}
		if err := edit(r, f); err != nil {
			return nil, err
		}
		if err := saveFinding(ctx, q, f, what, by); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := touchReview(ctx, q, id, by.At); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// AddResponse records a response to review e.ReviewId (and its finding,
// unless 0), once check allows it (the finding nil for the review as a
// whole). A response by anyone but the requesting session is an event of
// that session too.
func (s *Store) AddResponse(ctx context.Context, e *pb.ReviewEvent, after int64, check func(*pb.Review, *pb.Finding) error) (*pb.ReviewEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	r, err := reviewIn(ctx, q, e.ReviewId)
	if err != nil {
		return nil, err
	}
	var f *pb.Finding
	if e.FindingId != 0 {
		if f, err = findingIn(ctx, q, r.Id, e.FindingId); err != nil {
			return nil, err
		}
	}
	if err := check(r, f); err != nil {
		return nil, err
	}
	if after > 0 {
		newer, err := q.ListReviewEventsAfter(ctx, controllerdb.ListReviewEventsAfterParams{ReviewID: int64(r.Id), Seq: after})
		if err != nil {
			return nil, err
		}
		if len(newer) > 0 {
			return nil, api.Errorf(api.CodeConflict, "review %d has newer events (latest is %d, not %d)", r.Id, newer[len(newer)-1].Seq, after).
				WithSuggestion(fmt.Sprintf("hotbed review responses %d", r.Id))
		}
	}
	about := fmt.Sprintf("review %d", r.Id)
	if f != nil {
		about = fmt.Sprintf("finding %d (%s) of review %d", f.Id, f.Title, r.Id)
	}
	e.What = reviewResponded
	if e.Seq, err = addReviewEvent(ctx, q, e, ""); err != nil {
		return nil, err
	}
	if err := touchReview(ctx, q, r.Id, e.By.At); err != nil {
		return nil, err
	}
	if requester := r.Requested.GetSession(); requester != e.By.Session {
		if _, err := tellSession(ctx, q, requester, e.By.At, reviewUpdate(pb.ReviewUpdate_WHAT_RESPONDED, r.Id, responseOn(about, e))); err != nil {
			return nil, err
		}
	}
	return e, tx.Commit()
}

// responseOn is a response in a line: `the user on finding 3 (...): fix if verified; "and add a test"`.
func responseOn(about string, e *pb.ReviewEvent) string {
	who := "the user"
	if e.By.Session != "" {
		who = e.By.Session
	}
	var parts []string
	if e.Quick != pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED {
		parts = append(parts, quickWord(e.Quick))
	}
	if e.Text != "" {
		parts = append(parts, fmt.Sprintf("%q", firstLine(e.Text)))
	}
	return fmt.Sprintf("%s on %s: %s", who, about, strings.Join(parts, "; "))
}

// quickWord is a quick response as people say it: fix if verified.
func quickWord(q pb.QuickResponse) string {
	return quickWords[q]
}

var quickWords = map[pb.QuickResponse]string{
	pb.QuickResponse_QUICK_RESPONSE_VERIFY:          "verify",
	pb.QuickResponse_QUICK_RESPONSE_FIX_IF_VERIFIED: "fix if verified",
	pb.QuickResponse_QUICK_RESPONSE_WONT_FIX:        "won't fix",
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func touchReview(ctx context.Context, q *controllerdb.Queries, id int32, at int64) error {
	return q.TouchReview(ctx, controllerdb.TouchReviewParams{At: at, ID: int64(id)})
}

// saveReview stores r as it now is, and the event that made it so.
func saveReview(ctx context.Context, q *controllerdb.Queries, r *pb.Review, what string, by *pb.Stamp) error {
	stored := bareReview(r)
	doc, err := marshal(stored)
	if err != nil {
		return err
	}
	if err := q.UpdateReview(ctx, controllerdb.UpdateReviewParams{
		State: r.State.String(), UpdatedAt: r.UpdatedAt, Doc: doc, ID: int64(r.Id),
	}); err != nil {
		return fmt.Errorf("save review %d: %w", r.Id, err)
	}
	_, err = addReviewEvent(ctx, q, &pb.ReviewEvent{ReviewId: r.Id, What: what, By: by, Text: by.Reason}, doc)
	return err
}

// bareReview is r as its row keeps it: no project (a column), findings,
// events or counts.
func bareReview(r *pb.Review) *pb.Review {
	b := proto.Clone(r).(*pb.Review)
	b.Project, b.Findings, b.Events = "", nil, nil
	b.Important, b.PreExisting, b.Nits, b.Open, b.Responses = 0, 0, 0, 0, 0
	return b
}

// saveFinding stores f as it now is, and the event that made it so.
func saveFinding(ctx context.Context, q *controllerdb.Queries, f *pb.Finding, what string, by *pb.Stamp) error {
	stored := proto.Clone(f).(*pb.Finding)
	stored.Quick, stored.Responses = pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED, 0
	doc, err := marshal(stored)
	if err != nil {
		return err
	}
	if err := q.UpdateReviewFinding(ctx, controllerdb.UpdateReviewFindingParams{Doc: doc, ID: int64(f.Id)}); err != nil {
		return fmt.Errorf("save finding %d: %w", f.Id, err)
	}
	_, err = addReviewEvent(ctx, q, &pb.ReviewEvent{ReviewId: f.ReviewId, FindingId: f.Id, What: what, By: by, Text: by.Reason}, doc)
	return err
}

func addReviewEvent(ctx context.Context, q *controllerdb.Queries, e *pb.ReviewEvent, doc string) (int64, error) {
	quick := ""
	if e.Quick != pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED {
		quick = e.Quick.String()
	}
	res, err := q.InsertReviewEvent(ctx, controllerdb.InsertReviewEventParams{
		ReviewID: int64(e.ReviewId), FindingID: int64(e.FindingId), What: e.What,
		At: e.By.At, Session: e.By.Session, Host: e.By.Host, Quick: quick, Text: e.Text, Doc: doc,
	})
	if err != nil {
		return 0, fmt.Errorf("record review %d event: %w", e.ReviewId, err)
	}
	return res.LastInsertId()
}

func reviewIn(ctx context.Context, q *controllerdb.Queries, id int32) (*pb.Review, error) {
	row, err := q.GetReview(ctx, int64(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no review %d", id).WithSuggestion("hotbed review ls")
	}
	if err != nil {
		return nil, err
	}
	return decodeReview(int32(row.ID), row.Project, row.Doc)
}

func decodeReview(id int32, project, doc string) (*pb.Review, error) {
	r, err := unmarshal(doc, &pb.Review{})
	if err != nil {
		return nil, fmt.Errorf("read review %d: %w", id, err)
	}
	r.Id, r.Project = id, project
	return r, nil
}

func findingIn(ctx context.Context, q *controllerdb.Queries, review, id int32) (*pb.Finding, error) {
	doc, err := q.GetReviewFinding(ctx, controllerdb.GetReviewFindingParams{ID: int64(id), ReviewID: int64(review)})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "review %d has no finding %d", review, id).WithSuggestion(fmt.Sprintf("hotbed review show %d", review))
	}
	if err != nil {
		return nil, err
	}
	f, err := unmarshal(doc, &pb.Finding{})
	if err != nil {
		return nil, fmt.Errorf("read finding %d: %w", id, err)
	}
	return f, nil
}

// Review is review id, with its findings and every event when full; with
// its counts either way.
func (s *Store) Review(ctx context.Context, id int32, full bool) (*pb.Review, error) {
	r, err := reviewIn(ctx, s.q, id)
	if err != nil {
		return nil, err
	}
	return r, s.fill(ctx, r, full)
}

// fill gives r its findings (each with its responses counted and latest
// quick response), counts and, when full, its findings and events.
func (s *Store) fill(ctx context.Context, r *pb.Review, full bool) error {
	findings, err := s.findings(ctx, r.Id)
	if err != nil {
		return err
	}
	events, err := s.ReviewEvents(ctx, r.Id, 0, false)
	if err != nil {
		return err
	}
	byID := map[int32]*pb.Finding{}
	for _, f := range findings {
		byID[f.Id] = f
		switch f.Severity {
		case pb.FindingSeverity_FINDING_SEVERITY_IMPORTANT:
			r.Important++
		case pb.FindingSeverity_FINDING_SEVERITY_PRE_EXISTING:
			r.PreExisting++
		case pb.FindingSeverity_FINDING_SEVERITY_NIT:
			r.Nits++
		}
		if f.Decision == pb.FindingDecision_FINDING_DECISION_OPEN {
			r.Open++
		}
	}
	for _, e := range events {
		if e.What != reviewResponded {
			continue
		}
		r.Responses++
		if f, ok := byID[e.FindingId]; ok {
			f.Responses++
			if e.Quick != pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED {
				f.Quick = e.Quick
			}
		}
	}
	if full {
		r.Findings, r.Events = findings, events
	}
	return nil
}

func (s *Store) findings(ctx context.Context, review int32) ([]*pb.Finding, error) {
	rows, err := s.q.ListReviewFindings(ctx, int64(review))
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Finding, 0, len(rows))
	for _, r := range rows {
		f, err := unmarshal(r.Doc, &pb.Finding{})
		if err != nil {
			return nil, fmt.Errorf("read finding %d: %w", r.ID, err)
		}
		out = append(out, f)
	}
	return out, nil
}

// ReviewEvents lists review id's events after a cursor, oldest first; only
// its responses when responses is set.
func (s *Store) ReviewEvents(ctx context.Context, id int32, after int64, responses bool) ([]*pb.ReviewEvent, error) {
	if responses {
		rows, err := s.q.ListReviewResponsesAfter(ctx, controllerdb.ListReviewResponsesAfterParams{
			ReviewID: int64(id), Seq: after, What: reviewResponded,
		})
		if err != nil {
			return nil, err
		}
		out := make([]*pb.ReviewEvent, 0, len(rows))
		for _, r := range rows {
			out = append(out, &pb.ReviewEvent{
				Seq: r.Seq, ReviewId: id, FindingId: int32(r.FindingID), What: r.What,
				By:    &pb.Stamp{At: r.At, Session: r.Session, Host: r.Host},
				Quick: pb.QuickResponse(pb.QuickResponse_value[r.Quick]),
				Text:  r.Text,
			})
		}
		return out, nil
	}
	rows, err := s.q.ListReviewEventsAfter(ctx, controllerdb.ListReviewEventsAfterParams{ReviewID: int64(id), Seq: after})
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ReviewEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, &pb.ReviewEvent{
			Seq: r.Seq, ReviewId: id, FindingId: int32(r.FindingID), What: r.What,
			By:    &pb.Stamp{At: r.At, Session: r.Session, Host: r.Host},
			Quick: pb.QuickResponse(pb.QuickResponse_value[r.Quick]),
			Text:  r.Text,
		})
	}
	return out, nil
}

// Reviews lists the reviews of project (empty: every project's) in states
// (none: all), newest first, at most limit (0: all), with their counts.
func (s *Store) Reviews(ctx context.Context, project string, states []pb.ReviewState, limit int) ([]*pb.Review, error) {
	if project != "" {
		if _, err := s.Project(ctx, project); err != nil {
			return nil, err
		}
	}
	maxRows := int64(-1)
	if limit > 0 {
		maxRows = int64(limit)
	}
	var out []*pb.Review
	if len(states) == 0 {
		rows, err := s.q.ListReviews(ctx, controllerdb.ListReviewsParams{Project: project, MaxRows: maxRows})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			r, err := decodeReview(int32(row.ID), row.Project, row.Doc)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	} else {
		names := make([]string, len(states))
		for i, st := range states {
			names[i] = st.String()
		}
		rows, err := s.q.ListReviewsByStates(ctx, controllerdb.ListReviewsByStatesParams{
			Column1: project, States: names, Limit: maxRows,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			r, err := decodeReview(int32(row.ID), row.Project, row.Doc)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	for _, r := range out {
		r.Report = "" // a listing is the reviews' outlines; GetReview has the rest
		if err := s.fill(ctx, r, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReviewCounts counts, for each project, its reviews requested or in
// progress, and those done with findings still open.
func (s *Store) ReviewCounts(ctx context.Context) (active, toDecide map[string]int32, err error) {
	active, toDecide = map[string]int32{}, map[string]int32{}
	aRows, err := s.q.CountActiveReviewsByProject(ctx, controllerdb.CountActiveReviewsByProjectParams{
		RequestedState:  pb.ReviewState_REVIEW_STATE_REQUESTED.String(),
		InProgressState: pb.ReviewState_REVIEW_STATE_IN_PROGRESS.String(),
	})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range aRows {
		active[r.Name] = int32(r.Count)
	}
	dRows, err := s.q.CountReviewsToDecideByProject(ctx, controllerdb.CountReviewsToDecideByProjectParams{
		DoneState:    pb.ReviewState_REVIEW_STATE_DONE.String(),
		OpenDecision: pb.FindingDecision_FINDING_DECISION_OPEN.String(),
	})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range dRows {
		toDecide[r.Name] = int32(r.Count)
	}
	return active, toDecide, nil
}
