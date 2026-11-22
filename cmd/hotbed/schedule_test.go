package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// tomorrowAt is tomorrow at hh:mm UTC. The tests' schedules are in UTC
// (CRON_TZ=UTC), so no daylight saving time moves their slots.
func tomorrowAt(hh, mm int) time.Time {
	y, m, d := time.Now().UTC().AddDate(0, 0, 1).Date()
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

const nightly = "CRON_TZ=UTC 0 2 * * *"

// waitFor is a shell command that waits until flag exists, then runs then.
func waitFor(flag, then string) string {
	return fmt.Sprintf(`while [ ! -e %q ]; do sleep 0.05; done; %s`, flag, then)
}

// schedule returns the scheduled command name as hotbed schedule show has it.
func (c *cluster) schedule(name string) *pb.ScheduledCommand {
	c.t.Helper()
	sc := &pb.ScheduledCommand{}
	c.ok(sc, "schedule", "show", name)
	return sc
}

// at moves the clock to t and has the worker report at once, so the
// controller, its clock jumped, still counts the worker online.
func (c *cluster) at(t time.Time) {
	c.t.Helper()
	c.clock.set(t)
	c.report()
}

// later moves the clock on by d, as at does.
func (c *cluster) later(d time.Duration) {
	c.t.Helper()
	c.at(c.clock.Now().Add(d))
}

// report sends the worker's report now, as its loop does every few seconds.
func (c *cluster) report() {
	c.t.Helper()
	rep, err := c.engine.Report(c.id)
	if err != nil {
		c.t.Fatal(err)
	}
	res, err := c.asWorker.Report(c.t.Context(), rep)
	if err != nil {
		c.t.Fatal(err)
	}
	c.engine.Acknowledged(res.Ack)
}

// tick has the controller start what is due, as its loop does.
func (c *cluster) tick() {
	c.t.Helper()
	if err := c.server.TickSchedules(c.t.Context()); err != nil {
		c.t.Fatal(err)
	}
}

// session is the session id as the cockpit's snapshot has it.
func (c *cluster) session(id string) *pb.Session {
	c.t.Helper()
	snap, err := c.asWorker.GetSnapshot(c.t.Context(), &pb.GetSnapshotRequest{})
	if err != nil {
		c.t.Fatal(err)
	}
	for _, s := range snap.Sessions {
		if s.Id == id {
			return s
		}
	}
	c.t.Fatalf("no session %s in the snapshot", id)
	return nil
}

// A scheduled command runs at its slot in a session of its own, in a fresh
// workspace, with its env; a run that succeeds ends its session.
func TestScheduledCommandRunsInItsOwnSession(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.at(tomorrowAt(1, 59))

	sc := &pb.ScheduledCommand{}
	c.ok(sc, "schedule", "add", "nightly", "app", "--cron", nightly, "--env", "WHO=nightly", "--",
		"sh", "-c", `echo "$WHO at $(cat target/src/keep.txt)"`)
	slot := tomorrowAt(2, 0)
	if sc.Name != "nightly" || sc.Cron != nightly || sc.Repo != "app" || sc.GetNextAt() != slot.UnixMilli() ||
		strings.Join(sc.Command.Args, " ") != `sh -c echo "$WHO at $(cat target/src/keep.txt)"` || strings.Join(sc.Command.Env, " ") != "WHO=nightly" {
		t.Fatalf("schedule add = %v", sc)
	}

	// Nothing is due before the slot.
	c.tick()
	if runs := c.schedule("nightly").Runs; len(runs) != 0 {
		t.Fatalf("runs before the slot: %v", runs)
	}

	c.at(slot.Add(10 * time.Second))
	c.tick()
	run := c.schedule("nightly").Runs[0]
	wantSession := "nightly-" + slot.Format("20060102-1504")
	if run.SessionId != wantSession || run.GetSlotAt() != slot.UnixMilli() || run.CommandId == "" || run.WorkspaceId == "" {
		t.Fatalf("run = %v, want one in session %s", run, wantSession)
	}
	if s := c.session(wantSession); s.Schedule != "nightly" || s.Agent != "schedule:nightly" {
		t.Errorf("the run's session = %v", s)
	}

	// The worker's report of its end settles the run, without a tick.
	c.eventually("the run to succeed", func() bool {
		return c.schedule("nightly").Runs[0].Outcome == pb.RunOutcome_RUN_OUTCOME_SUCCEEDED
	})
	sc = c.schedule("nightly")
	run = sc.Runs[0]
	if run.GetExitCode() != 0 || run.EndedAt == nil || len(sc.Runs) != 1 {
		t.Errorf("settled run = %v", run)
	}
	if sc.GetNextAt() != slot.AddDate(0, 0, 1).UnixMilli() {
		t.Errorf("next run at %v, want a day after the slot", time.UnixMilli(sc.GetNextAt()))
	}
	if s := c.session(wantSession); s.EndedAt == nil || s.EndReason != "its scheduled run succeeded" {
		t.Errorf("the succeeded run's session = %v", s)
	}
	_, out, _ := c.hotbed("logs", wantSession, run.CommandId)
	if out != "nightly at keep\n" {
		t.Errorf("the run's output = %q", out)
	}

	ls := &pb.ListSchedulesResponse{}
	c.ok(ls, "schedule", "ls")
	if len(ls.Schedules) != 1 || ls.Schedules[0].Name != "nightly" || len(ls.Schedules[0].Runs) != 1 {
		t.Errorf("schedule ls = %v", ls)
	}
}

// A run is never ended by the idle reaper while its command runs. One that
// fails keeps its session for the idle hour, for an agent to look inside.
func TestFailedScheduledRunLingers(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	flag := filepath.Join(t.TempDir(), "go")
	c.ok(nil, "schedule", "add", "flaky", "app", "--cron", nightly, "--", "sh", "-c", waitFor(flag, "exit 3"))
	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "flaky")
	if run.Outcome != pb.RunOutcome_RUN_OUTCOME_RUNNING || run.SlotAt != nil || run.SessionId == "" {
		t.Fatalf("schedule run = %v", run)
	}
	if _, e := c.fails("schedule", "run", "flaky"); e.Code != api.CodeConflict {
		t.Errorf("a second run while one is in progress: %+v", e)
	}

	c.later(3 * time.Hour)
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := c.session(run.SessionId); s.EndedAt != nil || !s.ScheduleRunning {
		t.Fatalf("the reaper ended a run in progress, or it does not say it runs: %v", s)
	}

	write(t, flag, "")
	c.eventually("the run to fail", func() bool {
		return c.schedule("flaky").Runs[0].Outcome == pb.RunOutcome_RUN_OUTCOME_FAILED
	})
	if got := c.schedule("flaky").Runs[0]; got.GetExitCode() != 3 || got.Reason != "exited 3" {
		t.Errorf("failed run = %v", got)
	}
	c.later(59 * time.Minute)
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := c.session(run.SessionId); s.EndedAt != nil || s.ScheduleRunning {
		t.Fatalf("a failed run's session ended before its idle hour, or says it runs: %v", s)
	}
	c.later(2 * time.Minute)
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := c.session(run.SessionId); s.EndedAt == nil || !strings.Contains(s.EndReason, "abandoned") {
		t.Errorf("a failed run's session after its idle hour: %v", s)
	}
}

