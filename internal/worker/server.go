package worker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"google.golang.org/protobuf/proto"
)

// Service serves the engine's workspaces to the controller, which forwards
// clients' calls about workspaces and commands on this node here.
type Service struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	E *Engine
}

func notFound(kind, id string) *api.Error {
	return api.Errorf(api.CodeNotFound, "no %s %s on this node", kind, id)
}

func (s *Service) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	return s.E.CreateWorkspace(ctx, req)
}

func (s *Service) GetWorkspace(_ context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
	return s.E.Workspace(req.Id)
}

func (s *Service) ListWorkspaces(context.Context, *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	return &pb.ListWorkspacesResponse{Workspaces: s.E.Workspaces()}, nil
}

func (s *Service) ReleaseWorkspace(ctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
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
	files, err := s.E.Files(ctx, req.WorkspaceId)
	if err != nil {
		return nil, err
	}
	return &pb.ListFilesResponse{Files: files}, nil
}

func (s *Service) GetFile(ctx context.Context, req *pb.GetFileRequest) (*pb.FileContent, error) {
	return s.E.File(ctx, req.WorkspaceId, req.Path)
}

func (s *Service) RunCommand(ctx context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
	return s.E.Exec(ctx, req)
}

func (s *Service) GetCommand(_ context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
	c, ok := s.E.Command(req.Id)
	if !ok {
		return nil, notFound("command", req.Id)
	}
	return c, nil
}

func (s *Service) ListCommands(_ context.Context, req *pb.ListCommandsRequest) (*pb.ListCommandsResponse, error) {
	out := &pb.ListCommandsResponse{}
	for _, c := range s.E.Commands() {
		if req.SessionId == "" || c.SessionId == req.SessionId {
			out.Commands = append(out.Commands, c)
		}
	}
	return out, nil
}

func (s *Service) StopCommand(ctx context.Context, req *pb.StopCommandRequest) (*pb.Command, error) {
	return s.E.Stop(ctx, req.Id, "stopped by "+httpx.Peer(ctx).Hostname)
}

// FollowLog streams a command's output from an offset. Following, it keeps
// the stream open until the command ends, which is how an attached exec
// shows output and keeps its command from counting as orphaned.
func (s *Service) FollowLog(ctx context.Context, req *pb.FollowLogRequest, stream hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
	path, done, err := s.E.Follow(req.Id)
	if err != nil {
		return err
	}
	defer done()
	offset, follow := req.Offset, req.Follow
	buf := make([]byte, 64<<10)
	for {
		f, err := os.Open(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Queued: no output yet.
		case err != nil:
			return err
		default:
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				f.Close()
				return err
			}
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					if err := stream.Send(&pb.LogChunk{Data: buf[:n]}); err != nil {
						f.Close()
						return nil // the client went away
					}
					offset += int64(n)
				}
				if rerr != nil {
					break
				}
			}
			f.Close()
		}
		if !follow {
			return nil
		}
		live, updated := s.E.Live(req.Id)
		if !live {
			// One more read catches output written between the last read
			// and the command ending.
			follow = false
			continue
		}
		// Output is read every 200ms; the command's end is seen at once.
		select {
		case <-ctx.Done():
			return nil
		case <-updated:
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// WatchWorkspace sends the workspace whenever it changes, until it is
// released or the caller goes.
func (s *Service) WatchWorkspace(ctx context.Context, req *pb.WatchWorkspaceRequest, stream hotbedv1connect.WorkspaceServiceWatchWorkspaceServerStream) error {
	var sent *pb.Workspace
	for {
		ws, updated, err := s.E.WatchWorkspace(req.Id)
		if err != nil {
			return err
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
		case <-ctx.Done():
			return nil
		case <-updated:
		}
	}
}

func (s *Service) GetLogLines(_ context.Context, req *pb.GetLogLinesRequest) (*pb.CommandLog, error) {
	path, done, err := s.E.Follow(req.Id)
	if err != nil {
		return nil, err
	}
	done()
	tail := int(req.Tail)
	if tail <= 0 {
		tail = 2000
	}
	lines, err := tailLines(path, tail)
	if err != nil {
		return nil, err
	}
	return &pb.CommandLog{CommandId: req.Id, Lines: lines}, nil
}

// tailLines returns the last n lines of a file.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
		if len(lines) > 2*n {
			lines = append(lines[:0], lines[len(lines)-n:]...)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if lines == nil {
		lines = []string{}
	}
	return lines, sc.Err()
}
