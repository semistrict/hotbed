package main

import (
	"fmt"
	"path/filepath"
	"slices"
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

// at moves the clock to t and has the worker send a heartbeat at once, so
// the controller, its clock jumped, still counts the worker online.
func (c *cluster) at(t time.Time) {
	c.t.Helper()
	c.clock.set(t)
	c.heartbeat()
}

// later moves the clock on by d, as at does.
func (c *cluster) later(d time.Duration) {
	c.t.Helper()
	c.at(c.clock.Now().Add(d))
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
	c.ok(sc, "schedule", "add", "nightly", "-r", "app", "--cron", nightly, "--env", "WHO=nightly", "--",
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

	// The worker's event of its end settles the run, without a tick.
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
	// Its session ends after the run is settled: ending it releases the
	// workspace on its worker, which no transaction with the run can hold.
	c.eventually("the succeeded run's session to end", func() bool {
		return c.session(wantSession).EndedAt != nil
	})
	if s := c.session(wantSession); s.EndReason != "its scheduled run succeeded" {
		t.Errorf("the succeeded run's session = %v", s)
	}
	_, out, _ := c.hotbed("logs", "-s", wantSession, run.CommandId)
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
	c.ok(nil, "schedule", "add", "flaky", "-r", "app", "--cron", nightly, "--", "sh", "-c", waitFor(flag, "exit 3"))
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
	c.ok(nil, "schedule", "add", "hourly", "-r", "app", "--cron", "CRON_TZ=UTC 0 * * * *", "--", "sh", "-c", waitFor(flag, "true"))

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
	c.ok(nil, "schedule", "add", "soak", "-r", "app", "--cron", nightly, "--", "sh", "-c", waitFor(flag, "true"))
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
	c.ok(nil, "schedule", "add", "pinned", "-r", "app", "--cron", nightly, "--arch", "amd64", "--", "true")
	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "pinned")
	if run.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || run.Reason != "no node can take the run: asahimbp is arm64, not amd64" || run.EndedAt == nil {
		t.Fatalf("run on an arch that is not there = %v", run)
	}
	if s := c.session(run.SessionId); s.EndedAt == nil || s.EndReason != "its scheduled run could not start" {
		t.Errorf("the run's session = %v", s)
	}
}

// A schedule's runs go only to nodes of its architecture.
func TestScheduledRunKeepsToItsArchitecture(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	sc := &pb.ScheduledCommand{}
	c.ok(sc, "schedule", "add", "x86", "-r", "app", "--cron", nightly, "--arch", "amd64", "--", "true")
	if sc.Arch != "amd64" {
		t.Fatalf("schedule add --arch amd64 = %v", sc)
	}
	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "x86")
	if run.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || run.Reason != "no node can take the run: asahimbp is arm64, not amd64" {
		t.Errorf("run with no amd64 node = %v", run)
	}
}

// A schedule that sizes its runs' VM keeps the size, and its runs go only to
// nodes with the vm runtime.
func TestScheduledSizedRunNeedsAVMNode(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	sc := &pb.ScheduledCommand{}
	c.ok(sc, "schedule", "add", "big", "-r", "app", "--cron", nightly, "--vm-cpus", "2", "--vm-mem", "2G", "--", "true")
	if sc.Command.VmCpus != 2 || sc.Command.VmMemoryBytes != 2<<30 {
		t.Fatalf("schedule add --vm-cpus 2 --vm-mem 2G = %v", sc.Command)
	}
	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "big")
	if run.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || run.Reason != "no node can take the run: asahimbp does not support vm runtime" {
		t.Errorf("sized run with no vm node = %v", run)
	}
}

