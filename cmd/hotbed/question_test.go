package main

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// storeQuestion is AskUserQuestion's input with one question, as an agent
// would pass that tool.
const storeQuestion = `{"questions": [{
  "question": "Which store should sessions use?",
  "header": "Store",
  "multiSelect": false,
  "options": [
    {"label": "SQLite", "description": "One file, already used", "recommended": true},
    {"label": "Postgres", "description": "A server to run", "preview": "docker run postgres"}
  ]
}]}`

// twoQuestions is AskUserQuestion's input with two questions, one of them
// multiSelect.
const twoQuestions = `{"questions": [
  {"question": "Which platforms ship?", "header": "Platforms", "multiSelect": true,
   "options": [{"label": "linux", "description": "x"}, {"label": "darwin", "description": "y"}, {"label": "windows", "description": "z"}]},
  {"question": "Keep the old flag?", "header": "Flag",
   "options": [{"label": "Keep", "description": "a"}, {"label": "Drop", "description": "b"}]}
]}`

// questionCluster is a cluster with two agents' sessions, brave-otter and
// calm-heron, and the user's own client: the cockpit's calls, which name
// no session.
func questionCluster(t *testing.T) (*cluster, hotbedv1connect.QuestionServiceClient) {
	t.Helper()
	// newCluster leaves us outside any checkout, so the sessions bind to
	// nothing: a command names its session, or has none.
	c := newCluster(t)
	c.ok(nil, "session", "start", "brave-otter")
	c.ok(nil, "session", "start", "calm-heron")
	addr, err := c.cli.controller()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := api.Dial(addr, api.DefaultPort, c.cli.http)
	if err != nil {
		t.Fatal(err)
	}
	return c, hotbedv1connect.NewQuestionServiceClient(conn)
}

func (c *cluster) ask(session, input string) []*pb.Question {
	c.t.Helper()
	res := &pb.AddQuestionsResponse{}
	c.okIn(input, res, "question", "add", "-s", session)
	return res.Questions
}

// listed is the IDs hotbed question ls lists with args.
func (c *cluster) listed(args ...string) []int32 {
	c.t.Helper()
	res := &pb.ListQuestionsResponse{}
	c.ok(res, append([]string{"question", "ls"}, args...)...)
	return ids(res.Questions)
}

func (c *cluster) question(id string) *pb.Question {
	c.t.Helper()
	q := &pb.Question{}
	c.ok(q, "question", "show", id)
	return q
}

func ids(qs []*pb.Question) []int32 {
	out := []int32{}
	for _, q := range qs {
		out = append(out, q.Id)
	}
	return out
}

// whats is a question's history: each change and the session that made it
// ("user" for none).
func whats(q *pb.Question) []string {
	var out []string
	for _, ch := range q.History {
		who := ch.By.Session
		if who == "" {
			who = "user"
		}
		out = append(out, ch.What+" by "+who)
	}
	return out
}

// Questions are one list, every session's: each of AskUserQuestion's
// questions is an entry of its own, listed newest first by default, and in
// any other order on request.
func TestQuestionsAreOneListForEverySession(t *testing.T) {
	c, _ := questionCluster(t)
	asked := c.ask("brave-otter", twoQuestions)
	if got := ids(asked); !slices.Equal(got, []int32{1, 2}) {
		t.Fatalf("add made questions %v, want [1 2]", got)
	}
	q := asked[0]
	if q.State != pb.QuestionState_QUESTION_STATE_OPEN || q.Prompt.Header != "Platforms" || !q.Prompt.MultiSelect ||
		len(q.Prompt.Options) != 3 || q.Asked.Session != "brave-otter" || q.Asked.Host != "laptop" || q.Asked.At == 0 {
		t.Fatalf("question 1 = %v", q)
	}
	c.clock.advance(time.Minute)
	c.ask("calm-heron", storeQuestion)

	if got := c.listed(); !slices.Equal(got, []int32{3, 2, 1}) {
		t.Errorf("ls = %v, want every session's, newest first: [3 2 1]", got)
	}
	if got := c.listed("--sort", "oldest"); !slices.Equal(got, []int32{1, 2, 3}) {
		t.Errorf("ls --sort oldest = %v", got)
	}
	// New questions go to the top of the user's order, in the order asked.
	if got := c.listed("--sort", "rank"); !slices.Equal(got, []int32{3, 1, 2}) {
		t.Errorf("ls --sort rank = %v", got)
	}
	if got := c.listed("--asked-by", "brave-otter"); !slices.Equal(got, []int32{2, 1}) {
		t.Errorf("ls --asked-by brave-otter = %v", got)
	}
	res := &pb.ListQuestionsResponse{}
	c.ok(res, "question", "ls", "--limit", "1")
	if !slices.Equal(ids(res.Questions), []int32{3}) || res.Total != 3 {
		t.Errorf("ls --limit 1 = %v of %d", ids(res.Questions), res.Total)
	}
	store := c.question("3")
	if p := store.Prompt.Options[1]; p.Label != "Postgres" || p.Preview != "docker run postgres" {
		t.Errorf("question 3's second option = %v", p)
	}
}

// A question keeps where its agent asked from: the checkout's top, origin,
// branch, commit and what was changed in it.
func TestQuestionKeepsWhereItWasAsked(t *testing.T) {
	c, _ := questionCluster(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "work")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-qm", "first")
	gitIn(t, dir, "remote", "add", "origin", "https://example.invalid/app.git")
	write(t, filepath.Join(dir, "a.txt"), "b\n")
	write(t, filepath.Join(dir, "new.txt"), "new\n")
	t.Chdir(dir)

	// The project holds the checkout's repo, so the question is about it.
	c.ok(nil, "project", "add", "test", "--repo", "https://example.invalid/app.git")
	q := c.ask("brave-otter", storeQuestion)[0]
	if q.Project != "test" || q.Repo != "example.invalid/app" {
		t.Errorf("question about project %q, repo %q; want test, example.invalid/app", q.Project, q.Repo)
	}
	from := q.AskedFrom
	want := &pb.AskedFrom{
		Dir: gitIn(t, dir, "rev-parse", "--show-toplevel"), Remote: "https://example.invalid/app.git", Branch: "work",
		Head: gitIn(t, dir, "rev-parse", "HEAD"), DirtyFiles: 2, Dirty: []string{" M a.txt", "?? new.txt"},
	}
	if from.Dir != want.Dir || from.Remote != want.Remote || from.Branch != want.Branch || from.Head != want.Head ||
		from.DirtyFiles != want.DirtyFiles || !slices.Equal(from.Dirty, want.Dirty) {
		t.Errorf("asked from %v, want %v", from, want)
	}

	// Outside a checkout, only the directory.
	out := t.TempDir()
	t.Chdir(out)
	q = c.ask("brave-otter", storeQuestion)[0]
	if from := q.AskedFrom; from.Dir != out || from.Head != "" || from.DirtyFiles != 0 {
		t.Errorf("asked from outside a checkout: %v, want only the directory %s", from, out)
	}
	if q.Repo != "" {
		t.Errorf("question asked outside a checkout is about repo %q, want the whole project", q.Repo)
	}
	res := &pb.AddQuestionsResponse{}
	c.okIn(storeQuestion, res, "question", "add", "-s", "brave-otter", "--repo", "git@example.invalid:app.git")
	if res.Questions[0].Repo != "example.invalid/app" {
		t.Errorf("add --repo: repo %q", res.Questions[0].Repo)
	}
	if _, e := c.failsIn(storeQuestion, "question", "add", "-s", "brave-otter", "--repo", "github.com/o/elsewhere"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "does not hold github.com/o/elsewhere") {
		t.Errorf("add --repo of another project = %v, want INVALID_ARGS", e)
	}
}

