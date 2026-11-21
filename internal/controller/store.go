package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// errNotJoined is a report from a node that never joined.
var errNotJoined = errors.New("node has not joined")

// migrations bring the schema from version i to i+1; PRAGMA user_version
// records how many have run.
var migrations = []string{
	`CREATE TABLE nodes (
		id           TEXT PRIMARY KEY, -- tailnet stable node ID
		hostname     TEXT NOT NULL,
		login        TEXT NOT NULL,
		joined_at    INTEGER NOT NULL, -- unix nanoseconds
		last_seen    INTEGER,
		arch         TEXT,
		cpus         INTEGER,
		memory_bytes INTEGER,
		kernel       TEXT
	) STRICT`,
	`ALTER TABLE nodes ADD COLUMN report TEXT; -- the node's latest report, without its outbox
	ALTER TABLE nodes ADD COLUMN acked INTEGER NOT NULL DEFAULT 0; -- highest outbox seq stored
	CREATE TABLE repos (
		id  TEXT PRIMARY KEY,
		doc TEXT NOT NULL
	) STRICT;
	CREATE TABLE workspaces (
		id          TEXT PRIMARY KEY,
		node_id     TEXT NOT NULL,
		repo_id     TEXT NOT NULL,
		agent       TEXT NOT NULL,
		created_at  INTEGER NOT NULL, -- unix ms
		fork_millis INTEGER,
		released_at INTEGER
	) STRICT;
	CREATE INDEX workspaces_created ON workspaces (created_at);
	CREATE TABLE commands (
		id           TEXT PRIMARY KEY,
		node_id      TEXT NOT NULL,
		workspace_id TEXT NOT NULL,
		created_at   INTEGER NOT NULL
	) STRICT;
	CREATE TABLE history (
		id       TEXT PRIMARY KEY,
		node_id  TEXT NOT NULL,
		repo_id  TEXT NOT NULL,
		ended_at INTEGER NOT NULL, -- unix ms
		doc      TEXT NOT NULL
	) STRICT;
	CREATE INDEX history_ended ON history (ended_at);
	CREATE TABLE samples (
		node_id TEXT NOT NULL,
		t       INTEGER NOT NULL, -- unix ms
		cpu     REAL NOT NULL,
		memory  REAL NOT NULL,     -- share of RAM in use
		psi     REAL NOT NULL,     -- memory pressure, some avg10
		running INTEGER NOT NULL,
		queued  INTEGER NOT NULL,
		parked  INTEGER NOT NULL,
		oom     INTEGER NOT NULL,  -- kernel OOM kills since boot
		PRIMARY KEY (node_id, t)
	) STRICT;
	CREATE INDEX samples_t ON samples (t);
	CREATE TABLE events (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		at       INTEGER NOT NULL, -- unix ms
		kind     TEXT NOT NULL,
		node_id  TEXT,
		text     TEXT NOT NULL,
		ref_type TEXT,
		ref_id   TEXT
	) STRICT`,
}

// Retention: samples and events serve the cockpit's day; history serves
// longer questions.
const (
	keepSamples = 25 * time.Hour
	keepHistory = 30 * 24 * time.Hour
	keepEvents  = 5000
)

