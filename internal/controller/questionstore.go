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

// Questions are kept as they are now (questions) and as every change left
// them (question_changes), each change in one transaction with its history
// row, so the history always says who brought a question to where it is.

// What changes to a question are called in its history.
const (
	questionAsked     = "asked"
	questionEdited    = "edited"
	questionMoved     = "moved"
	questionAnswered  = "answered"
	questionDismissed = "dismissed"
	questionHandled   = "handled"
	questionDeleted   = "deleted"
)

// AddQuestions adds prompts as questions about project (and its repo, if
// set) asked by by, at the top of the user's order, in the order given.
func (s *Store) AddQuestions(ctx context.Context, project, repo string, prompts []*pb.Prompt, by *pb.Stamp, from *pb.AskedFrom) ([]*pb.Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	pid, err := projectID(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	var top int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(rank), 0) FROM questions`).Scan(&top); err != nil {
		return nil, err
	}
	out := make([]*pb.Question, 0, len(prompts))
	for i, p := range prompts {
		q := &pb.Question{
			Prompt: p, State: pb.QuestionState_QUESTION_STATE_OPEN, Rank: top - int64(len(prompts)-i),
			Asked: by, AskedFrom: from, UpdatedAt: by.At, Project: project, Repo: repo,
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO questions (project_id, repo, state, rank, asked_by, asked_at, updated_at, doc) VALUES (?, ?, ?, ?, ?, ?, ?, '')`,
			pid, repo, q.State.String(), q.Rank, by.Session, by.At, by.At)
		if err != nil {
			return nil, fmt.Errorf("add question: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		q.Id = int32(id)
		if err := saveQuestion(ctx, tx, q, questionAsked, by); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, tx.Commit()
}

// ChangeQuestion makes one change to question id: edit changes it in place,
// or refuses with an error, and the change is saved with its history row.
// An answer or dismissal is also an event of the asking session, if it is
// live, so its agent's hotbed wait hears of it.
func (s *Store) ChangeQuestion(ctx context.Context, id int32, what string, by *pb.Stamp, edit func(*pb.Question) error) (*pb.Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q, err := changeQuestion(ctx, tx, id, what, by, edit)
	if err != nil {
		return nil, err
	}
	return q, tx.Commit()
}

// ChangeQuestions makes the same change to each of ids, all of them or,
// if edit refuses one, none.
func (s *Store) ChangeQuestions(ctx context.Context, ids []int32, what string, by *pb.Stamp, edit func(*pb.Question) error) ([]*pb.Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]*pb.Question, 0, len(ids))
	for _, id := range ids {
		q, err := changeQuestion(ctx, tx, id, what, by, edit)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, tx.Commit()
}

// MoveQuestion puts question id just before question before in the user's
// order, or last when before is 0. check may refuse the move, seeing both.
func (s *Store) MoveQuestion(ctx context.Context, id, before int32, by *pb.Stamp, check func(q, before *pb.Question) error) (*pb.Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var next *pb.Question
	if before != 0 {
		if next, err = questionIn(ctx, tx, before); err != nil {
			return nil, err
		}
	}
	q, err := changeQuestion(ctx, tx, id, questionMoved, by, func(q *pb.Question) error {
		if err := check(q, next); err != nil {
			return err
		}
		if next == nil {
			return tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rank), 0) + 1 FROM questions`).Scan(&q.Rank)
		}
		// Make room: everything from before's place on moves down one.
		if _, err := tx.ExecContext(ctx, `UPDATE questions SET rank = rank + 1 WHERE rank >= ? AND id != ?`, next.Rank, q.Id); err != nil {
			return err
		}
		q.Rank = next.Rank
		return nil
	})
	if err != nil {
		return nil, err
	}
	return q, tx.Commit()
}

func changeQuestion(ctx context.Context, tx *sql.Tx, id int32, what string, by *pb.Stamp, edit func(*pb.Question) error) (*pb.Question, error) {
	q, err := questionIn(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := edit(q); err != nil {
		return nil, err
	}
	if what != questionMoved {
		q.UpdatedAt = by.At
	}
	if err := saveQuestion(ctx, tx, q, what, by); err != nil {
		return nil, err
	}
	if what == questionAnswered || what == questionDismissed {
		if err := tellAsker(ctx, tx, q, what); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// saveQuestion stores q as it now is, and the change that made it so.
func saveQuestion(ctx context.Context, tx *sql.Tx, q *pb.Question, what string, by *pb.Stamp) error {
	stored := proto.Clone(q).(*pb.Question)
	stored.Rank, stored.History, stored.Project = 0, nil, ""
	doc, err := marshal(stored)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE questions SET state = ?, rank = ?, updated_at = ?, doc = ? WHERE id = ?`,
		q.State.String(), q.Rank, q.UpdatedAt, doc, q.Id); err != nil {
		return fmt.Errorf("save question %d: %w", q.Id, err)
	}
	stored.Rank, stored.Project = q.Rank, q.Project
	if doc, err = marshal(stored); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO question_changes (question_id, what, at, session, host, reason, doc) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		q.Id, what, by.At, by.Session, by.Host, by.Reason, doc); err != nil {
		return fmt.Errorf("record change to question %d: %w", q.Id, err)
	}
	return nil
}

