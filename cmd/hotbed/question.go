package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// Questions for the user: one list on the controller, shared by every
// session. Agents add, edit, delete and acknowledge them as their session;
// the user answers, dismisses and orders them in the cockpit, and the same
// can be done here when an agent passes on what the user said.

type questionCmd struct {
	Add      questionAddCmd      `cmd:"" help:"Ask the user: AskUserQuestion's JSON ({\"questions\": [...]}) on stdin or --file. Each question is an entry of its own on the list of the session's project, which its sessions share; carry on with your best guess and read the answer later."`
	Ls       questionLsCmd       `cmd:"" help:"A project's questions, every session's (every state but deleted), newest first."`
	Show     questionShowCmd     `cmd:"" help:"A question with every change made to it: what, when, and by which session."`
	Edit     questionEditCmd     `cmd:"" help:"Rewrite an open question: one question's JSON on stdin or --file, bare or as add takes it."`
	Rm       questionRmCmd       `cmd:"" help:"Delete a question that is no longer needed, whichever session asked it; it stays in the history."`
	Outbox   questionOutboxCmd   `cmd:"" help:"A project's questions the user answered or dismissed that no agent has acknowledged yet, the newest answer first."`
	Projects questionProjectsCmd `cmd:"" help:"Every project with questions: how many wait on the user, and on agents."`
	Ack      questionAckCmd      `cmd:"" help:"Acknowledge answers you have read and acted on: they leave the outbox, handled."`
	Wait     questionWaitCmd     `cmd:"" help:"Wait (up to two minutes) until one of the named questions is answered, dismissed or deleted; naming none, until one of this session's questions is in the outbox."`
	Answer   questionAnswerCmd   `cmd:"" help:"Answer a question for the user. The user answers in the cockpit; use this only to pass on what the user said."`
	Dismiss  questionDismissCmd  `cmd:"" help:"Dismiss a question for the user: it will get no answer."`
	Move     questionMoveCmd     `cmd:"" help:"Move an open question in the user's order."`
}

// questionInput is where a question's JSON comes from.
type questionInput struct {
	File string `type:"existingfile" help:"Read the JSON from this file instead of stdin."`
}

func (in questionInput) read(d deps, example string) ([]byte, error) {
	var (
		b   []byte
		err error
	)
	if in.File != "" {
		b, err = os.ReadFile(in.File)
	} else {
		b, err = io.ReadAll(d.stdin)
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "no JSON on stdin").WithSuggestion(example)
	}
	return b, nil
}

// asked is AskUserQuestion's input: {"questions": [...]}.
func (in questionInput) asked(d deps) (*pb.AddQuestionsRequest, error) {
	b, err := in.read(d, askExample)
	if err != nil {
		return nil, err
	}
	return parseAsked(b)
}

func parseAsked(b []byte) (*pb.AddQuestionsRequest, error) {
	req := &pb.AddQuestionsRequest{}
	if err := protojson.Unmarshal(b, req); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "read the questions' JSON: %v", err).WithSuggestion(askExample)
	}
	if req.SessionId != "" || req.AskedFrom != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "the JSON names its session or where it was asked from; hotbed fills those in").WithSuggestion(askExample)
	}
	return req, nil
}

// prompt is one question: its JSON as in add's list, or AskUserQuestion's
// input holding just it.
func (in questionInput) prompt(d deps) (*pb.Prompt, error) {
	b, err := in.read(d, editExample)
	if err != nil {
		return nil, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "read the question's JSON: %v", err).WithSuggestion(editExample)
	}
	if _, wrapped := keys["questions"]; wrapped {
		req, err := parseAsked(b)
		if err != nil {
			return nil, err
		}
		if len(req.Questions) != 1 {
			return nil, api.Errorf(api.CodeInvalidArgs, "a question is edited into one question; the JSON has %d", len(req.Questions)).WithSuggestion(editExample)
		}
		return req.Questions[0], nil
	}
	p := &pb.Prompt{}
	if err := protojson.Unmarshal(b, p); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "read the question's JSON: %v", err).WithSuggestion(editExample)
	}
	return p, nil
}

const askExample = `hotbed question add <<'EOF'
{"questions": [{"question": "Which store should sessions use?", "header": "Store", "multiSelect": false,
  "options": [{"label": "SQLite (Recommended)", "description": "One file, already used"}, {"label": "Postgres", "description": "A server to run"}]}]}
EOF`

const editExample = `hotbed question edit 12 <<'EOF'
{"question": "Which store should sessions use?", "header": "Store",
 "options": [{"label": "SQLite", "description": "One file"}, {"label": "Postgres", "description": "A server"}]}
EOF`

