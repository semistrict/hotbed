package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/semistrict/hotbed/internal/api"
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	controllermigrations "github.com/semistrict/hotbed/internal/controller/db/migrations"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// errNotJoined is a report from a node that never joined.
var errNotJoined = errors.New("node has not joined")

// Retention: samples and events serve the cockpit's day; history and
// sessions serve longer questions. A scheduled command keeps its last
// keepRuns runs, however old: a nightly's go back months.
const (
	keepSamples = 25 * time.Hour
	// keepPlacements is how long placement decisions are kept.
	keepPlacements = 24 * time.Hour
	keepHistory    = 30 * 24 * time.Hour
	keepEvents     = 5000
	keepRuns       = 200
)

// Store is the controller's database.
type Store struct {
	db         *sql.DB
	q          *controllerdb.Queries
	samplesGen atomic.Uint64
}

func (s *Store) samplesVersion() uint64 { return s.samplesGen.Load() }

// OpenStore opens (creating if needed) the database at path and brings its
// schema up to date automatically via goose migrations.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection serializes writes, so none waits on SQLite's lock.
	db.SetMaxOpenConns(1)
	if err := controllermigrations.Migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	// The database holds write-only git credentials; keep it readable by the
	// controller's user alone even if the state directory is moved.
	if path != "" && path != ":memory:" {
		_ = os.Chmod(path, 0o600)
	}
	return &Store{db: db, q: controllerdb.New(db)}, nil
}

func (s *Store) Close() error { return s.db.Close() }

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
	if err := s.q.UpsertNode(ctx, controllerdb.UpsertNodeParams{
		ID: p.NodeID, Hostname: p.Hostname, Login: p.Login, Addr: addr, JoinedAt: now.UnixMilli(),
	}); err != nil {
		return nil, fmt.Errorf("record join of %s: %w", p.Hostname, err)
	}
	n, err := s.node(ctx, p.NodeID)
	if err != nil {
		return nil, err
	}
	return n.node, nil
}

// Prune drops samples, history, events, commands and scheduled runs past
// their retention.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	history := now.Add(-keepHistory).UnixMilli()
	for _, fn := range []func() error{
		func() error { return s.q.PruneSamples(ctx, now.Add(-keepSamples).UnixMilli()) },
		func() error { return s.q.PruneHistory(ctx, history) },
		func() error { return s.q.PruneCommandProfiles(ctx, history) },
		func() error { return s.q.PruneCommandDiagnostics(ctx, history) },
		func() error { return s.q.PruneEvents(ctx, keepEvents) },
		func() error { return s.q.PruneCommands(ctx, history) },
		func() error { return s.q.PruneScheduleRuns(ctx, keepRuns) },
		func() error { return s.q.PrunePlacements(ctx, now.Add(-keepPlacements).UnixMilli()) },
	} {
		if err := fn(); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	s.samplesGen.Add(1)
	return nil
}

// nodeRow is a node with its latest heartbeat.
type nodeRow struct {
	node *pb.Node
	addr string               // tailnet address; empty when the tailnet gave none
	hb   *pb.HeartbeatRequest // nil before the first heartbeat
}

// RetireNode marks the node with ID id gone for good, at at: Nodes leaves
// it out from then on, and its history stays.
func (s *Store) RetireNode(ctx context.Context, id string, at time.Time) error {
	if err := s.q.RetireNode(ctx, controllerdb.RetireNodeParams{RetiredAt: sql.NullInt64{Int64: at.UnixMilli(), Valid: true}, ID: id}); err != nil {
		return fmt.Errorf("retire node %s: %w", id, err)
	}
	return nil
}

