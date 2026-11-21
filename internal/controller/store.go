package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// errNotJoined is a heartbeat from a node that never joined.
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
}

// Store is the controller's database.
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the database at path and brings its
// schema up to date.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
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
	return s.node(ctx, p.NodeID)
}

// Heartbeat records a report from the worker with nodeID.
func (s *Store) Heartbeat(ctx context.Context, nodeID string, r api.Resources, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET last_seen = ?, arch = ?, cpus = ?, memory_bytes = ?, kernel = ?
		WHERE id = ?`,
		now.UnixNano(), r.Arch, r.CPUs, r.MemoryBytes, r.Kernel, nodeID)
	if err != nil {
		return fmt.Errorf("record heartbeat: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record heartbeat: %w", err)
	}
	if n == 0 {
		return errNotJoined
	}
	return nil
}

const nodeColumns = `id, hostname, login, joined_at, last_seen, arch, cpus, memory_bytes, kernel`

// Nodes lists every worker by host name. Online is left false; the server
// decides it from LastSeen.
func (s *Store) Nodes(ctx context.Context) ([]api.Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY hostname, id`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	nodes := []api.Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return nodes, nil
}

func (s *Store) node(ctx context.Context, id string) (api.Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id))
}

func scanNode(row interface{ Scan(...any) error }) (api.Node, error) {
	var (
		n        api.Node
		joinedAt int64
		lastSeen sql.NullInt64
		arch     sql.NullString
		cpus     sql.NullInt64
		memory   sql.NullInt64
		kernel   sql.NullString
	)
	if err := row.Scan(&n.ID, &n.Hostname, &n.Login, &joinedAt, &lastSeen, &arch, &cpus, &memory, &kernel); err != nil {
		return api.Node{}, fmt.Errorf("read node: %w", err)
	}
	n.JoinedAt = time.Unix(0, joinedAt).UTC()
	if lastSeen.Valid {
		t := time.Unix(0, lastSeen.Int64).UTC()
		n.LastSeen = &t
		n.Resources = &api.Resources{
			Arch:        arch.String,
			CPUs:        int(cpus.Int64),
			MemoryBytes: uint64(memory.Int64),
			Kernel:      kernel.String,
		}
	}
	return n, nil
}
