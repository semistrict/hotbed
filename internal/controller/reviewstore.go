package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
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
	reviewFinished  = "finished"
	reviewFailed    = "failed"
	reviewCancelled = "cancelled"
	findingAccepted = "accepted"
	findingDismiss  = "dismissed"
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
	pid, err := projectID(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO reviews (project_id, state, requested_by, requested_at, updated_at, doc) VALUES (?, ?, ?, ?, ?, '')`,
		pid, r.State.String(), r.Requested.Session, r.Requested.At, r.Requested.At)
	if err != nil {
		return nil, fmt.Errorf("add review: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	r.Id, r.Project, r.UpdatedAt = int32(id), project, r.Requested.At
	if err := saveReview(ctx, tx, r, reviewRequested, r.Requested); err != nil {
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
	r, err := reviewIn(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := edit(r); err != nil {
		return nil, err
	}
	r.UpdatedAt = by.At
	if err := saveReview(ctx, tx, r, what, by); err != nil {
		return nil, err
	}
	if what == reviewFinished || what == reviewFailed {
		line := fmt.Sprintf("review %d (%s) %s", r.Id, r.Title, map[string]string{reviewFinished: "is done", reviewFailed: "failed"}[what])
		if r.Summary != "" {
			line += ": " + firstLine(r.Summary)
		}
		e := &pb.SessionEvent{At: by.At, What: map[string]string{reviewFinished: "reviewed", reviewFailed: "review failed"}[what], SessionId: r.Requested.GetSession(), ReviewId: r.Id, Detail: line}
		if err := tellSession(ctx, tx, e); err != nil {
			return nil, err
		}
	}
	return r, tx.Commit()
}

// AddFindings adds findings to review id, once check allows it, each with
// its event.
func (s *Store) AddFindings(ctx context.Context, id int32, by *pb.Stamp, check func(*pb.Review) error, findings []*pb.Finding) ([]*pb.Finding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := reviewIn(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := check(r); err != nil {
		return nil, err
	}
	for _, f := range findings {
		res, err := tx.ExecContext(ctx, `INSERT INTO review_findings (review_id, doc) VALUES (?, '')`, id)
		if err != nil {
			return nil, fmt.Errorf("add finding to review %d: %w", id, err)
		}
		fid, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		f.Id, f.ReviewId, f.Found, f.Decision = int32(fid), id, by, pb.FindingDecision_FINDING_DECISION_OPEN
		if err := saveFinding(ctx, tx, f, reviewFound, by); err != nil {
			return nil, err
		}
	}
	if err := touchReview(ctx, tx, id, by.At); err != nil {
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
	r, err := reviewIn(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Finding, 0, len(ids))
	for _, fid := range ids {
		f, err := findingIn(ctx, tx, id, fid)
		if err != nil {
			return nil, err
		}
		if err := edit(r, f); err != nil {
			return nil, err
		}
		if err := saveFinding(ctx, tx, f, what, by); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := touchReview(ctx, tx, id, by.At); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// AddResponse records a response to review e.ReviewId (and its finding,
// unless 0), once check allows it. A response by anyone but the requesting
// session is an event of that session too.
func (s *Store) AddResponse(ctx context.Context, e *pb.ReviewEvent, check func(*pb.Review) error) (*pb.ReviewEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := reviewIn(ctx, tx, e.ReviewId)
	if err != nil {
		return nil, err
	}
	if err := check(r); err != nil {
		return nil, err
	}
	about := fmt.Sprintf("review %d", r.Id)
	if e.FindingId != 0 {
		f, err := findingIn(ctx, tx, r.Id, e.FindingId)
		if err != nil {
			return nil, err
		}
		about = fmt.Sprintf("finding %d (%s) of review %d", f.Id, f.Title, r.Id)
	}
	e.What = reviewResponded
	if e.Seq, err = addReviewEvent(ctx, tx, e, ""); err != nil {
		return nil, err
	}
	if err := touchReview(ctx, tx, r.Id, e.By.At); err != nil {
		return nil, err
	}
	if requester := r.Requested.GetSession(); requester != e.By.Session {
		if err := tellSession(ctx, tx, &pb.SessionEvent{At: e.By.At, What: reviewResponded, SessionId: requester, ReviewId: r.Id, Detail: responseOn(about, e)}); err != nil {
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

func touchReview(ctx context.Context, tx *sql.Tx, id int32, at int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?`, at, id)
	return err
}

// saveReview stores r as it now is, and the event that made it so.
func saveReview(ctx context.Context, tx *sql.Tx, r *pb.Review, what string, by *pb.Stamp) error {
	stored := bareReview(r)
	doc, err := marshal(stored)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE reviews SET state = ?, updated_at = ?, doc = ? WHERE id = ?`, r.State.String(), r.UpdatedAt, doc, r.Id); err != nil {
		return fmt.Errorf("save review %d: %w", r.Id, err)
	}
	_, err = addReviewEvent(ctx, tx, &pb.ReviewEvent{ReviewId: r.Id, What: what, By: by, Text: by.Reason}, doc)
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
func saveFinding(ctx context.Context, tx *sql.Tx, f *pb.Finding, what string, by *pb.Stamp) error {
	stored := proto.Clone(f).(*pb.Finding)
	stored.Quick, stored.Responses = pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED, 0
	doc, err := marshal(stored)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_findings SET doc = ? WHERE id = ?`, doc, f.Id); err != nil {
		return fmt.Errorf("save finding %d: %w", f.Id, err)
	}
	_, err = addReviewEvent(ctx, tx, &pb.ReviewEvent{ReviewId: f.ReviewId, FindingId: f.Id, What: what, By: by, Text: by.Reason}, doc)
	return err
}

