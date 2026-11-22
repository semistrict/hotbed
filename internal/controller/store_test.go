package controller

import (
	"path/filepath"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// A repo stored before its freshen commands became steps still loads.
func TestStoredRecordsOutliveDroppedFields(t *testing.T) {
	r, err := unmarshal(`{"id":"hexyl","url":"https://github.com/sharkdp/hexyl","freshen":["cargo build"]}`, &pb.Repo{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Id != "hexyl" || r.Url != "https://github.com/sharkdp/hexyl" || len(r.Steps) != 0 {
		t.Errorf("repo = %v", r)
	}
}

// A schedule keeps its last keepRuns runs, however old; a removed
// schedule's ended runs go, and a run in progress stays until it ends.
func TestPruneKeepsEachSchedulesLastRuns(t *testing.T) {
	ctx := t.Context()
	s, err := OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now()
	ended := now.Add(-365 * 24 * time.Hour).UnixMilli()
	for _, name := range []string{"nightly", "gone"} {
		if err := s.PutSchedule(ctx, &pb.ScheduledCommand{Name: name, Cron: "0 2 * * *"}); err != nil {
			t.Fatal(err)
		}
	}
	for range keepRuns + 5 {
		if err := s.AddRun(ctx, &pb.ScheduledRun{Schedule: "nightly", EndedAt: &ended}); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []*pb.ScheduledRun{{Schedule: "gone", EndedAt: &ended}, {Schedule: "gone", SessionId: "gone-20261122-0200"}} {
		if err := s.AddRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteSchedule(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx, "nightly", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != keepRuns || runs[0].Id != keepRuns+5 || runs[keepRuns-1].Id != 6 {
		t.Errorf("nightly kept %d runs, %d to %d; want %d, %d to 6", len(runs), runs[0].Id, runs[len(runs)-1].Id, keepRuns, keepRuns+5)
	}
	gone, err := s.Runs(ctx, "gone", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0].SessionId != "gone-20261122-0200" || gone[0].EndedAt != nil {
		t.Errorf("a removed schedule's runs after prune = %v, want only its live one", gone)
	}
}

// Samples, history, events and commands past their retention are pruned.
func TestPruneDropsWhatOutlivedItsRetention(t *testing.T) {
	ctx := t.Context()
	s, err := OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now()
	old, days, recent := now.Add(-keepHistory-time.Hour).UnixMilli(), now.Add(-48*time.Hour).UnixMilli(), now.Add(-time.Minute).UnixMilli()
	for _, q := range []string{
		`INSERT INTO samples (node_id, t, cpu, memory, psi, running, queued, parked, oom) VALUES ('n', ?, 0, 0, 0, 0, 0, 0, 0), ('n', ?, 0, 0, 0, 0, 0, 0, 0), ('n', ?, 0, 0, 0, 0, 0, 0, 0)`,
		`INSERT INTO history (id, node_id, repo_id, ended_at, doc) VALUES ('old', 'n', 'r', ?, '{}'), ('days', 'n', 'r', ?, '{}'), ('new', 'n', 'r', ?, '{}')`,
		`INSERT INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES ('old', 's', 'n', 'w', ?), ('days', 's', 'n', 'w', ?), ('new', 's', 'n', 'w', ?)`,
	} {
		if _, err := s.db.ExecContext(ctx, q, old, days, recent); err != nil {
			t.Fatal(err)
		}
	}
	for range keepEvents + 3 {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO events (at, kind, node_id, text, ref_type, ref_id) VALUES (?, 'k', 'n', 't', '', '')`, recent); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	// Samples go after a day; history and commands after a month.
	for table, want := range map[string]int{"samples": 1, "history": 2, "commands": 2, "events": keepEvents} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s after prune: %d rows, want %d", table, n, want)
		}
	}
}
