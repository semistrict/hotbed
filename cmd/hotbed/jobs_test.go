package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

var runningID = regexp.MustCompile(`hotbed: (\w+) running in `)

// A run that outlasts its --wait is left running, detached: its output
// past --max-output stops printing as it runs, and its last lines print
// as it is left; hotbed output reads all of it, kept whole.
func TestRunDetachesAfterItsWaitAndOutputReadsItAll(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "slow-owl")
	c.workspace("slow-owl", nil)

	script := `for i in $(seq 1 300); do echo line $i; done; echo error: boom; sleep 60`
	code, out, stderr := c.hotbed("run", "-s", "slow-owl", "-r", "app", "--wait", "3s", "--max-output", "200", "--", "sh", "-c", script)
	if code != exitDetached {
		t.Fatalf("run past its wait: exit %d, want %d; stdout %q stderr %q", code, exitDetached, out, stderr)
	}
	m := runningID.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("no command ID in stderr %q", stderr)
	}
	id := m[1]
	for _, want := range []string{
		"line 1\n",
		"[hotbed: output past 200 B stops printing here as it runs",
		"[hotbed: its last 40 of 301 lines:]\nline 262\n",
		"error: boom\n[hotbed: all 301 lines are kept: hotbed output " + id,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "line 200\n") {
		t.Errorf("stdout printed past --max-output and before the tail:\n%s", out)
	}
	if !strings.Contains(stderr, "hotbed: "+id+" still runs after 3s, detached") {
		t.Errorf("stderr %q, want it to say it was left running", stderr)
	}
	if !strings.Contains(stderr, "output exceeded --max-output 200 B") || !strings.Contains(stderr, "--max-output 0") {
		t.Errorf("stderr %q, want truncation warning naming --max-output 0", stderr)
	}
	if got, ok := c.engine().Command(id); !ok || got.Attached || got.State != pb.CommandState_COMMAND_STATE_RUNNING {
		t.Fatalf("after the run: %v, want it running, detached", got)
	}

	// Its output, read as asked, with per-line received timestamps.
	var q pb.OutputQuery
	c.ok(&q, "output", id, "--grep", "^error", "-C", "1")
	if len(q.Lines) != 2 || q.Lines[0].Text != "line 300" || q.Lines[1].Text != "error: boom" || !q.Lines[1].Match || q.GetMatches() != 1 || q.TotalLines != 301 {
		t.Fatalf("output --grep = %v", &q)
	}
	if q.Lines[0].ReceivedAt <= 0 || q.Lines[1].ReceivedAt <= 0 {
		t.Fatalf("output lines missing received_at timestamps: %v", q.Lines)
	}
	code, text, stderr := c.hotbed("output", id, "--tail", "2")
	wantPlain := "300     line 300\n301     error: boom\n[" + id + " running · 301 lines · next --after 301]\n"
	if code != exitOK || !strings.HasPrefix(text, wantPlain) {
		t.Fatalf("output --tail 2: exit %d, %q (stderr %q), want prefix %q", code, text, stderr, wantPlain)
	}
	code, timedText, stderr := c.hotbed("output", id, "--tail", "2", "--timestamps")
	wantTimedRe := regexp.MustCompile(`^300     \d\d:\d\d:\d\d\.\d\d\d  line 300\n301     \d\d:\d\d:\d\d\.\d\d\d  error: boom\n\[` + regexp.QuoteMeta(id) + ` running · 301 lines · next --after 301\]\n`)
	if code != exitOK || !wantTimedRe.MatchString(timedText) {
		t.Fatalf("output --tail 2 --timestamps: exit %d, %q (stderr %q), want match %s", code, timedText, stderr, wantTimedRe)
	}
	c.ok(&q, "output", id, "--until", "never", "--after", "301", "--timeout", "1")
	if q.Waited != "timeout" {
		t.Fatalf("output --until past the end: waited %q, want timeout", q.Waited)
	}

	// Detaching it again changes nothing; it has no terminal to type into.
	var cmd pb.Command
	c.ok(&cmd, "detach", "-s", "slow-owl", id)
	if cmd.Attached {
		t.Fatalf("detach = %v, want it detached", &cmd)
	}
	if code, e := c.fails("send", "-s", "slow-owl", id, "yes"); code != exitError || e.Code != api.CodeConflict || !strings.Contains(e.Message, "does not read its input from a terminal") {
		t.Fatalf("send to a command with no terminal: exit %d, %+v; want CONFLICT saying so", code, e)
	}
	c.ok(nil, "stop", "-s", "slow-owl", id)
}