// Nodes lists every worker not retired, by host name. Online is left
// false; the server decides it from last_seen.
func (s *Store) Nodes(ctx context.Context) ([]nodeRow, error) {
	rows, err := s.q.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	nodes := make([]nodeRow, 0, len(rows))
	for _, r := range rows {
		n, err := toNodeRow(r.ID, r.Hostname, r.Login, r.Addr, r.JoinedAt, r.LastSeen, r.Heartbeat)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// HasNode reports whether nodeID has joined the cluster.
func (s *Store) HasNode(ctx context.Context, nodeID string) (bool, error) {
	_, err := s.q.NodeExists(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) node(ctx context.Context, id string) (nodeRow, error) {
	r, err := s.q.GetNode(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nodeRow{}, api.Errorf(api.CodeNotFound, "no node %s", id)
	}
	if err != nil {
		return nodeRow{}, err
	}
	return toNodeRow(r.ID, r.Hostname, r.Login, r.Addr, r.JoinedAt, r.LastSeen, r.Heartbeat)
}

func toNodeRow(id, hostname, login, addr string, joinedAt int64, lastSeen sql.NullInt64, hb sql.NullString) (nodeRow, error) {
	n := &pb.Node{Id: id, Hostname: hostname, Login: login, JoinedAt: joinedAt}
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
		n.ReservedMemoryBytes, n.Storage, n.Profiler, n.DaemonLogs, n.Admission = h.ReservedMemoryBytes, h.Storage, h.Profiler, h.DaemonLogs, h.Admission
		n.StorageError = h.StorageError
	}
	return out, nil
}

// PutRepo adds or replaces a repo.
func (s *Store) PutRepo(ctx context.Context, r *pb.Repo) error {
	doc := proto.Clone(r).(*pb.Repo)
	doc.Reminder = nil
	text, err := marshal(doc)
	if err != nil {
		return err
	}
	return s.q.UpsertRepo(ctx, controllerdb.UpsertRepoParams{ID: r.Id, Doc: text})
}

// DeleteRepo removes a repo; workers stop keeping its bases.
func (s *Store) DeleteRepo(ctx context.Context, id string) (bool, error) {
	n, err := s.q.DeleteRepo(ctx, id)
	return n > 0, err
}

// Repos lists every repo by ID.
func (s *Store) Repos(ctx context.Context) ([]*pb.Repo, error) {
	docs, err := s.q.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Repo, 0, len(docs))
	for _, doc := range docs {
		r, err := unmarshal(doc, &pb.Repo{})
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// StartSession records a new session in a project; its name must be free.
func (s *Store) StartSession(ctx context.Context, id, agent, project, t3codeThreadID string, now time.Time) (*pb.Session, error) {
	pid, err := projectID(ctx, s.q, project)
	if err != nil {
		return nil, err
	}
	err = s.q.InsertSession(ctx, controllerdb.InsertSessionParams{
		ID: id, Agent: agent, ProjectID: pid, T3codeThreadID: t3codeThreadID, StartedAt: now.UnixMilli(), LastActiveAt: now.UnixMilli(),
	})
	if err != nil {
		if _, gerr := s.Session(ctx, id); gerr == nil {
			return nil, api.Errorf(api.CodeConflict, "a session called %s exists", id).WithSuggestion("pick another name, or leave it out for a random one")
		}
		return nil, err
	}
	return s.Session(ctx, id)
}

func sessionFromRow(id, agent, project, t3codeThreadID string, startedAt, lastActiveAt int64, endedAt sql.NullInt64, endReason string) *pb.Session {
	out := &pb.Session{
		Id: id, Agent: agent, Project: project, T3CodeThreadId: t3codeThreadID,
		StartedAt: startedAt, LastActiveAt: lastActiveAt, EndReason: endReason,
	}
	if endedAt.Valid {
		out.EndedAt = &endedAt.Int64
	}
	return out
}

// Session returns a session with its workspaces.
func (s *Store) Session(ctx context.Context, id string) (*pb.Session, error) {
	row, err := s.q.GetSession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no session %s", id).WithSuggestion("start one with hotbed session start")
	}
	if err != nil {
		return nil, err
	}
	out := sessionFromRow(row.ID, row.Agent, row.Project, row.T3codeThreadID, row.StartedAt, row.LastActiveAt, row.EndedAt, row.EndReason)
	if st, err := s.q.GetSessionScheduleStatus(ctx, id); err == nil {
		out.Schedule = st.Schedule
		out.ScheduleRunning = st.Running != 0
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	wsRows, err := s.q.ListWorkspacesBySession(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, r := range wsRows {
		w := &pb.SessionWorkspace{Id: r.ID, NodeId: r.NodeID, CreatedAt: r.CreatedAt}
		if r.ReleasedAt.Valid {
			w.ReleasedAt = &r.ReleasedAt.Int64
		} else {
			out.WorkspaceIds = append(out.WorkspaceIds, w.Id)
		}
		out.Workspaces = append(out.Workspaces, w)
	}
	return out, nil
}

// SessionProjects returns a map of session ID -> project name for live
// sessions and sessions referenced by node_state.
func (s *Store) SessionProjects(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.ListSessionProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Project
	}
	return out, nil
}

// SessionProjectsFor returns a map of session ID -> project name for the given
// session IDs only, using primary-key lookups rather than scanning sessions.
func (s *Store) SessionProjectsFor(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(ids))
	for i := 0; i < len(ids); i += 500 {
		rows, err := s.q.ListSessionProjectsByIDs(ctx, ids[i:min(i+500, len(ids))])
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ID] = r.Project
		}
	}
	return out, nil
}

