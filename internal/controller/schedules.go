package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// Scheduled commands: the controller starts each run by itself, in a session
// of its own with a fresh workspace of the repo, and runs the command there
// detached, as an agent's hotbed run --detach would.

const (
	// A slot the controller first sees later than this was missed: it was
	// down. Within it (a restart, a deploy) the slot still runs.
	slotGrace = 5 * time.Minute
	// How often the controller looks for slots that are due.
	scheduleEvery = 15 * time.Second
	// Runs shown with each scheduled command when listed, and by GetSchedule.
	runsListed = 10
	runsShown  = 50
	// A schedule's name, and so its runs' session names (NAME-YYYYMMDD-HHMM),
	// fit a session name's 48 characters with room for a -N.
	maxScheduleName = 30
	// How long starting a run may take: its session, and the worker's answer
	// to its workspace and command, which come back at once (a workspace
	// prepares, and its command waits, on the worker's own time).
	startTimeout = 2 * time.Minute
)

var scheduleName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// Five fields, as crontab(5) has them; CRON_TZ=ZONE in front sets the zone.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// parseCron reads a schedule and the time zone its slots are in. A
// schedule whose slot never comes (February 30) is refused: cron's Next
// gives the zero time for it.
func parseCron(spec string) (cron.Schedule, *time.Location, error) {
	sched, err := cronParser.Parse(spec)
	if err == nil && sched.Next(time.Now()).IsZero() {
		err = fmt.Errorf("no such time comes in the next five years")
	}
	if err != nil {
		return nil, nil, api.Errorf(api.CodeInvalidArgs, "cron schedule %q: %v", spec, err).
			WithSuggestion(`five fields, minute hour day-of-month month day-of-week, such as "0 2 * * *" for 02:00 daily`)
	}
	loc := time.Local
	if s, ok := sched.(*cron.SpecSchedule); ok && s.Location != nil {
		loc = s.Location
	}
	return sched, loc, nil
}