// questionID is the argument of the commands that act on one question.
type questionID struct {
	ID int32 `arg:"" help:"The question's number."`
}

// named is the session of a change the user makes, which an agent passing
// it on names with -s or HOTBED_SESSION; none otherwise. A session merely
// bound to this thread or worktree is not used: it may long have ended, and
// the change is the user's.
func (f sessionFlag) named() string {
	return strings.TrimSpace(f.Session)
}

type questionAddCmd struct {
	controllerClient
	sessionFlag
	questionInput
	Repo string `help:"The repo of the session's project they are about, as a git remote (default: this checkout's, if the project holds it, or else the project as a whole)."`
}

func (c *questionAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	req, err := c.asked(d)
	if err != nil {
		return nil, err
	}
	req.SessionId, req.Repo = session, c.Repo
	if req.AskedFrom, err = askedFrom(ctx); err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.questions.AddQuestions(ctx, req)
}

// dirtyShown is how many lines of git status a question keeps.
const dirtyShown = 20

// askedFrom is where this agent asks from: its directory and, in a git
// checkout, the checkout's top, origin, branch, commit and changed files.
func askedFrom(ctx context.Context) (*pb.AskedFrom, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	from := &pb.AskedFrom{Dir: dir}
	g := gitops.Git{Dir: dir}
	root, err := g.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return from, nil // not in a checkout: the directory is all there is
	}
	from.Dir = root
	g.Dir = root
	// Each of these may well be missing: a checkout with no origin, a
	// detached HEAD, a branch with no commit yet.
	from.Remote, _ = g.Run(ctx, "remote", "get-url", "origin")
	from.Branch, _ = g.Run(ctx, "branch", "--show-current")
	from.Head, _ = g.Run(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
	dirty, err := g.Lines(ctx, "status", "--porcelain=v1")
	if err != nil {
		return nil, err
	}
	from.DirtyFiles = int32(len(dirty))
	from.Dirty = dirty[:min(len(dirty), dirtyShown)]
	return from, nil
}

type questionLsCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	AllProjects bool     `help:"Every project's questions."`
	State       []string `enum:"open,answered,dismissed,handled,deleted,all" help:"Only questions in this state: open, answered, dismissed, handled, deleted, or all. Repeatable (default: every state but deleted)."`
	Sort        string   `enum:"newest,oldest,updated,rank" default:"newest" help:"newest or oldest asked first, the most recently changed (updated) first, or the user's order (rank)."`
	AskedBy     string   `help:"Only the questions this session asked."`
	Limit       int32    `help:"At most this many (default: all)."`
}

