package migrations

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
)

func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// historicalV11Schema is the exact schema of a live controller database at
// migration 11, built independently of 00011_baseline.sql from historical
// migrations 1..10 (before the squash). In particular, nodes.acked remains in
// its true historical column position between last_seen and epoch (from
// 00001_initial.sql + 00003_event_stream.sql dropping report and adding
// epoch/applied/applied_at/heartbeat).
const historicalV11Schema = `
CREATE TABLE nodes (
	id         TEXT PRIMARY KEY,
	hostname   TEXT NOT NULL,
	login      TEXT NOT NULL,
	addr       TEXT NOT NULL DEFAULT '',
	joined_at  INTEGER NOT NULL,
	last_seen  INTEGER,
	acked      INTEGER NOT NULL DEFAULT 0,
	epoch      TEXT NOT NULL DEFAULT '',
	applied    INTEGER NOT NULL DEFAULT 0,
	applied_at INTEGER NOT NULL DEFAULT 0,
	heartbeat  TEXT
) STRICT;

CREATE TABLE node_state (
	node_id TEXT NOT NULL,
	kind    TEXT NOT NULL,
	id      TEXT NOT NULL,
	doc     TEXT NOT NULL,
	PRIMARY KEY (node_id, kind, id)
) STRICT;

CREATE TABLE repos (
	id  TEXT PRIMARY KEY,
	doc TEXT NOT NULL
) STRICT;

CREATE TABLE projects (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	setup      TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE TABLE project_repos (
	project_id INTEGER NOT NULL REFERENCES projects (id),
	remote     TEXT NOT NULL,
	PRIMARY KEY (project_id, remote)
) STRICT;
CREATE INDEX project_repos_remote ON project_repos (remote);

CREATE TABLE project_changes (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects (id),
	what       TEXT NOT NULL,
	repo       TEXT NOT NULL,
	at         INTEGER NOT NULL,
	session    TEXT NOT NULL,
	host       TEXT NOT NULL,
	reason     TEXT NOT NULL
) STRICT;
CREATE INDEX project_changes_project ON project_changes (project_id, seq);

CREATE TABLE sessions (
	id             TEXT PRIMARY KEY,
	agent          TEXT NOT NULL,
	project_id     INTEGER NOT NULL REFERENCES projects (id),
	started_at     INTEGER NOT NULL,
	last_active_at INTEGER NOT NULL,
	ended_at       INTEGER,
	end_reason     TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX sessions_project ON sessions (project_id, ended_at);

CREATE TABLE workspaces (
	id          TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL,
	node_id     TEXT NOT NULL,
	agent       TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	fork_millis INTEGER,
	released_at INTEGER
) STRICT;
CREATE INDEX workspaces_session ON workspaces (session_id);
CREATE INDEX workspaces_created ON workspaces (created_at);

CREATE TABLE commands (
	id           TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	node_id      TEXT NOT NULL,
	workspace_id TEXT NOT NULL,
	created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE session_events (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX session_events_session ON session_events (session_id, seq);

CREATE TABLE history (
	id         TEXT PRIMARY KEY,
	node_id    TEXT NOT NULL,
	repo_id    TEXT NOT NULL,
	ended_at   INTEGER NOT NULL,
	doc        TEXT NOT NULL,
	project_id INTEGER REFERENCES projects (id)
) STRICT;
CREATE INDEX history_ended ON history (ended_at);
CREATE INDEX history_project ON history (project_id, ended_at);

CREATE TABLE command_profiles (
	id       TEXT PRIMARY KEY,
	node_id  TEXT NOT NULL,
	ended_at INTEGER NOT NULL,
	doc      BLOB NOT NULL
) STRICT;
CREATE INDEX command_profiles_ended ON command_profiles (ended_at);

CREATE TABLE samples (
	node_id TEXT NOT NULL,
	t       INTEGER NOT NULL,
	cpu     REAL NOT NULL,
	memory  REAL NOT NULL,
	psi     REAL NOT NULL,
	running INTEGER NOT NULL,
	queued  INTEGER NOT NULL,
	parked  INTEGER NOT NULL,
	oom     INTEGER NOT NULL,
	PRIMARY KEY (node_id, t)
) STRICT;
CREATE INDEX samples_t ON samples (t);

CREATE TABLE events (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	at       INTEGER NOT NULL,
	kind     TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	text     TEXT NOT NULL,
	ref_type TEXT NOT NULL,
	ref_id   TEXT NOT NULL
) STRICT;

CREATE TABLE schedules (
	name       TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects (id),
	doc        TEXT NOT NULL,
	next_at    INTEGER
) STRICT;
CREATE INDEX schedules_project ON schedules (project_id);

CREATE TABLE schedule_runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	schedule   TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	command_id TEXT NOT NULL DEFAULT '',
	ended_at   INTEGER,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX schedule_runs_schedule ON schedule_runs (schedule, id);
CREATE INDEX schedule_runs_session ON schedule_runs (session_id);
CREATE INDEX schedule_runs_live ON schedule_runs (ended_at) WHERE ended_at IS NULL;

CREATE TABLE questions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects (id),
	repo       TEXT NOT NULL,
	state      TEXT NOT NULL,
	rank       INTEGER NOT NULL,
	asked_by   TEXT NOT NULL,
	asked_at   INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX questions_project ON questions (project_id, state);

CREATE TABLE question_changes (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	question_id INTEGER NOT NULL REFERENCES questions (id),
	what        TEXT NOT NULL,
	at          INTEGER NOT NULL,
	session     TEXT NOT NULL,
	host        TEXT NOT NULL,
	reason      TEXT NOT NULL,
	doc         TEXT NOT NULL
) STRICT;
CREATE INDEX question_changes_question ON question_changes (question_id, seq);

CREATE TABLE reviews (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id   INTEGER NOT NULL REFERENCES projects (id),
	state        TEXT NOT NULL,
	requested_by TEXT NOT NULL,
	requested_at INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	doc          TEXT NOT NULL
) STRICT;
CREATE INDEX reviews_project ON reviews (project_id, state);

CREATE TABLE review_findings (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	review_id INTEGER NOT NULL REFERENCES reviews (id),
	doc       TEXT NOT NULL
) STRICT;
CREATE INDEX review_findings_review ON review_findings (review_id, id);

CREATE TABLE review_events (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	review_id  INTEGER NOT NULL REFERENCES reviews (id),
	finding_id INTEGER NOT NULL,
	what       TEXT NOT NULL,
	at         INTEGER NOT NULL,
	session    TEXT NOT NULL,
	host       TEXT NOT NULL,
	quick      TEXT NOT NULL,
	text       TEXT NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX review_events_review ON review_events (review_id, seq);
`

