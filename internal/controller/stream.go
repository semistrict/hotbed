package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
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
// exactly once. creating lists workspace IDs whose creation RPC is currently
// in flight on the controller so a concurrent checkpoint does not drop them.
func (s *Store) Push(ctx context.Context, nodeID string, req *pb.PushEventsRequest, now time.Time, creating map[string]bool) (pushed, error) {
	out := pushed{sessions: map[string]bool{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	st, err := q.GetNodeStreamState(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, errNotJoined
	} else if err != nil {
		return out, err
	}
	epoch := st.Epoch
	out.applied, out.appliedAt = st.Applied, st.AppliedAt
	switch {
	case req.Checkpoint != nil:
		// The events the checkpoint includes that this view may lack come
		// first, for what derives from them; then the checkpoint is the view.
		if req.Epoch != epoch {
			out.applied = 0
		}
		if err := applyEvents(ctx, q, nodeID, req.Events, &out); err != nil {
			return out, err
		}
		if err := applyCheckpoint(ctx, q, nodeID, req.Checkpoint, now, creating, &out); err != nil {
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
		if err := applyEvents(ctx, q, nodeID, req.Events, &out); err != nil {
			return out, err
		}
	}
	if err := q.UpdateNodeStreamState(ctx, controllerdb.UpdateNodeStreamStateParams{
		Epoch: epoch, Applied: out.applied, AppliedAt: out.appliedAt, ID: nodeID,
	}); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// follows says whether a push's events continue from the view's applied
// event: the first new one is the next, and the worker's event at the
// applied one is the one the view applied, when both sides know its time.
func follows(req *pb.PushEventsRequest, at pushed) bool {
	afterAt := req.AfterAt
	for _, ev := range req.Events {
		if ev.Seq <= at.applied {
			if ev.Seq == at.applied {
				afterAt = ev.At
			}
			continue // delivered before
		}
		if ev.Seq != at.applied+1 {
			return false
		}
		return afterAt == 0 || at.appliedAt == 0 || afterAt == at.appliedAt
	}
	return true
}

// applyEvents applies the events after out.applied, in order.
func applyEvents(ctx context.Context, q *controllerdb.Queries, nodeID string, events []*pb.Event, out *pushed) error {
	for _, ev := range events {
		if ev.Seq <= out.applied {
			continue // delivered before
		}
		if err := applyEvent(ctx, q, nodeID, ev, out); err != nil {
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
func applyEvent(ctx context.Context, q *controllerdb.Queries, nodeID string, ev *pb.Event, out *pushed) error {
	if ev.Feed != nil {
		if err := addFeed(ctx, q, nodeID, ev.Feed); err != nil {
			return err
		}
	}
	switch c := ev.Change.(type) {
	case *pb.Event_Base:
		if c.Base.What == "collected" {
			return dropState(ctx, q, nodeID, "base", c.Base.Base.Id)
		}
		return putState(ctx, q, nodeID, "base", c.Base.Base.Id, c.Base.Base)
	case *pb.Event_Workspace:
		ws := c.Workspace.Workspace
		if c.Workspace.What == "forgotten" {
			return dropState(ctx, q, nodeID, "workspace", ws.Id)
		}
		if err := putState(ctx, q, nodeID, "workspace", ws.Id, ws); err != nil {
			return err
		}
		if err := noteWorkspace(ctx, q, nodeID, ws); err != nil {
			return err
		}
		switch c.Workspace.What {
		case "ready", "failed":
			return addSessionEvent(ctx, q, ws.SessionId, ev.At, workspaceUpdate(workspaceWhat[c.Workspace.What], ws.Id, c.Workspace.Detail), out)
		case "synced", "active":
			// A client used the workspace on its worker, perhaps directly.
			return touchAt(ctx, q, ws.SessionId, ev.At)
		}
		return nil
	case *pb.Event_Command:
		cmd := c.Command.Command
		if c.Command.What == "forgotten" {
			return dropState(ctx, q, nodeID, "command", cmd.Id)
		}
		if err := putState(ctx, q, nodeID, "command", cmd.Id, cmd); err != nil {
			return err
		}
		// Where it runs, for calls about it, its freshen log included, after
		// the worker forgets it.
		if err := q.InsertCommandIgnore(ctx, controllerdb.InsertCommandIgnoreParams{
			ID: cmd.Id, SessionID: cmd.SessionId, NodeID: nodeID, WorkspaceID: cmd.WorkspaceId, CreatedAt: ev.At,
		}); err != nil {
			return err
		}
		if c.Command.Finished != nil {
			if err := addHistory(ctx, q, nodeID, c.Command.Finished); err != nil {
				return err
			}
		}
		if c.Command.Profile != nil {
			if err := addCommandProfile(ctx, q, nodeID, cmd.GetEndedAt(), c.Command.Profile); err != nil {
				return err
			}
		}
		if c.Command.Diagnostics != nil {
			if err := addCommandDiagnostics(ctx, q, nodeID, ev.At, c.Command.Diagnostics); err != nil {
				return err
			}
		}
		if what, ok := commandWhat[c.Command.What]; ok {
			return addSessionEvent(ctx, q, cmd.SessionId, ev.At, commandUpdate(what, cmd, c.Command.Detail), out)
		}
		if c.Command.What == "queued" {
			return touchAt(ctx, q, cmd.SessionId, ev.At)
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
func applyCheckpoint(ctx context.Context, q *controllerdb.Queries, nodeID string, cp *pb.Checkpoint, now time.Time, creating map[string]bool, out *pushed) error {
	old, err := loadView(ctx, q, nodeID)
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
	lostWS := map[string]bool{}
	for _, w := range old.workspaces {
		if kept["w:"+w.Id] || w.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED || w.SessionId == "" {
			continue
		}
		lostWS[w.Id] = true
		if err := q.ReleaseWorkspaceIfLive(ctx, controllerdb.ReleaseWorkspaceIfLiveParams{
			ReleasedAt: sql.NullInt64{Int64: at, Valid: true}, ID: w.Id,
		}); err != nil {
			return err
		}
		if err := addSessionEvent(ctx, q, w.SessionId, at, workspaceUpdate(pb.WorkspaceUpdate_WHAT_FAILED, w.Id, lostDetail), out); err != nil {
			return err
		}
	}
	// Also reconcile controller workspace rows on this node that never reached
	// node_state (e.g. from an interrupted or canceled CreateWorkspace).
	liveRows, err := q.ListLiveNodeWorkspaces(ctx, nodeID)
	if err != nil {
		return err
	}
	for _, w := range liveRows {
		if kept["w:"+w.ID] || lostWS[w.ID] || creating[w.ID] {
			continue
		}
		if !w.ForkMillis.Valid {
			if err := q.DeleteWorkspace(ctx, w.ID); err != nil {
				return err
			}
			continue
		}
		if err := q.ReleaseWorkspaceIfLive(ctx, controllerdb.ReleaseWorkspaceIfLiveParams{
			ReleasedAt: sql.NullInt64{Int64: at, Valid: true}, ID: w.ID,
		}); err != nil {
			return err
		}
	}
	for _, c := range old.commands {
		if kept["c:"+c.Id] || !api.Live(c.State) {
			continue
		}
		out.lost = append(out.lost, c.Id)
		lost := proto.Clone(c).(*pb.Command)
		lost.State = pb.CommandState_COMMAND_STATE_CANCELLED
		if err := addSessionEvent(ctx, q, c.SessionId, at, commandUpdate(pb.CommandUpdate_WHAT_CANCELLED, lost, lostDetail), out); err != nil {
			return err
		}
	}
	if err := replaceView(ctx, q, nodeID, cp.Bases, cp.Workspaces, cp.Commands); err != nil {
		return err
	}
	for _, w := range cp.Workspaces {
		if err := noteWorkspace(ctx, q, nodeID, w); err != nil {
			return err
		}
	}
	for _, c := range cp.Commands {
		if err := q.InsertCommandIgnore(ctx, controllerdb.InsertCommandIgnoreParams{
			ID: c.Id, SessionID: c.SessionId, NodeID: nodeID, WorkspaceID: c.WorkspaceId, CreatedAt: c.QueuedAt,
		}); err != nil {
			return err
		}
	}
	return nil
}

// noteWorkspace records what the controller keeps about a workspace beyond
// its worker's view: its session, how long its fork took, its release.
func noteWorkspace(ctx context.Context, q *controllerdb.Queries, nodeID string, ws *pb.Workspace) error {
	if ws.SessionId == "" {
		return nil
	}
	if err := q.InsertWorkspaceIgnore(ctx, controllerdb.InsertWorkspaceIgnoreParams{
		ID: ws.Id, SessionID: ws.SessionId, NodeID: nodeID, Agent: ws.Agent, CreatedAt: ws.CreatedAt,
	}); err != nil {
		return err
	}
	if ws.ForkMillis > 0 {
		if err := q.UpdateWorkspaceForkMillis(ctx, controllerdb.UpdateWorkspaceForkMillisParams{
			ForkMillis: sql.NullInt64{Int64: ws.ForkMillis, Valid: true}, ID: ws.Id,
		}); err != nil {
			return err
		}
	}
	if ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
		if err := q.ReleaseWorkspaceIfLive(ctx, controllerdb.ReleaseWorkspaceIfLiveParams{
			ReleasedAt: sql.NullInt64{Int64: ws.LastActiveAt, Valid: true}, ID: ws.Id,
		}); err != nil {
			return err
		}
	}
	return nil
}

func putState(ctx context.Context, q *controllerdb.Queries, nodeID, kind, id string, m proto.Message) error {
	doc, err := marshal(m)
	if err != nil {
		return err
	}
	return q.UpsertNodeState(ctx, controllerdb.UpsertNodeStateParams{NodeID: nodeID, Kind: kind, ID: id, Doc: doc})
}

func dropState(ctx context.Context, q *controllerdb.Queries, nodeID, kind, id string) error {
	return q.DeleteNodeState(ctx, controllerdb.DeleteNodeStateParams{NodeID: nodeID, Kind: kind, ID: id})
}

// replaceView makes these the node's whole view.
func replaceView(ctx context.Context, q *controllerdb.Queries, nodeID string, bases []*pb.Base, workspaces []*pb.Workspace, commands []*pb.Command) error {
	if err := q.DeleteAllNodeStateForNode(ctx, nodeID); err != nil {
		return err
	}
	for _, b := range bases {
		if err := putState(ctx, q, nodeID, "base", b.Id, b); err != nil {
			return err
		}
	}
	for _, w := range workspaces {
		if err := putState(ctx, q, nodeID, "workspace", w.Id, w); err != nil {
			return err
		}
	}
	for _, c := range commands {
		if err := putState(ctx, q, nodeID, "command", c.Id, c); err != nil {
			return err
		}
	}
	return nil
}

func addHistory(ctx context.Context, q *controllerdb.Queries, nodeID string, f *pb.FinishedCommand) error {
	var projectID sql.NullInt64
	if f.SessionId != "" {
		if pid, err := q.GetSessionProjectID(ctx, f.SessionId); err == nil {
			projectID = sql.NullInt64{Int64: pid, Valid: true}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	stored := proto.Clone(f).(*pb.FinishedCommand)
	stored.Project = ""
	doc, err := marshal(stored)
	if err != nil {
		return err
	}
	if err := q.InsertHistoryIgnore(ctx, controllerdb.InsertHistoryIgnoreParams{
		ID: f.Id, NodeID: nodeID, RepoID: f.RepoId, ProjectID: projectID, SessionID: f.SessionId, EndedAt: f.EndedAt, Doc: doc,
	}); err != nil {
		return fmt.Errorf("record finished command: %w", err)
	}
	return nil
}

func addFeed(ctx context.Context, q *controllerdb.Queries, nodeID string, e *pb.WorkerEvent) error {
	if err := q.InsertFeedEvent(ctx, controllerdb.InsertFeedEventParams{
		At: e.At, Kind: e.Kind, NodeID: nodeID, Text: e.Text, RefType: e.GetRef().GetType(), RefID: e.GetRef().GetId(),
	}); err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

// addSessionEvent adds what a worker reported to its session's events, and
// notes the session for waking.
func addSessionEvent(ctx context.Context, q *controllerdb.Queries, session string, at int64, e *pb.SessionEvent, out *pushed) error {
	added, err := recordSessionEvent(ctx, q, session, at, e)
	if added {
		out.sessions[session] = true
	}
	return err
}

// touchAt marks a live session active as of at, unless it already is later.
func touchAt(ctx context.Context, q *controllerdb.Queries, session string, at int64) error {
	if session == "" {
		return nil
	}
	return q.TouchLiveSessionMax(ctx, controllerdb.TouchLiveSessionMaxParams{At: at, ID: session})
}

// Beat records a worker's heartbeat: its live usage, a sample for the
// cockpit's charts, and the sessions clients used on it.
func (s *Store) Beat(ctx context.Context, nodeID string, h *pb.HeartbeatRequest, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if _, err := q.NodeExists(ctx, nodeID); errors.Is(err, sql.ErrNoRows) {
		return errNotJoined
	} else if err != nil {
		return err
	}
	if err := beat(ctx, q, nodeID, h, now); err != nil {
		return err
	}
	for _, session := range h.ActiveSessions {
		if err := touchAt(ctx, q, session, now.UnixMilli()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.samplesGen.Add(1)
	return nil
}

func beat(ctx context.Context, q *controllerdb.Queries, nodeID string, h *pb.HeartbeatRequest, now time.Time) error {
	state := proto.Clone(h).(*pb.HeartbeatRequest)
	state.ActiveSessions = nil
	doc, err := marshal(state)
	if err != nil {
		return err
	}
	if err := q.UpdateNodeHeartbeat(ctx, controllerdb.UpdateNodeHeartbeatParams{
		LastSeen:  sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
		Heartbeat: sql.NullString{String: doc, Valid: true},
		ID:        nodeID,
	}); err != nil {
		return fmt.Errorf("record heartbeat: %w", err)
	}
	counts, err := q.CountNodeLiveCommands(ctx, nodeID)
	if err != nil {
		return err
	}
	memFrac := 0.0
	if mb := h.GetResources().GetMemoryBytes(); mb > 0 {
		memFrac = float64(h.GetUsage().GetMemoryUsedBytes()) / float64(mb)
	}
	if err := q.UpsertSample(ctx, controllerdb.UpsertSampleParams{
		NodeID: nodeID, T: now.UnixMilli(), Cpu: h.GetUsage().GetCpuBusy(), Memory: memFrac,
		Psi: h.GetUsage().GetMemoryPressure(), CpuPsi: h.GetUsage().GetCpuPressure(), Holding: boolInt(h.GetAdmission().GetHolding()), Running: counts.Running, Queued: counts.Queued, Parked: counts.Parked, Oom: h.OomKills,
	}); err != nil {
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

// loadView reads one node's view.
func loadView(ctx context.Context, q *controllerdb.Queries, nodeID string) (*view, error) {
	rows, err := q.ListNodeStateForNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out, err := foldNodeState(rows)
	if err != nil {
		return nil, err
	}
	if v := out[nodeID]; v != nil {
		return v, nil
	}
	return &view{}, nil
}

// Views reads every node's view, by node ID.
func (s *Store) Views(ctx context.Context) (map[string]*view, error) {
	rows, err := s.q.ListAllNodeState(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]*view{}
	for _, r := range rows {
		if err := foldStateRow(out, r.NodeID, r.Kind, r.Doc); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func foldNodeState(rows []controllerdb.ListNodeStateForNodeRow) (map[string]*view, error) {
	out := map[string]*view{}
	for _, r := range rows {
		if err := foldStateRow(out, r.NodeID, r.Kind, r.Doc); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func foldStateRow(out map[string]*view, node, kind, doc string) error {
	v := out[node]
	if v == nil {
		v = &view{}
		out[node] = v
	}
	switch kind {
	case "base":
		b, err := unmarshal(doc, &pb.Base{})
		if err != nil {
			return err
		}
		v.bases = append(v.bases, b)
	case "workspace":
		w, err := unmarshal(doc, &pb.Workspace{})
		if err != nil {
			return err
		}
		v.workspaces = append(v.workspaces, w)
	case "command":
		c, err := unmarshal(doc, &pb.Command{})
		if err != nil {
			return err
		}
		v.commands = append(v.commands, c)
	}
	return nil
}

// ToRelease lists the workspaces a node's view has live whose sessions
// ended: the node should release them.
func (s *Store) ToRelease(ctx context.Context, nodeID string) ([]string, error) {
	return s.q.ListWorkspacesToRelease(ctx, nodeID)
}

// NodeView is the controller's view of a worker, as a checkpoint at the last
// event it applied: what a worker's own checkpoint should equal once the
// controller has caught up.
func (s *Store) NodeView(ctx context.Context, nodeID string) (*pb.Checkpoint, error) {
	applied, err := s.q.GetNodeApplied(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	v, err := loadView(ctx, s.q, nodeID)
	if err != nil {
		return nil, err
	}
	return &pb.Checkpoint{Seq: applied, Bases: v.bases, Workspaces: v.workspaces, Commands: v.commands}, nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
