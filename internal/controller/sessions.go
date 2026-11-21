package controller

import (
	"context"
	"regexp"
	"time"

	petname "github.com/dustinkirkland/golang-petname"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/httpx"
)

// sessions serves SessionService.
type sessions struct{ *Server }

var sessionName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)+$`)

// StartSession starts a session called req.Id, or a free adjective-noun
// name when it has none.
func (s *sessions) StartSession(ctx context.Context, req *pb.StartSessionRequest) (*pb.Session, error) {
	agent := req.Agent
	if agent == "" {
		agent = httpx.Peer(ctx).Hostname
	}
	if req.Id != "" {
		if !sessionName.MatchString(req.Id) || len(req.Id) > 48 {
			return nil, api.Errorf(api.CodeInvalidArgs, "session name %q must look like adjective-noun: lowercase words joined by dashes", req.Id)
		}
		return s.cfg.Store.StartSession(ctx, req.Id, agent, s.now())
	}
	for range 20 {
		sess, err := s.cfg.Store.StartSession(ctx, petname.Generate(2, "-"), agent, s.now())
		if err == nil || api.FromConnect(err).Code != api.CodeConflict {
			return sess, err
		}
	}
	return nil, api.Errorf(api.CodeConflict, "could not find a free session name")
}

func (s *sessions) EndSession(ctx context.Context, req *pb.EndSessionRequest) (*pb.Session, error) {
	if _, err := s.cfg.Store.LiveSession(ctx, req.Id, s.now()); err != nil {
		return nil, err
	}
	return s.endSession(ctx, req.Id, "ended by its agent")
}

// WaitSession long-polls: it returns the session's events after the cursor
// as soon as there are any, or empty-handed after the timeout (at most two
// minutes). Every call keeps the session alive.
func (s *sessions) WaitSession(ctx context.Context, req *pb.WaitSessionRequest) (*pb.WaitSessionResponse, error) {
	if _, err := s.cfg.Store.LiveSession(ctx, req.SessionId, s.now()); err != nil {
		return nil, err
	}
	wait := maxWait
	if t := time.Duration(req.TimeoutSeconds) * time.Second; t > 0 && t < wait {
		wait = t
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woken := s.waiter(req.SessionId)
		res, err := s.sessionState(ctx, req.SessionId, req.After)
		if err != nil || len(res.Events) > 0 || res.Ended {
			return res, err
		}
		select {
		case <-ctx.Done():
			return res, nil
		case <-deadline.C:
			return res, nil
		case <-woken:
		}
	}
}

// sessionState is the session's events after a cursor and its live commands.
func (s *sessions) sessionState(ctx context.Context, id string, after int64) (*pb.WaitSessionResponse, error) {
	sess, err := s.cfg.Store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	events, err := s.cfg.Store.SessionEvents(ctx, id, after)
	if err != nil {
		return nil, err
	}
	res := &pb.WaitSessionResponse{Events: events, Cursor: after, Ended: sess.EndedAt != nil, EndReason: sess.EndReason}
	if n := len(events); n > 0 {
		res.Cursor = events[n-1].Seq
	}
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range rows {
		for _, c := range n.report.GetCommands() {
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
