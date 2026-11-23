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

	workerdb "github.com/semistrict/hotbed/internal/worker/db/gen"
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

// historicalV2Schema is the exact schema of a live worker database at
// migration 2 (built independently of 00002_baseline.sql from historical
// migrations 1..2, including the retired outbox table).
const historicalV2Schema = `
CREATE TABLE records (
	kind TEXT NOT NULL,
	id   TEXT NOT NULL,
	doc  TEXT NOT NULL,
	PRIMARY KEY (kind, id)
) STRICT;

CREATE TABLE outbox (
	seq  INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,
	doc  TEXT NOT NULL
) STRICT;

CREATE TABLE events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	at  INTEGER NOT NULL,
	doc TEXT NOT NULL
) STRICT;

CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
`

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

// A fresh worker database applies 00002_baseline.sql and 00003_drop_outbox.sql
// cleanly, ends up with only records/events/meta (no outbox), and works with
// generated sqlc queries.
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
	if ver != 3 {
		t.Fatalf("schema version = %d, want 3", ver)
	}

	schema := inspectSchema(t, db)
	var gotTables []string
	for _, ts := range schema {
		gotTables = append(gotTables, ts.Name)
	}
	wantTables := []string{"events", "meta", "records"}
	if !slices.Equal(gotTables, wantTables) {
		t.Fatalf("fresh worker DB tables = %v, want %v", gotTables, wantTables)
	}

	q := workerdb.New(db)
	if err := q.InsertMetaIgnore(ctx, workerdb.InsertMetaIgnoreParams{Key: "epoch", Value: "ep-fresh"}); err != nil {
		t.Fatalf("InsertMetaIgnore: %v", err)
	}
	epoch, err := q.GetMeta(ctx, "epoch")
	if err != nil || epoch != "ep-fresh" {
		t.Fatalf("GetMeta(epoch) = %q, %v; want ep-fresh", epoch, err)
	}
	if err := q.UpsertRecord(ctx, workerdb.UpsertRecordParams{Kind: "base", ID: "p-g1", Doc: `{"id":"p-g1"}`}); err != nil {
		t.Fatalf("UpsertRecord: %v", err)
	}
	docs, err := q.ListRecordsByKind(ctx, "base")
	if err != nil || !slices.Equal(docs, []string{`{"id":"p-g1"}`}) {
		t.Fatalf("ListRecordsByKind = %v, %v", docs, err)
	}
	if _, err := q.InsertEvent(ctx, workerdb.InsertEventParams{At: 1000, Doc: `{"at":"1000"}`}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	evs, err := q.ListEventsAfter(ctx, workerdb.ListEventsAfterParams{Seq: 0, Limit: 10})
	if err != nil || len(evs) != 1 || evs[0].Doc != `{"at":"1000"}` {
		t.Fatalf("ListEventsAfter = %+v, %v", evs, err)
	}

	// Re-running Migrate is idempotent.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate on fresh DB: %v", err)
	}

	assertEventsAutoincrementMonotonic(t, db)
}

