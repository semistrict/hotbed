package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

// commandOf is the session's command id, as hotbed ps lists it.
func (c *cluster) commandOf(session, id string) *pb.Command {
	c.t.Helper()
	list := &pb.ListCommandsResponse{}
	c.ok(list, "ps", "-s", session)
	for _, cmd := range list.Commands {
		if cmd.Id == id {
			return cmd
		}
	}
	c.t.Fatalf("hotbed ps -s %s lists no %s", session, id)
	return nil
}

// runWith runs a command attached in session's workspace of app with run's
// flags, and returns its ID and how run exited.
func (c *cluster) runWith(session string, flags []string, args ...string) (id string, code int) {
	c.t.Helper()
	code, _, stderr := c.hotbed(append(append(append([]string{"run", "-s", session, "-r", "app"}, flags...), "--"), args...)...)
	m := started.FindStringSubmatch(stderr)
	if m == nil {
		c.t.Fatalf("run %v started nothing: exit %d, stderr %q", args, code, stderr)
	}
	return m[1], code
}

// eventWhats lists what happened to a command in its session's events.
func eventWhats(evs []*pb.SessionEvent, cmd string) []string {
	var out []string
	for _, e := range evs {
		if what, id := api.EventWhat(e); id == cmd && e.GetCommand() != nil {
			out = append(out, what)
		}
	}
	return out
}

// A command still running at its soft limit is paused and the attached run
// returns, exiting 124 and saying how to go on; waiting on it again with
// logs -f resumes it, and exits with its status once it ends.
func TestSoftLimitPausesAndWaitingResumes(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "slow-ox")
	c.workspace("slow-ox", nil)

	code, stdout, stderr := c.hotbed("run", "-s", "slow-ox", "-r", "app", "--soft-limit", "1s", "--",
		"sh", "-c", "echo start; while [ ! -e go ]; do sleep 0.1; done; echo done; exit 5")
	m := started.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("run started nothing: exit %d, stderr %q", code, stderr)
	}
	id := m[1]
	if want := "hotbed: " + id + " is paused: it ran 1s, its soft limit, without ending. To wait on it 1s more, which resumes it: hotbed logs -f -s slow-ox " + id +
		" (its hard limit stops it 2h0m0s after it started). To end it: hotbed stop -s slow-ox " + id + "\n"; code != exitPaused || stdout != "start\n" || !strings.HasSuffix(stderr, want) {
		t.Fatalf("run past its soft limit: exit %d, stdout %q, stderr %q; want exit 124, start, and stderr ending %q", code, stdout, stderr, want)
	}
	cmd := c.commandOf("slow-ox", id)
	wantReason := "paused: it ran 1s, its soft time limit, without ending; waiting on it again (hotbed logs -f -s slow-ox " + id + ") resumes it"
	if cmd.State != pb.CommandState_COMMAND_STATE_PAUSED || cmd.Reason != wantReason {
		t.Errorf("the paused command: %v, %q; want %q", cmd.State, cmd.Reason, wantReason)
	}

	// The paused command does not see go until it resumes. Executing the exact
	// resume command advertised in cmd.Reason must work verbatim even without
	// ambient session binding (e.g. from another session/thread context).
	c.ok(nil, "run", "-s", "slow-ox", "-r", "app", "--detach", "--mem", "1G", "--", "touch", "go")
	startIdx := strings.Index(cmd.Reason, "(hotbed ")
	endIdx := strings.Index(cmd.Reason, ") resumes it")
	if startIdx < 0 || endIdx <= startIdx {
		t.Fatalf("paused reason missing resume command hint: %q", cmd.Reason)
	}
	hintArgs := strings.Fields(cmd.Reason[startIdx+1 : endIdx])[1:] // strip leading "hotbed"
	code, stdout, stderr = c.hotbed(hintArgs...)
	if code != 5 || stdout != "start\ndone\n" {
		t.Fatalf("running hint %q of the paused command: exit %d, stdout %q, stderr %q; want exit 5 with all its output", hintArgs, code, stdout, stderr)
	}
	c.eventually("its end in the session", func() bool { return len(ends(c.events("slow-ox"), id)) > 0 })
	if got := eventWhats(c.events("slow-ox"), id); !slices.Equal(got, []string{"admitted", "paused", "resumed", "exited"}) {
		t.Errorf("events of %s = %q", id, got)
	}
}

