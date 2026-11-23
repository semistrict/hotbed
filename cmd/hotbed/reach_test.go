package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// When the controller is unreachable, workspaceFor only falls back to lastUsed
// when the requested placement imposes no restrictions incompatible with the
// cached workspace (--node must match, and --arch/--bench/--mem-low refuse to
// silently bypass placement) (board 7894).
func TestSessionWorkspaceForPlacementCheckWhenControllerUnreachable(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "safe-fox")
	c.workspace("safe-fox", nil) // caches asahimbp as lastUsed

	c.ctrl.setDown(true)

	// No placement restriction -> falls back to lastUsed on asahimbp.
	if code, out, stderr := c.hotbed("run", "-s", "safe-fox", "-r", "app", "--", "echo", "ok"); code != 0 || out != "ok\n" {
		t.Fatalf("run without placement offline: code=%d out=%q stderr=%q", code, out, stderr)
	}
	// Matching --node -> falls back to lastUsed on asahimbp.
	if code, out, stderr := c.hotbed("run", "-s", "safe-fox", "-r", "app", "--node", "asahimbp", "--", "echo", "matched"); code != 0 || out != "matched\n" {
		t.Fatalf("run with matching --node offline: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// Incompatible placement restrictions must NOT silently bypass placement.
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"different node", []string{"run", "-s", "safe-fox", "-r", "app", "--node", "mini", "--", "true"}},
		{"arch", []string{"run", "-s", "safe-fox", "-r", "app", "--arch", "arm64", "--", "true"}},
		{"bench", []string{"run", "-s", "safe-fox", "-r", "app", "--bench", "--", "true"}},
		{"mem-low", []string{"run", "-s", "safe-fox", "-r", "app", "--mem-low", "1G", "--", "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := c.fails(tc.args...)
			if e.Code != api.CodeControllerUnreachable {
				t.Errorf("%v when controller is down = %+v, want %s", tc.args, e, api.CodeControllerUnreachable)
			}
		})
	}
}

// When the controller is unreachable, hotbed logs on a base freshen (which has
// SessionId == "") still finds the command via askKnownWorkers (board 7894).
func TestSessionAskKnownWorkersFindsFreshenWhenControllerUnreachable(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "freshen-owl")
	// Establish a known workspace on asahimbp first.
	c.workspace("freshen-owl", nil)

	// Trigger a failing base freshen on the same worker via a second session.
	c.ok(nil, "step", "set", "-p", "app", "broken", "--", "echo broken offline >&2; exit 3")
	c.ok(nil, "session", "start", "other-owl")
	if code, out, stderr := c.hotbed("run", "-s", "other-owl", "-r", "app", "--", "true"); code != 0 {
		t.Fatalf("run triggering freshen: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// Find the failed freshen command ID from the worker's commands.
	var freshenID string
	c.eventually("freshen command recorded on worker", func() bool {
		cp, err := c.engine().Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		for _, cmd := range cp.Commands {
			if strings.HasPrefix(cmd.Id, "f-app-g2-") && cmd.SessionId == "" {
				freshenID = cmd.Id
				return true
			}
		}
		return false
	})

	// Take controller down and read the freshen log via the session with a known worker.
	c.ctrl.setDown(true)
	code, out, stderr := c.hotbed("logs", "-s", "freshen-owl", freshenID)
	if code != 0 || !strings.Contains(out, "broken offline") {
		t.Fatalf("logs of freshen %s with controller down: code=%d out=%q stderr=%q", freshenID, code, out, stderr)
	}
}

// A direct worker RPC that fails because the worker is down must report
// WORKER_UNREACHABLE rather than blaming the controller (board 7894).
func TestSessionDirectWorkerUnreachableReportsWorkerUnreachable(t *testing.T) {
	// Unit checks on api.FromWorker.
	if err := api.FromWorker("worker1:7121", nil); err != nil {
		t.Fatalf("FromWorker(nil) = %v, want nil", err)
	}
	if err := api.FromWorker("worker1:7121", io.EOF); !errors.Is(err, io.EOF) {
		t.Fatalf("FromWorker(EOF) = %v, want io.EOF", err)
	}
	unavail := connect.NewError(connect.CodeUnavailable, "connection refused")
	got := api.FromWorker("worker1:7121", unavail)
	e := api.FromConnect(got)
	if e.Code != api.CodeWorkerUnreachable || !e.Retryable || !strings.Contains(e.Message, "worker1:7121") {
		t.Fatalf("FromWorker(unavailable) = %+v, want WORKER_UNREACHABLE retryable naming worker", e)
	}
	notFound := api.ToConnect(api.Errorf(api.CodeNotFound, "missing"))
	if api.CodeOf(api.FromWorker("worker1:7121", notFound)) != api.CodeNotFound {
		t.Fatalf("FromWorker(NOT_FOUND) changed code to %s", api.CodeOf(api.FromWorker("worker1:7121", notFound)))
	}

	// End-to-end: halt the worker while the controller is down (using lastUsed)
	// and while the controller is up (after GetCommand / CreateWorkspace points to it).
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "down-yak")
	c.workspace("down-yak", nil)

	c.nodes[0].halted.Store(true)
	c.ctrl.setDown(true)
	_, errObj := c.fails("run", "-s", "down-yak", "-r", "app", "--", "true")
	if errObj.Code != api.CodeWorkerUnreachable || !errObj.Retryable {
		t.Fatalf("run against halted worker = %+v, want WORKER_UNREACHABLE (retryable)", errObj)
	}
	if strings.Contains(errObj.Suggestion, "controller") {
		t.Errorf("worker unreachable suggestion blames controller: %q", errObj.Suggestion)
	}
}

