package t3bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

type sentWake struct {
	ThreadID string
	Message  string
	At       time.Time
}

type fakeT3 struct {
	mu           sync.Mutex
	states       map[string]ThreadState
	statusErr    map[string]error
	sendErr      map[string]error
	statusCalls  map[string]int
	sent         []sentWake
	onStatusCall func(threadID string, callNum int)
	sentCh       chan sentWake
}

func newFakeT3() *fakeT3 {
	return &fakeT3{
		states:      map[string]ThreadState{},
		statusErr:   map[string]error{},
		sendErr:     map[string]error{},
		statusCalls: map[string]int{},
		sentCh:      make(chan sentWake, 32),
	}
}

func (f *fakeT3) setState(threadID string, st ThreadState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[threadID] = st
	delete(f.statusErr, threadID)
}

func (f *fakeT3) setStatusErr(threadID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.statusErr, threadID)
	} else {
		f.statusErr[threadID] = err
	}
}

func (f *fakeT3) setSendErr(threadID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.sendErr, threadID)
	} else {
		f.sendErr[threadID] = err
	}
}

func (f *fakeT3) statusCount(threadID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusCalls[threadID]
}

func (f *fakeT3) sentList() []sentWake {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentWake, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeT3) ThreadStatus(_ context.Context, threadID string) (ThreadState, error) {
	f.mu.Lock()
	f.statusCalls[threadID]++
	callNum := f.statusCalls[threadID]
	cb := f.onStatusCall
	err := f.statusErr[threadID]
	st, ok := f.states[threadID]
	f.mu.Unlock()

	if cb != nil {
		cb(threadID, callNum)
	}
	if err != nil {
		return ThreadState{}, err
	}
	if !ok {
		return ThreadState{Exists: false}, nil
	}
	return st, nil
}

func (f *fakeT3) SendMessage(_ context.Context, threadID string, message string) error {
	f.mu.Lock()
	if err := f.sendErr[threadID]; err != nil {
		f.mu.Unlock()
		return err
	}
	w := sentWake{ThreadID: threadID, Message: message, At: time.Now()}
	f.sent = append(f.sent, w)
	f.mu.Unlock()
	f.sentCh <- w
	return nil
}

func waitSent(t *testing.T, f *fakeT3, timeout time.Duration) sentWake {
	t.Helper()
	select {
	case w := <-f.sentCh:
		return w
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for wake message", timeout)
		return sentWake{}
	}
}

func assertNoSent(t *testing.T, f *fakeT3, wait time.Duration) {
	t.Helper()
	select {
	case w := <-f.sentCh:
		t.Fatalf("unexpected wake message sent to %s:\n%s", w.ThreadID, w.Message)
	case <-time.After(wait):
	}
}

func answeredQ(id int32, rev int64, threadID, header, question string, picked []string, other, notes string) *pb.Question {
	return &pb.Question{
		Id:             id,
		Revision:       rev,
		State:          pb.QuestionState_QUESTION_STATE_ANSWERED,
		T3CodeThreadId: threadID,
		Prompt: &pb.Prompt{
			Header:   header,
			Question: question,
		},
		Answer: &pb.Answer{
			Picked: picked,
			Other:  other,
			Notes:  notes,
		},
	}
}

func dismissedQ(id int32, rev int64, threadID, header, question, reason string) *pb.Question {
	return &pb.Question{
		Id:             id,
		Revision:       rev,
		State:          pb.QuestionState_QUESTION_STATE_DISMISSED,
		T3CodeThreadId: threadID,
		Prompt: &pb.Prompt{
			Header:   header,
			Question: question,
		},
		Dismissed: &pb.Stamp{Reason: reason},
	}
}

func stateQ(id int32, rev int64, threadID string, state pb.QuestionState) *pb.Question {
	return &pb.Question{
		Id:             id,
		Revision:       rev,
		State:          state,
		T3CodeThreadId: threadID,
	}
}