// releasedSquashedV11Schema is the exact schema of a controller database
// initialized from the released 26c5bf6 squashed 00011_baseline.sql (which
// omitted nodes.acked). Kept independent of the embedded migration files so
// regressions in 00011_baseline.sql or 00018_drop_nodes_acked.go cannot hide.
var releasedSquashedV11Schema = func() string {
	const ackedLine = "\tacked      INTEGER NOT NULL DEFAULT 0,\n"
	if !strings.Contains(historicalV11Schema, ackedLine) {
		panic("historicalV11Schema missing nodes.acked line")
	}
	return strings.Replace(historicalV11Schema, ackedLine, "", 1)
}()

// historicalV12To14Schema is the exact DDL added by migrations 12..14, kept
// independent of the embedded migration files so historical fixtures cannot
// mask regressions in the active migrations.
const historicalV12To14Schema = `
CREATE TABLE git_credentials (
	name   TEXT PRIMARY KEY,
	host   TEXT NOT NULL,
	doc    TEXT NOT NULL,
	secret TEXT NOT NULL
) STRICT;
CREATE INDEX git_credentials_host ON git_credentials (host);

ALTER TABLE samples ADD COLUMN cpu_psi REAL NOT NULL DEFAULT 0;
ALTER TABLE samples ADD COLUMN holding INTEGER NOT NULL DEFAULT 0;

CREATE TABLE placements (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	at         INTEGER NOT NULL,
	session_id TEXT NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX placements_at ON placements (at);

ALTER TABLE history ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
CREATE INDEX history_session ON history (session_id, ended_at);
`

// historicalV16To17Schema is the exact DDL added by migrations 16..17.
const historicalV16To17Schema = `
ALTER TABLE nodes ADD COLUMN retired_at INTEGER;
CREATE INDEX commands_session ON commands (session_id);
CREATE INDEX commands_created ON commands (created_at);
`

func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cols
}

