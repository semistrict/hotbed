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
}

// feed makes a line for the cockpit's feed.
func feed(kind, text string, ref *pb.EventRef) *pb.WorkerEvent {
	return &pb.WorkerEvent{Kind: kind, Text: text, Ref: ref}
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
func (e *Engine) saveCommand(c *command, ch change) {
	ev := e.newEvent(ch)
	ev.Change = &pb.Event_Command{Command: &pb.CommandChange{What: ch.what, Command: clone(c.cmd()), Detail: ch.detail, Finished: ch.finished}}
	e.write("command", c.cmd().Id, c.rec, ev)
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
	e.write("command", c.cmd().Id, nil, ev)
}

// write commits a record (nil deletes it) with its event, wakes whoever
// watches the engine, and has the event pushed soon. Call with e.mu held:
// events are numbered in the order the engine made the changes.
func (e *Engine) write(kind, id string, rec proto.Message, ev *pb.Event) {
	if err := e.db.commit(kind, id, rec, ev); err != nil {
		slog.Error("save "+kind, kind, id, "err", err)
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

// Checkpoint is the worker's whole state and the last event it includes.
func (e *Engine) Checkpoint() (*pb.Checkpoint, error) { return e.db.checkpoint() }

// Event retention: events the controller has applied are kept a day, for a
// controller restored from an older copy to catch up from; and never more
// than keepEvents, past which a controller that fell behind gets a
// checkpoint instead.
const (
	keepAppliedEvents = 24 * time.Hour
	keepEvents        = 50_000
)

// pruneEvents drops events past their retention, given what the controller
// has applied.
func (e *Engine) pruneEvents(applied int64) {
	if err := e.db.prune(applied, e.now().Add(-keepAppliedEvents).UnixMilli(), keepEvents); err != nil {
		slog.Error("prune events", "err", err)
	}
}
