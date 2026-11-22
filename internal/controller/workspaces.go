package controller

import (
	"cmp"
	"context"
	"errors"
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
// or a new one on a node that hosts the repo, which brings its base up to
// date first when the branch has moved.
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
	node, repo, err := s.place(ctx, req)
	if err != nil {
		return nil, err
	}
	fwd := &pb.CreateWorkspaceRequest{
		Repo: req.Repo, Agent: sess.Agent, Branch: req.Branch, Id: newID(), SessionId: sess.Id,
		SyncDir: req.SyncDir, RepoConfig: repo,
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
			ws.NodeAddr = s.addrOf(node.nodeRow)
			return ws, nil
		}
		err = fromWorker(host, err)
	}
	if derr := s.cfg.Store.DropWorkspace(ctx, fwd.Id); derr != nil {
		slog.Error("forget failed workspace", "workspace", fwd.Id, "err", derr)
	}
	return nil, err
}

// place picks, among the online nodes that host the repo, one that has a
// warm base of it (so its freshen is incremental) with the most free memory.
func (s *workspaces) place(ctx context.Context, req *pb.CreateWorkspaceRequest) (nodeView, *pb.Repo, error) {
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nodeView{}, nil, err
	}
	i := slices.IndexFunc(repos, func(r *pb.Repo) bool { return r.Id == req.Repo })
	if i < 0 {
		return nodeView{}, nil, api.Errorf(api.CodeNotFound, "no repo %s", req.Repo).WithSuggestion("hotbed repo add NAME URL, then hotbed step set NAME build --every-pull -- CMD")
	}
	repo := repos[i]
	rows, err := s.views(ctx)
	if err != nil {
		return nodeView{}, nil, err
	}
	var best *nodeView
	var bestWarm bool
	var bestFree int64
	for i := range rows {
		n := rows[i]
		if !n.online || n.hb == nil {
			continue
		}
		if req.Node != "" && n.node.Hostname != req.Node {
			continue
		}
		if len(repo.Hosts) > 0 && !slices.Contains(repo.Hosts, n.node.Hostname) {
			continue
		}
		warm := slices.ContainsFunc(n.bases, func(b *pb.Base) bool {
			return b.RepoId == req.Repo && b.State == pb.BaseState_BASE_STATE_WARM
		})
		r := n.hb
		free := r.GetResources().GetMemoryBytes() - max(r.GetUsage().GetMemoryUsedBytes(), r.ReservedMemoryBytes)
		if best == nil || (warm && !bestWarm) || (warm == bestWarm && free > bestFree) {
			best, bestWarm, bestFree = &rows[i], warm, free
		}
	}
	if best == nil {
		e := &api.Error{Code: api.CodeWorkerUnreachable, Message: "no online node hosts " + req.Repo, Retryable: true}
		if req.Node != "" {
			e.Message = req.Node + " is not online or does not host " + req.Repo
		}
		return nodeView{}, nil, e.WithSuggestion("hotbed nodes lists the workers and whether they are online")
	}
	return *best, repo, nil
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

// GetWorkspace asks the workspace's worker, or, when it is out of reach,
// answers from its node's view.
func (s *workspaces) GetWorkspace(ctx context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
	c, host, w, err := s.workspaceOf(ctx, req.Id)
	if err == nil {
		var ws *pb.Workspace
		if ws, err = c.GetWorkspace(ctx, req); err == nil {
			ws.NodeAddr = s.nodeAddr(ctx, w.node)
			return ws, nil
		}
		err = fromWorker(host, err)
	}
	if unreachable(err) {
		return s.viewWorkspace(ctx, req.Id)
	}
	return nil, err
}

// WatchWorkspace relays a workspace's changes from its worker. Watching
// counts as using the session: a first base can take long to freshen.
func (s *workspaces) WatchWorkspace(ctx context.Context, req *pb.WatchWorkspaceRequest, out hotbedv1connect.WorkspaceServiceWatchWorkspaceServerStream) error {
	c, host, w, err := s.workspaceOf(ctx, req.Id)
	if err != nil {
		return err
	}
	in, err := c.WatchWorkspace(ctx, req)
	if err != nil {
		return fromWorker(host, err)
	}
	defer in.Close()
	return relay(ctx, s.Server, w.session, host, in.Receive, out.Send)
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
// alive), or of every session, as their nodes' views have them.
func (s *workspaces) ListWorkspaces(ctx context.Context, req *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	if req.SessionId != "" {
		if _, err := s.cfg.Store.LiveSession(ctx, req.SessionId, s.now()); err != nil {
			return nil, err
		}
	}
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.ListWorkspacesResponse{}
	for _, nv := range nvs {
		for _, ws := range nv.workspaces {
			if ws.State != pb.WorkspaceState_WORKSPACE_STATE_RELEASED && (req.SessionId == "" || ws.SessionId == req.SessionId) {
				out.Workspaces = append(out.Workspaces, ws)
			}
		}
	}
	slices.SortFunc(out.Workspaces, func(a, b *pb.Workspace) int { return cmp.Compare(a.CreatedAt, b.CreatedAt) })
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

// GetCommand asks the command's worker, or, when it is out of reach,
// answers from its node's view.
func (s *workspaces) GetCommand(ctx context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
	c, host, err := s.commandOf(ctx, req.Id)
	if err == nil {
		var cmd *pb.Command
		if cmd, err = c.GetCommand(ctx, req); err == nil {
			cmd.NodeAddr = s.nodeAddr(ctx, cmd.NodeId)
			return cmd, nil
		}
		err = fromWorker(host, err)
	}
	if unreachable(err) {
		return s.viewCommand(ctx, req.Id)
	}
	return nil, err
}

// ListCommands lists the live and just-ended commands of a session (which
// it keeps alive), or of every session, as their nodes' views have them.
func (s *workspaces) ListCommands(ctx context.Context, req *pb.ListCommandsRequest) (*pb.ListCommandsResponse, error) {
	if req.SessionId != "" {
		if err := s.cfg.Store.Touch(ctx, req.SessionId, s.now()); err != nil {
			return nil, err
		}
	}
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.ListCommandsResponse{}
	for _, nv := range nvs {
		for _, c := range nv.commands {
			if c.Kind != "freshen" && (req.SessionId == "" || c.SessionId == req.SessionId) {
				out.Commands = append(out.Commands, c)
			}
		}
	}
	slices.SortFunc(out.Commands, func(a, b *pb.Command) int { return strings.Compare(a.Id, b.Id) })
	slices.SortStableFunc(out.Commands, func(a, b *pb.Command) int { return cmp.Compare(a.QueuedAt, b.QueuedAt) })
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
	return relay(ctx, s.Server, session, host, in.Receive, out.Send)
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
