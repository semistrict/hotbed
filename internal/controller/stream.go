package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// The controller is not the truth about a worker's bases, workspaces and
// commands: the worker is, and the controller keeps a view of each node,
// folded from the events the worker pushes in order (node_state). The
// view feeds everything that spans nodes: sessions' timelines, history, the
// cockpit, placement.

// pushed is what a push changed that someone may be waiting on.
type pushed struct {
	applied          int64
	appliedAt        int64
	checkpointNeeded bool
	finished         bool            // commands ended, into history
	lost             []string        // live commands a checkpoint no longer has
	sessions         map[string]bool // sessions with new events
}

// lostDetail is what sessions hear of what a worker's checkpoint no longer
// has.
const lostDetail = "lost: its worker no longer has it (the worker's state was reset)"

// Push folds a worker's events, or its checkpoint, into its view, in one
// transaction with the offset it continues from, so each event applies
// exactly once.
func (s *Store) Push(ctx context.Context, nodeID string, req *pb.PushEventsRequest, now time.Time) (pushed, error) {
	out := pushed{sessions: map[string]bool{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var epoch string
	if err := tx.QueryRowContext(ctx, `SELECT epoch, applied, applied_at FROM nodes WHERE id = ?`, nodeID).Scan(&epoch, &out.applied, &out.appliedAt); errors.Is(err, sql.ErrNoRows) {
		return out, errNotJoined
	} else if err != nil {
		return out, err
	}
	switch {
	case req.Checkpoint != nil:
		// The events the checkpoint includes that this view may lack come
		// first, for what derives from them; then the checkpoint is the view.
		if req.Epoch != epoch {
			out.applied = 0
		}
		if err := applyEvents(ctx, tx, nodeID, req.Events, &out); err != nil {
			return out, err
		}
		if err := applyCheckpoint(ctx, tx, nodeID, req.Checkpoint, now, &out); err != nil {
			return out, err
		}
		epoch, out.applied = req.Epoch, req.Checkpoint.Seq
		if len(req.Events) == 0 || req.Events[len(req.Events)-1].Seq != req.Checkpoint.Seq {
			out.appliedAt = 0 // not known: the next push is not checked against it
		}
	case req.Epoch != epoch:
		// A new controller, or a worker whose database is new: nothing to
		// continue from.
		return pushed{checkpointNeeded: true}, nil
	case !follows(req, out):
		// A gap, or a different event at the applied one: the worker's
		// database is not the history this view continues.
		return pushed{applied: out.applied, checkpointNeeded: true}, nil
	default:
		if err := applyEvents(ctx, tx, nodeID, req.Events, &out); err != nil {
			return out, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET epoch = ?, applied = ?, applied_at = ? WHERE id = ?`, epoch, out.applied, out.appliedAt, nodeID); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// follows says whether a push's events continue from the view's applied
// event: the first new one is the next, and the worker's event at the
// applied one is the one the view applied, when both sides know its time.
func follows(req *pb.PushEventsRequest, at pushed) bool {
	for _, ev := range req.Events {
		if ev.Seq <= at.applied {
			continue // delivered before
		}
		if ev.Seq != at.applied+1 {
			return false
		}
		return req.AfterAt == 0 || at.appliedAt == 0 || req.AfterAt == at.appliedAt
	}
	return true
}

// applyEvents applies the events after out.applied, in order.
func applyEvents(ctx context.Context, tx *sql.Tx, nodeID string, events []*pb.Event, out *pushed) error {
	for _, ev := range events {
		if ev.Seq <= out.applied {
			continue // delivered before
		}
		if err := applyEvent(ctx, tx, nodeID, ev, out); err != nil {
			return fmt.Errorf("apply event %d: %w", ev.Seq, err)
		}
		if c, ok := ev.Change.(*pb.Event_Command); ok && c.Command.Finished != nil {
			out.finished = true
		}
		out.applied, out.appliedAt = ev.Seq, ev.At
	}
	return nil
}

// applyEvent folds one event into the node's view and what derives from
// it: the session's timeline, history, the feed. A change this controller
// does not know (a newer worker's) is passed over.
func applyEvent(ctx context.Context, tx *sql.Tx, nodeID string, ev *pb.Event, out *pushed) error {
	if ev.Feed != nil {
		if err := addFeed(ctx, tx, nodeID, ev.Feed); err != nil {
			return err
		}
	}
	switch c := ev.Change.(type) {
	case *pb.Event_Base:
		if c.Base.What == "collected" {
			return dropState(ctx, tx, nodeID, "base", c.Base.Base.Id)
		}
		return putState(ctx, tx, nodeID, "base", c.Base.Base.Id, c.Base.Base)
	case *pb.Event_Workspace:
		ws := c.Workspace.Workspace
		if c.Workspace.What == "forgotten" {
			return dropState(ctx, tx, nodeID, "workspace", ws.Id)
		}
		if err := putState(ctx, tx, nodeID, "workspace", ws.Id, ws); err != nil {
			return err
		}
		if err := noteWorkspace(ctx, tx, nodeID, ws); err != nil {
			return err
		}
		switch c.Workspace.What {
		case "ready", "failed":
			return addSessionEvent(ctx, tx, &pb.SessionEvent{At: ev.At, What: c.Workspace.What, Detail: c.Workspace.Detail, SessionId: ws.SessionId, WorkspaceId: ws.Id}, out)
		case "synced", "active":
			// A client used the workspace on its worker, perhaps directly.
			return touchAt(ctx, tx, ws.SessionId, ev.At)
		}
		return nil
	case *pb.Event_Command:
		cmd := c.Command.Command
		if c.Command.What == "forgotten" {
			return dropState(ctx, tx, nodeID, "command", cmd.Id)
		}
		if err := putState(ctx, tx, nodeID, "command", cmd.Id, cmd); err != nil {
			return err
		}
		// Where it runs, for calls about it, its freshen log included, after
		// the worker forgets it.
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			cmd.Id, cmd.SessionId, nodeID, cmd.WorkspaceId, ev.At); err != nil {
			return err
		}
		if c.Command.Finished != nil {
			if err := addHistory(ctx, tx, nodeID, c.Command.Finished); err != nil {
				return err
			}
		}
		switch c.Command.What {
		case "admitted", "parked", "thawed", "isolation lost", "exited", "cancelled":
			return addSessionEvent(ctx, tx, &pb.SessionEvent{
				At: ev.At, CommandId: cmd.Id, Argv: cmd.Argv, What: c.Command.What, State: cmd.State,
				ExitCode: cmd.ExitCode, Detail: c.Command.Detail, SessionId: cmd.SessionId, WorkspaceId: cmd.WorkspaceId,
			}, out)
		case "queued":
			return touchAt(ctx, tx, cmd.SessionId, ev.At)
		}
		return nil
	default:
		slog.Warn("passing over an event of a change this controller does not know", "node", nodeID, "seq", ev.Seq)
		return nil
	}
}

// applyCheckpoint makes a worker's checkpoint the node's view. What the
// view had and the checkpoint lacks, the worker deleted (a command forgotten
// after it ended, a workspace a while after its release) or lost with its
// database: sessions hear that their live commands and workspaces were
// lost, and those workspaces count as released.
func applyCheckpoint(ctx context.Context, tx *sql.Tx, nodeID string, cp *pb.Checkpoint, now time.Time, out *pushed) error {
	old, err := loadView(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, w := range cp.Workspaces {
		kept["w:"+w.Id] = true
	}
	for _, c := range cp.Commands {
		kept["c:"+c.Id] = true
	}
	at := now.UnixMilli()
	for _, w := range old.workspaces {
		if kept["w:"+w.Id] || w.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED || w.SessionId == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET released_at = ? WHERE id = ? AND released_at IS NULL`, at, w.Id); err != nil {
			return err
		}
		if err := addSessionEvent(ctx, tx, &pb.SessionEvent{At: at, What: "failed", Detail: lostDetail, SessionId: w.SessionId, WorkspaceId: w.Id}, out); err != nil {
			return err
		}
	}
	for _, c := range old.commands {
		if kept["c:"+c.Id] || !live(c.State) {
			continue
		}
		out.lost = append(out.lost, c.Id)
		if err := addSessionEvent(ctx, tx, &pb.SessionEvent{
			At: at, CommandId: c.Id, Argv: c.Argv, What: "cancelled", State: pb.CommandState_COMMAND_STATE_CANCELLED,
			Detail: lostDetail, SessionId: c.SessionId, WorkspaceId: c.WorkspaceId,
		}, out); err != nil {
			return err
		}
	}
	if err := replaceView(ctx, tx, nodeID, cp.Bases, cp.Workspaces, cp.Commands); err != nil {
		return err
	}
	for _, w := range cp.Workspaces {
		if err := noteWorkspace(ctx, tx, nodeID, w); err != nil {
			return err
		}
	}
	for _, c := range cp.Commands {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.Id, c.SessionId, nodeID, c.WorkspaceId, c.QueuedAt); err != nil {
			return err
		}
	}
	return nil
}

// noteWorkspace records what the controller keeps about a workspace beyond
// its worker's view: its session, how long its fork took, its release.
func noteWorkspace(ctx context.Context, tx *sql.Tx, nodeID string, ws *pb.Workspace) error {
	if ws.SessionId == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workspaces (id, session_id, node_id, repo_id, agent, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		ws.Id, ws.SessionId, nodeID, ws.RepoId, ws.Agent, ws.CreatedAt); err != nil {
		return err
	}
	if ws.ForkMillis > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET fork_millis = ? WHERE id = ?`, ws.ForkMillis, ws.Id); err != nil {
			return err
		}
	}
	if ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET released_at = ? WHERE id = ? AND released_at IS NULL`, ws.LastActiveAt, ws.Id); err != nil {
			return err
		}
	}
	return nil
}

