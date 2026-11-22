package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// errNotJoined is a report from a node that never joined.
var errNotJoined = errors.New("node has not joined")

// migrations bring the schema from version i to i+1; PRAGMA user_version
// records how many have run. Messages are stored as protobuf JSON.
var migrations = []string{
	`CREATE TABLE nodes (
		id        TEXT PRIMARY KEY, -- tailnet stable node ID
		hostname  TEXT NOT NULL,
		login     TEXT NOT NULL,
		addr      TEXT NOT NULL DEFAULT '', -- tailnet address, to call it
		joined_at INTEGER NOT NULL, -- unix ms
		last_seen INTEGER,
		report    TEXT,             -- the latest ReportRequest, without its outbox
		acked     INTEGER NOT NULL DEFAULT 0 -- highest outbox seq stored
	) STRICT;
	CREATE TABLE repos (
		id  TEXT PRIMARY KEY,
		doc TEXT NOT NULL
	) STRICT;
	CREATE TABLE sessions (
		id             TEXT PRIMARY KEY,
		agent          TEXT NOT NULL,
		started_at     INTEGER NOT NULL,
		last_active_at INTEGER NOT NULL,
		ended_at       INTEGER,
		end_reason     TEXT NOT NULL DEFAULT ''
	) STRICT;
	CREATE TABLE workspaces (
		id          TEXT PRIMARY KEY,
		session_id  TEXT NOT NULL,
		node_id     TEXT NOT NULL,
		repo_id     TEXT NOT NULL,
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
		id       TEXT PRIMARY KEY,
		node_id  TEXT NOT NULL,
		repo_id  TEXT NOT NULL,
		ended_at INTEGER NOT NULL,
		doc      TEXT NOT NULL
	) STRICT;
	CREATE INDEX history_ended ON history (ended_at);
	CREATE TABLE samples (
		node_id TEXT NOT NULL,
		t       INTEGER NOT NULL,
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
		at       INTEGER NOT NULL,
		kind     TEXT NOT NULL,
		node_id  TEXT NOT NULL,
		text     TEXT NOT NULL,
		ref_type TEXT NOT NULL,
		ref_id   TEXT NOT NULL
	) STRICT`,
	`CREATE TABLE schedules (
		name    TEXT PRIMARY KEY,
		doc     TEXT NOT NULL,  -- the ScheduledCommand, without next_at and runs
		next_at INTEGER         -- unix ms; NULL while paused
	) STRICT;
	CREATE TABLE schedule_runs (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		schedule   TEXT NOT NULL,
		session_id TEXT NOT NULL DEFAULT '',
		command_id TEXT NOT NULL DEFAULT '',
		ended_at   INTEGER,     -- NULL while it runs
		doc        TEXT NOT NULL
	) STRICT;
	CREATE INDEX schedule_runs_schedule ON schedule_runs (schedule, id);
	CREATE INDEX schedule_runs_session ON schedule_runs (session_id);
	CREATE INDEX schedule_runs_live ON schedule_runs (ended_at) WHERE ended_at IS NULL`,
	// Workers push their events, which the controller folds into its view
	// of each node (node_state), instead of reporting their whole state; the
	// last reports become the views the events continue from.
	`ALTER TABLE nodes ADD COLUMN epoch TEXT NOT NULL DEFAULT '';
	ALTER TABLE nodes ADD COLUMN applied INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE nodes ADD COLUMN heartbeat TEXT;
	CREATE TABLE node_state (
		node_id TEXT NOT NULL,
		kind    TEXT NOT NULL, -- base, workspace, command
		id      TEXT NOT NULL,
		doc     TEXT NOT NULL,
		PRIMARY KEY (node_id, kind, id)
	) STRICT;
	INSERT INTO node_state (node_id, kind, id, doc)
		SELECT n.id, 'base', json_extract(j.value, '$.id'), j.value FROM nodes n, json_each(n.report, '$.bases') j WHERE n.report IS NOT NULL;
	INSERT INTO node_state (node_id, kind, id, doc)
		SELECT n.id, 'workspace', json_extract(j.value, '$.id'), j.value FROM nodes n, json_each(n.report, '$.workspaces') j WHERE n.report IS NOT NULL;
	INSERT INTO node_state (node_id, kind, id, doc)
		SELECT n.id, 'command', json_extract(j.value, '$.id'), j.value FROM nodes n, json_each(n.report, '$.commands') j WHERE n.report IS NOT NULL;
	-- A report's usage fields read as a heartbeat's.
	UPDATE nodes SET heartbeat = report;
	ALTER TABLE nodes DROP COLUMN report`,
}