// A question has a title and, if it needs one, a body. Its options say
// which the agent went ahead with (implemented) and which it would go on
// to (recommended). A question that takes one option marks one of each at
// most, and no option is both.
func TestQuestionsMarkTheImplementedAndRecommendedOptions(t *testing.T) {
	c, _ := questionCluster(t)
	asked := c.ask("brave-otter", `{"questions": [{
	  "question": "Keep the README guard on /work?", "header": "/work guard",
	  "body": "A test removed the empty `+"`/work`"+` on a live worker.\n\n- the file stops `+"`rmdir`"+`\n- a bind mount stops `+"`rm -rf`"+` too",
	  "options": [
	    {"label": "Keep the file", "description": "Stops **rmdir**; nothing to set up.", "implemented": true},
	    {"label": "Bind-mount /work", "description": "Set up again after every reboot.", "recommended": true}
	  ]}]}`)
	p := asked[0].Prompt
	if !strings.HasPrefix(p.Body, "A test removed the empty `/work`") || !strings.Contains(p.Body, "- a bind mount") {
		t.Errorf("body = %q", p.Body)
	}
	if o := p.Options[0]; o.Label != "Keep the file" || !o.Implemented || o.Recommended {
		t.Errorf("first option = %v, want implemented", o)
	}
	if o := p.Options[1]; o.Label != "Bind-mount /work" || o.Implemented || !o.Recommended {
		t.Errorf("second option = %v, want recommended", o)
	}

	// Several of each go together only where the user picks several.
	c.ask("brave-otter", `{"questions": [{"question": "Which platforms ship?", "header": "Platforms", "multiSelect": true,
	  "options": [{"label": "linux", "description": "x", "implemented": true}, {"label": "darwin", "description": "y", "implemented": true},
	              {"label": "windows", "description": "z", "recommended": true}, {"label": "freebsd", "description": "w", "recommended": true}]}]}`)

	for _, tc := range []struct {
		name, options, says string
	}{
		{"both on one", `{"label": "A", "description": "a", "implemented": true, "recommended": true}, {"label": "B", "description": "b"}`,
			`option "A" is both implemented and recommended`},
		{"two implemented", `{"label": "A", "description": "a", "implemented": true}, {"label": "B", "description": "b", "implemented": true}`,
			"takes one option, but 2 are implemented: A, B"},
		{"two recommended", `{"label": "A", "description": "a", "recommended": true}, {"label": "B", "description": "b", "recommended": true}`,
			"takes one option, but 2 are recommended: A, B"},
		{"empty label", `{"label": "   ", "description": "a"}, {"label": "B", "description": "b"}`,
			"has an option with no label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := `{"questions": [{"question": "Which?", "header": "Pick", "options": [` + tc.options + `]}]}`
			if _, e := c.failsIn(in, "question", "add", "-s", "brave-otter"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, tc.says) {
				t.Errorf("add = %v, want INVALID_ARGS saying %q", e, tc.says)
			}
		})
	}

	// An edit is held to the same marks.
	edited := &pb.Question{}
	c.okIn(`{"question": "Keep the README guard?", "header": "/work guard", "options": [
	  {"label": "Keep the file", "description": "a", "implemented": true}, {"label": "Bind-mount", "description": "b", "recommended": true}]}`,
		edited, "question", "edit", "-s", "calm-heron", "1")
	if o := edited.Prompt.Options[1]; o.Label != "Bind-mount" || !o.Recommended || edited.Prompt.Body != "" {
		t.Errorf("edited = %v", edited.Prompt)
	}
	if _, e := c.failsIn(`{"question": "Keep it?", "header": "/work guard", "options": [
	  {"label": "A", "description": "a", "implemented": true}, {"label": "B", "description": "b", "implemented": true}]}`,
		"question", "edit", "-s", "calm-heron", "1"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "2 are implemented") {
		t.Errorf("edit to two implemented = %v, want INVALID_ARGS", e)
	}
}

// Option labels containing "(Recommended)" are delivered verbatim without
// stripping or mutating Recommended: callers mark recommended options
// explicitly with "recommended": true (board 7876).
func TestQuestionRecommendedLabelIsPreservedVerbatim(t *testing.T) {
	c, _ := questionCluster(t)
	asked := c.ask("brave-otter", `{"questions": [{
	  "question": "Which store should sessions use?", "header": "Store",
	  "options": [
	    {"label": "SQLite (Recommended)", "description": "One file, already used"},
	    {"label": "Postgres (recommended)", "description": "A server to run", "implemented": true}
	  ]}]}`)
	p := asked[0].Prompt
	if o := p.Options[0]; o.Label != "SQLite (Recommended)" || o.Recommended {
		t.Errorf("option 0 = %+v, want label preserved verbatim with recommended=false", o)
	}
	if o := p.Options[1]; o.Label != "Postgres (recommended)" || !o.Implemented || o.Recommended {
		t.Errorf("option 1 = %+v, want label preserved verbatim with implemented=true, recommended=false", o)
	}

	edited := &pb.Question{}
	c.okIn(`{"question": "Which store?", "header": "Store", "options": [
	  {"label": "SQLite (Recommended)", "description": "Updated"},
	  {"label": "Postgres", "description": "A server"}]}`,
		edited, "question", "edit", "-s", "calm-heron", "1")
	if o := edited.Prompt.Options[0]; o.Label != "SQLite (Recommended)" || o.Recommended {
		t.Errorf("edited option 0 = %+v, want label preserved verbatim with recommended=false", o)
	}
}