// tellAsker adds the answer or dismissal of q to its asking session's
// events, if the session is live.
func tellAsker(ctx context.Context, tx *sql.Tx, q *pb.Question, what string) error {
	session := q.GetAsked().GetSession()
	var live bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = ? AND ended_at IS NULL)`, session).Scan(&live); err != nil || !live {
		return err
	}
	stamp := q.Answered
	if what == questionDismissed {
		stamp = q.Dismissed
	}
	doc, err := marshal(&pb.SessionEvent{At: stamp.At, What: what, SessionId: session, QuestionId: q.Id, Detail: responseLine(q)})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_events (session_id, doc) VALUES (?, ?)`, session, doc)
	return err
}

// responseLine is the user's response to q in a line:
// `question 12 (Auth method) answered: OAuth`.
func responseLine(q *pb.Question) string {
	head := fmt.Sprintf("question %d (%s)", q.Id, q.GetPrompt().GetHeader())
	if q.State == pb.QuestionState_QUESTION_STATE_DISMISSED {
		if r := q.Dismissed.GetReason(); r != "" {
			return head + " dismissed: " + r
		}
		return head + " dismissed"
	}
	a := q.GetAnswer()
	parts := append([]string{}, a.GetPicked()...)
	if a.GetOther() != "" {
		parts = append(parts, a.GetOther())
	}
	return head + " answered: " + strings.Join(parts, "; ")
}

// questionColumns are what scanQuestion reads, from questions q joined to
// their projects p.
const questionColumns = `q.id, q.rank, p.name, q.doc FROM questions q JOIN projects p ON p.id = q.project_id`

// Question is question id as it is now.
func (s *Store) Question(ctx context.Context, id int32) (*pb.Question, error) {
	return questionIn(ctx, s.db, id)
}

func questionIn(ctx context.Context, q querier, id int32) (*pb.Question, error) {
	return scanQuestion(q.QueryRowContext(ctx, `SELECT `+questionColumns+` WHERE q.id = ?`, id), id)
}

func scanQuestion(row interface{ Scan(...any) error }, id int32) (*pb.Question, error) {
	var (
		rank         int64
		project, doc string
	)
	err := row.Scan(&id, &rank, &project, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no question %d", id).WithSuggestion("hotbed question ls")
	}
	if err != nil {
		return nil, err
	}
	q, err := unmarshal(doc, &pb.Question{})
	if err != nil {
		return nil, fmt.Errorf("read question %d: %w", id, err)
	}
	q.Id, q.Rank, q.Project = id, rank, project
	return q, nil
}

// QuestionFilter picks questions to list.
type QuestionFilter struct {
	States  []pb.QuestionState // none: every state but deleted
	Order   pb.QuestionOrder
	Project string // only this project's; empty for every project's
	AskedBy string // only this session's
	Limit   int    // 0: all
}