// Retention: samples and events serve the cockpit's day; history and
// sessions serve longer questions. A scheduled command keeps its last
// keepRuns runs, however old: a nightly's go back months.
const (
	keepSamples = 25 * time.Hour
	keepHistory = 30 * 24 * time.Hour
	keepEvents  = 5000
	keepRuns    = 200
)

// Store is the controller's database.
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the database at path and brings its
// schema up to date.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
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

func marshal(m proto.Message) (string, error) {
	b, err := protojson.Marshal(m)
	return string(b), err
}

// unmarshal decodes a stored record. Fields a newer hotbed dropped are
// ignored, so records written before an upgrade still load.
func unmarshal[T proto.Message](doc string, m T) (T, error) {
	return m, protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal([]byte(doc), m)
}

// Join records p as a worker, or updates its name and owner if it joined
// before. Joining again keeps its join time and last report.
func (s *Store) Join(ctx context.Context, p tailnet.Peer, now time.Time) (*pb.Node, error) {
	addr := ""
	if p.Addr.IsValid() {
		addr = p.Addr.String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (id, hostname, login, addr, joined_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET hostname = excluded.hostname, login = excluded.login, addr = excluded.addr`,
		p.NodeID, p.Hostname, p.Login, addr, now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("record join of %s: %w", p.Hostname, err)
	}
	n, err := s.node(ctx, p.NodeID)
	if err != nil {
		return nil, err
	}
	return n.node, nil
}

// Ingest stores a report from a worker before the event stream: its whole
// state becomes the node's view, its usage the node's heartbeat, and the
// finished commands and events it has not delivered before go to history.
func (s *Store) Ingest(ctx context.Context, nodeID string, r *pb.ReportRequest, now time.Time) (pushed, error) {
	out := pushed{sessions: map[string]bool{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var acked int64
	if err := tx.QueryRowContext(ctx, `SELECT acked FROM nodes WHERE id = ?`, nodeID).Scan(&acked); errors.Is(err, sql.ErrNoRows) {
		return out, errNotJoined
	} else if err != nil {
		return out, err
	}
	if err := replaceView(ctx, tx, nodeID, r.Bases, r.Workspaces, r.Commands); err != nil {
		return out, err
	}
	hb := &pb.HeartbeatRequest{
		ClusterId: r.ClusterId, Resources: r.Resources, Usage: r.Usage, DiskBytes: r.DiskBytes, Disk: r.Disk,
		ReservedMemoryBytes: r.ReservedMemoryBytes, OomKills: r.OomKills, Storage: r.Storage,
	}
	for _, c := range r.Commands {
		hb.Commands = append(hb.Commands, &pb.CommandUsage{Id: c.Id, CpuCores: c.CpuCores, MemoryBytes: c.MemoryBytes, SwapBytes: c.SwapBytes})
	}
	for _, b := range r.Bases {
		hb.Bases = append(hb.Bases, &pb.BaseUsage{Id: b.Id, Progress: b.Progress, SizeBytes: b.SizeBytes})
	}
	for _, w := range r.Workspaces {
		hb.Workspaces = append(hb.Workspaces, &pb.WorkspaceUsage{Id: w.Id, ExclusiveBytes: w.ExclusiveBytes, SharedBytes: w.SharedBytes})
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET fork_millis = ? WHERE id = ?`, w.ForkMillis, w.Id); err != nil {
			return out, err
		}
	}
	if err := beat(ctx, tx, nodeID, hb, now); err != nil {
		return out, err
	}
	high := acked
	for _, f := range r.Finished {
		high = max(high, f.Seq)
		if f.Seq <= acked {
			continue
		}
		if err := addHistory(ctx, tx, nodeID, f.Item); err != nil {
			return out, err
		}
	}
	for _, e := range r.Events {
		high = max(high, e.Seq)
		if e.Seq <= acked {
			continue
		}
		if err := addFeed(ctx, tx, nodeID, e.Item); err != nil {
			return out, err
		}
	}
	for _, e := range r.SessionEvents {
		high = max(high, e.Seq)
		if e.Seq <= acked {
			continue
		}
		if err := addSessionEvent(ctx, tx, e.Item, out); err != nil {
			return out, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET acked = ? WHERE id = ?`, high, nodeID); err != nil {
		return out, err
	}
	out.applied = high
	return out, tx.Commit()
}

// Prune drops samples, history, events, commands and scheduled runs past
// their retention. Each statement is an Exec of its own: given several, the
// driver binds every one's parameters from the first argument.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	history := now.Add(-keepHistory).UnixMilli()
	for _, p := range []struct {
		query string
		arg   any
	}{
		{`DELETE FROM samples WHERE t < ?`, now.Add(-keepSamples).UnixMilli()},
		{`DELETE FROM history WHERE ended_at < ?`, history},
		{`DELETE FROM events WHERE id <= (SELECT MAX(id) FROM events) - ?`, keepEvents},
		{`DELETE FROM commands WHERE created_at < ?`, history},
		{`DELETE FROM schedule_runs WHERE ended_at IS NOT NULL AND (
			schedule NOT IN (SELECT name FROM schedules) OR
			id NOT IN (SELECT id FROM schedule_runs r WHERE r.schedule = schedule_runs.schedule ORDER BY id DESC LIMIT ?))`, keepRuns},
	} {
		if _, err := s.db.ExecContext(ctx, p.query, p.arg); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	return nil
}

// nodeRow is a node with its latest heartbeat.
type nodeRow struct {
	node *pb.Node
	addr string               // tailnet address; empty when the tailnet gave none
	hb   *pb.HeartbeatRequest // nil before the first heartbeat
}

const nodeColumns = `id, hostname, login, addr, joined_at, last_seen, heartbeat`

// Nodes lists every worker by host name. Online is left false; the server
// decides it from last_seen.
func (s *Store) Nodes(ctx context.Context) ([]nodeRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY hostname, id`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	var nodes []nodeRow
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
	n, err := scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nodeRow{}, api.Errorf(api.CodeNotFound, "no node %s", id)
	}
	return n, err
}

func scanNode(row interface{ Scan(...any) error }) (nodeRow, error) {
	var (
		n        = &pb.Node{}
		addr     string
		lastSeen sql.NullInt64
		hb       sql.NullString
	)
	if err := row.Scan(&n.Id, &n.Hostname, &n.Login, &addr, &n.JoinedAt, &lastSeen, &hb); err != nil {
		return nodeRow{}, err
	}
	out := nodeRow{node: n, addr: addr}
	if lastSeen.Valid {
		n.LastSeen = &lastSeen.Int64
	}
	if hb.Valid {
		h, err := unmarshal(hb.String, &pb.HeartbeatRequest{})
		if err != nil {
			return nodeRow{}, fmt.Errorf("read node %s heartbeat: %w", n.Hostname, err)
		}
		out.hb = h
		n.Resources, n.Usage, n.DiskBytes, n.Disk = h.Resources, h.Usage, h.DiskBytes, h.Disk
		n.ReservedMemoryBytes, n.Storage = h.ReservedMemoryBytes, h.Storage
	}
	return out, nil
}

// PutRepo adds or replaces a repo.
func (s *Store) PutRepo(ctx context.Context, r *pb.Repo) error {
	doc, err := marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO repos (id, doc) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET doc = excluded.doc`, r.Id, doc)
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
func (s *Store) Repos(ctx context.Context) ([]*pb.Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM repos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.Repo
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		r, err := unmarshal(doc, &pb.Repo{})
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StartSession records a new session; its name must be free.
func (s *Store) StartSession(ctx context.Context, id, agent string, now time.Time) (*pb.Session, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, agent, started_at, last_active_at) VALUES (?, ?, ?, ?)`, id, agent, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		if _, gerr := s.Session(ctx, id); gerr == nil {
			return nil, api.Errorf(api.CodeConflict, "a session called %s exists", id).WithSuggestion("pick another name, or leave it out for a random one")
		}
		return nil, err
	}
	return s.Session(ctx, id)
}