func addReviewEvent(ctx context.Context, tx *sql.Tx, e *pb.ReviewEvent, doc string) (int64, error) {
	quick := ""
	if e.Quick != pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED {
		quick = e.Quick.String()
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO review_events (review_id, finding_id, what, at, session, host, quick, text, doc) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ReviewId, e.FindingId, e.What, e.By.At, e.By.Session, e.By.Host, quick, e.Text, doc)
	if err != nil {
		return 0, fmt.Errorf("record review %d event: %w", e.ReviewId, err)
	}
	return res.LastInsertId()
}

// reviewColumns are what scanReview reads, from reviews r joined to their
// projects p.
const reviewColumns = `r.id, p.name, r.doc FROM reviews r JOIN projects p ON p.id = r.project_id`

func reviewIn(ctx context.Context, q querier, id int32) (*pb.Review, error) {
	return scanReview(q.QueryRowContext(ctx, `SELECT `+reviewColumns+` WHERE r.id = ?`, id), id)
}

func scanReview(row interface{ Scan(...any) error }, id int32) (*pb.Review, error) {
	var project, doc string
	err := row.Scan(&id, &project, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no review %d", id).WithSuggestion("hotbed review ls")
	}
	if err != nil {
		return nil, err
	}
	r, err := unmarshal(doc, &pb.Review{})
	if err != nil {
		return nil, fmt.Errorf("read review %d: %w", id, err)
	}
	r.Id, r.Project = id, project
	return r, nil
}

func findingIn(ctx context.Context, q querier, review, id int32) (*pb.Finding, error) {
	var doc string
	err := q.QueryRowContext(ctx, `SELECT doc FROM review_findings WHERE id = ? AND review_id = ?`, id, review).Scan(&doc)
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
	r, err := reviewIn(ctx, s.db, id)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, doc FROM review_findings WHERE review_id = ? ORDER BY id`, review)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.Finding
	for rows.Next() {
		var id int32
		var doc string
		if err := rows.Scan(&id, &doc); err != nil {
			return nil, err
		}
		f, err := unmarshal(doc, &pb.Finding{})
		if err != nil {
			return nil, fmt.Errorf("read finding %d: %w", id, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ReviewEvents lists review id's events after a cursor, oldest first; only
// its responses when responses is set.
func (s *Store) ReviewEvents(ctx context.Context, id int32, after int64, responses bool) ([]*pb.ReviewEvent, error) {
	query := `SELECT seq, finding_id, what, at, session, host, quick, text FROM review_events WHERE review_id = ? AND seq > ?`
	args := []any{id, after}
	if responses {
		query += ` AND what = ?`
		args = append(args, reviewResponded)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.ReviewEvent
	for rows.Next() {
		e := &pb.ReviewEvent{ReviewId: id, By: &pb.Stamp{}}
		var quick string
		if err := rows.Scan(&e.Seq, &e.FindingId, &e.What, &e.By.At, &e.By.Session, &e.By.Host, &quick, &e.Text); err != nil {
			return nil, err
		}
		e.Quick = pb.QuickResponse(pb.QuickResponse_value[quick])
		out = append(out, e)
	}
	return out, rows.Err()
}

// Reviews lists the reviews of project (empty: every project's) in states
// (none: all), newest first, at most limit (0: all), with their counts.
func (s *Store) Reviews(ctx context.Context, project string, states []pb.ReviewState, limit int) ([]*pb.Review, error) {
	query := `SELECT ` + reviewColumns + ` WHERE (? = '' OR p.name = ?)`
	args := []any{project, project}
	if project != "" {
		if _, err := s.Project(ctx, project); err != nil {
			return nil, err
		}
	}
	if len(states) > 0 {
		query += ` AND r.state IN (?` + strings.Repeat(", ?", len(states)-1) + `)`
		for _, st := range states {
			args = append(args, st.String())
		}
	}
	query += ` ORDER BY r.requested_at DESC, r.id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []*pb.Review
	for rows.Next() {
		r, err := scanReview(rows, 0)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
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
	for _, c := range []struct {
		into  map[string]int32
		query string
		args  []any
	}{
		{active, `SELECT p.name, COUNT(*) FROM reviews r JOIN projects p ON p.id = r.project_id WHERE r.state IN (?, ?) GROUP BY p.name`,
			[]any{pb.ReviewState_REVIEW_STATE_REQUESTED.String(), pb.ReviewState_REVIEW_STATE_IN_PROGRESS.String()}},
		{toDecide, `SELECT p.name, COUNT(DISTINCT r.id) FROM reviews r JOIN projects p ON p.id = r.project_id JOIN review_findings f ON f.review_id = r.id
			WHERE r.state = ? AND json_extract(f.doc, '$.decision') = ? GROUP BY p.name`,
			[]any{pb.ReviewState_REVIEW_STATE_DONE.String(), pb.FindingDecision_FINDING_DECISION_OPEN.String()}},
	} {
		rows, err := s.db.QueryContext(ctx, c.query, c.args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var name string
			var n int32
			if err := rows.Scan(&name, &n); err != nil {
				rows.Close()
				return nil, nil, err
			}
			c.into[name] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return active, toDecide, nil
}
