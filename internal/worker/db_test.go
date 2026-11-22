package worker

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// A pre-goose worker database (records/outbox/events/meta already present,
// no goose_db_version table) is adopted automatically at startup without
// losing existing rows.
func TestWorkerDBAdoptsLegacyDatabase(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "worker.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `
	CREATE TABLE records (kind TEXT NOT NULL, id TEXT NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (kind, id)) STRICT;
	CREATE TABLE outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
	INSERT INTO meta (key, value) VALUES ('epoch', 'legacy-epoch-123');
	`
	if _, err := raw.ExecContext(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := openDB(ctx, path)
	if err != nil {
		t.Fatalf("openDB on legacy worker DB failed: %v", err)
	}
	if d.epoch != "legacy-epoch-123" {
		t.Errorf("epoch = %q, want legacy-epoch-123", d.epoch)
	}
	if err := d.close(); err != nil {
		t.Fatal(err)
	}

	// Opening again is idempotent.
	d2, err := openDB(ctx, path)
	if err != nil {
		t.Fatalf("reopening migrated worker DB failed: %v", err)
	}
	if _, err := d2.sql.ExecContext(ctx, `INSERT INTO `+goose.DefaultTablename+` (version_id, is_applied) VALUES (999, 1)`); err != nil {
		t.Fatal(err)
	}
	d2.close()

	// A newer schema version is refused.
	if _, err := openDB(ctx, path); err == nil || !strings.Contains(err.Error(), "newer than this hotbed") {
		t.Errorf("openDB on future version = %v, want newer-than-hotbed error", err)
	}
}
