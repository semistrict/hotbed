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
	NoWait  bool   `help:"Return while the workspace is still preparing (its base being pulled and freshened); commands run in it queue until it is ready."`
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
	if c.NoWait {
		return ws, nil
	}
	stream, err := c.clients(d.stream)
	if err != nil {
		return nil, err
	}
	if ws, err = waitReady(ctx, stream, ws, d.stderr); err != nil {
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

// waitReady waits for a preparing workspace (its base being pulled and
// freshened) to be ready, watching it on a stream (stream's clients, which
// have no timeout) and saying what it waits for on stderr.
func waitReady(ctx context.Context, stream clients, ws *pb.Workspace, stderr io.Writer) (*pb.Workspace, error) {
	said := ""
	var lost time.Time // when the watch first dropped, of the drops since it last worked
	for ws.State == pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
		in, err := stream.workspaces.WatchWorkspace(ctx, &pb.WatchWorkspaceRequest{Id: ws.Id})
		if err == nil {
			for ws.State == pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
				if ws.Reason != said {
					fmt.Fprintf(stderr, "hotbed: %s is preparing: %s\n", ws.Id, ws.Reason)
					said = ws.Reason
				}
				var next *pb.Workspace
				if next, err = in.Receive(); err != nil {
					break
				}
				ws, lost = next, time.Time{}
			}
			in.Close()
		}
		if ws.State != pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The watch ends while the workspace still prepares when the controller or its
		// worker restarts (a deploy): watch again, unless the workspace is gone or the
		// cluster stays out of reach.
		if e := api.FromConnect(err); e != nil && e.Code == api.CodeNotFound {
			return nil, e
		}
		switch {
		case lost.IsZero():
			lost = time.Now()
			fmt.Fprintf(stderr, "hotbed: lost the watch of %s (%v); watching again\n", ws.Id, err)
		case time.Since(lost) > 2*time.Minute:
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
		return nil, api.Errorf(api.CodeNotFound, "workspace %s was released while it prepared", ws.Id)
	}
	if ws.State != pb.WorkspaceState_WORKSPACE_STATE_FAILED && ws.Reason != "" {
		fmt.Fprintf(stderr, "hotbed: warning: %s %s\n", ws.Id, ws.Reason)
	}
	if ws.State == pb.WorkspaceState_WORKSPACE_STATE_FAILED {
		return nil, api.Errorf(api.CodeWorkspaceFailed, "workspace %s failed: %s", ws.Id, ws.Reason).
			WithSuggestion("fix the failing step (hotbed step ls REPO, then hotbed step add), and add the repo again")
	}
	return ws, nil
}

// watchSession returns the session's first update with events (or its end),
// or how it stands when the watch times out.
func watchSession(ctx context.Context, stream clients, req *pb.WatchSessionRequest) (*pb.WatchSessionResponse, error) {
	in, err := stream.sessions.WatchSession(ctx, req)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	var last *pb.WatchSessionResponse
	for {
		res, err := in.Receive()
		if errors.Is(err, io.EOF) && last != nil {
			return last, nil
		}
		if err != nil {
			return nil, err
		}
		if len(res.Events) > 0 || res.Ended {
			return res, nil
		}
		last = res
	}
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

// commandFlags are what a command is run with, by hotbed run and by a
// scheduled command alike.
type commandFlags struct {
	CPUWeight int      `default:"100" help:"Share of contended CPU (cgroup cpu.weight, 1-10000)."`
	MemLow    string   `default:"0" help:"Memory guaranteed to the command (memory.low), such as 2G; it waits until that fits."`
	MemHigh   string   `default:"0" help:"Memory past which it is throttled and swapped (memory.high), such as 8G; 0 for none."`
	Env       []string `help:"KEY=VALUE for the command. Repeatable."`
	Kind      string   `enum:",build,test,check,server,shell" default:"" help:"What it is, for history: build, test, check, server or shell (default: guessed)."`
	Bench     bool     `help:"Run it as a measurement: alone among measurements on the node, once running work leaves it room, on the node's measurement CPUs, which no other command uses meanwhile, isolated for its whole run (cache, memory bandwidth, IRQs, others' clocks and disk IO where the node has them), never swapped or parked. It fails if the isolation was lost."`
}

// request is the command args (kong's passthrough, with or without its
// leading --) run with these flags, in no workspace yet.
func (f commandFlags) request(args []string, example string) (*pb.RunCommandRequest, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "no command to run").WithSuggestion(example)
	}
	low, err := parseSize(f.MemLow)
	if err != nil {
		return nil, err
	}
	high, err := parseSize(f.MemHigh)
	if err != nil {
		return nil, err
	}
	return &pb.RunCommandRequest{
		Args: args, Env: f.Env, Kind: f.Kind, Bench: f.Bench,
		Limits: &pb.CommandLimits{CpuWeight: int32(f.CPUWeight), MemoryLowBytes: low, MemoryHighBytes: high},
	}, nil
}