// A fresh controller database applies 00011_baseline.sql and 00012..00018 cleanly,
// creates all expected tables, drops nodes.acked, and reaches the latest migration version.
func TestMigrateFreshDatabase(t *testing.T) {
	ctx := t.Context()
	db := openRawSQLite(t, filepath.Join(t.TempDir(), "fresh.db"))

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on fresh DB: %v", err)
	}

	var ver int64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM `+goose.DefaultTablename+` WHERE is_applied = 1`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != 19 {
		t.Fatalf("schema version = %d, want 19", ver)
	}

	wantNodesCols := []string{
		"id", "hostname", "login", "addr", "joined_at", "last_seen",
		"epoch", "applied", "applied_at", "heartbeat", "retired_at",
	}
	if gotCols := tableColumns(t, db, "nodes"); !slices.Equal(gotCols, wantNodesCols) {
		t.Fatalf("fresh DB nodes columns = %v, want %v", gotCols, wantNodesCols)
	}

	wantTables := []string{
		"nodes", "node_state", "repos", "projects", "project_repos",
		"sessions", "workspaces", "commands", "session_events",
		"history", "command_profiles", "command_diagnostics", "samples", "events",
		"schedules", "schedule_runs", "questions", "question_changes",
		"reviews", "review_findings", "review_events",
		"git_credentials", "placements",
	}
	for _, tbl := range wantTables {
		var name string
		err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("fresh DB missing table %q: %v", tbl, err)
		}
	}

	// Generated sqlc queries work cleanly on a fresh database.
	q := controllerdb.New(db)
	if err := q.UpsertNode(ctx, controllerdb.UpsertNodeParams{
		ID: "n1", Hostname: "ubu1", Login: "ramon@", Addr: "100.64.0.1:7700", JoinedAt: 1000,
	}); err != nil {
		t.Fatalf("UpsertNode on fresh DB: %v", err)
	}
	got, err := q.GetNode(ctx, "n1")
	if err != nil {
		t.Fatalf("GetNode on fresh DB: %v", err)
	}
	if got.Hostname != "ubu1" || got.Addr != "100.64.0.1:7700" {
		t.Fatalf("GetNode on fresh DB = %+v, want hostname ubu1 addr 100.64.0.1:7700", got)
	}
	listed, err := q.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes on fresh DB: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "n1" {
		t.Fatalf("ListNodes on fresh DB = %+v, want [n1]", listed)
	}

	// Re-running Migrate is idempotent.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate on fresh DB: %v", err)
	}

	assertAutoincrementMonotonic(t, db)
}

// Pre-existing live databases at v11, v14, and v17 — both pre-squash databases
// (with legacy nodes.acked in its original column position between last_seen
// and epoch) and databases initialized from the released 26c5bf6 squashed
// baseline (without nodes.acked) — open and upgrade cleanly to v18 without
// re-running 00011_baseline.sql, match a fresh database's PRAGMA schema
// identically, preserve all live rows and stream state, keep AUTOINCREMENT
// cursors strictly monotonic, and work cleanly with generated sqlc queries
// (board 7872 / 9388 / 9586).
func TestMigratePreExistingLiveDatabases(t *testing.T) {
	ctx := t.Context()
	fresh := openRawSQLite(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err := Migrate(ctx, fresh); err != nil {
		t.Fatalf("Migrate fresh DB: %v", err)
	}
	wantSchema := inspectSchema(t, fresh)

	type layout struct {
		name     string
		v11DDL   string
		hasAcked bool
	}
	layouts := []layout{
		{name: "pre_squash", v11DDL: historicalV11Schema, hasAcked: true},
		{name: "released_26c5bf6", v11DDL: releasedSquashedV11Schema, hasAcked: false},
	}

	for _, l := range layouts {
		for _, startVer := range []int64{11, 14, 17} {
			t.Run(fmt.Sprintf("%s_from_v%d", l.name, startVer), func(t *testing.T) {
				ctx := t.Context()
				path := filepath.Join(t.TempDir(), "live.db")
				db := openRawSQLite(t, path)

				// Seed the exact independent historical schema for this layout and startVer.
				if _, err := db.ExecContext(ctx, l.v11DDL); err != nil {
					t.Fatalf("seed v11 schema (%s): %v", l.name, err)
				}
				if startVer >= 14 {
					if _, err := db.ExecContext(ctx, historicalV12To14Schema); err != nil {
						t.Fatalf("seed historical v12..v14 schema: %v", err)
					}
				}
				if startVer >= 17 {
					if _, err := db.ExecContext(ctx, historicalV16To17Schema); err != nil {
						t.Fatalf("seed historical v16..v17 schema: %v", err)
					}
				}

				// Confirm pre-migration nodes column layout matches expectations.
				var wantPreCols []string
				if l.hasAcked {
					wantPreCols = []string{
						"id", "hostname", "login", "addr", "joined_at", "last_seen",
						"acked", "epoch", "applied", "applied_at", "heartbeat",
					}
				} else {
					wantPreCols = []string{
						"id", "hostname", "login", "addr", "joined_at", "last_seen",
						"epoch", "applied", "applied_at", "heartbeat",
					}
				}
				if startVer >= 17 {
					wantPreCols = append(wantPreCols, "retired_at")
				}
				if gotCols := tableColumns(t, db, "nodes"); !slices.Equal(gotCols, wantPreCols) {
					t.Fatalf("pre-migration nodes columns = %v, want %v", gotCols, wantPreCols)
				}

				if _, err := db.ExecContext(ctx, `CREATE TABLE `+goose.DefaultTablename+` (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT (datetime('now')))`); err != nil {
					t.Fatal(err)
				}
				if l.hasAcked {
					for v := int64(0); v <= startVer; v++ {
						if _, err := db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					if _, err := db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (0, 1)`); err != nil {
						t.Fatal(err)
					}
					for v := int64(11); v <= startVer; v++ {
						if _, err := db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
							t.Fatal(err)
						}
					}
				}

				// Seed live rows across nodes (with stream state), projects, sessions,
				// workspaces, commands, history, events, and session_events.
				if l.hasAcked {
					if _, err := db.ExecContext(ctx, `
						INSERT INTO nodes (id, hostname, login, addr, joined_at, last_seen, acked, epoch, applied, applied_at, heartbeat)
						VALUES
							('n1', 'ubu1', 'ramon@', '100.64.0.1:7700', 1000, 2000, 42, 'ep-1', 17, 1900, '{"storage":"btrfs"}'),
							('n2', 'asahimbp', 'ramon@', '100.64.0.2:7700', 1100, NULL, 7, 'ep-2', 5, 1800, NULL)
					`); err != nil {
						t.Fatal(err)
					}
					if startVer >= 17 {
						if _, err := db.ExecContext(ctx, `
							INSERT INTO nodes (id, hostname, login, addr, joined_at, last_seen, acked, epoch, applied, applied_at, heartbeat, retired_at)
							VALUES ('n3', 'oldbox', 'ramon@', '100.64.0.3:7700', 900, 1200, 3, 'ep-0', 1, 1100, NULL, 1500)
						`); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					if _, err := db.ExecContext(ctx, `
						INSERT INTO nodes (id, hostname, login, addr, joined_at, last_seen, epoch, applied, applied_at, heartbeat)
						VALUES
							('n1', 'ubu1', 'ramon@', '100.64.0.1:7700', 1000, 2000, 'ep-1', 17, 1900, '{"storage":"btrfs"}'),
							('n2', 'asahimbp', 'ramon@', '100.64.0.2:7700', 1100, NULL, 'ep-2', 5, 1800, NULL)
					`); err != nil {
						t.Fatal(err)
					}
					if startVer >= 17 {
						if _, err := db.ExecContext(ctx, `
							INSERT INTO nodes (id, hostname, login, addr, joined_at, last_seen, epoch, applied, applied_at, heartbeat, retired_at)
							VALUES ('n3', 'oldbox', 'ramon@', '100.64.0.3:7700', 900, 1200, 'ep-0', 1, 1100, NULL, 1500)
						`); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO projects (id, name, created_at) VALUES (1, 'hotbed', 1000)`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO sessions (id, agent, project_id, started_at, last_active_at) VALUES ('s1', 'agent', 1, 1000, 2000)`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO workspaces (id, session_id, node_id, agent, created_at, fork_millis) VALUES ('w1', 's1', 'n1', 'agent', 1500, 42)`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES ('c1', 's1', 'n1', 'w1', 1600)`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO events (at, kind, node_id, text, ref_type, ref_id) VALUES (1700, 'node', 'n1', 'joined', 'node', 'n1')`); err != nil {
					t.Fatal(err)
				}
				if startVer < 14 {
					if _, err := db.ExecContext(ctx, `INSERT INTO history (id, node_id, repo_id, ended_at, doc, project_id) VALUES ('h1', 'n1', 'hotbed', 2000, '{"id":"h1","sessionId":"s1"}', 1)`); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := db.ExecContext(ctx, `INSERT INTO history (id, node_id, repo_id, ended_at, doc, project_id, session_id) VALUES ('h1', 'n1', 'hotbed', 2000, '{"id":"h1","sessionId":"s1"}', 1, 's1')`); err != nil {
						t.Fatal(err)
					}
				}
				if startVer < 17 {
					if _, err := db.ExecContext(ctx, `INSERT INTO session_events (session_id, doc) VALUES ('s1', '{"at":"10","what":"ready","sessionId":"s1","workspaceId":"w1"}')`); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := db.ExecContext(ctx, `INSERT INTO session_events (session_id, doc) VALUES ('s1', '{"at":"10","sessionId":"s1","workspace":{"what":"WHAT_READY","workspaceId":"w1"}}')`); err != nil {
						t.Fatal(err)
					}
				}

				// Run Migrate as startup does.
				if err := Migrate(ctx, db); err != nil {
					t.Fatalf("Migrate (%s) from v%d: %v", l.name, startVer, err)
				}

				var ver int64
				if err := db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM `+goose.DefaultTablename+` WHERE is_applied = 1`).Scan(&ver); err != nil {
					t.Fatal(err)
				}
				if ver != 19 {
					t.Fatalf("version after Migrate (%s) from v%d = %d, want 19", l.name, startVer, ver)
				}

				// Re-running Migrate is idempotent.
				if err := Migrate(ctx, db); err != nil {
					t.Fatalf("second Migrate (%s) from v%d: %v", l.name, startVer, err)
				}

				// Exact PRAGMA schema parity with fresh DB.
				gotSchema := inspectSchema(t, db)
				if diff := cmp.Diff(wantSchema, gotSchema); diff != "" {
					t.Fatalf("schema drift between fresh DB and upgraded %s v%d DB (-fresh +upgraded):\n%s", l.name, startVer, diff)
				}

				// Post-migration nodes has no acked column and has retired_at at the end.
				wantPostCols := []string{
					"id", "hostname", "login", "addr", "joined_at", "last_seen",
					"epoch", "applied", "applied_at", "heartbeat", "retired_at",
				}
				if gotCols := tableColumns(t, db, "nodes"); !slices.Equal(gotCols, wantPostCols) {
					t.Fatalf("post-migration nodes columns = %v, want %v", gotCols, wantPostCols)
				}

				// Exercise generated sqlc queries and verify all rows & stream state survived.
				q := controllerdb.New(db)

				gotN1, err := q.GetNode(ctx, "n1")
				if err != nil {
					t.Fatalf("GetNode(n1) after Migrate: %v", err)
				}
				wantN1 := controllerdb.GetNodeRow{
					ID:        "n1",
					Hostname:  "ubu1",
					Login:     "ramon@",
					Addr:      "100.64.0.1:7700",
					JoinedAt:  1000,
					LastSeen:  sql.NullInt64{Int64: 2000, Valid: true},
					Heartbeat: sql.NullString{String: `{"storage":"btrfs"}`, Valid: true},
				}
				if diff := cmp.Diff(wantN1, gotN1); diff != "" {
					t.Fatalf("GetNode(n1) mismatch (-want +got):\n%s", diff)
				}

				gotN2, err := q.GetNode(ctx, "n2")
				if err != nil {
					t.Fatalf("GetNode(n2) after Migrate: %v", err)
				}
				wantN2 := controllerdb.GetNodeRow{
					ID:       "n2",
					Hostname: "asahimbp",
					Login:    "ramon@",
					Addr:     "100.64.0.2:7700",
					JoinedAt: 1100,
				}
				if diff := cmp.Diff(wantN2, gotN2); diff != "" {
					t.Fatalf("GetNode(n2) mismatch (-want +got):\n%s", diff)
				}

				streamState, err := q.GetNodeStreamState(ctx, "n1")
				if err != nil {
					t.Fatalf("GetNodeStreamState(n1): %v", err)
				}
				wantStream := controllerdb.GetNodeStreamStateRow{
					Epoch:     "ep-1",
					Applied:   17,
					AppliedAt: 1900,
				}
				if diff := cmp.Diff(wantStream, streamState); diff != "" {
					t.Fatalf("GetNodeStreamState(n1) mismatch (-want +got):\n%s", diff)
				}

				streamStateN2, err := q.GetNodeStreamState(ctx, "n2")
				if err != nil {
					t.Fatalf("GetNodeStreamState(n2): %v", err)
				}
				wantStreamN2 := controllerdb.GetNodeStreamStateRow{
					Epoch:     "ep-2",
					Applied:   5,
					AppliedAt: 1800,
				}
				if diff := cmp.Diff(wantStreamN2, streamStateN2); diff != "" {
					t.Fatalf("GetNodeStreamState(n2) mismatch (-want +got):\n%s", diff)
				}

				if startVer >= 17 {
					var retiredAt sql.NullInt64
					if err := db.QueryRowContext(ctx, `SELECT retired_at FROM nodes WHERE id = 'n3'`).Scan(&retiredAt); err != nil {
						t.Fatalf("select retired_at for n3: %v", err)
					}
					if !retiredAt.Valid || retiredAt.Int64 != 1500 {
						t.Fatalf("n3 retired_at = %+v, want 1500", retiredAt)
					}
				}

				listed, err := q.ListNodes(ctx)
				if err != nil {
					t.Fatalf("ListNodes after Migrate: %v", err)
				}
				wantList := []controllerdb.ListNodesRow{
					{
						ID:       "n2",
						Hostname: "asahimbp",
						Login:    "ramon@",
						Addr:     "100.64.0.2:7700",
						JoinedAt: 1100,
					},
					{
						ID:        "n1",
						Hostname:  "ubu1",
						Login:     "ramon@",
						Addr:      "100.64.0.1:7700",
						JoinedAt:  1000,
						LastSeen:  sql.NullInt64{Int64: 2000, Valid: true},
						Heartbeat: sql.NullString{String: `{"storage":"btrfs"}`, Valid: true},
					},
				}
				if diff := cmp.Diff(wantList, listed); diff != "" {
					t.Fatalf("ListNodes mismatch (-want +got):\n%s", diff)
				}

				// Verify sessions, workspaces, commands, events, and session_events survived.
				var sessAgent string
				if err := db.QueryRowContext(ctx, `SELECT agent FROM sessions WHERE id = 's1'`).Scan(&sessAgent); err != nil || sessAgent != "agent" {
					t.Fatalf("sessions(s1) = %q, %v; want agent", sessAgent, err)
				}
				var wsForkMillis int64
				if err := db.QueryRowContext(ctx, `SELECT fork_millis FROM workspaces WHERE id = 'w1' AND session_id = 's1' AND node_id = 'n1'`).Scan(&wsForkMillis); err != nil || wsForkMillis != 42 {
					t.Fatalf("workspaces(w1) fork_millis = %d, %v; want 42", wsForkMillis, err)
				}
				var cmdCreated int64
				if err := db.QueryRowContext(ctx, `SELECT created_at FROM commands WHERE id = 'c1' AND session_id = 's1' AND workspace_id = 'w1'`).Scan(&cmdCreated); err != nil || cmdCreated != 1600 {
					t.Fatalf("commands(c1) created_at = %d, %v; want 1600", cmdCreated, err)
				}
				var evText string
				if err := db.QueryRowContext(ctx, `SELECT text FROM events WHERE node_id = 'n1' AND kind = 'node'`).Scan(&evText); err != nil || evText != "joined" {
					t.Fatalf("events(n1) = %q, %v; want joined", evText, err)
				}
				var seDoc string
				if err := db.QueryRowContext(ctx, `SELECT doc FROM session_events WHERE session_id = 's1'`).Scan(&seDoc); err != nil || !strings.Contains(seDoc, `"WHAT_READY"`) {
					t.Fatalf("session_events(s1) = %q, %v; want typed WHAT_READY", seDoc, err)
				}

				// Verify writes via generated sqlc queries also succeed after migration 18.
				if err := q.UpsertNode(ctx, controllerdb.UpsertNodeParams{
					ID: "n4", Hostname: "vm1", Login: "ramon@", Addr: "100.64.0.4:7700", JoinedAt: 3000,
				}); err != nil {
					t.Fatalf("UpsertNode(n4) on upgraded DB: %v", err)
				}
				gotN4, err := q.GetNode(ctx, "n4")
				if err != nil {
					t.Fatalf("GetNode(n4) on upgraded DB: %v", err)
				}
				if gotN4.Hostname != "vm1" || gotN4.Addr != "100.64.0.4:7700" {
					t.Fatalf("GetNode(n4) = %+v, want hostname vm1 addr 100.64.0.4:7700", gotN4)
				}

				// Verify migration 14's history.session_id backfill ran on v11->v18.
				var histSess string
				if err := db.QueryRowContext(ctx, `SELECT session_id FROM history WHERE id = 'h1'`).Scan(&histSess); err != nil || histSess != "s1" {
					t.Fatalf("history.session_id = %q, %v; want s1", histSess, err)
				}

				assertAutoincrementMonotonic(t, db)
			})
		}
	}
}