// The user answers; the answer waits in the outbox, the asking session's
// hotbed poll hears of it, and any agent that acts on it acknowledges it,
// which makes it handled. Every step is kept, with the session that took it.
func TestAnsweredQuestionGoesThroughTheOutbox(t *testing.T) {
	c, user := questionCluster(t)
	c.ask("brave-otter", storeQuestion)
	c.ok(nil, "poll", "-s", "brave-otter", "--timeout", "1") // past what happened so far

	q, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Picked: []string{"SQLite"}, Notes: "keep it simple"}})
	if err != nil {
		t.Fatal(err)
	}
	if q.State != pb.QuestionState_QUESTION_STATE_ANSWERED || q.Answered.Session != "" || q.Answered.Host != "laptop" {
		t.Fatalf("answered question = %v", q)
	}
	outbox := &pb.ListQuestionsResponse{}
	c.ok(outbox, "question", "outbox")
	if !slices.Equal(ids(outbox.Questions), []int32{1}) || outbox.Questions[0].Answer.Picked[0] != "SQLite" {
		t.Fatalf("outbox = %v", outbox)
	}

	waited := &pb.SessionEvents{}
	c.ok(waited, "poll", "-s", "brave-otter", "--timeout", "5")
	if len(waited.Events) != 1 {
		t.Fatalf("brave-otter's poll = %v, want the answer to its question", waited.Events)
	}
	if q := waited.Events[0].GetQuestion(); q.GetWhat() != pb.QuestionUpdate_WHAT_ANSWERED || q.GetQuestion().GetId() != 1 ||
		!slices.Equal(q.GetQuestion().GetAnswer().GetPicked(), []string{"SQLite"}) || waited.Events[0].Detail != "question 1 (Store) answered: SQLite" {
		t.Fatalf("brave-otter's wait = %v, want the answer to its question", waited.Events)
	}

	// Another session may act on it: the list is everyone's.
	acked := &pb.AckQuestionsResponse{}
	c.ok(acked, "question", "ack", "-s", "calm-heron", "1")
	if q := acked.Questions[0]; q.State != pb.QuestionState_QUESTION_STATE_HANDLED || q.Handled.Session != "calm-heron" {
		t.Fatalf("acked = %v", q)
	}
	c.ok(outbox, "question", "outbox")
	if len(outbox.Questions) != 0 {
		t.Errorf("outbox after ack = %v", ids(outbox.Questions))
	}
	// Handled, nothing is lost: its answer stays, and so does each step.
	if got := c.listed("--state", "handled"); !slices.Equal(got, []int32{1}) {
		t.Errorf("ls --state handled = %v", got)
	}
	shown := c.question("1")
	if shown.Answer.GetNotes() != "keep it simple" {
		t.Errorf("handled question's answer = %v", shown.Answer)
	}
	if got, want := whats(shown), []string{"asked by brave-otter", "answered by user", "handled by calm-heron"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	if h := shown.History[0]; h.Question.State != pb.QuestionState_QUESTION_STATE_OPEN || h.Question.Answer != nil || h.By.Host != "laptop" {
		t.Errorf("history's first entry = %v, want the question as asked", h)
	}

	if _, e := c.fails("question", "ack", "-s", "calm-heron", "1"); e.Code != api.CodeConflict {
		t.Errorf("ack twice = %v, want CONFLICT", e)
	}
	_, err = user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Other: "again"}})
	if e := api.FromConnect(err); e == nil || e.Code != api.CodeConflict {
		t.Errorf("answer a handled question = %v, want CONFLICT", err)
	}
}

// Until an agent acts on it, the user may change a response in the outbox:
// answer again, or dismiss instead. Answered, dismissed or handled, the
// user may return a question to the inbox, its response cleared, at the
// top of their order; and may revise a question in any state but deleted,
// so long as its answer still fits it. Each wakes the asker as an answer
// does, and each is kept in the history.
func TestUserChangesResponsesReopensAndRevises(t *testing.T) {
	c, user := questionCluster(t)
	c.ask("brave-otter", storeQuestion) // 1
	c.ask("brave-otter", twoQuestions)  // 2, 3: on top of 1
	answer := func(id int32, a *pb.Answer) *pb.Question {
		t.Helper()
		q, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: id, Answer: a})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	answer(1, &pb.Answer{Picked: []string{"SQLite"}})
	c.ok(nil, "poll", "-s", "brave-otter", "--timeout", "1") // past the first answer

	// In the outbox: answered again, then dismissed instead, then answered.
	if q := answer(1, &pb.Answer{Picked: []string{"Postgres"}, Notes: "on second thought"}); q.State != pb.QuestionState_QUESTION_STATE_ANSWERED || q.Answer.Picked[0] != "Postgres" {
		t.Fatalf("answered again = %v", q)
	}
	waited := &pb.SessionEvents{}
	c.ok(waited, "poll", "-s", "brave-otter", "--timeout", "5")
	if len(waited.Events) != 1 || waited.Events[0].Detail != "question 1 (Store) answered: Postgres" {
		t.Errorf("brave-otter's wait = %v, want the new answer", waited.Events)
	}
	dismissed := &pb.Question{}
	c.ok(dismissed, "question", "dismiss", "1", "--reason", "later")
	if dismissed.State != pb.QuestionState_QUESTION_STATE_DISMISSED || dismissed.Answer != nil || dismissed.Answered != nil {
		t.Errorf("dismissed instead = %v, want no answer left", dismissed)
	}
	if q := answer(1, &pb.Answer{Other: "MySQL"}); q.Dismissed != nil || q.Answer.Other != "MySQL" {
		t.Errorf("answered after dismissal = %v, want no dismissal left", q)
	}

	// Handled, it is the agent's: no more changes to the response, but it
	// can go back to the inbox, on top, with no response.
	c.ok(nil, "question", "ack", "-s", "brave-otter", "1")
	if _, e := c.fails("question", "answer", "1", "--pick", "SQLite"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "is handled") {
		t.Errorf("answer a handled question = %v, want CONFLICT", e)
	}
	reopened := &pb.Question{}
	c.ok(reopened, "question", "reopen", "1")
	if reopened.State != pb.QuestionState_QUESTION_STATE_OPEN || reopened.Answer != nil || reopened.Answered != nil || reopened.Handled != nil {
		t.Errorf("reopened = %v, want open with no response", reopened)
	}
	if got := c.listed("--sort", "rank"); !slices.Equal(got, []int32{1, 2, 3}) {
		t.Errorf("rank order after reopening 1 = %v, want it on top", got)
	}
	if _, e := c.fails("question", "reopen", "1"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "open already") {
		t.Errorf("reopen an open question = %v, want CONFLICT", e)
	}
	answer(1, &pb.Answer{Picked: []string{"SQLite"}})
	if got, want := whats(c.question("1")), []string{"asked by brave-otter", "answered by user", "answered by user", "dismissed by user",
		"answered by user", "handled by brave-otter", "reopened by user", "answered by user"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}

	// Revised in the outbox: the answer must still fit.
	revised := &pb.Question{}
	c.okIn(`{"question": "Which store should **sessions** use?", "header": "Store", "body": "They outgrew the JSON file.",
	  "options": [{"label": "SQLite", "description": "One file"}, {"label": "Postgres", "description": "A server"}]}`, revised, "question", "revise", "1")
	if revised.Prompt.Body != "They outgrew the JSON file." || revised.Edited.Session != "" || revised.State != pb.QuestionState_QUESTION_STATE_ANSWERED {
		t.Errorf("revised = %v, want the user's edit, still answered", revised)
	}
	if _, e := c.failsIn(`{"question": "Which?", "header": "Store", "options": [{"label": "Redis", "description": "a"}, {"label": "Postgres", "description": "b"}]}`,
		"question", "revise", "1"); e.Code != api.CodeConflict || !strings.Contains(e.Message, `answer no longer fits it: question 1 has no option "SQLite"`) {
		t.Errorf("revise away the picked option = %v, want CONFLICT", e)
	}
	c.ok(nil, "question", "rm", "-s", "brave-otter", "2")
	for _, args := range [][]string{{"question", "revise", "2"}, {"question", "reopen", "2"}} {
		if _, e := c.failsIn(storeQuestion, args...); e.Code != api.CodeConflict || !strings.Contains(e.Message, "was deleted") {
			t.Errorf("%v on a deleted question = %v, want CONFLICT", args, e)
		}
	}
}