// Runs never overlap: a slot that comes while the previous run is still in
// progress is skipped. A slot that passed while the controller was down is
// missed, not run late.
func TestScheduledRunsAreSkippedOrMissed(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	flag := filepath.Join(t.TempDir(), "go")
	first := tomorrowAt(2, 0)
	c.at(first.Add(-time.Minute))
	c.ok(nil, "schedule", "add", "hourly", "app", "--cron", "CRON_TZ=UTC 0 * * * *", "--", "sh", "-c", waitFor(flag, "true"))

	c.at(first.Add(5 * time.Second))
	c.tick()
	c.at(first.Add(time.Hour + 5*time.Second))
	c.tick()
	runs := c.schedule("hourly").Runs
	if len(runs) != 2 || runs[0].Outcome != pb.RunOutcome_RUN_OUTCOME_SKIPPED || runs[0].GetSlotAt() != first.Add(time.Hour).UnixMilli() ||
		runs[0].Reason != "the run due at "+first.Format("15:04")+" ("+runs[1].SessionId+") was still in progress" ||
		runs[1].Outcome != pb.RunOutcome_RUN_OUTCOME_RUNNING {
		t.Fatalf("runs after a slot during a run: %v", runs)
	}

	write(t, flag, "")
	c.eventually("the run to succeed", func() bool {
		return c.schedule("hourly").Runs[1].Outcome == pb.RunOutcome_RUN_OUTCOME_SUCCEEDED
	})

	// The controller is down from 03:01 to 05:30: the 04:00 and 05:00 slots
	// pass unseen.
	c.at(first.Add(3*time.Hour + 30*time.Minute))
	c.restart()
	c.tick()
	sc := c.schedule("hourly")
	if len(sc.Runs) != 3 || sc.Runs[0].Outcome != pb.RunOutcome_RUN_OUTCOME_MISSED || sc.Runs[0].GetSlotAt() != first.Add(3*time.Hour).UnixMilli() ||
		sc.Runs[0].Reason != "the controller was down: missed 2 slots since "+first.Add(2*time.Hour).Format("15:04") || sc.Runs[0].SessionId != "" {
		t.Fatalf("runs after the controller was down: %v", sc.Runs)
	}
	if sc.GetNextAt() != first.Add(4*time.Hour).UnixMilli() {
		t.Errorf("next run at %v, want 06:00", time.UnixMilli(sc.GetNextAt()))
	}

	// A slot seen a little late, as across a restart, still runs.
	c.at(first.Add(4*time.Hour + 3*time.Minute))
	c.tick()
	c.eventually("the late slot's run to succeed", func() bool {
		return c.schedule("hourly").Runs[0].Outcome == pb.RunOutcome_RUN_OUTCOME_SUCCEEDED
	})
	if run := c.schedule("hourly").Runs[0]; run.GetSlotAt() != first.Add(4*time.Hour).UnixMilli() {
		t.Errorf("the late slot's run = %v", run)
	}
}

