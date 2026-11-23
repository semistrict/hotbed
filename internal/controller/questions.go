package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

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
)

func (s *questions) AddQuestions(ctx context.Context, req *pb.AddQuestionsRequest) (*pb.AddQuestionsResponse, error) {
	if n := len(req.Questions); n == 0 || n > maxAsked {
		return nil, api.Errorf(api.CodeInvalidArgs, "ask one to %d questions at a time, as AskUserQuestion does; got %d", maxAsked, n).
			WithSuggestion(`{"questions": [{"question": "Which ...?", "header": "Short", "options": [{"label": "A", "description": "..."}, {"label": "B", "description": "..."}], "multiSelect": false}]}`)
	}
	var decided []*pb.Answer
	for i, p := range req.Questions {
		err := settlePrompt(p)
		if err == nil && req.Answered {
			var a *pb.Answer
			if a, err = decision(p); err == nil {
				decided = append(decided, a)
			}
		}
		if err != nil {
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
	qs, err := s.cfg.Store.AddQuestions(ctx, project.Name, repo, req.Questions, decided, by, req.AskedFrom)
	if err != nil {
		return nil, err
	}
	return &pb.AddQuestionsResponse{Questions: qs}, nil
}

// decision is the answer a decision already taken has: the options its
// agent marked implemented, which must be an answer the user could give.
func decision(p *pb.Prompt) (*pb.Answer, *api.Error) {
	a := &pb.Answer{}
	for _, o := range p.Options {
		if o.Implemented {
			a.Picked = append(a.Picked, o.Label)
		}
	}
	if len(a.Picked) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "question %q is answered, but no option is implemented: mark what was decided", p.Question).
			WithSuggestion(`{"label": "What was decided", "description": "...", "implemented": true}`)
	}
	return a, checkAnswer(&pb.Question{Prompt: p}, a)
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

func checkQuestionRevision(q *pb.Question, want int64) error {
	if want > 0 && q.Revision != want {
		return api.Errorf(api.CodeConflict, "question %d is at revision %d, not %d", q.Id, q.Revision, want).
			WithSuggestion(fmt.Sprintf("hotbed question show %d", q.Id))
	}
	return nil
}

func (s *questions) EditQuestion(ctx context.Context, req *pb.EditQuestionRequest) (*pb.Question, error) {
	if err := settlePrompt(req.Prompt); err != nil {
		return nil, err
	}
	by, project, err := s.agentStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	return s.cfg.Store.ChangeQuestion(ctx, req.Id, questionEdited, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if err := checkQuestionRevision(q, req.IfRevision); err != nil {
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
	return s.cfg.Store.ChangeQuestion(ctx, req.Id, questionDeleted, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if err := checkQuestionRevision(q, req.IfRevision); err != nil {
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
	project, err := s.listedProject(ctx, req)
	if err != nil {
		return nil, err
	}
	qs, total, err := s.cfg.Store.Questions(ctx, QuestionFilter{States: req.States, Order: req.Order, Project: project, AskedBy: req.AskedBy, Limit: int(req.Limit)})
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
	by, project, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	q, err := s.cfg.Store.ChangeQuestion(ctx, req.Id, questionAnswered, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if err := checkQuestionRevision(q, req.IfRevision); err != nil {
			return err
		}
		if err := mustBeAnswerable(q, "answered"); err != nil {
			return err
		}
		if err := checkAnswer(q, req.Answer); err != nil {
			return err
		}
		q.State, q.Answer, q.Answered, q.Dismissed = pb.QuestionState_QUESTION_STATE_ANSWERED, req.Answer, by, nil
		return nil
	})
	if err == nil {
		s.sessionWakes.wake(q.Asked.GetSession())
	}
	return q, err
}

func (s *questions) DismissQuestion(ctx context.Context, req *pb.DismissQuestionRequest) (*pb.Question, error) {
	by, project, err := s.userStamp(ctx, req.SessionId, req.Reason)
	if err != nil {
		return nil, err
	}
	q, err := s.cfg.Store.ChangeQuestion(ctx, req.Id, questionDismissed, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if err := checkQuestionRevision(q, req.IfRevision); err != nil {
			return err
		}
		if err := mustBeAnswerable(q, "dismissed"); err != nil {
			return err
		}
		q.State, q.Dismissed, q.Answer, q.Answered = pb.QuestionState_QUESTION_STATE_DISMISSED, by, nil, nil
		return nil
	})
	if err == nil {
		s.sessionWakes.wake(q.Asked.GetSession())
	}
	return q, err
}

func (s *questions) ReviseQuestion(ctx context.Context, req *pb.ReviseQuestionRequest) (*pb.Question, error) {
	if err := settlePrompt(req.Prompt); err != nil {
		return nil, err
	}
	by, project, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	return s.cfg.Store.ChangeQuestion(ctx, req.Id, questionEdited, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if err := checkQuestionRevision(q, req.IfRevision); err != nil {
			return err
		}
		if q.State == pb.QuestionState_QUESTION_STATE_DELETED {
			return api.Errorf(api.CodeConflict, "question %d was deleted %s; ask it again if it still needs asking", q.Id, stamped(q.Deleted))
		}
		revised := proto.Clone(q).(*pb.Question)
		revised.Prompt = req.Prompt
		if q.Answer != nil {
			if err := checkAnswer(revised, q.Answer); err != nil {
				return api.Errorf(api.CodeConflict, "question %d's answer no longer fits it: %s", q.Id, err.Message).
					WithSuggestion(fmt.Sprintf("keep the options it picks, or reopen it first: hotbed question reopen %d", q.Id))
			}
		}
		q.Prompt, q.Edited = req.Prompt, by
		return nil
	})
}

func (s *questions) ReopenQuestion(ctx context.Context, req *pb.ReopenQuestionRequest) (*pb.Question, error) {
	by, project, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	q, err := s.cfg.Store.ReopenQuestion(ctx, req.Id, by, func(q *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		switch q.State {
		case pb.QuestionState_QUESTION_STATE_OPEN:
			return api.Errorf(api.CodeConflict, "question %d is open already", q.Id)
		case pb.QuestionState_QUESTION_STATE_DELETED:
			return api.Errorf(api.CodeConflict, "question %d was deleted %s; ask it again if it still needs asking", q.Id, stamped(q.Deleted))
		}
		q.State, q.Answer, q.Answered, q.Dismissed, q.Handled = pb.QuestionState_QUESTION_STATE_OPEN, nil, nil, nil, nil
		return nil
	})
	if err != nil {
		return nil, err
	}
	return q, nil
}

func (s *questions) MoveQuestion(ctx context.Context, req *pb.MoveQuestionRequest) (*pb.Question, error) {
	by, project, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	var place questionPlace
	switch p := req.Place.(type) {
	case *pb.MoveQuestionRequest_Before:
		if p.Before == req.Id {
			return nil, api.Errorf(api.CodeInvalidArgs, "question %d cannot go before itself", req.Id)
		}
		place.before = p.Before
	case *pb.MoveQuestionRequest_First:
		place.first = p.First
	case *pb.MoveQuestionRequest_Last:
	}
	if place == (questionPlace{}) && !req.GetLast() {
		return nil, api.Errorf(api.CodeInvalidArgs, "say where question %d goes: before another, first or last", req.Id)
	}
	q, err := s.cfg.Store.MoveQuestion(ctx, req.Id, place, by, func(q, before *pb.Question) error {
		if err := inProject("question", q.Id, q.Project, project, by); err != nil {
			return err
		}
		if err := mustBeOpen(q, "moved"); err != nil {
			return err
		}
		if before != nil && before.Project != q.Project {
			return api.Errorf(api.CodeInvalidArgs, "question %d is in project %s, not %s: move questions within their own project", before.Id, before.Project, q.Project)
		}
		if before != nil && before.State != pb.QuestionState_QUESTION_STATE_OPEN {
			return api.Errorf(api.CodeConflict, "question %d is %s: only open questions have a place in the order", before.Id, stateWord(before.State))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
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
	return &pb.AckQuestionsResponse{Questions: qs}, nil
}

var outbox = []pb.QuestionState{pb.QuestionState_QUESTION_STATE_ANSWERED, pb.QuestionState_QUESTION_STATE_DISMISSED}

func inOutbox(st pb.QuestionState) bool { return slices.Contains(outbox, st) }

// mustBeAnswerable holds an answer or dismissal to a question the user may
// still respond to: open, or in the outbox, where no agent has acted on
// the response yet.
func mustBeAnswerable(q *pb.Question, done string) error {
	if q.State == pb.QuestionState_QUESTION_STATE_OPEN || inOutbox(q.State) {
		return nil
	}
	return api.Errorf(api.CodeConflict, "question %d is %s; only an open question, or one in the outbox, can be %s", q.Id, stateWord(q.State), done).
		WithSuggestion(fmt.Sprintf("reopen it first: hotbed question reopen %d", q.Id))
}

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

// settlePrompt holds a question to what AskUserQuestion takes, and to its
// marks. Option labels are delivered verbatim; callers mark recommended
// options explicitly with recommended: true.
func settlePrompt(p *pb.Prompt) *api.Error {
	if p == nil || strings.TrimSpace(p.Question) == "" {
		return api.Errorf(api.CodeInvalidArgs, "a question needs its question, a short title; put the detail in its body")
	}
	return checkPrompt(p)
}

// checkPrompt holds a settled question to what AskUserQuestion takes, and
// its options' marks to what they mean: an option is built now or
// recommended, not both, and a question that takes one option marks one of
// each at most.
func checkPrompt(p *pb.Prompt) *api.Error {
	switch {
	case strings.TrimSpace(p.Header) == "":
		return api.Errorf(api.CodeInvalidArgs, "question %q needs a header, a short label for it", p.Question).WithSuggestion(`"header": "Auth method"`)
	case utf8.RuneCountInString(p.Header) > maxHeader:
		return api.Errorf(api.CodeInvalidArgs, "header %q is longer than %d characters", p.Header, maxHeader)
	case len(p.Options) < minOptions || len(p.Options) > maxOptions:
		return api.Errorf(api.CodeInvalidArgs, "question %q has %d options; give %d to %d (the user can always write their own answer)", p.Question, len(p.Options), minOptions, maxOptions)
	}
	var labels, implemented, recommended []string
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
		case o.Implemented && o.Recommended:
			return api.Errorf(api.CodeInvalidArgs, "option %q is both implemented and recommended: recommended is for a way forward not built yet", o.Label).
				WithSuggestion(`keep "implemented": true, and mark another option recommended only if you would change to it`)
		default:
			labels = append(labels, label)
		}
		if o.Implemented {
			implemented = append(implemented, o.Label)
		}
		if o.Recommended {
			recommended = append(recommended, o.Label)
		}
	}
	if !p.MultiSelect {
		for _, marked := range []struct {
			mark   string
			labels []string
		}{{"implemented", implemented}, {"recommended", recommended}} {
			if len(marked.labels) > 1 {
				return api.Errorf(api.CodeInvalidArgs, "question %q takes one option, but %d are %s: %s", p.Question, len(marked.labels), marked.mark, strings.Join(marked.labels, ", ")).
					WithSuggestion(`mark one, or set "multiSelect": true if several go together`)
			}
		}
	}
	return nil
}

// checkAnswer holds an answer to its question: picks among its options,
// one at most unless it is multi_select, or words of the user's own.
func checkAnswer(q *pb.Question, a *pb.Answer) *api.Error {
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