// Session returns a session with its workspaces.
func (s *Store) Session(ctx context.Context, id string) (*pb.Session, error) {
	out := &pb.Session{Id: id}
	var ended sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT agent, started_at, last_active_at, ended_at, end_reason FROM sessions WHERE id = ?`, id).
		Scan(&out.Agent, &out.StartedAt, &out.LastActiveAt, &ended, &out.EndReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no session %s", id).WithSuggestion("start one with hotbed session start")
	}
	if err != nil {
		return nil, err
	}
	if ended.Valid {
		out.EndedAt = &ended.Int64
	}
	err = s.db.QueryRowContext(ctx, `SELECT schedule, ended_at IS NULL FROM schedule_runs WHERE session_id = ? LIMIT 1`, id).Scan(&out.Schedule, &out.ScheduleRunning)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_id, node_id, created_at, released_at FROM workspaces WHERE session_id = ? ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		w := &pb.SessionWorkspace{}
		var released sql.NullInt64
		if err := rows.Scan(&w.Id, &w.RepoId, &w.NodeId, &w.CreatedAt, &released); err != nil {
			return nil, err
		}
		if released.Valid {
			w.ReleasedAt = &released.Int64
		} else {
			out.WorkspaceIds = append(out.WorkspaceIds, w.Id)
		}
		out.Workspaces = append(out.Workspaces, w)
	}
	return out, rows.Err()
}

// RecentSessions are the live sessions and those that ended since, the
// most recently active first.
func (s *Store) RecentSessions(ctx context.Context, since time.Time) ([]*pb.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE ended_at IS NULL OR ended_at >= ? ORDER BY last_active_at DESC LIMIT 500`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*pb.Session, 0, len(ids))
	for _, id := range ids {
		sess, err := s.Session(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, nil
}

// LiveSession returns a session that has not ended, and marks it active.
func (s *Store) LiveSession(ctx context.Context, id string, now time.Time) (*pb.Session, error) {
	sess, err := s.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.EndedAt != nil {
		return nil, api.Errorf(api.CodeNotFound, "session %s ended: %s", id, sess.EndReason).WithSuggestion("start a new one with hotbed session start")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_active_at = ? WHERE id = ?`, now.UnixMilli(), id); err != nil {
		return nil, err
	}
	sess.LastActiveAt = now.UnixMilli()
	return sess, nil
}

// Touch marks a session active, if it has not ended.
func (s *Store) Touch(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_active_at = ? WHERE id = ? AND ended_at IS NULL`, now.UnixMilli(), id)
	return err
}

// EndSession marks a session ended.
func (s *Store) EndSession(ctx context.Context, id, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL`, now.UnixMilli(), reason, id)
	return err
}

// IdleSessions lists live sessions nothing has touched since before. A
// scheduled run's session is never idle while the run is in progress: no
// agent is there to touch it.
func (s *Store) IdleSessions(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM sessions WHERE ended_at IS NULL AND last_active_at < ?
		AND id NOT IN (SELECT session_id FROM schedule_runs WHERE ended_at IS NULL)`, before.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LiveSessions lists sessions that have not ended, for the cockpit.
func (s *Store) LiveSessions(ctx context.Context) ([]*pb.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE ended_at IS NULL ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*pb.Session, 0, len(ids))
	for _, id := range ids {
		sess, err := s.Session(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, nil
}

// SessionEvents lists a session's events after seq.
func (s *Store) SessionEvents(ctx context.Context, session string, after int64) ([]*pb.SessionEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, doc FROM session_events WHERE session_id = ? AND seq > ? ORDER BY seq`, session, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.SessionEvent
	for rows.Next() {
		var seq int64
		var doc string
		if err := rows.Scan(&seq, &doc); err != nil {
			return nil, err
		}
		ev, err := unmarshal(doc, &pb.SessionEvent{})
		if err != nil {
			return nil, err
		}
		ev.Seq = seq
		out = append(out, ev)
	}
	return out, rows.Err()
}

// AddWorkspace records where a workspace lives, so its calls reach its node.
func (s *Store) AddWorkspace(ctx context.Context, id, session, nodeID, repo, agent string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces (id, session_id, node_id, repo_id, agent, created_at) VALUES (?, ?, ?, ?, ?, ?)`, id, session, nodeID, repo, agent, now.UnixMilli())
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

// workspaceRow is where a workspace lives and whose it is.
type workspaceRow struct {
	id, session, node, repo string
	released                bool
}

// Workspace looks up where a workspace lives.
func (s *Store) Workspace(ctx context.Context, id string) (workspaceRow, error) {
	w := workspaceRow{id: id}
	var released sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT session_id, node_id, repo_id, released_at FROM workspaces WHERE id = ?`, id).Scan(&w.session, &w.node, &w.repo, &released)
	if errors.Is(err, sql.ErrNoRows) {
		return w, api.Errorf(api.CodeNotFound, "no workspace %s", id)
	}
	w.released = released.Valid
	return w, err
}

// SessionWorkspace finds a session's live workspace of a repo.
func (s *Store) SessionWorkspace(ctx context.Context, session, repo string) (workspaceRow, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE session_id = ? AND repo_id = ? AND released_at IS NULL ORDER BY created_at DESC LIMIT 1`, session, repo).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return workspaceRow{}, false, nil
	}
	if err != nil {
		return workspaceRow{}, false, err
	}
	w, err := s.Workspace(ctx, id)
	return w, err == nil, err
}

// AddCommand records where a command runs.
func (s *Store) AddCommand(ctx context.Context, id, session, nodeID, workspace string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES (?, ?, ?, ?, ?)`, id, session, nodeID, workspace, now.UnixMilli())
	return err
}

func (s *Store) DropCommand(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM commands WHERE id = ?`, id)
	return err
}

