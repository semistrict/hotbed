package worker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// jobEngine is an engine with command id started (attached unless
// mutated), whose log holds out.
func jobEngine(t *testing.T, id, out string, reqMut ...func(*pb.RunCommandRequest)) *Engine {
	t.Helper()
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	e := newInterventionEngine(t, t.TempDir(), store, units, nil, &now)
	startTestCommand(t, e, store, units, id, reqMut...)
	writeLog(t, e, id, out)
	return e
}

func writeLog(t *testing.T, e *Engine, id, out string) {
	t.Helper()
	path, err := e.logPath(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLog(t *testing.T, e *Engine, id, out string) {
	t.Helper()
	path, _ := e.logPath(id)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(out); err != nil {
		t.Fatal(err)
	}
}

func texts(q *pb.OutputQuery) []string {
	var out []string
	for _, l := range q.Lines {
		mark := " "
		if l.Match {
			mark = ":"
		}
		out = append(out, mark+l.Text)
	}
	return out
}

func TestQueryOutput(t *testing.T) {
	e := jobEngine(t, "c-q", "compile a\n\x1b[31merror\x1b[0m: one\ncompile b\n")
	q, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-q", Grep: "^error", Context: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(texts(q), "|"), " compile a|:error: one| compile b"; got != want || q.GetMatches() != 1 || q.TotalLines != 3 || q.NextLine != 3 {
		t.Errorf("grep: %q (matches %d, total %d, next %d), want %q, 1, 3, 3", got, q.GetMatches(), q.TotalLines, q.NextLine, want)
	}
	if q.State != running || q.TotalBytes == 0 {
		t.Errorf("state %v, %d bytes; want running and its log's size", q.State, q.TotalBytes)
	}
	if _, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-q", Grep: "("}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("a bad pattern: %v, want INVALID_ARGS", err)
	}
	if _, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-q", Head: 1, Tail: 1}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("head and tail: %v, want INVALID_ARGS", err)
	}
}

func TestQueryOutputWaits(t *testing.T) {
	e := jobEngine(t, "c-w", "starting\n")
	go func() {
		time.Sleep(300 * time.Millisecond)
		appendLog(t, e, "c-w", "listening on :3000\n")
	}()
	q, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-w", Until: "listening", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(q); q.Waited != "matched" || len(got) != 1 || got[0] != ":listening on :3000" {
		t.Fatalf("until: waited %q, lines %q; want matched, the line marked", q.Waited, got)
	}
	// Past --after, nothing new matches: it times out, saying so, with what came.
	q, err = e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-w", Until: "listening", After: q.NextLine, TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if q.Waited != "timeout" {
		t.Fatalf("until past --after: waited %q, want timeout", q.Waited)
	}
	// It ends: the wait ends with it.
	go func() {
		time.Sleep(200 * time.Millisecond)
		if _, err := e.Stop(context.WithoutCancel(t.Context()), "c-w", "done"); err != nil {
			t.Error(err)
		}
	}()
	q, err = e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-w", Wait: true, TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if q.Waited != "ended" || q.State != cancelled {
		t.Fatalf("wait: waited %q state %v, want ended, cancelled", q.Waited, q.State)
	}
}