// RecentSessions are the live sessions and those that ended since, of
// project (or empty for every project) and optionally matching t3codeThreadID,
// the most recently active first.
func (s *Store) RecentSessions(ctx context.Context, since time.Time, project, t3codeThreadID string) ([]*pb.Session, error) {
	rows, err := s.q.ListRecentSessions(ctx, controllerdb.ListRecentSessionsParams{
		Since: since.UnixMilli(), Project: project, T3codeThreadID: t3codeThreadID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionFromRow(r.ID, r.Agent, r.Project, r.T3codeThreadID, r.StartedAt, r.LastActiveAt, r.EndedAt, r.EndReason))
	}
	return out, s.hydrateSessions(ctx, out)
}

// SearchSessionsByT3CodeThread returns sessions persistently associated with
// t3codeThreadID (live ones first, then most recently active), optionally
// filtered by project and liveOnly, up to limit (0 for all), plus the total
// matching count.
func (s *Store) SearchSessionsByT3CodeThread(ctx context.Context, t3codeThreadID, project string, liveOnly bool, limit int) ([]*pb.Session, int, error) {
	lim := int64(limit)
	if limit == 0 {
		lim = -1 // SQLite's LIMIT for none
	}
	var liveFlag int64
	if liveOnly {
		liveFlag = 1
	}
	rows, err := s.q.SearchSessionsByT3CodeThread(ctx, controllerdb.SearchSessionsByT3CodeThreadParams{
		T3codeThreadID: t3codeThreadID, LiveOnly: liveFlag, Project: project, Lim: lim,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := s.q.CountSessionsByT3CodeThread(ctx, controllerdb.CountSessionsByT3CodeThreadParams{
		T3codeThreadID: t3codeThreadID, LiveOnly: liveFlag, Project: project,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]*pb.Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionFromRow(r.ID, r.Agent, r.Project, r.T3codeThreadID, r.StartedAt, r.LastActiveAt, r.EndedAt, r.EndReason))
	}
	if err := s.hydrateSessions(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, int(total), nil
}

// SetSessionT3CodeThreadIDIfEmpty associates a T3 Code thread ID with a
// session only if it does not already have one.
func (s *Store) SetSessionT3CodeThreadIDIfEmpty(ctx context.Context, id, t3codeThreadID string) error {
	return s.q.SetSessionT3CodeThreadIDIfEmpty(ctx, controllerdb.SetSessionT3CodeThreadIDIfEmptyParams{
		T3codeThreadID: t3codeThreadID, ID: id,
	})
}

func (s *Store) hydrateSessions(ctx context.Context, sessions []*pb.Session) error {
	if len(sessions) == 0 {
		return nil
	}
	byID := make(map[string]*pb.Session, len(sessions))
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		byID[sess.Id] = sess
		ids[i] = sess.Id
	}
	seenSched := make(map[string]bool, len(sessions))
	for i := 0; i < len(ids); i += 500 {
		chunk := ids[i:min(i+500, len(ids))]
		schedRows, err := s.q.ListScheduleStatusBySessions(ctx, chunk)
		if err != nil {
			return err
		}
		for _, r := range schedRows {
			if seenSched[r.SessionID] {
				continue
			}
			seenSched[r.SessionID] = true
			if sess := byID[r.SessionID]; sess != nil {
				sess.Schedule = r.Schedule
				sess.ScheduleRunning = r.Running != 0
			}
		}
		wsRows, err := s.q.ListWorkspacesBySessions(ctx, chunk)
		if err != nil {
			return err
		}
		for _, r := range wsRows {
			sess := byID[r.SessionID]
			if sess == nil {
				continue
			}
			w := &pb.SessionWorkspace{Id: r.ID, NodeId: r.NodeID, CreatedAt: r.CreatedAt}
			if r.ReleasedAt.Valid {
				w.ReleasedAt = &r.ReleasedAt.Int64
			} else {
				sess.WorkspaceIds = append(sess.WorkspaceIds, w.Id)
			}
			sess.Workspaces = append(sess.Workspaces, w)
		}
	}
	return nil
}

// LiveSession returns a session that has not ended, and marks it active.
func (s *Store) LiveSession(ctx context.Context, id string, now time.Time) (*pb.Session, error) {
	sess, err := s.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.EndedAt != nil {
		return nil, sessionEndedError(id, sess.EndReason)
	}
	n, err := s.q.TouchSession(ctx, controllerdb.TouchSessionParams{LastActiveAt: now.UnixMilli(), ID: id})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		sess, err = s.Session(ctx, id)
		if err != nil {
			return nil, err
		}
		if sess.EndedAt != nil {
			return nil, sessionEndedError(id, sess.EndReason)
		}
		// Session was ended and resumed between our read and touch; try once more.
		n, err = s.q.TouchSession(ctx, controllerdb.TouchSessionParams{LastActiveAt: now.UnixMilli(), ID: id})
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, sessionEndedError(id, "")
		}
	}
	sess.LastActiveAt = now.UnixMilli()
	return sess, nil
}