type tableSchema struct {
	Name         string
	Type         string
	NCol         int
	WithoutRowID int
	Strict       int
	Columns      []columnSchema
	Indexes      []indexSchema
	ForeignKeys  []fkSchema
}

type columnSchema struct {
	CID     int
	Name    string
	Type    string
	NotNull int
	Default sql.NullString
	PK      int
	Hidden  int
}

type indexSchema struct {
	Name    string
	Unique  int
	Origin  string
	Partial int
	Columns []indexColumnSchema
	SQL     sql.NullString
}

type indexColumnSchema struct {
	Seqno int
	CID   int
	Name  sql.NullString
}

type fkSchema struct {
	Table    string
	From     string
	To       sql.NullString
	OnUpdate string
	OnDelete string
	Match    string
}

func inspectSchema(t *testing.T, db *sql.DB) []tableSchema {
	t.Helper()
	ctx := t.Context()

	rows, err := db.QueryContext(ctx, `PRAGMA table_list`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []tableSchema
	for rows.Next() {
		var schema string
		var ts tableSchema
		if err := rows.Scan(&schema, &ts.Name, &ts.Type, &ts.NCol, &ts.WithoutRowID, &ts.Strict); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if schema != "main" || strings.HasPrefix(ts.Name, "sqlite_") || ts.Name == goose.DefaultTablename {
			continue
		}
		tables = append(tables, ts)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	slices.SortFunc(tables, func(a, b tableSchema) int {
		return strings.Compare(a.Name, b.Name)
	})

	for i := range tables {
		tbl := &tables[i]

		colRows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_xinfo(%q)`, tbl.Name))
		if err != nil {
			t.Fatal(err)
		}
		for colRows.Next() {
			var c columnSchema
			if err := colRows.Scan(&c.CID, &c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK, &c.Hidden); err != nil {
				colRows.Close()
				t.Fatal(err)
			}
			tbl.Columns = append(tbl.Columns, c)
		}
		if err := colRows.Err(); err != nil {
			colRows.Close()
			t.Fatal(err)
		}
		colRows.Close()

		idxRows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_list(%q)`, tbl.Name))
		if err != nil {
			t.Fatal(err)
		}
		for idxRows.Next() {
			var seq int
			var idx indexSchema
			if err := idxRows.Scan(&seq, &idx.Name, &idx.Unique, &idx.Origin, &idx.Partial); err != nil {
				idxRows.Close()
				t.Fatal(err)
			}
			tbl.Indexes = append(tbl.Indexes, idx)
		}
		if err := idxRows.Err(); err != nil {
			idxRows.Close()
			t.Fatal(err)
		}
		idxRows.Close()

		for j := range tbl.Indexes {
			idx := &tbl.Indexes[j]
			infoRows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_info(%q)`, idx.Name))
			if err != nil {
				t.Fatal(err)
			}
			for infoRows.Next() {
				var ic indexColumnSchema
				if err := infoRows.Scan(&ic.Seqno, &ic.CID, &ic.Name); err != nil {
					infoRows.Close()
					t.Fatal(err)
				}
				idx.Columns = append(idx.Columns, ic)
			}
			if err := infoRows.Err(); err != nil {
				infoRows.Close()
				t.Fatal(err)
			}
			infoRows.Close()

			if idx.Origin == "c" {
				if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, idx.Name).Scan(&idx.SQL); err != nil {
					t.Fatal(err)
				}
			}
		}
		slices.SortFunc(tbl.Indexes, func(a, b indexSchema) int {
			return strings.Compare(a.Name, b.Name)
		})

		fkRows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%q)`, tbl.Name))
		if err != nil {
			t.Fatal(err)
		}
		for fkRows.Next() {
			var id, seq int
			var fk fkSchema
			if err := fkRows.Scan(&id, &seq, &fk.Table, &fk.From, &fk.To, &fk.OnUpdate, &fk.OnDelete, &fk.Match); err != nil {
				fkRows.Close()
				t.Fatal(err)
			}
			tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
		}
		if err := fkRows.Err(); err != nil {
			fkRows.Close()
			t.Fatal(err)
		}
		fkRows.Close()

		slices.SortFunc(tbl.ForeignKeys, func(a, b fkSchema) int {
			if c := strings.Compare(a.Table, b.Table); c != 0 {
				return c
			}
			if c := strings.Compare(a.From, b.From); c != 0 {
				return c
			}
			if c := strings.Compare(a.To.String, b.To.String); c != 0 {
				return c
			}
			if c := strings.Compare(a.OnUpdate, b.OnUpdate); c != 0 {
				return c
			}
			if c := strings.Compare(a.OnDelete, b.OnDelete); c != 0 {
				return c
			}
			return strings.Compare(a.Match, b.Match)
		})
	}

	return tables
}