func startTestBridge(t *testing.T, cfg Config) (*Bridge, context.Context, func()) {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New bridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- b.RunLoop(ctx)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("RunLoop error: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return b, ctx, stop
}

func TestBridgeDebounceCoalescesMultipleAnswers(t *testing.T) {
	t3 := newFakeT3()
	t3.setState("t-1", ThreadState{Exists: true, Status: "completed"})
	store := &StateStore{Path: filepath.Join(t.TempDir(), "state.json")}

	b, ctx, _ := startTestBridge(t, Config{
		T3:           t3,
		State:        store,
		Debounce:     80 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
	})

	b.Enqueue(ctx, Event{Kind: EventSnapshot})
	q1 := answeredQ(10, 2, "t-1", "Store", "Which DB?", []string{"SQLite"}, "", "fast")
	q2 := dismissedQ(11, 2, "t-1", "Cache", "Add Redis?", "not needed")

	b.Enqueue(ctx, Event{Kind: EventUpdate, EventName: "answered", Questions: []*pb.Question{q1}})
	time.Sleep(35 * time.Millisecond)
	// Second answer arrives before debounce expires -> extends window and coalesces.
	b.Enqueue(ctx, Event{Kind: EventUpdate, EventName: "dismissed", Questions: []*pb.Question{q2}})

	w := waitSent(t, t3, 2*time.Second)
	if w.ThreadID != "t-1" {
		t.Fatalf("thread = %q, want t-1", w.ThreadID)
	}
	for _, want := range []string{
		"#10 (Store)",
		"SQLite",
		"fast",
		"#11 (Cache)",
		"Dismissed:",
		"not needed",
		"hotbed question ack 10 11",
	} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("wake message missing %q:\n%s", want, w.Message)
		}
	}
	// Only one coalesced message was sent.
	assertNoSent(t, t3, 120*time.Millisecond)
	if calls := t3.statusCount("t-1"); calls != 1 {
		t.Fatalf("expected exactly 1 status check (no polling after notify), got %d", calls)
	}
}

func TestBridgeWaitsUntilIdleAndStopsPolling(t *testing.T) {
	t3 := newFakeT3()
	t3.setState("t-busy", ThreadState{Exists: true, Status: "running"})
	store := &StateStore{Path: filepath.Join(t.TempDir(), "state.json")}

	busyTransitions := []ThreadState{
		{Exists: true, Status: "running"},
		{Exists: true, Status: "preparing"},
		{Exists: true, Status: "starting"},
		{Exists: true, Status: "queued"},
		{Exists: true, Status: "waiting"},
		{Exists: true, Status: "idle", PendingRequestCount: 1},
		{Exists: true, Status: "completed", ActiveRunID: "run-99"},
		{Exists: true, Status: "completed", PendingRequestCount: 0},
	}
	t3.onStatusCall = func(tid string, callNum int) {
		if tid != "t-busy" {
			return
		}
		if callNum < len(busyTransitions) {
			t3.setState("t-busy", busyTransitions[callNum])
		}
	}

	b, ctx, _ := startTestBridge(t, Config{
		T3:           t3,
		State:        store,
		Debounce:     20 * time.Millisecond,
		PollInterval: 15 * time.Millisecond,
	})

	b.Enqueue(ctx, Event{
		Kind: EventSnapshot,
		Questions: []*pb.Question{
			answeredQ(1, 2, "t-busy", "API", "Use Connect?", []string{"Yes"}, "", ""),
		},
	})

	w := waitSent(t, t3, 2*time.Second)
	if w.ThreadID != "t-busy" || !strings.Contains(w.Message, "hotbed question ack 1") {
		t.Fatalf("unexpected wake: %+v", w)
	}
	gotCalls := t3.statusCount("t-busy")
	if gotCalls < len(busyTransitions) {
		t.Fatalf("expected at least %d status polls through busy states, got %d", len(busyTransitions), gotCalls)
	}
	// Verify zero polling after notification completes.
	time.Sleep(80 * time.Millisecond)
	if after := t3.statusCount("t-busy"); after != gotCalls {
		t.Fatalf("continued polling after notification: before=%d after=%d", gotCalls, after)
	}
}

