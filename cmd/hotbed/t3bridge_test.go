package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/t3bridge"
)

type bridgeTestSent struct {
	ThreadID string
	Message  string
}

type bridgeTestT3 struct {
	mu     sync.Mutex
	states map[string]t3bridge.ThreadState
	sent   []bridgeTestSent
	sentCh chan bridgeTestSent
}

func newBridgeTestT3() *bridgeTestT3 {
	return &bridgeTestT3{
		states: map[string]t3bridge.ThreadState{},
		sentCh: make(chan bridgeTestSent, 16),
	}
}

func (f *bridgeTestT3) set(tid string, st t3bridge.ThreadState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[tid] = st
}

func (f *bridgeTestT3) ThreadStatus(_ context.Context, threadID string) (t3bridge.ThreadState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[threadID]
	if !ok {
		return t3bridge.ThreadState{Exists: false}, nil
	}
	return st, nil
}

func (f *bridgeTestT3) SendMessage(_ context.Context, threadID string, message string) error {
	f.mu.Lock()
	w := bridgeTestSent{ThreadID: threadID, Message: message}
	f.sent = append(f.sent, w)
	f.mu.Unlock()
	f.sentCh <- w
	return nil
}

func TestT3BridgeEndToEndWithControllerDO(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "project", "add", "bridgeproj")

	// Start a T3-linked session and an unlinked session.
	t.Setenv("T3CODE_THREAD_ID", "thread-e2e-1")
	c.ok(nil, "session", "start", "s-t3", "-p", "bridgeproj")

	t.Setenv("T3CODE_THREAD_ID", "thread-e2e-remote")
	c.ok(nil, "session", "start", "s-remote", "-p", "bridgeproj")

	t.Setenv("T3CODE_THREAD_ID", "")
	c.ok(nil, "session", "start", "s-plain", "-p", "bridgeproj")

	addQ := func(sess, header, q string) int32 {
		t.Helper()
		payload := fmt.Sprintf(`{"questions":[{"question":%q,"header":%q,"options":[{"label":"Yes","description":"Do it","implemented":true},{"label":"No","description":"Skip it"}]}]}`, q, header)
		qs := c.ask(sess, payload)
		if len(qs) != 1 {
			t.Fatalf("expected 1 question, got %d", len(qs))
		}
		return qs[0].Id
	}

	q1 := addQ("s-t3", "DB", "Use SQLite?")
	q2 := addQ("s-t3", "Cache", "Add cache?")
	qRemote := addQ("s-remote", "Remote", "Hosted elsewhere?")
	qPlain := addQ("s-plain", "Plain", "No T3 thread?")

	t3 := newBridgeTestT3()
	// thread-e2e-1 starts busy; thread-e2e-remote does not exist on this machine.
	t3.set("thread-e2e-1", t3bridge.ThreadState{Exists: true, Status: "running"})

	stateFile := filepath.Join(t.TempDir(), "t3-bridge-state.json")
	store := &t3bridge.StateStore{Path: stateFile}

	bridge, err := t3bridge.New(t3bridge.Config{
		ControllerURL: c.ctrlSrv.URL,
		TokenFunc:     func(context.Context) (string, error) { return c.harness.token, nil },
		HTTPClient:    c.cli.stream,
		T3:            t3,
		State:         store,
		Debounce:      60 * time.Millisecond,
		PollInterval:  25 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("t3bridge.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx) }()
	var stop1Once sync.Once
	stop1 := func() {
		stop1Once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("bridge.Run: %v", err)
			}
		})
	}
	defer stop1()

	// Wait until the bridge WebSocket is connected to ControllerDO.
	c.eventually("question bridge connected", func() bool {
		var res struct {
			Count int `json:"count"`
		}
		if err := c.harness.doJSON(context.Background(), c.doName, "GET", "/__test/inspect?kind=connectedQuestionBridges", nil, &res); err != nil {
			return false
		}
		return res.Count == 1
	})

	// 1. Answer q1, q2, qRemote, and qPlain while thread-e2e-1 is busy.
	c.ok(nil, "question", "answer", fmt.Sprintf("%d", q1), "--pick", "Yes", "--notes", "ship it")
	c.ok(nil, "question", "dismiss", fmt.Sprintf("%d", q2), "--reason", "handled differently")
	c.ok(nil, "question", "answer", fmt.Sprintf("%d", qRemote), "--pick", "No")
	c.ok(nil, "question", "answer", fmt.Sprintf("%d", qPlain), "--pick", "Yes")

	// While busy, no wake message is sent.
	select {
	case w := <-t3.sentCh:
		t.Fatalf("unexpected wake while thread is busy: %+v", w)
	case <-time.After(150 * time.Millisecond):
	}

	// 2. Agent polls and acks q2 during its busy turn -> only q1 remains when idle!
	c.ok(nil, "question", "ack", "-s", "s-t3", fmt.Sprintf("%d", q2))
	time.Sleep(50 * time.Millisecond)

	// Transition thread-e2e-1 to idle -> bridge sends wake message for q1 only.
	t3.set("thread-e2e-1", t3bridge.ThreadState{Exists: true, Status: "completed"})

	select {
	case w := <-t3.sentCh:
		if w.ThreadID != "thread-e2e-1" {
			t.Fatalf("wake thread = %q, want thread-e2e-1", w.ThreadID)
		}
		if !strings.Contains(w.Message, fmt.Sprintf("#%d (DB)", q1)) || !strings.Contains(w.Message, "ship it") {
			t.Fatalf("wake message missing q1 details:\n%s", w.Message)
		}
		if strings.Contains(w.Message, fmt.Sprintf("#%d", q2)) {
			t.Fatalf("wake message should have suppressed acked q2:\n%s", w.Message)
		}
		if !strings.Contains(w.Message, fmt.Sprintf("hotbed question ack %d", q1)) {
			t.Fatalf("wake message missing ack instruction:\n%s", w.Message)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for wake message after thread became idle")
	}

	// 3. Revise q1's answer -> higher revision triggers a new wake message once idle.
	c.ok(nil, "question", "answer", fmt.Sprintf("%d", q1), "--pick", "No", "--notes", "actually skip")
	select {
	case w := <-t3.sentCh:
		if !strings.Contains(w.Message, "**Picked:** No") || !strings.Contains(w.Message, "actually skip") {
			t.Fatalf("revised answer wake missing updated pick/notes:\n%s", w.Message)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for revised answer wake message")
	}

	// 4. Restart bridge with same persisted state file -> snapshot resync must NOT double-notify q1.
	stop1()

	t3Restart := newBridgeTestT3()
	t3Restart.set("thread-e2e-1", t3bridge.ThreadState{Exists: true, Status: "completed"})
	bridge2, err := t3bridge.New(t3bridge.Config{
		ControllerURL: c.ctrlSrv.URL,
		TokenFunc:     func(context.Context) (string, error) { return c.harness.token, nil },
		HTTPClient:    c.cli.stream,
		T3:            t3Restart,
		State:         store,
		Debounce:      40 * time.Millisecond,
		PollInterval:  20 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("t3bridge.New (restart): %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- bridge2.Run(ctx2) }()
	defer func() {
		cancel2()
		<-done2
	}()

	select {
	case w := <-t3Restart.sentCh:
		t.Fatalf("unexpected duplicate notification after bridge restart: %+v", w)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestT3BridgeCLICommands(t *testing.T) {
	c := newCluster(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	// Status works in JSON and plain text modes.
	var st t3bridge.ServiceStatus
	c.ok(&st, "t3-bridge", "status", "--state-file", statePath)
	if st.StateFile != statePath {
		t.Fatalf("status stateFile = %q, want %q", st.StateFile, statePath)
	}

	code, plain, stderr := c.hotbed("t3-bridge", "status", "--state-file", statePath)
	if code != exitOK || !strings.Contains(plain, "installed=") {
		t.Fatalf("plain t3-bridge status: code=%d stdout=%q stderr=%q", code, plain, stderr)
	}

	// Default subcommand ("run") parses flags directly on `hotbed t3-bridge`.
	code, _, stderr = c.hotbed("t3-bridge", "--debounce", "0s")
	if code == exitOK || !strings.Contains(stderr, "--debounce must be positive") {
		t.Fatalf("expected --debounce validation error, got code=%d stderr=%q", code, stderr)
	}
}
