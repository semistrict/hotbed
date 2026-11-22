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
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	controllermigrations "github.com/semistrict/hotbed/internal/controller/db/migrations"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// errNotJoined is a report from a node that never joined.
var errNotJoined = errors.New("node has not joined")

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
	q  *controllerdb.Queries
}

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
	q := s.q.WithTx(tx)
	acked, err := q.GetNodeAcked(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, errNotJoined
	} else if err != nil {
		return out, err
	}
	if err := replaceView(ctx, q, nodeID, r.Bases, r.Workspaces, r.Commands); err != nil {
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
		if err := q.UpdateWorkspaceForkMillis(ctx, controllerdb.UpdateWorkspaceForkMillisParams{
			ForkMillis: sql.NullInt64{Int64: w.ForkMillis, Valid: true}, ID: w.Id,
		}); err != nil {
			return out, err
		}
	}
	if err := beat(ctx, q, nodeID, hb, now); err != nil {
		return out, err
	}
	high := acked
	for _, f := range r.Finished {
		high = max(high, f.Seq)
		if f.Seq <= acked {
			continue
		}
		if err := addHistory(ctx, q, nodeID, f.Item); err != nil {
			return out, err
		}
	}
	for _, e := range r.Events {
		high = max(high, e.Seq)
		if e.Seq <= acked {
			continue
		}
		if err := addFeed(ctx, q, nodeID, e.Item); err != nil {
			return out, err
		}
	}
	for _, e := range r.SessionEvents {
		high = max(high, e.Seq)
		if e.Seq <= acked {
			continue
		}
		if err := addSessionEvent(ctx, q, e.Item, &out); err != nil {
			return out, err
		}
	}
	if err := q.UpdateNodeAcked(ctx, controllerdb.UpdateNodeAckedParams{Acked: high, ID: nodeID}); err != nil {
		return out, err
	}
	out.applied = high
	return out, tx.Commit()
}

