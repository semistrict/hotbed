package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/daemonlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/profiler"
)

const (
	// cleanupTimeout bounds SQLite cleanup/reconciliation after a caller's
	// context may have been cancelled.
	cleanupTimeout = 10 * time.Second
	// orphanReleaseTimeout bounds best-effort worker ReleaseWorkspace cleanup
	// when a CreateWorkspace RPC fails because the caller cancelled or timed out.
	orphanReleaseTimeout = 5 * time.Second
)

// upgradeWait is how long CreateWorkspace waits when every eligible node is
// still upgrading to the controller's build before failing retryably.
var upgradeWait = 90 * time.Second

func (s *Server) upgradeWaitTimeout() time.Duration {
	if s.cfg.WakeWait > 0 && s.cfg.WakeWait < upgradeWait {
		return s.cfg.WakeWait
	}
	return upgradeWait
}

// workspaces serves the controller's WorkspaceService RPCs: workspace
// placement and lifecycle, cluster-wide listings, command interventions
// (StopCommand, PauseCommand, ResumeCommand), and cockpit read/diagnostics
// forwarding. Data-plane calls (Sync, WatchWorkspace, RunCommand, FollowLog)
// go directly to the workspace's worker.
type workspaces struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	*Server
}

// CreateWorkspace gives a session a workspace of its project for a run, on
// the node placement picks: the session's workspace there, or a new one,
// which brings the node's base up to date first when a repo's branch has
// moved. A node picked asleep is woken and waited for (NODE_WAKING when it
// is not up within wakeWait), then the run placed again. A node that
// cannot make it now, out of reach or unable to read a repo's remote, is
// passed over for the next one placement picks; when none is left, the run
// fails as the last of them did.
func (s *workspaces) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	sess, err := s.cfg.Store.LiveSession(ctx, req.SessionId, s.now())
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	var last error
	var upgradeDeadline time.Time
	for {
		node, fwd, upgrading, err := s.claim(ctx, sess, req, skip)
		switch {
		case err != nil && last != nil:
			return nil, last
		case err != nil && len(upgrading) > 0:
			if upgradeDeadline.IsZero() {
				upgradeDeadline = s.now().Add(s.upgradeWaitTimeout())
				slog.Info("waiting for upgrading node before placing run", "nodes", upgrading, "session", sess.Id)
			}
			if !s.now().Before(upgradeDeadline) {
				return nil, err
			}
			ready, werr := s.waitUpgrade(ctx, upgrading, upgradeDeadline)
			if werr != nil {
				return nil, werr
			}
			if !ready {
				return nil, err
			}
			continue
		case err != nil:
			return nil, err
		case node.asleep:
			// Woken, it is placed again: it is up, and as busy as it is.
			if err := s.awaken(ctx, node.host); err != nil {
				return nil, err
			}
			continue
		case fwd == nil:
			// One still being made (by another run of the session) is waited
			// for, not reported missing.
			if made := s.creatingDone(node.mine); made != nil {
				select {
				case <-made:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			ws, gerr := s.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: node.mine})
			if gerr != nil && api.CodeOf(gerr) == api.CodeNotFound && !s.isCreatingWS(node.mine) {
				slog.Warn("reconciling missing workspace during placement", "workspace", node.mine, "node", node.host)
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
				rerr := s.cfg.Store.ReconcileMissingWorkspace(rctx, node.mine, s.now())
				cancel()
				if rerr != nil {
					slog.Error("reconcile missing workspace", "workspace", node.mine, "err", rerr)
					return nil, gerr
				}
				continue
			}
			if gerr == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_FAILED {
				if rerr := s.releaseFailedWorkspace(ctx, ws.Id); rerr != nil {
					return nil, rerr
				}
				continue
			}
			if gerr == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
				rerr := s.cfg.Store.ReleaseWorkspace(rctx, ws.Id, s.now())
				cancel()
				if rerr != nil {
					return nil, rerr
				}
				continue
			}
			return ws, gerr
		}
		ws, err := s.make(ctx, sess, node, fwd)
		if err == nil || ctx.Err() != nil || !passOver(err) {
			return ws, err
		}
		slog.Warn("a node cannot make a workspace now; placing the run elsewhere", "node", node.host, "project", sess.Project, "err", err)
		skip[node.node.Id], last = true, err
	}
}

// createAwake is CreateWorkspace for the controller's own runs, which have
// no client to call again: it waits for a node it woke as long as one may
// take to boot.
func (s *workspaces) createAwake(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	deadline := time.Now().Add(bootTimeout + s.power.wakeWait)
	for {
		w, err := s.CreateWorkspace(ctx, req)
		if api.CodeOf(err) != api.CodeNodeWaking || ctx.Err() != nil || time.Now().After(deadline) {
			return w, err
		}
	}
}

// passOver says a node's failure to make a workspace is the node's alone,
// so another may make it.
func passOver(err error) bool {
	switch api.CodeOf(err) {
	case api.CodeWorkerUnreachable, api.CodeRepoUnreachable:
		return true
	}
	return false
}