// A run within its wait prints all it wrote and exits with its status.
func TestRunWithinItsWaitPrintsItAll(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "fast-owl")
	c.workspace("fast-owl", nil)
	code, out, stderr := c.hotbed("run", "-s", "fast-owl", "-r", "app", "--", "sh", "-c", "seq 1 3; exit 3")
	if code != 3 || out != "1\n2\n3\n" {
		t.Fatalf("quick run: exit %d, stdout %q (stderr %q), want 3 and all of it", code, out, stderr)
	}
}

// A detached run says what it needs: --mem, or an earlier run of the same
// command's peak. Its estimate stays with it.
func TestDetachedRunsNeedAnEstimate(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "wise-elk")
	c.workspace("wise-elk", nil)

	code, e := c.fails("run", "-s", "wise-elk", "-r", "app", "--detach", "--", "sleep", "60")
	if code != exitError || e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "a detached run needs --mem") {
		t.Fatalf("detached run with no estimate: exit %d, %+v; want INVALID_ARGS asking for --mem", code, e)
	}

	var first pb.Command
	c.ok(&first, "run", "-s", "wise-elk", "-r", "app", "--detach", "--mem", "2G", "--expect", "10m", "--", "sleep", "60")
	if est := first.Estimate; est.GetMemoryBytes() != 2<<30 || est.GetSeconds() != 600 || est.GetSource() != "given" {
		t.Fatalf("estimate = %v, want 2 GiB, 600s, given", est)
	}
	c.eventually("it runs", func() bool {
		got, ok := c.engine().Command(first.Id)
		return ok && got.State == pb.CommandState_COMMAND_STATE_RUNNING
	})
	c.nodes[0].units.SetAnon("hotbed-"+first.Id, 3<<30)
	c.eventually("its peak measured", func() bool {
		got, _ := c.engine().Command(first.Id)
		return got.GetMemoryBytes() == 3<<30
	})
	c.ok(nil, "stop", "-s", "wise-elk", first.Id)
	c.eventually("history to have it", func() bool {
		_, ok, err := c.store.Finished(t.Context(), first.Id)
		return err == nil && ok
	})

	var second pb.Command
	c.ok(&second, "run", "-s", "wise-elk", "-r", "app", "--detach", "--", "sleep", "60")
	if est := second.Estimate; est.GetMemoryBytes() != 3<<30 || est.GetSource() != "history" {
		t.Fatalf("estimate from history = %v, want the earlier run's 3 GiB peak", est)
	}
	c.ok(nil, "stop", "-s", "wise-elk", second.Id)
}

// A detached run with --ready returns once its output says it is ready,
// and fails, saying so, if it ends first.
func TestDetachedRunWaitsUntilReady(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "keen-bat")
	c.workspace("keen-bat", nil)
	code, out, stderr := c.hotbed("run", "-s", "keen-bat", "-r", "app", "--detach", "--mem", "1G", "--ready", "^listening", "--",
		"sh", "-c", "echo booting; sleep 1; echo listening on 3000; sleep 60")
	m := regexp.MustCompile(`(\w+) is ready: its output says so`).FindStringSubmatch(stderr)
	if code != exitOK || m == nil {
		t.Fatalf("run --ready: exit %d, stdout %q, stderr %q; want it ready", code, out, stderr)
	}
	c.ok(nil, "stop", "-s", "keen-bat", m[1])
	code, e := c.fails("run", "-s", "keen-bat", "-r", "app", "--detach", "--mem", "1G", "--ready", "^listening", "--", "sh", "-c", "echo no port; exit 1")
	if code != exitError || e.Code != api.CodeNotReady || !strings.Contains(e.Message, "ended before writing a line matching") {
		t.Fatalf("run --ready of one that exits: exit %d, %+v; want NOT_READY saying it ended", code, e)
	}
}

// A port proxy that accepts and closes at once (its VM's server not up) is
// not serving; a server that holds the connection, or answers, is.
func TestServesSeesThroughAPortProxy(t *testing.T) {
	t.Parallel()
	listen := func(handle func(net.Conn)) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go handle(c)
			}
		}()
		return ln.Addr().String()
	}
	closes := listen(func(c net.Conn) { c.Close() })
	holds := listen(func(c net.Conn) { time.Sleep(3 * time.Second); c.Close() })
	answers := listen(func(c net.Conn) { _, _ = c.Write([]byte("hello")); c.Close() })
	serving := func(addr string) bool {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		return serves(conn)
	}
	if serving(closes) {
		t.Error("a proxy closing at once counts as serving")
	}
	if !serving(holds) || !serving(answers) {
		t.Error("a server holding or answering a connection does not count as serving")
	}
}

