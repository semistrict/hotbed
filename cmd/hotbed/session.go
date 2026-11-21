package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

type sessionCmd struct {
	Start sessionStartCmd `cmd:"" help:"Start a session; prints its name. Call hotbed wait at least hourly, or it is abandoned."`
	End   sessionEndCmd   `cmd:"" help:"End a session: stop its commands and release its workspaces."`
}

type sessionStartCmd struct {
	controllerFlag
	Name  string `arg:"" optional:"" help:"adjective-noun, such as brave-otter (default: a random free one)."`
	Agent string `env:"HOTBED_AGENT" help:"Who you are, for the cockpit (default: this machine)."`
}

func (c *sessionStartCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.sessions.StartSession(ctx, &pb.StartSessionRequest{Id: c.Name, Agent: c.Agent})
}

type sessionEndCmd struct {
	controllerFlag
	Session string `arg:"" help:"The session's name."`
}

func (c *sessionEndCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.sessions.EndSession(ctx, &pb.EndSessionRequest{Id: c.Session})
}

type addCmd struct {
	controllerFlag
	Session string `arg:"" help:"The session's name."`
	Repo    string `arg:"" help:"The repo's name, from hotbed repo ls."`
	Sync    string `type:"path" help:"A local checkout of the repo the workspace follows: run from this machine syncs it first. Without it the workspace is remote-only."`
	Node    string `help:"Put the workspace on this node (host name)."`
	Branch  string `help:"The branch you work on, for the cockpit."`
}

func (c *addCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	ws, err := cl.workspaces.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		SessionId: c.Session, Repo: c.Repo, SyncDir: c.Sync, Node: c.Node, Branch: c.Branch,
	})
	if err != nil {
		return nil, err
	}
	if c.Sync != "" {
		if _, err := syncTo(ctx, cl, ws, c.Sync); err != nil {
			return nil, err
		}
		if ws, err = cl.workspaces.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: ws.Id}); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

// workspace is the session's workspace of a repo (or with that ID).
func workspace(ctx context.Context, cl clients, session, repo string) (*pb.Workspace, error) {
	list, err := cl.workspaces.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{SessionId: session})
	if err != nil {
		return nil, err
	}
	for _, w := range list.Workspaces {
		if w.RepoId == repo || w.Id == repo {
			return cl.workspaces.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: w.Id})
		}
	}
	return nil, api.Errorf(api.CodeNotFound, "session %s has no workspace of %s", session, repo).
		WithSuggestion(fmt.Sprintf("hotbed add %s %s (with --sync DIR to follow a local checkout)", session, repo))
}

type syncCmd struct {
	controllerFlag
	Session string `arg:"" help:"The session's name."`
	Repo    string `arg:"" help:"The repo's name."`
	Dir     string `type:"path" help:"The checkout to send (default: the one the workspace follows)."`
}

func (c *syncCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	ws, err := workspace(ctx, cl, c.Session, c.Repo)
	if err != nil {
		return nil, err
	}
	dir := c.Dir
	if dir == "" {
		dir = ws.SyncDir
	}
	if dir == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "the workspace follows no checkout").WithSuggestion("pass --dir CHECKOUT")
	}
	return syncTo(ctx, cl, ws, dir)
}

const syncChunk = 256 << 10

// syncTo sends the checkout's state to a workspace, unless the workspace
// already has exactly this tree.
func syncTo(ctx context.Context, cl clients, ws *pb.Workspace, dir string) (*pb.SyncResponse, error) {
	snap, err := gitops.Snapshot(ctx, dir)
	if err != nil {
		return nil, err
	}
	if snap.Tree == ws.Tree {
		return &pb.SyncResponse{WorkspaceId: ws.Id, Head: ws.Head, Tree: ws.Tree, FilesChanged: ws.FilesChanged, Skipped: true}, nil
	}
	var bundle bytes.Buffer
	if err := gitops.Bundle(ctx, snap, ws.BaseCommit, &bundle); err != nil {
		return nil, err
	}
	stream, err := cl.workspaces.Sync(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&pb.SyncRequest{Msg: &pb.SyncRequest_Header{Header: &pb.SyncHeader{WorkspaceId: ws.Id, Commit: snap.Commit, Tree: snap.Tree}}}); err != nil {
		_, cerr := stream.CloseAndReceive()
		return nil, errors.Join(err, cerr)
	}
	for b := bundle.Bytes(); len(b) > 0; {
		n := min(len(b), syncChunk)
		if err := stream.Send(&pb.SyncRequest{Msg: &pb.SyncRequest_Chunk{Chunk: b[:n]}}); err != nil {
			break // the server failed; CloseAndReceive says why
		}
		b = b[n:]
	}
	return stream.CloseAndReceive()
}