// make has the node make the workspace claim recorded, and forgets it if
// the node cannot.
func (s *workspaces) make(ctx context.Context, sess *pb.Session, node candidate, fwd *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	defer s.unmarkCreatingWS(fwd.Id)
	c, host, err := s.worker(ctx, node.node.Id)
	if err == nil {
		var ws *pb.Workspace
		if ws, err = c.CreateWorkspace(ctx, fwd); err == nil {
			checkCtx, checkCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			w, werr := s.cfg.Store.Workspace(checkCtx, fwd.Id)
			sessNow, serr := s.cfg.Store.Session(checkCtx, sess.Id)
			checkCancel()
			if werr != nil || w.released || (serr == nil && sessNow.EndedAt != nil) {
				// The session ended or this workspace was released while the worker
				// was creating it; release it on the worker so it does not leak.
				rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), workerRPCTimeout)
				defer rcancel()
				if _, rerr := c.ReleaseWorkspace(rctx, &pb.ReleaseWorkspaceRequest{Id: fwd.Id}); rerr != nil && api.CodeOf(rerr) != api.CodeNotFound {
					slog.Warn("release workspace created during session end", "workspace", fwd.Id, "err", rerr)
				}
				_ = s.cfg.Store.ReleaseWorkspace(rctx, fwd.Id, s.now())
				if serr == nil && sessNow.EndedAt != nil {
					return nil, sessionEndedError(sess.Id, sessNow.EndReason)
				}
				return nil, api.Errorf(api.CodeNotFound, "workspace %s was released", fwd.Id)
			}
			ws.NodeAddr, ws.Node = s.addrOf(node.nodeRow), node.host
			ws.Project = sess.Project
			return ws, nil
		}
		if ctx.Err() != nil {
			// The caller cancelled or timed out while the worker RPC was in
			// flight; the worker may still have finished creating fwd.Id.
			// Release it best-effort before dropping the controller row so it
			// does not leak as an orphan on the worker.
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), orphanReleaseTimeout)
			if _, rerr := c.ReleaseWorkspace(rctx, &pb.ReleaseWorkspaceRequest{Id: fwd.Id}); rerr != nil && api.CodeOf(rerr) != api.CodeNotFound {
				slog.Warn("release workspace after canceled create", "workspace", fwd.Id, "node", host, "err", rerr)
			}
			rcancel()
		}
		err = fromWorker(host, err)
	}
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer dcancel()
	if derr := s.cfg.Store.DropWorkspace(dctx, fwd.Id); derr != nil {
		slog.Error("forget failed workspace", "workspace", fwd.Id, "err", derr)
	}
	return nil, err
}

// claim places the run among the nodes not in skip and, when the session
// has no workspace on that node, records the one it gets there and returns
// the request that makes it; fwd is nil when node.mine is the workspace.
// Any worker RPCs needed to release a failed workspace or probe a missing one
// run with s.placing unlocked.
func (s *workspaces) claim(ctx context.Context, sess *pb.Session, req *pb.CreateWorkspaceRequest, skip map[string]bool) (node candidate, fwd *pb.CreateWorkspaceRequest, upgrading []string, err error) {
	probed := map[string]bool{}
	for {
		s.placing.Lock()
		res, err := s.place(ctx, sess, req, skip, probed)
		if err != nil || res.node.asleep {
			s.placing.Unlock()
			return res.node, nil, res.upgrading, err
		}
		if len(res.needRelease) > 0 || len(res.needProbe) > 0 {
			s.placing.Unlock()
			for _, w := range res.needRelease {
				if rerr := s.releaseFailedWorkspace(ctx, w.id); rerr != nil {
					return candidate{}, nil, nil, rerr
				}
			}
			for _, w := range res.needProbe {
				probed[w.id] = true
				pctx, cancel := context.WithTimeout(ctx, workerProbeTimeout)
				if c, _, werr := s.worker(pctx, w.node); werr == nil {
					gws, gerr := c.GetWorkspace(pctx, &pb.GetWorkspaceRequest{Id: w.id})
					cancel()
					switch {
					case gerr != nil && api.CodeOf(gerr) == api.CodeNotFound:
						rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
						rerr := s.cfg.Store.ReconcileMissingWorkspace(rctx, w.id, s.now())
						rcancel()
						if rerr != nil {
							return candidate{}, nil, nil, rerr
						}
					case gerr == nil && gws.State == pb.WorkspaceState_WORKSPACE_STATE_FAILED:
						if rerr := s.releaseFailedWorkspace(ctx, w.id); rerr != nil {
							return candidate{}, nil, nil, rerr
						}
					case gerr == nil && gws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED:
						rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
						rerr := s.cfg.Store.ReleaseWorkspace(rctx, w.id, s.now())
						rcancel()
						if rerr != nil {
							return candidate{}, nil, nil, rerr
						}
					}
				} else {
					cancel()
				}
			}
			if ctx.Err() != nil {
				return candidate{}, nil, nil, ctx.Err()
			}
			continue
		}
		node = res.node
		// A run placed on it is work: the node does not sleep under it.
		s.power.wake(node.host)
		if node.mine != "" {
			s.placing.Unlock()
			return node, nil, nil, nil
		}
		fwd = &pb.CreateWorkspaceRequest{
			Repo: req.Repo, Agent: sess.Agent, Id: newID(), SessionId: sess.Id,
			SyncDir: req.SyncDir, Project: res.project,
		}
		if req.SyncDir != "" {
			fwd.SyncHost = httpx.Peer(ctx).Hostname
		}
		ok, err := s.cfg.Store.AddLiveWorkspace(ctx, fwd.Id, sess.Id, node.node.Id, sess.Agent, s.now())
		if err != nil {
			s.placing.Unlock()
			return node, nil, nil, err
		}
		if !ok {
			s.placing.Unlock()
			if cur, serr := s.cfg.Store.Session(ctx, sess.Id); serr != nil {
				return node, nil, nil, serr
			} else if cur.EndedAt != nil {
				return node, nil, nil, sessionEndedError(sess.Id, cur.EndReason)
			}
			return node, nil, nil, sessionEndedError(sess.Id, "")
		}
		s.markCreatingWS(fwd.Id)
		s.placing.Unlock()
		return node, fwd, nil, nil
	}
}