// A decision an agent already took is added answered: picked as its
// implemented options, answered and handled by the asking session in the
// one add, so it never waits in the outbox and wakes no one. The user
// overturns it by reopening it and answering. A decision marks what was
// decided, or none of the add's questions is made.
func TestDecisionsAreAddedAnswered(t *testing.T) {
	c, user := questionCluster(t)
	c.ok(nil, "poll", "-s", "brave-otter", "--timeout", "1") // past what happened so far
	res := &pb.AddQuestionsResponse{}
	c.okIn(`{"questions": [
	  {"question": "Which store do sessions use?", "header": "Store", "body": "Decided on 2026-11-08: one file.",
	   "options": [{"label": "SQLite", "description": "One file", "implemented": true}, {"label": "Postgres", "description": "A server", "recommended": true}]},
	  {"question": "Which platforms ship?", "header": "Platforms", "multiSelect": true,
	   "options": [{"label": "linux", "description": "x", "implemented": true}, {"label": "darwin", "description": "y", "implemented": true},
	               {"label": "windows", "description": "z"}]}]}`, res, "question", "add", "-s", "brave-otter", "--answered")
	if got := ids(res.Questions); !slices.Equal(got, []int32{1, 2}) {
		t.Fatalf("add --answered made %v, want [1 2]", got)
	}
	for i, want := range [][]string{{"SQLite"}, {"linux", "darwin"}} {
		q := res.Questions[i]
		if q.State != pb.QuestionState_QUESTION_STATE_HANDLED || !slices.Equal(q.Answer.GetPicked(), want) ||
			q.Answered.GetSession() != "brave-otter" || q.Handled.GetSession() != "brave-otter" {
			t.Errorf("decision %d = %v, want handled, picked %v by brave-otter", q.Id, q, want)
		}
	}
	if got, want := whats(c.question("1")), []string{"asked by brave-otter", "answered by brave-otter", "handled by brave-otter"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	outbox := &pb.ListQuestionsResponse{}
	c.ok(outbox, "question", "outbox")
	if len(outbox.Questions) != 0 {
		t.Errorf("outbox = %v, want decisions never in it", ids(outbox.Questions))
	}
	if got := c.listed("--state", "open"); len(got) != 0 {
		t.Errorf("open = %v, want none", got)
	}
	waited := &pb.SessionEvents{}
	c.ok(waited, "poll", "-s", "brave-otter", "--timeout", "1")
	if len(waited.Events) != 0 {
		t.Errorf("brave-otter's wait = %v, want nothing: it decided", waited.Events)
	}

	// The user overturns one: reopened, then answered, it goes to the
	// outbox and its asker hears of it, as any answer does.
	c.ok(nil, "question", "reopen", "1")
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Picked: []string{"Postgres"}}}); err != nil {
		t.Fatal(err)
	}
	c.ok(outbox, "question", "outbox")
	if !slices.Equal(ids(outbox.Questions), []int32{1}) {
		t.Errorf("outbox after overturning = %v, want [1]", ids(outbox.Questions))
	}
	c.ok(waited, "poll", "-s", "brave-otter", "--timeout", "5")
	if len(waited.Events) != 1 || waited.Events[0].Detail != "question 1 (Store) answered: Postgres" {
		t.Errorf("brave-otter's wait = %v, want the user's answer", waited.Events)
	}

	// No implemented option: nothing of the add is made.
	noDecision := `{"questions": [
	  {"question": "Keep it?", "header": "Keep", "options": [{"label": "Keep", "description": "a", "implemented": true}, {"label": "Drop", "description": "b"}]},
	  {"question": "Which?", "header": "Pick", "options": [{"label": "A", "description": "a", "recommended": true}, {"label": "B", "description": "b"}]}]}`
	if _, e := c.failsIn(noDecision, "question", "add", "-s", "brave-otter", "--answered"); e.Code != api.CodeInvalidArgs ||
		!strings.Contains(e.Message, `question 2 of 2: question "Which?" is answered, but no option is implemented`) {
		t.Errorf("add --answered without a decision = %v, want INVALID_ARGS", e)
	}
	if got := c.listed(); !slices.Equal(got, []int32{2, 1}) {
		t.Errorf("ls after the refused add = %v, want only [2 1]", got)
	}
	// Without --answered, an implemented option is the agent's guess, and
	// the question waits on the user.
	if q := c.ask("brave-otter", noDecision)[0]; q.State != pb.QuestionState_QUESTION_STATE_OPEN || q.Answer != nil {
		t.Errorf("asked with an implemented option = %v, want open", q)
	}
}

// An agent passing on the user's word answers as its session; a
// multiSelect question takes several picks, and words of the user's own.
func TestAgentPassesOnAnAnswer(t *testing.T) {
	c, _ := questionCluster(t)
	c.ask("brave-otter", twoQuestions)
	q := &pb.Question{}
	c.ok(q, "question", "answer", "-s", "calm-heron", "1", "--pick", "linux", "--pick", "darwin", "--other", "and freebsd later")
	if a := q.Answer; !slices.Equal(a.Picked, []string{"linux", "darwin"}) || a.Other != "and freebsd later" || q.Answered.Session != "calm-heron" {
		t.Errorf("answer = %v by %v", a, q.Answered)
	}
}