func (c *questionLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	project, err := c.listed(ctx, d, cl, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	return cl.questions.ListQuestions(ctx, &pb.ListQuestionsRequest{
		States: questionStates(c.State), Order: questionOrder(c.Sort), Project: project, AskedBy: c.AskedBy, Limit: c.Limit,
	})
}

// questionStates are the states the CLI names; all is every one.
func questionStates(words []string) []pb.QuestionState {
	var out []pb.QuestionState
	for _, w := range words {
		if w == "all" {
			return []pb.QuestionState{
				pb.QuestionState_QUESTION_STATE_OPEN, pb.QuestionState_QUESTION_STATE_ANSWERED, pb.QuestionState_QUESTION_STATE_DISMISSED,
				pb.QuestionState_QUESTION_STATE_HANDLED, pb.QuestionState_QUESTION_STATE_DELETED,
			}
		}
		out = append(out, pb.QuestionState(pb.QuestionState_value["QUESTION_STATE_"+strings.ToUpper(w)]))
	}
	return out
}

func questionOrder(word string) pb.QuestionOrder {
	return pb.QuestionOrder(pb.QuestionOrder_value["QUESTION_ORDER_"+strings.ToUpper(word)])
}

type questionShowCmd struct {
	controllerClient
	questionID
}

func (c *questionShowCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.questions.GetQuestion(ctx, &pb.GetQuestionRequest{Id: c.ID})
}

type questionEditCmd struct {
	controllerClient
	sessionFlag
	questionID
	questionInput
}

func (c *questionEditCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	prompt, err := c.prompt(d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.questions.EditQuestion(ctx, &pb.EditQuestionRequest{Id: c.ID, Prompt: prompt, SessionId: session})
}

type questionRmCmd struct {
	controllerClient
	sessionFlag
	questionID
	Reason string `help:"Why it is no longer needed."`
}

func (c *questionRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.questions.DeleteQuestion(ctx, &pb.DeleteQuestionRequest{Id: c.ID, SessionId: session, Reason: c.Reason})
}

type questionOutboxCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	AllProjects bool   `help:"Every project's outbox."`
	AskedBy     string `help:"Only the questions this session asked."`
}

func (c *questionOutboxCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	project, err := c.listed(ctx, d, cl, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	return cl.questions.ListQuestions(ctx, &pb.ListQuestionsRequest{
		States: questionStates([]string{"answered", "dismissed"}), Order: pb.QuestionOrder_QUESTION_ORDER_UPDATED, Project: project, AskedBy: c.AskedBy,
	})
}

type questionAckCmd struct {
	controllerClient
	sessionFlag
	IDs []int32 `arg:"" name:"id" help:"The questions whose answers you read."`
}

func (c *questionAckCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.questions.AckQuestions(ctx, &pb.AckQuestionsRequest{Ids: c.IDs, SessionId: session})
}

type questionWaitCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	ProjectWide bool    `help:"Naming no questions, wait for any answer in the project's outbox, not only to this session's questions."`
	AllProjects bool    `help:"With --project-wide, any project's outbox."`
	IDs         []int32 `arg:"" optional:"" name:"id" help:"The questions to wait on (default: wait for an answer to one of this session's questions)."`
	Timeout     int32   `default:"120" help:"Seconds to wait, at most 120."`
}

func (c *questionWaitCmd) Exec(ctx context.Context, d deps) (any, error) {
	stream, err := c.clients(d.stream)
	if err != nil {
		return nil, err
	}
	req := &pb.WaitQuestionsRequest{Ids: c.IDs, TimeoutSeconds: c.Timeout}
	switch {
	case len(c.IDs) > 0:
	case c.ProjectWide:
		if req.Project, err = c.listed(ctx, d, stream, c.sessionFlag, c.AllProjects); err != nil {
			return nil, err
		}
	default:
		// An answer left in the outbox by another session, which may never
		// ack it, would end every wait at once: wait for this session's.
		if req.AskedBy, err = c.resolve(ctx, d); err != nil {
			return nil, err
		}
	}
	return stream.questions.WaitQuestions(ctx, req)
}

type questionAnswerCmd struct {
	controllerClient
	sessionFlag
	questionID
	Pick  []string `help:"The label of an option the user picked. Repeatable for a multiSelect question."`
	Other string   `help:"What the user said instead of picking, or besides."`
	Notes string   `help:"The user's note on the answer."`
}

func (c *questionAnswerCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.questions.AnswerQuestion(ctx, &pb.AnswerQuestionRequest{
		Id: c.ID, Answer: &pb.Answer{Picked: c.Pick, Other: c.Other, Notes: c.Notes}, SessionId: c.named(),
	})
}

type questionDismissCmd struct {
	controllerClient
	sessionFlag
	questionID
	Reason string `help:"Why it gets no answer."`
}

func (c *questionDismissCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.questions.DismissQuestion(ctx, &pb.DismissQuestionRequest{Id: c.ID, Reason: c.Reason, SessionId: c.named()})
}

type questionMoveCmd struct {
	controllerClient
	sessionFlag
	questionID
	Before int32 `help:"Put it just before this open question." xor:"place"`
	First  bool  `help:"Put it first." xor:"place"`
	Last   bool  `help:"Put it last." xor:"place"`
}

func (c *questionMoveCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	before := c.Before
	switch {
	case c.First:
		// First of its project's list, as the user sees it.
		q, err := cl.questions.GetQuestion(ctx, &pb.GetQuestionRequest{Id: c.ID})
		if err != nil {
			return nil, err
		}
		top, err := cl.questions.ListQuestions(ctx, &pb.ListQuestionsRequest{
			States: questionStates([]string{"open"}), Order: pb.QuestionOrder_QUESTION_ORDER_RANK, Project: q.Project, Limit: 1,
		})
		if err != nil {
			return nil, err
		}
		if len(top.Questions) == 0 {
			return nil, api.Errorf(api.CodeConflict, "no question is open")
		}
		if before = top.Questions[0].Id; before == c.ID {
			return top.Questions[0], nil // first already
		}
	case c.Last:
		before = 0
	case before == 0:
		return nil, api.Errorf(api.CodeInvalidArgs, "say where: --before ID, --first or --last")
	}
	return cl.questions.MoveQuestion(ctx, &pb.MoveQuestionRequest{Id: c.ID, Before: before, SessionId: c.named()})
}

type questionProjectsCmd struct {
	controllerClient
}

func (c *questionProjectsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.questions.ListQuestionProjects(ctx, &pb.ListQuestionProjectsRequest{})
}