// waitUpgrade waits until at least one host in hosts serves on the
// controller's build, or until deadline or ctx expires. Must be called without
// s.placing held.
func (s *Server) waitUpgrade(ctx context.Context, hosts []string, deadline time.Time) (bool, error) {
	remaining := deadline.Sub(s.now())
	if remaining <= 0 {
		return false, nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	for {
		// Register wake channels before checking stored node state so a
		// Heartbeat arriving between the check and the wait is never missed.
		waiters := make([]<-chan struct{}, len(hosts))
		for i, h := range hosts {
			waiters[i] = s.power.changes.wait(h)
		}
		ready, err := s.anyNodeServes(ctx, hosts)
		if err != nil {
			return false, err
		}
		if ready {
			return true, nil
		}
		if !s.now().Before(deadline) {
			return false, nil
		}
		if !waitAny(ctx, timer.C, waiters) {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, nil
		}
	}
}

func (s *Server) anyNodeServes(ctx context.Context, hosts []string) (bool, error) {
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range rows {
		if slices.Contains(hosts, n.node.Hostname) && s.serves(n) {
			return true, nil
		}
	}
	return false, nil
}

// waitAny blocks until any channel in chs closes, or until ctx or timeout
// fires. It returns true if a channel in chs closed, false on ctx/timeout.
func waitAny(ctx context.Context, timeout <-chan time.Time, chs []<-chan struct{}) bool {
	if len(chs) == 1 {
		select {
		case <-chs[0]:
			return true
		case <-ctx.Done():
			return false
		case <-timeout:
			return false
		}
	}
	woke := make(chan struct{}, 1)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, ch := range chs {
		wg.Add(1)
		go func(c <-chan struct{}) {
			defer wg.Done()
			select {
			case <-c:
				select {
				case woke <- struct{}{}:
				default:
				}
			case <-done:
			}
		}(ch)
	}
	var ok bool
	select {
	case <-woke:
		ok = true
	case <-ctx.Done():
	case <-timeout:
	}
	close(done)
	wg.Wait()
	return ok
}

func (s *workspaces) releaseFailedWorkspace(ctx context.Context, id string) error {
	err := s.releaseWorkspace(ctx, id)
	switch e := api.FromConnect(err); {
	case err == nil, e.Code == api.CodeNotFound:
		return nil
	case e.Code == api.CodeWorkerUnreachable:
		// Its worker's next heartbeat releases it; healthy nodes can place now.
		slog.Warn("release failed workspace later", "workspace", id, "err", err)
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		return s.cfg.Store.ReleaseWorkspace(rctx, id, s.now())
	default:
		return err
	}
}

// workspaceOf is a live workspace's worker, and keeps its session alive.
func (s *workspaces) workspaceOf(ctx context.Context, id string) (hotbedv1connect.WorkspaceServiceClient, string, workspaceRow, string, error) {
	w, err := s.cfg.Store.Workspace(ctx, id)
	if err != nil {
		return nil, "", w, "", err
	}
	if w.released {
		return nil, "", w, "", api.Errorf(api.CodeNotFound, "workspace %s was released", id)
	}
	sess, err := s.cfg.Store.LiveSession(ctx, w.session, s.now())
	if err != nil {
		return nil, "", w, "", err
	}
	c, host, err := s.worker(ctx, w.node)
	return c, host, w, sess.Project, err
}

// commandOf is a command's worker; it keeps the command's session alive.
func (s *workspaces) commandOf(ctx context.Context, id string) (hotbedv1connect.WorkspaceServiceClient, string, string, error) {
	session, node, err := s.cfg.Store.Command(ctx, id)
	if err != nil {
		return nil, "", "", err
	}
	if session != "" {
		if err := s.cfg.Store.Touch(ctx, session, s.now()); err != nil {
			return nil, "", "", err
		}
	}
	c, host, err := s.worker(ctx, node)
	return c, host, session, err
}

// GetWorkspace asks the workspace's worker, or, when it is out of reach,
// answers from its node's view.
func (s *workspaces) GetWorkspace(ctx context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
	c, host, w, project, err := s.workspaceOf(ctx, req.Id)
	if err == nil {
		wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
		defer cancel()
		var ws *pb.Workspace
		if ws, err = c.GetWorkspace(wctx, req); err == nil {
			s.locateWorkspace(ctx, ws, w.node)
			ws.Project = project
			return ws, nil
		}
		err = fromWorker(host, err)
	}
	if unreachable(err) {
		return s.viewWorkspace(ctx, req.Id)
	}
	return nil, err
}

// relay forwards a worker's stream to a client until either ends, keeping
// the session alive meanwhile.
func relay[T any](ctx context.Context, s *Server, session, host string, receive func() (*T, error), send func(*T) error) error {
	touched := time.Now()
	for {
		m, err := receive()
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil // the worker is done, or the client went away
		}
		if err != nil {
			return fromWorker(host, err)
		}
		if err := send(m); err != nil {
			return nil // the client went away
		}
		if session != "" && time.Since(touched) > time.Minute {
			touched = time.Now()
			if err := s.cfg.Store.Touch(ctx, session, s.now()); err != nil {
				slog.Warn("keep session alive", "session", session, "err", err)
			}
		}
	}
}

// ListWorkspaces lists the live workspaces of a session (which it keeps
// alive), a project, or every session, as their nodes' views have them.
func (s *workspaces) ListWorkspaces(ctx context.Context, req *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	var sess *pb.Session
	if req.SessionId != "" {
		var err error
		if sess, err = s.cfg.Store.LiveSession(ctx, req.SessionId, s.now()); err != nil {
			return nil, err
		}
	}
	if req.Project != "" {
		if _, err := s.cfg.Store.Project(ctx, req.Project); err != nil {
			return nil, err
		}
	}
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	var viewIDs []string
	for _, nv := range nvs {
		for _, ws := range nv.workspaces {
			if ws.State != pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
				viewIDs = append(viewIDs, ws.Id)
			}
		}
	}
	releasedWS, err := s.cfg.Store.ReleasedWorkspaces(ctx, viewIDs)
	if err != nil {
		return nil, err
	}
	out := &pb.ListWorkspacesResponse{}
	seen := map[string]bool{}
	for _, nv := range nvs {
		for _, ws := range nv.workspaces {
			if ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED || releasedWS[ws.Id] {
				continue
			}
			if req.SessionId != "" && ws.SessionId != req.SessionId {
				continue
			}
			if req.Project != "" && ws.Project != req.Project {
				continue
			}
			out.Workspaces = append(out.Workspaces, ws)
			seen[ws.Id] = true
		}
	}
	// A workspace just created may not be in its node's view yet: its
	// worker says how it is.
	for _, id := range sess.GetWorkspaceIds() {
		if seen[id] {
			continue
		}
		ws, err := s.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: id})
		if err != nil {
			if api.CodeOf(err) == api.CodeNotFound && !s.isCreatingWS(id) {
				rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
				if rerr := s.cfg.Store.ReconcileMissingWorkspace(rctx, id, s.now()); rerr != nil {
					slog.Warn("reconcile missing workspace", "workspace", id, "err", rerr)
				}
				rcancel()
				continue
			}
			slog.Warn("list a workspace not in its node's view", "workspace", id, "err", err)
			continue
		}
		if req.Project != "" && ws.Project != req.Project {
			continue
		}
		out.Workspaces = append(out.Workspaces, ws)
	}
	slices.SortFunc(out.Workspaces, func(a, b *pb.Workspace) int { return cmp.Compare(a.CreatedAt, b.CreatedAt) })
	return out, nil
}

