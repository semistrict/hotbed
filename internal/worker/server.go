package worker

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/settle"
	"google.golang.org/protobuf/proto"
)

// Service serves the engine's workspaces to clients, which call it directly
// once the controller says where a workspace is, and to the controller,
// which forwards older clients' calls. Each call about a session's
// workspace or command counts as using the session, which the next
// heartbeat tells the controller.
type Service struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	E *Engine
}

func notFound(kind, id string) *api.Error {
	return api.Errorf(api.CodeNotFound, "no %s %s on this node", kind, id)
}

func (s *Service) isController(ctx context.Context) bool {
	if !s.E.cfg.RequireController && s.E.cfg.ControllerNodeID == "" {
		return true
	}
	peer := httpx.Peer(ctx)
	return s.E.cfg.ControllerNodeID != "" && peer.NodeID != "" && peer.NodeID == s.E.cfg.ControllerNodeID && !httpx.IsLocalNonRoot(ctx)
}

// checkInterventionTarget validates req.Id and, when sessionID is non-empty,
// verifies that the command belongs to that session so a caller cannot spoof
// another session's name in the intervention audit stamp. Sessionless calls
// (sessionID == "", e.g. cockpit/human) are allowed for any command.
func (s *Service) checkInterventionTarget(id, sessionID string) error {
	cmd, ok := s.E.Command(id)
	if !ok {
		if err := validateID(id); err != nil {
			return err
		}
		return notFound("command", id)
	}
	if sessionID != "" && cmd.SessionId != sessionID {
		return api.Errorf(api.CodeNotFound, "session %s has no command %s", sessionID, id)
	}
	return nil
}

func (s *Service) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	if !s.isController(ctx) {
		return nil, api.Errorf(api.CodeNotAuthorized, "only the controller may create workspaces on this worker")
	}
	return s.E.CreateWorkspace(ctx, req)
}

func (s *Service) GetWorkspace(_ context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
	if err := validateID(req.Id); err != nil {
		return nil, err
	}
	s.E.usedWorkspace(req.Id)
	return s.E.Workspace(req.Id)
}

func (s *Service) ListWorkspaces(context.Context, *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	return &pb.ListWorkspacesResponse{Workspaces: s.E.Workspaces()}, nil
}

func (s *Service) ReleaseWorkspace(ctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
	if err := validateID(req.Id); err != nil {
		return nil, err
	}
	s.E.usedWorkspace(req.Id)
	return &pb.ReleaseWorkspaceResponse{}, s.E.ReleaseWorkspace(ctx, req.Id)
}

// Sync reads the header, then hands the rest of the stream to the engine as
// the bundle's bytes.
func (s *Service) Sync(ctx context.Context, stream hotbedv1connect.WorkspaceServiceSyncServerStream) (*pb.SyncResponse, error) {
	first, err := stream.Receive()
	if err != nil {
		return nil, err
	}
	h := first.GetHeader()
	if h == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "sync must start with a header")
	}
	if err := validateID(h.WorkspaceId); err != nil {
		return nil, err
	}
	s.E.usedWorkspace(h.WorkspaceId)
	return s.E.Sync(ctx, h, &chunks{recv: stream.Receive})
}

// chunks reads a stream of SyncRequest chunks as one byte stream.
type chunks struct {
	recv func() (*pb.SyncRequest, error)
	buf  []byte
}

func (c *chunks) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		m, err := c.recv()
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		c.buf = m.GetChunk()
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (s *Service) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	if err := validateID(req.WorkspaceId); err != nil {
		return nil, err
	}
	s.E.usedWorkspace(req.WorkspaceId)
	files, err := s.E.Files(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	return &pb.ListFilesResponse{Files: files}, nil
}

func (s *Service) GetFile(ctx context.Context, req *pb.GetFileRequest) (*pb.FileContent, error) {
	if err := validateID(req.WorkspaceId); err != nil {
		return nil, err
	}
	s.E.usedWorkspace(req.WorkspaceId)
	return s.E.File(ctx, req.WorkspaceId, req.Path)
}

func (s *Service) RunCommand(ctx context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
	if err := validateID(req.WorkspaceId); err != nil {
		return nil, err
	}
	if req.Id != "" {
		if err := validateID(req.Id); err != nil {
			return nil, err
		}
	}
	s.E.usedWorkspace(req.WorkspaceId)
	return s.E.Exec(ctx, req)
}

