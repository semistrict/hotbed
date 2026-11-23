package controller

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// A session's events are what its agent polls for: what happened to its
// commands and workspaces, as workers report it, and to the questions it
// asked and the reviews it requested.

// commandWhat is the command changes a session hears of, by the worker's
// word for them; the rest (queued, waiting, ...) it does not.
var commandWhat = map[string]pb.CommandUpdate_What{
	"admitted":       pb.CommandUpdate_WHAT_ADMITTED,
	"parked":         pb.CommandUpdate_WHAT_PARKED,
	"thawed":         pb.CommandUpdate_WHAT_THAWED,
	"paused":         pb.CommandUpdate_WHAT_PAUSED,
	"resumed":        pb.CommandUpdate_WHAT_RESUMED,
	"isolation lost": pb.CommandUpdate_WHAT_ISOLATION_LOST,
	"exited":         pb.CommandUpdate_WHAT_EXITED,
	"cancelled":      pb.CommandUpdate_WHAT_CANCELLED,
}

// workspaceWhat is likewise the workspace changes a session hears of.
var workspaceWhat = map[string]pb.WorkspaceUpdate_What{
	"ready":  pb.WorkspaceUpdate_WHAT_READY,
	"failed": pb.WorkspaceUpdate_WHAT_FAILED,
}

func commandUpdate(what pb.CommandUpdate_What, c *pb.Command, detail string) *pb.SessionEvent {
	if detail == "" {
		if iv := interventionForCommandUpdate(what, c); iv != nil {
			detail = api.InterventionNote(c, iv)
		}
	}
	return &pb.SessionEvent{Detail: detail, Event: &pb.SessionEvent_Command{Command: &pb.CommandUpdate{
		What: what, CommandId: c.Id, Argv: c.Argv, WorkspaceId: c.WorkspaceId, State: c.State, ExitCode: c.ExitCode,
	}}}
}

func interventionForCommandUpdate(what pb.CommandUpdate_What, c *pb.Command) *pb.CommandIntervention {
	last := api.LastIntervention(c, pb.InterventionKind_INTERVENTION_KIND_UNSPECIFIED)
	if last == nil {
		return nil
	}
	switch {
	case what == pb.CommandUpdate_WHAT_CANCELLED && last.Kind == pb.InterventionKind_INTERVENTION_KIND_STOP,
		what == pb.CommandUpdate_WHAT_PAUSED && last.Kind == pb.InterventionKind_INTERVENTION_KIND_PAUSE,
		what == pb.CommandUpdate_WHAT_RESUMED && last.Kind == pb.InterventionKind_INTERVENTION_KIND_RESUME:
		return last
	default:
		return nil
	}
}

func workspaceUpdate(what pb.WorkspaceUpdate_What, id, detail string) *pb.SessionEvent {
	return &pb.SessionEvent{Detail: detail, Event: &pb.SessionEvent_Workspace{Workspace: &pb.WorkspaceUpdate{What: what, WorkspaceId: id}}}
}

// questionUpdate carries the question as it is now, without its history.
func questionUpdate(what pb.QuestionUpdate_What, q *pb.Question, detail string) *pb.SessionEvent {
	q = proto.Clone(q).(*pb.Question)
	q.History = nil
	return &pb.SessionEvent{Detail: detail, Event: &pb.SessionEvent_Question{Question: &pb.QuestionUpdate{What: what, Question: q}}}
}

func reviewUpdate(what pb.ReviewUpdate_What, id int32, detail string) *pb.SessionEvent {
	return &pb.SessionEvent{Detail: detail, Event: &pb.SessionEvent_Review{Review: &pb.ReviewUpdate{What: what, ReviewId: id}}}
}

// tellSession adds e, which happened at at, to session's events if the
// session is live, and says whether it did: an answer or a review's news
// is for an agent still there to act on it.
func tellSession(ctx context.Context, q *controllerdb.Queries, session string, at int64, e *pb.SessionEvent) (bool, error) {
	if session == "" {
		return false, nil
	}
	live, err := q.IsSessionLive(ctx, session)
	if err != nil || live == 0 {
		return false, err
	}
	return recordSessionEvent(ctx, q, session, at, e)
}

// recordSessionEvent adds e, which happened at at, to session's events,
// live or not: what happened to a session's commands is its history, even
// when the news comes after it ended (a command its end stopped, reported
// by a worker the controller could not reach then). Its seq is the row's,
// set when read.
func recordSessionEvent(ctx context.Context, q *controllerdb.Queries, session string, at int64, e *pb.SessionEvent) (bool, error) {
	if session == "" {
		return false, nil
	}
	e.At, e.SessionId = at, session
	doc, err := marshal(e)
	if err != nil {
		return false, err
	}
	if err := q.InsertSessionEvent(ctx, controllerdb.InsertSessionEventParams{SessionID: session, Doc: doc}); err != nil {
		return false, fmt.Errorf("record session event: %w", err)
	}
	return true, nil
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