func TestQueryOutputWaitDrainsOnShutdown(t *testing.T) {
	e := jobEngine(t, "c-drain", "running\n")
	errCh := make(chan error, 1)
	go func() {
		_, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-drain", Wait: true, TimeoutSeconds: 60})
		errCh <- err
	}()
	time.Sleep(100 * time.Millisecond)
	e.DrainStreams()
	select {
	case err := <-errCh:
		ae := api.FromConnect(err)
		if ae == nil || ae.Code != api.CodeWorkerUnreachable || !ae.Retryable {
			t.Fatalf("DrainStreams during QueryOutput --wait = %v, want retryable WORKER_UNREACHABLE", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("QueryOutput --wait did not unblock when DrainStreams was called")
	}
}

func TestQueryOutputSinceSync(t *testing.T) {
	e := jobEngine(t, "c-s", "booted\nGET /old\n")
	e.mu.Lock()
	e.markSyncedLocked("w")
	e.mu.Unlock()
	appendLog(t, e, "c-s", "reloaded\nGET /new\n")
	q, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-s", SinceSync: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(texts(q), "|"), " reloaded| GET /new"; got != want {
		t.Errorf("since sync: %q, want %q", got, want)
	}
}

func TestDetach(t *testing.T) {
	e := jobEngine(t, "c-d", "")
	got, err := e.Detach("c-d")
	if err != nil {
		t.Fatal(err)
	}
	last := got.Timeline[len(got.Timeline)-1]
	if got.Attached || got.Limits.SoftLimitSeconds != 0 || got.Limits.HardLimitSeconds != 7200 || last.What != "detached" {
		t.Fatalf("detached = attached %v, soft %d, hard %d, last event %q; want detached, no soft limit, the hard one kept, said so",
			got.Attached, got.Limits.SoftLimitSeconds, got.Limits.HardLimitSeconds, last.What)
	}
	// No one following it no longer stops it.
	e.mu.Lock()
	e.commands["c-d"].lastFollow = e.now().Add(-time.Hour)
	e.mu.Unlock()
	e.reap(t.Context())
	if c, _ := e.Command("c-d"); c.State != running {
		t.Fatalf("after the reaper: %v, want it still running", c.State)
	}
	if again, err := e.Detach("c-d"); err != nil || again.Attached {
		t.Fatalf("detaching again: %v, %v; want it as it is", again, err)
	}
	if _, err := e.Stop(t.Context(), "c-d", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Detach("c-d"); api.CodeOf(err) != api.CodeConflict {
		t.Fatalf("detaching an ended command: %v, want CONFLICT", err)
	}
}

func TestSendInputNeedsATerminal(t *testing.T) {
	e := jobEngine(t, "c-i", "")
	err := e.SendInput(t.Context(), "c-i", []byte("y\n"))
	if api.CodeOf(err) != api.CodeConflict || !strings.Contains(err.Error(), "does not read its input from a terminal") {
		t.Fatalf("input to a command with no terminal: %v, want CONFLICT saying so", err)
	}
}

func TestTerminalOnTheHost(t *testing.T) {
	e := jobEngine(t, "c-t", "")
	// A detached command on the host runtime gets no terminal; one asked
	// for is refused.
	e.mu.Lock()
	defer e.mu.Unlock()
	if tty, input, err := e.ttyFor(&pb.RunCommandRequest{WorkspaceId: "w"}); tty || input || err != nil {
		t.Errorf("default on the host: %v %v %v, want no terminal", tty, input, err)
	}
	if _, _, err := e.ttyFor(&pb.RunCommandRequest{WorkspaceId: "w", Tty: pb.TtyMode_TTY_MODE_ON}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("--tty on the host: %v, want INVALID_ARGS", err)
	}
}

// Admission charges a quick (attached) command little, a long one what it
// estimated, and one with neither the default.
func TestReservationsByKind(t *testing.T) {
	e := jobEngine(t, "c-quick", "")
	startTestCommand(t, e, &fake.Store{Root: t.TempDir()}, &fake.Units{}, "c-long", func(r *pb.RunCommandRequest) {
		r.Attached = false
		r.Estimate = &pb.CommandEstimate{MemoryBytes: 3 << 30, Seconds: 600}
	})
	startTestCommand(t, e, &fake.Store{Root: t.TempDir()}, &fake.Units{}, "c-bare", func(r *pb.RunCommandRequest) { r.Attached = false })
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, want := range map[string]int64{
		"c-quick": min(quickReservation, e.admitLimit()),
		"c-long":  min(3<<30, e.admitLimit()),
		"c-bare":  min(defaultReservation, e.admitLimit()),
	} {
		if got := e.reservation(e.commands[id]); got != want {
			t.Errorf("%s reservation = %s, want %s", id, gib(got), gib(want))
		}
	}
	if est := e.commands["c-long"].cmd().Estimate; est.GetSource() != "given" {
		t.Errorf("c-long estimate %v, want it kept, given", est)
	}
}

func TestEstimateComparedAtTheEnd(t *testing.T) {
	e := jobEngine(t, "c-e", "", func(r *pb.RunCommandRequest) {
		r.Estimate = &pb.CommandEstimate{MemoryBytes: 4 << 30, Seconds: 60}
	})
	got, err := e.Stop(t.Context(), "c-e", "done")
	if err != nil {
		t.Fatal(err)
	}
	var note string
	for _, ev := range got.Timeline {
		if ev.What == "estimate" {
			note = ev.Detail
		}
	}
	if !strings.Contains(note, "of 4.0 GiB estimated") || !strings.Contains(note, "expected") || !strings.HasSuffix(note, "(given)") {
		t.Fatalf("estimate note %q, want peak and time set beside the estimate", note)
	}
}

// Waiting --until a line since the last sync ignores lines from before it.
func TestQueryOutputUntilSinceSync(t *testing.T) {
	e := jobEngine(t, "c-us", "listening on :1\n")
	e.mu.Lock()
	e.markSyncedLocked("w")
	e.mu.Unlock()
	q, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-us", SinceSync: true, Until: "listening", TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if q.Waited != "timeout" {
		t.Fatalf("until since sync, only an old match: waited %q, want timeout", q.Waited)
	}
	appendLog(t, e, "c-us", "reloading\nlistening on :2\n")
	q, err = e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-us", SinceSync: true, Until: "listening", TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(texts(q), "|"); q.Waited != "matched" || got != ":listening on :2" {
		t.Fatalf("until since sync: waited %q, lines %q; want matched, the new line", q.Waited, got)
	}
}

// A row still being drawn is not passed by next_line: waiting --after it
// sees how it ends.
func TestNextLineStopsBeforeTheRowBeingDrawn(t *testing.T) {
	e := jobEngine(t, "c-n", "start\nbuilding |")
	q, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-n"})
	if err != nil {
		t.Fatal(err)
	}
	if q.TotalLines != 2 || q.NextLine != 1 {
		t.Fatalf("total %d next %d, want 2 and 1", q.TotalLines, q.NextLine)
	}
	appendLog(t, e, "c-n", "\r\x1b[KBuild finished\n")
	q, err = e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-n", Until: "finished", After: q.NextLine, TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if q.Waited != "matched" {
		t.Fatalf("until after next_line: waited %q, want matched", q.Waited)
	}
}
