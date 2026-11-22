package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// PutSchedule adds or replaces a scheduled command. Its next_at goes in a
// column of its own, which the scheduler moves on; its runs are kept apart.
func (s *Store) PutSchedule(ctx context.Context, sc *pb.ScheduledCommand) error {
	doc := proto.Clone(sc).(*pb.ScheduledCommand)
	doc.NextAt, doc.Runs = nil, nil
	text, err := marshal(doc)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO schedules (name, doc, next_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET doc = excluded.doc, next_at = excluded.next_at`,
		sc.Name, text, nullable(sc.NextAt))
	return err
}

// SetNextAt records when a schedule's next run is due.
func (s *Store) SetNextAt(ctx context.Context, name string, at *int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET next_at = ? WHERE name = ?`, nullable(at), name)
	return err
}

// DeleteSchedule removes a scheduled command and its ended runs; a run in
// progress is kept until it ends, so it is settled like any other.
func (s *Store) DeleteSchedule(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM schedule_runs WHERE schedule = ? AND ended_at IS NOT NULL`, name)
	return true, err
}

// Schedule is the scheduled command called name, without its runs.
func (s *Store) Schedule(ctx context.Context, name string) (*pb.ScheduledCommand, error) {
	sc, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT doc, next_at FROM schedules WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no scheduled command %s", name).WithSuggestion("hotbed schedule ls")
	}
	return sc, err
}

// Schedules lists every scheduled command by name, without their runs.
func (s *Store) Schedules(ctx context.Context) ([]*pb.ScheduledCommand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc, next_at FROM schedules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.ScheduledCommand
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func scanSchedule(row interface{ Scan(...any) error }) (*pb.ScheduledCommand, error) {
	var doc string
	var next sql.NullInt64
	if err := row.Scan(&doc, &next); err != nil {
		return nil, err
	}
	sc, err := unmarshal(doc, &pb.ScheduledCommand{})
	if err != nil {
		return nil, fmt.Errorf("read scheduled command: %w", err)
	}
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO schedule_runs (schedule, session_id, command_id, ended_at, doc) VALUES (?, ?, ?, ?, ?)`,
		run.Schedule, run.SessionId, run.CommandId, nullable(run.EndedAt), doc)
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
	res, err := s.db.ExecContext(ctx, `
		UPDATE schedule_runs SET session_id = ?, command_id = ?, ended_at = ?, doc = ?
		WHERE id = ? AND ended_at IS NULL`,
		run.SessionId, run.CommandId, nullable(run.EndedAt), doc, run.Id)
	if err != nil {
		return false, fmt.Errorf("record run %d of %s: %w", run.Id, run.Schedule, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// runDoc is a run as stored: its ID is the row's.
func runDoc(run *pb.ScheduledRun) (string, error) {
	doc := proto.Clone(run).(*pb.ScheduledRun)
	doc.Id = 0
	return marshal(doc)
}

// SessionRuns lists the runs in progress whose session is id.
func (s *Store) SessionRuns(ctx context.Context, id string) ([]*pb.ScheduledRun, error) {
	return s.runs(ctx, `SELECT id, doc FROM schedule_runs WHERE session_id = ? AND ended_at IS NULL`, id)
}

// Runs lists a schedule's last n runs, the newest first.
func (s *Store) Runs(ctx context.Context, schedule string, n int) ([]*pb.ScheduledRun, error) {
	return s.runs(ctx, `SELECT id, doc FROM schedule_runs WHERE schedule = ? ORDER BY id DESC LIMIT ?`, schedule, n)
}

// LiveRuns lists the runs in progress, of every schedule.
func (s *Store) LiveRuns(ctx context.Context) ([]*pb.ScheduledRun, error) {
	return s.runs(ctx, `SELECT id, doc FROM schedule_runs WHERE ended_at IS NULL ORDER BY id`)
}

// LiveRun is a schedule's run in progress; ok is false when it has none.
func (s *Store) LiveRun(ctx context.Context, schedule string) (run *pb.ScheduledRun, ok bool, err error) {
	runs, err := s.runs(ctx, `SELECT id, doc FROM schedule_runs WHERE schedule = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`, schedule)
	if err != nil || len(runs) == 0 {
		return nil, false, err
	}
	return runs[0], true, nil
}

func (s *Store) runs(ctx context.Context, query string, args ...any) ([]*pb.ScheduledRun, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.ScheduledRun
	for rows.Next() {
		var id int64
		var doc string
		if err := rows.Scan(&id, &doc); err != nil {
			return nil, err
		}
		run, err := unmarshal(doc, &pb.ScheduledRun{})
		if err != nil {
			return nil, fmt.Errorf("read scheduled run %d: %w", id, err)
		}
		run.Id = id
		out = append(out, run)
	}
	return out, rows.Err()
}

// nullable is an optional int as SQL has it.
func nullable(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}
