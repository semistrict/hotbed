package main

import (
	"context"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

type scheduleCmd struct {
	Add    scheduleAddCmd    `cmd:"" help:"Schedule a command, or replace the scheduled command of that name. Each run gets a session of its own and a fresh workspace of the repo at its branch's head."`
	Ls     scheduleLsCmd     `cmd:"" help:"Scheduled commands, when each runs next, and their recent runs."`
	Show   scheduleShowCmd   `cmd:"" help:"A scheduled command and its runs, each with its session (hotbed logs -s SESSION COMMAND)."`
	Rm     scheduleRmCmd     `cmd:"" help:"Remove a scheduled command; a run in progress goes on."`
	Run    scheduleRunCmd    `cmd:"" help:"Start a run now, outside the schedule, unless one is in progress; prints the run and its session."`
	Pause  schedulePauseCmd  `cmd:"" help:"Start no runs until resumed."`
	Resume scheduleResumeCmd `cmd:"" help:"Run again, from the next slot; the slots paused through are not made up."`
}

type scheduleAddCmd struct {
	controllerFlag
	repoFlag
	Name   string   `arg:"" help:"Lowercase words joined by dashes, such as cladefs-nightly; each run's session is NAME-YYYYMMDD-HHMM."`
	Cron   string   `required:"" help:"When it runs: five-field cron (minute hour day-of-month month day-of-week) in the controller's time zone, such as '0 2 * * *'; CRON_TZ=ZONE in front for another zone. A slot that passes while the controller is down is missed, not run late, and a slot that comes while the previous run is in progress is skipped."`
	Node   string   `help:"Run on this node (host name), such as the one earlier measurements came from."`
	Paused bool     `help:"Add it paused."`
	Args   []string `arg:"" optional:"" passthrough:"" help:"The command, after --; it runs at /work/REPO, as with hotbed run."`
	commandFlags
}

func (c *scheduleAddCmd) SetDashArgs(args []string) { c.Args = args }

func (c *scheduleAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	req, err := c.request(c.Args, "hotbed schedule add NAME -r REPO --cron '0 2 * * *' -- scripts/nightly.sh")
	if err != nil {
		return nil, err
	}
	repo, err := c.resolveRepo(ctx, d, "", c.Repo, "")
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.schedules.PutSchedule(ctx, &pb.PutScheduleRequest{Schedule: &pb.ScheduledCommand{
		Name: c.Name, Cron: c.Cron, Repo: repo, Node: c.Node, Paused: c.Paused, Command: req,
	}})
}

type scheduleLsCmd struct {
	controllerFlag
}

func (c *scheduleLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.schedules.ListSchedules(ctx, &pb.ListSchedulesRequest{})
}

// scheduleNamed is the argument of the commands that act on one schedule.
type scheduleNamed struct {
	controllerFlag
	Name string `arg:"" help:"The scheduled command's name."`
}

type scheduleShowCmd struct{ scheduleNamed }

func (c *scheduleShowCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.schedules.GetSchedule(ctx, &pb.GetScheduleRequest{Name: c.Name})
}

type scheduleRmCmd struct{ scheduleNamed }

func (c *scheduleRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.schedules.DeleteSchedule(ctx, &pb.DeleteScheduleRequest{Name: c.Name})
}

type scheduleRunCmd struct{ scheduleNamed }

func (c *scheduleRunCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.schedules.RunSchedule(ctx, &pb.RunScheduleRequest{Name: c.Name})
}

type schedulePauseCmd struct{ scheduleNamed }

func (c *schedulePauseCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.schedules.PauseSchedule(ctx, &pb.PauseScheduleRequest{Name: c.Name, Paused: true})
}

type scheduleResumeCmd struct{ scheduleNamed }

func (c *scheduleResumeCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.schedules.PauseSchedule(ctx, &pb.PauseScheduleRequest{Name: c.Name, Paused: false})
}
