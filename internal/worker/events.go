package worker

import (
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// change is what a save says happened, for the controller.
type change struct {
	what, detail string
	// feed is a line for the cockpit's feed, when the change is worth one.
	feed *pb.WorkerEvent
	// finished is a command's history entry, when it ends.
	finished *pb.FinishedCommand
	// profile is the command's continuous eBPF profile snapshot, when it ends.
	profile *pb.CommandProfile
	// diagnostics is the diagnostics taken before a stop or pause by hand.
	diagnostics *pb.CommandDiagnostics
}

// feedKind is what a line of the cockpit's feed says happened. Every kind
// is one of these, which the cockpit (ui/src/api/types.ts EventKind) and the
// controller's filters know by name.
type feedKind string

const (
	feedAdmit   feedKind = "admit"   // a command started
	feedWait    feedKind = "wait"    // a queued command was made to wait, and why
	feedHold    feedKind = "hold"    // the node held all new work, and why
	feedUnhold  feedKind = "unhold"  // the node started new work again
	feedFinish  feedKind = "finish"  // a command ended (or was cancelled)
	feedFail    feedKind = "fail"    // a command or base failed
	feedPark    feedKind = "park"    // a command was held beside a measurement
	feedThaw    feedKind = "thaw"    // a held command went on
	feedPause   feedKind = "pause"   // a command was frozen at its soft time limit
	feedResume  feedKind = "resume"  // a paused command went on, waited on again
	feedFork    feedKind = "fork"    // a workspace was forked from a base
	feedRelease feedKind = "release" // a workspace was released
	feedBase    feedKind = "base"    // a base began building, warmed, or was dropped
)

// feedKinds is every feedKind.
var feedKinds = []feedKind{feedAdmit, feedWait, feedHold, feedUnhold, feedFinish, feedFail, feedPark, feedThaw, feedPause, feedResume, feedFork, feedRelease, feedBase}

// feed makes a line for the cockpit's feed.
func feed(kind feedKind, text string, ref *pb.EventRef) *pb.WorkerEvent {
	return &pb.WorkerEvent{Kind: string(kind), Text: text, Ref: ref}
}

func ref(kind, id string) *pb.EventRef { return &pb.EventRef{Type: kind, Id: id} }

func (e *Engine) newEvent(ch change) *pb.Event {
	at := e.nowMs()
	if ch.feed != nil {
		ch.feed.At = at
	}
	return &pb.Event{At: at, Feed: ch.feed}
}

// saveBase records a base's new state. Call with e.mu held.
func (e *Engine) saveBase(b *pb.Base, ch change) {
	ev := e.newEvent(ch)
	ev.Change = &pb.Event_Base{Base: &pb.BaseChange{What: ch.what, Base: clone(b), Detail: ch.detail}}
	e.write("base", b.Id, b, ev)
}

// saveWorkspace records a workspace's new state. Call with e.mu held.
func (e *Engine) saveWorkspace(w *workspace, ch change) {
	ev := e.newEvent(ch)
	ev.Change = &pb.Event_Workspace{Workspace: &pb.WorkspaceChange{What: ch.what, Workspace: clone(w.ws()), Detail: ch.detail}}
	e.write("workspace", w.ws().Id, w.rec, ev)
}

// saveCommand records a command's new state. Call with e.mu held.
// Any captured diagnostics are persisted atomically in worker.db as pending_diag
// records and delivered separately via SaveCommandDiagnostics so events and
// checkpoints stay small and never drop diagnostics.
func (e *Engine) saveCommand(c *command, ch change) {
	ev := e.newEvent(ch)
	ev.Change = &pb.Event_Command{Command: &pb.CommandChange{What: ch.what, Command: clone(c.cmd()), Detail: ch.detail, Finished: ch.finished, Profile: ch.profile}}
	e.writeCommand(c.cmd().Id, c.rec, ch.diagnostics, ev)
}

// note records an event that changes no record: what the node itself did,
// such as holding new work. Call with e.mu held.
func (e *Engine) note(f *pb.WorkerEvent) {
	if err := e.db.commitEvent(e.newEvent(change{feed: f})); err != nil {
		slog.Error("save event", "kind", f.Kind, "err", err)
	}
	e.notify()
	e.changed()
}

// deleteBase, deleteWorkspace and deleteCommand drop a record, saying so.
// Call with e.mu held.
func (e *Engine) deleteBase(b *pb.Base, what string) {
	ev := e.newEvent(change{})
	ev.Change = &pb.Event_Base{Base: &pb.BaseChange{What: what, Base: clone(b)}}
	e.write("base", b.Id, nil, ev)
}

func (e *Engine) deleteWorkspace(w *workspace) {
	ev := e.newEvent(change{})
	ev.Change = &pb.Event_Workspace{Workspace: &pb.WorkspaceChange{What: "forgotten", Workspace: clone(w.ws())}}
	e.write("workspace", w.ws().Id, nil, ev)
}

func (e *Engine) deleteCommand(c *command) {
	ev := e.newEvent(change{})
	ev.Change = &pb.Event_Command{Command: &pb.CommandChange{What: "forgotten", Command: clone(c.cmd())}}
	e.writeCommand(c.cmd().Id, nil, nil, ev)
}

// writeCommand commits a command record (nil deletes it) and any pending
// intervention diagnostics with its event in one transaction. Call with e.mu
// held.
func (e *Engine) writeCommand(id string, rec *pb.CommandRecord, diag *pb.CommandDiagnostics, ev *pb.Event) {
	if err := e.db.commitCommand(id, rec, diag, ev); err != nil {
		slog.Error("save command", "command", id, "err", err)
		e.dirtyGen++
	}
	e.notify()
	e.changed()
}

// PendingDiagnostics returns captured intervention diagnostics persisted in
// worker.db that have not yet been acknowledged by the controller.
func (e *Engine) PendingDiagnostics() ([]*pb.CommandDiagnostics, error) {
	return e.db.listPendingDiagnostics()
}

// isDirty reports whether a DB save failed since the last in-memory checkpoint
// was delivered to the controller. Call with e.mu held.
func (e *Engine) isDirty() bool { return e.dirtyGen != e.savedGen }

// write commits a record (nil deletes it) with its event, wakes whoever
// watches the engine, and has the event pushed soon. Call with e.mu held:
// events are numbered in the order the engine made the changes.
func (e *Engine) write(kind, id string, rec proto.Message, ev *pb.Event) {
	if err := e.db.commit(kind, id, rec, ev); err != nil {
		// Neither the record nor its event is written (a full disk): the
		// controller gets the state as the engine has it instead.
		slog.Error("save "+kind, kind, id, "err", err)
		e.dirtyGen++
	}
	e.notify()
	e.changed()
}

// Epoch names the worker's database.
func (e *Engine) Epoch() string { return e.db.epoch }

// Events returns up to limit events after seq, oldest first.
func (e *Engine) Events(after int64, limit int) ([]*pb.Event, error) {
	return e.db.events(after, limit)
}

// memoryCheckpoint builds a checkpoint from the engine's live in-memory state
// and the database's newest event sequence. Call with e.mu held.
func (e *Engine) memoryCheckpoint() (*pb.Checkpoint, error) {
	_, newest, err := e.db.eventRange()
	if err != nil {
		return nil, err
	}
	cp := &pb.Checkpoint{Seq: newest}
	for _, b := range e.bases {
		cp.Bases = append(cp.Bases, clone(b))
	}
	for _, w := range e.workspaces {
		cp.Workspaces = append(cp.Workspaces, clone(w.ws()))
	}
	for _, c := range e.commands {
		cp.Commands = append(cp.Commands, clone(c.cmd()))
	}
	return cp, nil
}

// Checkpoint is the worker's whole state and the last event it includes.
// If any DB write failed in this process (dirtyGen > 0), SQLite may lack live
// records, so build the checkpoint from live in-memory state instead.
func (e *Engine) Checkpoint() (*pb.Checkpoint, error) {
	e.mu.Lock()
	if e.dirtyGen > 0 {
		cp, err := e.memoryCheckpoint()
		e.mu.Unlock()
		return cp, err
	}
	e.mu.Unlock()
	return e.db.checkpoint()
}

// unsaved returns the engine's state as a checkpoint and its dirty generation
// when a save failed since the last delivered checkpoint, and nil otherwise:
// the database lacks a change, so its own checkpoint would too. Callers must
// call markSaved(gen) only after PushEvents succeeds.
func (e *Engine) unsaved() (*pb.Checkpoint, uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.isDirty() {
		return nil, 0, nil
	}
	cp, err := e.memoryCheckpoint()
	if err != nil {
		return nil, 0, err
	}
	return cp, e.dirtyGen, nil
}

// markSaved records that the in-memory checkpoint for dirty generation gen has
// been delivered to the controller. If another save failed concurrently,
// dirtyGen will exceed gen and unsaved() will retry with the newer state.
func (e *Engine) markSaved(gen uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if gen > e.savedGen {
		e.savedGen = gen
	}
}

// Event retention: events the controller has applied are kept a day, for a
// controller restored from an older copy to catch up from; and never more
// than keepEvents, past which a controller that fell behind gets a
// checkpoint instead.
const (
	keepAppliedEvents = 24 * time.Hour
	keepEvents        = 50_000
)

// pruneEvents drops events past their retention, given what the controller
// last said it applied. It runs on a timer, so the cap holds while the
// controller is out of reach.
func (e *Engine) pruneEvents() {
	if err := e.db.prune(e.applied.Load(), e.now().Add(-keepAppliedEvents).UnixMilli(), keepEvents); err != nil {
		slog.Error("prune events", "err", err)
	}
}