func sessionEndedError(id, reason string) error {
	msg := fmt.Sprintf("session %s ended", id)
	if reason != "" {
		msg += ": " + reason
	}
	return api.Errorf(api.CodeNotFound, "%s", msg).WithSuggestion("start a new one with hotbed session start")
}

// ResumeSession makes an ended session live again, and active now.
func (s *Store) ResumeSession(ctx context.Context, id string, now time.Time) error {
	return s.q.ResumeSession(ctx, controllerdb.ResumeSessionParams{LastActiveAt: now.UnixMilli(), ID: id})
}

// Touch marks a session active, if it has not ended.
func (s *Store) Touch(ctx context.Context, id string, now time.Time) error {
	return s.q.TouchLiveSession(ctx, controllerdb.TouchLiveSessionParams{LastActiveAt: now.UnixMilli(), ID: id})
}

// EndSession marks a session ended.
func (s *Store) EndSession(ctx context.Context, id, reason string, now time.Time) error {
	return s.q.EndSession(ctx, controllerdb.EndSessionParams{
		EndedAt: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}, EndReason: reason, ID: id,
	})
}

// EndSessionAndWorkspaces atomically marks a session ended and returns the
// live workspaces it held at the moment of the transition.
func (s *Store) EndSessionAndWorkspaces(ctx context.Context, id, reason string, now time.Time) ([]workspaceRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := q.EndSession(ctx, controllerdb.EndSessionParams{
		EndedAt: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}, EndReason: reason, ID: id,
	}); err != nil {
		return nil, err
	}
	rows, err := q.ListLiveSessionWorkspaces(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out := make([]workspaceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, workspaceRow{id: r.ID, session: id, node: r.NodeID})
	}
	return out, nil
}

// EndSessionIfIdle atomically marks a session ended only if it is still live,
// has no running scheduled run, and has not been touched since before.
func (s *Store) EndSessionIfIdle(ctx context.Context, id, reason string, now, before time.Time) (bool, error) {
	n, err := s.q.EndSessionIfIdle(ctx, controllerdb.EndSessionIfIdleParams{
		EndedAt:      sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
		EndReason:    reason,
		ID:           id,
		LastActiveAt: before.UnixMilli(),
	})
	return n > 0, err
}

// EndSessionIfIdleAndWorkspaces atomically marks an idle live session ended
// and returns the live workspaces it held at the moment of the transition.
func (s *Store) EndSessionIfIdleAndWorkspaces(ctx context.Context, id, reason string, now, before time.Time) (bool, []workspaceRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	n, err := q.EndSessionIfIdle(ctx, controllerdb.EndSessionIfIdleParams{
		EndedAt:      sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
		EndReason:    reason,
		ID:           id,
		LastActiveAt: before.UnixMilli(),
	})
	if err != nil {
		return false, nil, err
	}
	if n == 0 {
		return false, nil, nil
	}
	rows, err := q.ListLiveSessionWorkspaces(ctx, id)
	if err != nil {
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	out := make([]workspaceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, workspaceRow{id: r.ID, session: id, node: r.NodeID})
	}
	return true, out, nil
}

// IdleSessions lists live sessions nothing has touched since before. A
// scheduled run's session is never idle while the run is in progress: no
// agent is there to touch it.
func (s *Store) IdleSessions(ctx context.Context, before time.Time) ([]string, error) {
	return s.q.ListIdleSessionIDs(ctx, before.UnixMilli())
}