// Store is the controller's database.
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the database at path and brings its
// schema up to date.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection serializes writes, so none waits on SQLite's lock.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this hotbed (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Join records p as a worker, or updates its name and owner if it joined
// before. Joining again keeps its join time and last report.
func (s *Store) Join(ctx context.Context, p tailnet.Peer, now time.Time) (api.Node, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (id, hostname, login, joined_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET hostname = excluded.hostname, login = excluded.login`,
		p.NodeID, p.Hostname, p.Login, now.UnixNano())
	if err != nil {
		return api.Node{}, fmt.Errorf("record join of %s: %w", p.Hostname, err)
	}
	n, err := s.node(ctx, p.NodeID)
	return n.Node, err
}

// Ingest stores a worker's report: its state, a sample, and the finished
// commands and events it has not delivered before. It returns the highest
// outbox sequence number now stored.
func (s *Store) Ingest(ctx context.Context, nodeID string, r api.Report, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var acked int64
	if err := tx.QueryRowContext(ctx, `SELECT acked FROM nodes WHERE id = ?`, nodeID).Scan(&acked); errors.Is(err, sql.ErrNoRows) {
		return 0, errNotJoined
	} else if err != nil {
		return 0, err
	}
	state := r
	state.Finished, state.Events = nil, nil
	doc, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE nodes SET last_seen = ?, arch = ?, cpus = ?, memory_bytes = ?, kernel = ?, report = ?
		WHERE id = ?`,
		now.UnixNano(), r.Resources.Arch, r.Resources.CPUs, r.Resources.MemoryBytes, r.Resources.Kernel, string(doc), nodeID); err != nil {
		return 0, fmt.Errorf("record report: %w", err)
	}
	var running, queued, parked int
	for _, c := range r.Commands {
		switch c.State {
		case api.CommandRunning:
			running++
		case api.CommandQueued:
			queued++
		case api.CommandParked:
			parked++
		}
	}
	memFrac := 0.0
	if r.Resources.MemoryBytes > 0 {
		memFrac = float64(r.Usage.MemoryUsedBytes) / float64(r.Resources.MemoryBytes)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO samples (node_id, t, cpu, memory, psi, running, queued, parked, oom) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, now.UnixMilli(), r.Usage.CPUBusy, memFrac, r.Usage.MemoryPressure, running, queued, parked, r.OOMKills); err != nil {
		return 0, fmt.Errorf("record sample: %w", err)
	}
	for _, w := range r.Workspaces {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET fork_millis = ? WHERE id = ?`, w.ForkMillis, w.ID); err != nil {
			return 0, err
		}
	}
	high := acked
	for _, f := range r.Finished {
		high = max(high, f.Seq)
		if f.Seq <= acked {
			continue
		}
		doc, err := json.Marshal(f.Item)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history (id, node_id, repo_id, ended_at, doc) VALUES (?, ?, ?, ?, ?)`,
			f.Item.ID, nodeID, f.Item.RepoID, f.Item.EndedAt, string(doc)); err != nil {
			return 0, fmt.Errorf("record finished command: %w", err)
		}
	}
	for _, e := range r.Events {
		high = max(high, e.Seq)
		if e.Seq <= acked {
			continue
		}
		var refType, refID any
		if e.Item.Ref != nil {
			refType, refID = e.Item.Ref.Type, e.Item.Ref.ID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, kind, node_id, text, ref_type, ref_id) VALUES (?, ?, ?, ?, ?, ?)`,
			e.Item.At.UnixMilli(), e.Item.Kind, nodeID, e.Item.Text, refType, refID); err != nil {
			return 0, fmt.Errorf("record event: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET acked = ? WHERE id = ?`, high, nodeID); err != nil {
		return 0, err
	}
	return high, tx.Commit()
}

// Prune drops samples, history and events past their retention.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM samples WHERE t < ?;
		DELETE FROM history WHERE ended_at < ?;
		DELETE FROM events WHERE id <= (SELECT MAX(id) FROM events) - ?;
		DELETE FROM commands WHERE created_at < ?;`,
		now.Add(-keepSamples).UnixMilli(), now.Add(-keepHistory).UnixMilli(), keepEvents, now.Add(-keepHistory).UnixMilli())
	return err
}

// nodeRow is a node with its latest report.
type nodeRow struct {
	api.Node
	Report *api.Report
}

const nodeColumns = `id, hostname, login, joined_at, last_seen, arch, cpus, memory_bytes, kernel, report`

// Nodes lists every worker by host name. Online is left false; the server
// decides it from LastSeen.
func (s *Store) Nodes(ctx context.Context) ([]nodeRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY hostname, id`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	nodes := []nodeRow{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (s *Store) node(ctx context.Context, id string) (nodeRow, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id))
}

func scanNode(row interface{ Scan(...any) error }) (nodeRow, error) {
	var (
		n        nodeRow
		joinedAt int64
		lastSeen sql.NullInt64
		arch     sql.NullString
		cpus     sql.NullInt64
		memory   sql.NullInt64
		kernel   sql.NullString
		report   sql.NullString
	)
	if err := row.Scan(&n.ID, &n.Hostname, &n.Login, &joinedAt, &lastSeen, &arch, &cpus, &memory, &kernel, &report); err != nil {
		return nodeRow{}, fmt.Errorf("read node: %w", err)
	}
	n.JoinedAt = time.Unix(0, joinedAt).UTC()
	if lastSeen.Valid {
		t := time.Unix(0, lastSeen.Int64).UTC()
		n.LastSeen = &t
		n.Resources = &api.Resources{Arch: arch.String, CPUs: int(cpus.Int64), MemoryBytes: memory.Int64, Kernel: kernel.String}
	}
	if report.Valid {
		var r api.Report
		if err := json.Unmarshal([]byte(report.String), &r); err != nil {
			return nodeRow{}, fmt.Errorf("read node %s report: %w", n.Hostname, err)
		}
		n.Report = &r
		if n.Resources != nil {
			n.Resources.SwapBytes = r.Resources.SwapBytes
			n.Resources.Machine = r.Resources.Machine
		}
	}
	return n, nil
}