func TestBridgeSuppressesWhenAckedWhileBusy(t *testing.T) {
	t3 := newFakeT3()
	t3.setState("t-ack", ThreadState{Exists: true, Status: "running"})
	store := &StateStore{Path: filepath.Join(t.TempDir(), "state.json")}

	b, ctx, _ := startTestBridge(t, Config{
		T3:           t3,
		State:        store,
		Debounce:     20 * time.Millisecond,
		PollInterval: 15 * time.Millisecond,
	})

	b.Enqueue(ctx, Event{
		Kind: EventSnapshot,
		Questions: []*pb.Question{
			answeredQ(20, 2, "t-ack", "Port", "Which port?", []string{"8080"}, "", ""),
		},
	})

	// Wait until at least one busy status check occurred.
	deadline := time.Now().Add(time.Second)
	for t3.statusCount("t-ack") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if t3.statusCount("t-ack") == 0 {
		t.Fatal("expected at least one busy status check before ack")
	}

	// Agent polls & acks question 20 during its busy turn, then thread becomes idle.
	b.Enqueue(ctx, Event{
		Kind:      EventUpdate,
		EventName: "handled",
		Questions: []*pb.Question{stateQ(20, 3, "t-ack", pb.QuestionState_QUESTION_STATE_HANDLED)},
	})
	time.Sleep(15 * time.Millisecond)
	t3.setState("t-ack", ThreadState{Exists: true, Status: "completed"})

	assertNoSent(t, t3, 120*time.Millisecond)
	callsAfterAck := t3.statusCount("t-ack")
	time.Sleep(80 * time.Millisecond)
	if t3.statusCount("t-ack") != callsAfterAck {
		t.Fatalf("expected zero polling after ack, got %d -> %d", callsAfterAck, t3.statusCount("t-ack"))
	}
}

func TestBridgeSuppressesWhenAckedInFlightDuringStatusCheck(t *testing.T) {
	t3 := newFakeT3()
	t3.setState("t-race", ThreadState{Exists: true, Status: "completed"})
	store := &StateStore{Path: filepath.Join(t.TempDir(), "state.json")}

	inStatus := make(chan struct{}, 1)
	releaseStatus := make(chan struct{})
	t3.onStatusCall = func(tid string, _ int) {
		if tid == "t-race" {
			inStatus <- struct{}{}
			<-releaseStatus
		}
	}

	b, ctx, _ := startTestBridge(t, Config{
		T3:           t3,
		State:        store,
		Debounce:     15 * time.Millisecond,
		PollInterval: 15 * time.Millisecond,
	})

	b.Enqueue(ctx, Event{
		Kind: EventSnapshot,
		Questions: []*pb.Question{
			answeredQ(25, 2, "t-race", "Race", "Check race?", []string{"Yes"}, "", ""),
		},
	})

	select {
	case <-inStatus:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ThreadStatus call")
	}

	// Ack arrives while ThreadStatus is in flight.
	b.Enqueue(ctx, Event{
		Kind:      EventUpdate,
		EventName: "handled",
		Questions: []*pb.Question{stateQ(25, 3, "t-race", pb.QuestionState_QUESTION_STATE_HANDLED)},
	})
	time.Sleep(20 * time.Millisecond)
	close(releaseStatus)

	assertNoSent(t, t3, 120*time.Millisecond)
}

func TestBridgeSuppressesReopenedAndDeletedQuestions(t *testing.T) {
	t3 := newFakeT3()
	t3.setState("t-rd", ThreadState{Exists: true, Status: "completed"})
	store := &StateStore{Path: filepath.Join(t.TempDir(), "state.json")}

	b, ctx, _ := startTestBridge(t, Config{
		T3:           t3,
		State:        store,
		Debounce:     60 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
	})

	b.Enqueue(ctx, Event{
		Kind: EventSnapshot,
		Questions: []*pb.Question{
			answeredQ(30, 2, "t-rd", "Q1", "First?", []string{"A"}, "", ""),
			answeredQ(31, 2, "t-rd", "Q2", "Second?", []string{"B"}, "", ""),
		},
	})
	time.Sleep(15 * time.Millisecond)
	b.Enqueue(ctx, Event{
		Kind:      EventUpdate,
		EventName: "reopened",
		Questions: []*pb.Question{stateQ(30, 3, "t-rd", pb.QuestionState_QUESTION_STATE_OPEN)},
	})
	b.Enqueue(ctx, Event{
		Kind:      EventUpdate,
		EventName: "deleted",
		Questions: []*pb.Question{stateQ(31, 3, "t-rd", pb.QuestionState_QUESTION_STATE_DELETED)},
	})

	assertNoSent(t, t3, 150*time.Millisecond)
}

