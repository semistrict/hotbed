package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
)

// workspaces serves WorkspaceService to clients by forwarding each call to
// the worker that holds the workspace or command.
type workspaces struct{ *Server }

// CreateWorkspace gives a session its workspace of a repo: the one it has,
// or a fork of the newest warm base on the node with the most free memory.
func (s *workspaces) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	sess, err := s.cfg.Store.LiveSession(ctx, req.SessionId, s.now())
	if err != nil {
		return nil, err
	}
	if w, ok, err := s.cfg.Store.SessionWorkspace(ctx, sess.Id, req.Repo); err != nil {
		return nil, err
	} else if ok {
		return s.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: w.id})
	}
	node, err := s.place(ctx, req)
	if err != nil {
		return nil, err
	}
	fwd := &pb.CreateWorkspaceRequest{
		Repo: req.Repo, Agent: sess.Agent, Branch: req.Branch, Id: newID(), SessionId: sess.Id,
		SyncDir: req.SyncDir,
	}
	if req.SyncDir != "" {
		fwd.SyncHost = httpx.Peer(ctx).Hostname
	}
	if err := s.cfg.Store.AddWorkspace(ctx, fwd.Id, sess.Id, node.node.Id, req.Repo, sess.Agent, s.now()); err != nil {
		return nil, err
	}
	c, host, err := s.worker(ctx, node.node.Id)
	if err == nil {
		var ws *pb.Workspace
		if ws, err = c.CreateWorkspace(ctx, fwd); err == nil {
			return ws, nil
		}
		err = fromWorker(host, err)
	}
	if derr := s.cfg.Store.DropWorkspace(ctx, fwd.Id); derr != nil {
		slog.Error("forget failed workspace", "workspace", fwd.Id, "err", derr)
	}
	return nil, err
}

// place picks the online node with a warm base of the repo and the most
// free memory.
func (s *workspaces) place(ctx context.Context, req *pb.CreateWorkspaceRequest) (nodeRow, error) {
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nodeRow{}, err
	}
	var best *nodeRow
	var bestFree int64
	building := ""
	for i := range rows {
		n := rows[i]
		if !s.withOnline(n.node).Online || n.report == nil || (req.Node != "" && n.node.Hostname != req.Node) {
			continue
		}
		warm := false
		for _, b := range n.report.Bases {
			if b.RepoId != req.Repo {
				continue
			}
			switch b.State {
			case pb.BaseState_BASE_STATE_WARM:
				warm = true
			case pb.BaseState_BASE_STATE_BUILDING:
				if building == "" {
					building = fmt.Sprintf("g%d on %s is %.0f%% freshened", b.Generation, n.node.Hostname, 100*b.Progress)
				}
			}
		}
		if !warm {
			continue
		}
		r := n.report
		free := r.GetResources().GetMemoryBytes() - max(r.GetUsage().GetMemoryUsedBytes(), r.ReservedMemoryBytes)
		if best == nil || free > bestFree {
			best, bestFree = &rows[i], free
		}
	}
	if best != nil {
		return *best, nil
	}
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nodeRow{}, err
	}
	if !slices.ContainsFunc(repos, func(r *pb.Repo) bool { return r.Id == req.Repo }) {
		return nodeRow{}, api.Errorf(api.CodeNotFound, "no repo %s", req.Repo).WithSuggestion("hotbed repo add NAME URL --freshen CMD")
	}
	e := &api.Error{Code: api.CodeNoWarmBase, Message: "no online node has a warm base of " + req.Repo, Retryable: true}
	if req.Node != "" {
		e.Message += " on " + req.Node
	}
	e.Suggestion = "check hotbed nodes, and the repo's freshen in the cockpit"
	if building != "" {
		e.Suggestion = "wait: " + building
	}
	return nodeRow{}, e
}

// workspaceOf is a live workspace's worker, and keeps its session alive.
func (s *workspaces) workspaceOf(ctx context.Context, id string) (hotbedv1connect.WorkspaceServiceClient, string, workspaceRow, error) {
	w, err := s.cfg.Store.Workspace(ctx, id)
	if err != nil {
		return nil, "", w, err
	}
	if w.released {
		return nil, "", w, api.Errorf(api.CodeNotFound, "workspace %s was released", id)
	}
	if _, err := s.cfg.Store.LiveSession(ctx, w.session, s.now()); err != nil {
		return nil, "", w, err
	}
	c, host, err := s.worker(ctx, w.node)
	return c, host, w, err
}