// Prune drops samples, history, events, commands and scheduled runs past
// their retention.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	history := now.Add(-keepHistory).UnixMilli()
	for _, fn := range []func() error{
		func() error { return s.q.PruneSamples(ctx, now.Add(-keepSamples).UnixMilli()) },
		func() error { return s.q.PruneHistory(ctx, history) },
		func() error { return s.q.PruneEvents(ctx, keepEvents) },
		func() error { return s.q.PruneCommands(ctx, history) },
		func() error { return s.q.PruneScheduleRuns(ctx, keepRuns) },
	} {
		if err := fn(); err != nil {
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
	// pushes says the worker pushes events: it is new enough to take
	// clients' calls itself. Older ones admit only the controller.
	pushes bool
}

// Nodes lists every worker by host name. Online is left false; the server
// decides it from last_seen.
func (s *Store) Nodes(ctx context.Context) ([]nodeRow, error) {
	rows, err := s.q.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	nodes := make([]nodeRow, 0, len(rows))
	for _, r := range rows {
		n, err := toNodeRow(r.ID, r.Hostname, r.Login, r.Addr, r.JoinedAt, r.LastSeen, r.Heartbeat, r.Pushes != 0)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func (s *Store) node(ctx context.Context, id string) (nodeRow, error) {
	r, err := s.q.GetNode(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nodeRow{}, api.Errorf(api.CodeNotFound, "no node %s", id)
	}
	if err != nil {
		return nodeRow{}, err
	}
	return toNodeRow(r.ID, r.Hostname, r.Login, r.Addr, r.JoinedAt, r.LastSeen, r.Heartbeat, r.Pushes != 0)
}

func toNodeRow(id, hostname, login, addr string, joinedAt int64, lastSeen sql.NullInt64, hb sql.NullString, pushes bool) (nodeRow, error) {
	n := &pb.Node{Id: id, Hostname: hostname, Login: login, JoinedAt: joinedAt}
	out := nodeRow{node: n, addr: addr, pushes: pushes}
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
	return s.q.UpsertRepo(ctx, controllerdb.UpsertRepoParams{ID: r.Id, Doc: doc})
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
func (s *Store) StartSession(ctx context.Context, id, agent, project string, now time.Time) (*pb.Session, error) {
	pid, err := projectID(ctx, s.q, project)
	if err != nil {
		return nil, err
	}
	err = s.q.InsertSession(ctx, controllerdb.InsertSessionParams{
		ID: id, Agent: agent, ProjectID: pid, StartedAt: now.UnixMilli(), LastActiveAt: now.UnixMilli(),
	})
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
	row, err := s.q.GetSession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.Errorf(api.CodeNotFound, "no session %s", id).WithSuggestion("start one with hotbed session start")
	}
	if err != nil {
		return nil, err
	}
	out := &pb.Session{
		Id: row.ID, Agent: row.Agent, Project: row.Project,
		StartedAt: row.StartedAt, LastActiveAt: row.LastActiveAt, EndReason: row.EndReason,
	}
	if row.EndedAt.Valid {
		out.EndedAt = &row.EndedAt.Int64
	}
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
		w := &pb.SessionWorkspace{Id: r.ID, RepoId: r.RepoID, NodeId: r.NodeID, CreatedAt: r.CreatedAt}
		if r.ReleasedAt.Valid {
			w.ReleasedAt = &r.ReleasedAt.Int64
		} else {
			out.WorkspaceIds = append(out.WorkspaceIds, w.Id)
		}
		out.Workspaces = append(out.Workspaces, w)
	}
	return out, nil
}

// SessionProjects returns a map of session ID -> project name for all sessions.
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

// RecentSessions are the live sessions and those that ended since, of
// project or, empty, of every project, the most recently active first.
func (s *Store) RecentSessions(ctx context.Context, since time.Time, project string) ([]*pb.Session, error) {
	ids, err := s.q.ListRecentSessionIDs(ctx, controllerdb.ListRecentSessionIDsParams{
		Since: since.UnixMilli(), Project: project,
	})
	if err != nil {
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
	if err := s.q.TouchSession(ctx, controllerdb.TouchSessionParams{LastActiveAt: now.UnixMilli(), ID: id}); err != nil {
		return nil, err
	}
	sess.LastActiveAt = now.UnixMilli()
	return sess, nil
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

// IdleSessions lists live sessions nothing has touched since before. A
// scheduled run's session is never idle while the run is in progress: no
// agent is there to touch it.
func (s *Store) IdleSessions(ctx context.Context, before time.Time) ([]string, error) {
	return s.q.ListIdleSessionIDs(ctx, before.UnixMilli())
}

// LiveSessions lists sessions that have not ended, for the cockpit.
func (s *Store) LiveSessions(ctx context.Context) ([]*pb.Session, error) {
	ids, err := s.q.ListLiveSessionIDs(ctx)
	if err != nil {
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
	rows, err := s.q.ListSessionEventsAfter(ctx, controllerdb.ListSessionEventsAfterParams{SessionID: session, Seq: after})
	if err != nil {
		return nil, err
	}
	out := make([]*pb.SessionEvent, 0, len(rows))
	for _, r := range rows {
		ev, err := unmarshal(r.Doc, &pb.SessionEvent{})
		if err != nil {
			return nil, err
		}
		ev.Seq = r.Seq
		out = append(out, ev)
	}
	return out, nil
}

// AddWorkspace records where a workspace lives, so its calls reach its node.
func (s *Store) AddWorkspace(ctx context.Context, id, session, nodeID, repo, agent string, now time.Time) error {
	return s.q.InsertWorkspace(ctx, controllerdb.InsertWorkspaceParams{
		ID: id, SessionID: session, NodeID: nodeID, RepoID: repo, Agent: agent, CreatedAt: now.UnixMilli(),
	})
}

func (s *Store) DropWorkspace(ctx context.Context, id string) error {
	return s.q.DeleteWorkspace(ctx, id)
}

func (s *Store) ReleaseWorkspace(ctx context.Context, id string, now time.Time) error {
	return s.q.ReleaseWorkspace(ctx, controllerdb.ReleaseWorkspaceParams{
		ReleasedAt: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}, ID: id,
	})
}

// workspaceRow is where a workspace lives and whose it is.
type workspaceRow struct {
	id, session, node, repo string
	released                bool
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
	return workspaceRow{id: id, session: r.SessionID, node: r.NodeID, repo: r.RepoID, released: r.ReleasedAt.Valid}, nil
}

// SessionWorkspace finds a session's live workspace of a repo.
func (s *Store) SessionWorkspace(ctx context.Context, session, repo string) (workspaceRow, bool, error) {
	id, err := s.q.GetLiveSessionWorkspaceID(ctx, controllerdb.GetLiveSessionWorkspaceIDParams{SessionID: session, RepoID: repo})
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
	return s.q.InsertCommand(ctx, controllerdb.InsertCommandParams{
		ID: id, SessionID: session, NodeID: nodeID, WorkspaceID: workspace, CreatedAt: now.UnixMilli(),
	})
}

func (s *Store) DropCommand(ctx context.Context, id string) error {
	return s.q.DeleteCommand(ctx, id)
}

// Command looks up the session and node of a command.
func (s *Store) Command(ctx context.Context, id string) (session, node string, err error) {
	r, err := s.q.GetCommand(ctx, id)
	if err == nil {
		return r.SessionID, r.NodeID, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		// A base's freshen belongs to no session; history knows its node.
		node, err = s.q.GetHistoryNode(ctx, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", api.Errorf(api.CodeNotFound, "no command %s", id)
	}
	return "", node, err
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
	f, err = unmarshal(row.Doc, &pb.FinishedCommand{})
	if err != nil {
		return nil, false, err
	}
	if row.Project != "" {
		f.Project = row.Project
	}
	return f, true, nil
}

// History lists commands that ended since since, oldest first.
func (s *Store) History(ctx context.Context, since time.Time) ([]*pb.FinishedCommand, error) {
	rows, err := s.q.ListHistorySince(ctx, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make([]*pb.FinishedCommand, 0, len(rows))
	for _, r := range rows {
		f, err := unmarshal(r.Doc, &pb.FinishedCommand{})
		if err != nil {
			return nil, err
		}
		if r.Project != "" {
			f.Project = r.Project
		}
		out = append(out, f)
	}
	return out, nil
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
	running, queued, parked int
	oom                     int64
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
			node: r.NodeID, t: r.T, cpu: r.Cpu, memory: r.Memory, psi: r.Psi,
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