func TestBridgeRevisionUpdatesAndRestartIdempotency(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "t3-bridge-state.json")
	store := &StateStore{Path: stateFile}

	// First bridge run: notifies Q40@rev2 and persists state.
	t3a := newFakeT3()
	t3a.setState("t-rev", ThreadState{Exists: true, Status: "completed"})
	b1, ctx1, stop1 := startTestBridge(t, Config{
		T3:           t3a,
		State:        store,
		Debounce:     15 * time.Millisecond,
		PollInterval: 15 * time.Millisecond,
	})

	q40v2 := answeredQ(40, 2, "t-rev", "Style", "Tabs or spaces?", []string{"Tabs"}, "", "")
	b1.Enqueue(ctx1, Event{Kind: EventSnapshot, Questions: []*pb.Question{q40v2}})
	w1 := waitSent(t, t3a, time.Second)
	if !strings.Contains(w1.Message, "Tabs") {
		t.Fatalf("first wake missing Tabs:\n%s", w1.Message)
	}
	stop1()

	// Second bridge run (simulating restart + reconnect snapshot with same rev2): MUST NOT double-notify.
	t3b := newFakeT3()
	t3b.setState("t-rev", ThreadState{Exists: true, Status: "completed"})
	b2, ctx2, _ := startTestBridge(t, Config{
		T3:           t3b,
		State:        store,
		Debounce:     15 * time.Millisecond,
		PollInterval: 15 * time.Millisecond,
	})
	b2.Enqueue(ctx2, Event{Kind: EventSnapshot, Questions: []*pb.Question{q40v2}})
	assertNoSent(t, t3b, 100*time.Millisecond)
	if t3b.statusCount("t-rev") != 0 {
		t.Fatalf("expected 0 status checks for already-notified rev2, got %d", t3b.statusCount("t-rev"))
	}

	// User revises answer to rev3 -> MUST trigger a new notification.
	q40v3 := answeredQ(40, 3, "t-rev", "Style", "Tabs or spaces?", []string{"Spaces"}, "", "changed my mind")
	b2.Enqueue(ctx2, Event{Kind: EventUpdate, EventName: "answered", Questions: []*pb.Question{q40v3}})
	w2 := waitSent(t, t3b, time.Second)
	if !strings.Contains(w2.Message, "Spaces") || !strings.Contains(w2.Message, "changed my mind") {
		t.Fatalf("revised wake missing updated answer:\n%s", w2.Message)
	}

	// New question Q41 on the same thread (while Q40@rev3 is still unacked in outbox) -> notifies ONLY Q41.
	q41v2 := answeredQ(41, 2, "t-rev", "Lint", "Run linter?", []string{"Yes"}, "", "")
	b2.Enqueue(ctx2, Event{Kind: EventUpdate, EventName: "answered", Questions: []*pb.Question{q41v2}})
	w3 := waitSent(t, t3b, time.Second)
	if !strings.Contains(w3.Message, "#41") || strings.Contains(w3.Message, "#40") {
		t.Fatalf("expected wake for only #41, got:\n%s", w3.Message)
	}
}

func TestBridgeSkipsUnknownLocalThread(t *testing.T) {
	t3 := newFakeT3()
	// t-remote is NOT registered in fakeT3 -> Exists: false.
	store := &StateStore{Path: filepath.Join(t.TempDir(), "state.json")}

	b, ctx, _ := startTestBridge(t, Config{
		T3:           t3,
		State:        store,
		Debounce:     15 * time.Millisecond,
		PollInterval: 15 * time.Millisecond,
	})

	b.Enqueue(ctx, Event{
		Kind: EventSnapshot,
		Questions: []*pb.Question{
			answeredQ(50, 2, "t-remote", "Other", "On another machine?", []string{"Yes"}, "", ""),
		},
	})

	assertNoSent(t, t3, 120*time.Millisecond)
	if calls := t3.statusCount("t-remote"); calls != 1 {
		t.Fatalf("expected exactly 1 status check for unknown local thread (no polling), got %d", calls)
	}
}