// commandOf is a command's worker; it keeps the command's session alive.
func (s *workspaces) commandOf(ctx context.Context, id string) (hotbedv1connect.WorkspaceServiceClient, string, error) {
	session, node, err := s.cfg.Store.Command(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if session != "" {
		if err := s.cfg.Store.Touch(ctx, session, s.now()); err != nil {
			return nil, "", err
		}
	}
	return s.worker(ctx, node)
}

func (s *workspaces) GetWorkspace(ctx context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
	c, host, _, err := s.workspaceOf(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	ws, err := c.GetWorkspace(ctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return ws, nil
}

// ListWorkspaces lists a session's workspaces as their workers have them,
// or, without a session, every live workspace as its node last reported it.
func (s *workspaces) ListWorkspaces(ctx context.Context, req *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	if req.SessionId != "" {
		sess, err := s.cfg.Store.LiveSession(ctx, req.SessionId, s.now())
		if err != nil {
			return nil, err
		}
		out := &pb.ListWorkspacesResponse{}
		for _, id := range sess.WorkspaceIds {
			ws, err := s.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: id})
			if err != nil {
				return nil, err
			}
			out.Workspaces = append(out.Workspaces, ws)
		}
		return out, nil
	}
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.ListWorkspacesResponse{}
	for _, n := range rows {
		for _, ws := range n.report.GetWorkspaces() {
			if ws.State != pb.WorkspaceState_WORKSPACE_STATE_RELEASED && (req.SessionId == "" || ws.SessionId == req.SessionId) {
				out.Workspaces = append(out.Workspaces, ws)
			}
		}
	}
	slices.SortFunc(out.Workspaces, func(a, b *pb.Workspace) int { return int(a.CreatedAt - b.CreatedAt) })
	return out, nil
}

func (s *workspaces) ReleaseWorkspace(ctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
	if _, _, _, err := s.workspaceOf(ctx, req.Id); err != nil {
		return nil, err
	}
	return &pb.ReleaseWorkspaceResponse{}, s.releaseWorkspace(ctx, req.Id)
}

// Sync relays a sync stream to the workspace's worker.
func (s *workspaces) Sync(ctx context.Context, in hotbedv1connect.WorkspaceServiceSyncServerStream) (*pb.SyncResponse, error) {
	first, err := in.Receive()
	if err != nil {
		return nil, err
	}
	h := first.GetHeader()
	if h == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "sync must start with a header")
	}
	c, host, _, err := s.workspaceOf(ctx, h.WorkspaceId)
	if err != nil {
		return nil, err
	}
	out, err := c.Sync(ctx)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	if err := out.Send(first); err != nil {
		return nil, fromWorker(host, err)
	}
	for {
		m, err := in.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := out.Send(m); err != nil {
			break // the worker failed; CloseAndReceive says why
		}
	}
	res, err := out.CloseAndReceive()
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

func (s *workspaces) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	c, host, _, err := s.workspaceOf(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	res, err := c.ListFiles(ctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

func (s *workspaces) GetFile(ctx context.Context, req *pb.GetFileRequest) (*pb.FileContent, error) {
	c, host, _, err := s.workspaceOf(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	res, err := c.GetFile(ctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

// RunCommand gives the command an ID and runs it on the workspace's worker.
func (s *workspaces) RunCommand(ctx context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
	c, host, w, err := s.workspaceOf(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	fwd := &pb.RunCommandRequest{
		WorkspaceId: req.WorkspaceId, Args: req.Args, Env: req.Env, Kind: req.Kind,
		Limits: req.Limits, Attached: req.Attached, Port: req.Port, Id: newID(),
	}
	if err := s.cfg.Store.AddCommand(ctx, fwd.Id, w.session, w.node, w.id, s.now()); err != nil {
		return nil, err
	}
	cmd, err := c.RunCommand(ctx, fwd)
	if err != nil {
		if derr := s.cfg.Store.DropCommand(ctx, fwd.Id); derr != nil {
			slog.Error("forget failed command", "command", fwd.Id, "err", derr)
		}
		return nil, fromWorker(host, err)
	}
	return cmd, nil
}

func (s *workspaces) GetCommand(ctx context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
	c, host, err := s.commandOf(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	res, err := c.GetCommand(ctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

// ListCommands lists live and just-ended commands as their nodes last
// reported them.
func (s *workspaces) ListCommands(ctx context.Context, req *pb.ListCommandsRequest) (*pb.ListCommandsResponse, error) {
	if req.SessionId != "" {
		if err := s.cfg.Store.Touch(ctx, req.SessionId, s.now()); err != nil {
			return nil, err
		}
	}
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.ListCommandsResponse{}
	for _, n := range rows {
		for _, c := range n.report.GetCommands() {
			if req.SessionId == "" || c.SessionId == req.SessionId {
				out.Commands = append(out.Commands, c)
			}
		}
	}
	slices.SortFunc(out.Commands, func(a, b *pb.Command) int { return strings.Compare(a.Id, b.Id) })
	slices.SortStableFunc(out.Commands, func(a, b *pb.Command) int { return int(a.QueuedAt - b.QueuedAt) })
	return out, nil
}

func (s *workspaces) StopCommand(ctx context.Context, req *pb.StopCommandRequest) (*pb.Command, error) {
	c, host, err := s.commandOf(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	res, err := c.StopCommand(ctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}

// FollowLog relays a command's output from its worker. Following a long
// command counts as using the session, so it is not abandoned meanwhile.
func (s *workspaces) FollowLog(ctx context.Context, req *pb.FollowLogRequest, out hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
	c, host, err := s.commandOf(ctx, req.Id)
	if err != nil {
		return err
	}
	session, _, err := s.cfg.Store.Command(ctx, req.Id)
	if err != nil {
		return err
	}
	in, err := c.FollowLog(ctx, req)
	if err != nil {
		return fromWorker(host, err)
	}
	defer in.Close()
	touched := time.Now()
	for {
		chunk, err := in.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fromWorker(host, err)
		}
		if err := out.Send(chunk); err != nil {
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

func (s *workspaces) GetLogLines(ctx context.Context, req *pb.GetLogLinesRequest) (*pb.CommandLog, error) {
	c, host, err := s.commandOf(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	res, err := c.GetLogLines(ctx, req)
	if err != nil {
		return nil, fromWorker(host, err)
	}
	return res, nil
}
