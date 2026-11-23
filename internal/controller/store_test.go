package controller

import (
	"bytes"
	"compress/gzip"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/profiler/profilertest"
)

// A repo stored before its freshen commands became steps, or before its
// steps and hosts moved to its project, still loads.
func TestStoredRecordsOutliveDroppedFields(t *testing.T) {
	r, err := unmarshal(`{"id":"hexyl","url":"https://github.com/sharkdp/hexyl","freshen":["cargo build"],"hosts":["ubu1"],"steps":[{"name":"build","script":"cargo build"}],"scratchEpoch":"2"}`, &pb.Repo{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Id != "hexyl" || r.Url != "https://github.com/sharkdp/hexyl" {
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

// OpenStore enforces foreign keys and NOT NULL constraints and reopens idempotently.
func TestOpenStoreEnforcesForeignKeysAndReopens(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, agent, project_id, started_at, last_active_at) VALUES ('bad', 'a', 999999, 1, 1)`); err == nil {
		t.Errorf("inserting session with invalid project_id succeeded; expected foreign key violation")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schedules (name, project_id, doc) VALUES ('bad', 999999, '{}')`); err == nil {
		t.Errorf("inserting schedule with invalid project_id succeeded; expected foreign key violation")
	}
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

// A finished command's profile from before profiles listed their frames
// once (folded strings for stacks, frames spelled out in functions and
// buckets) reads in today's shape, whether it was stored by an older build
// or comes now in an old worker's event; one naming frames it lacks is
// dropped, and its event still applies.
func TestOldProfilesReadInTodaysShape(t *testing.T) {
	ctx := t.Context()
	s, err := OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(profilertest.LegacyProfile())
	w.Close()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO command_profiles (id, node_id, ended_at, doc) VALUES ('cmd0000001', 'n1', 1, ?)`, gz.Bytes()); err != nil {
		t.Fatal(err)
	}
	inEvent := &pb.CommandProfile{}
	if err := proto.Unmarshal(profilertest.LegacyProfile(), inEvent); err != nil {
		t.Fatal(err)
	}
	inEvent.CommandId = "cmd0000002"
	if err := addCommandProfile(ctx, s.q, "n1", 2, inEvent); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cmd0000001", "cmd0000002"} {
		p, ok, err := s.CommandProfile(ctx, id)
		if err != nil || !ok {
			t.Fatalf("CommandProfile(%s) = %v, %v", id, ok, err)
		}
		if len(p.Stacks) != 2 || flame.Fold(p.Frames, p.Stacks[0]) != "main.main;main.work;vfs_write_[k]" || p.Stacks[0].Samples != 3 {
			t.Fatalf("%s: stacks = %v of frames %v", id, p.Stacks, p.Frames)
		}
		if f := p.Frames[p.TopFunctions[0].Frame]; f.Name != "vfs_write" || f.Module != "vmlinux" || p.Buckets[0].TopFrame != p.TopFunctions[0].Frame {
			t.Fatalf("%s: function frame = %v, bucket = %v", id, f, p.Buckets[0])
		}
	}

	bad := &pb.CommandProfile{CommandId: "cmd0000003", Stacks: []*pb.FoldedStack{{Frames: []uint32{4}, Samples: 1}}}
	if err := addCommandProfile(ctx, s.q, "n1", 3, bad); err != nil {
		t.Fatalf("addCommandProfile of a profile naming frames it lacks = %v, want it dropped", err)
	}
	if _, ok, err := s.CommandProfile(ctx, "cmd0000003"); err != nil || ok {
		t.Fatalf("CommandProfile of a dropped profile = %v, %v; want none", ok, err)
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