func (s *workspaces) ReleaseWorkspace(ctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
	if _, _, _, _, err := s.workspaceOf(ctx, req.Id); err != nil {
		return nil, err
	}
	return &pb.ReleaseWorkspaceResponse{}, s.releaseWorkspace(ctx, req.Id)
}

func (s *workspaces) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	c, host, _, _, err := s.workspaceOf(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
	defer cancel()
	res, err := c.ListFiles(wctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

func (s *workspaces) GetFile(ctx context.Context, req *pb.GetFileRequest) (*pb.FileContent, error) {
	c, host, _, _, err := s.workspaceOf(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
	defer cancel()
	res, err := c.GetFile(wctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

// runCommand gives a scheduled command an ID and runs it on the workspace's
// worker. Clients run commands directly on the worker.
func (s *workspaces) runCommand(ctx context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
	c, host, w, project, err := s.workspaceOf(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	fwd := &pb.RunCommandRequest{
		WorkspaceId: req.WorkspaceId, Args: req.Args, Env: req.Env, Kind: req.Kind,
		Limits: req.Limits, Attached: req.Attached, Port: req.Port, Bench: req.Bench, Id: newID(),
		Repo: req.Repo,
	}
	if err := s.cfg.Store.AddCommand(ctx, fwd.Id, w.session, w.node, w.id, s.now()); err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
	defer cancel()
	cmd, err := c.RunCommand(wctx, fwd)
	if err != nil {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		if derr := s.cfg.Store.DropCommand(dctx, fwd.Id); derr != nil {
			slog.Error("forget failed command", "command", fwd.Id, "err", derr)
		}
		dcancel()
		return nil, fromWorker(host, err)
	}
	cmd.Project = project
	return cmd, nil
}

func (s *workspaces) OpenShell(ctx context.Context, req *pb.OpenShellRequest) (*pb.OpenShellResponse, error) {
	c, host, _, _, err := s.workspaceOf(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
	defer cancel()
	res, err := c.OpenShell(wctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

// GetCommand asks the command's worker, or, when it is out of reach,
// answers from its node's view. When the worker has forgotten the command
// (or restarted without it), history, the base whose freshen it was, or the
// controller's own command record still names its node so its on-disk log
// stays reachable (board 7951, board 7917).
func (s *workspaces) GetCommand(ctx context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
	route, err := s.cfg.Store.lookupCommand(ctx, req.Id)
	if err != nil && api.CodeOf(err) != api.CodeNotFound {
		return nil, err
	}
	known := err == nil
	if known {
		if route.session != "" {
			if err = s.cfg.Store.Touch(ctx, route.session, s.now()); err != nil {
				return nil, err
			}
		}
		var c hotbedv1connect.WorkspaceServiceClient
		var host string
		if c, host, err = s.worker(ctx, route.node); err == nil {
			wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
			defer cancel()
			var cmd *pb.Command
			if cmd, err = c.GetCommand(wctx, req); err == nil {
				cmd.NodeAddr = s.nodeAddr(ctx, cmd.NodeId)
				if route.session != "" {
					if sess, serr := s.cfg.Store.Session(ctx, route.session); serr == nil {
						cmd.Project = sess.Project
					}
				}
				return cmd, nil
			}
			err = fromWorker(host, err)
		}
	}
	if unreachable(err) {
		// Worker is unreachable: answer only from authoritative cached/terminal
		// sources (view, history, or a terminal base). Never fabricate FAILED
		// from the routing row alone — preserve the retryable unreachable error.
		vcmd, verr := s.viewCommandByID(ctx, req.Id)
		if verr == nil {
			return vcmd, nil
		}
		if api.CodeOf(verr) != api.CodeNotFound {
			return nil, verr
		}
		if !route.histChecked {
			fcmd, ok, ferr := s.finishedCommand(ctx, req.Id)
			if ferr != nil {
				return nil, ferr
			}
			if ok {
				return fcmd, nil
			}
		}
		if bcmd, ok, berr := s.baseCommand(ctx, req.Id, route, true); berr != nil {
			return nil, berr
		} else if ok {
			return bcmd, nil
		}
		return nil, err
	}
	if api.CodeOf(err) == api.CodeNotFound {
		// Its worker forgets a command a while after it ends; history
		// keeps it, and the worker its log.
		if !route.histChecked {
			fcmd, ok, ferr := s.finishedCommand(ctx, req.Id)
			if ferr != nil {
				return nil, ferr
			}
			if ok {
				return fcmd, nil
			}
		}
		// If the worker just forgot an ended command before the "forgotten"
		// event reached the controller, its cached ended view still holds
		// full command metadata.
		vcmd, verr := s.viewCommandByID(ctx, req.Id)
		if verr != nil && api.CodeOf(verr) != api.CodeNotFound {
			return nil, verr
		}
		if verr == nil && !api.Live(vcmd.State) {
			return vcmd, nil
		}
		// Base freshens are not stored in history; a base in node_state
		// still names its freshen command and outcome.
		if bcmd, ok, berr := s.baseCommand(ctx, req.Id, route, false); berr != nil {
			return nil, berr
		} else if ok {
			return bcmd, nil
		}
		// If the controller recorded which node ran this command (e.g. a
		// superseded base freshen whose base was collected, or a command
		// lost across worker restart before FinishedCommand arrived), return
		// a synthesized record pointing to its node so `hotbed logs` can
		// still stream any retained on-disk log file.
		if known && route.node != "" {
			return s.knownCommand(ctx, req.Id, route.session, route.node), nil
		}
		return nil, err
	}
	return nil, err
}

// viewCommandByID reads a single command from node_state and enriches it with
// its node's address, session project, and live heartbeat usage when online.
func (s *workspaces) viewCommandByID(ctx context.Context, id string) (*pb.Command, error) {
	nodeID, cmd, ok, err := s.cfg.Store.ViewCommand(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, api.Errorf(api.CodeNotFound, "no command %s", id)
	}
	n, err := s.cfg.Store.node(ctx, nodeID)
	if err != nil {
		if api.CodeOf(err) == api.CodeNotFound {
			return nil, api.Errorf(api.CodeNotFound, "no command %s", id)
		}
		return nil, err
	}
	cmd.NodeAddr = s.addrOf(n)
	if cmd.SessionId != "" {
		sess, serr := s.cfg.Store.Session(ctx, cmd.SessionId)
		if serr != nil && api.CodeOf(serr) != api.CodeNotFound {
			return nil, serr
		}
		if serr == nil && sess.Project != "" {
			cmd.Project = sess.Project
		}
	}
	if s.withOnline(n.node).Online && n.hb != nil && api.Live(cmd.State) {
		for _, u := range n.hb.Commands {
			if u.Id == cmd.Id {
				cmd.CpuCores, cmd.MemoryBytes, cmd.SwapBytes = u.CpuCores, u.MemoryBytes, u.SwapBytes
				cmd.DroppedOutputBytes = u.DroppedOutputBytes
				if cmd.State == pb.CommandState_COMMAND_STATE_QUEUED && u.Reason != "" {
					cmd.Reason = u.Reason
				}
				break
			}
		}
	}
	return cmd, nil
}

// baseCommand synthesizes a command record for a base freshen whose worker has
// forgotten it (or is unreachable, if the base already has terminal evidence),
// from the base entry still held in node_state.
func (s *workspaces) baseCommand(ctx context.Context, id string, route commandRoute, wasUnreachable bool) (*pb.Command, bool, error) {
	nodeID, b := route.node, route.base
	if !route.baseChecked {
		var ok bool
		var err error
		nodeID, b, ok, err = s.cfg.Store.BaseByCommand(ctx, id)
		if err != nil || !ok {
			return nil, false, err
		}
	} else if b == nil {
		return nil, false, nil
	}
	nid := cmp.Or(b.NodeId, nodeID)
	cmd := &pb.Command{
		Id:       id,
		NodeId:   nid,
		NodeAddr: s.nodeAddr(ctx, nid),
		Project:  b.Project,
		Kind:     "freshen",
		Argv:     fmt.Sprintf("freshen %s g%d", b.Project, b.Generation),
		EndedAt:  b.BuiltAt,
	}
	if step := failedBaseStep(b); step != "" {
		cmd.State = pb.CommandState_COMMAND_STATE_FAILED
		cmd.Reason = fmt.Sprintf("base %s failed at step %s", b.Id, step)
		return cmd, true, nil
	}
	switch b.State {
	case pb.BaseState_BASE_STATE_WARM:
		cmd.State = pb.CommandState_COMMAND_STATE_SUCCEEDED
	case pb.BaseState_BASE_STATE_STALE:
		if b.BuiltAt != nil && *b.BuiltAt > 0 {
			cmd.State = pb.CommandState_COMMAND_STATE_SUCCEEDED
		} else {
			cmd.State = pb.CommandState_COMMAND_STATE_CANCELLED
			cmd.Reason = fmt.Sprintf("base %s became stale without completing", b.Id)
		}
	case pb.BaseState_BASE_STATE_FAILED:
		cmd.State = pb.CommandState_COMMAND_STATE_FAILED
		cmd.Reason = fmt.Sprintf("base %s failed to freshen", b.Id)
	default:
		if wasUnreachable {
			return nil, false, nil
		}
		cmd.State = pb.CommandState_COMMAND_STATE_FAILED
		cmd.Reason = fmt.Sprintf("base %s freshen is no longer tracked by its worker", b.Id)
	}
	return cmd, true, nil
}

func failedBaseStep(b *pb.Base) string {
	for _, st := range b.GetSteps() {
		if st.Ran && !st.Ok {
			return st.Name
		}
	}
	return ""
}

// knownCommand synthesizes a non-live command record from the controller's
// commands table when the worker has forgotten it and neither history nor
// node_state holds a richer record.
func (s *workspaces) knownCommand(ctx context.Context, id, session, node string) *pb.Command {
	cmd := &pb.Command{
		Id:        id,
		NodeId:    node,
		NodeAddr:  s.nodeAddr(ctx, node),
		SessionId: session,
		State:     pb.CommandState_COMMAND_STATE_FAILED,
		Reason:    "command is no longer tracked by its worker",
	}
	if session == "" && strings.HasPrefix(id, "f-") {
		cmd.Kind = "freshen"
	}
	if session != "" {
		if sess, err := s.cfg.Store.Session(ctx, session); err == nil {
			cmd.Project = sess.Project
		}
	}
	return cmd
}

// finishedCommand is a command as history keeps it; ok is false when history
// has no such command.
func (s *workspaces) finishedCommand(ctx context.Context, id string) (*pb.Command, bool, error) {
	f, ok, err := s.cfg.Store.Finished(ctx, id)
	if err != nil || !ok {
		return nil, false, err
	}
	cmd := commandOfHistory(f)
	cmd.NodeAddr = s.nodeAddr(ctx, f.NodeId)
	return cmd, true, nil
}

// commandOfHistory is a command that ended, as history keeps it: what a
// command says of itself once it has ended, less what history drops (its
// arguments one by one, limits and timeline).
func commandOfHistory(f *pb.FinishedCommand) *pb.Command {
	ended := f.EndedAt
	started := ended - int64(f.Seconds*1000)
	cmd := &pb.Command{
		Id: f.Id, WorkspaceId: f.WorkspaceId, NodeId: f.NodeId, Argv: f.Argv, Kind: f.Kind, State: f.State,
		QueuedAt: started - int64(f.QueuedSeconds*1000), StartedAt: &started, EndedAt: &ended,
		ExitCode: f.ExitCode, ColdSeconds: f.ColdSeconds, Reason: f.Reason,
		SessionId: f.SessionId, Bench: f.Bench, Project: f.Project, Repo: f.RepoId,
	}
	if f.Rebuilt != nil && f.Of != nil {
		cmd.Rebuilt = &pb.Rebuilt{Units: *f.Rebuilt, Of: *f.Of}
	}
	if len(f.Interventions) > 0 {
		cmd.Interventions = make([]*pb.CommandIntervention, len(f.Interventions))
		for i, iv := range f.Interventions {
			cmd.Interventions[i] = proto.Clone(iv).(*pb.CommandIntervention)
		}
	}
	return cmd
}

// ListCommands lists the live and just-ended commands of a session (which
// it keeps alive), or of a project or every session, base builds among
// them; with ended, those history keeps too. Online workers are queried directly so stops and completions are
// reflected immediately rather than waiting on the asynchronous event stream;
// offline nodes fall back to their cached view.
func (s *workspaces) ListCommands(ctx context.Context, req *pb.ListCommandsRequest) (*pb.ListCommandsResponse, error) {
	if req.GetLimit() < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "limit %d: give 0 for every ended command, or a positive number", req.GetLimit())
	}
	var sess *pb.Session
	if req.SessionId != "" {
		if err := s.cfg.Store.Touch(ctx, req.SessionId, s.now()); err != nil {
			return nil, err
		}
		if got, err := s.cfg.Store.Session(ctx, req.SessionId); err == nil {
			sess = got
		}
	}
	if req.Project != "" {
		if _, err := s.cfg.Store.Project(ctx, req.Project); err != nil {
			return nil, err
		}
	}
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	var targetNodes map[string]bool
	if req.SessionId != "" {
		targetNodes = map[string]bool{}
		for _, sw := range sess.GetWorkspaces() {
			targetNodes[sw.NodeId] = true
		}
		for _, nv := range nvs {
			for _, ws := range nv.workspaces {
				if ws.SessionId == req.SessionId {
					targetNodes[nv.node.Id] = true
				}
			}
			for _, c := range nv.commands {
				if c.SessionId == req.SessionId {
					targetNodes[nv.node.Id] = true
				}
			}
		}
	}
	live := s.liveNodeCommands(ctx, nvs, targetNodes, req.SessionId)
	var running, ended []*pb.Command
	for _, nv := range nvs {
		cmds := nv.commands
		if lc, ok := live[nv.node.Id]; ok {
			cmds = lc
		}
		for _, c := range cmds {
			// A base build is no session's, but a project's and the node's:
			// it shows beside them, parked too (board 7422).
			if req.SessionId != "" && c.SessionId != req.SessionId {
				continue
			}
			if req.Project != "" && c.Project != req.Project {
				continue
			}
			if !strings.Contains(c.Argv, req.Match) {
				continue
			}
			if c.EndedAt == nil {
				running = append(running, c)
			} else {
				ended = append(ended, c)
			}
		}
	}
	limit := defaultCommandLimit
	if req.Limit != nil {
		limit = int(*req.Limit)
	}
	ended, more, err := s.newestEnded(ctx, req, ended, limit)
	if err != nil {
		return nil, err
	}
	out := &pb.ListCommandsResponse{Commands: append(running, ended...), More: more}
	slices.SortFunc(out.Commands, func(a, b *pb.Command) int { return strings.Compare(a.Id, b.Id) })
	slices.SortStableFunc(out.Commands, func(a, b *pb.Command) int { return cmp.Compare(a.QueuedAt, b.QueuedAt) })
	return out, nil
}

// defaultCommandLimit is how many ended commands ListCommands lists when
// asked for no other number.
const defaultCommandLimit = 50

// newestEnded is the newest limit (0: every one) of the ended commands req
// picks: those the nodes' views have and, with req.Ended, those history
// keeps; and how many more it picks. A view's command is the one kept when
// history has it too: it says more.
func (s *workspaces) newestEnded(ctx context.Context, req *pb.ListCommandsRequest, viewed []*pb.Command, limit int) ([]*pb.Command, int32, error) {
	picked, total := viewed, len(viewed)
	if req.Ended {
		f := HistoryFilter{Project: req.Project, Session: req.SessionId, Match: req.Match}
		newest, kept, err := s.cfg.Store.HistoryMatching(ctx, f, limit)
		if err != nil {
			return nil, 0, err
		}
		ids := make([]string, len(viewed))
		for i, c := range viewed {
			ids[i] = c.Id
		}
		inHistory, err := s.cfg.Store.InHistory(ctx, ids)
		if err != nil {
			return nil, 0, err
		}
		total = kept
		for _, id := range ids {
			if !inHistory[id] {
				total++
			}
		}
		// The newest limit of all are among history's newest limit and
		// what the views have.
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			seen[id] = true
		}
		picked = slices.Clone(viewed)
		for _, h := range newest {
			if !seen[h.Id] {
				picked = append(picked, commandOfHistory(h))
			}
		}
	}
	slices.SortFunc(picked, func(a, b *pb.Command) int {
		return cmp.Or(cmp.Compare(b.GetEndedAt(), a.GetEndedAt()), strings.Compare(a.Id, b.Id))
	})
	if limit > 0 && len(picked) > limit {
		picked = picked[:limit]
	}
	return picked, int32(total - len(picked)), nil
}

// diagnoseRPCTimeout bounds controller->worker StopCommand/PauseCommand RPCs
// that request diagnostics first (sample + inspect can take up to a minute).
const diagnoseRPCTimeout = 90 * time.Second

// StopCommand, PauseCommand and ResumeCommand forward a user or agent
// intervention to the command's worker. Host is cleared so the worker derives
// the caller host solely from its authenticated peer rather than trusting a
// forwarded field.
func (s *workspaces) StopCommand(ctx context.Context, req *pb.StopCommandRequest) (*pb.Command, error) {
	fwd := proto.Clone(req).(*pb.StopCommandRequest)
	fwd.Host = ""
	fwd.Internal = false
	fwd.Reason = ""
	return s.byHand(ctx, fwd.Id, fwd.SessionId, fwd.Diagnose, func(wctx context.Context, c hotbedv1connect.WorkspaceServiceClient) (*pb.Command, error) {
		return c.StopCommand(wctx, fwd)
	})
}

func (s *workspaces) PauseCommand(ctx context.Context, req *pb.PauseCommandRequest) (*pb.Command, error) {
	fwd := proto.Clone(req).(*pb.PauseCommandRequest)
	fwd.Host = ""
	return s.byHand(ctx, fwd.Id, fwd.SessionId, fwd.Diagnose, func(wctx context.Context, c hotbedv1connect.WorkspaceServiceClient) (*pb.Command, error) {
		return c.PauseCommand(wctx, fwd)
	})
}

func (s *workspaces) ResumeCommand(ctx context.Context, req *pb.ResumeCommandRequest) (*pb.Command, error) {
	fwd := proto.Clone(req).(*pb.ResumeCommandRequest)
	fwd.Host = ""
	return s.byHand(ctx, fwd.Id, fwd.SessionId, false, func(wctx context.Context, c hotbedv1connect.WorkspaceServiceClient) (*pb.Command, error) {
		return c.ResumeCommand(wctx, fwd)
	})
}

// byHand forwards a stop, pause, or resume RPC to command id's worker and
// enriches the returned command with its project. When reqSession is non-empty,
// it must match the command's owning session.
func (s *workspaces) byHand(ctx context.Context, id, reqSession string, diagnose bool, call func(context.Context, hotbedv1connect.WorkspaceServiceClient) (*pb.Command, error)) (*pb.Command, error) {
	session, node, err := s.cfg.Store.Command(ctx, id)
	if err != nil {
		return nil, err
	}
	if reqSession != "" && reqSession != session {
		return nil, api.Errorf(api.CodeNotFound, "session %s has no command %s", reqSession, id)
	}
	if session != "" {
		if err := s.cfg.Store.Touch(ctx, session, s.now()); err != nil {
			return nil, err
		}
	}
	c, host, err := s.worker(ctx, node)
	if err != nil {
		return nil, err
	}
	timeout := workerRPCTimeout
	if diagnose {
		timeout = diagnoseRPCTimeout
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := call(wctx, c)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	if session != "" {
		if sess, serr := s.cfg.Store.Session(ctx, session); serr == nil {
			res.Project = sess.Project
		}
	}
	return res, nil
}

// stopCommand stops a command internally on its worker (e.g. schedule cleanup)
// without recording a human intervention.
func (s *workspaces) stopCommand(ctx context.Context, req *pb.StopCommandRequest) (*pb.Command, error) {
	fwd := proto.Clone(req).(*pb.StopCommandRequest)
	fwd.Host = ""
	fwd.Internal = true
	if fwd.Reason == "" {
		fwd.Reason = "stopped by controller"
	}
	return s.byHand(ctx, fwd.Id, "", fwd.Diagnose, func(wctx context.Context, c hotbedv1connect.WorkspaceServiceClient) (*pb.Command, error) {
		return c.StopCommand(wctx, fwd)
	})
}

func (s *workspaces) GetLogLines(ctx context.Context, req *pb.GetLogLinesRequest) (*pb.CommandLog, error) {
	c, host, _, err := s.commandOf(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
	defer cancel()
	res, err := c.GetLogLines(wctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

func (s *workspaces) InspectCommand(ctx context.Context, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	c, host, _, err := s.commandOf(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
	defer cancel()
	res, err := c.InspectCommand(wctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

func (s *workspaces) TraceCommand(ctx context.Context, req *pb.TraceCommandRequest, out hotbedv1connect.WorkspaceServiceTraceCommandServerStream) error {
	c, host, session, err := s.commandOf(ctx, req.Id)
	if err != nil {
		return err
	}
	in, err := c.TraceCommand(ctx, req)
	if err != nil {
		return fromWorker(host, err)
	}
	defer in.Close()
	return relay(ctx, s.Server, session, host, in.Receive, out.Send)
}

func (s *workspaces) SampleCommand(ctx context.Context, req *pb.SampleCommandRequest, out hotbedv1connect.WorkspaceServiceSampleCommandServerStream) error {
	c, host, session, err := s.commandOf(ctx, req.Id)
	if err != nil {
		return err
	}
	in, err := c.SampleCommand(ctx, req)
	if err != nil {
		return fromWorker(host, err)
	}
	defer in.Close()
	return relay(ctx, s.Server, session, host, in.Receive, out.Send)
}

func (s *workspaces) GetProfile(ctx context.Context, req *pb.GetProfileRequest) (*pb.CommandProfile, error) {
	switch t := req.GetTarget().(type) {
	case *pb.GetProfileRequest_CommandId:
		id := t.CommandId
		if strings.TrimSpace(id) == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "command ID is required").
				WithSuggestion("pass a command ID (`hotbed profile CMD`)")
		}
		if strings.TrimSpace(id) != id {
			return nil, api.Errorf(api.CodeInvalidArgs, "command ID %q must not have leading or trailing whitespace", id).
				WithSuggestion("pass a command ID (`hotbed profile CMD`)")
		}
		// If the command has already finished and its profile was persisted in the
		// controller database, query it directly so it remains available even after
		// the worker forgets the command or goes offline.
		if stored, ok, err := s.cfg.Store.CommandProfile(ctx, id); err != nil {
			return nil, err
		} else if ok {
			stored.Live = false
			return profiler.QueryProfile(stored, req), nil
		}
		c, host, _, err := s.commandOf(ctx, id)
		if err == nil {
			wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
			defer cancel()
			res, werr := c.GetProfile(wctx, req)
			if werr == nil {
				if err := profiler.Admit(res); err != nil {
					return nil, api.Errorf(api.CodeInternal, "%s's profile of %s: %v", host, id, err)
				}
				return res, nil
			}
			err = fromWorker(host, werr)
		}
		// If the command exists in history (ended with zero captured samples, or
		// profiler was inactive on that worker), return an empty profile instead of 404.
		if f, ok, ferr := s.cfg.Store.Finished(ctx, id); ferr == nil && ok && f != nil {
			return &pb.CommandProfile{
				CommandId:  id,
				Profiler:   profiler.EngineName,
				Live:       false,
				Active:     false,
				StatusNote: "no continuous eBPF samples were recorded for this command",
			}, nil
		}
		return nil, err
	case *pb.GetProfileRequest_Node:
		target := strings.TrimSpace(t.Node)
		if target == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "node hostname or ID is required").
				WithSuggestion("pass a node name from `hotbed nodes`")
		}
		n, err := s.node(ctx, target)
		if err != nil {
			return nil, err
		}
		c, host, err := s.worker(ctx, n.Id)
		if err != nil {
			return nil, err
		}
		wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
		defer cancel()
		res, err := c.GetProfile(wctx, req)
		if err != nil {
			return nil, fromWorker(host, err)
		}
		if err := profiler.Admit(res); err != nil {
			return nil, api.Errorf(api.CodeInternal, "%s's node profile: %v", host, err)
		}
		if res.Node == "" {
			res.Node = host
		}
		return res, nil
	default:
		return nil, api.Errorf(api.CodeInvalidArgs, "profile target (command_id or node) is required").
			WithSuggestion("pass a command ID (`hotbed profile CMD`) or a node (`hotbed profile --node HOST`)")
	}
}

const daemonLogWorkerTimeout = 5 * time.Second

// GetDaemonLogs returns recent WARN and ERROR slog records from the controller,
// a named worker, or all of them combined (newest first).
func (s *workspaces) GetDaemonLogs(ctx context.Context, req *pb.GetDaemonLogsRequest) (*pb.GetDaemonLogsResponse, error) {
	fromMs, toMs, limit, err := daemonlog.NormalizeWindow(req, s.now())
	if err != nil {
		return nil, err
	}
	normalized := &pb.GetDaemonLogsRequest{
		Level:  req.GetLevel(),
		Query:  req.GetQuery(),
		FromMs: fromMs,
		ToMs:   toMs,
		Limit:  int32(limit),
	}

	target := strings.TrimSpace(req.GetSource())
	if target == "controller" {
		return s.cfg.DaemonLogs.Query("controller", "controller", s.cfg.Controller, normalized)
	}

	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}

	if target != "" {
		var nodeID string
		for _, r := range rows {
			if r.node.Hostname == target || r.node.Id == target {
				nodeID = r.node.Id
				break
			}
		}
		if nodeID == "" {
			return nil, api.Errorf(api.CodeNotFound, "no daemon log source %q", target).
				WithSuggestion(`pass "controller" or a node name from hotbed nodes`)
		}
		c, host, err := s.worker(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		wctx, cancel := context.WithTimeout(ctx, workerRPCTimeout)
		defer cancel()
		res, err := c.GetDaemonLogs(wctx, normalized)
		if err != nil {
			return nil, fromWorker(host, err)
		}
		fillWorkerLogMetadata(res, host)
		return res, nil
	}

	ctrlRes, err := s.cfg.DaemonLogs.Query("controller", "controller", s.cfg.Controller, normalized)
	if err != nil {
		return nil, err
	}

	type workerResult struct {
		entries   []*pb.DaemonLogEntry
		source    *pb.DaemonLogSource
		truncated bool
	}
	results := make([]workerResult, len(rows))
	var wg sync.WaitGroup
	for i, r := range rows {
		host := r.node.Hostname
		if !s.withOnline(r.node).Online {
			src := offlineDaemonLogSource(r.node)
			results[i] = workerResult{source: src}
			continue
		}
		wg.Add(1)
		go func(idx int, node *pb.Node) {
			defer wg.Done()
			wctx, cancel := context.WithTimeout(ctx, daemonLogWorkerTimeout)
			defer cancel()
			c, h, err := s.worker(wctx, node.Id)
			if err != nil {
				results[idx] = workerResult{source: unreachableDaemonLogSource(node, err.Error())}
				return
			}
			res, err := c.GetDaemonLogs(wctx, normalized)
			if err != nil {
				results[idx] = workerResult{source: unreachableDaemonLogSource(node, fromWorker(h, err).Error())}
				return
			}
			fillWorkerLogMetadata(res, h)
			var src *pb.DaemonLogSource
			if len(res.Sources) > 0 {
				src = res.Sources[0]
			} else {
				src = &pb.DaemonLogSource{Source: h, Role: "worker", Host: h, Reachable: true}
			}
			results[idx] = workerResult{entries: res.Entries, source: src, truncated: res.Truncated}
		}(i, r.node)
		_ = host
	}
	wg.Wait()

	allEntries := append([]*pb.DaemonLogEntry(nil), ctrlRes.Entries...)
	sources := append([]*pb.DaemonLogSource(nil), ctrlRes.Sources...)
	truncated := ctrlRes.Truncated
	for _, r := range results {
		allEntries = append(allEntries, r.entries...)
		if r.source != nil {
			sources = append(sources, r.source)
		}
		if r.truncated {
			truncated = true
		}
	}

	slices.SortFunc(allEntries, func(a, b *pb.DaemonLogEntry) int {
		if c := cmp.Compare(b.At, a.At); c != 0 {
			return c
		}
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return strings.Compare(a.Msg, b.Msg)
	})
	if len(allEntries) > limit {
		allEntries = allEntries[:limit]
		truncated = true
	}

	slices.SortFunc(sources, func(a, b *pb.DaemonLogSource) int {
		if a.Role != b.Role {
			if a.Role == "controller" {
				return -1
			}
			if b.Role == "controller" {
				return 1
			}
		}
		return strings.Compare(a.Source, b.Source)
	})

	return &pb.GetDaemonLogsResponse{
		Entries:   allEntries,
		Sources:   sources,
		Truncated: truncated,
	}, nil
}

func fillWorkerLogMetadata(res *pb.GetDaemonLogsResponse, host string) {
	for _, e := range res.Entries {
		if e.Source == "" {
			e.Source = host
		}
		if e.Role == "" {
			e.Role = "worker"
		}
		if e.Host == "" {
			e.Host = host
		}
	}
	for _, src := range res.Sources {
		if src.Source == "" {
			src.Source = host
		}
		if src.Role == "" {
			src.Role = "worker"
		}
		if src.Host == "" {
			src.Host = host
		}
	}
}

func offlineDaemonLogSource(n *pb.Node) *pb.DaemonLogSource {
	return unreachableDaemonLogSource(n, "offline")
}

func unreachableDaemonLogSource(n *pb.Node, msg string) *pb.DaemonLogSource {
	src := &pb.DaemonLogSource{
		Source:    n.Hostname,
		Role:      "worker",
		Host:      n.Hostname,
		Reachable: false,
		Error:     msg,
	}
	if prev := n.GetDaemonLogs(); prev != nil {
		src.Errors_24H = prev.Errors_24H
		src.Warns_24H = prev.Warns_24H
		src.LastAt = prev.LastAt
		src.DiskBytes = prev.DiskBytes
		src.Files = prev.Files
		src.MaxFileBytes = prev.MaxFileBytes
		src.MaxFiles = prev.MaxFiles
		src.MaxAgeDays = prev.MaxAgeDays
	}
	return src
}