// A controller restarted while a run is in progress settles it from what is
// in its database.
func TestScheduledRunSurvivesRestart(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	flag := filepath.Join(t.TempDir(), "go")
	c.ok(nil, "schedule", "add", "soak", "app", "--cron", nightly, "--", "sh", "-c", waitFor(flag, "true"))
	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "soak")

	c.restart()
	if sc := c.schedule("soak"); sc.Cron != nightly || len(sc.Runs) != 1 || sc.Runs[0].Outcome != pb.RunOutcome_RUN_OUTCOME_RUNNING {
		t.Fatalf("after a restart: %v", sc)
	}
	write(t, flag, "")
	c.eventually("the run to succeed", func() bool {
		return c.schedule("soak").Runs[0].Outcome == pb.RunOutcome_RUN_OUTCOME_SUCCEEDED
	})
}

// A run that cannot start says why, and leaves no session behind.
func TestScheduledRunThatCannotStart(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "schedule", "add", "pinned", "app", "--cron", nightly, "--node", "nowhere", "--", "true")
	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "pinned")
	if run.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || run.Reason != "nowhere is not online or does not host app" || run.EndedAt == nil {
		t.Fatalf("run on a node that is not there = %v", run)
	}
	if s := c.session(run.SessionId); s.EndedAt == nil || s.EndReason != "its scheduled run could not start" {
		t.Errorf("the run's session = %v", s)
	}
}

// A paused schedule starts nothing until resumed, and then not the slots it
// slept through.
func TestPausedScheduleRunsNothing(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.at(tomorrowAt(1, 0))
	c.ok(nil, "schedule", "add", "nightly", "app", "--cron", nightly, "--", "true")
	paused := &pb.ScheduledCommand{}
	c.ok(paused, "schedule", "pause", "nightly")
	if !paused.Paused || paused.NextAt != nil {
		t.Fatalf("paused = %v", paused)
	}
	c.at(tomorrowAt(2, 1))
	c.tick()
	resumed := &pb.ScheduledCommand{}
	c.ok(resumed, "schedule", "resume", "nightly")
	if resumed.Paused || len(resumed.Runs) != 0 || resumed.GetNextAt() != tomorrowAt(2, 0).AddDate(0, 0, 1).UnixMilli() {
		t.Errorf("resumed = %v", resumed)
	}
	c.ok(nil, "schedule", "rm", "nightly")
	if _, e := c.fails("schedule", "show", "nightly"); e.Code != api.CodeNotFound {
		t.Errorf("show after rm: %+v", e)
	}
}