// The soft limit counts again from each time an agent starts waiting on the
// command: a resumed command runs a whole soft limit more before it pauses
// again.
func TestSoftLimitCountsAgainFromEachWait(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "keen-gnu")
	c.workspace("keen-gnu", nil)
	id, code := c.runWith("keen-gnu", []string{"--soft-limit", "2s"}, "sh", "-c", "while :; do echo tick; sleep 0.1; done")
	if code != exitPaused {
		t.Fatalf("run past its soft limit: exit %d, want 124", code)
	}
	_, before, _ := c.hotbed("logs", "-s", "keen-gnu", id)
	start := time.Now()
	code, after, stderr := c.hotbed("logs", "-f", "-s", "keen-gnu", id)
	if took := time.Since(start); code != exitPaused || took < 1500*time.Millisecond {
		t.Fatalf("logs -f of the paused command: exit %d after %s (stderr %q); want 124 after its 2s soft limit", code, took, stderr)
	}
	if n, m := strings.Count(before, "tick"), strings.Count(after, "tick"); m < n+10 {
		t.Errorf("%d ticks before it resumed and %d after, want it to have run on meanwhile", n, m)
	}
	c.ok(nil, "stop", "-s", "keen-gnu", id)
	c.eventually("its end in the session", func() bool { return len(ends(c.events("keen-gnu"), id)) > 0 })
	if got := eventWhats(c.events("keen-gnu"), id); !slices.Equal(got, []string{"admitted", "paused", "resumed", "paused", "cancelled"}) {
		t.Errorf("events of %s = %q", id, got)
	}
}

// signalWriter buffers written output and closes done once want appears.
type signalWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	done chan struct{}
	once sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.done) })
	}
	w.mu.Unlock()
	return n, err
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// frozenClock holds a fixed instant that moves only when advance is called, so
// wall-clock jitter during command startup cannot advance a worker's clock
// (board 9592).
type frozenClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFrozenClock(t time.Time) *frozenClock {
	return &frozenClock{now: t}
}

func (c *frozenClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *frozenClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// A command its soft limit paused and no one came back for is stopped at
// its hard limit.
func TestHardLimitStopsAPausedCommand(t *testing.T) {
	t.Parallel()
	// Drive the worker's clock with a frozen stepped clock and wide limits
	// (10s soft, 10m hard) so wall-clock jitter during shell startup or CPU
	// load can never advance limits before the attached client observes
	// "ready\n" and the soft-limit pause (board 9409, board 9592).
	workerClock := newFrozenClock(time.Now())
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Now = workerClock.Now })
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "lost-elk")
	c.workspace("lost-elk", nil)

	out := &signalWriter{want: "ready\n", done: make(chan struct{})}
	type runResult struct {
		code   int
		stderr string
	}
	runDone := make(chan runResult, 1)
	go func() {
		var stderr bytes.Buffer
		d := c.cli
		d.stdin, d.stdout, d.stderr = strings.NewReader(""), out, &stderr
		code := run(t.Context(), []string{
			"run", "-s", "lost-elk", "-r", "app",
			"--soft-limit", "10s", "--hard-limit", "10m",
			"--", "sh", "-c", "echo ready; exec sleep 300",
		}, d)
		runDone <- runResult{code: code, stderr: stderr.String()}
	}()

	// Wait until FollowLog has attached, called WaitOn, and streamed "ready\n".
	select {
	case <-out.done:
	case r := <-runDone:
		t.Fatalf("run exited before becoming ready: exit %d, stderr %q", r.code, r.stderr)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for command output")
	}

	// Past the 10s soft limit (and under orphanAfter=30s and hard limit=10m).
	workerClock.advance(11 * time.Second)
	var r runResult
	select {
	case r = <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for soft-limit pause")
	}
	m := started.FindStringSubmatch(r.stderr)
	if m == nil {
		t.Fatalf("run started nothing: exit %d, stderr %q", r.code, r.stderr)
	}
	id := m[1]
	if r.code != exitPaused {
		t.Fatalf("run past its soft limit: exit %d (stderr %q), want 124", r.code, r.stderr)
	}

	// Past the 10m hard limit: the paused command is stopped.
	workerClock.advance(10 * time.Minute)
	c.eventually("its end", func() bool { return len(ends(c.events("lost-elk"), id)) > 0 })
	if got := ends(c.events("lost-elk"), id); !slices.Equal(got, []string{"cancelled its hard time limit of 10m0s passed"}) {
		t.Errorf("ends of %s = %q", id, got)
	}
	code, _, stderr := c.hotbed("logs", "-f", "-s", "lost-elk", id)
	if want := "hotbed: INTERNAL: " + id + " was cancelled: its hard time limit of 10m0s passed\n"; code != exitError || stderr != want {
		t.Errorf("logs -f of it: exit %d, stderr %q; want exit 1, %q", code, stderr, want)
	}
}

