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
	Start   sessionStartCmd   `cmd:"" help:"Start a session and bind it to this T3 thread or git worktree in ~/.hotbed/sessions.json. Call hotbed wait at least hourly, or it is abandoned."`
	Status  sessionStatusCmd  `cmd:"" help:"Show the active session's state, idle time, workspaces, and commands."`
	Current sessionCurrentCmd `cmd:"" help:"Print the active session resolved from -s, HOTBED_SESSION, T3CODE_THREAD_ID, or the current git worktree."`
	End     sessionEndCmd     `cmd:"" help:"End a session: stop its commands, release its workspaces, and clear its local mapping."`
}

type sessionStartCmd struct {
	controllerClient
	Name  string `arg:"" optional:"" help:"adjective-noun, such as brave-otter (default: a random free one)."`
	Agent string `env:"HOTBED_AGENT" help:"Who you are, for the cockpit (default: this machine)."`
}

func (c *sessionStartCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := cl.sessions.StartSession(ctx, &pb.StartSessionRequest{Id: c.Name, Agent: c.Agent})
	if err != nil {
		return nil, err
	}
	bindSessionForContext(ctx, d, sess.Id)
	return sess, nil
}

type sessionStatusCmd struct {
	controllerClient
	sessionFlag
}

type sessionStatusResult struct {
	Active             bool                     `json:"active"`
	State              string                   `json:"state"` // "active", "ended", "not_found", "none"
	ID                 string                   `json:"id,omitempty"`
	Source             string                   `json:"source,omitempty"`
	Key                string                   `json:"key,omitempty"`
	Agent              string                   `json:"agent,omitempty"`
	StartedAt          int64                    `json:"startedAt,omitempty"`
	LastActiveAt       int64                    `json:"lastActiveAt,omitempty"`
	EndedAt            *int64                   `json:"endedAt,omitempty"`
	EndReason          string                   `json:"endReason,omitempty"`
	IdleSeconds        *int64                   `json:"idleSeconds,omitempty"`
	IdleTimeoutSeconds int64                    `json:"idleTimeoutSeconds,omitempty"`
	ExpiresInSeconds   *int64                   `json:"expiresInSeconds,omitempty"`
	Schedule           string                   `json:"schedule,omitempty"`
	ScheduleRunning    bool                     `json:"scheduleRunning,omitempty"`
	LiveCommands       int                      `json:"liveCommands"`
	Workspaces         []sessionWorkspaceStatus `json:"workspaces"`
	Commands           []sessionCommandStatus   `json:"commands"`
}