// session start must reload-modify-write sessions.json after StartSession
// returns instead of overwriting concurrent edits with the pre-RPC snapshot
// (board 7894).
func TestSessionStartDoesNotClobberConcurrentSessionsFile(t *testing.T) {
	sessionsFile := filepath.Join(t.TempDir(), "sessions.json")
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/StartSession") {
			// Simulate another thread binding a session while StartSession is in flight.
			once.Do(func() {
				if err := updateSessionMap(sessionsFile, func(m sessionMap) {
					m.bind(bindPlace{Source: "thread", Key: "other-thread"}, "other-fox")
				}); err != nil {
					t.Errorf("concurrent updateSessionMap: %v", err)
				}
			})
			b, err := proto.Marshal(&pb.Session{Id: "first-elk", Project: "app"})
			if err != nil {
				t.Fatal(err)
			}
			w.Header().Set("Content-Type", "application/proto")
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	t.Setenv("T3CODE_THREAD_ID", "my-thread")
	t.Setenv("HOTBED_SESSION", "")
	var stdout, stderr bytes.Buffer
	d := deps{
		http:         &http.Client{Timeout: 5 * time.Second},
		stream:       &http.Client{},
		stdin:        strings.NewReader(""),
		stdout:       &stdout,
		stderr:       &stderr,
		controller:   at(strings.TrimPrefix(srv.URL, "http://")),
		cache:        t.TempDir(),
		sessionsFile: sessionsFile,
	}
	code := run(t.Context(), []string{"--json", "session", "start", "first-elk", "-p", "app"}, d)
	if code != 0 {
		t.Fatalf("session start exited %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var env output
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil || !env.OK {
		t.Fatalf("session start envelope = %q (%v)", stdout.String(), err)
	}
	m, err := loadSessionMap(sessionsFile)
	if err != nil {
		t.Fatal(err)
	}
	if m.Threads["my-thread"] != "first-elk" {
		t.Errorf("my-thread = %q, want first-elk", m.Threads["my-thread"])
	}
	if m.Threads["other-thread"] != "other-fox" {
		t.Errorf("other-thread = %q, want other-fox (clobbered by pre-RPC map!)", m.Threads["other-thread"])
	}
}

type barrierRoundTripper struct {
	base    http.RoundTripper
	path    string
	mu      sync.Mutex
	waiting int
	need    int
	release chan struct{}
}

func (b *barrierRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, b.path) {
		b.mu.Lock()
		b.waiting++
		if b.waiting == b.need {
			close(b.release)
		}
		ch := b.release
		b.mu.Unlock()
		select {
		case <-ch:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return b.base.RoundTrip(req)
}

// Two concurrent `hotbed session start` calls in the same unbound thread must
// settle on one winning binding: the loser must not overwrite sessions.json,
// must best-effort end its losing session, and must return the winning live
// session. If the concurrent winner has already ended/disappeared, the caller
// cleanly re-claims the binding (board 8295).
func TestSessionStartConcurrentSamePlaceDoesNotOverwriteBinding(t *testing.T) {
	t.Run("concurrent starts converge on one live binding without orphan", func(t *testing.T) {
		c := newCluster(t)
		c.addRepo(upstream(t))
		t.Setenv("T3CODE_THREAD_ID", "race-thread")
		t.Setenv("HOTBED_SESSION", "")

		base := c.cli.http.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		rt := &barrierRoundTripper{
			base:    base,
			path:    "/StartSession",
			need:    2,
			release: make(chan struct{}),
		}

		type result struct {
			code   int
			stdout string
			stderr string
			sess   *pb.Session
		}
		results := make([]result, 2)
		names := []string{"alpha-fox", "beta-owl"}

		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				var stdout, stderr bytes.Buffer
				d := c.cli
				d.http = &http.Client{Transport: rt, Timeout: 10 * time.Second}
				d.stdout = &stdout
				d.stderr = &stderr
				d.stdin = strings.NewReader("")
				code := run(t.Context(), []string{"--json", "session", "start", names[idx], "-p", "app"}, d)
				r := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
				if code == 0 {
					var env output
					if err := json.Unmarshal(stdout.Bytes(), &env); err == nil && env.OK {
						var s pb.Session
						if protojson.Unmarshal(env.Data, &s) == nil {
							r.sess = &s
						}
					}
				}
				results[idx] = r
			}(i)
		}
		wg.Wait()

		for i, r := range results {
			if r.code != 0 || r.sess == nil || r.sess.Id == "" {
				t.Fatalf("caller %d (%s): code=%d sess=%v stdout=%q stderr=%q", i, names[i], r.code, r.sess, r.stdout, r.stderr)
			}
		}
		if results[0].sess.Id != results[1].sess.Id {
			t.Fatalf("concurrent starts returned different sessions: %q vs %q", results[0].sess.Id, results[1].sess.Id)
		}
		winner := results[0].sess.Id
		loser := names[0]
		if winner == loser {
			loser = names[1]
		}

		m, err := loadSessionMap(sessionsFilePath(c.cli))
		if err != nil {
			t.Fatal(err)
		}
		if m.Threads["race-thread"] != winner {
			t.Fatalf("sessions.json race-thread = %q, want %q", m.Threads["race-thread"], winner)
		}
		if s := c.session(winner); s.EndedAt != nil {
			t.Fatalf("winning session %s was ended: %+v", winner, s)
		}
		if s := c.session(loser); s.EndedAt == nil {
			t.Fatalf("losing session %s was left live (orphaned): %+v", loser, s)
		}

		var ls pb.ListSessionsResponse
		c.ok(&ls, "session", "ls", "-p", "app")
		if len(ls.Sessions) != 1 || ls.Sessions[0].Id != winner {
			t.Fatalf("live sessions = %+v, want only %s", ls.Sessions, winner)
		}
	})

	t.Run("re-claims binding cleanly when concurrent winner is already ended", func(t *testing.T) {
		c := newCluster(t)
		c.addRepo(upstream(t))
		// Create and immediately end a session on the real controller.
		c.ok(nil, "session", "start", "gone-fox", "-p", "app")
		c.ok(nil, "session", "end", "-s", "gone-fox")

		t.Setenv("T3CODE_THREAD_ID", "reclaim-thread")
		t.Setenv("HOTBED_SESSION", "")

		base := c.cli.http.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		var once sync.Once
		rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/StartSession") {
				once.Do(func() {
					// Simulate another start binding gone-fox (already ended) while StartSession is in flight.
					if err := updateSessionMap(sessionsFilePath(c.cli), func(m sessionMap) {
						m.bind(bindPlace{Source: "thread", Key: "reclaim-thread"}, "gone-fox")
					}); err != nil {
						t.Errorf("inject ended binding: %v", err)
					}
				})
			}
			return base.RoundTrip(req)
		})

		var stdout, stderr bytes.Buffer
		d := c.cli
		d.http = &http.Client{Transport: rt, Timeout: 10 * time.Second}
		d.stdout = &stdout
		d.stderr = &stderr
		d.stdin = strings.NewReader("")
		code := run(t.Context(), []string{"--json", "session", "start", "fresh-owl", "-p", "app"}, d)
		if code != 0 {
			t.Fatalf("session start exited %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		var env output
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil || !env.OK {
			t.Fatalf("session start envelope = %q (%v)", stdout.String(), err)
		}
		var got pb.Session
		if err := protojson.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Id != "fresh-owl" {
			t.Fatalf("returned session = %q, want fresh-owl", got.Id)
		}
		m, err := loadSessionMap(sessionsFilePath(c.cli))
		if err != nil {
			t.Fatal(err)
		}
		if m.Threads["reclaim-thread"] != "fresh-owl" {
			t.Fatalf("reclaim-thread = %q, want fresh-owl", m.Threads["reclaim-thread"])
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// workspaceFor must call CreateWorkspace with d.stream (no short unary client
// timeout) rather than d.http so slow base preparation or heavy worker load
// does not fail with Client.Timeout exceeded while awaiting headers (board 7957).
func TestReachWorkspaceForUsesStreamClientNotShortHTTPTimeout(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "slow-ws")

	// Hold CreateWorkspace on the worker longer than d.http's Timeout.
	gate := make(chan struct{})
	c.nodes[0].createGate.Store(&gate)
	go func() {
		time.Sleep(150 * time.Millisecond)
		c.nodes[0].createGate.Store(nil)
		close(gate)
	}()

	d := c.cli
	// Short d.http timeout (50ms) would abort CreateWorkspace (150ms+) if
	// workspaceFor used d.http, while still allowing fast unary calls (like
	// RunCommand / GetCommand) to succeed.
	d.http = &http.Client{Transport: c.cli.http.Transport, Timeout: 50 * time.Millisecond}
	d.stream = &http.Client{Transport: c.cli.stream.Transport}

	addr, err := d.controller()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	d.stdout, d.stderr = &stdout, &stderr

	f := controllerClient{}
	f.callController(addr, d)
	ws, w, err := f.workspaceFor(t.Context(), d, "slow-ws", "app", &pb.Placement{}, "")
	if err != nil {
		t.Fatalf("workspaceFor with 50ms d.http timeout and 150ms CreateWorkspace failed: %v (stderr=%q)", err, stderr.String())
	}
	if ws == nil || ws.Id == "" {
		t.Fatalf("workspaceFor returned empty workspace: %+v", ws)
	}
	if _, err := waitReady(t.Context(), w, ws, &stderr); err != nil {
		t.Fatalf("waitReady after slow CreateWorkspace: %v", err)
	}
}

// CreateWorkspace runs on d.stream (no http.Client.Timeout), so workspaceFor
// must bound the call with createWorkspaceTimeout: a live-but-stuck controller
// must time out instead of hanging forever, and fall back to a cached workspace
// when one satisfies the placement.
//
// Only the stuck calls run under the short timeout: a setup that builds a
// real workspace (a base first) takes longer than it on a loaded host.
func TestReachWorkspaceForTimesOutOnStuckController(t *testing.T) {
	// stuckTimeout lowers createWorkspaceTimeout until the test ends.
	stuckTimeout := func(t *testing.T) {
		prev := createWorkspaceTimeout
		createWorkspaceTimeout = 150 * time.Millisecond
		t.Cleanup(func() { createWorkspaceTimeout = prev })
	}

	t.Run("stuck controller without cached workspace fails promptly", func(t *testing.T) {
		unblock := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-unblock:
			}
		}))
		defer func() {
			close(unblock)
			srv.Close()
		}()

		var stdout, stderr bytes.Buffer
		d := deps{
			http:       &http.Client{Timeout: 5 * time.Second},
			stream:     &http.Client{},
			stdin:      strings.NewReader(""),
			stdout:     &stdout,
			stderr:     &stderr,
			controller: at(strings.TrimPrefix(srv.URL, "http://")),
			cache:      t.TempDir(),
		}
		f := controllerClient{}
		f.callController(strings.TrimPrefix(srv.URL, "http://"), d)

		stuckTimeout(t)
		start := time.Now()
		_, _, err := f.workspaceFor(t.Context(), d, "stuck-elk", "app", &pb.Placement{}, "")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("workspaceFor against stuck controller succeeded, want CONTROLLER_UNREACHABLE")
		}
		if code := api.CodeOf(err); code != api.CodeControllerUnreachable {
			t.Fatalf("workspaceFor err = %v (code %s), want %s", err, code, api.CodeControllerUnreachable)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("workspaceFor took %v, expected prompt timeout around %v", elapsed, createWorkspaceTimeout)
		}
	})

	t.Run("stuck controller with cached workspace falls back offline", func(t *testing.T) {
		c := newCluster(t)
		c.addRepo(upstream(t))
		c.ok(nil, "session", "start", "stuck-fallback")
		cached := c.workspace("stuck-fallback", nil)

		base := c.cli.stream.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		stuck := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/CreateWorkspace") {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			return base.RoundTrip(req)
		})

		d := c.cli
		var stdout, stderr bytes.Buffer
		d.stdout, d.stderr = &stdout, &stderr
		d.stream = &http.Client{Transport: stuck}

		addr, err := d.controller()
		if err != nil {
			t.Fatal(err)
		}
		f := controllerClient{}
		f.callController(addr, d)

		stuckTimeout(t)
		start := time.Now()
		ws, _, err := f.workspaceFor(t.Context(), d, "stuck-fallback", "app", &pb.Placement{}, "")
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("workspaceFor fallback failed: %v (stderr=%q)", err, stderr.String())
		}
		if ws.Id != cached.Id {
			t.Fatalf("fallback workspace = %q, want cached %q", ws.Id, cached.Id)
		}
		if !strings.Contains(stderr.String(), "controller is out of reach") {
			t.Errorf("stderr = %q, want offline fallback notice", stderr.String())
		}
		if elapsed > 3*time.Second {
			t.Fatalf("workspaceFor fallback took %v, expected prompt timeout around %v", elapsed, createWorkspaceTimeout)
		}
	})
}

// reach fails hard when given an empty worker address rather than silently
// falling back to controller forwarding (board 7872).
func TestReachFailsHardOnEmptyAddr(t *testing.T) {
	f := controllerClient{}
	if _, err := f.reach(deps{}, ""); err == nil || api.CodeOf(err) != api.CodeInternal {
		t.Fatalf("reach(\"\") = %v (code %s), want %s", err, api.CodeOf(err), api.CodeInternal)
	}
}