// An attached run has the default limits; a detached one none unless given,
// and a soft limit brings the default hard limit; a measurement takes no
// soft limit, and limits are whole seconds.
func TestTimeLimitDefaultsAndFlags(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "neat-cod")
	c.workspace("neat-cod", nil)
	c.heartbeat() // keep the node online after a slow cold build under load (board 7949)

	attached, _ := c.runAttached("neat-cod", "true")
	detached := &pb.Command{}
	c.ok(detached, "run", "-s", "neat-cod", "-r", "app", "--detach", "--mem", "1G", "--", "true")
	given := &pb.Command{}
	c.ok(given, "run", "-s", "neat-cod", "-r", "app", "--detach", "--mem", "1G", "--soft-limit", "30m", "--", "true")
	unbounded := &pb.Command{}
	c.ok(unbounded, "run", "-s", "neat-cod", "-r", "app", "--soft-limit", "0", "--hard-limit", "0", "--detach", "--mem", "1G", "--", "true")
	for _, tc := range []struct {
		id         string
		soft, hard int64
	}{{attached, 600, 7200}, {detached.Id, 0, 0}, {given.Id, 1800, 7200}, {unbounded.Id, 0, 0}} {
		if l := c.commandOf("neat-cod", tc.id).Limits; l.SoftLimitSeconds != tc.soft || l.HardLimitSeconds != tc.hard {
			t.Errorf("%s: soft %ds, hard %ds; want %ds, %ds", tc.id, l.SoftLimitSeconds, l.HardLimitSeconds, tc.soft, tc.hard)
		}
	}

	// --keep-idle keeps a command idle on much memory running; without it,
	// one is stopped as stuck.
	kept := &pb.Command{}
	c.ok(kept, "run", "-s", "neat-cod", "-r", "app", "--detach", "--mem", "1G", "--keep-idle", "--", "true")
	if !c.commandOf("neat-cod", kept.Id).Limits.KeepIdle {
		t.Errorf("%s run with --keep-idle has no keep_idle", kept.Id)
	}
	if c.commandOf("neat-cod", detached.Id).Limits.KeepIdle {
		t.Errorf("%s run without --keep-idle has keep_idle", detached.Id)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--bench", "--soft-limit", "5m"}, "a measurement has no soft limit: pausing it would spoil its numbers"},
		{[]string{"--soft-limit", "1.5s"}, `--soft-limit "1.5s": not whole seconds`},
		{[]string{"--hard-limit=-1h"}, `--hard-limit "-1h": not whole seconds`},
		{[]string{"--hard-limit", "soon"}, `--hard-limit "soon": time: invalid duration "soon"`},
		{[]string{"--soft-limit", "5m", "--hard-limit", "0"}, "a command with a soft limit needs a hard limit: paused, and no one coming back for it, it would hold its swap forever"},
	} {
		code, e := c.fails(append(append([]string{"run", "-s", "neat-cod", "-r", "app"}, tc.args...), "--", "true")...)
		if code != exitError || e.Code != api.CodeInvalidArgs || e.Message != tc.want {
			t.Errorf("run %v: exit %d, %+v; want INVALID_ARGS %q", tc.args, code, e, tc.want)
		}
	}
}