type sessionWorkspaceStatus struct {
	ID           string `json:"id"`
	Repo         string `json:"repo"`
	Node         string `json:"node,omitempty"`
	NodeID       string `json:"nodeId"`
	State        string `json:"state"`
	BaseID       string `json:"baseId,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Head         string `json:"head,omitempty"`
	SyncDir      string `json:"syncDir,omitempty"`
	SyncHost     string `json:"syncHost,omitempty"`
	FilesChanged int32  `json:"filesChanged,omitempty"`
	Reason       string `json:"reason,omitempty"`
	CreatedAt    int64  `json:"createdAt,omitempty"`
	ReleasedAt   *int64 `json:"releasedAt,omitempty"`
}

type sessionCommandStatus struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	Node        string `json:"node,omitempty"`
	Argv        string `json:"argv"`
	Kind        string `json:"kind,omitempty"`
	State       string `json:"state"`
	ExitCode    *int32 `json:"exitCode,omitempty"`
	URL         string `json:"url,omitempty"`
	Reason      string `json:"reason,omitempty"`
	QueuedAt    int64  `json:"queuedAt,omitempty"`
	StartedAt   *int64 `json:"startedAt,omitempty"`
	EndedAt     *int64 `json:"endedAt,omitempty"`
}

func (c *sessionStatusCmd) Exec(ctx context.Context, d deps) (any, error) {
	binding, err := c.sessionFlag.resolveBinding(ctx, d)
	if err != nil {
		return sessionStatusResult{
			Active:     false,
			State:      "none",
			Workspaces: []sessionWorkspaceStatus{},
			Commands:   []sessionCommandStatus{},
		}, nil
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	res, err := cl.sessions.GetSession(ctx, &pb.GetSessionRequest{Id: binding.ID})
	if err != nil {
		if api.FromConnect(err).Code == api.CodeNotFound && (binding.Source == "thread" || binding.Source == "worktree") {
			return sessionStatusResult{
				Active:     false,
				State:      "not_found",
				ID:         binding.ID,
				Source:     binding.Source,
				Key:        binding.Key,
				Workspaces: []sessionWorkspaceStatus{},
				Commands:   []sessionCommandStatus{},
			}, nil
		}
		return nil, err
	}
	sess := res.Session
	out := sessionStatusResult{
		Active:             sess.EndedAt == nil,
		State:              "active",
		ID:                 sess.Id,
		Source:             binding.Source,
		Key:                binding.Key,
		Agent:              sess.Agent,
		StartedAt:          sess.StartedAt,
		LastActiveAt:       sess.LastActiveAt,
		EndedAt:            sess.EndedAt,
		EndReason:          sess.EndReason,
		IdleTimeoutSeconds: res.IdleTimeoutSeconds,
		Schedule:           sess.Schedule,
		ScheduleRunning:    sess.ScheduleRunning,
		Workspaces:         make([]sessionWorkspaceStatus, 0, len(sess.Workspaces)),
		Commands:           make([]sessionCommandStatus, 0, len(res.Commands)),
	}
	if sess.EndedAt != nil {
		out.State = "ended"
	} else {
		idleSec := max(int64(0), (res.Now-sess.LastActiveAt)/1000)
		out.IdleSeconds = &idleSec
		if !sess.ScheduleRunning && res.IdleTimeoutSeconds > 0 {
			exp := max(int64(0), res.IdleTimeoutSeconds-idleSec)
			out.ExpiresInSeconds = &exp
		}
	}
	liveWS := make(map[string]*pb.Workspace, len(res.Workspaces))
	for _, ws := range res.Workspaces {
		liveWS[ws.Id] = ws
	}
	seen := make(map[string]bool, len(sess.Workspaces))
	for _, sw := range sess.Workspaces {
		seen[sw.Id] = true
		if ws, ok := liveWS[sw.Id]; ok {
			out.Workspaces = append(out.Workspaces, sessionWorkspaceStatus{
				ID:           ws.Id,
				Repo:         ws.RepoId,
				Node:         res.NodeHosts[ws.NodeId],
				NodeID:       ws.NodeId,
				State:        workspaceStateName(ws.State),
				BaseID:       ws.BaseId,
				Branch:       ws.Branch,
				Head:         ws.Head,
				SyncDir:      ws.SyncDir,
				SyncHost:     ws.SyncHost,
				FilesChanged: ws.FilesChanged,
				Reason:       ws.Reason,
				CreatedAt:    ws.CreatedAt,
			})
			continue
		}
		state := "released"
		if sw.ReleasedAt == nil {
			state = "unknown"
		}
		out.Workspaces = append(out.Workspaces, sessionWorkspaceStatus{
			ID:         sw.Id,
			Repo:       sw.RepoId,
			Node:       res.NodeHosts[sw.NodeId],
			NodeID:     sw.NodeId,
			State:      state,
			CreatedAt:  sw.CreatedAt,
			ReleasedAt: sw.ReleasedAt,
		})
	}
	for _, ws := range res.Workspaces {
		if seen[ws.Id] {
			continue
		}
		out.Workspaces = append(out.Workspaces, sessionWorkspaceStatus{
			ID:           ws.Id,
			Repo:         ws.RepoId,
			Node:         res.NodeHosts[ws.NodeId],
			NodeID:       ws.NodeId,
			State:        workspaceStateName(ws.State),
			BaseID:       ws.BaseId,
			Branch:       ws.Branch,
			Head:         ws.Head,
			SyncDir:      ws.SyncDir,
			SyncHost:     ws.SyncHost,
			FilesChanged: ws.FilesChanged,
			Reason:       ws.Reason,
			CreatedAt:    ws.CreatedAt,
		})
	}
	for _, cmd := range res.Commands {
		if live(cmd.State) {
			out.LiveCommands++
		}
		out.Commands = append(out.Commands, sessionCommandStatus{
			ID:          cmd.Id,
			WorkspaceID: cmd.WorkspaceId,
			Node:        res.NodeHosts[cmd.NodeId],
			Argv:        cmd.Argv,
			Kind:        cmd.Kind,
			State:       commandStateName(cmd.State),
			ExitCode:    cmd.ExitCode,
			URL:         cmd.Url,
			Reason:      cmd.Reason,
			QueuedAt:    cmd.QueuedAt,
			StartedAt:   cmd.StartedAt,
			EndedAt:     cmd.EndedAt,
		})
	}
	return out, nil
}

func workspaceStateName(s pb.WorkspaceState) string {
	switch s {
	case pb.WorkspaceState_WORKSPACE_STATE_ACTIVE:
		return "active"
	case pb.WorkspaceState_WORKSPACE_STATE_IDLE:
		return "idle"
	case pb.WorkspaceState_WORKSPACE_STATE_RELEASED:
		return "released"
	case pb.WorkspaceState_WORKSPACE_STATE_PREPARING:
		return "preparing"
	case pb.WorkspaceState_WORKSPACE_STATE_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}

func commandStateName(s pb.CommandState) string {
	switch s {
	case pb.CommandState_COMMAND_STATE_QUEUED:
		return "queued"
	case pb.CommandState_COMMAND_STATE_RUNNING:
		return "running"
	case pb.CommandState_COMMAND_STATE_PARKED:
		return "parked"
	case pb.CommandState_COMMAND_STATE_SUCCEEDED:
		return "succeeded"
	case pb.CommandState_COMMAND_STATE_FAILED:
		return "failed"
	case pb.CommandState_COMMAND_STATE_CANCELLED:
		return "cancelled"
	default:
		return "unknown"
	}
}

type sessionCurrentCmd struct {
	sessionFlag
}

func (c *sessionCurrentCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.sessionFlag.resolveBinding(ctx, d)
}

type sessionEndCmd struct {
	controllerClient
	sessionFlag
}

func (c *sessionEndCmd) Exec(ctx context.Context, d deps) (any, error) {
	sessID, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	res, err := cl.sessions.EndSession(ctx, &pb.EndSessionRequest{Id: sessID})
	if err != nil {
		return nil, err
	}
	unbindSession(d, sessID)
	return res, nil
}

type addCmd struct {
	controllerClient
	sessionFlag
	repoFlag
	Sync   string `type:"path" help:"A local checkout of the repo the workspace follows: run from this machine syncs it first. Without it the workspace is remote-only."`
	Node   string `help:"Put the workspace on this node (host name)."`
	Branch string `help:"The branch you work on, for the cockpit."`
	NoWait bool   `help:"Return while the workspace is still preparing (its base being pulled and freshened); commands run in it queue until it is ready."`
}

func (c *addCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	repo, err := c.resolveRepo(ctx, d, session, c.Repo, c.Sync)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	ws, err := cl.workspaces.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		SessionId: session, Repo: repo, SyncDir: c.Sync, Node: c.Node, Branch: c.Branch,
	})
	if err != nil {
		return nil, err
	}
	remember(d, session, ws)
	if c.NoWait {
		return ws, nil
	}
	w, err := c.reach(d, ws.NodeAddr)
	if err != nil {
		return nil, err
	}
	if ws, err = waitReady(ctx, w, ws, d.stderr); err != nil {
		return nil, err
	}
	if c.Sync != "" {
		if _, err := syncTo(ctx, w, ws, c.Sync); err != nil {
			return nil, err
		}
		addr := ws.NodeAddr
		if ws, err = w.calls.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: ws.Id}); err != nil {
			return nil, err
		}
		ws.NodeAddr = addr
	}
	return ws, nil
}

// waitReady waits for a preparing workspace (its base being pulled and
// freshened) to be ready, watching it on its worker and saying what it waits
// for on stderr.
func waitReady(ctx context.Context, w workerClients, ws *pb.Workspace, stderr io.Writer) (*pb.Workspace, error) {
	addr := ws.NodeAddr
	defer func() { ws.NodeAddr = addr }()
	said := ""
	var lost time.Time // when the watch first dropped, of the drops since it last worked
	for ws.State == pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
		in, err := w.stream.WatchWorkspace(ctx, &pb.WatchWorkspaceRequest{Id: ws.Id})
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
			WithSuggestion("fix the failing step (hotbed step ls -r REPO, then hotbed step add), and add the repo again")
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

type syncCmd struct {
	controllerClient
	sessionFlag
	repoFlag
	Dir string `type:"path" help:"The checkout to send (default: the one the workspace follows)."`
}

func (c *syncCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	repo, err := c.resolveRepo(ctx, d, session, c.Repo, c.Dir)
	if err != nil {
		return nil, err
	}
	ws, w, err := c.locate(ctx, d, session, repo)
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
	return syncTo(ctx, w, ws, dir)
}

const syncChunk = 256 << 10

// syncTo sends the checkout's state to a workspace, unless the workspace
// already has exactly this tree.
func syncTo(ctx context.Context, w workerClients, ws *pb.Workspace, dir string) (*pb.SyncResponse, error) {
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
	stream, err := w.stream.Sync(ctx)
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
	controllerClient
	sessionFlag
	repoFlag
	Args   []string `arg:"" optional:"" passthrough:"" help:"The command, after --."`
	NoSync bool     `help:"Do not sync the checkout the workspace follows first."`
	Detach bool     `help:"Return once the command is queued; it runs until it ends or hotbed stop. For dev servers and long jobs; follow with hotbed wait."`
	Port   int      `help:"Port a dev server listens on, for its URL."`
	commandFlags
}

func (c *runCmd) SetDashArgs(args []string) { c.Args = args }

// Raw syncs if the workspace follows a checkout on this machine, runs the
// command, and streams its output to stdout. Interrupting stops it.
func (c *runCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	req, err := c.request(c.Args, "hotbed run -- cargo test")
	if err != nil {
		return 0, false, err
	}
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return 0, false, err
	}
	repo, err := c.resolveRepo(ctx, d, session, c.Repo, "")
	if err != nil {
		return 0, false, err
	}
	ws, w, err := c.locate(ctx, d, session, repo)
	if err != nil {
		return 0, false, err
	}
	if !c.NoSync && ws.SyncDir != "" {
		self, err := d.tailnet.Self(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(d.stderr, "hotbed: not syncing: %v\n", err)
		case self.Hostname == ws.SyncHost:
			if ws, err = waitReady(ctx, w, ws, d.stderr); err != nil {
				return 0, false, err
			}
			res, err := syncTo(ctx, w, ws, ws.SyncDir)
			if err != nil {
				return 0, false, err
			}
			if !res.Skipped {
				fmt.Fprintf(d.stderr, "hotbed: synced %d changed files (%s) from %s\n", res.FilesChanged, size(res.BundleBytes), ws.SyncDir)
			}
		}
	}
	req.WorkspaceId, req.Attached, req.Port = ws.Id, !c.Detach, int32(c.Port)
	cmd, err := w.calls.RunCommand(ctx, req)
	if err != nil {
		return 0, false, err
	}
	if c.Detach {
		writeEnvelope(d.stdout, success("run", cmd))
		return exitOK, true, nil
	}
	fmt.Fprintf(d.stderr, "hotbed: %s running in %s (hotbed stop -s %s %s)\n", cmd.Id, ws.Id, session, cmd.Id)
	code, err := attach(ctx, w, cmd.Id, d.stdout, d.stderr)
	return code, true, err
}

// attach prints an attached command's output to w until it ends and returns
// its exit status. Interrupted, it stops the command, which ends with its
// client, and returns 130 as a shell would.
func attach(ctx context.Context, wk workerClients, id string, w, stderr io.Writer) (int, error) {
	code, err := follow(ctx, wk, id, w)
	if ctx.Err() != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := wk.calls.StopCommand(stopCtx, &pb.StopCommandRequest{Id: id}); err != nil {
			fmt.Fprintf(stderr, "hotbed: could not stop %s: %v\n", id, api.FromConnect(err))
		}
		return 130, nil
	}
	return code, err
}

func (c *runCmd) Exec(context.Context, deps) (any, error) { return nil, errors.New("run is raw") }

// rideOut is how long follow waits for a worker that stopped answering:
// long enough for it to fetch and start a new build.
const rideOut = 10 * time.Minute

// follow prints a command's output until it ends (followLog) and returns
// its exit status, a shell's 128 plus the signal for one a signal killed.
func follow(ctx context.Context, wk workerClients, id string, w io.Writer) (int, error) {
	c, err := followLog(ctx, wk, id, w)
	if err != nil {
		return 0, err
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

// followLog copies a command's output to w until the command ends, picking
// up where it left off if the stream drops, and returns it as it ended.
func followLog(ctx context.Context, wk workerClients, id string, w io.Writer) (*pb.Command, error) {
	var offset int64
	// A worker that stops answering is waited for (riding out its restart,
	// such as a new build's) for rideOut, from its first failure.
	var down time.Time
	wait := func(err error) bool {
		if ctx.Err() != nil || !api.FromConnect(err).Retryable {
			return false
		}
		if down.IsZero() {
			down = time.Now()
		}
		if time.Since(down) > rideOut {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
			return true
		}
	}
	for {
		in, err := wk.stream.FollowLog(ctx, &pb.FollowLogRequest{Id: id, Offset: offset, Follow: true})
		if err != nil {
			if wait(err) {
				continue
			}
			return nil, err
		}
		down = time.Time{}
		for {
			chunk, err := in.Receive()
			if err != nil {
				break
			}
			n, werr := w.Write(chunk.Data)
			offset += int64(n)
			if werr != nil {
				in.Close()
				return nil, werr
			}
		}
		in.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c, err := wk.calls.GetCommand(ctx, &pb.GetCommandRequest{Id: id})
		if err != nil {
			if wait(err) {
				continue
			}
			return nil, err
		}
		if live(c.State) {
			continue
		}
		return c, nil
	}
}

func live(s pb.CommandState) bool {
	return s == pb.CommandState_COMMAND_STATE_QUEUED || s == pb.CommandState_COMMAND_STATE_RUNNING || s == pb.CommandState_COMMAND_STATE_PARKED
}

type waitCmd struct {
	controllerClient
	sessionFlag
	After   int64 `default:"-1" help:"Return events after this cursor (default: after the last ones this machine saw)."`
	Timeout int   `default:"120" help:"Seconds to wait for an event, at most 120."`
}

// Exec watches the session until something happens. It remembers the cursor per session on this
// machine, so repeated hotbed wait calls return each event once.
func (c *waitCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	stream, err := c.clients(d.stream)
	if err != nil {
		return nil, err
	}
	cursorFile := filepath.Join(d.cache, "sessions", session+".cursor")
	after := c.After
	if after < 0 {
		after = 0
		if b, err := os.ReadFile(cursorFile); err == nil {
			after, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		}
	}
	res, err := watchSession(ctx, stream, &pb.WatchSessionRequest{SessionId: session, After: after, TimeoutSeconds: int32(c.Timeout)})
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
	controllerClient
	sessionFlag
}

func (c *psCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.workspaces.ListCommands(ctx, &pb.ListCommandsRequest{SessionId: session})
}

type logsCmd struct {
	controllerClient
	sessionFlag
	Command string `arg:"" help:"The command's ID."`
	Follow  bool   `short:"f" help:"Keep printing until the command ends."`
}

// Raw prints the command's output as is, from its worker. A base's
// freshen, whose log a failed add names, can be read from any session.
func (c *logsCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return 0, false, err
	}
	_, w, err := c.locateCommand(ctx, d, session, c.Command, true)
	if err != nil {
		return 0, false, err
	}
	if c.Follow {
		// Until it ends, across its worker's restarts.
		if _, err := followLog(ctx, w, c.Command, d.stdout); err != nil && ctx.Err() == nil {
			return 0, true, err
		}
		return exitOK, true, nil
	}
	in, err := w.stream.FollowLog(ctx, &pb.FollowLogRequest{Id: c.Command})
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
	controllerClient
	sessionFlag
	Command string `arg:"" help:"The command's ID."`
}

func (c *stopCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	_, w, err := c.locateCommand(ctx, d, session, c.Command, false)
	if err != nil {
		return nil, err
	}
	return w.calls.StopCommand(ctx, &pb.StopCommandRequest{Id: c.Command})
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