// RunSchedules starts what is due and settles what ended, until ctx ends.
func (s *Server) RunSchedules(ctx context.Context) {
	t := time.NewTicker(scheduleEvery)
	defer t.Stop()
	for {
		if err := s.TickSchedules(ctx); err != nil {
			slog.Error("run scheduled commands", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// TickSchedules settles the runs whose commands ended, then, for each
// schedule whose slot has come, starts its run, or records the slot as
// skipped (the previous run is still in progress) or missed (it passed
// while the controller was down).
func (s *Server) TickSchedules(ctx context.Context) error {
	s.scheduling.Lock()
	defer s.scheduling.Unlock()
	if err := s.failInterruptedStarts(ctx); err != nil {
		return err
	}
	if err := s.settleRuns(ctx); err != nil {
		return err
	}
	all, err := s.cfg.Store.Schedules(ctx)
	if err != nil {
		return err
	}
	for _, sc := range all {
		if err := s.tick(ctx, sc); err != nil {
			slog.Error("run scheduled command", "schedule", sc.Name, "err", err)
		}
	}
	return nil
}

// tick handles one schedule's slots that have come.
func (s *Server) tick(ctx context.Context, sc *pb.ScheduledCommand) error {
	if sc.Paused || sc.NextAt == nil {
		return nil
	}
	now := s.now()
	first := time.UnixMilli(*sc.NextAt)
	if now.Before(first) {
		return nil
	}
	sched, loc, err := parseCron(sc.Cron)
	if err != nil {
		return err
	}
	// Every slot from the one due through now: the newest may still run.
	slots := []time.Time{first}
	for t := sched.Next(first); !t.IsZero() && !t.After(now); t = sched.Next(t) {
		slots = append(slots, t)
	}
	latest := slots[len(slots)-1]
	missed := slots[:len(slots)-1]
	if now.Sub(latest) > slotGrace {
		missed = slots
	}
	// The next slot is recorded before anything starts: a controller that
	// stops halfway then loses this slot, rather than running it twice.
	next := sched.Next(now)
	var nextAt *int64
	if !next.IsZero() {
		nextAt = new(next.UnixMilli())
	}
	if err := s.cfg.Store.SetNextAt(ctx, sc.Name, nextAt); err != nil {
		return err
	}
	if len(missed) > 0 {
		at := missed[len(missed)-1].UnixMilli()
		ended := now.UnixMilli()
		run := &pb.ScheduledRun{
			Schedule: sc.Name, SlotAt: &at, StartedAt: ended, EndedAt: &ended, Outcome: pb.RunOutcome_RUN_OUTCOME_MISSED,
			Reason: fmt.Sprintf("the controller was down: missed %d %s since %s", len(missed), plural(len(missed), "slot"), clock(missed[0], loc)),
		}
		if err := s.cfg.Store.AddRun(ctx, run); err != nil {
			return err
		}
	}
	if len(missed) < len(slots) {
		return s.startOrSkip(ctx, sc, latest, loc)
	}
	return nil
}

// startOrSkip starts the run of a slot, unless the previous run is still in
// progress: runs never overlap.
func (s *Server) startOrSkip(ctx context.Context, sc *pb.ScheduledCommand, slot time.Time, loc *time.Location) error {
	prev, busy, err := s.cfg.Store.LiveRun(ctx, sc.Name)
	if err != nil {
		return err
	}
	at := slot.UnixMilli()
	if !busy {
		_, err := s.start(ctx, sc, &at, loc)
		return err
	}
	how := "started at " + clock(time.UnixMilli(prev.StartedAt), loc)
	if prev.SlotAt != nil {
		how = "due at " + clock(time.UnixMilli(*prev.SlotAt), loc)
	}
	now := s.now().UnixMilli()
	return s.cfg.Store.AddRun(ctx, &pb.ScheduledRun{
		Schedule: sc.Name, SlotAt: &at, StartedAt: now, EndedAt: &now, Outcome: pb.RunOutcome_RUN_OUTCOME_SKIPPED,
		Reason: fmt.Sprintf("the run %s (%s) was still in progress", how, prev.SessionId),
	})
}

// start begins a run: its session, its workspace, its command. The run is
// recorded before the workspace and command, so a controller that stops
// halfway leaves a run without a command, which the next tick fails. A run
// that cannot start is recorded as such, with why; it returns an error only
// when the controller's own database fails.
//
// It runs on its own deadline, not its caller's: a hotbed schedule run
// interrupted after the worker took the command must not leave a command its
// run never heard of.
func (s *Server) start(ctx context.Context, sc *pb.ScheduledCommand, slot *int64, loc *time.Location) (*pb.ScheduledRun, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
	defer cancel()
	now := s.now()
	at := now
	if slot != nil {
		at = time.UnixMilli(*slot)
	}
	run := &pb.ScheduledRun{Schedule: sc.Name, SlotAt: slot, StartedAt: now.UnixMilli(), Outcome: pb.RunOutcome_RUN_OUTCOME_RUNNING}
	project, err := s.runProject(ctx, sc)
	var refused *api.Error
	if errors.As(err, &refused) {
		ended := now.UnixMilli()
		run.Outcome, run.Reason, run.EndedAt = pb.RunOutcome_RUN_OUTCOME_NOT_STARTED, refused.Message, &ended
		return run, s.cfg.Store.AddRun(ctx, run)
	}
	if err != nil {
		return nil, err
	}
	session, err := s.runSession(ctx, sc.Name+"-"+at.In(loc).Format("20060102-1504"), "schedule:"+sc.Name, project.Name)
	if err != nil {
		return nil, err
	}
	run.SessionId = session
	if err := s.cfg.Store.AddRun(ctx, run); err != nil {
		return nil, err
	}
	ws := &workspaces{s}
	w, err := ws.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{SessionId: session, Repo: sc.Repo, Node: sc.Node})
	if err != nil {
		return run, s.notStarted(ctx, run, err)
	}
	run.WorkspaceId, run.NodeId = w.Id, w.NodeId
	req := proto.Clone(sc.Command).(*pb.RunCommandRequest)
	req.WorkspaceId, req.Id, req.Attached, req.Port = w.Id, "", false, 0
	cmd, err := ws.RunCommand(ctx, req)
	if err != nil {
		return run, s.notStarted(ctx, run, err)
	}
	run.CommandId = cmd.Id
	_, err = s.cfg.Store.UpdateRun(ctx, run)
	return run, err
}

// runProject is the project a schedule's runs work on: its own, or, for a
// schedule added before schedules had one, the one holding its repo.
func (s *Server) runProject(ctx context.Context, sc *pb.ScheduledCommand) (*pb.Project, error) {
	repo, err := s.repo(ctx, sc.Repo)
	if err != nil {
		return nil, err
	}
	return s.sessionProject(ctx, sc.Project, gitops.CanonicalRemote(repo.Url))
}

// runSession starts a run's session, called name, or name-2, name-3 when
// that is taken (two runs in a minute, or an agent's session).
func (s *Server) runSession(ctx context.Context, name, agent, project string) (string, error) {
	for n := 1; n <= 20; n++ {
		id := name
		if n > 1 {
			id = fmt.Sprintf("%s-%d", name, n)
		}
		_, err := s.cfg.Store.StartSession(ctx, id, agent, project, s.now())
		if err == nil {
			return id, nil
		}
		if api.FromConnect(err).Code != api.CodeConflict {
			return "", err
		}
	}
	return "", api.Errorf(api.CodeConflict, "no free session name for %s", name)
}

// notStarted records that a run could not start, and why, and ends its
// session. A session that will not end now is left to the idle reaper, which
// no longer spares it.
func (s *Server) notStarted(ctx context.Context, run *pb.ScheduledRun, why error) error {
	slog.Warn("scheduled run could not start", "schedule", run.Schedule, "session", run.SessionId, "err", why)
	ended := s.now().UnixMilli()
	run.Outcome, run.Reason, run.EndedAt = pb.RunOutcome_RUN_OUTCOME_NOT_STARTED, api.FromConnect(why).Message, &ended
	if ok, err := s.cfg.Store.UpdateRun(ctx, run); err != nil || !ok {
		return err
	}
	if _, err := s.endSession(ctx, run.SessionId, "its scheduled run could not start"); err != nil {
		slog.Error("end the session of a run that could not start", "session", run.SessionId, "err", err)
	}
	return nil
}

// failInterruptedStarts fails the runs a controller began and stopped before
// their commands started. Call with s.scheduling held, so none is starting.
func (s *Server) failInterruptedStarts(ctx context.Context) error {
	live, err := s.cfg.Store.LiveRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range live {
		if run.CommandId == "" {
			if err := s.notStarted(ctx, run, fmt.Errorf("the controller stopped while starting it")); err != nil {
				return err
			}
		}
	}
	return nil
}

// settleRuns records the end of each run whose command ended. One that
// succeeded ends its session. One that failed leaves it for the idle hour,
// from now, so an agent can look inside; its workspace goes after that.
// A session that will not end now is left to the idle reaper.
func (s *Server) settleRuns(ctx context.Context) error {
	live, err := s.cfg.Store.LiveRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range live {
		if run.CommandId == "" {
			continue
		}
		f, ok, err := s.cfg.Store.Finished(ctx, run.CommandId)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		run.EndedAt, run.ExitCode, run.Reason = &f.EndedAt, f.ExitCode, f.Reason
		switch f.State {
		case pb.CommandState_COMMAND_STATE_SUCCEEDED:
			run.Outcome = pb.RunOutcome_RUN_OUTCOME_SUCCEEDED
		case pb.CommandState_COMMAND_STATE_CANCELLED:
			run.Outcome = pb.RunOutcome_RUN_OUTCOME_CANCELLED
		default:
			run.Outcome = pb.RunOutcome_RUN_OUTCOME_FAILED
			if run.Reason == "" && f.ExitCode != nil {
				run.Reason = fmt.Sprintf("exited %d", *f.ExitCode)
			}
		}
		// The idle hour of a failed run's session starts now; touched first,
		// so the reaper, which spares it until the run ends, never sees it
		// ended and idle since its start.
		if run.Outcome != pb.RunOutcome_RUN_OUTCOME_SUCCEEDED {
			if err := s.cfg.Store.Touch(ctx, run.SessionId, s.now()); err != nil {
				return err
			}
		}
		if ok, err := s.cfg.Store.UpdateRun(ctx, run); err != nil {
			return err
		} else if !ok {
			continue // settled already, by a report and a tick at once
		}
		if run.Outcome == pb.RunOutcome_RUN_OUTCOME_SUCCEEDED {
			if _, err := s.endSession(ctx, run.SessionId, "its scheduled run succeeded"); err != nil {
				slog.Error("end the session of a run that succeeded", "session", run.SessionId, "err", err)
			}
		}
	}
	return nil
}

// endRuns settles the runs in progress in a session that ended under them,
// stopped by hand or abandoned with its worker gone: their commands' ends
// may never be reported, and a run left in progress would hold its
// schedule's next slots forever.
func (s *Server) endRuns(ctx context.Context, session, why string) error {
	runs, err := s.cfg.Store.SessionRuns(ctx, session)
	if err != nil {
		return err
	}
	for _, run := range runs {
		ended := s.now().UnixMilli()
		run.Outcome, run.Reason, run.EndedAt = pb.RunOutcome_RUN_OUTCOME_CANCELLED, "its session ended: "+why, &ended
		if _, err := s.cfg.Store.UpdateRun(ctx, run); err != nil {
			return err
		}
	}
	return nil
}

// clock is a slot's time of day, as its schedule reads it.
func clock(t time.Time, loc *time.Location) string { return t.In(loc).Format("15:04") }

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// ---- ScheduleService ------------------------------------------------------

// schedules serves ScheduleService.
type schedules struct{ *Server }

func (s *schedules) PutSchedule(ctx context.Context, req *pb.PutScheduleRequest) (*pb.ScheduledCommand, error) {
	if req.Schedule == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "no scheduled command")
	}
	sc := proto.Clone(req.Schedule).(*pb.ScheduledCommand)
	if !scheduleName.MatchString(sc.Name) || len(sc.Name) > maxScheduleName {
		return nil, api.Errorf(api.CodeInvalidArgs, "scheduled command name %q must be lowercase words joined by dashes, at most %d characters", sc.Name, maxScheduleName)
	}
	if _, _, err := parseCron(sc.Cron); err != nil {
		return nil, err
	}
	repo, err := s.repo(ctx, sc.Repo)
	if err != nil {
		return nil, err
	}
	// Settled now, so a repo that a second project takes in later does not
	// leave the runs not knowing which project they are in.
	project, err := s.sessionProject(ctx, sc.Project, gitops.CanonicalRemote(repo.Url))
	if err != nil {
		return nil, err
	}
	sc.Project = project.Name
	if len(sc.GetCommand().GetArgs()) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "scheduled command %s has no command", sc.Name).
			WithSuggestion("hotbed schedule add NAME REPO --cron '0 2 * * *' -- CMD...")
	}
	for _, kv := range sc.Command.Env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "env %q is not KEY=VALUE", kv)
		}
	}
	sc.Command.WorkspaceId, sc.Command.Id, sc.Command.Attached, sc.Command.Port = "", "", false, 0
	now := s.now().UnixMilli()
	sc.CreatedAt, sc.UpdatedAt, sc.Runs = now, now, nil
	if old, err := s.cfg.Store.Schedule(ctx, sc.Name); err == nil {
		sc.CreatedAt = old.CreatedAt
	}
	s.scheduling.Lock()
	defer s.scheduling.Unlock()
	if err := s.planNext(sc); err != nil {
		return nil, err
	}
	if err := s.cfg.Store.PutSchedule(ctx, sc); err != nil {
		return nil, err
	}
	return s.withRuns(ctx, sc, runsListed)
}