// Any session may edit or delete any question; a deleted one is left out
// of the list unless asked for, and keeps its history. The user may
// dismiss a question, which sends it to the outbox unanswered.
func TestQuestionsAreEditedDeletedAndDismissed(t *testing.T) {
	c, _ := questionCluster(t)
	c.ask("brave-otter", twoQuestions)

	edited := &pb.Question{}
	c.okIn(`{"question": "Which platforms ship first?", "header": "Platforms", "options": [{"label": "linux", "description": "x"}, {"label": "darwin", "description": "y"}]}`,
		edited, "question", "edit", "-s", "calm-heron", "1")
	if edited.Prompt.Question != "Which platforms ship first?" || len(edited.Prompt.Options) != 2 || edited.Edited.Session != "calm-heron" || edited.Asked.Session != "brave-otter" {
		t.Fatalf("edited = %v", edited)
	}

	deleted := &pb.Question{}
	c.ok(deleted, "question", "rm", "-s", "calm-heron", "2", "--reason", "settled in review")
	if deleted.State != pb.QuestionState_QUESTION_STATE_DELETED || deleted.Deleted.Session != "calm-heron" || deleted.Deleted.Reason != "settled in review" {
		t.Fatalf("deleted = %v", deleted)
	}
	if got := c.listed(); !slices.Equal(got, []int32{1}) {
		t.Errorf("ls = %v, want the deleted question left out", got)
	}
	if got := c.listed("--state", "deleted"); !slices.Equal(got, []int32{2}) {
		t.Errorf("ls --state deleted = %v", got)
	}
	if got := c.listed("--state", "all"); !slices.Equal(got, []int32{2, 1}) {
		t.Errorf("ls --state all = %v", got)
	}
	if _, e := c.fails("question", "rm", "-s", "brave-otter", "2"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "by calm-heron on laptop") {
		t.Errorf("rm twice = %v, want CONFLICT naming who deleted it", e)
	}
	if _, e := c.failsIn(storeQuestion, "question", "edit", "-s", "brave-otter", "2"); e.Code != api.CodeConflict {
		t.Errorf("edit a deleted question = %v, want CONFLICT", e)
	}

	dismissed := &pb.Question{}
	c.ok(dismissed, "question", "dismiss", "1", "--reason", "ask me after the release")
	if dismissed.Dismissed.Session != "" || dismissed.Dismissed.Host != "laptop" {
		t.Errorf("dismissed by %v, want the user on laptop", dismissed.Dismissed)
	}
	outbox := &pb.ListQuestionsResponse{}
	c.ok(outbox, "question", "outbox")
	if len(outbox.Questions) != 1 || outbox.Questions[0].State != pb.QuestionState_QUESTION_STATE_DISMISSED || outbox.Questions[0].Dismissed.Reason != "ask me after the release" {
		t.Errorf("outbox = %v, want the dismissed question", outbox.Questions)
	}

	one := c.question("1")
	if got, want := whats(one), []string{"asked by brave-otter", "edited by calm-heron", "dismissed by user"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	if was := one.History[0].Question.Prompt.Question; was != "Which platforms ship?" {
		t.Errorf("history's first entry asks %q, want the question before its edit", was)
	}
	two := c.question("2")
	if got, want := whats(two), []string{"asked by brave-otter", "deleted by calm-heron"}; !slices.Equal(got, want) || two.History[1].By.Reason != "settled in review" {
		t.Errorf("history = %v, want %v with its reason", two.History, want)
	}
}

// The user orders the open questions; moving one is kept in its history
// but is not a change to it as the updated order sees it.
func TestUserOrdersTheQuestions(t *testing.T) {
	c, user := questionCluster(t)
	for range 3 {
		c.ask("brave-otter", storeQuestion)
		c.clock.advance(time.Second)
	}
	if got := c.listed("--sort", "rank"); !slices.Equal(got, []int32{3, 2, 1}) {
		t.Fatalf("rank order = %v", got)
	}
	updated := c.question("1").UpdatedAt

	c.ok(nil, "question", "move", "1", "--first")
	if got := c.listed("--sort", "rank"); !slices.Equal(got, []int32{1, 3, 2}) {
		t.Errorf("after moving 1 first: %v", got)
	}
	c.ok(nil, "question", "move", "3", "--last")
	if got := c.listed("--sort", "rank"); !slices.Equal(got, []int32{1, 2, 3}) {
		t.Errorf("after moving 3 last: %v", got)
	}
	if _, err := user.MoveQuestion(t.Context(), &pb.MoveQuestionRequest{Id: 3, Place: &pb.MoveQuestionRequest_Before{Before: 2}}); err != nil {
		t.Fatal(err)
	}
	if got := c.listed("--sort", "rank"); !slices.Equal(got, []int32{1, 3, 2}) {
		t.Errorf("after moving 3 before 2: %v", got)
	}

	one := c.question("1")
	if one.UpdatedAt != updated {
		t.Errorf("a move changed question 1's updated_at from %d to %d", updated, one.UpdatedAt)
	}
	if got := whats(one); !slices.Equal(got, []string{"asked by brave-otter", "moved by user"}) {
		t.Errorf("history = %v", got)
	}

	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 2, Answer: &pb.Answer{Other: "neither"}}); err != nil {
		t.Fatal(err)
	}
	if _, e := c.fails("question", "move", "2", "--first"); e.Code != api.CodeConflict {
		t.Errorf("move an answered question = %v, want CONFLICT", e)
	}
	if _, e := c.fails("question", "move", "1", "--before", "2"); e.Code != api.CodeConflict {
		t.Errorf("move before an answered question = %v, want CONFLICT", e)
	}
	if _, e := c.fails("question", "move", "1"); e.Code != api.CodeInvalidArgs {
		t.Errorf("move without a place = %v, want INVALID_ARGS", e)
	}
}

// hotbed poll returns at its timeout with nothing while a question is
// open, and at once when the user answers it, with the question as
// answered.
func TestPollHearsAnAnswer(t *testing.T) {
	c, user := questionCluster(t)
	c.ask("brave-otter", storeQuestion)
	c.ok(nil, "poll", "-s", "brave-otter", "--timeout", "1") // past what happened so far

	res := &pb.SessionEvents{}
	c.ok(res, "poll", "-s", "brave-otter", "--timeout", "1")
	if len(res.Events) != 0 {
		t.Fatalf("poll that timed out = %v, want nothing", res.Events)
	}

	polled := &pb.SessionEvents{}
	start := time.Now()
	done := c.hotbedAsync(polled, "poll", "-s", "brave-otter")
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Picked: []string{"Postgres"}}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("the poll took %v: the answer did not wake it", took)
	}
	if len(polled.Events) != 1 {
		t.Fatalf("poll = %v, want the answer", polled.Events)
	}
	q := polled.Events[0].GetQuestion()
	if q.GetWhat() != pb.QuestionUpdate_WHAT_ANSWERED || q.GetQuestion().GetId() != 1 ||
		q.GetQuestion().GetState() != pb.QuestionState_QUESTION_STATE_ANSWERED ||
		!slices.Equal(q.GetQuestion().GetAnswer().GetPicked(), []string{"Postgres"}) || len(q.GetQuestion().GetHistory()) != 0 {
		t.Errorf("poll = %v, want question 1 as answered, without its history", polled.Events[0])
	}
}

// Each project has its own list, which every session working on it shares;
// a listing is of the session's project unless told otherwise.
func TestQuestionsBelongToAProject(t *testing.T) {
	c, user := questionCluster(t)
	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "far-fox", "-p", "other")
	if q := c.ask("brave-otter", storeQuestion)[0]; q.Project != "test" {
		t.Errorf("brave-otter's question is about %q, want its session's project", q.Project)
	}
	c.ask("far-fox", storeQuestion)

	if got := c.listed(); !slices.Equal(got, []int32{1}) {
		t.Errorf("ls in project test = %v", got)
	}
	if got := c.listed("-p", "other"); !slices.Equal(got, []int32{2}) {
		t.Errorf("ls -p other = %v", got)
	}
	// --all-projects outweighs HOTBED_PROJECT (test), which -p does too.
	if got := c.listed("--all-projects"); !slices.Equal(got, []int32{2, 1}) {
		t.Errorf("ls --all-projects = %v", got)
	}
	if _, e := c.fails("question", "ls", "-p", "other", "--all-projects"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "can't be used together") {
		t.Errorf("ls -p other --all-projects = %v, want INVALID_ARGS: one or the other", e)
	}
	t.Setenv("HOTBED_PROJECT", "")
	if got := c.listed("-s", "far-fox"); !slices.Equal(got, []int32{2}) {
		t.Errorf("ls -s far-fox = %v, want its project's", got)
	}
	if _, e := c.fails("question", "ls"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "say which project") {
		t.Errorf("ls with no project or session = %v, want INVALID_ARGS", e)
	}
	if _, e := c.fails("question", "ls", "-p", "nosuch"); e.Code != api.CodeNotFound {
		t.Errorf("ls -p nosuch = %v, want NOT_FOUND", e)
	}

	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 2, Answer: &pb.Answer{Other: "later"}}); err != nil {
		t.Fatal(err)
	}
	outbox := &pb.ListQuestionsResponse{}
	c.ok(outbox, "question", "outbox", "-p", "test")
	if len(outbox.Questions) != 0 {
		t.Errorf("test's outbox = %v, want nothing: the answer is other's", ids(outbox.Questions))
	}
	c.ok(outbox, "question", "outbox", "-p", "other")
	if !slices.Equal(ids(outbox.Questions), []int32{2}) {
		t.Errorf("outbox -p other = %v", ids(outbox.Questions))
	}
	projects := &pb.ListQuestionProjectsResponse{}
	c.ok(projects, "question", "projects")
	if got := projects.Projects; len(got) != 2 || got[0].Name != "other" || got[0].Outbox != 1 || got[0].Open != 0 ||
		got[1].Name != "test" || got[1].Open != 1 || got[1].Total != 1 {
		t.Errorf("question projects = %v", got)
	}
}

