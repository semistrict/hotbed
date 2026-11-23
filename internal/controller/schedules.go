package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
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
// Launches run asynchronously so a schedule waking a sleeping node does not
// stall subsequent ticks or other schedules.
func (s *Server) RunSchedules(ctx context.Context) {
	t := time.NewTicker(scheduleEvery)
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		toEnd, toStart, err := s.tickDue(ctx)
		if err != nil {
			slog.Error("run scheduled commands", "err", err)
		}
		s.endScheduledSessions(ctx, toEnd)
		s.launch(toStart, &wg)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type startingRun struct {
	schedule  string
	scheduled bool // true for cron-triggered ticks, false for manual RunSchedule
	cancel    context.CancelFunc
	stopWhy   string
}

type pendingStart struct {
	sc        *pb.ScheduledCommand
	run       *pb.ScheduledRun
	parentCtx context.Context
	ctx       context.Context
	sr        *startingRun
}

type sessionEnd struct {
	id, reason string
}

func (s *Server) registerStartingRunLocked(runID int64, sr *startingRun) {
	if s.startingRuns == nil {
		s.startingRuns = map[int64]*startingRun{}
	}
	s.startingRuns[runID] = sr
	s.startsWG.Add(1)
}

func (s *Server) unregisterStartingRun(runID int64) {
	s.scheduling.Lock()
	_, ok := s.startingRuns[runID]
	delete(s.startingRuns, runID)
	s.scheduling.Unlock()
	if ok {
		s.startsWG.Done()
	}
}

// cancelStartingRunsLocked cancels in-flight starts while s.scheduling is held.
// An empty schedule matches all schedules. When scheduledOnly is true, manual
// RunSchedule starts are left alone.
func (s *Server) cancelStartingRunsLocked(schedule string, scheduledOnly bool, why string) {
	for _, sr := range s.startingRuns {
		if schedule != "" && sr.schedule != schedule {
			continue
		}
		if scheduledOnly && !sr.scheduled {
			continue
		}
		if sr.stopWhy == "" {
			sr.stopWhy = why
		}
		sr.cancel()
	}
}

func (s *Server) startingRunState(sr *startingRun) (stopWhy string, shuttingDown bool) {
	s.scheduling.Lock()
	defer s.scheduling.Unlock()
	return sr.stopWhy, s.shuttingDown
}

// beginScheduleShutdown marks the server as shutting down and cancels all
// in-flight scheduled and manual schedule starts. Safe to call multiple times.
func (s *Server) beginScheduleShutdown() {
	s.scheduling.Lock()
	s.shuttingDown = true
	s.cancelStartingRunsLocked("", false, "the controller stopped while starting it")
	s.scheduling.Unlock()
}

// Shutdown cancels all in-flight schedule starts (including manual RunSchedule
// calls) and waits for them to finish cleanup so none can write to the store
// after it closes.
func (s *Server) Shutdown() {
	s.beginScheduleShutdown()
	s.startsWG.Wait()
}

func (s *Server) launch(toStart []pendingStart, wg *sync.WaitGroup) {
	for _, p := range toStart {
		wg.Add(1)
		go func(p pendingStart) {
			defer wg.Done()
			if _, err := s.finishStart(p); err != nil {
				slog.Error("run scheduled command", "schedule", p.sc.Name, "err", err)
			}
		}(p)
	}
}

// TickSchedules settles the runs whose commands ended, then, for each
// schedule whose slot has come, starts its run, or records the slot as
// skipped (the previous run is still in progress) or missed (it passed
// while the controller was down). Slow worker RPCs and node wake waits run
// after releasing s.scheduling.
func (s *Server) TickSchedules(ctx context.Context) error {
	toEnd, toStart, err := s.tickDue(ctx)
	s.endScheduledSessions(ctx, toEnd)
	var wg sync.WaitGroup
	s.launch(toStart, &wg)
	wg.Wait()
	return err
}

func (s *Server) tickDue(ctx context.Context) ([]sessionEnd, []pendingStart, error) {
	s.scheduling.Lock()
	defer s.scheduling.Unlock()
	if s.shuttingDown {
		return nil, nil, nil
	}
	var toEnd []sessionEnd
	interruptedEnd, err := s.failInterruptedStartsLocked(ctx)
	toEnd = append(toEnd, interruptedEnd...)
	if err != nil {
		return toEnd, nil, err
	}
	settledEnd, err := s.settleRunsLocked(ctx)
	toEnd = append(toEnd, settledEnd...)
	if err != nil {
		return toEnd, nil, err
	}
	all, err := s.cfg.Store.Schedules(ctx, "")
	if err != nil {
		return toEnd, nil, err
	}
	var toStart []pendingStart
	for _, sc := range all {
		p, err := s.tickLocked(ctx, sc)
		if err != nil {
			slog.Error("run scheduled command", "schedule", sc.Name, "err", err)
			continue
		}
		if p != nil {
			toStart = append(toStart, *p)
		}
	}
	return toEnd, toStart, nil
}

func (s *Server) endScheduledSessions(ctx context.Context, ends []sessionEnd) {
	for _, e := range ends {
		if _, err := s.endSession(ctx, e.id, e.reason); err != nil {
			slog.Error("end the session of a scheduled run", "session", e.id, "reason", e.reason, "err", err)
		}
	}
}

// tickLocked handles one schedule's slots that have come, with s.scheduling held.
// If a run needs to be launched on a worker, it returns a pendingStart to finish
// after s.scheduling is released.
func (s *Server) tickLocked(ctx context.Context, sc *pb.ScheduledCommand) (*pendingStart, error) {
	if sc.Paused || sc.NextAt == nil {
		return nil, nil
	}
	now := s.now()
	first := time.UnixMilli(*sc.NextAt)
	if now.Before(first) {
		return nil, nil
	}
	sched, loc, err := parseCron(sc.Cron)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if len(missed) > 0 {
		at := missed[len(missed)-1].UnixMilli()
		ended := now.UnixMilli()
		run := &pb.ScheduledRun{
			Schedule: sc.Name, SlotAt: &at, StartedAt: ended, EndedAt: &ended, Outcome: pb.RunOutcome_RUN_OUTCOME_MISSED,
			Reason: fmt.Sprintf("the controller was down: missed %d %s since %s", len(missed), plural(len(missed), "slot"), clock(missed[0], loc)),
		}
		if err := s.cfg.Store.AddRun(ctx, run); err != nil {
			return nil, err
		}
	}
	if len(missed) < len(slots) {
		return s.startOrSkipLocked(ctx, sc, latest, loc)
	}
	return nil, nil
}

// startOrSkipLocked reserves the run of a slot, unless the previous run is still
// in progress: runs never overlap.
func (s *Server) startOrSkipLocked(ctx context.Context, sc *pb.ScheduledCommand, slot time.Time, loc *time.Location) (*pendingStart, error) {
	prev, busy, err := s.cfg.Store.LiveRun(ctx, sc.Name)
	if err != nil {
		return nil, err
	}
	at := slot.UnixMilli()
	if !busy {
		p, _, err := s.beginStartLocked(ctx, sc, &at, loc, true)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	how := "started at " + clock(time.UnixMilli(prev.StartedAt), loc)
	if prev.SlotAt != nil {
		how = "due at " + clock(time.UnixMilli(*prev.SlotAt), loc)
	}
	now := s.now().UnixMilli()
	return nil, s.cfg.Store.AddRun(ctx, &pb.ScheduledRun{
		Schedule: sc.Name, SlotAt: &at, StartedAt: now, EndedAt: &now, Outcome: pb.RunOutcome_RUN_OUTCOME_SKIPPED,
		Reason: fmt.Sprintf("the run %s (%s) was still in progress", how, prev.SessionId),
	})
}

// beginStartLocked reserves a run's session and live run row under s.scheduling,
// and marks the run in-flight in s.startingRuns so concurrent ticks neither
// overlap it nor treat it as an interrupted start. When pending is non-nil, the
// caller must call finishStart after releasing s.scheduling.
func (s *Server) beginStartLocked(ctx context.Context, sc *pb.ScheduledCommand, slot *int64, loc *time.Location, cancelOnCtx bool) (pending *pendingStart, run *pb.ScheduledRun, err error) {
	if s.shuttingDown {
		return nil, nil, &api.Error{Code: api.CodeControllerUnreachable, Message: "the controller is shutting down", Retryable: true}
	}
	now := s.now()
	at := now
	if slot != nil {
		at = time.UnixMilli(*slot)
	}
	run = &pb.ScheduledRun{Schedule: sc.Name, SlotAt: slot, StartedAt: now.UnixMilli(), Outcome: pb.RunOutcome_RUN_OUTCOME_RUNNING}
	project, err := s.runProject(ctx, sc)
	var refused *api.Error
	if errors.As(err, &refused) {
		ended := now.UnixMilli()
		run.Outcome, run.Reason, run.EndedAt = pb.RunOutcome_RUN_OUTCOME_NOT_STARTED, refused.Message, &ended
		return nil, run, s.cfg.Store.AddRun(ctx, run)
	}
	if err != nil {
		return nil, nil, err
	}
	session, err := s.runSession(ctx, sc.Name+"-"+at.In(loc).Format("20060102-1504"), "schedule:"+sc.Name, project.Name)
	if err != nil {
		return nil, nil, err
	}
	run.SessionId = session
	if err := s.cfg.Store.AddRun(ctx, run); err != nil {
		return nil, nil, err
	}
	base := ctx
	if !cancelOnCtx {
		base = context.WithoutCancel(ctx)
	}
	timeout := max(startTimeout, bootTimeout+s.power.wakeWait+30*time.Second)
	startCtx, cancel := context.WithTimeout(base, timeout)
	sr := &startingRun{schedule: sc.Name, scheduled: slot != nil, cancel: cancel}
	s.registerStartingRunLocked(run.Id, sr)
	return &pendingStart{sc: sc, run: run, parentCtx: ctx, ctx: startCtx, sr: sr}, run, nil
}

// finishStart launches the workspace and command for a run reserved by
// beginStartLocked, without holding s.scheduling.
//
// Once RunCommand is invoked, it uses its own deadline rather than its
// caller's so a hotbed schedule run interrupted after the worker took the
// command never leaves a command its run never heard of.
func (s *Server) finishStart(p pendingStart) (*pb.ScheduledRun, error) {
	defer func() {
		p.sr.cancel()
		s.unregisterStartingRun(p.run.Id)
	}()

	abortReason := func(err error) error {
		if why, _ := s.startingRunState(p.sr); why != "" {
			return errors.New(why)
		}
		if p.ctx.Err() != nil && p.parentCtx.Err() != nil {
			return errors.New("the controller stopped while starting it")
		}
		return err
	}
	cleanupCtxFor := func() (context.Context, context.CancelFunc) {
		_, shuttingDown := s.startingRunState(p.sr)
		timeout := startTimeout
		if p.parentCtx.Err() != nil || shuttingDown {
			timeout = 5 * time.Second
		}
		return context.WithTimeout(context.WithoutCancel(p.parentCtx), timeout)
	}
	abortNotStarted := func(err error) (*pb.ScheduledRun, error) {
		cleanupCtx, cleanupCancel := cleanupCtxFor()
		defer cleanupCancel()
		return s.notStarted(cleanupCtx, p.run, abortReason(err))
	}

	if why, _ := s.startingRunState(p.sr); why != "" {
		return abortNotStarted(errors.New(why))
	}
	if err := p.ctx.Err(); err != nil {
		return abortNotStarted(err)
	}

	ws := &workspaces{Server: s}
	w, err := ws.createAwake(p.ctx, &pb.CreateWorkspaceRequest{SessionId: p.run.SessionId, Repo: p.sc.Repo, Placement: &pb.Placement{
		Node: p.sc.Node, Arch: p.sc.Arch, Bench: p.sc.Command.GetBench(), MemoryLowBytes: p.sc.Command.GetLimits().GetMemoryLowBytes(),
	}})
	if err != nil {
		return abortNotStarted(err)
	}
	p.run.WorkspaceId, p.run.NodeId = w.Id, w.NodeId
	if why, _ := s.startingRunState(p.sr); why != "" {
		return abortNotStarted(errors.New(why))
	}
	if err := p.ctx.Err(); err != nil {
		return abortNotStarted(err)
	}

	launchCtx, launchCancel := context.WithTimeout(context.WithoutCancel(p.parentCtx), startTimeout)
	defer launchCancel()
	req := proto.Clone(p.sc.Command).(*pb.RunCommandRequest)
	// It runs in its repo's checkout, as a run from a checkout of the repo does.
	req.WorkspaceId, req.Id, req.Attached, req.Port, req.Repo = w.Id, "", false, 0, p.sc.Repo
	cmd, err := ws.runCommand(launchCtx, req)
	if err != nil {
		return abortNotStarted(err)
	}
	if why, _ := s.startingRunState(p.sr); why != "" {
		stopCtx, stopCancel := cleanupCtxFor()
		defer stopCancel()
		if _, serr := ws.stopCommand(stopCtx, &pb.StopCommandRequest{Id: cmd.Id, Internal: true, Reason: why}); serr != nil {
			slog.Warn("stop command of cancelled scheduled run", "schedule", p.run.Schedule, "command", cmd.Id, "err", serr)
		}
		return s.notStarted(stopCtx, p.run, errors.New(why))
	}
	p.run.CommandId = cmd.Id
	ok, err := s.cfg.Store.UpdateRun(launchCtx, p.run)
	if err != nil {
		return p.run, err
	}
	if !ok {
		// The run was ended concurrently (e.g. its session was ended while
		// starting): stop the just-launched command and clean up its workspace,
		// then return the authoritative stored terminal run.
		if _, serr := ws.stopCommand(launchCtx, &pb.StopCommandRequest{Id: cmd.Id, Internal: true, Reason: "scheduled run ended concurrently"}); serr != nil {
			slog.Warn("stop command of concurrently ended scheduled run", "schedule", p.run.Schedule, "command", cmd.Id, "err", serr)
		}
		if sess, serr := s.cfg.Store.Session(launchCtx, p.run.SessionId); serr == nil && sess.EndedAt != nil {
			if rerr := s.releaseWorkspace(launchCtx, w.Id); rerr != nil && api.CodeOf(rerr) == api.CodeWorkerUnreachable {
				_ = s.cfg.Store.ReleaseWorkspace(launchCtx, w.Id, s.now())
			}
		}
		stored, rerr := s.cfg.Store.Run(launchCtx, p.run.Id)
		if rerr != nil {
			return nil, rerr
		}
		return stored, nil
	}
	return p.run, nil
}

// runProject is the project a schedule's runs work on.
func (s *Server) runProject(ctx context.Context, sc *pb.ScheduledCommand) (*pb.Project, error) {
	return s.cfg.Store.Project(ctx, sc.Project)
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
		if api.CodeOf(err) != api.CodeConflict {
			return "", err
		}
	}
	return "", api.Errorf(api.CodeConflict, "no free session name for %s", name)
}

// notStarted records that a run could not start, and why, and ends its
// session. A session that will not end now is left to the idle reaper, which
// no longer spares it.
func (s *Server) notStarted(ctx context.Context, run *pb.ScheduledRun, why error) (*pb.ScheduledRun, error) {
	end, err := s.markNotStarted(ctx, run, why)
	if err != nil {
		return run, err
	}
	if end == nil {
		// Already terminal concurrently: return the authoritative stored run.
		stored, rerr := s.cfg.Store.Run(ctx, run.Id)
		if rerr != nil {
			return nil, rerr
		}
		return stored, nil
	}
	if _, err := s.endSession(ctx, end.id, end.reason); err != nil {
		slog.Error("end the session of a run that could not start", "session", end.id, "err", err)
	}
	return run, nil
}

func (s *Server) markNotStarted(ctx context.Context, run *pb.ScheduledRun, why error) (*sessionEnd, error) {
	slog.Warn("scheduled run could not start", "schedule", run.Schedule, "session", run.SessionId, "err", why)
	ended := s.now().UnixMilli()
	run.Outcome, run.Reason, run.EndedAt = pb.RunOutcome_RUN_OUTCOME_NOT_STARTED, api.FromConnect(why).Message, &ended
	ok, err := s.cfg.Store.UpdateRun(ctx, run)
	if err != nil || !ok {
		return nil, err
	}
	return &sessionEnd{id: run.SessionId, reason: "its scheduled run could not start"}, nil
}

// failInterruptedStartsLocked fails the runs a controller began and stopped
// before their commands started. Call with s.scheduling held; runs currently in
// s.startingRuns are skipped, and returned sessions are ended after unlock.
func (s *Server) failInterruptedStartsLocked(ctx context.Context) ([]sessionEnd, error) {
	live, err := s.cfg.Store.LiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	var toEnd []sessionEnd
	for _, run := range live {
		if run.CommandId == "" && s.startingRuns[run.Id] == nil {
			end, err := s.markNotStarted(ctx, run, fmt.Errorf("the controller stopped while starting it"))
			if err != nil {
				return toEnd, err
			}
			if end != nil {
				toEnd = append(toEnd, *end)
			}
		}
	}
	return toEnd, nil
}

// settleRuns records the end of each run whose command ended. One that
// succeeded ends its session. One that failed leaves it for the idle hour,
// from now, so an agent can look inside; its workspace goes after that.
// A session that will not end now is left to the idle reaper.
func (s *Server) settleRuns(ctx context.Context) error {
	toEnd, err := s.settleRunsLocked(ctx)
	s.endScheduledSessions(ctx, toEnd)
	return err
}

func (s *Server) settleRunsLocked(ctx context.Context) ([]sessionEnd, error) {
	live, err := s.cfg.Store.LiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	var toEnd []sessionEnd
	for _, run := range live {
		if run.CommandId == "" {
			continue
		}
		f, ok, err := s.cfg.Store.Finished(ctx, run.CommandId)
		if err != nil {
			return toEnd, err
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
				return toEnd, err
			}
		}
		if ok, err := s.cfg.Store.UpdateRun(ctx, run); err != nil {
			return toEnd, err
		} else if !ok {
			continue // settled already, by a report and a tick at once
		}
		if run.Outcome == pb.RunOutcome_RUN_OUTCOME_SUCCEEDED {
			toEnd = append(toEnd, sessionEnd{id: run.SessionId, reason: "its scheduled run succeeded"})
		}
	}
	return toEnd, nil
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
	project, changed, err := s.sessionProject(ctx, sc.Project, gitops.CanonicalRemote(repo.Url))
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
	sc.CreatedAt, sc.UpdatedAt, sc.Runs, sc.Reminder = now, now, nil, nil
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
	if sc.Paused {
		s.cancelStartingRunsLocked(sc.Name, true, fmt.Sprintf("schedule %s was paused while starting", sc.Name))
	}
	out, err := s.withRuns(ctx, sc, runsListed)
	if err != nil {
		return nil, err
	}
	if changed {
		reminder := setupStepsReminder(project)
		out.Reminder = &reminder
	}
	return out, nil
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

// allSchedules is every scheduled command of project (empty: every
// project's), with its recent runs.
func (s *Server) allSchedules(ctx context.Context, project string) ([]*pb.ScheduledCommand, error) {
	all, err := s.cfg.Store.Schedules(ctx, project)
	if err != nil || len(all) == 0 {
		return all, err
	}
	runsBySchedule, err := s.cfg.Store.RecentRuns(ctx, project, runsListed)
	if err != nil {
		return nil, err
	}
	for _, sc := range all {
		sc.Runs = runsBySchedule[sc.Name]
	}
	return all, nil
}

func (s *schedules) ListSchedules(ctx context.Context, req *pb.ListSchedulesRequest) (*pb.ListSchedulesResponse, error) {
	project, err := s.listedProject(ctx, req)
	if err != nil {
		return nil, err
	}
	all, err := s.allSchedules(ctx, project)
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
	s.cancelStartingRunsLocked(req.Name, false, fmt.Sprintf("schedule %s was deleted while starting", req.Name))
	return &pb.DeleteScheduleResponse{}, nil
}

// RunSchedule starts a run now, outside the schedule; its slot is unset.
func (s *schedules) RunSchedule(ctx context.Context, req *pb.RunScheduleRequest) (*pb.ScheduledRun, error) {
	s.scheduling.Lock()
	if s.shuttingDown {
		s.scheduling.Unlock()
		return nil, &api.Error{Code: api.CodeControllerUnreachable, Message: "the controller is shutting down", Retryable: true}
	}
	sc, err := s.cfg.Store.Schedule(ctx, req.Name)
	if err != nil {
		s.scheduling.Unlock()
		return nil, err
	}
	toEnd, err := s.settleRunsLocked(ctx)
	if err != nil {
		s.scheduling.Unlock()
		s.endScheduledSessions(ctx, toEnd)
		return nil, err
	}
	if prev, busy, err := s.cfg.Store.LiveRun(ctx, sc.Name); err != nil {
		s.scheduling.Unlock()
		s.endScheduledSessions(ctx, toEnd)
		return nil, err
	} else if busy {
		s.scheduling.Unlock()
		s.endScheduledSessions(ctx, toEnd)
		return nil, api.Errorf(api.CodeConflict, "a run of %s is in progress, in session %s", sc.Name, prev.SessionId).
			WithSuggestion(fmt.Sprintf("hotbed poll -s %s until it ends, or hotbed session end -s %s to stop it", prev.SessionId, prev.SessionId))
	}
	_, loc, err := parseCron(sc.Cron)
	if err != nil {
		s.scheduling.Unlock()
		s.endScheduledSessions(ctx, toEnd)
		return nil, err
	}
	pending, run, err := s.beginStartLocked(ctx, sc, nil, loc, false)
	s.scheduling.Unlock()

	s.endScheduledSessions(ctx, toEnd)
	if err != nil || pending == nil {
		return run, err
	}
	return s.finishStart(*pending)
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
	if sc.Paused {
		s.cancelStartingRunsLocked(sc.Name, true, fmt.Sprintf("schedule %s was paused while starting", sc.Name))
	}
	return s.withRuns(ctx, sc, runsListed)
}