// Command looks up the session and node of a command.
func (s *Store) Command(ctx context.Context, id string) (session, node string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT session_id, node_id FROM commands WHERE id = ?`, id).Scan(&session, &node)
	if errors.Is(err, sql.ErrNoRows) {
		// A base's freshen belongs to no session; history knows its node.
		err = s.db.QueryRowContext(ctx, `SELECT node_id FROM history WHERE id = ?`, id).Scan(&node)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", api.Errorf(api.CodeNotFound, "no command %s", id)
	}
	return session, node, err
}

// Finished is a command that ended, as history keeps it; ok is false
// while it has not (or its worker has not said so yet).
func (s *Store) Finished(ctx context.Context, id string) (f *pb.FinishedCommand, ok bool, err error) {
	var doc string
	err = s.db.QueryRowContext(ctx, `SELECT doc FROM history WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	f, err = unmarshal(doc, &pb.FinishedCommand{})
	return f, err == nil, err
}

// History lists commands that ended since since, oldest first.
func (s *Store) History(ctx context.Context, since time.Time) ([]*pb.FinishedCommand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM history WHERE ended_at >= ? ORDER BY ended_at`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.FinishedCommand
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		f, err := unmarshal(doc, &pb.FinishedCommand{})
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Events lists the newest n events, newest first.
func (s *Store) Events(ctx context.Context, n int) ([]*pb.ClusterEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, kind, node_id, text, ref_type, ref_id FROM events ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.ClusterEvent
	for rows.Next() {
		e := &pb.ClusterEvent{}
		var refType, refID string
		if err := rows.Scan(&e.Id, &e.At, &e.Kind, &e.NodeId, &e.Text, &refType, &refID); err != nil {
			return nil, err
		}
		if refType != "" {
			e.Ref = &pb.EventRef{Type: refType, Id: refID}
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