// planNext sets when a schedule's next run is due: its next slot after now,
// or none while it is paused.
func (s *Server) planNext(sc *pb.ScheduledCommand) error {
	sc.NextAt = nil
	if sc.Paused {
		return nil
	}
	sched, _, err := parseCron(sc.Cron)
	if err != nil {
		return err
	}
	next := sched.Next(s.now()).UnixMilli()
	sc.NextAt = &next
	return nil
}

// withRuns is a scheduled command with its last n runs.
func (s *Server) withRuns(ctx context.Context, sc *pb.ScheduledCommand, n int) (*pb.ScheduledCommand, error) {
	runs, err := s.cfg.Store.Runs(ctx, sc.Name, n)
	sc.Runs = runs
	return sc, err
}

// allSchedules is every scheduled command with its recent runs.
func (s *Server) allSchedules(ctx context.Context) ([]*pb.ScheduledCommand, error) {
	all, err := s.cfg.Store.Schedules(ctx)
	if err != nil {
		return nil, err
	}
	for _, sc := range all {
		if _, err := s.withRuns(ctx, sc, runsListed); err != nil {
			return nil, err
		}
	}
	return all, nil
}

func (s *schedules) ListSchedules(ctx context.Context, _ *pb.ListSchedulesRequest) (*pb.ListSchedulesResponse, error) {
	all, err := s.allSchedules(ctx)
	return &pb.ListSchedulesResponse{Schedules: all}, err
}