// assertEventsAutoincrementMonotonic verifies behaviorally that events.seq has
// SQLite AUTOINCREMENT semantics: after inserting an event and deleting/pruning
// it, the next inserted event receives a strictly greater sequence number rather
// than reusing the deleted rowid (board 9586).
func assertEventsAutoincrementMonotonic(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()
	q := workerdb.New(db)

	res1, err := q.InsertEvent(ctx, workerdb.InsertEventParams{At: 9001, Doc: `{"at":"9001"}`})
	if err != nil {
		t.Fatalf("InsertEvent #1: %v", err)
	}
	seq1, err := res1.LastInsertId()
	if err != nil {
		t.Fatalf("InsertEvent #1 LastInsertId: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM events WHERE seq = ?`, seq1); err != nil {
		t.Fatalf("delete event %d: %v", seq1, err)
	}
	res2, err := q.InsertEvent(ctx, workerdb.InsertEventParams{At: 9002, Doc: `{"at":"9002"}`})
	if err != nil {
		t.Fatalf("InsertEvent #2: %v", err)
	}
	seq2, err := res2.LastInsertId()
	if err != nil {
		t.Fatalf("InsertEvent #2 LastInsertId: %v", err)
	}
	if seq2 <= seq1 {
		t.Fatalf("events.seq reused or did not strictly advance after delete: first=%d second=%d (AUTOINCREMENT missing?)", seq1, seq2)
	}
}

// A historical live v2 worker database (with goose_db_version 0..2 and an
// outbox table) upgrades cleanly to v3 without re-running 00002_baseline.sql,
// drops outbox, preserves all live rows, and matches a fresh v3 database's
// schema identically (board 9388).
func TestMigratePreExistingV2DatabaseAndSchemaParity(t *testing.T) {
	ctx := t.Context()

	fresh := openRawSQLite(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err := Migrate(ctx, fresh); err != nil {
		t.Fatalf("Migrate fresh DB: %v", err)
	}
	wantSchema := inspectSchema(t, fresh)
	assertEventsAutoincrementMonotonic(t, fresh)

	live := openRawSQLite(t, filepath.Join(t.TempDir(), "live.db"))
	if _, err := live.ExecContext(ctx, historicalV2Schema); err != nil {
		t.Fatalf("seed historical v2 schema: %v", err)
	}
	if _, err := live.ExecContext(ctx, `CREATE TABLE `+goose.DefaultTablename+` (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT (datetime('now')))`); err != nil {
		t.Fatal(err)
	}
	for v := int64(0); v <= 2; v++ {
		if _, err := live.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := live.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES ('epoch', 'ep-live-v2');
		INSERT INTO records (kind, id, doc) VALUES ('base', 'hotbed-g1', '{"id":"hotbed-g1","project":"hotbed"}');
		INSERT INTO events (at, doc) VALUES (1234, '{"at":"1234"}');
	`); err != nil {
		t.Fatal(err)
	}

	// Confirm outbox is present before migration.
	var preOutbox string
	if err := live.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='outbox'`).Scan(&preOutbox); err != nil {
		t.Fatalf("expected outbox table before migration: %v", err)
	}

	if err := Migrate(ctx, live); err != nil {
		t.Fatalf("Migrate from v2: %v", err)
	}

	var ver int64
	if err := live.QueryRowContext(ctx, `SELECT MAX(version_id) FROM `+goose.DefaultTablename+` WHERE is_applied = 1`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != 3 {
		t.Fatalf("version after Migrate from v2 = %d, want 3", ver)
	}

	// Re-running Migrate is idempotent.
	if err := Migrate(ctx, live); err != nil {
		t.Fatalf("second Migrate from v2: %v", err)
	}

	// outbox must be gone.
	var postOutbox string
	if err := live.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='outbox'`).Scan(&postOutbox); err != sql.ErrNoRows {
		t.Fatalf("outbox table after migration = %q, %v; want sql.ErrNoRows", postOutbox, err)
	}

	// Schema must be 100% identical to a fresh v3 DB.
	gotSchema := inspectSchema(t, live)
	if diff := cmp.Diff(wantSchema, gotSchema); diff != "" {
		t.Fatalf("schema drift between fresh DB and upgraded v2 DB (-fresh +upgraded):\n%s", diff)
	}

	// Existing live rows are preserved and sqlc queries work cleanly.
	q := workerdb.New(live)
	epoch, err := q.GetMeta(ctx, "epoch")
	if err != nil || epoch != "ep-live-v2" {
		t.Fatalf("GetMeta(epoch) = %q, %v; want ep-live-v2", epoch, err)
	}
	docs, err := q.ListRecordsByKind(ctx, "base")
	if err != nil || !slices.Equal(docs, []string{`{"id":"hotbed-g1","project":"hotbed"}`}) {
		t.Fatalf("ListRecordsByKind = %v, %v", docs, err)
	}
	evs, err := q.ListEventsAfter(ctx, workerdb.ListEventsAfterParams{Seq: 0, Limit: 10})
	if err != nil || len(evs) != 1 || evs[0].Doc != `{"at":"1234"}` {
		t.Fatalf("ListEventsAfter = %+v, %v", evs, err)
	}

	assertEventsAutoincrementMonotonic(t, live)
}

// An archived v1 worker database predates the squashed v2 baseline; Migrate
// fails before applying 00002_baseline.sql with an actionable message rather
// than a raw "table records already exists" error (board 9586).
func TestMigrateRejectsPreBaselineV1Database(t *testing.T) {
	ctx := t.Context()
	db := openRawSQLite(t, filepath.Join(t.TempDir(), "v1.db"))

	if _, err := db.ExecContext(ctx, historicalV2Schema); err != nil {
		t.Fatalf("seed v1 schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE `+goose.DefaultTablename+` (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT (datetime('now')))`); err != nil {
		t.Fatal(err)
	}
	for v := int64(0); v <= 1; v++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('epoch', 'ep-v1')`); err != nil {
		t.Fatal(err)
	}

	err := Migrate(ctx, db)
	if err == nil {
		t.Fatal("Migrate on v1 worker DB succeeded, want pre-baseline error")
	}
	msg := err.Error()
	if strings.Contains(msg, "already exists") {
		t.Fatalf("Migrate on v1 worker DB returned raw DDL error: %v", err)
	}
	for _, want := range []string{"version 1", "baseline 2", "preceding release"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}

	// Database remains untouched at v1.
	var ver int64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM `+goose.DefaultTablename+` WHERE is_applied = 1`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Fatalf("version after rejected v1 Migrate = %d, want 1", ver)
	}
	var outboxName string
	if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='outbox'`).Scan(&outboxName); err != nil {
		t.Fatalf("expected outbox to remain after rejected v1 Migrate: %v", err)
	}
}
