package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

// A command's log past its limit keeps its head and tail as it runs: logs
// prints them with a line in place of what was dropped, and ps says how
// much was dropped.
func TestLogsOfACommandPastItsLogLimit(t *testing.T) {
	limit := cmdlog.Limit{Head: 1 << 20, Tail: 2 << 20}
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.LogLimit = limit })
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "loud-yak")
	c.workspace("loud-yak", nil)
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "loud-yak", "-r", "app", "--detach", "--", "sh", "-c", "awk 'BEGIN { for (i = 1; i <= 2000000; i++) print i }'; sleep 300")
	var full strings.Builder
	for i := 1; i <= 2_000_000; i++ {
		fmt.Fprintln(&full, i)
	}
	all := full.String() // 14.8 MiB
	wantTo := (int64(len(all)) - limit.Tail) / (1 << 20) * (1 << 20)
	wantDropped := wantTo - limit.Head

	var dropped int64
	c.eventually("awk to finish and the log to be compacted", func() bool {
		ps := &pb.ListCommandsResponse{}
		c.ok(ps, "ps", "-s", "loud-yak")
		for _, p := range ps.Commands {
			if p.Id == cmd.Id {
				dropped = p.DroppedOutputBytes
			}
		}
		if dropped < wantDropped {
			return false
		}
		code, tail, _ := c.hotbed("logs", "-s", "loud-yak", "-n", "1", cmd.Id)
		return code == 0 && tail == "2000000\n"
	})
	d := cmdlog.Dropped{From: limit.Head, To: limit.Head + dropped}
	if d.To != wantTo {
		t.Fatalf("dropped up to %d, want %d: all but the last 2 MiB, to a MiB boundary", d.To, wantTo)
	}
	want := all[:d.From] + "\n" + cmdlog.Marker(d) + "\n" + all[d.To:]
	if all[d.From-1] == '\n' {
		want = all[:d.From] + cmdlog.Marker(d) + "\n" + all[d.To:]
	}
	code, out, errOut := c.hotbed("logs", "-s", "loud-yak", cmd.Id)
	if code != 0 || out != want {
		t.Errorf("logs: exit %d, %d bytes (stderr %q), want %d: the first MiB, the marker, the rest from %d", code, len(out), errOut, len(want), d.To)
	}
	// logs -n reads the end alone (the tail a log keeps is more than one
	// response holds, so it never reaches back to the marker).
	if code, out, _ := c.hotbed("logs", "-s", "loud-yak", "-n", "2", cmd.Id); code != 0 || out != "1999999\n2000000\n" {
		t.Errorf("logs -n 2: exit %d, %q", code, out)
	}
	c.ok(nil, "stop", "-s", "loud-yak", cmd.Id)
}

// A command's ID names it cluster-wide: its output reads by the ID alone,
// with no session bound or another one, while waiting on it (logs -f,
// which resumes it and keeps its session in use) and stopping it take its
// own session.
func TestACommandReadsByItsIDFromAnySession(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "own-ox")
	c.ok(nil, "session", "start", "other-elk")
	c.workspace("own-ox", nil)
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "own-ox", "-r", "app", "--detach", "--", "sh", "-c", "echo hello; sleep 300")
	c.eventually("its output", func() bool {
		_, out, _ := c.hotbed("logs", "-s", "own-ox", cmd.Id)
		return out == "hello\n"
	})
	for _, bound := range []string{"", "other-elk"} {
		t.Setenv("HOTBED_SESSION", bound)
		if code, out, errOut := c.hotbed("logs", cmd.Id); code != 0 || out != "hello\n" {
			t.Errorf("logs with session %q bound: exit %d, %q (stderr %q), want hello", bound, code, out, errOut)
		}
		if code, out, errOut := c.hotbed("logs", "-n", "1", cmd.Id); code != 0 || out != "hello\n" {
			t.Errorf("logs -n 1 with session %q bound: exit %d, %q (stderr %q), want hello", bound, code, out, errOut)
		}
		_, e := c.fails("logs", "-f", cmd.Id)
		if want := (api.Error{Code: api.CodeInvalidArgs,
			Message:    "waiting on " + cmd.Id + " would resume it and keep session own-ox in use",
			Suggestion: cmd.Id + " is session own-ox's: pass -s own-ox to act on it; or read it without -f"}); *e != want {
			t.Errorf("logs -f with session %q bound: %+v, want %+v", bound, *e, want)
		}
	}
	_, e := c.fails("stop", "-s", "other-elk", cmd.Id)
	if want := (api.Error{Code: api.CodeNotFound,
		Message:    "session other-elk has no command " + cmd.Id,
		Suggestion: cmd.Id + " is session own-ox's: pass -s own-ox to act on it"}); *e != want {
		t.Errorf("stop from other-elk: %+v, want %+v", *e, want)
	}
	t.Setenv("HOTBED_SESSION", "")
	c.ok(nil, "stop", "-s", "own-ox", cmd.Id)
}