// LiveSessions lists sessions that have not ended, for the cockpit.
func (s *Store) LiveSessions(ctx context.Context) ([]*pb.Session, error) {
	rows, err := s.q.ListLiveSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionFromRow(r.ID, r.Agent, r.Project, r.T3codeThreadID, r.StartedAt, r.LastActiveAt, r.EndedAt, r.EndReason))
	}
	return out, s.hydrateSessions(ctx, out)
}

// ReleasedWorkspaces returns the subset of ids that are marked released in SQLite.
func (s *Store) ReleasedWorkspaces(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := map[string]bool{}
	for i := 0; i < len(ids); i += 500 {
		released, err := s.q.ListReleasedWorkspaceIDsIn(ctx, ids[i:min(i+500, len(ids))])
		if err != nil {
			return nil, err
		}
		for _, id := range released {
			out[id] = true
		}
	}
	return out, nil
}

// AddWorkspace records where a workspace lives, so its calls reach its node.
func (s *Store) AddWorkspace(ctx context.Context, id, session, nodeID, agent string, now time.Time) error {
	return s.q.InsertWorkspace(ctx, controllerdb.InsertWorkspaceParams{
		ID: id, SessionID: session, NodeID: nodeID, Agent: agent, CreatedAt: now.UnixMilli(),
	})
}

// AddLiveWorkspace atomically records a workspace only if its session is still
// live (ended_at IS NULL). Returns ok=false if the session ended concurrently.
func (s *Store) AddLiveWorkspace(ctx context.Context, id, session, nodeID, agent string, now time.Time) (bool, error) {
	n, err := s.q.InsertWorkspaceIfSessionLive(ctx, controllerdb.InsertWorkspaceIfSessionLiveParams{
		ID: id, SessionID: session, NodeID: nodeID, Agent: agent, CreatedAt: now.UnixMilli(),
	})
	return n > 0, err
}

func (s *Store) DropWorkspace(ctx context.Context, id string) error {
	return s.q.DeleteWorkspace(ctx, id)
}

func (s *Store) ReleaseWorkspace(ctx context.Context, id string, now time.Time) error {
	return s.q.ReleaseWorkspace(ctx, controllerdb.ReleaseWorkspaceParams{
		ReleasedAt: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}, ID: id,
	})
}

// ReconcileMissingWorkspace cleans up a controller workspace row that an online
// worker authoritatively does not have: a row that never finished forking is
// dropped (so it never haunts session status as "unknown"); one that previously
// existed is marked released.
func (s *Store) ReconcileMissingWorkspace(ctx context.Context, id string, now time.Time) error {
	r, err := s.q.GetWorkspace(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.ReleasedAt.Valid) {
		return nil
	}
	if err != nil {
		return err
	}
	if !r.ForkMillis.Valid {
		return s.q.DeleteWorkspace(ctx, id)
	}
	return s.q.ReleaseWorkspaceIfLive(ctx, controllerdb.ReleaseWorkspaceIfLiveParams{
		ReleasedAt: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}, ID: id,
	})
}

// workspaceRow is where a workspace lives and whose it is.
type workspaceRow struct {
	id, session, node string
	forked            bool
	released          bool
}

// Workspace looks up where a workspace lives.
func (s *Store) Workspace(ctx context.Context, id string) (workspaceRow, error) {
	r, err := s.q.GetWorkspace(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return workspaceRow{id: id}, api.Errorf(api.CodeNotFound, "no workspace %s", id)
	}
	if err != nil {
		return workspaceRow{id: id}, err
	}
	return workspaceRow{id: id, session: r.SessionID, node: r.NodeID, forked: r.ForkMillis.Valid, released: r.ReleasedAt.Valid}, nil
}

// SessionWorkspaces are a session's live workspaces, the oldest first: at
// most one on each node.
func (s *Store) SessionWorkspaces(ctx context.Context, session string) ([]workspaceRow, error) {
	rows, err := s.q.ListLiveSessionWorkspaces(ctx, session)
	if err != nil {
		return nil, err
	}
	out := make([]workspaceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, workspaceRow{id: r.ID, session: session, node: r.NodeID})
	}
	return out, nil
}

// AddCommand records where a command runs.
func (s *Store) AddCommand(ctx context.Context, id, session, nodeID, workspace string, now time.Time) error {
	return s.q.InsertCommand(ctx, controllerdb.InsertCommandParams{
		ID: id, SessionID: session, NodeID: nodeID, WorkspaceID: workspace, CreatedAt: now.UnixMilli(),
	})
}