func putState(ctx context.Context, tx *sql.Tx, nodeID, kind, id string, m proto.Message) error {
	doc, err := marshal(m)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO node_state (node_id, kind, id, doc) VALUES (?, ?, ?, ?)
		ON CONFLICT (node_id, kind, id) DO UPDATE SET doc = excluded.doc`, nodeID, kind, id, doc)
	return err
}

func dropState(ctx context.Context, tx *sql.Tx, nodeID, kind, id string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM node_state WHERE node_id = ? AND kind = ? AND id = ?`, nodeID, kind, id)
	return err
}

// replaceView makes these the node's whole view.
func replaceView(ctx context.Context, tx *sql.Tx, nodeID string, bases []*pb.Base, workspaces []*pb.Workspace, commands []*pb.Command) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_state WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	for _, b := range bases {
		if err := putState(ctx, tx, nodeID, "base", b.Id, b); err != nil {
			return err
		}
	}
	for _, w := range workspaces {
		if err := putState(ctx, tx, nodeID, "workspace", w.Id, w); err != nil {
			return err
		}
	}
	for _, c := range commands {
		if err := putState(ctx, tx, nodeID, "command", c.Id, c); err != nil {
			return err
		}
	}
	return nil
}

func addHistory(ctx context.Context, tx *sql.Tx, nodeID string, f *pb.FinishedCommand) error {
	doc, err := marshal(f)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history (id, node_id, repo_id, ended_at, doc) VALUES (?, ?, ?, ?, ?)`,
		f.Id, nodeID, f.RepoId, f.EndedAt, doc); err != nil {
		return fmt.Errorf("record finished command: %w", err)
	}
	return nil
}

func addFeed(ctx context.Context, tx *sql.Tx, nodeID string, e *pb.WorkerEvent) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, kind, node_id, text, ref_type, ref_id) VALUES (?, ?, ?, ?, ?, ?)`,
		e.At, e.Kind, nodeID, e.Text, e.GetRef().GetType(), e.GetRef().GetId()); err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