// A wedged QueryOutput --wait HTTP stream (e.g. silent edge/proxy drop after
// the command already finished) must not hang past its slice deadline: the
// client recycles the transport, re-issues the bounded slice, and returns the
// completed output promptly.
func TestOutputWaitRecoversFromWedgedHTTPStream(t *testing.T) {
	origSlice, origGrace, origRetry := queryOutputSlice, queryOutputSliceGrace, queryOutputRetryDelay
	queryOutputSlice = time.Second
	queryOutputSliceGrace = 250 * time.Millisecond
	queryOutputRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() {
		queryOutputSlice, queryOutputSliceGrace, queryOutputRetryDelay = origSlice, origGrace, origRetry
	})

	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "keen-owl")
	c.workspace("keen-owl", nil)

	var cmd pb.Command
	c.ok(&cmd, "run", "-s", "keen-owl", "-r", "app", "--detach", "--mem", "256M", "--", "sh", "-c", "echo line1; echo line2")
	c.eventually("command finishes", func() bool {
		got, ok := c.engine().Command(cmd.Id)
		return ok && got.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})

	// Blackhole the first QueryOutput HTTP request (never send headers until
	// the client cancels it) to simulate a wedged/silently dropped HTTP stream
	// after the command already finished.
	var calls atomic.Int32
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	origHandler := *c.handler.Load()
	var wrapped http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hotbed.v1.WorkspaceService/QueryOutput" && calls.Add(1) == 1 {
			select {
			case <-r.Context().Done():
			case <-unblock:
			}
			return
		}
		origHandler.ServeHTTP(w, r)
	})
	c.handler.Store(&wrapped)

	start := time.Now()
	var q pb.OutputQuery
	c.ok(&q, "output", cmd.Id, "--wait", "--timeout", "10", "--tail", "2")
	elapsed := time.Since(start)
	if calls.Load() < 2 {
		t.Fatalf("expected retry after wedged first slice, got %d calls", calls.Load())
	}
	if q.State != pb.CommandState_COMMAND_STATE_SUCCEEDED || q.Waited != "ended" || q.TotalLines != 2 {
		t.Fatalf("output --wait = %+v, want SUCCEEDED, waited=ended, 2 lines", &q)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("output --wait took %v after wedged slice, want fast recovery", elapsed)
	}
}

// A server-side Connect-Timeout-Ms deadline or transient retryable
// WORKER_UNREACHABLE on QueryOutput (both --wait and plain reads like
// --after N) must be retried by queryOutputBounded rather than aborting the
// command immediately (issue #22).
func TestOutputBoundedRetriesServerDeadlineAndPlainWorkerUnreachable(t *testing.T) {
	origSlice, origGrace, origRetry := queryOutputSlice, queryOutputSliceGrace, queryOutputRetryDelay
	queryOutputSlice = time.Second
	queryOutputSliceGrace = 2 * time.Second
	queryOutputRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() {
		queryOutputSlice, queryOutputSliceGrace, queryOutputRetryDelay = origSlice, origGrace, origRetry
	})

	c := newCluster(t)
	c.cli.rideOut = 5 * time.Second
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "swift-elk")
	c.workspace("swift-elk", nil)

	var cmd pb.Command
	c.ok(&cmd, "run", "-s", "swift-elk", "-r", "app", "--detach", "--mem", "256M", "--", "sh", "-c", "echo first; echo second")
	c.eventually("command finishes", func() bool {
		got, ok := c.engine().Command(cmd.Id)
		return ok && got.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})

	// 1. Waiting QueryOutput: first request gets Connect deadline_exceeded
	// directly from the server before the client's local sliceCtx expires.
	var waitCalls atomic.Int32
	origHandler := *c.handler.Load()
	var deadlineHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hotbed.v1.WorkspaceService/QueryOutput" && waitCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestTimeout)
			_, _ = w.Write([]byte(`{"code":"deadline_exceeded","message":"slice deadline exceeded"}`))
			return
		}
		origHandler.ServeHTTP(w, r)
	})
	c.handler.Store(&deadlineHandler)

	var q pb.OutputQuery
	c.ok(&q, "output", cmd.Id, "--wait", "--timeout", "10")
	if waitCalls.Load() < 2 {
		t.Fatalf("expected retry after server deadline_exceeded, got %d calls", waitCalls.Load())
	}
	if q.State != pb.CommandState_COMMAND_STATE_SUCCEEDED || q.TotalLines != 2 {
		t.Fatalf("output --wait = %+v, want SUCCEEDED with 2 lines", &q)
	}

	// 2. Plain read (--after 1): first request gets retryable WORKER_UNREACHABLE
	// timed out; client retries and returns the trailing line.
	var plainCalls atomic.Int32
	detailBytes, err := proto.Marshal(&pb.ErrorInfo{
		Code:      string(api.CodeWorkerUnreachable),
		Retryable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	errPayload := fmt.Sprintf(
		`{"code":"unavailable","message":"mac: /hotbed.v1.WorkspaceService/QueryOutput timed out","details":[{"type":"hotbed.v1.ErrorInfo","value":%q}]}`,
		base64.RawStdEncoding.EncodeToString(detailBytes),
	)
	var unreachableHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hotbed.v1.WorkspaceService/QueryOutput" && plainCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(errPayload))
			return
		}
		origHandler.ServeHTTP(w, r)
	})
	c.handler.Store(&unreachableHandler)

	var q2 pb.OutputQuery
	c.ok(&q2, "output", cmd.Id, "--after", "1")
	if plainCalls.Load() < 2 {
		t.Fatalf("expected plain QueryOutput retry after WORKER_UNREACHABLE, got %d calls", plainCalls.Load())
	}
	if len(q2.Lines) != 1 || q2.Lines[0].Text != "second" {
		t.Fatalf("output --after 1 = %+v, want [second]", q2.Lines)
	}
}