// A paused schedule starts nothing until resumed, and then not the slots it
// slept through.
func TestPausedScheduleRunsNothing(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.at(tomorrowAt(1, 0))
	c.ok(nil, "schedule", "add", "nightly", "-r", "app", "--cron", nightly, "--", "true")
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

// A run whose session ends under it (by hand, or abandoned with its worker
// gone, so its command's end may never be reported) ends with it, and the
// next run may start.
func TestScheduledRunEndsWithItsSession(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	flag := filepath.Join(t.TempDir(), "go")
	c.ok(nil, "schedule", "add", "soak", "-r", "app", "--cron", nightly, "--", "sh", "-c", waitFor(flag, "true"))
	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "soak")
	c.admitted(run.SessionId) // its base built, so nothing is left building when the test ends
	c.ok(nil, "session", "end", "-s", run.SessionId)
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
		res := &pb.SessionEvents{}
		c.ok(res, "poll", "-s", session, "--timeout", "1")
		events = append(events, res.Events...)
		return countWhat(events, "admitted") > 0
	})
}

// A run a controller recorded and stopped before its command started is
// recorded as not started by the next, and its session ended.
func TestInterruptedStartIsNotStarted(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "schedule", "add", "half", "-r", "app", "--cron", nightly, "--", "true")
	store, now := c.store, c.clock.Now()
	if _, err := store.AddProject(t.Context(), "app", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartSession(t.Context(), "half-20261122-0200", "schedule:half", "app", "", now); err != nil {
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

// A schedule keeps the project it was added in, so a repo that another
// project takes in later does not leave its runs not knowing which.
func TestScheduleKeepsItsProject(t *testing.T) {
	c := newCluster(t)
	t.Setenv("HOTBED_PROJECT", "")
	origin := upstream(t)
	c.addRepo(origin)
	sc := &pb.ScheduledCommand{}
	c.ok(sc, "schedule", "add", "nightly", "-r", "app", "--cron", nightly, "--", "true")
	if sc.Project == "" {
		t.Fatalf("schedule add: no project in %v", sc)
	}
	c.ok(nil, "project", "add", "elsewhere", "--repo", origin, "--shared")

	run := &pb.ScheduledRun{}
	c.ok(run, "schedule", "run", "nightly")
	if run.Outcome == pb.RunOutcome_RUN_OUTCOME_NOT_STARTED {
		t.Fatalf("run of a schedule whose repo two projects hold: not started: %s", run.Reason)
	}
	if s := c.session(run.SessionId); s.Project != sc.Project {
		t.Errorf("the run's session is in %q, want the schedule's %q", s.Project, sc.Project)
	}
	c.eventually("the run to succeed", func() bool {
		return c.schedule("nightly").Runs[0].Outcome == pb.RunOutcome_RUN_OUTCOME_SUCCEEDED
	})
}

// Schedules belong to a project: schedule ls lists the active project's by
// default, -p narrows to another, --all-projects lists every project's, and
// project ls counts them.
func TestSchedulesBelongToAProject(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	c.ok(nil, "project", "add", "other", "--repo", origin, "--shared")

	c.ok(nil, "schedule", "add", "in-test", "-r", "app", "-p", "app", "--cron", nightly, "--", "true")
	c.ok(nil, "schedule", "add", "in-other", "-r", "app", "-p", "other", "--cron", nightly, "--", "true")

	names := func(res *pb.ListSchedulesResponse) []string {
		var out []string
		for _, sc := range res.Schedules {
			out = append(out, sc.Name)
		}
		return out
	}

	ls := &pb.ListSchedulesResponse{}
	c.ok(ls, "schedule", "ls")
	if got := names(ls); !slices.Equal(got, []string{"in-test"}) {
		t.Errorf("schedule ls (default project app) = %v, want [in-test]", got)
	}
	c.ok(ls, "schedule", "ls", "-p", "other")
	if got := names(ls); !slices.Equal(got, []string{"in-other"}) {
		t.Errorf("schedule ls -p other = %v, want [in-other]", got)
	}
	c.ok(ls, "schedule", "ls", "--all-projects")
	if got := names(ls); !slices.Equal(got, []string{"in-other", "in-test"}) {
		t.Errorf("schedule ls --all-projects = %v, want [in-other in-test]", got)
	}

	projects := &pb.ListProjectsResponse{}
	c.ok(projects, "project", "ls")
	counts := map[string]int32{}
	for _, p := range projects.Projects {
		counts[p.Name] = p.Schedules
	}
	if counts["app"] != 1 || counts["other"] != 1 {
		t.Errorf("project ls schedule counts = %v, want app=1 other=1", counts)
	}
}
