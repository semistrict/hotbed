package main

import (
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
