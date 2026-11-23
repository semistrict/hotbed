package worker

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"google.golang.org/protobuf/proto"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Opening a fresh worker database is idempotent, preserves its epoch across
// restarts, and refuses a schema version newer than this binary.
func TestWorkerDBOpenIdempotentAndRefusesFutureVersion(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "worker.db")

	d, err := openDB(ctx, path)
	if err != nil {
		t.Fatalf("openDB on fresh worker DB failed: %v", err)
	}
	epoch := d.epoch
	if epoch == "" {
		t.Fatal("fresh worker DB has empty epoch")
	}
	if err := d.close(); err != nil {
		t.Fatal(err)
	}

	// Opening again is idempotent and keeps the epoch.
	d2, err := openDB(ctx, path)
	if err != nil {
		t.Fatalf("reopening migrated worker DB failed: %v", err)
	}
	if d2.epoch != epoch {
		t.Errorf("epoch after reopen = %q, want %q", d2.epoch, epoch)
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

// A live v2 worker database (with the retired outbox table) upgrades cleanly
// through openDB: outbox is dropped while the existing epoch and records are
// preserved (board 9388).
func TestWorkerDBUpgradesLiveV2AndDropsOutbox(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "worker.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	v2 := `
	CREATE TABLE ` + goose.DefaultTablename + ` (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT (datetime('now')));
	INSERT INTO ` + goose.DefaultTablename + ` (version_id, is_applied) VALUES (0, 1), (1, 1), (2, 1);
	CREATE TABLE records (kind TEXT NOT NULL, id TEXT NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (kind, id)) STRICT;
	CREATE TABLE outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, doc TEXT NOT NULL) STRICT;
	CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
	INSERT INTO meta (key, value) VALUES ('epoch', 'live-v2-epoch');
	INSERT INTO records (kind, id, doc) VALUES
		('base', 'hotbed-g3', '{"id":"hotbed-g3","project":"hotbed","nodeId":"n1","generation":3,"state":"BASE_STATE_WARM","recipe":"r3","repos":[{"repo":"hotbed","commit":"c3","message":"fix it","toolchain":"go 1.25"}]}'),
		('workspace', 'w1', '{"workspace":{"id":"w1","project":"hotbed","baseId":"hotbed-g3","state":"WORKSPACE_STATE_IDLE","sessionId":"s1","syncHost":"laptop","repos":[{"repo":"hotbed","head":"h1","baseCommit":"c3","tree":"t1","filesChanged":2,"syncDir":"/home/me/hotbed"}]},"volume":"ws/w1","baseSnapshot":"hotbed-g3"}'),
		('command', 'c1', '{"command":{"id":"c1","workspaceId":"w1"},"repoId":"hotbed","project":"hotbed","dir":"/work/hotbed"}');
	`
	if _, err := raw.ExecContext(ctx, v2); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.close() })
	if d.epoch != "live-v2-epoch" {
		t.Errorf("epoch = %q, want live-v2-epoch", d.epoch)
	}
	var outboxName string
	if err := d.sql.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='outbox'`).Scan(&outboxName); err != sql.ErrNoRows {
		t.Errorf("outbox table still exists after v2->v3 upgrade: %q, %v", outboxName, err)
	}
	got := map[string]proto.Message{}
	for kind, m := range map[string]func() proto.Message{
		"base":      func() proto.Message { return &pb.Base{} },
		"workspace": func() proto.Message { return &pb.WorkspaceRecord{} },
		"command":   func() proto.Message { return &pb.CommandRecord{} },
	} {
		if err := d.all(kind, m, func(r proto.Message) { got[kind] = r }); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]proto.Message{
		"base": &pb.Base{
			Id: "hotbed-g3", Project: "hotbed", NodeId: "n1", Generation: 3, State: pb.BaseState_BASE_STATE_WARM, Recipe: "r3",
			Repos: []*pb.BaseRepo{{Repo: "hotbed", Commit: "c3", Message: "fix it", Toolchain: "go 1.25"}},
		},
		"workspace": &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{
				Id: "w1", Project: "hotbed", BaseId: "hotbed-g3", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE,
				SessionId: "s1", SyncHost: "laptop",
				Repos: []*pb.WorkspaceRepo{{Repo: "hotbed", Head: "h1", BaseCommit: "c3", Tree: "t1", FilesChanged: 2, SyncDir: "/home/me/hotbed"}},
			},
			Volume: "ws/w1", BaseSnapshot: "hotbed-g3",
		},
		"command": &pb.CommandRecord{Command: &pb.Command{Id: "c1", WorkspaceId: "w1"}, RepoId: "hotbed", Project: "hotbed", Dir: "/work/hotbed"},
	}
	for kind, w := range want {
		if !proto.Equal(got[kind], w) {
			t.Errorf("%s = %v\nwant %v", kind, got[kind], w)
		}
	}
}
