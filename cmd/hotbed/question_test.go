package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
    {"label": "SQLite (Recommended)", "description": "One file, already used"},
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
	t.Setenv("HOTBED_SESSION", "")
	t.Setenv("T3CODE_THREAD_ID", "")
	// Outside any checkout, so the sessions bind to nothing: a command
	// names its session, or has none.
	t.Chdir(t.TempDir())
	c := newCluster(t)
	c.ok(nil, "session", "start", "brave-otter")
	c.ok(nil, "session", "start", "calm-heron")
	conn, err := api.Dial(os.Getenv("HOTBED_CONTROLLER"), api.DefaultPort, c.cli.http)
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

// The user answers; the answer waits in the outbox, the asking session's
// hotbed wait hears of it, and any agent that acts on it acknowledges it,
// which makes it handled. Every step is kept, with the session that took it.
func TestAnsweredQuestionGoesThroughTheOutbox(t *testing.T) {
	c, user := questionCluster(t)
	c.ask("brave-otter", storeQuestion)
	c.ok(nil, "wait", "-s", "brave-otter", "--timeout", "1") // past what happened so far

	q, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Picked: []string{"SQLite (Recommended)"}, Notes: "keep it simple"}})
	if err != nil {
		t.Fatal(err)
	}
	if q.State != pb.QuestionState_QUESTION_STATE_ANSWERED || q.Answered.Session != "" || q.Answered.Host != "laptop" {
		t.Fatalf("answered question = %v", q)
	}
	outbox := &pb.ListQuestionsResponse{}
	c.ok(outbox, "question", "outbox")
	if !slices.Equal(ids(outbox.Questions), []int32{1}) || outbox.Questions[0].Answer.Picked[0] != "SQLite (Recommended)" {
		t.Fatalf("outbox = %v", outbox)
	}

	waited := &pb.WatchSessionResponse{}
	c.ok(waited, "wait", "-s", "brave-otter", "--timeout", "5")
	if len(waited.Events) != 1 || waited.Events[0].What != "answered" || waited.Events[0].QuestionId != 1 ||
		waited.Events[0].Detail != "question 1 (Store) answered: SQLite (Recommended)" {
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
	c, user := questionCluster(t)
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

	if _, err := user.DismissQuestion(t.Context(), &pb.DismissQuestionRequest{Id: 1, Reason: "ask me after the release"}); err != nil {
		t.Fatal(err)
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
	if _, err := user.MoveQuestion(t.Context(), &pb.MoveQuestionRequest{Id: 3, Before: 2}); err != nil {
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

// hotbed question wait returns when a named question is answered (or at
// its timeout, with it still open), and without names, once the outbox
// holds something.
func TestWaitForAnAnswer(t *testing.T) {
	c, user := questionCluster(t)
	c.ask("brave-otter", storeQuestion)

	res := &pb.ListQuestionsResponse{}
	c.ok(res, "question", "wait", "1", "--timeout", "1")
	if len(res.Questions) != 1 || res.Questions[0].State != pb.QuestionState_QUESTION_STATE_OPEN {
		t.Fatalf("wait that timed out = %v, want question 1 still open", res.Questions)
	}

	done := make(chan *pb.ListQuestionsResponse, 2)
	start := time.Now()
	for _, args := range [][]string{{"question", "wait", "1"}, {"question", "wait"}} {
		go func() {
			res := &pb.ListQuestionsResponse{}
			c.ok(res, args...)
			done <- res
		}()
	}
	if _, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: &pb.Answer{Picked: []string{"Postgres"}}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		res := <-done
		if len(res.Questions) != 1 || res.Questions[0].State != pb.QuestionState_QUESTION_STATE_ANSWERED {
			t.Errorf("wait = %v, want question 1 answered", res.Questions)
		}
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("the waits took %v: the answer did not wake them", took)
	}

	if _, e := c.fails("question", "wait", "99"); e.Code != api.CodeNotFound {
		t.Errorf("wait for no such question = %v, want NOT_FOUND", e)
	}
}

// Questions take what AskUserQuestion takes, and nothing it would refuse;
// answers pick among a question's options.
func TestQuestionsHoldToAskUserQuestion(t *testing.T) {
	c, user := questionCluster(t)
	opts := `[{"label": "A", "description": "a"}, {"label": "B", "description": "b"}]`
	for _, tc := range []struct{ name, input, says string }{
		{"no questions", `{"questions": []}`, "ask one to 4 questions"},
		{"five questions", `{"questions": [` + strings.Repeat(`{"question": "Q?", "header": "H", "options": `+opts+`},`, 4) + `{"question": "Q?", "header": "H", "options": ` + opts + `}]}`, "got 5"},
		{"no header", `{"questions": [{"question": "Q?", "options": ` + opts + `}]}`, "needs a header"},
		{"long header", `{"questions": [{"question": "Q?", "header": "Thirteen char", "options": ` + opts + `}]}`, "longer than 12"},
		{"one option", `{"questions": [{"question": "Q?", "header": "H", "options": [{"label": "A", "description": "a"}]}]}`, "has 1 options"},
		{"no description", `{"questions": [{"question": "Q?", "header": "H", "options": [{"label": "A"}, {"label": "B", "description": "b"}]}]}`, `option "A" needs a description`},
		{"other", `{"questions": [{"question": "Q?", "header": "H", "options": [{"label": "A", "description": "a"}, {"label": "Other", "description": "b"}]}]}`, "leave out the \"Other\" option"},
		{"same label", `{"questions": [{"question": "Q?", "header": "H", "options": [{"label": "A", "description": "a"}, {"label": "A", "description": "b"}]}]}`, `two options labelled "A"`},
		{"unknown field", `{"questions": [{"question": "Q?", "header": "H", "options": ` + opts + `}], "metadata": {"source": "x"}}`, `unknown field "metadata"`},
		{"no JSON", ``, "no JSON on stdin"},
	} {
		code, e := c.failsIn(tc.input, "question", "add", "-s", "brave-otter")
		if code != exitError || e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, tc.says) {
			t.Errorf("%s: exit %d, %v; want INVALID_ARGS saying %q", tc.name, code, e, tc.says)
		}
	}
	if _, e := c.failsIn(storeQuestion, "question", "add"); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "no session") {
		t.Errorf("add with no session = %v, want INVALID_ARGS", e)
	}
	c.ok(nil, "session", "end", "-s", "calm-heron")
	if _, e := c.failsIn(storeQuestion, "question", "add", "-s", "calm-heron"); e.Code != api.CodeNotFound || !strings.Contains(e.Message, "ended") {
		t.Errorf("add from an ended session = %v, want NOT_FOUND", e)
	}

	c.ask("brave-otter", storeQuestion)
	for _, tc := range []struct {
		name   string
		answer *pb.Answer
		says   string
	}{
		{"nothing", &pb.Answer{Notes: "hm"}, "picks an option or says something"},
		{"not an option", &pb.Answer{Picked: []string{"MySQL"}}, `no option "MySQL"`},
		{"two of one", &pb.Answer{Picked: []string{"Postgres", "SQLite (Recommended)"}}, "takes one option"},
	} {
		_, err := user.AnswerQuestion(t.Context(), &pb.AnswerQuestionRequest{Id: 1, Answer: tc.answer})
		if e := api.FromConnect(err); e == nil || e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, tc.says) {
			t.Errorf("answer %s = %v, want INVALID_ARGS saying %q", tc.name, err, tc.says)
		}
	}
	if q := c.question("1"); q.State != pb.QuestionState_QUESTION_STATE_OPEN || len(q.History) != 1 {
		t.Errorf("refused answers changed question 1: %v", q)
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
	if got := c.listed("--all-projects"); !slices.Equal(got, []int32{2, 1}) {
		t.Errorf("ls --all-projects = %v", got)
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
	c.ok(outbox, "question", "wait", "-p", "other", "--timeout", "5")
	if !slices.Equal(ids(outbox.Questions), []int32{2}) {
		t.Errorf("wait -p other = %v", ids(outbox.Questions))
	}
	projects := &pb.ListQuestionProjectsResponse{}
	c.ok(projects, "question", "projects")
	if got := projects.Projects; len(got) != 2 || got[0].Name != "other" || got[0].Outbox != 1 || got[0].Open != 0 ||
		got[1].Name != "test" || got[1].Open != 1 || got[1].Total != 1 {
		t.Errorf("question projects = %v", got)
	}
}