func (s *Store) DropCommand(ctx context.Context, id string) error {
	return s.q.DeleteCommand(ctx, id)
}

// commandRoute records where a command lives and any authoritative record
// already resolved while looking it up, so GetCommand does not repeat the
// same SQLite queries when falling back.
type commandRoute struct {
	session     string
	node        string
	base        *pb.Base
	baseChecked bool
	histChecked bool
}

// lookupCommand resolves a command's session and node from commands, history,
// or node_state bases, preserving any base found so callers need not query
// BaseByCommand again.
func (s *Store) lookupCommand(ctx context.Context, id string) (commandRoute, error) {
	r, err := s.q.GetCommand(ctx, id)
	if err == nil {
		return commandRoute{session: r.SessionID, node: r.NodeID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return commandRoute{}, err
	}
	// A base's freshen belongs to no session; history knows its node.
	node, err := s.q.GetHistoryNode(ctx, id)
	if err == nil {
		return commandRoute{node: node}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return commandRoute{}, err
	}
	// A base in node_state may still name its freshen command after
	// commands/history pruned or missed it (board 7951).
	node, base, ok, err := s.BaseByCommand(ctx, id)
	if err != nil {
		return commandRoute{}, err
	}
	if ok {
		return commandRoute{node: node, base: base, baseChecked: true, histChecked: true}, nil
	}
	return commandRoute{baseChecked: true, histChecked: true}, api.Errorf(api.CodeNotFound, "no command %s", id)
}

// Command looks up the session and node of a command.
func (s *Store) Command(ctx context.Context, id string) (session, node string, err error) {
	r, err := s.lookupCommand(ctx, id)
	if err != nil {
		return "", "", err
	}
	return r.session, r.node, nil
}

// ViewCommand reads a single command from node_state without scanning the
// whole cluster view.
func (s *Store) ViewCommand(ctx context.Context, id string) (nodeID string, cmd *pb.Command, ok bool, err error) {
	if id == "" {
		return "", nil, false, nil
	}
	row, err := s.q.GetCommandStateDoc(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	cmd, err = unmarshal(row.Doc, &pb.Command{})
	if err != nil {
		return "", nil, false, fmt.Errorf("read command %s state: %w", id, err)
	}
	return row.NodeID, cmd, true, nil
}

// BaseByCommand finds the node and base in node_state whose freshen command is id.
func (s *Store) BaseByCommand(ctx context.Context, id string) (nodeID string, base *pb.Base, ok bool, err error) {
	if id == "" {
		return "", nil, false, nil
	}
	row, err := s.q.GetBaseByCommand(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	base, err = unmarshal(row.Doc, &pb.Base{})
	if err != nil {
		return "", nil, false, fmt.Errorf("read base for command %s: %w", id, err)
	}
	return row.NodeID, base, true, nil
}

// Finished is a command that ended, as history keeps it; ok is false
// while it has not (or its worker has not said so yet).
func (s *Store) Finished(ctx context.Context, id string) (f *pb.FinishedCommand, ok bool, err error) {
	row, err := s.q.GetHistoryDoc(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if f, err = finishedOf(row.Doc, row.Project); err != nil {
		return nil, false, err
	}
	return f, true, nil
}

// CommandProfile returns a finished command's persisted continuous eBPF profile,
// or (nil, false, nil) if none was recorded.
func (s *Store) CommandProfile(ctx context.Context, id string) (*pb.CommandProfile, bool, error) {
	raw, err := s.q.GetCommandProfile(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	p, err := profiler.DecodeProfile(raw)
	if err != nil {
		return nil, false, fmt.Errorf("command %s's profile: %w", id, err)
	}
	return p, true, nil
}

func addCommandProfile(ctx context.Context, q *controllerdb.Queries, nodeID string, endedAt int64, p *pb.CommandProfile) error {
	if p == nil || p.CommandId == "" {
		return nil
	}
	// A profile this build cannot read goes without the event that carried
	// it, which still applies: refusing the event would stop the node's
	// stream at it for good.
	if err := profiler.Admit(p); err != nil {
		slog.Error("dropping a finished command's profile this controller cannot read", "node", nodeID, "command", p.CommandId, "err", err)
		return nil
	}
	doc, err := profiler.EncodeProfile(p)
	if err != nil {
		return err
	}
	if endedAt == 0 {
		endedAt = p.UpdatedAt
	}
	if err := q.UpsertCommandProfile(ctx, controllerdb.UpsertCommandProfileParams{
		ID: p.CommandId, NodeID: nodeID, EndedAt: endedAt, Doc: doc,
	}); err != nil {
		return fmt.Errorf("record command profile: %w", err)
	}
	return nil
}

// SaveCommandDiagnostics persists one intervention's diagnostics idempotently.
func (s *Store) SaveCommandDiagnostics(ctx context.Context, nodeID string, d *pb.CommandDiagnostics) error {
	return addCommandDiagnostics(ctx, s.q, nodeID, time.Now().UnixMilli(), d)
}

// CommandDiagnostics returns the diagnostics taken of a command as someone
// stopped or paused it by hand, oldest first.
func (s *Store) CommandDiagnostics(ctx context.Context, id string) ([]*pb.CommandDiagnostics, error) {
	rows, err := s.q.ListCommandDiagnostics(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.CommandDiagnostics, 0, len(rows))
	for _, doc := range rows {
		d, err := unmarshal(doc, &pb.CommandDiagnostics{})
		if err != nil {
			return nil, fmt.Errorf("read diagnostics of command %s: %w", id, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// CommandDiagnostic returns the diagnostics report for (id, seq), or
// (nil, false, nil) if none exists.
func (s *Store) CommandDiagnostic(ctx context.Context, id string, seq int32) (*pb.CommandDiagnostics, bool, error) {
	doc, err := s.q.GetCommandDiagnosticBySeq(ctx, controllerdb.GetCommandDiagnosticBySeqParams{
		CommandID: id,
		Seq:       int64(seq),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	d, err := unmarshal(doc, &pb.CommandDiagnostics{})
	if err != nil {
		return nil, false, fmt.Errorf("read diagnostics of command %s seq %d: %w", id, seq, err)
	}
	return d, true, nil
}

// CommandDiagnosticsAfter returns diagnostics for id with seq > afterSeq,
// oldest first.
func (s *Store) CommandDiagnosticsAfter(ctx context.Context, id string, afterSeq int32) ([]*pb.CommandDiagnostics, error) {
	rows, err := s.q.ListCommandDiagnosticsAfter(ctx, controllerdb.ListCommandDiagnosticsAfterParams{
		CommandID: id,
		Seq:       int64(afterSeq),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*pb.CommandDiagnostics, 0, len(rows))
	for _, doc := range rows {
		d, err := unmarshal(doc, &pb.CommandDiagnostics{})
		if err != nil {
			return nil, fmt.Errorf("read diagnostics of command %s: %w", id, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// HasCommandDiagnostics reports whether any diagnostics exist for command id.
func (s *Store) HasCommandDiagnostics(ctx context.Context, id string) (bool, error) {
	n, err := s.q.HasCommandDiagnostics(ctx, id)
	if err != nil {
		return false, err
	}
	return n != 0, nil
}

func addCommandDiagnostics(ctx context.Context, q *controllerdb.Queries, nodeID string, eventAt int64, d *pb.CommandDiagnostics) error {
	if d == nil || d.CommandId == "" {
		return nil
	}
	doc, err := marshal(d)
	if err != nil {
		return err
	}
	iv := d.GetIntervention()
	createdAt := iv.GetBy().GetAt()
	if createdAt == 0 {
		createdAt = eventAt
	}
	if err := q.InsertCommandDiagnostics(ctx, controllerdb.InsertCommandDiagnosticsParams{
		CommandID: d.CommandId,
		Seq:       int64(iv.GetSeq()),
		NodeID:    nodeID,
		CreatedAt: createdAt,
		Doc:       doc,
	}); err != nil {
		return fmt.Errorf("record command diagnostics: %w", err)
	}
	return nil
}

// History lists commands that ended since since, oldest first.
func (s *Store) History(ctx context.Context, since time.Time) ([]*pb.FinishedCommand, error) {
	rows, err := s.q.ListHistorySince(ctx, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make([]*pb.FinishedCommand, 0, len(rows))
	for _, r := range rows {
		f, err := finishedOf(r.Doc, r.Project)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// HistoryFilter picks commands from history; an empty field matches every
// command.
type HistoryFilter struct {
	Project, Session string
	// Text the command line holds, as written.
	Match string
}

// HistoryMatching lists the newest commands history keeps that f picks,
// newest first, at most limit of them (0 for every one), and how many it
// keeps in all.
func (s *Store) HistoryMatching(ctx context.Context, f HistoryFilter, limit int) (newest []*pb.FinishedCommand, total int, err error) {
	lim := int64(limit)
	if limit == 0 {
		lim = -1 // SQLite's LIMIT for none
	}
	rows, err := s.q.ListHistoryMatching(ctx, controllerdb.ListHistoryMatchingParams{
		Project: f.Project, SessionID: f.Session, Match: f.Match, Lim: lim,
	})
	if err != nil {
		return nil, 0, err
	}
	n, err := s.q.CountHistoryMatching(ctx, controllerdb.CountHistoryMatchingParams{
		Project: f.Project, SessionID: f.Session, Match: f.Match,
	})
	if err != nil {
		return nil, 0, err
	}
	newest = make([]*pb.FinishedCommand, 0, len(rows))
	for _, r := range rows {
		c, err := finishedOf(r.Doc, r.Project)
		if err != nil {
			return nil, 0, err
		}
		newest = append(newest, c)
	}
	return newest, int(n), nil
}

// InHistory says which of ids history keeps.
func (s *Store) InHistory(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	kept, err := s.q.ListHistoryIDsIn(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(kept))
	for _, id := range kept {
		out[id] = true
	}
	return out, nil
}

// finishedOf is a history row's command: its doc, with the project its
// row names (the doc does not keep it).
func finishedOf(doc, project string) (*pb.FinishedCommand, error) {
	f, err := unmarshal(doc, &pb.FinishedCommand{})
	if err != nil {
		return nil, err
	}
	if project != "" {
		f.Project = project
	}
	return f, nil
}

// Events lists the newest n events, newest first.
func (s *Store) Events(ctx context.Context, n int) ([]*pb.ClusterEvent, error) {
	rows, err := s.q.ListRecentEvents(ctx, int64(n))
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ClusterEvent, 0, len(rows))
	for _, r := range rows {
		e := &pb.ClusterEvent{Id: r.ID, At: r.At, Kind: r.Kind, NodeId: r.NodeID, Text: r.Text}
		if r.RefType != "" {
			e.Ref = &pb.EventRef{Type: r.RefType, Id: r.RefID}
		}
		out = append(out, e)
	}
	return out, nil
}

type sampleRow struct {
	node                    string
	t                       int64
	cpu, memory, psi        float64
	cpuPsi                  float64
	holding                 bool
	running, queued, parked int
	oom                     int64
}

// AddPlacement records a placement decision made at now.
func (s *Store) AddPlacement(ctx context.Context, d *pb.PlacementDecision, now time.Time) error {
	d.At = now.UnixMilli()
	doc, err := marshal(d)
	if err != nil {
		return err
	}
	id, err := s.q.InsertPlacement(ctx, controllerdb.InsertPlacementParams{At: d.At, SessionID: d.SessionId, Doc: doc})
	if err != nil {
		return fmt.Errorf("record placement: %w", err)
	}
	d.Id = id
	return nil
}

// Placements lists the placement decisions made since since, newest first.
func (s *Store) Placements(ctx context.Context, since time.Time) ([]*pb.PlacementDecision, error) {
	rows, err := s.q.ListPlacementsSince(ctx, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make([]*pb.PlacementDecision, 0, len(rows))
	for _, r := range rows {
		d, err := unmarshal(r.Doc, &pb.PlacementDecision{})
		if err != nil {
			return nil, fmt.Errorf("read placement %d: %w", r.ID, err)
		}
		d.Id, d.At = r.ID, r.At
		out = append(out, d)
	}
	return out, nil
}

// Samples lists samples since since, oldest first.
func (s *Store) Samples(ctx context.Context, since time.Time) ([]sampleRow, error) {
	rows, err := s.q.ListSamplesSince(ctx, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make([]sampleRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, sampleRow{
			node: r.NodeID, t: r.T, cpu: r.Cpu, memory: r.Memory, psi: r.Psi, cpuPsi: r.CpuPsi, holding: r.Holding != 0,
			running: int(r.Running), queued: int(r.Queued), parked: int(r.Parked), oom: r.Oom,
		})
	}
	return out, nil
}

type forkRow struct {
	at     int64
	millis int64
}

// Forks lists workspaces created since since.
func (s *Store) Forks(ctx context.Context, since time.Time) ([]forkRow, error) {
	rows, err := s.q.ListForksSince(ctx, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make([]forkRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, forkRow{at: r.CreatedAt, millis: r.ForkMillis})
	}
	return out, nil
}