func TestBridgeRetriesTransientT3ServerErrors(t *testing.T) {
	t3 := newFakeT3()
	t3.setStatusErr("t-err", errors.New("No running T3 Code server"))
	store := &StateStore{Path: filepath.Join(t.TempDir(), "state.json")}

	b, ctx, _ := startTestBridge(t, Config{
		T3:              t3,
		State:           store,
		Debounce:        15 * time.Millisecond,
		PollInterval:    20 * time.Millisecond,
		MaxErrorBackoff: 40 * time.Millisecond,
	})

	b.Enqueue(ctx, Event{
		Kind: EventSnapshot,
		Questions: []*pb.Question{
			answeredQ(60, 2, "t-err", "Retry", "Server back?", []string{"Yes"}, "", ""),
		},
	})

	// Wait for at least 2 failed attempts.
	deadline := time.Now().Add(time.Second)
	for t3.statusCount("t-err") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if t3.statusCount("t-err") < 2 {
		t.Fatalf("expected retries on T3 error, got %d calls", t3.statusCount("t-err"))
	}

	// Server starts up and thread is idle -> bridge delivers.
	t3.setState("t-err", ThreadState{Exists: true, Status: "completed"})
	w := waitSent(t, t3, time.Second)
	if w.ThreadID != "t-err" || !strings.Contains(w.Message, "hotbed question ack 60") {
		t.Fatalf("unexpected wake after recovery: %+v", w)
	}
}