// logs -f waits on a command and exits with its status, as attached run
// does: a shell's 128 plus the signal for one a signal killed.
func TestLogsFollowExitsWithTheCommandsStatus(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "glad-asp")
	c.workspace("glad-asp", nil)
	for script, want := range map[string]int{"echo seven; exit 7": 7, "true": 0, "kill -TERM $$": 143} {
		cmd := &pb.Command{}
		c.ok(cmd, "run", "-s", "glad-asp", "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", script)
		if code, _, stderr := c.hotbed("logs", "-f", "-s", "glad-asp", cmd.Id); code != want {
			t.Errorf("logs -f of %q: exit %d (stderr %q), want %d", script, code, stderr, want)
		}
	}
}

// When systemd OOM-kills a command (even if DefaultOOMPolicy=stop sent SIGTERM
// to the main process), logs -f reports the OOM kill on stderr and exits 137
// rather than a silent 143 (board 7232).
func TestLogsFollowReportsOOMKill(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "bold-yak")
	c.workspace("bold-yak", nil)
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "bold-yak", "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "60")
	c.eventually("command running", func() bool {
		got, ok := c.engine().Command(cmd.Id)
		return ok && got.State == pb.CommandState_COMMAND_STATE_RUNNING
	})
	if err := c.nodes[0].units.OOMKill("hotbed-"+cmd.Id, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := c.hotbed("logs", "-f", "-s", "bold-yak", cmd.Id)
	if code != 137 {
		t.Errorf("logs -f after OOM kill: exit %d (stderr %q), want 137", code, stderr)
	}
	if !strings.Contains(stderr, "killed by the OOM killer") {
		t.Errorf("logs -f stderr = %q, want OOM killer explanation", stderr)
	}
}

// A command's worker forgets it a while after it ends (keepEnded), but its
// log stays and history keeps how it ended: logs, logs -n and logs -f still
// read it, and logs -f still exits with its status (board 6174: an agent
// coming back to a command it ran detached found "no command on this node").
func TestLogsOfACommandItsWorkerForgot(t *testing.T) {
	t.Parallel()
	workerClock := &clock{}
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Now = workerClock.Now })
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "keen-elk")
	c.workspace("keen-elk", nil)
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "keen-elk", "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", "echo kept; exit 3")
	if code, _, stderr := c.hotbed("logs", "-f", "-s", "keen-elk", cmd.Id); code != 3 {
		t.Fatalf("logs -f while its worker has it: exit %d (stderr %q), want 3", code, stderr)
	}
	c.eventually("history to have it", func() bool {
		_, ok, err := c.store.Finished(t.Context(), cmd.Id)
		return err == nil && ok
	})
	workerClock.advance(3 * time.Minute)
	c.eventually("its worker to forget it", func() bool {
		_, ok := c.engine().Command(cmd.Id)
		return !ok
	})

	for _, tc := range []struct {
		args     []string
		wantCode int
	}{
		{[]string{"logs", "-s", "keen-elk", cmd.Id}, 0},
		{[]string{"logs", "-n", "1", "-s", "keen-elk", cmd.Id}, 0},
		{[]string{"logs", "-f", "-s", "keen-elk", cmd.Id}, 3},
	} {
		if code, stdout, stderr := c.hotbed(tc.args...); code != tc.wantCode || stdout != "kept\n" {
			t.Errorf("hotbed %v: exit %d, stdout %q, stderr %q; want exit %d and %q", tc.args, code, stdout, stderr, tc.wantCode, "kept\n")
		}
	}
	// Stopping it says how it ended, as stopping an ended command does.
	stopped := &pb.Command{}
	c.ok(stopped, "stop", "-s", "keen-elk", cmd.Id)
	if stopped.State != pb.CommandState_COMMAND_STATE_FAILED || stopped.GetExitCode() != 3 || stopped.Argv != "sh -c 'echo kept; exit 3'" {
		t.Errorf("stop after its worker forgot it = %s exit %d %q, want FAILED exit 3 %q", stopped.State, stopped.GetExitCode(), stopped.Argv, "sh -c 'echo kept; exit 3'")
	}
}

