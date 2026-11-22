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
	q := s.q.WithTx(tx)
	pid, err := projectID(ctx, q, project)
	if err != nil {
		return nil, err
	}
	top, err := q.GetMinQuestionRank(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Question, 0, len(prompts))
	for i, p := range prompts {
		item := &pb.Question{
			Prompt: p, State: pb.QuestionState_QUESTION_STATE_OPEN, Rank: top - int64(len(prompts)-i),
			Asked: by, AskedFrom: from, UpdatedAt: by.At, Project: project, Repo: repo,
		}
		res, err := q.InsertQuestion(ctx, controllerdb.InsertQuestionParams{
			ProjectID: pid, Repo: repo, State: item.State.String(), Rank: item.Rank,
			AskedBy: by.Session, AskedAt: by.At, UpdatedAt: by.At,
		})
		if err != nil {
			return nil, fmt.Errorf("add question: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		item.Id = int32(id)
		if err := saveQuestion(ctx, q, item, questionAsked, by); err != nil {
			return nil, err
		}
		out = append(out, item)
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
	q, err := changeQuestion(ctx, s.q.WithTx(tx), id, what, by, edit)
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
	q := s.q.WithTx(tx)
	out := make([]*pb.Question, 0, len(ids))
	for _, id := range ids {
		item, err := changeQuestion(ctx, q, id, what, by, edit)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
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
	q := s.q.WithTx(tx)
	var next *pb.Question
	if before != 0 {
		if next, err = questionIn(ctx, q, before); err != nil {
			return nil, err
		}
	}
	item, err := changeQuestion(ctx, q, id, questionMoved, by, func(item *pb.Question) error {
		if err := check(item, next); err != nil {
			return err
		}
		if next == nil {
			maxRank, err := q.GetMaxQuestionRank(ctx)
			if err != nil {
				return err
			}
			item.Rank = maxRank + 1
			return nil
		}
		// Make room: everything from before's place on moves down one.
		if err := q.ShiftQuestionRanks(ctx, controllerdb.ShiftQuestionRanksParams{Rank: next.Rank, ID: int64(item.Id)}); err != nil {
			return err
		}
		item.Rank = next.Rank
		return nil
	})
	if err != nil {
		return nil, err
	}
	return item, tx.Commit()
}

func changeQuestion(ctx context.Context, q *controllerdb.Queries, id int32, what string, by *pb.Stamp, edit func(*pb.Question) error) (*pb.Question, error) {
	item, err := questionIn(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if err := edit(item); err != nil {
		return nil, err
	}
	if what != questionMoved {
		item.UpdatedAt = by.At
	}
	if err := saveQuestion(ctx, q, item, what, by); err != nil {
		return nil, err
	}
	if what == questionAnswered || what == questionDismissed {
		if err := tellAsker(ctx, q, item, what); err != nil {
			return nil, err
		}
	}
	return item, nil
}

// saveQuestion stores q as it now is, and the change that made it so.
func saveQuestion(ctx context.Context, q *controllerdb.Queries, item *pb.Question, what string, by *pb.Stamp) error {
	stored := proto.Clone(item).(*pb.Question)
	stored.Rank, stored.History, stored.Project = 0, nil, ""
	doc, err := marshal(stored)
	if err != nil {
		return err
	}
	if err := q.UpdateQuestion(ctx, controllerdb.UpdateQuestionParams{
		State: item.State.String(), Rank: item.Rank, UpdatedAt: item.UpdatedAt, Doc: doc, ID: int64(item.Id),
	}); err != nil {
		return fmt.Errorf("save question %d: %w", item.Id, err)
	}
	stored.Rank, stored.Project = item.Rank, item.Project
	if doc, err = marshal(stored); err != nil {
		return err
	}
	if err := q.InsertQuestionChange(ctx, controllerdb.InsertQuestionChangeParams{
		QuestionID: int64(item.Id), What: what, At: by.At, Session: by.Session, Host: by.Host, Reason: by.Reason, Doc: doc,
	}); err != nil {
		return fmt.Errorf("record change to question %d: %w", item.Id, err)
	}
	return nil
}

// tellAsker adds the answer or dismissal of q to its asking session's
// events, if the session is live.
func tellAsker(ctx context.Context, q *controllerdb.Queries, item *pb.Question, what string) error {
	stamp := item.Answered
	if what == questionDismissed {
		stamp = item.Dismissed
	}
	return tellSession(ctx, q, &pb.SessionEvent{At: stamp.At, What: what, SessionId: item.GetAsked().GetSession(), QuestionId: item.Id, Detail: responseLine(item)})
}

// tellSession adds e to its session's events, if the session is live, so
// its agent's hotbed wait hears of it.
func tellSession(ctx context.Context, q *controllerdb.Queries, e *pb.SessionEvent) error {
	live, err := q.IsSessionLive(ctx, e.SessionId)
	if err != nil || live == 0 {
		return err
	}
	doc, err := marshal(e)
	if err != nil {
		return err
	}
	return q.InsertSessionEvent(ctx, controllerdb.InsertSessionEventParams{SessionID: e.SessionId, Doc: doc})
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

// Question is question id as it is now.
func (s *Store) Question(ctx context.Context, id int32) (*pb.Question, error) {
	return questionIn(ctx, s.q, id)
}

func questionIn(ctx context.Context, q *controllerdb.Queries, id int32) (*pb.Question, error) {
	row, err := q.GetQuestion(ctx, int64(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no question %d", id).WithSuggestion("hotbed question ls")
	}
	if err != nil {
		return nil, err
	}
	return decodeQuestion(int32(row.ID), row.Rank, row.Project, row.Doc)
}

func decodeQuestion(id int32, rank int64, project, doc string) (*pb.Question, error) {
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

// Questions lists the questions f picks, in its order, and how many it
// picks without its limit.
func (s *Store) Questions(ctx context.Context, f QuestionFilter) ([]*pb.Question, int, error) {
	states := f.States
	if len(states) == 0 {
		states = []pb.QuestionState{
			pb.QuestionState_QUESTION_STATE_OPEN, pb.QuestionState_QUESTION_STATE_ANSWERED,
			pb.QuestionState_QUESTION_STATE_DISMISSED, pb.QuestionState_QUESTION_STATE_HANDLED,
		}
	}
	stateNames := make([]string, len(states))
	for i, st := range states {
		stateNames[i] = st.String()
	}
	if f.Project != "" {
		if _, err := s.Project(ctx, f.Project); err != nil {
			return nil, 0, err
		}
	}
	limit := int64(-1)
	if f.Limit > 0 {
		limit = int64(f.Limit)
	}
	var out []*pb.Question
	switch f.Order {
	case pb.QuestionOrder_QUESTION_ORDER_UNSPECIFIED, pb.QuestionOrder_QUESTION_ORDER_NEWEST:
		rows, err := s.q.ListQuestionsNewest(ctx, controllerdb.ListQuestionsNewestParams{
			States: stateNames, Column2: f.Project, Column3: f.AskedBy, Limit: limit,
		})
		if err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			q, err := decodeQuestion(int32(r.ID), r.Rank, r.Project, r.Doc)
			if err != nil {
				return nil, 0, err
			}
			out = append(out, q)
		}
	case pb.QuestionOrder_QUESTION_ORDER_OLDEST:
		rows, err := s.q.ListQuestionsOldest(ctx, controllerdb.ListQuestionsOldestParams{
			States: stateNames, Column2: f.Project, Column3: f.AskedBy, Limit: limit,
		})
		if err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			q, err := decodeQuestion(int32(r.ID), r.Rank, r.Project, r.Doc)
			if err != nil {
				return nil, 0, err
			}
			out = append(out, q)
		}
	case pb.QuestionOrder_QUESTION_ORDER_UPDATED:
		rows, err := s.q.ListQuestionsUpdated(ctx, controllerdb.ListQuestionsUpdatedParams{
			States: stateNames, Column2: f.Project, Column3: f.AskedBy, Limit: limit,
		})
		if err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			q, err := decodeQuestion(int32(r.ID), r.Rank, r.Project, r.Doc)
			if err != nil {
				return nil, 0, err
			}
			out = append(out, q)
		}
	case pb.QuestionOrder_QUESTION_ORDER_RANK:
		rows, err := s.q.ListQuestionsRank(ctx, controllerdb.ListQuestionsRankParams{
			States: stateNames, Column2: f.Project, Column3: f.AskedBy, Limit: limit,
		})
		if err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			q, err := decodeQuestion(int32(r.ID), r.Rank, r.Project, r.Doc)
			if err != nil {
				return nil, 0, err
			}
			out = append(out, q)
		}
	default:
		return nil, 0, api.Errorf(api.CodeInvalidArgs, "no question order %v", f.Order)
	}
	total, err := s.q.CountQuestions(ctx, controllerdb.CountQuestionsParams{
		States: stateNames, Column2: f.Project, Column3: f.AskedBy,
	})
	if err != nil {
		return nil, 0, err
	}
	return out, int(total), nil
}

// QuestionHistory is every change made to question id, oldest first.
func (s *Store) QuestionHistory(ctx context.Context, id int32) ([]*pb.QuestionChange, error) {
	q, err := s.Question(ctx, id)
	if err != nil {
		return nil, err
	}
	project := q.Project
	rows, err := s.q.ListQuestionChanges(ctx, int64(id))
	if err != nil {
		return nil, err
	}
	out := make([]*pb.QuestionChange, 0, len(rows))
	for _, r := range rows {
		c := &pb.QuestionChange{
			Seq:  r.Seq,
			What: r.What,
			By:   &pb.Stamp{At: r.At, Session: r.Session, Host: r.Host, Reason: r.Reason},
		}
		if c.Question, err = unmarshal(r.Doc, &pb.Question{}); err != nil {
			return nil, fmt.Errorf("read change %d to question %d: %w", c.Seq, id, err)
		}
		c.Question.Id, c.Question.Project = id, project
		out = append(out, c)
	}
	return out, nil
}

// QuestionProjects counts, for each project with questions, those waiting
// on the user, those in the outbox, and every one.
func (s *Store) QuestionProjects(ctx context.Context) ([]*pb.QuestionProject, error) {
	rows, err := s.q.ListQuestionProjects(ctx, controllerdb.ListQuestionProjectsParams{
		OpenState:      pb.QuestionState_QUESTION_STATE_OPEN.String(),
		AnsweredState:  pb.QuestionState_QUESTION_STATE_ANSWERED.String(),
		DismissedState: pb.QuestionState_QUESTION_STATE_DISMISSED.String(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*pb.QuestionProject, 0, len(rows))
	for _, r := range rows {
		out = append(out, &pb.QuestionProject{
			Name: r.Name, Open: int32(r.OpenCount), Outbox: int32(r.OutboxCount), Total: int32(r.Total),
		})
	}
	return out, nil
}

// OpenQuestions counts the questions waiting for the user.
func (s *Store) OpenQuestions(ctx context.Context) (int32, error) {
	n, err := s.q.CountOpenQuestions(ctx, pb.QuestionState_QUESTION_STATE_OPEN.String())
	return int32(n), err
}