// A session's poll hears of answers to its own questions alone: an answer
// to another session's question is that session's.
func TestPollHearsOnlyOwnAnswers(t *testing.T) {
	c, user := questionCluster(t)
	c.ask("brave-otter", storeQuestion)
	c.ask("calm-heron", storeQuestion)
	c.ok(nil, "poll", "-s", "brave-otter", "--timeout", "1")
	c.ok(nil, "poll", "-s", "calm-heron", "--timeout", "1")
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 2, Answer: &pb.Answer{Other: "yes"}}); err != nil {
		t.Fatal(err)
	}
	res := &pb.SessionEvents{}
	c.ok(res, "poll", "-s", "brave-otter", "--timeout", "1")
	if len(res.Events) != 0 {
		t.Errorf("brave-otter's poll = %v, want nothing: the answer is to calm-heron's question", res.Events)
	}
	c.ok(res, "poll", "-s", "calm-heron", "--timeout", "1")
	if len(res.Events) != 1 || res.Events[0].GetQuestion().GetQuestion().GetId() != 2 {
		t.Errorf("calm-heron's poll = %v, want its answered question", res.Events)
	}
	// Each event comes once; --after reads again from a cursor.
	answered := res.Events[0].Seq
	c.ok(res, "poll", "-s", "calm-heron", "--timeout", "1")
	if len(res.Events) != 0 {
		t.Errorf("calm-heron's poll again = %v, want nothing new", res.Events)
	}
	c.ok(res, "poll", "-s", "calm-heron", "--after", strconv.FormatInt(answered-1, 10), "--timeout", "1")
	if len(res.Events) != 1 || res.Events[0].Seq != answered {
		t.Errorf("calm-heron's poll --after %d = %v, want the answer again", answered-1, res.Events)
	}
	if _, e := c.fails("poll", "-s", "no-such-session", "--timeout", "1"); e.Code != api.CodeNotFound {
		t.Errorf("poll as no such session = %v, want NOT_FOUND", e)
	}
}

// A change the user makes is the user's: a session merely bound here, even
// one long ended, is not recorded, and does not stop it. A session named
// with -s is recorded, and must be live.
func TestUserChangesIgnoreABoundSession(t *testing.T) {
	c, _ := questionCluster(t)
	c.ask("brave-otter", storeQuestion)
	t.Setenv("T3CODE_THREAD_ID", "thread-old")
	c.ok(nil, "session", "start", "old-owl")
	c.ok(nil, "session", "end", "-s", "old-owl")

	q := &pb.Question{}
	c.ok(q, "question", "answer", "1", "--pick", "Postgres")
	if q.Answered.Session != "" {
		t.Errorf("answered as %q, want the user", q.Answered.Session)
	}
	c.ask("brave-otter", storeQuestion)
	if _, e := c.fails("question", "dismiss", "2", "-s", "old-owl"); e.Code != api.CodeNotFound || !strings.Contains(e.Message, "ended") {
		t.Errorf("dismiss -s of an ended session = %v, want NOT_FOUND", e)
	}
}

// A session changes only its own project's questions: their list is what
// its project's sessions read.
func TestAgentsChangeOnlyTheirProjectsQuestions(t *testing.T) {
	c, user := questionCluster(t)
	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "far-fox", "-p", "other")
	c.ask("brave-otter", storeQuestion)
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Other: "yes"}}); err != nil {
		t.Fatal(err)
	}
	c.ask("brave-otter", storeQuestion)
	for _, args := range [][]string{
		{"question", "ack", "-s", "far-fox", "1"},
		{"question", "rm", "-s", "far-fox", "2"},
		{"question", "answer", "-s", "far-fox", "2", "--other", "no"},
		{"question", "dismiss", "-s", "far-fox", "2"},
		{"question", "move", "-s", "far-fox", "2", "--first"},
		{"question", "move", "-s", "far-fox", "2", "--last"},
	} {
		if _, e := c.fails(args...); e.Code != api.CodeConflict || !strings.Contains(e.Message, "is project test's; session far-fox works on other") {
			t.Errorf("%v = %v, want CONFLICT", args, e)
		}
	}
	c.ask("far-fox", storeQuestion) // 3, in project other
	if _, e := c.fails("question", "move", "2", "--before", "3"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "within their own project") {
		t.Errorf("move across projects = %v, want INVALID_ARGS", e)
	}
	if _, e := c.failsIn(storeQuestion, "question", "edit", "-s", "far-fox", "2"); e.Code != api.CodeConflict {
		t.Errorf("edit another project's question = %v, want CONFLICT", e)
	}
	if got := whats(c.question("1")); !slices.Equal(got, []string{"asked by brave-otter", "answered by user"}) {
		t.Errorf("refused changes left history %v", got)
	}
}

// move --first puts a question first in its own project's list.
func TestMoveFirstWithinItsProject(t *testing.T) {
	c, _ := questionCluster(t)
	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "far-fox", "-p", "other")
	c.ask("brave-otter", storeQuestion) // 1, test
	c.ask("brave-otter", storeQuestion) // 2, test
	c.ask("far-fox", storeQuestion)     // 3, other: first of every project's
	c.ok(nil, "question", "move", "1", "--first")
	if got := c.listed("--sort", "rank"); !slices.Equal(got, []int32{1, 2}) {
		t.Errorf("test's order after move 1 --first = %v", got)
	}
	if got := c.listed("--all-projects", "--sort", "rank"); !slices.Equal(got, []int32{3, 1, 2}) {
		t.Errorf("every project's order = %v: moving within test should leave 3 where it was", got)
	}
}

// A generated command whose session defaults to the one it acts as sends
// none when there is none, and the controller refuses it, saying what is
// missing, rather than acting for everyone or as the user. Every such
// command is here: a new one fails the test until it is.
func TestCommandsActingAsNoSessionAreRefused(t *testing.T) {
	c, _ := questionCluster(t)
	c.ask("brave-otter", storeQuestion)
	t.Setenv("HOTBED_PROJECT", "")
	t.Setenv("HOTBED_SESSION", "")
	edited := `{"question": "Which store?", "header": "Store", "options": [{"label": "A", "description": "a"}, {"label": "B", "description": "b"}]}`
	cases := map[string]struct {
		stdin string
		args  []string
		says  string
	}{
		"question add":    {storeQuestion, nil, "name the session making the change"},
		"question edit":   {edited, []string{"1"}, "name the session making the change"},
		"question rm":     {"", []string{"1"}, "name the session making the change"},
		"question ack":    {"", []string{"1"}, "name the session making the change"},
		"question ls":     {"", nil, "say which project"},
		"question outbox": {"", nil, "say which project"},
		"memory add":      {"", []string{"--slug", "s", "--summary", "sum"}, "specify --global or -p PROJECT"},
		"memory propose":  {"", []string{"--slug", "s", "--summary", "sum"}, "specify --global or -p PROJECT"},
		"memory ls":       {"", nil, "say which project"},
		"poll":            {"", []string{"--timeout", "1"}, "name the session"},
	}
	s, err := apiSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range s.Commands {
		for _, p := range cmd.Params {
			if _, ok := cases[cmd.Name()]; p.Default == "session" && !ok {
				t.Errorf("%s defaults %s to the session, and is not tested acting as none here", cmd.Name(), p.Field().FullName())
			}
		}
	}
	// The cluster's helpers fail the test itself, so no subtests.
	for name, tc := range cases {
		_, e := c.failsIn(tc.stdin, append(strings.Fields(name), tc.args...)...)
		if e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, tc.says) {
			t.Errorf("%s with no session = %v, want INVALID_ARGS saying %q", name, e, tc.says)
		}
	}
	// A session named must be one, even where a project makes it unused.
	if _, e := c.fails("question", "ls", "-p", "test", "-s", "no-such-session"); e.Code != api.CodeNotFound {
		t.Errorf("ls -p test -s no-such-session = %v, want NOT_FOUND", e)
	}
}

