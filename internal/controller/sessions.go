package controller

import (
	"context"
	"regexp"
	"time"

	petname "github.com/dustinkirkland/golang-petname"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/gitops"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
)

// sessions serves SessionService.
type sessions struct{ *Server }

var sessionName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)+$`)

// StartSession starts a session called req.Id, or a free adjective-noun
// name when it has none, in its project.
func (s *sessions) StartSession(ctx context.Context, req *pb.StartSessionRequest) (*pb.Session, error) {
	agent := req.Agent
	if agent == "" {
		agent = httpx.Peer(ctx).Hostname
	}
	if req.Id != "" && (!sessionName.MatchString(req.Id) || len(req.Id) > 48) {
		return nil, api.Errorf(api.CodeInvalidArgs, "session name %q must look like adjective-noun: lowercase words joined by dashes", req.Id)
	}
	project, err := s.sessionProject(ctx, req.Project, gitops.CanonicalRemote(req.Remote), "")
	if err != nil {
		return nil, err
	}
	if req.Id != "" {
		return s.cfg.Store.StartSession(ctx, req.Id, agent, project.Name, s.now())
	}
	for range 20 {
		sess, err := s.cfg.Store.StartSession(ctx, petname.Generate(2, "-"), agent, project.Name, s.now())
		if err == nil || api.FromConnect(err).Code != api.CodeConflict {
			return sess, err
		}
	}
	return nil, api.Errorf(api.CodeConflict, "could not find a free session name")
}

func (s *sessions) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.Session, error) {
	return s.cfg.Store.Session(ctx, req.Id)
}

func (s *sessions) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	if req.Project != "" {
		if _, err := s.cfg.Store.Project(ctx, req.Project); err != nil {
			return nil, err
		}
	}
	since := s.now().Add(time.Millisecond) // only live ones
	if req.Ended {
		since = s.now().Add(-24 * time.Hour)
	}
	all, err := s.cfg.Store.RecentSessions(ctx, since)
	if err != nil {
		return nil, err
	}
	out := &pb.ListSessionsResponse{}
	for _, sess := range all {
		if req.Project == "" || sess.Project == req.Project {
			out.Sessions = append(out.Sessions, sess)
		}
	}
	return out, nil
}

func (s *sessions) EndSession(ctx context.Context, req *pb.EndSessionRequest) (*pb.Session, error) {
	if _, err := s.cfg.Store.LiveSession(ctx, req.Id, s.now()); err != nil {
		return nil, err
	}
	return s.endSession(ctx, req.Id, "ended by its agent")
}

// WatchSession streams the session: how it stands now, then each new batch
// of events as workers report them, until the session ends or the watch
// times out (at most two minutes). Every watch keeps the session alive.
func (s *sessions) WatchSession(ctx context.Context, req *pb.WatchSessionRequest, out hotbedv1connect.SessionServiceWatchSessionServerStream) error {
	if _, err := s.cfg.Store.LiveSession(ctx, req.SessionId, s.now()); err != nil {
		return err
	}
	wait := maxWait
	if t := time.Duration(req.TimeoutSeconds) * time.Second; t > 0 && t < wait {
		wait = t
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	after, first := req.After, true
	for {
		woken := s.sessionWakes.wait(req.SessionId)
		res, err := s.sessionState(ctx, req.SessionId, after)
		if err != nil {
			return err
		}
		if first || len(res.Events) > 0 || res.Ended {
			if err := out.Send(res); err != nil {
				return nil // the client went away
			}
			first, after = false, res.Cursor
		}
		if res.Ended {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-woken:
		}
	}
}

// sessionState is the session's events after a cursor and its live commands.
func (s *sessions) sessionState(ctx context.Context, id string, after int64) (*pb.WatchSessionResponse, error) {
	sess, err := s.cfg.Store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	events, err := s.cfg.Store.SessionEvents(ctx, id, after)
	if err != nil {
		return nil, err
	}
	res := &pb.WatchSessionResponse{Events: events, Cursor: after, Ended: sess.EndedAt != nil, EndReason: sess.EndReason}
	if n := len(events); n > 0 {
		res.Cursor = events[n-1].Seq
	}
	views, err := s.cfg.Store.Views(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		for _, c := range v.commands {
			if c.SessionId == id && live(c.State) {
				res.Live++
			}
		}
	}
	return res, nil
}

func live(st pb.CommandState) bool {
	return st == pb.CommandState_COMMAND_STATE_QUEUED || st == pb.CommandState_COMMAND_STATE_RUNNING || st == pb.CommandState_COMMAND_STATE_PARKED
}