func addSessionEvent(ctx context.Context, tx *sql.Tx, e *pb.SessionEvent, out *pushed) error {
	if e.SessionId == "" {
		return nil
	}
	doc, err := marshal(e)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_events (session_id, doc) VALUES (?, ?)`, e.SessionId, doc); err != nil {
		return fmt.Errorf("record session event: %w", err)
	}
	out.sessions[e.SessionId] = true
	return nil
}

// touchAt marks a live session active as of at, unless it already is later.
func touchAt(ctx context.Context, tx *sql.Tx, session string, at int64) error {
	if session == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET last_active_at = MAX(last_active_at, ?) WHERE id = ? AND ended_at IS NULL`, at, session)
	return err
}

// Beat records a worker's heartbeat: its live usage, a sample for the
// cockpit's charts, and the sessions clients used on it.
func (s *Store) Beat(ctx context.Context, nodeID string, h *pb.HeartbeatRequest, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM nodes WHERE id = ?`, nodeID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return errNotJoined
	} else if err != nil {
		return err
	}
	if err := beat(ctx, tx, nodeID, h, now); err != nil {
		return err
	}
	for _, session := range h.ActiveSessions {
		if err := touchAt(ctx, tx, session, now.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func beat(ctx context.Context, tx *sql.Tx, nodeID string, h *pb.HeartbeatRequest, now time.Time) error {
	state := proto.Clone(h).(*pb.HeartbeatRequest)
	state.ActiveSessions = nil
	doc, err := marshal(state)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET last_seen = ?, heartbeat = ? WHERE id = ?`, now.UnixMilli(), doc, nodeID); err != nil {
		return fmt.Errorf("record heartbeat: %w", err)
	}
	v, err := loadView(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	var runningN, queuedN, parkedN int
	for _, c := range v.commands {
		switch c.State {
		case pb.CommandState_COMMAND_STATE_RUNNING:
			runningN++
		case pb.CommandState_COMMAND_STATE_QUEUED:
			queuedN++
		case pb.CommandState_COMMAND_STATE_PARKED:
			parkedN++
		}
	}
	memFrac := 0.0
	if mb := h.GetResources().GetMemoryBytes(); mb > 0 {
		memFrac = float64(h.GetUsage().GetMemoryUsedBytes()) / float64(mb)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO samples (node_id, t, cpu, memory, psi, running, queued, parked, oom) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, now.UnixMilli(), h.GetUsage().GetCpuBusy(), memFrac, h.GetUsage().GetMemoryPressure(), runningN, queuedN, parkedN, h.OomKills); err != nil {
		return fmt.Errorf("record sample: %w", err)
	}
	return nil
}

