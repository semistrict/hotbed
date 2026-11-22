package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// questions serves QuestionService: one list of questions for every
// session. Agents ask, edit, delete and acknowledge as their session; the
// user answers, dismisses and orders them, from the cockpit or through an
// agent that passes the user's word on.
type questions struct{ *Server }

// What AskUserQuestion allows, which questions keep to so that the same
// JSON serves both.
const (
	maxAsked   = 4
	maxHeader  = 12
	minOptions = 2
	maxOptions = 4
	// The lines of git status an AskedFrom keeps.
	dirtyKept = 20
	// The questionWakes key: every change to a question wakes it.
	anyQuestion = ""
)

func (s *questions) AddQuestions(ctx context.Context, req *pb.AddQuestionsRequest) (*pb.AddQuestionsResponse, error) {
	if n := len(req.Questions); n == 0 || n > maxAsked {
		return nil, api.Errorf(api.CodeInvalidArgs, "ask one to %d questions at a time, as AskUserQuestion does; got %d", maxAsked, n).
			WithSuggestion(`{"questions": [{"question": "Which ...?", "header": "Short", "options": [{"label": "A", "description": "..."}, {"label": "B", "description": "..."}], "multiSelect": false}]}`)
	}
	for i, p := range req.Questions {
		if err := checkPrompt(p); err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "question %d of %d: %s", i+1, len(req.Questions), err.Message).WithSuggestion(err.Suggestion)
		}
	}
	by, project, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	repo, err := questionRepo(project, req.Repo, req.AskedFrom.GetRemote())
	if err != nil {
		return nil, err
	}
	if from := req.AskedFrom; from != nil && len(from.Dirty) > dirtyKept {
		from.Dirty = from.Dirty[:dirtyKept]
	}
	qs, err := s.cfg.Store.AddQuestions(ctx, project.Name, repo, req.Questions, by, req.AskedFrom)
	if err != nil {
		return nil, err
	}
	s.questionWakes.wake(anyQuestion)
	return &pb.AddQuestionsResponse{Questions: qs}, nil
}

// questionRepo is the repo of project a question is about: the one named,
// which the project must hold, or else the one it was asked from, if the
// project holds it, or else none: the project as a whole.
func questionRepo(project *pb.Project, named, askedFrom string) (string, error) {
	if named != "" {
		repo := gitops.CanonicalRemote(named)
		if !slices.Contains(project.Repos, repo) {
			return "", api.Errorf(api.CodeInvalidArgs, "project %s does not hold %s; it holds %s", project.Name, repo, strings.Join(project.Repos, ", ")).
				WithSuggestion("hotbed project add " + project.Name + " --repo " + repo)
		}
		return repo, nil
	}
	if repo := gitops.CanonicalRemote(askedFrom); repo != "" && slices.Contains(project.Repos, repo) {
		return repo, nil
	}
	return "", nil
}

func (s *questions) EditQuestion(ctx context.Context, req *pb.EditQuestionRequest) (*pb.Question, error) {
	if err := checkPrompt(req.Prompt); err != nil {
		return nil, err
	}
	by, project, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	return s.change(ctx, req.Id, questionEdited, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if err := mustBeOpen(q, "edited"); err != nil {
			return err
		}
		q.Prompt, q.Edited = req.Prompt, by
		return nil
	})
}

func (s *questions) DeleteQuestion(ctx context.Context, req *pb.DeleteQuestionRequest) (*pb.Question, error) {
	by, project, err := s.agentStamp(ctx, req.SessionId, req.Reason)
	if err != nil {
		return nil, err
	}
	return s.change(ctx, req.Id, questionDeleted, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if q.State == pb.QuestionState_QUESTION_STATE_DELETED {
			return api.Errorf(api.CodeConflict, "question %d was deleted already, %s", q.Id, stamped(q.Deleted))
		}
		q.State, q.Deleted = pb.QuestionState_QUESTION_STATE_DELETED, by
		return nil
	})
}

func (s *questions) ListQuestions(ctx context.Context, req *pb.ListQuestionsRequest) (*pb.ListQuestionsResponse, error) {
	if req.Limit < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "limit %d: give 0 for all, or how many", req.Limit)
	}
	qs, total, err := s.cfg.Store.Questions(ctx, QuestionFilter{States: req.States, Order: req.Order, Project: req.Project, AskedBy: req.AskedBy, Limit: int(req.Limit)})
	if err != nil {
		return nil, err
	}
	return &pb.ListQuestionsResponse{Questions: qs, Total: int32(total)}, nil
}

func (s *questions) ListQuestionProjects(ctx context.Context, _ *pb.ListQuestionProjectsRequest) (*pb.ListQuestionProjectsResponse, error) {
	ps, err := s.cfg.Store.QuestionProjects(ctx)
	return &pb.ListQuestionProjectsResponse{Projects: ps}, err
}

