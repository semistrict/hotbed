package controller

import (
	"context"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/httpx"
)

// Who changed a question or a review: an agent, as its session, which says
// the project it may change things in; or the user, as no session.

// agentStamp stamps a change an agent makes, and says the project it may make
// it in: its session's. It must name its session, which must be live, and
// which the change keeps alive.
func (s *Server) agentStamp(ctx context.Context, session, reason string) (*pb.Stamp, *pb.Project, error) {
	if session == "" {
		return nil, nil, api.Errorf(api.CodeInvalidArgs, "name the session making the change: questions and reviews record which session did what").
			WithSuggestion("pass -s SESSION, set HOTBED_SESSION, or run `hotbed session start`")
	}
	return s.userStamp(ctx, session, reason)
}

// inProject refuses an agent's change to another project's question or
// review (what, id, of project of): its sessions are the ones that read it.
// When project is nil (the user acting without a session), any project is allowed.
func inProject(what string, id int32, of string, project *pb.Project, by *pb.Stamp) error {
	if project == nil || of == project.Name {
		return nil
	}
	return api.Errorf(api.CodeConflict, "%s %d is project %s's; session %s works on %s", what, id, of, by.Session, project.Name).
		WithSuggestion("leave it to a session of " + of)
}

// userStamp stamps a change the user makes: from the cockpit, with no
// session (returning a nil project), or passed on by an agent, as its
// session, which must be live and returns its project.
func (s *Server) userStamp(ctx context.Context, session, reason string) (*pb.Stamp, *pb.Project, error) {
	var project *pb.Project
	if session != "" {
		sess, err := s.cfg.Store.LiveSession(ctx, session, s.now())
		if err != nil {
			return nil, nil, err
		}
		if project, err = s.cfg.Store.Project(ctx, sess.Project); err != nil {
			return nil, nil, err
		}
	}
	return &pb.Stamp{At: s.now().UnixMilli(), Session: session, Host: httpx.Peer(ctx).Hostname, Reason: reason}, project, nil
}
