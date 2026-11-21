package worker

import (
	"log/slog"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// sessionEvent tells the controller what happened to a command, for its
// session's waiters. Call with e.mu held.
func (e *Engine) sessionEvent(c *command, what, detail string) {
	cmd := c.cmd()
	if cmd.SessionId == "" {
		return // a freshen belongs to no session
	}
	ev := &pb.SessionEvent{
		At: e.nowMs(), CommandId: cmd.Id, Argv: cmd.Argv, What: what, State: cmd.State,
		ExitCode: cmd.ExitCode, Detail: detail, SessionId: cmd.SessionId, WorkspaceId: cmd.WorkspaceId,
	}
	if _, err := e.db.enqueue("session", ev); err != nil {
		slog.Error("queue session event", "command", cmd.Id, "err", err)
	}
	e.changed()
}