func (s *questions) GetQuestion(ctx context.Context, req *pb.GetQuestionRequest) (*pb.Question, error) {
	q, err := s.cfg.Store.Question(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if q.History, err = s.cfg.Store.QuestionHistory(ctx, req.Id); err != nil {
		return nil, err
	}
	return q, nil
}

func (s *questions) AnswerQuestion(ctx context.Context, req *pb.AnswerQuestionRequest) (*pb.Question, error) {
	by, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	q, err := s.change(ctx, req.Id, questionAnswered, by, func(q *pb.Question) error {
		if err := mustBeOpen(q, "answered"); err != nil {
			return err
		}
		if err := checkAnswer(q, req.Answer); err != nil {
			return err
		}
		q.State, q.Answer, q.Answered = pb.QuestionState_QUESTION_STATE_ANSWERED, req.Answer, by
		return nil
	})
	if err == nil {
		s.sessionWakes.wake(q.Asked.GetSession())
	}
	return q, err
}

func (s *questions) DismissQuestion(ctx context.Context, req *pb.DismissQuestionRequest) (*pb.Question, error) {
	by, err := s.userStamp(ctx, req.SessionId, req.Reason)
	if err != nil {
		return nil, err
	}
	q, err := s.change(ctx, req.Id, questionDismissed, by, func(q *pb.Question) error {
		if err := mustBeOpen(q, "dismissed"); err != nil {
			return err
		}
		q.State, q.Dismissed = pb.QuestionState_QUESTION_STATE_DISMISSED, by
		return nil
	})
	if err == nil {
		s.sessionWakes.wake(q.Asked.GetSession())
	}
	return q, err
}

func (s *questions) MoveQuestion(ctx context.Context, req *pb.MoveQuestionRequest) (*pb.Question, error) {
	by, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	if req.Before == req.Id {
		return nil, api.Errorf(api.CodeInvalidArgs, "question %d cannot go before itself", req.Id)
	}
	q, err := s.cfg.Store.MoveQuestion(ctx, req.Id, req.Before, by, func(q, before *pb.Question) error {
		if err := mustBeOpen(q, "moved"); err != nil {
			return err
		}
		if before != nil && before.State != pb.QuestionState_QUESTION_STATE_OPEN {
			return api.Errorf(api.CodeConflict, "question %d is %s: only open questions have a place in the order", before.Id, stateWord(before.State))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.questionWakes.wake(anyQuestion)
	return q, nil
}

func (s *questions) AckQuestions(ctx context.Context, req *pb.AckQuestionsRequest) (*pb.AckQuestionsResponse, error) {
	if len(req.Ids) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "name the questions whose answers you read").WithSuggestion("hotbed question outbox")
	}
	by, project, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	qs, err := s.cfg.Store.ChangeQuestions(ctx, req.Ids, questionHandled, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if !inOutbox(q.State) {
			return api.Errorf(api.CodeConflict, "question %d is %s, not in the outbox: nothing was acknowledged", q.Id, stateWord(q.State)).
				WithSuggestion("hotbed question outbox lists what can be acknowledged")
		}
		q.State, q.Handled = pb.QuestionState_QUESTION_STATE_HANDLED, by
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.questionWakes.wake(anyQuestion)
	return &pb.AckQuestionsResponse{Questions: qs}, nil
}

// WaitQuestions returns once one of the named questions is no longer open,
// or, naming none, once the outbox holds something; or when its time is up.
func (s *questions) WaitQuestions(ctx context.Context, req *pb.WaitQuestionsRequest) (*pb.ListQuestionsResponse, error) {
	wait := maxWait
	if t := time.Duration(req.TimeoutSeconds) * time.Second; t > 0 && t < wait {
		wait = t
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woken := s.questionWakes.wait(anyQuestion)
		res, done, err := s.settled(ctx, req.Ids, req.Project, req.AskedBy)
		if err != nil || done {
			return res, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return res, nil
		case <-woken:
		}
	}
}

// settled is the named questions, done once one of them is not open; or,
// naming none, the outbox (of a project, and of one session's questions,
// when given), done once it holds something.
func (s *questions) settled(ctx context.Context, ids []int32, project, askedBy string) (*pb.ListQuestionsResponse, bool, error) {
	if len(ids) == 0 {
		qs, n, err := s.cfg.Store.Questions(ctx, QuestionFilter{States: outbox, Order: pb.QuestionOrder_QUESTION_ORDER_UPDATED, Project: project, AskedBy: askedBy})
		return &pb.ListQuestionsResponse{Questions: qs, Total: int32(n)}, n > 0, err
	}
	res := &pb.ListQuestionsResponse{Total: int32(len(ids))}
	done := false
	for _, id := range ids {
		q, err := s.cfg.Store.Question(ctx, id)
		if err != nil {
			return nil, false, err
		}
		res.Questions = append(res.Questions, q)
		done = done || q.State != pb.QuestionState_QUESTION_STATE_OPEN
	}
	return res, done, nil
}

// change makes one change to a question and wakes those waiting on
// questions.
func (s *questions) change(ctx context.Context, id int32, what string, by *pb.Stamp, edit func(*pb.Question) error) (*pb.Question, error) {
	q, err := s.cfg.Store.ChangeQuestion(ctx, id, what, by, edit)
	if err != nil {
		return nil, err
	}
	s.questionWakes.wake(anyQuestion)
	return q, nil
}

var outbox = []pb.QuestionState{pb.QuestionState_QUESTION_STATE_ANSWERED, pb.QuestionState_QUESTION_STATE_DISMISSED}

func inOutbox(st pb.QuestionState) bool { return slices.Contains(outbox, st) }

func mustBeOpen(q *pb.Question, done string) error {
	if q.State == pb.QuestionState_QUESTION_STATE_OPEN {
		return nil
	}
	return api.Errorf(api.CodeConflict, "question %d is %s; only an open question can be %s", q.Id, stateWord(q.State), done).
		WithSuggestion("hotbed question show " + fmt.Sprint(q.Id) + " says who changed it; ask a new question if it still needs asking")
}

// stateWord is a state as the CLI's flags name it: open, answered, ...
func stateWord(st pb.QuestionState) string {
	return strings.ToLower(strings.TrimPrefix(st.String(), "QUESTION_STATE_"))
}

// stamped says who made a change and when: "by brave-otter on ubu1 at 14:05".
func stamped(st *pb.Stamp) string {
	who := "by the user"
	if st.GetSession() != "" {
		who = "by " + st.Session
	}
	if st.GetHost() != "" {
		who += " on " + st.Host
	}
	return who + " at " + time.UnixMilli(st.GetAt()).Format(time.DateTime)
}

// checkPrompt holds a question to what AskUserQuestion takes.
func checkPrompt(p *pb.Prompt) *api.Error {
	switch {
	case p == nil || strings.TrimSpace(p.Question) == "":
		return api.Errorf(api.CodeInvalidArgs, "a question needs its question")
	case strings.TrimSpace(p.Header) == "":
		return api.Errorf(api.CodeInvalidArgs, "question %q needs a header, a short label for it", p.Question).WithSuggestion(`"header": "Auth method"`)
	case utf8.RuneCountInString(p.Header) > maxHeader:
		return api.Errorf(api.CodeInvalidArgs, "header %q is longer than %d characters", p.Header, maxHeader)
	case len(p.Options) < minOptions || len(p.Options) > maxOptions:
		return api.Errorf(api.CodeInvalidArgs, "question %q has %d options; give %d to %d (the user can always write their own answer)", p.Question, len(p.Options), minOptions, maxOptions)
	}
	var labels []string
	for _, o := range p.Options {
		switch label := strings.TrimSpace(o.Label); {
		case label == "":
			return api.Errorf(api.CodeInvalidArgs, "question %q has an option with no label", p.Question)
		case strings.TrimSpace(o.Description) == "":
			return api.Errorf(api.CodeInvalidArgs, "option %q needs a description: what picking it means", o.Label)
		case strings.EqualFold(label, "other"):
			return api.Errorf(api.CodeInvalidArgs, "leave out the %q option: the user can always write their own answer", o.Label)
		case slices.Contains(labels, label):
			return api.Errorf(api.CodeInvalidArgs, "question %q has two options labelled %q", p.Question, o.Label)
		default:
			labels = append(labels, label)
		}
	}
	return nil
}

// checkAnswer holds an answer to its question: picks among its options,
// one at most unless it is multi_select, or words of the user's own.
func checkAnswer(q *pb.Question, a *pb.Answer) error {
	if a == nil || (len(a.Picked) == 0 && strings.TrimSpace(a.Other) == "") {
		return api.Errorf(api.CodeInvalidArgs, "an answer picks an option or says something of its own")
	}
	if len(a.Picked) > 1 && !q.Prompt.MultiSelect {
		return api.Errorf(api.CodeInvalidArgs, "question %d takes one option; %d were picked", q.Id, len(a.Picked))
	}
	var labels []string
	for _, o := range q.Prompt.Options {
		labels = append(labels, o.Label)
	}
	for i, p := range a.Picked {
		if !slices.Contains(labels, p) {
			return api.Errorf(api.CodeInvalidArgs, "question %d has no option %q", q.Id, p).WithSuggestion("its options: " + strings.Join(labels, ", "))
		}
		if slices.Contains(a.Picked[:i], p) {
			return api.Errorf(api.CodeInvalidArgs, "option %q was picked twice", p)
		}
	}
	return nil
}
