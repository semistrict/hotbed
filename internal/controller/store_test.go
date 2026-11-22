package controller

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

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
	if _, err := s.AddProject(ctx, "test", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	ended := now.Add(-365 * 24 * time.Hour).UnixMilli()
	for _, name := range []string{"nightly", "gone"} {
		if err := s.PutSchedule(ctx, &pb.ScheduledCommand{Name: name, Project: "test", Cron: "0 2 * * *"}); err != nil {
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

// A pre-goose database at PRAGMA user_version = 5 is adopted automatically at
// startup: migration 6 backfills repos, orphan sessions, and schedules into
// projects, and migration 7 enforces NOT NULL foreign keys.
func TestMigrateAdoptsLegacyUserVersionDatabase(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Build a legacy v5 database using only the pre-goose schema.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	legacyDDL := `
	CREATE TABLE nodes (id TEXT PRIMARY KEY, hostname TEXT NOT NULL, login TEXT NOT NULL, addr TEXT NOT NULL DEFAULT '', joined_at INTEGER NOT NULL, last_seen INTEGER, acked INTEGER NOT NULL DEFAULT 0, epoch TEXT NOT NULL DEFAULT '', applied INTEGER NOT NULL DEFAULT 0, applied_at INTEGER NOT NULL DEFAULT 0, heartbeat TEXT) STRICT;
	CREATE TABLE node_state (node_id TEXT NOT NULL, kind TEXT NOT NULL, id TEXT NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (node_id, kind, id)) STRICT;
	CREATE TABLE repos (id TEXT PRIMARY KEY, doc TEXT NOT NULL) STRICT;
	CREATE TABLE projects (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL) STRICT;
	CREATE TABLE project_repos (project_id INTEGER NOT NULL REFERENCES projects (id), remote TEXT NOT NULL, PRIMARY KEY (project_id, remote)) STRICT;
	CREATE TABLE sessions (id TEXT PRIMARY KEY, agent TEXT NOT NULL, started_at INTEGER NOT NULL, last_active_at INTEGER NOT NULL, ended_at INTEGER, end_reason TEXT NOT NULL DEFAULT '', project_id INTEGER REFERENCES projects (id)) STRICT;
	CREATE TABLE workspaces (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, node_id TEXT NOT NULL, repo_id TEXT NOT NULL, agent TEXT NOT NULL, created_at INTEGER NOT NULL, fork_millis INTEGER, released_at INTEGER) STRICT;
	CREATE TABLE commands (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, node_id TEXT NOT NULL, workspace_id TEXT NOT NULL, created_at INTEGER NOT NULL) STRICT;
	CREATE TABLE session_events (seq INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE history (id TEXT PRIMARY KEY, node_id TEXT NOT NULL, repo_id TEXT NOT NULL, ended_at INTEGER NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE samples (node_id TEXT NOT NULL, t INTEGER NOT NULL, cpu REAL NOT NULL, memory REAL NOT NULL, psi REAL NOT NULL, running INTEGER NOT NULL, queued INTEGER NOT NULL, parked INTEGER NOT NULL, oom INTEGER NOT NULL, PRIMARY KEY (node_id, t)) STRICT;
	CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, kind TEXT NOT NULL, node_id TEXT NOT NULL, text TEXT NOT NULL, ref_type TEXT NOT NULL, ref_id TEXT NOT NULL) STRICT;
	CREATE TABLE schedules (name TEXT PRIMARY KEY, doc TEXT NOT NULL, next_at INTEGER) STRICT;
	CREATE TABLE schedule_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, schedule TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '', command_id TEXT NOT NULL DEFAULT '', ended_at INTEGER, doc TEXT NOT NULL) STRICT;
	CREATE TABLE questions (id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER NOT NULL REFERENCES projects (id), repo TEXT NOT NULL, state TEXT NOT NULL, rank INTEGER NOT NULL, asked_by TEXT NOT NULL, asked_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE question_changes (seq INTEGER PRIMARY KEY AUTOINCREMENT, question_id INTEGER NOT NULL REFERENCES questions (id), what TEXT NOT NULL, at INTEGER NOT NULL, session TEXT NOT NULL, host TEXT NOT NULL, reason TEXT NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE reviews (id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER NOT NULL REFERENCES projects (id), state TEXT NOT NULL, requested_by TEXT NOT NULL, requested_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE review_findings (id INTEGER PRIMARY KEY AUTOINCREMENT, review_id INTEGER NOT NULL REFERENCES reviews (id), doc TEXT NOT NULL) STRICT;
	CREATE TABLE review_events (seq INTEGER PRIMARY KEY AUTOINCREMENT, review_id INTEGER NOT NULL REFERENCES reviews (id), finding_id INTEGER NOT NULL, what TEXT NOT NULL, at INTEGER NOT NULL, session TEXT NOT NULL, host TEXT NOT NULL, quick TEXT NOT NULL, text TEXT NOT NULL, doc TEXT NOT NULL) STRICT;
	PRAGMA user_version = 5;
	`
	if _, err := raw.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatal(err)
	}
	now := int64(1795320000000)
	// Seed pre-project state:
	// - repo "cladefs" with no project yet
	// - orphan session "accurate-gazelle" holding a workspace of cladefs
	// - orphan empty session "lonely-owl" with no workspaces at all
	// - legacy schedule "nightly" with no project in its JSON doc
	// - history row for accurate-gazelle
	for _, stmt := range []string{
		`INSERT INTO repos (id, doc) VALUES ('cladefs', '{"id":"cladefs","url":"https://github.com/semistrict/cladefs.git"}')`,
		`INSERT INTO sessions (id, agent, started_at, last_active_at) VALUES ('accurate-gazelle', 'laptop', 1795320000000, 1795320000000)`,
		`INSERT INTO sessions (id, agent, started_at, last_active_at) VALUES ('lonely-owl', 'laptop', 1795320000000, 1795320000000)`,
		`INSERT INTO workspaces (id, session_id, node_id, repo_id, agent, created_at) VALUES ('ws-1', 'accurate-gazelle', 'n1', 'cladefs', 'laptop', 1795320000000)`,
		`INSERT INTO schedules (name, doc) VALUES ('nightly', '{"name":"nightly","cron":"0 2 * * *","repo":"cladefs"}')`,
		`INSERT INTO history (id, node_id, repo_id, ended_at, doc) VALUES ('cmd-1', 'n1', 'cladefs', 1795320000000, '{"id":"cmd-1","repoId":"cladefs","sessionId":"accurate-gazelle","endedAt":"1795320000000"}')`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = now
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// OpenStore adopts v5 and runs migrations 6 and 7 automatically.
	s, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatalf("OpenStore on legacy v5 DB failed: %v", err)
	}
	defer s.Close()

	// Verify repo project was created.
	p, err := s.Project(ctx, "cladefs")
	if err != nil {
		t.Fatalf("expected auto-created project cladefs: %v", err)
	}
	if !slices.Equal(p.Repos, []string{"github.com/semistrict/cladefs"}) {
		t.Errorf("cladefs repos = %v", p.Repos)
	}

	// Verify orphan sessions were attached.
	gazelle, err := s.Session(ctx, "accurate-gazelle")
	if err != nil {
		t.Fatal(err)
	}
	if gazelle.Project != "cladefs" {
		t.Errorf("accurate-gazelle project = %q, want cladefs", gazelle.Project)
	}
	owl, err := s.Session(ctx, "lonely-owl")
	if err != nil {
		t.Fatal(err)
	}
	if owl.Project != "default" {
		t.Errorf("lonely-owl project = %q, want default", owl.Project)
	}

	// Verify legacy schedule was attached to cladefs.
	sc, err := s.Schedule(ctx, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Project != "cladefs" {
		t.Errorf("schedule nightly project = %q, want cladefs", sc.Project)
	}

	// Verify history entry carries its session's project.
	h, ok, err := s.Finished(ctx, "cmd-1")
	if err != nil || !ok {
		t.Fatalf("Finished(cmd-1) = %v, %v, %v", h, ok, err)
	}
	if h.Project != "cladefs" {
		t.Errorf("history cmd-1 project = %q, want cladefs", h.Project)
	}

	// Verify foreign keys and NOT NULL are enforced by SQLite now.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, agent, project_id, started_at, last_active_at) VALUES ('bad', 'a', 999999, 1, 1)`); err == nil {
		t.Errorf("inserting session with invalid project_id succeeded; expected foreign key violation")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schedules (name, project_id, doc) VALUES ('bad', 999999, '{}')`); err == nil {
		t.Errorf("inserting schedule with invalid project_id succeeded; expected foreign key violation")
	}

	// Reopening is idempotent.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatalf("reopening migrated DB failed: %v", err)
	}
	s2.Close()
}

// OpenStore refuses a database whose schema version is newer than this binary.
func TestOpenStoreRefusesNewerSchemaVersion(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "future.db")
	s, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (999, 1)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := OpenStore(ctx, path); err == nil || !strings.Contains(err.Error(), "newer than this hotbed") {
		t.Errorf("OpenStore on future version = %v, want newer-than-hotbed error", err)
	}
}

// sqlc emits ?N for sqlc.arg(...) and bare ? for positional params. In SQLite,
// mixing ?N and bare ? in the same statement causes bare ? to advance past the
// highest ?N (e.g. ?2 followed by ? asks for arg #3), and combining ?N with
// /*SLICE:...*/ shifts positions whenever the slice has >1 element. Guard every
// generated query in both controller and worker against both bug classes.
func TestSQLcGeneratedQueriesHaveSafeSQLitePlaceholders(t *testing.T) {
	numbered := regexp.MustCompile(`\?[0-9]+`)
	bare := regexp.MustCompile(`\?(?:[^0-9]|$)`)
	dirs := []string{
		filepath.Join("db", "gen"),
		filepath.Join("..", "worker", "db", "gen"),
	}
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.sql.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no generated *.sql.go files in %s", dir)
		}
		for _, file := range files {
			f, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Values) != 1 {
						continue
					}
					lit, ok := vs.Values[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					sqlText, err := strconv.Unquote(lit.Value)
					if err != nil || !strings.Contains(sqlText, "-- name:") {
						continue
					}
					checked++
					hasNum := numbered.MatchString(sqlText)
					hasSlice := strings.Contains(sqlText, "/*SLICE:")
					// Strip /*SLICE:name*/? before testing for bare ?
					stripped := regexp.MustCompile(`/\*SLICE:[^*]+\*/\?`).ReplaceAllString(sqlText, "")
					hasBare := bare.MatchString(stripped)
					if hasNum && hasBare {
						t.Errorf("%s (%s): mixes numbered ?N and bare ? placeholders:\n%s", file, vs.Names[0].Name, sqlText)
					}
					if hasNum && hasSlice {
						t.Errorf("%s (%s): mixes numbered ?N with sqlc.slice:\n%s", file, vs.Names[0].Name, sqlText)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("checked 0 sqlc queries")
	}
}