// A failed base build's log remains reachable via `hotbed logs` after the
// worker forgets the ended freshen command (>2 min), after a controller
// restart, and even after a newer generation supersedes and collects the failed
// base (board 7951 AC #2).
func TestFreshenLogReadableAfterWorkerForgetsItAndControllerRestarts(t *testing.T) {
	workerClock := &clock{}
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Now = workerClock.Now })
	c.addProject(upstream(t))
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", "echo broken base build >&2; exit 3")
	c.ok(nil, "session", "start", "keen-ox")

	_, e := c.fails("run", "-s", "keen-ox", "-r", "app", "--", "true")
	m := regexp.MustCompile(`its log is command (f-\S+)\)`).FindStringSubmatch(e.Message)
	if m == nil {
		t.Fatalf("run's error names no freshen log: %+v", e)
	}
	freshenID := m[1]

	// Wait for the controller to apply the freshen events, then advance the
	// worker clock past keepEnded (2 min) so the worker forgets the freshen.
	c.eventually("controller to record the failed base", func() bool {
		_, _, ok, err := c.store.BaseByCommand(t.Context(), freshenID)
		return err == nil && ok
	})
	workerClock.advance(3 * time.Minute)
	c.eventually("worker to forget the freshen command", func() bool {
		_, ok := c.engine().Command(freshenID)
		return !ok
	})

	// Restart the controller on the same database to ensure nothing relies on
	// in-memory controller state.
	c.restart()

	for _, args := range [][]string{
		{"logs", freshenID},
		{"logs", "-s", "keen-ox", freshenID},
		{"logs", "-n", "10", freshenID},
	} {
		code, out, stderr := c.hotbed(args...)
		if code != 0 || !strings.Contains(out, "broken base build") {
			t.Errorf("hotbed %v: exit %d, stdout %q, stderr %q; want broken base build", args, code, out, stderr)
		}
	}

	// Now fix the step and run again so g2 builds warm and g1 is collected from
	// node_state bases; g1's freshen log must STILL be readable via commands.
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", "echo fixed base build")
	code, _, stderr := c.hotbed("run", "-s", "keen-ox", "-r", "app", "--", "true")
	if code != 0 {
		t.Fatalf("run after fixing step: exit %d, stderr %q", code, stderr)
	}
	c.eventually("superseded failed base g1 to be collected", func() bool {
		_, _, ok, err := c.store.BaseByCommand(t.Context(), freshenID)
		return err == nil && !ok
	})

	code, out, stderr := c.hotbed("logs", freshenID)
	if code != 0 || !strings.Contains(out, "broken base build") {
		t.Errorf("hotbed logs %s after base collected: exit %d, stdout %q, stderr %q", freshenID, code, out, stderr)
	}
}

// A command's on-disk log remains reachable via `hotbed logs` across both a
// controller restart and a worker restart that lost its worker database before
// emitting FinishedCommand (board 7917 AC #2).
func TestLogsReadableAfterControllerAndWorkerRestartWithLostDB(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "crab-log")
	c.workspace("crab-log", nil)

	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "crab-log", "-r", "app", "--detach", "--", "sh", "-c", "echo survivor-log-line; sleep 300")
	c.eventually("output on disk and command recorded on controller", func() bool {
		code, out, _ := c.hotbed("logs", "-s", "crab-log", cmd.Id)
		if code != 0 || out != "survivor-log-line\n" {
			return false
		}
		_, _, err := c.store.Command(t.Context(), cmd.Id)
		return err == nil
	})

	// Wipe worker.db (keeping logs/<id>.log on disk) and restart the controller.
	c.nodes[0].wipe()
	c.restart()
	c.online()

	// Confirm neither worker memory nor controller history holds the command.
	if _, ok := c.engine().Command(cmd.Id); ok {
		t.Fatalf("expected worker to have lost %s after DB wipe", cmd.Id)
	}
	if _, ok, err := c.store.Finished(t.Context(), cmd.Id); err != nil || ok {
		t.Fatalf("expected %s absent from history (ok=%v err=%v)", cmd.Id, ok, err)
	}

	for _, args := range [][]string{
		{"logs", cmd.Id},
		{"logs", "-s", "crab-log", cmd.Id},
		{"logs", "-n", "5", cmd.Id},
	} {
		code, out, stderr := c.hotbed(args...)
		if code != 0 || out != "survivor-log-line\n" {
			t.Errorf("hotbed %v after restart/wipe: exit %d, stdout %q, stderr %q; want survivor-log-line", args, code, out, stderr)
		}
	}
}