var questionOrders = map[pb.QuestionOrder]string{
	pb.QuestionOrder_QUESTION_ORDER_UNSPECIFIED: "q.asked_at DESC, q.id DESC",
	pb.QuestionOrder_QUESTION_ORDER_NEWEST:      "q.asked_at DESC, q.id DESC",
	pb.QuestionOrder_QUESTION_ORDER_OLDEST:      "q.asked_at, q.id",
	pb.QuestionOrder_QUESTION_ORDER_UPDATED:     "q.updated_at DESC, q.id DESC",
	pb.QuestionOrder_QUESTION_ORDER_RANK:        "q.rank, q.id",
}

// Questions lists the questions f picks, in its order, and how many it
// picks without its limit.
func (s *Store) Questions(ctx context.Context, f QuestionFilter) ([]*pb.Question, int, error) {
	order, ok := questionOrders[f.Order]
	if !ok {
		return nil, 0, api.Errorf(api.CodeInvalidArgs, "no question order %v", f.Order)
	}
	states := f.States
	if len(states) == 0 {
		states = []pb.QuestionState{
			pb.QuestionState_QUESTION_STATE_OPEN, pb.QuestionState_QUESTION_STATE_ANSWERED,
			pb.QuestionState_QUESTION_STATE_DISMISSED, pb.QuestionState_QUESTION_STATE_HANDLED,
		}
	}
	where := `q.state IN (?` + strings.Repeat(", ?", len(states)-1) + `)`
	args := make([]any, 0, len(states)+1)
	for _, st := range states {
		args = append(args, st.String())
	}
	if f.Project != "" {
		if _, err := s.Project(ctx, f.Project); err != nil {
			return nil, 0, err
		}
		where += ` AND p.name = ?`
		args = append(args, f.Project)
	}
	if f.AskedBy != "" {
		where += ` AND q.asked_by = ?`
		args = append(args, f.AskedBy)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM questions q JOIN projects p ON p.id = q.project_id WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `SELECT ` + questionColumns + ` WHERE ` + where + ` ORDER BY ` + order
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*pb.Question
	for rows.Next() {
		q, err := scanQuestion(rows, 0)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, q)
	}
	return out, total, rows.Err()
}

// QuestionHistory is every change made to question id, oldest first.
func (s *Store) QuestionHistory(ctx context.Context, id int32) ([]*pb.QuestionChange, error) {
	q, err := s.Question(ctx, id)
	if err != nil {
		return nil, err
	}
	project := q.Project
	rows, err := s.db.QueryContext(ctx, `SELECT seq, what, at, session, host, reason, doc FROM question_changes WHERE question_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.QuestionChange
	for rows.Next() {
		c := &pb.QuestionChange{By: &pb.Stamp{}}
		var doc string
		if err := rows.Scan(&c.Seq, &c.What, &c.By.At, &c.By.Session, &c.By.Host, &c.By.Reason, &doc); err != nil {
			return nil, err
		}
		if c.Question, err = unmarshal(doc, &pb.Question{}); err != nil {
			return nil, fmt.Errorf("read change %d to question %d: %w", c.Seq, id, err)
		}
		c.Question.Id, c.Question.Project = id, project
		out = append(out, c)
	}
	return out, rows.Err()
}

// QuestionProjects counts, for each project with questions, those waiting
// on the user, those in the outbox, and every one.
func (s *Store) QuestionProjects(ctx context.Context) ([]*pb.QuestionProject, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.name, SUM(q.state = ?), SUM(q.state IN (?, ?)), COUNT(*)
		FROM questions q JOIN projects p ON p.id = q.project_id GROUP BY p.name ORDER BY p.name`,
		pb.QuestionState_QUESTION_STATE_OPEN.String(), pb.QuestionState_QUESTION_STATE_ANSWERED.String(), pb.QuestionState_QUESTION_STATE_DISMISSED.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.QuestionProject
	for rows.Next() {
		p := &pb.QuestionProject{}
		if err := rows.Scan(&p.Name, &p.Open, &p.Outbox, &p.Total); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OpenQuestions counts the questions waiting for the user.
func (s *Store) OpenQuestions(ctx context.Context) (int32, error) {
	var n int32
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM questions WHERE state = ?`, pb.QuestionState_QUESTION_STATE_OPEN.String()).Scan(&n)
	return n, err
}