type runCmd struct {
	controllerFlag
	Session   string   `arg:"" help:"The session's name."`
	Repo      string   `arg:"" help:"The repo whose workspace to run in."`
	Args      []string `arg:"" passthrough:"" help:"The command, after --."`
	NoSync    bool     `help:"Do not sync the checkout the workspace follows first."`
	Detach    bool     `help:"Return once the command is queued; it runs until it ends or hotbed stop. For dev servers and long jobs; follow with hotbed wait."`
	CPUWeight int      `default:"100" help:"Share of contended CPU (cgroup cpu.weight, 1-10000)."`
	MemLow    string   `default:"0" help:"Memory guaranteed to the command (memory.low), such as 2G; it waits until that fits."`
	MemHigh   string   `default:"0" help:"Memory past which it is throttled and swapped (memory.high), such as 8G; 0 for none."`
	Port      int      `help:"Port a dev server listens on, for its URL."`
	Env       []string `help:"KEY=VALUE for the command. Repeatable."`
	Kind      string   `enum:",build,test,check,server,shell" default:"" help:"What it is, for history: build, test, check, server or shell (default: guessed)."`
}

// Raw syncs if the workspace follows a checkout on this machine, runs the
// command, and streams its output to stdout. Interrupting stops it.
func (c *runCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	// kong's passthrough keeps the -- that ends hotbed's own flags.
	if len(c.Args) > 0 && c.Args[0] == "--" {
		c.Args = c.Args[1:]
	}
	if len(c.Args) == 0 {
		return 0, false, api.Errorf(api.CodeInvalidArgs, "no command to run").WithSuggestion("hotbed run SESSION REPO -- cargo test")
	}
	low, err := parseSize(c.MemLow)
	if err != nil {
		return 0, false, err
	}
	high, err := parseSize(c.MemHigh)
	if err != nil {
		return 0, false, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return 0, false, err
	}
	stream, err := c.clients(d.stream)
	if err != nil {
		return 0, false, err
	}
	ws, err := workspace(ctx, cl, c.Session, c.Repo)
	if err != nil {
		return 0, false, err
	}
	if !c.NoSync && ws.SyncDir != "" {
		self, err := d.tailnet.Self(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(d.stderr, "hotbed: not syncing: %v\n", err)
		case self.Hostname == ws.SyncHost:
			res, err := syncTo(ctx, cl, ws, ws.SyncDir)
			if err != nil {
				return 0, false, err
			}
			if !res.Skipped {
				fmt.Fprintf(d.stderr, "hotbed: synced %d changed files (%s) from %s\n", res.FilesChanged, size(res.BundleBytes), ws.SyncDir)
			}
		}
	}
	cmd, err := cl.workspaces.RunCommand(ctx, &pb.RunCommandRequest{
		WorkspaceId: ws.Id, Args: c.Args, Env: c.Env, Kind: c.Kind, Attached: !c.Detach, Port: int32(c.Port),
		Limits: &pb.CommandLimits{CpuWeight: int32(c.CPUWeight), MemoryLowBytes: low, MemoryHighBytes: high},
	})
	if err != nil {
		return 0, false, err
	}
	if c.Detach {
		writeEnvelope(d.stdout, success("run", cmd))
		return exitOK, true, nil
	}
	fmt.Fprintf(d.stderr, "hotbed: %s running in %s (hotbed stop %s %s)\n", cmd.Id, ws.Id, c.Session, cmd.Id)
	code, err := follow(ctx, cl, stream, cmd.Id, d.stdout)
	if ctx.Err() != nil {
		// Interrupted: an attached command stops with its client.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := cl.workspaces.StopCommand(stopCtx, &pb.StopCommandRequest{Id: cmd.Id}); err != nil {
			fmt.Fprintf(d.stderr, "hotbed: could not stop %s: %v\n", cmd.Id, api.FromConnect(err))
		}
		return 130, true, nil
	}
	if err != nil {
		return 0, true, err
	}
	return code, true, nil
}

func (c *runCmd) Exec(context.Context, deps) (any, error) { return nil, errors.New("run is raw") }