func (s *schedules) GetSchedule(ctx context.Context, req *pb.GetScheduleRequest) (*pb.ScheduledCommand, error) {
	sc, err := s.cfg.Store.Schedule(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	return s.withRuns(ctx, sc, runsShown)
}

func (s *schedules) DeleteSchedule(ctx context.Context, req *pb.DeleteScheduleRequest) (*pb.DeleteScheduleResponse, error) {
	s.scheduling.Lock()
	defer s.scheduling.Unlock()
	found, err := s.cfg.Store.DeleteSchedule(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, api.Errorf(api.CodeNotFound, "no scheduled command %s", req.Name).WithSuggestion("hotbed schedule ls")
	}
	return &pb.DeleteScheduleResponse{}, nil
}

// RunSchedule starts a run now, outside the schedule; its slot is unset.
func (s *schedules) RunSchedule(ctx context.Context, req *pb.RunScheduleRequest) (*pb.ScheduledRun, error) {
	s.scheduling.Lock()
	defer s.scheduling.Unlock()
	sc, err := s.cfg.Store.Schedule(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if err := s.settleRuns(ctx); err != nil {
		return nil, err
	}
	if prev, busy, err := s.cfg.Store.LiveRun(ctx, sc.Name); err != nil {
		return nil, err
	} else if busy {
		return nil, api.Errorf(api.CodeConflict, "a run of %s is in progress, in session %s", sc.Name, prev.SessionId).
			WithSuggestion(fmt.Sprintf("hotbed wait %s until it ends, or hotbed session end %s to stop it", prev.SessionId, prev.SessionId))
	}
	_, loc, err := parseCron(sc.Cron)
	if err != nil {
		return nil, err
	}
	return s.start(ctx, sc, nil, loc)
}

// PauseSchedule pauses a schedule or resumes it. Resumed, it runs from its
// next slot: the slots it slept through are neither run nor missed.
func (s *schedules) PauseSchedule(ctx context.Context, req *pb.PauseScheduleRequest) (*pb.ScheduledCommand, error) {
	s.scheduling.Lock()
	defer s.scheduling.Unlock()
	sc, err := s.cfg.Store.Schedule(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	sc.Paused, sc.UpdatedAt = req.Paused, s.now().UnixMilli()
	if err := s.planNext(sc); err != nil {
		return nil, err
	}
	if err := s.cfg.Store.PutSchedule(ctx, sc); err != nil {
		return nil, err
	}
	return s.withRuns(ctx, sc, runsListed)
}
