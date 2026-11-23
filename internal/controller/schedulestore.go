package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// PutSchedule adds or replaces a scheduled command in its project. Its
// project_id and next_at go in columns of their own; its runs are kept apart.
func (s *Store) PutSchedule(ctx context.Context, sc *pb.ScheduledCommand) error {
	pid, err := projectID(ctx, s.q, sc.Project)
	if err != nil {
		return err
	}
	doc := proto.Clone(sc).(*pb.ScheduledCommand)
	doc.Project, doc.NextAt, doc.Runs, doc.Reminder = "", nil, nil, nil
	text, err := marshal(doc)
	if err != nil {
		return err
	}
	return s.q.UpsertSchedule(ctx, controllerdb.UpsertScheduleParams{
		Name: sc.Name, ProjectID: pid, Doc: text, NextAt: nullable(sc.NextAt),
	})
}

// SetNextAt records when a schedule's next run is due.
func (s *Store) SetNextAt(ctx context.Context, name string, at *int64) error {
	return s.q.UpdateScheduleNextAt(ctx, controllerdb.UpdateScheduleNextAtParams{
		NextAt: nullable(at), Name: name,
	})
}

// DeleteSchedule removes a scheduled command and its ended runs; a run in
// progress is kept until it ends, so it is settled like any other.
func (s *Store) DeleteSchedule(ctx context.Context, name string) (bool, error) {
	n, err := s.q.DeleteSchedule(ctx, name)
	if err != nil || n == 0 {
		return false, err
	}
	return true, s.q.DeleteEndedRunsForSchedule(ctx, name)
}

// Schedule is the scheduled command called name, without its runs.
func (s *Store) Schedule(ctx context.Context, name string) (*pb.ScheduledCommand, error) {
	row, err := s.q.GetSchedule(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no scheduled command %s", name).WithSuggestion("hotbed schedule ls")
	}
	if err != nil {
		return nil, err
	}
	return decodeSchedule(row.Project, row.Doc, row.NextAt)
}

// Schedules lists the scheduled commands of project (empty: every project's)
// by name, without their runs.
func (s *Store) Schedules(ctx context.Context, project string) ([]*pb.ScheduledCommand, error) {
	rows, err := s.q.ListSchedules(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ScheduledCommand, 0, len(rows))
	for _, r := range rows {
		sc, err := decodeSchedule(r.Project, r.Doc, r.NextAt)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

func decodeSchedule(project, doc string, next sql.NullInt64) (*pb.ScheduledCommand, error) {
	sc, err := unmarshal(doc, &pb.ScheduledCommand{})
	if err != nil {
		return nil, fmt.Errorf("read scheduled command: %w", err)
	}
	sc.Project = project
	if next.Valid {
		sc.NextAt = &next.Int64
	}
	return sc, nil
}

// AddRun records a run, in one statement, and gives it its ID.
func (s *Store) AddRun(ctx context.Context, run *pb.ScheduledRun) error {
	doc, err := runDoc(run)
	if err != nil {
		return err
	}
	res, err := s.q.InsertScheduleRun(ctx, controllerdb.InsertScheduleRunParams{
		Schedule: run.Schedule, SessionID: run.SessionId, CommandID: run.CommandId,
		EndedAt: nullable(run.EndedAt), Doc: doc,
	})
	if err != nil {
		return fmt.Errorf("record run of %s: %w", run.Schedule, err)
	}
	run.Id, err = res.LastInsertId()
	return err
}

// UpdateRun stores what a run has become, unless it has ended already: ok
// is false then, and nothing changed, so two that settle a run at once
// cannot both act on its end.
func (s *Store) UpdateRun(ctx context.Context, run *pb.ScheduledRun) (ok bool, err error) {
	doc, err := runDoc(run)
	if err != nil {
		return false, err
	}
	n, err := s.q.UpdateLiveScheduleRun(ctx, controllerdb.UpdateLiveScheduleRunParams{
		SessionID: run.SessionId, CommandID: run.CommandId, EndedAt: nullable(run.EndedAt),
		Doc: doc, ID: run.Id,
	})
	if err != nil {
		return false, fmt.Errorf("record run %d of %s: %w", run.Id, run.Schedule, err)
	}
	return n > 0, nil
}

// runDoc is a run as stored: its ID is the row's.
func runDoc(run *pb.ScheduledRun) (string, error) {
	doc := proto.Clone(run).(*pb.ScheduledRun)
	doc.Id = 0
	return marshal(doc)
}

// SessionRuns lists the runs in progress whose session is id.
func (s *Store) SessionRuns(ctx context.Context, id string) ([]*pb.ScheduledRun, error) {
	rows, err := s.q.ListLiveScheduleRunsBySession(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ScheduledRun, 0, len(rows))
	for _, r := range rows {
		run, err := decodeRun(r.ID, r.Doc)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// Runs lists a schedule's last n runs, the newest first.
func (s *Store) Runs(ctx context.Context, schedule string, n int) ([]*pb.ScheduledRun, error) {
	rows, err := s.q.ListScheduleRuns(ctx, controllerdb.ListScheduleRunsParams{Schedule: schedule, Limit: int64(n)})
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ScheduledRun, 0, len(rows))
	for _, r := range rows {
		run, err := decodeRun(r.ID, r.Doc)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// RecentRuns lists up to n newest runs for every schedule of project (empty:
// every project's), grouped by schedule name, newest first, in one SQL query.
func (s *Store) RecentRuns(ctx context.Context, project string, n int) (map[string][]*pb.ScheduledRun, error) {
	rows, err := s.q.ListRecentScheduleRuns(ctx, controllerdb.ListRecentScheduleRunsParams{
		Project:     project,
		PerSchedule: int64(n),
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string][]*pb.ScheduledRun)
	for _, r := range rows {
		run, err := decodeRun(r.ID, r.Doc)
		if err != nil {
			return nil, err
		}
		out[r.Schedule] = append(out[r.Schedule], run)
	}
	return out, nil
}

// LiveRuns lists the runs in progress, of every schedule.
func (s *Store) LiveRuns(ctx context.Context) ([]*pb.ScheduledRun, error) {
	rows, err := s.q.ListAllLiveScheduleRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ScheduledRun, 0, len(rows))
	for _, r := range rows {
		run, err := decodeRun(r.ID, r.Doc)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// Run returns a scheduled run by ID.
func (s *Store) Run(ctx context.Context, id int64) (*pb.ScheduledRun, error) {
	r, err := s.q.GetScheduleRun(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no scheduled run %d", id)
	}
	if err != nil {
		return nil, err
	}
	return decodeRun(r.ID, r.Doc)
}

// LiveRun is a schedule's run in progress; ok is false when it has none.
func (s *Store) LiveRun(ctx context.Context, schedule string) (run *pb.ScheduledRun, ok bool, err error) {
	r, err := s.q.GetLiveScheduleRun(ctx, schedule)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	run, err = decodeRun(r.ID, r.Doc)
	return run, err == nil, err
}

func decodeRun(id int64, doc string) (*pb.ScheduledRun, error) {
	run, err := unmarshal(doc, &pb.ScheduledRun{})
	if err != nil {
		return nil, fmt.Errorf("read scheduled run %d: %w", id, err)
	}
	run.Id = id
	return run, nil
}

// nullable is an optional int as SQL has it.
func nullable(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}