// bounce takes the controller down as a restart does, its port refusing
// calls and its connections cut, and brings it back on the same address
// after down.
func (c *cluster) bounce(down time.Duration) {
	c.bounceUntil(func() { time.Sleep(down) })
}

// bounceUntil is bounce, bringing the controller back once wait returns (or
// the test ends).
func (c *cluster) bounceUntil(wait func()) {
	addr := c.ctrlSrv.Listener.Addr().String()
	if c.ctrlBack != nil {
		c.ctrlBack.Close()
	} else {
		c.ctrlSrv.Listener.Close()
		c.ctrlSrv.CloseClientConnections()
	}
	srv := &http.Server{Handler: c.ctrl}
	c.ctrlBack = srv
	back := make(chan struct{})
	c.t.Cleanup(func() {
		<-back
		srv.Close()
	})
	go func() {
		defer close(back)
		waited := make(chan struct{})
		go func() { defer close(waited); wait() }()
		select {
		case <-waited:
		case <-c.t.Context().Done():
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			c.t.Errorf("the controller's address after its restart: %v", err)
			return
		}
		go srv.Serve(l)
	}()
}

// saying is a writer that closes said once what it was written says what.
type saying struct {
	what string
	said chan struct{}
	mu   sync.Mutex
	buf  strings.Builder
}

func (s *saying) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.Contains(s.buf.String(), s.what) {
		s.buf.Write(p)
		if strings.Contains(s.buf.String(), s.what) {
			close(s.said)
		}
	}
	return len(p), nil
}

// Calls to a controller that is restarting wait for it, saying so, rather
// than failing: a call that never reached it (a session's first run, which
// has nowhere else to go) and idempotent ones (ps, wait) alike.
func TestCallsRideOutAControllerRestart(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.cli.rideOut = time.Minute
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "calm-yak")

	for _, args := range [][]string{
		{"run", "-s", "calm-yak", "-r", "app", "--", "true"},
		{"ps", "-s", "calm-yak"},
		{"poll", "-s", "calm-yak", "--timeout", "1"},
	} {
		// The controller stays down until the call has found it out of
		// reach, however long the CLI takes to get there.
		said := &saying{what: "is out of reach", said: make(chan struct{})}
		cameBack := make(chan time.Time, 1)
		c.bounceUntil(func() {
			<-said.said
			cameBack <- time.Now()
		})
		var stdout, stderr bytes.Buffer
		d := c.cli
		d.stdin, d.stdout, d.stderr = strings.NewReader(""), &stdout, io.MultiWriter(&stderr, said)
		code := run(t.Context(), args, d)
		ended := time.Now()
		if code != exitOK || !strings.Contains(stderr.String(), "is out of reach") || !strings.Contains(stderr.String(), "waiting up to 1m0s for it to come back") {
			t.Errorf("hotbed %v across a restart: exit %d, stdout %q, stderr %q; want it to wait for the controller and succeed", args, code, stdout.String(), stderr.String())
			continue
		}
		if back := <-cameBack; ended.Before(back) {
			t.Errorf("hotbed %v across a restart ended at %s, before the controller came back at %s", args, ended, back)
		}
	}
}