// assertAutoincrementMonotonic verifies behaviorally that every AUTOINCREMENT
// durable-cursor / ID table in the controller schema never reuses a deleted
// rowid: inserting a row, deleting it, and inserting a second row must produce
// a strictly greater generated key (board 9586).
func assertAutoincrementMonotonic(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()

	check := func(table, pkCol, insertSQL, deleteSQL string, args ...any) int64 {
		t.Helper()
		res1, err := db.ExecContext(ctx, insertSQL, args...)
		if err != nil {
			t.Fatalf("%s insert #1: %v", table, err)
		}
		id1, err := res1.LastInsertId()
		if err != nil {
			t.Fatalf("%s lastInsertId #1: %v", table, err)
		}
		if _, err := db.ExecContext(ctx, deleteSQL, id1); err != nil {
			t.Fatalf("%s delete %s=%d: %v", table, pkCol, id1, err)
		}
		res2, err := db.ExecContext(ctx, insertSQL, args...)
		if err != nil {
			t.Fatalf("%s insert #2: %v", table, err)
		}
		id2, err := res2.LastInsertId()
		if err != nil {
			t.Fatalf("%s lastInsertId #2: %v", table, err)
		}
		if id2 <= id1 {
			t.Fatalf("%s.%s reused or did not strictly advance after delete: first=%d second=%d (AUTOINCREMENT missing?)", table, pkCol, id1, id2)
		}
		return id2
	}

	// Keep parent rows inserted by #2 alive while checking child tables with FKs.
	projID := check(
		"projects", "id",
		`INSERT INTO projects (name, created_at) VALUES ('autoinc-probe-proj', 1)`,
		`DELETE FROM projects WHERE id = ?`,
	)
	check(
		"project_changes", "seq",
		`INSERT INTO project_changes (project_id, what, repo, at, session, host, reason) VALUES (?, 'added', 'r', 1, '', '', '')`,
		`DELETE FROM project_changes WHERE seq = ?`,
		projID,
	)
	check(
		"session_events", "seq",
		`INSERT INTO session_events (session_id, doc) VALUES ('autoinc-probe', '{}')`,
		`DELETE FROM session_events WHERE seq = ?`,
	)
	check(
		"events", "id",
		`INSERT INTO events (at, kind, node_id, text, ref_type, ref_id) VALUES (1, 'probe', 'n1', 't', 'node', 'n1')`,
		`DELETE FROM events WHERE id = ?`,
	)
	check(
		"schedule_runs", "id",
		`INSERT INTO schedule_runs (schedule, doc) VALUES ('autoinc-probe', '{}')`,
		`DELETE FROM schedule_runs WHERE id = ?`,
	)
	questionID := check(
		"questions", "id",
		`INSERT INTO questions (project_id, repo, state, rank, asked_by, asked_at, updated_at, doc) VALUES (?, '', 'OPEN', 1, 's', 1, 1, '{}')`,
		`DELETE FROM questions WHERE id = ?`,
		projID,
	)
	check(
		"question_changes", "seq",
		`INSERT INTO question_changes (question_id, what, at, session, host, reason, doc) VALUES (?, 'asked', 1, '', '', '', '{}')`,
		`DELETE FROM question_changes WHERE seq = ?`,
		questionID,
	)
	reviewID := check(
		"reviews", "id",
		`INSERT INTO reviews (project_id, state, requested_by, requested_at, updated_at, doc) VALUES (?, 'OPEN', 's', 1, 1, '{}')`,
		`DELETE FROM reviews WHERE id = ?`,
		projID,
	)
	check(
		"review_findings", "id",
		`INSERT INTO review_findings (review_id, doc) VALUES (?, '{}')`,
		`DELETE FROM review_findings WHERE id = ?`,
		reviewID,
	)
	check(
		"review_events", "seq",
		`INSERT INTO review_events (review_id, finding_id, what, at, session, host, quick, text, doc) VALUES (?, 0, 'opened', 1, '', '', '', '', '{}')`,
		`DELETE FROM review_events WHERE seq = ?`,
		reviewID,
	)
	check(
		"placements", "id",
		`INSERT INTO placements (at, session_id, doc) VALUES (1, 'autoinc-probe', '{}')`,
		`DELETE FROM placements WHERE id = ?`,
	)

	// Guard against future migrations adding a new AUTOINCREMENT table without
	// adding it to the behavioral check above.
	checkedTables := []string{
		"events", "placements", "project_changes", "projects",
		"question_changes", "questions", "review_events", "review_findings",
		"reviews", "schedule_runs", "session_events",
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND sql LIKE '%AUTOINCREMENT%' AND name != ? ORDER BY name`, goose.DefaultTablename)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actualTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		actualTables = append(actualTables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(checkedTables, actualTables); diff != "" {
		t.Fatalf("AUTOINCREMENT tables mismatch (-checked +actual):\n%s", diff)
	}
}

// A fresh controller database migrated from 00011_baseline.sql..00018 must have
// the exact same schema (tables, STRICT flags, columns, types, defaults,
// indexes, and foreign keys) as both historical pre-squash live v11/v14/v17
// databases and released 26c5bf6 squashed v11/v14/v17 databases upgraded to
// v18, and all AUTOINCREMENT tables must advance monotonically after deletion
// (board 9388 / 9586).
func TestSchemaParityFreshVsUpgraded(t *testing.T) {
	ctx := t.Context()
	fresh := openRawSQLite(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err := Migrate(ctx, fresh); err != nil {
		t.Fatalf("Migrate fresh DB: %v", err)
	}
	wantSchema := inspectSchema(t, fresh)
	assertAutoincrementMonotonic(t, fresh)

	type layout struct {
		name     string
		v11DDL   string
		hasAcked bool
	}
	layouts := []layout{
		{name: "pre_squash", v11DDL: historicalV11Schema, hasAcked: true},
		{name: "released_26c5bf6", v11DDL: releasedSquashedV11Schema, hasAcked: false},
	}

	for _, l := range layouts {
		for _, startVer := range []int64{11, 14, 17} {
			t.Run(fmt.Sprintf("%s_from_v%d", l.name, startVer), func(t *testing.T) {
				db := openRawSQLite(t, filepath.Join(t.TempDir(), "live.db"))
				if _, err := db.ExecContext(ctx, l.v11DDL); err != nil {
					t.Fatalf("seed v11 schema (%s): %v", l.name, err)
				}
				if startVer >= 14 {
					if _, err := db.ExecContext(ctx, historicalV12To14Schema); err != nil {
						t.Fatalf("seed historical v12..v14 schema: %v", err)
					}
				}
				if startVer >= 17 {
					if _, err := db.ExecContext(ctx, historicalV16To17Schema); err != nil {
						t.Fatalf("seed historical v16..v17 schema: %v", err)
					}
				}
				if _, err := db.ExecContext(ctx, `CREATE TABLE `+goose.DefaultTablename+` (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT (datetime('now')))`); err != nil {
					t.Fatal(err)
				}
				if l.hasAcked {
					for v := int64(0); v <= startVer; v++ {
						if _, err := db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					if _, err := db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (0, 1)`); err != nil {
						t.Fatal(err)
					}
					for v := int64(11); v <= startVer; v++ {
						if _, err := db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
							t.Fatal(err)
						}
					}
				}

				if err := Migrate(ctx, db); err != nil {
					t.Fatalf("Migrate (%s) from v%d: %v", l.name, startVer, err)
				}

				gotSchema := inspectSchema(t, db)
				if diff := cmp.Diff(wantSchema, gotSchema); diff != "" {
					t.Fatalf("schema drift between fresh DB and upgraded %s v%d DB (-fresh +upgraded):\n%s", l.name, startVer, diff)
				}
				assertAutoincrementMonotonic(t, db)
			})
		}
	}
}

// An archived controller database below the squashed v11 baseline fails before
// applying 00011_baseline.sql with an actionable upgrade error rather than a
// raw "table nodes already exists" error.
func TestMigrateRejectsPreBaselineDatabase(t *testing.T) {
	ctx := t.Context()
	db := openRawSQLite(t, filepath.Join(t.TempDir(), "v10.db"))

	if _, err := db.ExecContext(ctx, historicalV11Schema); err != nil {
		t.Fatalf("seed pre-baseline schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE `+goose.DefaultTablename+` (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT (datetime('now')))`); err != nil {
		t.Fatal(err)
	}
	for v := int64(0); v <= 10; v++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
			t.Fatal(err)
		}
	}

	err := Migrate(ctx, db)
	if err == nil {
		t.Fatal("Migrate on v10 controller DB succeeded, want pre-baseline error")
	}
	msg := err.Error()
	if strings.Contains(msg, "already exists") {
		t.Fatalf("Migrate on v10 controller DB returned raw DDL error: %v", err)
	}
	for _, want := range []string{"version 10", "baseline 11", "preceding release"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}