func TestScheduleAddChecksItsArguments(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	for _, tc := range []struct {
		args []string
		code api.Code
	}{
		{[]string{"nightly", "app", "--cron", "0 2 * *", "--", "true"}, api.CodeInvalidArgs},
		{[]string{"nightly", "app", "--cron", "0 0 30 2 *", "--", "true"}, api.CodeInvalidArgs}, // February 30 never comes
		{[]string{"Nightly", "app", "--cron", nightly, "--", "true"}, api.CodeInvalidArgs},
		{[]string{"nightly", "nope", "--cron", nightly, "--", "true"}, api.CodeNotFound},
		{[]string{"nightly", "app", "--cron", nightly}, api.CodeInvalidArgs},
	} {
		if _, e := c.fails(append([]string{"schedule", "add"}, tc.args...)...); e.Code != tc.code {
			t.Errorf("schedule add %v: %+v, want %s", tc.args, e, tc.code)
		}
	}
}

// A run whose session ends under it (by hand, or abandoned with its worker
// gone, so its command's end may never be reported) ends with it, and the
// next run may start.
func TestScheduledRunEndsWithItsSession(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	flag := filepath.Join(t.TempDir(), "go")
	c.ok(nil, "schedule", "add", "soak", "app", "--cron", nightly, "--", "sh", "-c", waitFor(flag, "true"))
	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "soak")
	c.admitted(run.SessionId) // its base built, so nothing is left building when the test ends
	c.ok(nil, "session", "end", run.SessionId)
	got := c.schedule("soak").Runs[0]
	if got.Outcome != pb.RunOutcome_RUN_OUTCOME_CANCELLED || got.Reason != "its session ended: ended by its agent" || got.EndedAt == nil {
		t.Fatalf("run after its session ended = %v", got)
	}
	again := &pb.ScheduledRun{}
	c.ok(again, "schedule", "run", "soak")
	if again.Outcome != pb.RunOutcome_RUN_OUTCOME_RUNNING || again.SessionId == run.SessionId {
		t.Errorf("the next run = %v", again)
	}
	write(t, flag, "")
	c.eventually("the next run to succeed", func() bool {
		return c.schedule("soak").Runs[0].Outcome == pb.RunOutcome_RUN_OUTCOME_SUCCEEDED
	})
}

// admitted waits for a session's command to be admitted: its workspace is
// ready, its base built.
func (c *cluster) admitted(session string) {
	c.t.Helper()
	var events []*pb.SessionEvent
	c.eventually("a command of "+session+" to be admitted", func() bool {
		res := &pb.WatchSessionResponse{}
		c.ok(res, "wait", session, "--timeout", "1")
		events = append(events, res.Events...)
		return countWhat(events, "admitted") > 0
	})
}

// A run a controller recorded and stopped before its command started is
// recorded as not started by the next, and its session ended.
func TestInterruptedStartIsNotStarted(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "schedule", "add", "half", "app", "--cron", nightly, "--", "true")
	store, now := c.serverCfg.Store, c.clock.Now()
	if _, err := store.StartSession(t.Context(), "half-20261122-0200", "schedule:half", now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddRun(t.Context(), &pb.ScheduledRun{
		Schedule: "half", StartedAt: now.UnixMilli(), Outcome: pb.RunOutcome_RUN_OUTCOME_RUNNING, SessionId: "half-20261122-0200",
	}); err != nil {
		t.Fatal(err)
	}
	c.restart()
	c.tick()
	run := c.schedule("half").Runs[0]
	if run.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || run.Reason != "the controller stopped while starting it" || run.EndedAt == nil {
		t.Fatalf("interrupted run = %v", run)
	}
	if s := c.session("half-20261122-0200"); s.EndedAt == nil || s.EndReason != "its scheduled run could not start" || s.ScheduleRunning {
		t.Errorf("its session = %v", s)
	}
}