// PutRepo adds or replaces a repo.
func (s *Store) PutRepo(ctx context.Context, r api.Repo) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO repos (id, doc) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET doc = excluded.doc`, r.ID, string(doc))
	return err
}

// DeleteRepo removes a repo; workers stop keeping its bases.
func (s *Store) DeleteRepo(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Repos lists every repo by ID.
func (s *Store) Repos(ctx context.Context) ([]api.Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM repos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Repo{}
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var r api.Repo
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddWorkspace records where a workspace lives, so its calls reach its node.
func (s *Store) AddWorkspace(ctx context.Context, id, nodeID, repo, agent string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces (id, node_id, repo_id, agent, created_at) VALUES (?, ?, ?, ?, ?)`, id, nodeID, repo, agent, now.UnixMilli())
	return err
}

func (s *Store) DropWorkspace(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	return err
}

func (s *Store) ReleaseWorkspace(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspaces SET released_at = ? WHERE id = ?`, now.UnixMilli(), id)
	return err
}

// WorkspaceNode is the node a workspace lives on.
func (s *Store) WorkspaceNode(ctx context.Context, id string) (string, error) {
	var node string
	err := s.db.QueryRowContext(ctx, `SELECT node_id FROM workspaces WHERE id = ?`, id).Scan(&node)
	if errors.Is(err, sql.ErrNoRows) {
		return "", api.Errorf(api.CodeNotFound, "no workspace %s", id).WithSuggestion("hotbed ws ls lists workspaces")
	}
	return node, err
}

// AddCommand records where a command runs.
func (s *Store) AddCommand(ctx context.Context, id, nodeID, workspace string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO commands (id, node_id, workspace_id, created_at) VALUES (?, ?, ?, ?)`, id, nodeID, workspace, now.UnixMilli())
	return err
}

func (s *Store) DropCommand(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM commands WHERE id = ?`, id)
	return err
}

// CommandNode is the node a command runs or ran on.
func (s *Store) CommandNode(ctx context.Context, id string) (string, error) {
	var node string
	err := s.db.QueryRowContext(ctx, `SELECT node_id FROM commands WHERE id = ?`, id).Scan(&node)
	if errors.Is(err, sql.ErrNoRows) {
		// Freshen commands and old ones are known only by their history.
		err = s.db.QueryRowContext(ctx, `SELECT node_id FROM history WHERE id = ?`, id).Scan(&node)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", api.Errorf(api.CodeNotFound, "no command %s", id)
	}
	return node, err
}

// History lists commands that ended since since, oldest first.
func (s *Store) History(ctx context.Context, since time.Time) ([]api.FinishedCommand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM history WHERE ended_at >= ? ORDER BY ended_at`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.FinishedCommand{}
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var f api.FinishedCommand
		if err := json.Unmarshal([]byte(doc), &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Events lists the newest n events, newest first.
func (s *Store) Events(ctx context.Context, n int) ([]api.ClusterEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, kind, node_id, text, ref_type, ref_id FROM events ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.ClusterEvent{}
	for rows.Next() {
		var (
			e              api.ClusterEvent
			at             int64
			node           sql.NullString
			refType, refID sql.NullString
		)
		if err := rows.Scan(&e.ID, &at, &e.Kind, &node, &e.Text, &refType, &refID); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC().Format(time.RFC3339Nano)
		if node.Valid {
			e.NodeID = &node.String
		}
		if refType.Valid {
			e.Ref = &api.EventRef{Type: refType.String, ID: refID.String}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type sampleRow struct {
	node                    string
	t                       int64
	cpu, memory, psi        float64
	running, queued, parked int
	oom                     int64
}

// Samples lists samples since since, oldest first.
func (s *Store) Samples(ctx context.Context, since time.Time) ([]sampleRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, t, cpu, memory, psi, running, queued, parked, oom FROM samples WHERE t >= ? ORDER BY t`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sampleRow
	for rows.Next() {
		var r sampleRow
		if err := rows.Scan(&r.node, &r.t, &r.cpu, &r.memory, &r.psi, &r.running, &r.queued, &r.parked, &r.oom); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type forkRow struct {
	at     int64
	millis int64
}

// Forks lists workspaces created since since.
func (s *Store) Forks(ctx context.Context, since time.Time) ([]forkRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT created_at, COALESCE(fork_millis, -1) FROM workspaces WHERE created_at >= ? ORDER BY created_at`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []forkRow
	for rows.Next() {
		var f forkRow
		if err := rows.Scan(&f.at, &f.millis); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
