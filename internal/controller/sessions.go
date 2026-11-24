package controller

import (
	"cmp"
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	petname "github.com/dustinkirkland/golang-petname"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/httpx"
)

// sessions serves SessionService.
type sessions struct{ *Server }

var sessionName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)+$`)

const (
	maxT3CodeThreadIDLen      = 256
	defaultSessionSearchLimit = 50
)

func validateT3CodeThreadID(raw string) (string, error) {
	tid := strings.TrimSpace(raw)
	if len(tid) > maxT3CodeThreadIDLen {
		return "", api.Errorf(api.CodeInvalidArgs, "t3code_thread_id is too long (max %d bytes)", maxT3CodeThreadIDLen)
	}
	if strings.ContainsAny(tid, "\r\n\x00") {
		return "", api.Errorf(api.CodeInvalidArgs, "t3code_thread_id must not contain control characters")
	}
	return tid, nil
}

// StartSession starts a session called req.Id, or a free adjective-noun
// name when it has none, in its project; with req.Resume, it takes up the
// session of that name if there is one.
func (s *sessions) StartSession(ctx context.Context, req *pb.StartSessionRequest) (*pb.Session, error) {
	agent := req.Agent
	if agent == "" {
		agent = httpx.Peer(ctx).Hostname
	}
	t3codeThreadID, err := validateT3CodeThreadID(req.T3CodeThreadId)
	if err != nil {
		return nil, err
	}
	if req.Id != "" && (!sessionName.MatchString(req.Id) || len(req.Id) > 48) {
		return nil, api.Errorf(api.CodeInvalidArgs, "session name %q must look like adjective-noun: lowercase words joined by dashes", req.Id)
	}
	if req.Resume && req.Id == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "say which session to resume")
	}
	if req.Id != "" {
		// Refused (or resumed) before its project is looked for, which may
		// make one.
		if _, err := s.cfg.Store.Session(ctx, req.Id); err == nil {
			if !req.Resume {
				return nil, api.Errorf(api.CodeConflict, "a session called %s exists", req.Id).WithSuggestion("pick another name, or leave it out for a random one")
			}
			resumed, err := s.resume(ctx, req.Id, req.Project, t3codeThreadID)
			if err != nil {
				return nil, err
			}
			proj, err := s.cfg.Store.Project(ctx, resumed.Project)
			if err != nil {
				return nil, err
			}
			reminder := setupStepsReminder(proj)
			resumed.Reminder = &reminder
			return resumed, nil
		}
	}
	project, _, err := s.sessionProject(ctx, req.Project, gitops.CanonicalRemote(req.Remote))
	if err != nil {
		return nil, err
	}
	var sess *pb.Session
	if req.Id != "" {
		sess, err = s.cfg.Store.StartSession(ctx, req.Id, agent, project.Name, t3codeThreadID, s.now())
	} else {
		err = api.Errorf(api.CodeConflict, "could not find a free session name")
		for range 20 {
			candidate, serr := s.cfg.Store.StartSession(ctx, petname.Generate(2, "-"), agent, project.Name, t3codeThreadID, s.now())
			if serr == nil {
				sess, err = candidate, nil
				break
			}
			if api.CodeOf(serr) != api.CodeConflict {
				err = serr
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	reminder := setupStepsReminder(project)
	sess.Reminder = &reminder
	return sess, nil
}

// resume takes up a session again: live again if it ended, active now.
// Serializes against concurrent endSession/ReapIdle on the same session ID.
func (s *sessions) resume(ctx context.Context, id, project, t3codeThreadID string) (*pb.Session, error) {
	unlock := s.lockSession(id)
	defer unlock()
	sess, err := s.cfg.Store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	if project != "" && project != sess.Project {
		return nil, api.Errorf(api.CodeConflict, "session %s works on project %s, not %s", sess.Id, sess.Project, project).
			WithSuggestion("resume it without naming a project, or start another session")
	}
	if t3codeThreadID != "" && sess.T3CodeThreadId == "" {
		if err := s.cfg.Store.SetSessionT3CodeThreadIDIfEmpty(ctx, sess.Id, t3codeThreadID); err != nil {
			return nil, err
		}
	}
	if sess.EndedAt != nil {
		if err := s.cfg.Store.ResumeSession(ctx, sess.Id, s.now()); err != nil {
			return nil, err
		}
		s.sessionWakes.wake(sess.Id)
	}
	return s.cfg.Store.LiveSession(ctx, sess.Id, s.now())
}

func (s *sessions) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	id := strings.TrimSpace(req.Id)
	tid, err := validateT3CodeThreadID(req.T3CodeThreadId)
	if err != nil {
		return nil, err
	}
	if id != "" && tid != "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "give either id or t3code_thread_id, not both")
	}
	if id == "" && tid == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "session id or t3code_thread_id is required")
	}
	var sess *pb.Session
	if tid != "" {
		matches, _, err := s.cfg.Store.SearchSessionsByT3CodeThread(ctx, tid, "", false, 1)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			return nil, api.Errorf(api.CodeNotFound, "no session for T3 Code thread %s", tid).WithSuggestion("start one with hotbed session start")
		}
		sess = matches[0]
		id = sess.Id
	} else {
		sess, err = s.cfg.Store.Session(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	nodeHosts := make(map[string]string, len(nvs))
	targetNodes := map[string]bool{}
	for _, sw := range sess.Workspaces {
		targetNodes[sw.NodeId] = true
	}
	for _, nv := range nvs {
		nodeHosts[nv.node.Id] = nv.node.Hostname
		for _, ws := range nv.workspaces {
			if ws.SessionId == id {
				targetNodes[nv.node.Id] = true
			}
		}
		for _, c := range nv.commands {
			if c.SessionId == id {
				targetNodes[nv.node.Id] = true
			}
		}
	}
	// Query reachable nodes for live commands and workspaces without touching
	// the session's idle timer so ps / session status agree with each node
	// right after a stop or release, falling back to views for offline nodes.
	liveCmds := s.liveNodeCommands(ctx, nvs, targetNodes, id)
	liveWSByNode := s.liveNodeWorkspaces(ctx, nvs, targetNodes)

	// Reconcile unreleased workspace rows against authoritative live answers.
	reconciled := false
	for _, sw := range sess.Workspaces {
		if sw.ReleasedAt != nil || s.isCreatingWS(sw.Id) {
			continue
		}
		nodeWS, answered := liveWSByNode[sw.NodeId]
		if !answered {
			continue
		}
		var found *pb.Workspace
		for _, ws := range nodeWS {
			if ws.Id == sw.Id {
				found = ws
				break
			}
		}
		switch {
		case found == nil:
			if rerr := s.cfg.Store.ReconcileMissingWorkspace(context.WithoutCancel(ctx), sw.Id, s.now()); rerr != nil {
				slog.Warn("reconcile missing workspace in get session", "workspace", sw.Id, "err", rerr)
			} else {
				reconciled = true
			}
		case found.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED:
			if rerr := s.cfg.Store.ReleaseWorkspace(context.WithoutCancel(ctx), sw.Id, s.now()); rerr != nil {
				slog.Warn("reconcile released workspace in get session", "workspace", sw.Id, "err", rerr)
			} else {
				reconciled = true
			}
		}
	}
	if reconciled {
		if fresh, ferr := s.cfg.Store.Session(ctx, id); ferr == nil {
			sess = fresh
		}
	}

	releasedWS := make(map[string]bool, len(sess.Workspaces))
	for _, sw := range sess.Workspaces {
		if sw.ReleasedAt != nil {
			releasedWS[sw.Id] = true
		}
	}
	var wsList []*pb.Workspace
	var cmdList []*pb.Command
	for _, nv := range nvs {
		workspaces := nv.workspaces
		if lws, ok := liveWSByNode[nv.node.Id]; ok {
			workspaces = lws
		}
		for _, ws := range workspaces {
			if ws.State != pb.WorkspaceState_WORKSPACE_STATE_RELEASED && !releasedWS[ws.Id] && ws.SessionId == id {
				if ws.Project == "" {
					ws.Project = sess.Project
				}
				wsList = append(wsList, ws)
			}
		}
		cmds := nv.commands
		if lc, ok := liveCmds[nv.node.Id]; ok {
			cmds = lc
		}
		for _, c := range cmds {
			if c.Kind != "freshen" && c.SessionId == id {
				cmdList = append(cmdList, c)
			}
		}
	}
	slices.SortFunc(wsList, func(a, b *pb.Workspace) int { return cmp.Compare(a.CreatedAt, b.CreatedAt) })
	slices.SortFunc(cmdList, func(a, b *pb.Command) int { return strings.Compare(a.Id, b.Id) })
	slices.SortStableFunc(cmdList, func(a, b *pb.Command) int { return cmp.Compare(a.QueuedAt, b.QueuedAt) })
	return &pb.GetSessionResponse{
		Session:            sess,
		Workspaces:         wsList,
		Commands:           cmdList,
		Now:                s.now().UnixMilli(),
		IdleTimeoutSeconds: int64(sessionIdle / time.Second),
		NodeHosts:          nodeHosts,
	}, nil
}

func (s *sessions) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	tid, err := validateT3CodeThreadID(req.T3CodeThreadId)
	if err != nil {
		return nil, err
	}
	var project string
	if tid == "" || req.GetAllProjects() || req.GetProject() != "" || req.GetSessionId() != "" || req.GetRemote() != "" {
		project, err = s.listedProject(ctx, req)
		if err != nil {
			return nil, err
		}
	}
	since := s.now().Add(time.Millisecond) // only live ones
	if req.Ended {
		since = s.now().Add(-24 * time.Hour)
	}
	sessions, err := s.cfg.Store.RecentSessions(ctx, since, project, tid)
	return &pb.ListSessionsResponse{Sessions: sessions}, err
}

func (s *sessions) SearchSessions(ctx context.Context, req *pb.SearchSessionsRequest) (*pb.SearchSessionsResponse, error) {
	tid, err := validateT3CodeThreadID(req.T3CodeThreadId)
	if err != nil {
		return nil, err
	}
	if tid == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "t3code_thread_id is required").
			WithSuggestion("pass a T3 Code thread ID: hotbed session search THREAD_ID")
	}
	project := strings.TrimSpace(req.Project)
	if project != "" {
		if _, err := s.cfg.Store.Project(ctx, project); err != nil {
			return nil, err
		}
	}
	limit := defaultSessionSearchLimit
	if req.Limit != nil {
		if *req.Limit < 0 {
			return nil, api.Errorf(api.CodeInvalidArgs, "limit must be non-negative")
		}
		limit = int(*req.Limit)
	}
	matches, total, err := s.cfg.Store.SearchSessionsByT3CodeThread(ctx, tid, project, req.LiveOnly, limit)
	if err != nil {
		return nil, err
	}
	more := int32(max(0, total-len(matches)))
	return &pb.SearchSessionsResponse{Sessions: matches, More: more}, nil
}

func (s *sessions) EndSession(ctx context.Context, req *pb.EndSessionRequest) (*pb.Session, error) {
	return s.endLiveSession(ctx, req.Id, "ended by its agent")
}

// WatchSession streams the session: how it stands now, then each new batch
// of events as they come, until the session ends or the watch times out (at
// most two minutes). Every watch keeps the session alive.
func (s *sessions) WatchSession(ctx context.Context, req *pb.WatchSessionRequest, out hotbedv1connect.SessionServiceWatchSessionServerStream) error {
	if _, err := s.cfg.Store.LiveSession(ctx, req.SessionId, s.now()); err != nil {
		return err
	}
	return s.watch(ctx, req.SessionId, req.After, req.TimeoutSeconds, true, func(res *pb.SessionEvents) (bool, error) {
		if err := out.Send(res); err != nil {
			return false, nil // the client went away
		}
		return true, nil
	})
}

// Poll waits until the session has events after the cursor, or ends, or
// the wait times out, and returns how it stands then. It changes nothing,
// so a client may make it again when the reply is lost.
func (s *sessions) Poll(ctx context.Context, req *pb.PollRequest) (*pb.SessionEvents, error) {
	if req.SessionId == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "name the session to poll: its events are what happened to its commands, questions and reviews").
			WithSuggestion("pass -s SESSION, set HOTBED_SESSION, or run `hotbed session start`")
	}
	if _, err := s.cfg.Store.LiveSession(ctx, req.SessionId, s.now()); err != nil {
		return nil, err
	}
	var last *pb.SessionEvents
	if err := s.watch(ctx, req.SessionId, req.After, req.TimeoutSeconds, false, func(res *pb.SessionEvents) (bool, error) {
		last = res
		return false, nil
	}); err != nil {
		return nil, err
	}
	if last == nil {
		// Timed out (or the caller went away) with nothing new.
		return s.sessionState(ctx, req.SessionId, req.After)
	}
	return last, nil
}

// watch calls send with the session's events after a cursor each time it
// has new ones (and first with how it stands, if first), until send says
// to stop, the session ends, or timeout seconds (at most two minutes) pass.
// Its caller has checked the session is live, which keeps it alive.
func (s *sessions) watch(ctx context.Context, id string, after int64, timeout int32, first bool, send func(*pb.SessionEvents) (bool, error)) error {
	wait := maxWait
	if t := time.Duration(timeout) * time.Second; t > 0 && t < wait {
		wait = t
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woken := s.sessionWakes.wait(id)
		res, err := s.sessionState(ctx, id, after)
		if err != nil {
			return err
		}
		if first || len(res.Events) > 0 || res.Ended {
			more, err := send(res)
			if err != nil || !more {
				return err
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
func (s *sessions) sessionState(ctx context.Context, id string, after int64) (*pb.SessionEvents, error) {
	sess, err := s.cfg.Store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	events, err := s.cfg.Store.SessionEvents(ctx, id, after)
	if err != nil {
		return nil, err
	}
	res := &pb.SessionEvents{Events: events, Cursor: after, Ended: sess.EndedAt != nil, EndReason: sess.EndReason}
	if n := len(events); n > 0 {
		res.Cursor = events[n-1].Seq
	}
	views, err := s.cfg.Store.Views(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		for _, c := range v.commands {
			if c.SessionId == id && api.Live(c.State) {
				res.Live++
			}
		}
	}
	return res, nil
}