type runCmd struct {
	controllerFlag
	Session string   `arg:"" help:"The session's name."`
	Repo    string   `arg:"" help:"The repo whose workspace to run in."`
	Args    []string `arg:"" passthrough:"" help:"The command, after --."`
	NoSync  bool     `help:"Do not sync the checkout the workspace follows first."`
	Detach  bool     `help:"Return once the command is queued; it runs until it ends or hotbed stop. For dev servers and long jobs; follow with hotbed wait."`
	Port    int      `help:"Port a dev server listens on, for its URL."`
	commandFlags
}

// Raw syncs if the workspace follows a checkout on this machine, runs the
// command, and streams its output to stdout. Interrupting stops it.
func (c *runCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	req, err := c.request(c.Args, "hotbed run SESSION REPO -- cargo test")
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
			if ws, err = waitReady(ctx, stream, ws, d.stderr); err != nil {
				return 0, false, err
			}
			res, err := syncTo(ctx, cl, ws, ws.SyncDir)
			if err != nil {
				return 0, false, err
			}
			if !res.Skipped {
				fmt.Fprintf(d.stderr, "hotbed: synced %d changed files (%s) from %s\n", res.FilesChanged, size(res.BundleBytes), ws.SyncDir)
			}
		}
	}
	req.WorkspaceId, req.Attached, req.Port = ws.Id, !c.Detach, int32(c.Port)
	cmd, err := cl.workspaces.RunCommand(ctx, req)
	if err != nil {
		return 0, false, err
	}
	if c.Detach {
		writeEnvelope(d.stdout, success("run", cmd))
		return exitOK, true, nil
	}
	fmt.Fprintf(d.stderr, "hotbed: %s running in %s (hotbed stop %s %s)\n", cmd.Id, ws.Id, c.Session, cmd.Id)
	code, err := attach(ctx, cl, stream, cmd.Id, d.stdout, d.stderr)
	return code, true, err
}

// attach prints an attached command's output to w until it ends and returns
// its exit status. Interrupted, it stops the command, which ends with its
// client, and returns 130 as a shell would.
func attach(ctx context.Context, cl, stream clients, id string, w, stderr io.Writer) (int, error) {
	code, err := follow(ctx, cl, stream, id, w)
	if ctx.Err() != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := cl.workspaces.StopCommand(stopCtx, &pb.StopCommandRequest{Id: id}); err != nil {
			fmt.Fprintf(stderr, "hotbed: could not stop %s: %v\n", id, api.FromConnect(err))
		}
		return 130, nil
	}
	return code, err
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

// Exec watches the session until something happens. It remembers the cursor per session on this
// machine, so repeated hotbed wait calls return each event once.
func (c *waitCmd) Exec(ctx context.Context, d deps) (any, error) {
	stream, err := c.clients(d.stream)
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
	res, err := watchSession(ctx, stream, &pb.WatchSessionRequest{SessionId: c.Session, After: after, TimeoutSeconds: int32(c.Timeout)})
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