func (s *Service) OpenShell(ctx context.Context, req *pb.OpenShellRequest) (*pb.OpenShellResponse, error) {
	if err := validateID(req.WorkspaceId); err != nil {
		return nil, err
	}
	s.E.usedWorkspace(req.WorkspaceId)
	return s.E.OpenShell(ctx, req)
}

func (s *Service) GetCommand(_ context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
	if err := validateID(req.Id); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	c, ok := s.E.Command(req.Id)
	if !ok {
		return nil, notFound("command", req.Id)
	}
	return c, nil
}

// ListCommands lists the node's commands. History, searching and limits
// are the controller's: the worker refuses them rather than ignore them.
func (s *Service) ListCommands(_ context.Context, req *pb.ListCommandsRequest) (*pb.ListCommandsResponse, error) {
	if req.Ended || req.Match != "" || req.Limit != nil || req.Project != "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "a worker lists its commands by session alone: ask the controller for history, projects, matches or limits")
	}
	out := &pb.ListCommandsResponse{}
	for _, c := range s.E.Commands() {
		if req.SessionId == "" || c.SessionId == req.SessionId {
			out.Commands = append(out.Commands, c)
		}
	}
	return out, nil
}

func (s *Service) StopCommand(ctx context.Context, req *pb.StopCommandRequest) (*pb.Command, error) {
	if req.Internal {
		if !s.isController(ctx) {
			return nil, api.Errorf(api.CodeNotAuthorized, "only the controller may perform an internal stop")
		}
		if _, ok := s.E.Command(req.Id); !ok {
			if err := validateID(req.Id); err != nil {
				return nil, err
			}
		}
		s.E.usedCommand(req.Id)
		return s.E.Stop(ctx, req.Id, cmp.Or(req.Reason, "stopped by controller"))
	}
	if err := s.checkInterventionTarget(req.Id, req.SessionId); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	return s.E.StopWith(ctx, req.Id, Intervention{
		Comment:  req.Comment,
		Diagnose: req.Diagnose,
		Session:  req.SessionId,
		Host:     s.callerHost(ctx, req.Host),
	})
}

func (s *Service) PauseCommand(ctx context.Context, req *pb.PauseCommandRequest) (*pb.Command, error) {
	if err := s.checkInterventionTarget(req.Id, req.SessionId); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	return s.E.Pause(ctx, req.Id, Intervention{
		Comment:  req.Comment,
		Diagnose: req.Diagnose,
		Session:  req.SessionId,
		Host:     s.callerHost(ctx, req.Host),
	})
}

func (s *Service) ResumeCommand(ctx context.Context, req *pb.ResumeCommandRequest) (*pb.Command, error) {
	if err := s.checkInterventionTarget(req.Id, req.SessionId); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	return s.E.Resume(ctx, req.Id, Intervention{
		Comment: req.Comment,
		Session: req.SessionId,
		Host:    s.callerHost(ctx, req.Host),
	})
}

// callerHost is the host a call came from: the one the controller says
// (host) when it forwards the call, else the caller's own.
func (s *Service) callerHost(ctx context.Context, host string) string {
	if host != "" && s.isController(ctx) {
		return host
	}
	return httpx.Peer(ctx).Hostname
}

func (s *Service) streamsDone() <-chan struct{} {
	return s.E.streamsDone
}