// A poll the controller's restart cuts off is made again once it is back,
// and returns how the session stands at its timeout, rather than failing.
func TestPollCutOffByARestartIsMadeAgain(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.cli.rideOut = time.Minute
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "deft-owl")
	c.workspace("deft-owl", nil)
	c.ok(nil, "poll", "-s", "deft-owl", "--timeout", "1") // the workspace's ready

	pollArrived := make(chan struct{})
	var arrivedOnce sync.Once
	origHandler := *c.handler.Load()
	var wrapped http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hotbed.v1.SessionService/Poll" {
			arrivedOnce.Do(func() { close(pollArrived) })
		}
		origHandler.ServeHTTP(w, r)
	})
	c.handler.Store(&wrapped)

	type result struct {
		code           int
		stdout, stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, stdout, stderr := c.hotbed("--json", "poll", "-s", "deft-owl", "--timeout", "2")
		done <- result{code, stdout, stderr}
	}()
	select {
	case <-pollArrived:
	case <-time.After(15 * time.Second):
		t.Fatal("poll never reached the controller")
	}
	time.Sleep(50 * time.Millisecond)
	c.bounce(300 * time.Millisecond)
	start := time.Now()
	select {
	case r := <-done:
		if r.code != exitOK || !strings.Contains(r.stdout, `"events": []`) || time.Since(start) > 20*time.Second {
			t.Errorf("poll cut off by a restart: exit %d, stdout %q, stderr %q; want how the session stands", r.code, r.stdout, r.stderr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("poll cut off by a restart never returned")
	}
}

// stuckThaw is units that cannot thaw.
type stuckThaw struct{ worker.Units }

func (stuckThaw) Thaw(context.Context, string) error { return errors.New("the freezer is stuck") }

// A wait that cannot resume its paused command fails, saying why, rather
// than reporting the pause again (exit 124) for the agent to wait once more.
func TestAWaitThatCannotResumeFails(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Units = stuckThaw{cfg.Units} })
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "numb-eel")
	c.workspace("numb-eel", nil)
	id, code := c.runWith("numb-eel", []string{"--soft-limit", "1s"}, "sleep", "300")
	if code != exitPaused {
		t.Fatalf("run past its soft limit: exit %d, want 124", code)
	}
	code, _, stderr := c.hotbed("logs", "-f", "-s", "numb-eel", id)
	if want := "hotbed: INTERNAL: resume " + id + ": the freezer is stuck\n"; code != exitError || stderr != want {
		t.Errorf("logs -f of a command that cannot resume: exit %d, stderr %q; want exit 1, %q", code, stderr, want)
	}
}

// A base build shows in every session's commands and its project's, so a
// parked one can be seen, but in no session's own (board 7422).
func TestPsShowsBaseBuilds(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "deft-owl")
	c.workspace("deft-owl", nil)
	freshens := func(args ...string) []*pb.Command {
		var ls pb.ListCommandsResponse
		c.ok(&ls, append([]string{"ps"}, args...)...)
		return slices.DeleteFunc(ls.Commands, func(c *pb.Command) bool { return c.Kind != "freshen" })
	}
	all := freshens("--all")
	if len(all) != 1 || all[0].Project == "" {
		t.Fatalf("ps --all lists base builds %v, want the workspace's base's, of its project", all)
	}
	if got := freshens("-p", all[0].Project); len(got) != 1 {
		t.Errorf("ps -p %s lists %d base builds, want its base's", all[0].Project, len(got))
	}
	if got := freshens("-s", "deft-owl"); len(got) != 0 {
		t.Errorf("ps -s deft-owl lists %d base builds, want none", len(got))
	}
}

// --vm-cpus and --vm-mem ask for a VM, so such a run goes only to a node with
// the vm runtime, and a size no VM can have is refused at once.
func TestVMFlagsNeedAVMNode(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "keen-elk")
	c.workspace("keen-elk", nil)

	code, e := c.fails("run", "-s", "keen-elk", "-r", "app", "--vm-cpus=-1", "--", "true")
	if code != exitError || e.Code != api.CodeInvalidArgs || e.Message != "--vm-cpus -1: a VM needs at least 1" {
		t.Errorf("run --vm-cpus -1: exit %d, %+v; want INVALID_ARGS", code, e)
	}
	for _, flags := range [][]string{{"--vm-cpus", "2"}, {"--vm-mem", "2G"}} {
		code, e := c.fails(append(append([]string{"run", "-s", "keen-elk", "-r", "app"}, flags...), "--", "true")...)
		if code != exitError || e.Code != api.CodeInvalidArgs || e.Message != "no node can take the run: asahimbp does not support vm runtime" {
			t.Errorf("run %v on host-runtime nodes: exit %d, %+v; want it refused for want of a vm node", flags, code, e)
		}
	}
}