// view is a node's bases, workspaces and commands as its events left them.
type view struct {
	bases      []*pb.Base
	workspaces []*pb.Workspace
	commands   []*pb.Command
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadView reads one node's view.
func loadView(ctx context.Context, q queryer, nodeID string) (*view, error) {
	views, err := loadViews(ctx, q, `WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	if v := views[nodeID]; v != nil {
		return v, nil
	}
	return &view{}, nil
}

func loadViews(ctx context.Context, q queryer, where string, args ...any) (map[string]*view, error) {
	rows, err := q.QueryContext(ctx, `SELECT node_id, kind, doc FROM node_state `+where+` ORDER BY node_id, kind, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*view{}
	for rows.Next() {
		var node, kind, doc string
		if err := rows.Scan(&node, &kind, &doc); err != nil {
			return nil, err
		}
		v := out[node]
		if v == nil {
			v = &view{}
			out[node] = v
		}
		switch kind {
		case "base":
			b, err := unmarshal(doc, &pb.Base{})
			if err != nil {
				return nil, err
			}
			v.bases = append(v.bases, b)
		case "workspace":
			w, err := unmarshal(doc, &pb.Workspace{})
			if err != nil {
				return nil, err
			}
			v.workspaces = append(v.workspaces, w)
		case "command":
			c, err := unmarshal(doc, &pb.Command{})
			if err != nil {
				return nil, err
			}
			v.commands = append(v.commands, c)
		}
	}
	return out, rows.Err()
}

// Views reads every node's view, by node ID.
func (s *Store) Views(ctx context.Context) (map[string]*view, error) {
	return loadViews(ctx, s.db, ``)
}

// ToRelease lists the workspaces a node's view has live whose sessions
// ended: the node should release them.
func (s *Store) ToRelease(ctx context.Context, nodeID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id FROM node_state s JOIN workspaces w ON w.id = s.id
		WHERE s.node_id = ? AND s.kind = 'workspace' AND w.released_at IS NOT NULL
		AND json_extract(s.doc, '$.state') IS NOT 'WORKSPACE_STATE_RELEASED'
		ORDER BY s.id`, nodeID)
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

// NodeView is the controller's view of a worker, as a checkpoint at the last
// event it applied: what a worker's own checkpoint should equal once the
// controller has caught up.
func (s *Store) NodeView(ctx context.Context, nodeID string) (*pb.Checkpoint, error) {
	cp := &pb.Checkpoint{}
	if err := s.db.QueryRowContext(ctx, `SELECT applied FROM nodes WHERE id = ?`, nodeID).Scan(&cp.Seq); err != nil {
		return nil, err
	}
	v, err := loadView(ctx, s.db, nodeID)
	if err != nil {
		return nil, err
	}
	cp.Bases, cp.Workspaces, cp.Commands = v.bases, v.workspaces, v.commands
	return cp, nil
}