// FollowLog streams a command's output from an offset. Following, it keeps
// the stream open until the command ends, which is how an attached exec
// shows output and keeps its command from counting as orphaned. It keeps the
// log file descriptor open across iterations and, on Linux, waits on inotify
// write events with a coarse fallback timer instead of re-opening on every tick.
func (s *Service) FollowLog(ctx context.Context, req *pb.FollowLogRequest, stream hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
	if err := validateID(req.Id); err != nil {
		return err
	}
	s.E.usedCommand(req.Id)
	if req.Wait && req.Follow {
		if err := s.E.WaitOn(ctx, req.Id); err != nil {
			return err
		}
	}
	path, done, err := s.E.Follow(req.Id)
	if err != nil {
		return err
	}
	defer done()

	var f *os.File
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	var watcher *logWatcher
	defer func() {
		if watcher != nil {
			watcher.Close()
		}
	}()

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	offset, follow := req.Offset, req.Follow
	startIvSeq := s.E.LastInterventionSeq(req.Id)
	var endState pb.CommandState
	var endedBy *pb.CommandIntervention
	var tsIdx *cmdlog.TimestampIndex
	buf := make([]byte, 64<<10)
	for {
		if f == nil {
			opened, err := os.Open(path)
			switch {
			case errors.Is(err, os.ErrNotExist):
				// Queued: no output yet.
			case err != nil:
				return err
			default:
				f = opened
				if follow {
					// Start watching before reading so writes during/after
					// the initial drain are not missed.
					watcher = newLogWatcher(path)
				}
			}
		}
		if f != nil {
			for {
				data, next, err := cmdlog.ReadChunk(f, path, offset, buf)
				if err != nil {
					return err
				}
				if len(data) == 0 {
					break
				}
				if req.Timestamps && next == offset+int64(len(data)) {
					if tsIdx == nil || next > tsIdx.MaxOffset() {
						tsIdx, _ = cmdlog.ReadTimestamps(path)
					}
					if err := tsIdx.SplitChunks(offset, data, func(chunkOff int64, chunk []byte, ts int64) error {
						return stream.Send(&pb.LogChunk{
							Data:       chunk,
							NextOffset: chunkOff + int64(len(chunk)),
							ReceivedAt: ts,
						})
					}); err != nil {
						return nil // the client went away
					}
				} else {
					if err := stream.Send(&pb.LogChunk{Data: data, NextOffset: next}); err != nil {
						return nil // the client went away
					}
				}
				offset = next
			}
		}
		if !follow {
			if req.Follow && (endState != pb.CommandState_COMMAND_STATE_UNSPECIFIED || endedBy != nil) {
				_ = stream.Send(&pb.LogChunk{NextOffset: offset, State: endState, EndedBy: endedBy})
			}
			return nil
		}
		going, state, by, updated := s.E.FollowState(req.Id, startIvSeq)
		if !going {
			// Capture state and intervention now, then do one more read to
			// drain output written between the last read and the command ending
			// or pausing.
			endState, endedBy = state, by
			follow = false
			continue
		}
		wait := 200 * time.Millisecond
		var watchCh <-chan struct{}
		if watcher != nil {
			watchCh = watcher.Events()
			wait = 2 * time.Second
		}
		if timer == nil {
			timer = time.NewTimer(wait)
		} else {
			timer.Reset(wait)
		}
		streamsDone := s.streamsDone()
		select {
		case <-streamsDone:
			return connect.NewError(connect.CodeUnavailable, "worker shutting down")
		default:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-streamsDone:
			return connect.NewError(connect.CodeUnavailable, "worker shutting down")
		case <-updated:
			timer.Stop()
		case <-watchCh:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// WatchWorkspace sends the workspace whenever it changes, until it is
// released or the caller goes.
func (s *Service) WatchWorkspace(ctx context.Context, req *pb.WatchWorkspaceRequest, stream hotbedv1connect.WorkspaceServiceWatchWorkspaceServerStream) error {
	if err := validateID(req.Id); err != nil {
		return err
	}
	s.E.usedWorkspace(req.Id)
	var sent *pb.Workspace
	streamsDone := s.streamsDone()
	for {
		ws, updated, err := s.E.WatchWorkspace(req.Id)
		if err != nil {
			return err
		}
		if sent == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
			if cmdID := s.E.WorkspaceFreshenCommand(req.Id); cmdID != "" {
				api.SetProgress(ctx, "/commands/"+cmdID)
			} else {
				api.SetProgress(ctx, "/workspaces/"+req.Id)
			}
		}
		if !proto.Equal(ws, sent) {
			if err := stream.Send(ws); err != nil {
				return nil // the caller went away
			}
			sent = ws
		}
		if ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
			return nil
		}
		select {
		case <-streamsDone:
			return connect.NewError(connect.CodeUnavailable, "worker shutting down")
		default:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-streamsDone:
			return connect.NewError(connect.CodeUnavailable, "worker shutting down")
		case <-updated:
		}
	}
}

// maxLogLinesBytes caps GetLogLines' response.
const maxLogLinesBytes = 1 << 20

func (s *Service) GetLogLines(_ context.Context, req *pb.GetLogLinesRequest) (*pb.CommandLog, error) {
	if err := validateID(req.Id); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	path, done, err := s.E.Follow(req.Id)
	if err != nil {
		return nil, err
	}
	done()
	tail := int(req.Tail)
	if tail <= 0 {
		tail = 2000
	}
	entries, cut, err := cmdlog.TailEntries(path, tail, maxLogLinesBytes)
	if err != nil {
		return nil, err
	}
	// As its terminal shows them: what a command in one redraws (progress,
	// colors) settled. A tail starts at a line's start, so it settles as the
	// whole output would, but for a redraw reaching above it. One that
	// redraws nothing is its lines as they are, however long.
	needsSettle := false
	for _, e := range entries {
		if strings.ContainsAny(e.Text, "\x1b\r\b") {
			needsSettle = true
			break
		}
	}
	var lines []string
	var timestamps []int64
	if needsSettle {
		sc := settle.NewTimed(func(line string, ts int64) {
			lines = append(lines, line)
			timestamps = append(timestamps, ts)
		})
		for i, e := range entries {
			if i > 0 {
				sc.WriteAt([]byte{'\n'}, entries[i-1].ReceivedAt)
			}
			sc.WriteAt([]byte(e.Text), e.ReceivedAt)
		}
		sc.Flush()
	} else {
		lines = make([]string, len(entries))
		timestamps = make([]int64, len(entries))
		for i, e := range entries {
			lines[i] = e.Text
			timestamps[i] = e.ReceivedAt
		}
	}
	return &pb.CommandLog{CommandId: req.Id, Lines: lines, Cut: cut, LineTimestamps: timestamps}, nil
}

func (s *Service) QueryOutput(ctx context.Context, req *pb.QueryOutputRequest) (*pb.OutputQuery, error) {
	if err := validateID(req.Id); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	return s.E.QueryOutput(ctx, req)
}

func (s *Service) DetachCommand(_ context.Context, req *pb.DetachCommandRequest) (*pb.Command, error) {
	if err := s.checkInterventionTarget(req.Id, req.SessionId); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	return s.E.Detach(req.Id)
}

func (s *Service) SendInput(ctx context.Context, req *pb.SendInputRequest) (*pb.SendInputResponse, error) {
	if err := s.checkInterventionTarget(req.Id, req.SessionId); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	data := req.Text
	if !req.NoNewline {
		data += "\n"
	}
	if err := s.E.SendInput(ctx, req.Id, []byte(data)); err != nil {
		return nil, err
	}
	return &pb.SendInputResponse{Bytes: int64(len(data))}, nil
}

func (s *Service) InspectCommand(ctx context.Context, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	if err := validateID(req.Id); err != nil {
		return nil, err
	}
	s.E.usedCommand(req.Id)
	return s.E.Inspect(ctx, req)
}

func (s *Service) TraceCommand(ctx context.Context, req *pb.TraceCommandRequest, stream hotbedv1connect.WorkspaceServiceTraceCommandServerStream) error {
	if err := validateID(req.Id); err != nil {
		return err
	}
	s.E.usedCommand(req.Id)
	return s.E.Trace(ctx, req, func(chunk *pb.TraceCommandChunk) error {
		return stream.Send(chunk)
	})
}

func (s *Service) SampleCommand(ctx context.Context, req *pb.SampleCommandRequest, stream hotbedv1connect.WorkspaceServiceSampleCommandServerStream) error {
	if err := validateID(req.Id); err != nil {
		return err
	}
	s.E.usedCommand(req.Id)
	return s.E.SampleCommand(ctx, req, httpx.Peer(ctx).Hostname, func(chunk *pb.SampleCommandChunk) error {
		return stream.Send(chunk)
	})
}

func (s *Service) GetProfile(_ context.Context, req *pb.GetProfileRequest) (*pb.CommandProfile, error) {
	switch t := req.GetTarget().(type) {
	case *pb.GetProfileRequest_CommandId:
		if err := validateID(t.CommandId); err != nil {
			return nil, err
		}
		s.E.usedCommand(t.CommandId)
		return s.E.CommandProfile(t.CommandId, req)
	case *pb.GetProfileRequest_Node:
		if strings.TrimSpace(t.Node) == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "node hostname or ID is required").
				WithSuggestion("pass a node name from `hotbed nodes`")
		}
		return s.E.NodeProfile(t.Node, req)
	default:
		return nil, api.Errorf(api.CodeInvalidArgs, "profile target (command_id or node) is required").
			WithSuggestion("pass a command ID (`hotbed profile CMD`) or a node (`hotbed profile --node HOST`)")
	}
}

func (s *Service) GetDaemonLogs(_ context.Context, req *pb.GetDaemonLogsRequest) (*pb.GetDaemonLogsResponse, error) {
	return s.E.DaemonLogs(req)
}

func (s *Service) UpgradeSystem(ctx context.Context, req *pb.WorkerUpgradeSystemRequest) (*pb.NodeSystemUpgrade, error) {
	if !s.isController(ctx) {
		return nil, api.Errorf(api.CodeNotAuthorized, "only the controller may run system upgrades on this worker")
	}
	return s.E.UpgradeSystem(ctx, req)
}