func TestBridgeEndToEndWebSocketReconnectAndIdempotency(t *testing.T) {
	var connCount atomic.Int32
	var mu sync.Mutex
	var activeConn *websocket.Conn

	q1 := answeredQ(70, 2, "t-ws", "WS", "Reconnect works?", []string{"Yes"}, "", "")
	q1Raw, err := protojson.Marshal(q1)
	if err != nil {
		t.Fatalf("marshal q1: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/questions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-tok" {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		n := connCount.Add(1)
		mu.Lock()
		activeConn = c
		mu.Unlock()

		// Send initial snapshot with q1 on every connect (testing reconnect idempotency).
		snap, _ := json.Marshal(map[string]any{
			"type":      "snapshot",
			"questions": []json.RawMessage{q1Raw},
		})
		_ = c.Write(r.Context(), websocket.MessageText, snap)

		if n == 2 {
			// On second connection, also push a new question q2.
			q2 := answeredQ(71, 2, "t-ws", "WS2", "Second after reconnect?", []string{"Yep"}, "", "")
			q2Raw, _ := protojson.Marshal(q2)
			upd, _ := json.Marshal(map[string]any{
				"type":      "update",
				"event":     "answered",
				"questions": []json.RawMessage{q2Raw},
			})
			_ = c.Write(r.Context(), websocket.MessageText, upd)
		}

		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if string(data) == "ping" {
				_ = c.Write(r.Context(), websocket.MessageText, []byte("pong"))
			}
		}
	}))
	defer srv.Close()

	t3 := newFakeT3()
	t3.setState("t-ws", ThreadState{Exists: true, Status: "completed"})
	store := &StateStore{Path: filepath.Join(t.TempDir(), "ws-state.json")}

	bridge, err := New(Config{
		ControllerURL: srv.URL,
		TokenFunc:     func(context.Context) (string, error) { return "test-tok", nil },
		HTTPClient:    srv.Client(),
		T3:            t3,
		State:         store,
		Debounce:      20 * time.Millisecond,
		PollInterval:  15 * time.Millisecond,
		PingInterval:  100 * time.Millisecond,
		ReadTimeout:   500 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	// First connection delivers q1.
	w1 := waitSent(t, t3, 2*time.Second)
	if !strings.Contains(w1.Message, "#70") {
		t.Fatalf("expected #70 in first wake:\n%s", w1.Message)
	}

	// Force disconnect on server to trigger backoff + reconnect + snapshot resync.
	mu.Lock()
	c := activeConn
	mu.Unlock()
	if c != nil {
		_ = c.Close(websocket.StatusGoingAway, "simulated drop")
	}

	// Second connection sends snapshot(q1) + update(q2); bridge must NOT re-notify q1, only q2!
	w2 := waitSent(t, t3, 5*time.Second)
	if !strings.Contains(w2.Message, "#71") || strings.Contains(w2.Message, "#70") {
		t.Fatalf("expected only #71 after reconnect, got:\n%s", w2.Message)
	}
	assertNoSent(t, t3, 120*time.Millisecond)
}

func TestCLIClientWrapper(t *testing.T) {
	dir := t.TempDir()
	sentFile := filepath.Join(dir, "sent.txt")
	script := filepath.Join(dir, "fake-t3")
	src := `#!/bin/sh
if [ "$1" = "thread" ] && [ "$2" = "show" ]; then
  tid="$3"
  case "$tid" in
    idle-thread)
      echo '{"thread":{"threadId":"idle-thread","link":"https://app.t3.codes/threads/env-1/idle-thread","status":"completed","activeRunId":null,"pendingRequestCount":0,"blockedOnThreadWait":false,"archived":false},"messages":[],"error":null}'
      exit 0
      ;;
    busy-thread)
      echo '{"thread":{"threadId":"busy-thread","status":"running","activeRunId":"run-1","pendingRequestCount":1,"blockedOnThreadWait":false,"archived":false},"messages":[],"error":null}'
      exit 0
      ;;
    missing-thread)
      echo "Thread missing-thread is no longer available. Thread ids belong to one environment." >&2
      exit 1
      ;;
    down-thread)
      echo "No running T3 Code server uses /tmp/x. Start T3 Code, then retry." >&2
      exit 1
      ;;
  esac
fi
if [ "$1" = "thread" ] && [ "$2" = "send" ]; then
  cat > "` + sentFile + `"
  echo '{"ok":true}'
  exit 0
fi
exit 2
`
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatalf("write fake t3: %v", err)
	}

	cli := &CLIClient{Binary: script}
	ctx := context.Background()

	st, err := cli.ThreadStatus(ctx, "idle-thread")
	if err != nil || !st.Exists || st.IsBusy() || st.Link != "https://app.t3.codes/threads/env-1/idle-thread" {
		t.Fatalf("idle-thread = (%+v, %v), want exists+idle with its link", st, err)
	}

	st, err = cli.ThreadStatus(ctx, "busy-thread")
	if err != nil || !st.Exists || !st.IsBusy() || st.PendingRequestCount != 1 {
		t.Fatalf("busy-thread = (%+v, %v), want exists+busy", st, err)
	}

	st, err = cli.ThreadStatus(ctx, "missing-thread")
	if err != nil || st.Exists {
		t.Fatalf("missing-thread = (%+v, %v), want Exists=false, err=nil", st, err)
	}

	if _, err := cli.ThreadStatus(ctx, "down-thread"); err == nil {
		t.Fatal("down-thread expected transient error, got nil")
	}

	if err := cli.SendMessage(ctx, "idle-thread", "hello from bridge\n"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got, err := os.ReadFile(sentFile)
	if err != nil {
		t.Fatalf("read sent file: %v", err)
	}
	if string(got) != "hello from bridge\n" {
		t.Fatalf("sent stdin = %q, want %q", string(got), "hello from bridge\n")
	}
}

func TestFormatWakeMessage(t *testing.T) {
	q1 := answeredQ(5, 2, "t1", "Auth", "Which auth?", []string{"WorkOS", "API Token"}, "custom", "use org tokens")
	msg := FormatWakeMessage([]*pb.Question{q1})
	for _, want := range []string{
		"Your Hotbed question was answered:",
		"#5 (Auth)",
		"Which auth?",
		"**Picked:** WorkOS, API Token",
		"**Other:** custom",
		"**Notes:** use org tokens",
		"`hotbed question ack 5`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("single answered message missing %q:\n%s", want, msg)
		}
	}

	q2 := dismissedQ(3, 2, "t1", "Skip", "Do this?", "")
	msg2 := FormatWakeMessage([]*pb.Question{q2})
	if !strings.Contains(msg2, "question was dismissed:") || !strings.Contains(msg2, "(no reason given)") || !strings.Contains(msg2, "`hotbed question ack 3`") {
		t.Errorf("single dismissed message unexpected:\n%s", msg2)
	}
}