// A run with --max-output 0 streams output well past the 32 KiB default
// verbatim without inserting truncation banners, and `hotbed logs` resumes
// from its byte offset if the FollowLog stream fails with a retryable error.
func TestRunMaxOutputZeroAndLogsResumeOnRetryableStreamBreak(t *testing.T) {
	c := newCluster(t)
	c.cli.rideOut = 5 * time.Second
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "wide-fox")
	c.workspace("wide-fox", nil)

	// 64 KiB of deterministic lines (> defaultMaxOutput = 32 KiB).
	code, out, stderr := c.hotbed("run", "-s", "wide-fox", "-r", "app", "--max-output", "0", "--",
		"sh", "-c", `i=1; while [ $i -le 1000 ]; do printf 'row-%04d-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n' "$i"; i=$((i+1)); done`)
	if code != exitOK {
		t.Fatalf("run --max-output 0: exit %d, stderr %q", code, stderr)
	}
	if len(out) < 60*1024 {
		t.Fatalf("stdout len = %d, want > 60 KiB", len(out))
	}
	if strings.Contains(out, "[hotbed: output past") {
		t.Fatalf("stdout with --max-output 0 must not contain truncation banner")
	}
	if !strings.HasPrefix(out, "row-0001-") || !strings.HasSuffix(out, "row-1000-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n") {
		t.Fatalf("stdout boundaries corrupted: prefix=%q suffix=%q", out[:min(32, len(out))], out[max(0, len(out)-70):])
	}

	m := runningID.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("no command ID in stderr %q", stderr)
	}
	id := m[1]

	// Force the first FollowLog call for `hotbed logs` to fail with a retryable
	// WORKER_UNREACHABLE error; `hotbed logs` must retry and return the exact same bytes.
	var logCalls atomic.Int32
	detailBytes, err := proto.Marshal(&pb.ErrorInfo{
		Code:      string(api.CodeWorkerUnreachable),
		Retryable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	errPayload := fmt.Sprintf(
		`{"code":"unavailable","message":"worker stream exceeded controller buffer limit","details":[{"type":"hotbed.v1.ErrorInfo","value":%q}]}`,
		base64.RawStdEncoding.EncodeToString(detailBytes),
	)
	origHandler := *c.handler.Load()
	var flaky http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hotbed.v1.WorkspaceService/FollowLog" && logCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(errPayload))
			return
		}
		origHandler.ServeHTTP(w, r)
	})
	c.handler.Store(&flaky)

	code, logsOut, logsErr := c.hotbed("logs", id)
	if code != exitOK {
		t.Fatalf("hotbed logs %s: exit %d, stderr %q", id, code, logsErr)
	}
	if logCalls.Load() < 2 {
		t.Fatalf("expected FollowLog retry in hotbed logs, got %d calls", logCalls.Load())
	}
	if logsOut != out {
		t.Fatalf("hotbed logs len=%d != run stdout len=%d", len(logsOut), len(out))
	}
}