// Every question call reminds the agent that answers come in through poll,
// so it polls before it ends its turn or commits rather than assume the
// user has not answered: every QuestionService response has the reminder,
// set on the response alone, and the plain text prints it.
func TestEveryQuestionResponseReminds(t *testing.T) {
	svc := pb.File_hotbed_v1_hotbed_proto.Services().ByName("QuestionService")
	methods := svc.Methods()
	for i := range methods.Len() {
		out := methods.Get(i).Output()
		if f := out.Fields().ByName("reminder"); f == nil || f.Kind() != protoreflect.StringKind || !f.HasPresence() {
			t.Errorf("%s returns %s, which has no optional string reminder", methods.Get(i).Name(), out.FullName())
		}
	}

	c, user := questionCluster(t)
	added := &pb.AddQuestionsResponse{}
	c.okIn(storeQuestion, added, "question", "add", "-s", "brave-otter")
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Picked: []string{"Postgres"}}}); err != nil {
		t.Fatal(err)
	}
	listed := &pb.ListQuestionsResponse{}
	c.ok(listed, "question", "ls", "-s", "brave-otter")
	shown := &pb.Question{}
	c.ok(shown, "question", "show", "1")
	acked := &pb.AckQuestionsResponse{}
	c.ok(acked, "question", "ack", "1", "-s", "brave-otter")
	for name, got := range map[string]string{
		"question add": added.GetReminder(), "question ls": listed.GetReminder(), "question show": shown.GetReminder(), "question ack": acked.GetReminder(),
	} {
		if !strings.Contains(got, "run `hotbed poll`") || !strings.Contains(got, "Before you end your turn, commit") {
			t.Errorf("%s's reminder = %q, want it to say to poll before ending the turn or committing", name, got)
		}
	}
	if r := listed.Questions[0].Reminder; r != nil {
		t.Errorf("a listed question has a reminder of its own (%q): only the response does", *r)
	}
	polled := &pb.SessionEvents{}
	c.ok(polled, "poll", "-s", "brave-otter", "--timeout", "1")
	for _, e := range polled.Events {
		if q := e.GetQuestion().GetQuestion(); q.Reminder != nil {
			t.Errorf("poll's question %d has a reminder (%q): only question calls remind", q.Id, *q.Reminder)
		}
	}

	code, out, _ := c.hotbed("question", "show", "1")
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), shown.GetReminder()) {
		t.Errorf("question show prints %q, want it to end with the reminder", out)
	}
}