// follow copies a command's output to w until it ends, picking up where it
// left off if the stream drops, and returns its exit status.
func follow(ctx context.Context, cl, stream clients, id string, w io.Writer) (int, error) {
	var offset int64
	for {
		in, err := stream.workspaces.FollowLog(ctx, &pb.FollowLogRequest{Id: id, Offset: offset, Follow: true})
		if err != nil {
			return 0, err
		}
		for {
			chunk, err := in.Receive()
			if err != nil {
				break
			}
			n, werr := w.Write(chunk.Data)
			offset += int64(n)
			if werr != nil {
				in.Close()
				return 0, werr
			}
		}
		in.Close()
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		c, err := cl.workspaces.GetCommand(ctx, &pb.GetCommandRequest{Id: id})
		if err != nil {
			return 0, err
		}
		if live(c.State) {
			continue
		}
		switch {
		case c.ExitCode != nil:
			return int(*c.ExitCode), nil
		case c.State == pb.CommandState_COMMAND_STATE_CANCELLED:
			return 0, api.Errorf(api.CodeInternal, "%s was cancelled: %s", id, c.Reason)
		case strings.HasPrefix(c.Reason, "killed by signal "):
			// Report it the way a shell would: 128 plus the signal.
			sig, _ := strconv.Atoi(strings.TrimPrefix(c.Reason, "killed by signal "))
			return 128 + sig, nil
		default:
			return 0, api.Errorf(api.CodeInternal, "%s failed: %s", id, c.Reason)
		}
	}
}

func live(s pb.CommandState) bool {
	return s == pb.CommandState_COMMAND_STATE_QUEUED || s == pb.CommandState_COMMAND_STATE_RUNNING || s == pb.CommandState_COMMAND_STATE_PARKED
}

type waitCmd struct {
	controllerFlag
	Session string `arg:"" help:"The session's name."`
	After   int64  `default:"-1" help:"Return events after this cursor (default: after the last ones this machine saw)."`
	Timeout int    `default:"120" help:"Seconds to wait for an event, at most 120."`
}

// Exec long-polls the session. It remembers the cursor per session on this
// machine, so repeated hotbed wait calls return each event once.
func (c *waitCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	cursorFile := filepath.Join(d.cache, "sessions", c.Session+".cursor")
	after := c.After
	if after < 0 {
		after = 0
		if b, err := os.ReadFile(cursorFile); err == nil {
			after, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		}
	}
	res, err := cl.sessions.WaitSession(ctx, &pb.WaitSessionRequest{SessionId: c.Session, After: after, TimeoutSeconds: int32(c.Timeout)})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cursorFile), 0o755); err == nil {
		if err := os.WriteFile(cursorFile, []byte(strconv.FormatInt(res.Cursor, 10)), 0o644); err != nil {
			fmt.Fprintf(d.stderr, "hotbed: could not save the cursor: %v\n", err)
		}
	}
	return res, nil
}

type psCmd struct {
	controllerFlag
	Session string `arg:"" help:"The session's name."`
}

func (c *psCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.workspaces.ListCommands(ctx, &pb.ListCommandsRequest{SessionId: c.Session})
}

// sessionCommand is a command of the session, or NOT_FOUND.
func sessionCommand(ctx context.Context, cl clients, session, id string) (*pb.Command, error) {
	cmd, err := cl.workspaces.GetCommand(ctx, &pb.GetCommandRequest{Id: id})
	if err != nil {
		return nil, err
	}
	if cmd.SessionId != session {
		return nil, api.Errorf(api.CodeNotFound, "session %s has no command %s", session, id)
	}
	return cmd, nil
}

type logsCmd struct {
	controllerFlag
	Session string `arg:"" help:"The session's name."`
	Command string `arg:"" help:"The command's ID."`
	Follow  bool   `short:"f" help:"Keep printing until the command ends."`
}

// Raw prints the command's output as is.
func (c *logsCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return 0, false, err
	}
	stream, err := c.clients(d.stream)
	if err != nil {
		return 0, false, err
	}
	if _, err := sessionCommand(ctx, cl, c.Session, c.Command); err != nil {
		return 0, false, err
	}
	in, err := stream.workspaces.FollowLog(ctx, &pb.FollowLogRequest{Id: c.Command, Follow: c.Follow})
	if err != nil {
		return 0, false, err
	}
	defer in.Close()
	for {
		chunk, err := in.Receive()
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return exitOK, true, nil
		}
		if err != nil {
			return 0, true, err
		}
		if _, err := d.stdout.Write(chunk.Data); err != nil {
			return 0, true, err
		}
	}
}

func (c *logsCmd) Exec(context.Context, deps) (any, error) { return nil, errors.New("logs is raw") }

type stopCmd struct {
	controllerFlag
	Session string `arg:"" help:"The session's name."`
	Command string `arg:"" help:"The command's ID."`
}

func (c *stopCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	if _, err := sessionCommand(ctx, cl, c.Session, c.Command); err != nil {
		return nil, err
	}
	return cl.workspaces.StopCommand(ctx, &pb.StopCommandRequest{Id: c.Command})
}

// parseSize reads 512M, 2G, 1.5G or a plain byte count.
func parseSize(s string) (int64, error) {
	s = strings.TrimSuffix(strings.TrimSpace(strings.ToUpper(s)), "B")
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		m      int64
	}{{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40}} {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = v, u.m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, api.Errorf(api.CodeInvalidArgs, "size %q is not like 512M or 2G", s)
	}
	return int64(f * float64(mult)), nil
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