// onePrompt rejects extra AddQuestionsRequest fields such as "answered": true
// on question edit and question revise rather than silently dropping them
// (board 7894).
func TestQuestionOnePromptRejectsAnsweredAndExtraFields(t *testing.T) {
	c, _ := questionCluster(t)
	c.ask("brave-otter", storeQuestion)

	base := `{"question": "Which store?", "header": "Store", "options": [{"label": "SQLite", "description": "a", "implemented": true}, {"label": "Postgres", "description": "b"}]}`
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"answered true", `{"questions": [` + base + `], "answered": true}`},
		{"answered false", `{"questions": [` + base + `], "answered": false}`},
		{"repo set", `{"questions": [` + base + `], "repo": "github.com/a/b"}`},
		{"repo empty", `{"questions": [` + base + `], "repo": ""}`},
		{"sessionId set", `{"questions": [` + base + `], "sessionId": "brave-otter"}`},
		{"sessionId empty", `{"questions": [` + base + `], "sessionId": ""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := onePrompt([]byte(tc.in)); api.CodeOf(err) != api.CodeInvalidArgs {
				t.Errorf("onePrompt(%s) = %v, want INVALID_ARGS", tc.name, err)
			}
		})
	}
	for _, in := range []string{
		`{"questions": [` + base + `], "answered": true}`,
		`{"questions": [` + base + `], "answered": false}`,
	} {
		if _, e := c.failsIn(in, "question", "edit", "-s", "brave-otter", "1"); e.Code != api.CodeInvalidArgs {
			t.Errorf("question edit with %s = %+v, want INVALID_ARGS", in, e)
		}
		if _, e := c.failsIn(in, "question", "revise", "1"); e.Code != api.CodeInvalidArgs {
			t.Errorf("question revise with %s = %+v, want INVALID_ARGS", in, e)
		}
	}
}

// Questions map back to their asking sessions and T3 Code threads across
// question add, ls, outbox, and show — both from T3CODE_THREAD_ID captured
// when asked and from the asking session's persisted t3code_thread_id.
func TestQuestionT3CodeThreadMappingAndOutboxSessions(t *testing.T) {
	c, user := questionCluster(t)
	c.ok(nil, "project", "add", "other")

	// 1. Session started with T3CODE_THREAD_ID; asks questions later with env unset.
	t.Setenv("T3CODE_THREAD_ID", "thread-alpha")
	c.ok(nil, "session", "start", "alpha-one")
	t.Setenv("T3CODE_THREAD_ID", "")
	qAlpha1 := c.ask("alpha-one", twoQuestions) // #1, #2
	if qAlpha1[0].T3CodeThreadId != "thread-alpha" || qAlpha1[1].T3CodeThreadId != "thread-alpha" {
		t.Fatalf("alpha-one questions t3 = [%q %q], want thread-alpha", qAlpha1[0].T3CodeThreadId, qAlpha1[1].T3CodeThreadId)
	}

	// 2. Second session for the same T3 thread in another project.
	c.ok(nil, "session", "start", "alpha-two", "-p", "other", "--t3code-thread-id", "thread-alpha")
	t.Setenv("T3CODE_THREAD_ID", "thread-alpha")
	qAlpha2 := c.ask("alpha-two", storeQuestion) // #3
	if qAlpha2[0].T3CodeThreadId != "thread-alpha" {
		t.Fatalf("alpha-two question t3 = %q, want thread-alpha", qAlpha2[0].T3CodeThreadId)
	}

	// 3. Session started without a T3 thread; asking with T3CODE_THREAD_ID records
	//    it on the question AND backfills the session.
	t.Setenv("T3CODE_THREAD_ID", "")
	c.ok(nil, "session", "start", "beta-one")
	t.Setenv("T3CODE_THREAD_ID", "thread-beta")
	qBeta := c.ask("beta-one", storeQuestion) // #4
	if qBeta[0].T3CodeThreadId != "thread-beta" {
		t.Fatalf("beta-one question t3 = %q, want thread-beta", qBeta[0].T3CodeThreadId)
	}
	var betaStatus sessionStatusResult
	c.ok(&betaStatus, "session", "status", "-s", "beta-one")
	if betaStatus.T3CodeThreadID != "thread-beta" {
		t.Fatalf("beta-one session T3CodeThreadID = %q, want backfilled thread-beta", betaStatus.T3CodeThreadID)
	}

	// 4. Session started and asked without any T3 thread, then later resumed with one:
	//    previously-asked questions inherit the session's t3code_thread_id via JOIN.
	t.Setenv("T3CODE_THREAD_ID", "")
	c.ok(nil, "session", "start", "gamma-one")
	qGamma := c.ask("gamma-one", storeQuestion) // #5
	if qGamma[0].T3CodeThreadId != "" {
		t.Fatalf("gamma-one question t3 = %q, want empty", qGamma[0].T3CodeThreadId)
	}
	c.ok(nil, "session", "end", "-s", "gamma-one")
	c.ok(nil, "session", "start", "gamma-one", "--resume", "--t3code-thread-id", "thread-gamma")
	if got := c.question("5").T3CodeThreadId; got != "thread-gamma" {
		t.Fatalf("question 5 after session resume t3 = %q, want thread-gamma", got)
	}

	// End alpha-one so outbox covers both ended and live sessions.
	c.ok(nil, "session", "end", "-s", "alpha-one")

	// Open questions across all projects include t3codeThreadId and session summaries.
	openRes := &pb.ListQuestionsResponse{}
	c.ok(openRes, "question", "ls", "--state", "open", "--all-projects")
	if len(openRes.Questions) != 5 {
		t.Fatalf("open questions = %d, want 5", len(openRes.Questions))
	}
	openSess := map[string]*pb.QuestionSession{}
	for _, s := range openRes.Sessions {
		openSess[s.SessionId] = s
	}
	if len(openSess) != 4 || openSess["gamma-one"].GetT3CodeThreadId() != "thread-gamma" || !openSess["gamma-one"].GetLive() || openSess["gamma-one"].GetOpen() != 1 {
		t.Fatalf("open sessions = %+v, want 4 sessions with gamma-one live & thread-gamma", openRes.Sessions)
	}
	if openSess["alpha-one"].GetLive() || openSess["alpha-one"].GetOpen() != 2 || openSess["alpha-one"].GetT3CodeThreadId() != "thread-alpha" {
		t.Fatalf("open alpha-one session = %+v", openSess["alpha-one"])
	}

	// Answer/dismiss #1, #2 (alpha-one), #3 (alpha-two), #4 (beta-one); leave #5 open.
	c.clock.advance(time.Second)
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Picked: []string{"linux"}}}); err != nil {
		t.Fatal(err)
	}
	c.clock.advance(time.Second)
	if _, err := user.DismissQuestion(t.Context(), &pb.DismissQuestionRequest{Id: 2, Reason: "skip"}); err != nil {
		t.Fatal(err)
	}
	c.clock.advance(time.Second)
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 3, Answer: &pb.Answer{Picked: []string{"SQLite"}}}); err != nil {
		t.Fatal(err)
	}
	c.clock.advance(time.Second)
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 4, Answer: &pb.Answer{Picked: []string{"Postgres"}}}); err != nil {
		t.Fatal(err)
	}

	// Survive controller restart.
	c.restart()

	// Peek at the outbox across all projects.
	outbox := &pb.ListQuestionsResponse{}
	c.ok(outbox, "question", "outbox", "--all-projects")
	if got := ids(outbox.Questions); !slices.Equal(got, []int32{4, 3, 2, 1}) {
		t.Fatalf("outbox ids = %v, want [4 3 2 1]", got)
	}
	if len(outbox.Sessions) != 3 {
		t.Fatalf("outbox sessions = %+v, want 3 sessions", outbox.Sessions)
	}
	// Ordered by most recently answered first: beta-one (#4), alpha-two (#3), alpha-one (#2, #1).
	if s := outbox.Sessions[0]; s.SessionId != "beta-one" || s.Project != "test" || s.T3CodeThreadId != "thread-beta" || !s.Live || !slices.Equal(s.QuestionIds, []int32{4}) || s.Answered != 1 || s.Dismissed != 0 || s.Open != 0 {
		t.Errorf("outbox sessions[0] = %+v", s)
	}
	if s := outbox.Sessions[1]; s.SessionId != "alpha-two" || s.Project != "other" || s.T3CodeThreadId != "thread-alpha" || !s.Live || !slices.Equal(s.QuestionIds, []int32{3}) || s.Answered != 1 || s.Dismissed != 0 {
		t.Errorf("outbox sessions[1] = %+v", s)
	}
	if s := outbox.Sessions[2]; s.SessionId != "alpha-one" || s.Project != "test" || s.T3CodeThreadId != "thread-alpha" || s.Live || !slices.Equal(s.QuestionIds, []int32{2, 1}) || s.Answered != 1 || s.Dismissed != 1 {
		t.Errorf("outbox sessions[2] = %+v", s)
	}

	// Plain-text outbox shows t3=... and the Sessions: summary block.
	code, plainOutbox, stderr := c.hotbed("question", "outbox", "--all-projects")
	if code != exitOK {
		t.Fatalf("plain question outbox: exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"t3=thread-beta", "t3=thread-alpha", "Sessions:", "beta-one", "alpha-two", "alpha-one", "ended", "live", "questions=2,1"} {
		if !strings.Contains(plainOutbox, want) {
			t.Errorf("plain question outbox missing %q:\n%s", want, plainOutbox)
		}
	}

	// Plain-text show displays session and thread.
	code, plainShow, stderr := c.hotbed("question", "show", "1")
	if code != exitOK {
		t.Fatalf("plain question show: exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"session  alpha-one", "thread   thread-alpha"} {
		if !strings.Contains(plainShow, want) {
			t.Errorf("plain question show missing %q:\n%s", want, plainShow)
		}
	}

	// Filtering by --t3code-thread-id works across projects when no project/session is named.
	t.Setenv("HOTBED_PROJECT", "")
	t.Setenv("HOTBED_SESSION", "")
	t.Setenv("T3CODE_THREAD_ID", "")
	byAlpha := &pb.ListQuestionsResponse{}
	c.ok(byAlpha, "question", "outbox", "--t3code-thread-id", "thread-alpha")
	if got := ids(byAlpha.Questions); !slices.Equal(got, []int32{3, 2, 1}) || len(byAlpha.Sessions) != 2 {
		t.Fatalf("outbox --t3code-thread-id thread-alpha = ids %v sessions %+v, want [3 2 1] and 2 sessions", got, byAlpha.Sessions)
	}
	byGamma := &pb.ListQuestionsResponse{}
	c.ok(byGamma, "question", "ls", "--t3code-thread-id", "thread-gamma")
	if got := ids(byGamma.Questions); !slices.Equal(got, []int32{5}) {
		t.Fatalf("ls --t3code-thread-id thread-gamma = %v, want [5]", got)
	}

	// Invalid t3code-thread-id is rejected cleanly.
	if _, e := c.fails("question", "outbox", "--t3code-thread-id", "bad\nthread"); e.Code != api.CodeInvalidArgs {
		t.Errorf("outbox with newline in t3code-thread-id = %+v, want INVALID_ARGS", e)
	}
	if _, e := c.fails("question", "ls", "--t3code-thread-id", strings.Repeat("a", 257)); e.Code != api.CodeInvalidArgs {
		t.Errorf("ls with overly long t3code-thread-id = %+v, want INVALID_ARGS", e)
	}
}
