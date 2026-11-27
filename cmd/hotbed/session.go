package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

type sessionCmd struct {
	Start   sessionStartCmd   `cmd:"" help:"Start a session in a project (-p, or the one holding this checkout's repo), associate it with T3CODE_THREAD_ID when set, and bind it to this T3 thread or git worktree in ~/.hotbed/sessions.json. It is abandoned after an hour in which no call names it and no attached run or logs -f streams from it."`
	Status  sessionStatusCmd  `cmd:"" help:"Show the active session's state, idle time, workspaces, and commands (or look up by --t3code-thread-id)."`
	Current sessionCurrentCmd `cmd:"" help:"Print the active session resolved from -s, HOTBED_SESSION, T3CODE_THREAD_ID, or the current git worktree."`
	Ls      sessionLsCmd      `cmd:"" help:"A project's live sessions (default: this session's project, or the one holding this checkout's repo), the most recently active first."`
	Search  sessionSearchCmd  `cmd:"" help:"Search sessions by T3 Code thread ID across every project, live and ended (including sessions that ended more than a day ago), live ones first then most recently active."`
	End     sessionEndCmd     `cmd:"" help:"End a session: stop its commands, release its workspaces, and clear its local mapping."`
}

type sessionStartCmd struct {
	controllerClient
	projectFlag
	Name           string `arg:"" optional:"" help:"adjective-noun, such as brave-otter (default: a random free one)."`
	Agent          string `env:"HOTBED_AGENT" help:"Who you are, for the cockpit (default: this machine)."`
	Model          string `env:"HOTBED_MODEL" help:"Exact model ID running this session (e.g. gemini-4-neon, claude-opus-4-6)."`
	T3CodeThreadID string `name:"t3code-thread-id" env:"T3CODE_THREAD_ID" help:"The T3 Code thread ID to associate with the session on the controller (default: T3CODE_THREAD_ID)."`
	Replace        bool   `help:"End the live session already bound to this T3 thread or git worktree and bind the new one in its place."`
	Resume         bool   `help:"Take up the session NAME if there is one, rather than refuse: live again if it ended (with no workspaces), as it is if not. It keeps its project."`
	OmitMemories   bool   `name:"omit-memories" help:"Do not include the compact memory briefing in the output."`
}

func (c *sessionStartCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	remote, err := d.checkoutRemote(ctx)
	if err != nil {
		return nil, err
	}
	path := sessionsFilePath(d)
	m, err := loadSessionMap(path)
	if err != nil {
		return nil, err
	}
	if c.Resume && c.Name == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "say which session to resume").WithSuggestion("hotbed session start NAME --resume")
	}
	place, binds := d.startPlace(ctx)
	// prevBound is whatever was bound here before StartSession; replaced is the
	// live session bound here that the new one replaces.
	var prevBound, replaced string
	if binds {
		prevBound = m.bound(place)
		if prevBound != "" && !(c.Resume && prevBound == c.Name) {
			live, err := sessionLive(ctx, cl, prevBound)
			if err != nil {
				return nil, err
			}
			if live && !c.Replace {
				return nil, api.Errorf(api.CodeConflict, "%s is bound to session %s, which is live", place, prevBound).
					WithSuggestion(fmt.Sprintf("keep using %s (hotbed session status), end it first (hotbed session end), or pass --replace to end it and start a new one", prevBound))
			}
			if live {
				replaced = prevBound
			}
		}
	}
	tid := strings.TrimSpace(c.T3CodeThreadID)
	sess, err := cl.sessions.StartSession(ctx, &pb.StartSessionRequest{
		Id:              c.Name,
		Agent:           c.Agent,
		Model:           strings.TrimSpace(c.Model),
		Project:         c.Project,
		Remote:          remote,
		Resume:          c.Resume,
		T3CodeThreadId:  tid,
		T3CodeThreadUrl: d.t3codeThreadURL(ctx, tid),
		OmitMemories:    c.OmitMemories,
	})
	if err != nil {
		return nil, err
	}
	if binds {
		for {
			var winner string
			if err := updateSessionMapIf(path, func(m sessionMap) bool {
				if existing := m.bound(place); existing != "" && existing != prevBound && existing != sess.Id {
					winner = existing
					return false
				}
				if replaced != "" {
					m.unbind(replaced)
				}
				m.bind(place, sess.Id)
				return true
			}); err != nil {
				return nil, api.Errorf(api.CodeInternal, "started session %s, but could not bind it to %s: %v", sess.Id, place, err).
					WithSuggestion(fmt.Sprintf("pass -s %s to every command, or set HOTBED_SESSION=%s", sess.Id, sess.Id))
			}
			if winner == "" {
				break
			}
			res, err := cl.sessions.GetSession(ctx, &pb.GetSessionRequest{Id: winner})
			if err != nil {
				if api.CodeOf(err) == api.CodeNotFound {
					// The winning session is already gone; re-claim cleanly.
					prevBound = winner
					continue
				}
				_, _ = cl.sessions.EndSession(ctx, &pb.EndSessionRequest{Id: sess.Id})
				return nil, err
			}
			if res.Session.EndedAt != nil {
				// The winning session already ended; re-claim cleanly.
				prevBound = winner
				continue
			}
			// Another concurrent session start already won the race and bound a
			// live session: keep it bound, best-effort end our losing session,
			// and return the winning session.
			_, _ = cl.sessions.EndSession(ctx, &pb.EndSessionRequest{Id: sess.Id})
			if res.Session.Memories == nil && res.Session.Project == sess.Project {
				res.Session.Memories = sess.Memories
			}
			return res.Session, nil
		}
	}
	if replaced != "" {
		if _, err := cl.sessions.EndSession(ctx, &pb.EndSessionRequest{Id: replaced}); err != nil && api.CodeOf(err) != api.CodeNotFound {
			return nil, api.Errorf(api.CodeInternal, "started session %s in place of %s, but could not end %s: %v", sess.Id, replaced, replaced, toAPIError(err)).
				WithSuggestion(fmt.Sprintf("hotbed session end -s %s", replaced))
		}
	}
	return sess, nil
}

// sessionLive says whether the controller has session id and it has not
// ended.
func sessionLive(ctx context.Context, cl clients, id string) (bool, error) {
	res, err := cl.sessions.GetSession(ctx, &pb.GetSessionRequest{Id: id})
	if err != nil {
		if api.CodeOf(err) == api.CodeNotFound {
			return false, nil
		}
		return false, err
	}
	return res.Session.EndedAt == nil, nil
}

type sessionStatusCmd struct {
	controllerClient
	sessionFlag
	T3CodeThreadID string `name:"t3code-thread-id" help:"Look up the most relevant session for this T3 Code thread ID on the controller (live first, then most recently active)."`
}

type sessionStatusResult struct {
	Active             bool                     `json:"active"`
	State              string                   `json:"state"` // "active", "ended", "not_found", "none"
	ID                 string                   `json:"id,omitempty"`
	Source             string                   `json:"source,omitempty"`
	Key                string                   `json:"key,omitempty"`
	Agent              string                   `json:"agent,omitempty"`
	T3CodeThreadID     string                   `json:"t3codeThreadId,omitempty"`
	T3CodeThreadURL    string                   `json:"t3codeThreadUrl,omitempty"`
	StartedAt          int64                    `json:"startedAt,omitempty"`
	LastActiveAt       int64                    `json:"lastActiveAt,omitempty"`
	EndedAt            *int64                   `json:"endedAt,omitempty"`
	EndReason          string                   `json:"endReason,omitempty"`
	IdleSeconds        *int64                   `json:"idleSeconds,omitempty"`
	IdleTimeoutSeconds int64                    `json:"idleTimeoutSeconds,omitempty"`
	ExpiresInSeconds   *int64                   `json:"expiresInSeconds,omitempty"`
	Schedule           string                   `json:"schedule,omitempty"`
	ScheduleRunning    bool                     `json:"scheduleRunning,omitempty"`
	CockpitURL         string                   `json:"cockpitUrl,omitempty"`
	LiveCommands       int                      `json:"liveCommands"`
	Workspaces         []sessionWorkspaceStatus `json:"workspaces"`
	Commands           []sessionCommandStatus   `json:"commands"`
}

type sessionWorkspaceStatus struct {
	ID         string              `json:"id"`
	Project    string              `json:"project,omitempty"`
	Node       string              `json:"node,omitempty"`
	NodeID     string              `json:"nodeId"`
	State      string              `json:"state"`
	BaseID     string              `json:"baseId,omitempty"`
	Repos      []*pb.WorkspaceRepo `json:"repos,omitempty"`
	SyncHost   string              `json:"syncHost,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	CreatedAt  int64               `json:"createdAt,omitempty"`
	ReleasedAt *int64              `json:"releasedAt,omitempty"`
	CockpitURL string              `json:"cockpitUrl,omitempty"`
	Throwaway  bool                `json:"throwaway,omitempty"`
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
	CockpitURL  string `json:"cockpitUrl,omitempty"`
}

func (c *sessionStatusCmd) Exec(ctx context.Context, d deps) (any, error) {
	var (
		binding sessionBinding
		req     *pb.GetSessionRequest
	)
	if tid := strings.TrimSpace(c.T3CodeThreadID); tid != "" {
		if s := strings.TrimSpace(c.Session); s != "" && s != d.env("HOTBED_SESSION") {
			return nil, api.Errorf(api.CodeInvalidArgs, "--session and --t3code-thread-id can't be used together")
		}
		binding = sessionBinding{Source: "t3code-thread", Key: tid}
		req = &pb.GetSessionRequest{T3CodeThreadId: tid}
	} else {
		var found bool
		var err error
		binding, found, err = c.sessionFlag.lookupBinding(ctx, d)
		if err != nil {
			return nil, err
		}
		if !found {
			return sessionStatusResult{
				Active:     false,
				State:      "none",
				Workspaces: []sessionWorkspaceStatus{},
				Commands:   []sessionCommandStatus{},
			}, nil
		}
		req = &pb.GetSessionRequest{Id: binding.ID}
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	res, err := cl.sessions.GetSession(ctx, req)
	if err != nil {
		if api.CodeOf(err) == api.CodeNotFound && (binding.Source == "thread" || binding.Source == "worktree") {
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
		T3CodeThreadID:     sess.T3CodeThreadId,
		T3CodeThreadURL:    sess.T3CodeThreadUrl,
		StartedAt:          sess.StartedAt,
		LastActiveAt:       sess.LastActiveAt,
		EndedAt:            sess.EndedAt,
		EndReason:          sess.EndReason,
		IdleTimeoutSeconds: res.IdleTimeoutSeconds,
		Schedule:           sess.Schedule,
		ScheduleRunning:    sess.ScheduleRunning,
		CockpitURL:         d.cockpit.session(sess.Id),
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
			out.Workspaces = append(out.Workspaces, liveWorkspaceStatus(ws, res.NodeHosts, d.cockpit))
			continue
		}
		state := "released"
		if sw.ReleasedAt == nil {
			state = "unknown"
		}
		out.Workspaces = append(out.Workspaces, sessionWorkspaceStatus{
			ID:         sw.Id,
			Project:    sess.Project,
			Node:       res.NodeHosts[sw.NodeId],
			NodeID:     sw.NodeId,
			State:      state,
			CreatedAt:  sw.CreatedAt,
			ReleasedAt: sw.ReleasedAt,
			CockpitURL: d.cockpit.workspace(sw.Id),
		})
	}
	for _, ws := range res.Workspaces {
		if seen[ws.Id] {
			continue
		}
		out.Workspaces = append(out.Workspaces, liveWorkspaceStatus(ws, res.NodeHosts, d.cockpit))
	}
	for _, cmd := range res.Commands {
		if api.Live(cmd.State) {
			out.LiveCommands++
		}
		out.Commands = append(out.Commands, sessionCommandStatus{
			ID:          cmd.Id,
			WorkspaceID: cmd.WorkspaceId,
			Node:        res.NodeHosts[cmd.NodeId],
			Argv:        cmd.Argv,
			Kind:        cmd.Kind,
			State:       enumWord(cmd.State),
			ExitCode:    cmd.ExitCode,
			URL:         cmd.Url,
			Reason:      cmd.Reason,
			QueuedAt:    cmd.QueuedAt,
			StartedAt:   cmd.StartedAt,
			EndedAt:     cmd.EndedAt,
			CockpitURL:  d.cockpit.command(cmd.Id),
		})
	}
	return out, nil
}

// liveWorkspaceStatus is a workspace its node still has, as session status
// shows it.
func liveWorkspaceStatus(ws *pb.Workspace, nodeHosts map[string]string, c cockpit) sessionWorkspaceStatus {
	return sessionWorkspaceStatus{
		ID:         ws.Id,
		Project:    ws.Project,
		Node:       nodeHosts[ws.NodeId],
		NodeID:     ws.NodeId,
		State:      enumWord(ws.State),
		BaseID:     ws.BaseId,
		Repos:      ws.Repos,
		SyncHost:   ws.SyncHost,
		Reason:     ws.Reason,
		CreatedAt:  ws.CreatedAt,
		CockpitURL: c.workspace(ws.Id),
		Throwaway:  ws.Throwaway,
	}
}

type sessionCurrentCmd struct {
	sessionFlag
}

func (c *sessionCurrentCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.sessionFlag.resolveBinding(ctx, d)
}

type sessionLsCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	AllProjects    bool   `help:"Every project's sessions."`
	Ended          bool   `help:"Those that ended in the last day too."`
	T3CodeThreadID string `name:"t3code-thread-id" help:"Only sessions associated with this T3 Code thread ID."`
}

func (c *sessionLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	req := &pb.ListSessionsRequest{
		SessionId:      sc.session,
		Remote:         sc.remote,
		Ended:          c.Ended,
		T3CodeThreadId: strings.TrimSpace(c.T3CodeThreadID),
	}
	switch {
	case sc.all:
		req.Scope = &pb.ListSessionsRequest_AllProjects{AllProjects: true}
	case sc.project != "":
		req.Scope = &pb.ListSessionsRequest_Project{Project: sc.project}
	}
	return cl.sessions.ListSessions(ctx, req)
}

type sessionSearchCmd struct {
	controllerClient
	ThreadID       string `arg:"" optional:"" name:"thread-id" help:"The T3 Code thread ID to search for (default: --t3code-thread-id or T3CODE_THREAD_ID)."`
	T3CodeThreadID string `name:"t3code-thread-id" help:"The T3 Code thread ID to search for (default: T3CODE_THREAD_ID)."`
	Project        string `short:"p" help:"Only sessions in this project (default: every project)."`
	Live           bool   `help:"Only live (not yet ended) sessions."`
	Limit          *int32 `short:"n" help:"At most this many matching sessions (default 50; 0 for every one)."`
}

func (c *sessionSearchCmd) Exec(ctx context.Context, d deps) (any, error) {
	tid := strings.TrimSpace(c.ThreadID)
	flagTID := strings.TrimSpace(c.T3CodeThreadID)
	if tid != "" && flagTID != "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "give the T3 Code thread ID either as an argument or with --t3code-thread-id, not both")
	}
	if tid == "" {
		tid = flagTID
	}
	if tid == "" {
		tid = d.t3codeThreadID()
	}
	if tid == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "no T3 Code thread ID specified and T3CODE_THREAD_ID is not set").
			WithSuggestion("pass a T3 Code thread ID: hotbed session search THREAD_ID")
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.sessions.SearchSessions(ctx, &pb.SearchSessionsRequest{
		T3CodeThreadId: tid,
		Project:        strings.TrimSpace(c.Project),
		LiveOnly:       c.Live,
		Limit:          c.Limit,
	})
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
	if err := unbindSession(d, sessID); err != nil {
		return nil, api.Errorf(api.CodeInternal, "ended session %s, but could not clear its binding in %s: %v", sessID, tildePath(sessionsFilePath(d)), err)
	}
	return res, nil
}

// waitReady waits for a preparing workspace (its base being pulled and
// freshened) to be ready, watching it on its worker and saying what it waits
// for on stderr.
func waitReady(ctx context.Context, w workerClients, ws *pb.Workspace, stderr io.Writer) (*pb.Workspace, error) {
	// The worker's copy lacks what the controller filled in.
	node, project := ws.Node, ws.Project
	defer func() {
		if ws == nil {
			return
		}
		ws.Node = node
		if ws.Project == "" {
			ws.Project = project
		}
	}()
	said := ""
	var lost time.Time // when the watch first dropped, of the drops since it last worked
	for ws.State == pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
		in, err := w.stream.WatchWorkspace(ctx, &pb.WatchWorkspaceRequest{Id: ws.Id})
		if err == nil {
			for ws.State == pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
				if ws.Reason != said {
					where := ""
					if node != "" {
						where = " on " + node
					}
					fmt.Fprintf(stderr, "hotbed: %s%s is preparing: %s\n", ws.Id, where, ws.Reason)
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
			WithSuggestion("fix the failing step (hotbed step ls, then hotbed step add), then run again")
	}
	return ws, nil
}

// watchSession returns the session's first update with events (or its end),
// or how it stands when the watch times out.
func watchSession(ctx context.Context, stream clients, req *pb.WatchSessionRequest) (*pb.SessionEvents, error) {
	in, err := stream.sessions.WatchSession(ctx, req)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	var last *pb.SessionEvents
	for {
		res, err := in.Receive()
		if err != nil && last != nil && (errors.Is(err, io.EOF) || api.FromConnect(err).Retryable) {
			// Its two minutes are up, or the controller went away under it
			// (a restart): nothing happened yet; the next wait goes on.
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
	Dir string `type:"path" help:"The checkout to send (default: the one this is run in)."`
}

type syncResult struct {
	Synced []*pb.SyncResponse `json:"synced"`
}

// Exec sends the checkout to every workspace the session has but throwaway
// ones, into its checkout of the repo: what is synced is what each of them
// holds.
func (c *syncCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	repo, err := c.resolveRepo(ctx, d, session, c.Repo, c.Dir)
	if err != nil {
		return nil, err
	}
	dir := c.Dir
	if dir == "" {
		if dir, err = c.checkoutOf(ctx, d, session, c.Repo, repo); err != nil {
			return nil, err
		}
	}
	if dir == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "this is no checkout of %s", repo).WithSuggestion("run it in a checkout of the repo, or pass --dir CHECKOUT")
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	list, err := cl.workspaces.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{SessionId: session})
	if err != nil {
		return nil, err
	}
	out := syncResult{Synced: []*pb.SyncResponse{}}
	for _, ws := range ownWorkspaces(list.Workspaces) {
		w, err := c.reach(d, ws.NodeId)
		if err != nil {
			return nil, err
		}
		if ws, err = waitReady(ctx, w, ws, d.stderr); err != nil {
			return nil, err
		}
		res, err := syncTo(ctx, w, ws, repo, dir)
		if err != nil {
			return nil, err
		}
		out.Synced = append(out.Synced, res)
	}
	if len(out.Synced) == 0 {
		return nil, api.Errorf(api.CodeNotFound, "session %s has no workspace", session).
			WithSuggestion("hotbed run syncs the checkout it is run in to the workspace it runs in")
	}
	return out, nil
}

const syncChunk = 256 << 10

// syncTo sends the state of dir, a checkout of repo, to the workspace's
// checkout of it, unless that already has exactly this tree.
func syncTo(ctx context.Context, w workerClients, ws *pb.Workspace, repo, dir string) (*pb.SyncResponse, error) {
	i := slices.IndexFunc(ws.Repos, func(r *pb.WorkspaceRepo) bool { return r.Repo == repo })
	if i < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "workspace %s of project %s has no checkout of %s", ws.Id, ws.Project, repo).
			WithSuggestion("hotbed project ls shows each project's repos")
	}
	r := ws.Repos[i]
	snap, err := gitops.Snapshot(ctx, dir)
	if err != nil {
		return nil, err
	}
	if snap.Tree == r.Tree {
		return &pb.SyncResponse{WorkspaceId: ws.Id, Head: r.Head, Tree: r.Tree, FilesChanged: r.FilesChanged, Skipped: true}, nil
	}
	var bundle bytes.Buffer
	subs, err := gitops.Bundle(ctx, snap, r.BaseCommit, &bundle)
	if err != nil {
		return nil, err
	}
	header := &pb.SyncHeader{WorkspaceId: ws.Id, Commit: snap.Commit, Tree: snap.Tree, Repo: repo, Dir: dir}
	// The submodules' bundles go first, each its size long, then the repo's.
	var payload [][]byte
	for _, s := range subs {
		header.Submodules = append(header.Submodules, &pb.SyncSubmodule{Path: s.Path, Commit: s.Commit, BundleBytes: int64(len(s.Data))})
		payload = append(payload, s.Data)
	}
	payload = append(payload, bundle.Bytes())
	stream, err := w.stream.Sync(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&pb.SyncRequest{Msg: &pb.SyncRequest_Header{Header: header}}); err != nil {
		_, cerr := stream.CloseAndReceive()
		return nil, errors.Join(err, cerr)
	}
send:
	for _, b := range payload {
		for len(b) > 0 {
			n := min(len(b), syncChunk)
			if err := stream.Send(&pb.SyncRequest{Msg: &pb.SyncRequest_Chunk{Chunk: b[:n]}}); err != nil {
				break send // the server failed; CloseAndReceive says why
			}
			b = b[n:]
		}
	}
	return stream.CloseAndReceive()
}

// commandFlags are what a command is run with, by hotbed run and by a
// scheduled command alike.
type commandFlags struct {
	CPUWeight int      `default:"100" help:"Share of contended CPU (cgroup cpu.weight, 1-10000)."`
	Priority  string   `default:"normal" help:"Scheduling priority: low (-10), normal (0), high (10), urgent (20), or an integer from -100 to 100. Higher priority is admitted first; nothing running is preempted for it."`
	MemLow    string   `default:"0" help:"Memory guaranteed to the command (memory.low), such as 2G; it waits until that fits."`
	MemHigh   string   `default:"0" help:"Memory past which it is throttled and swapped (memory.high), such as 8G; 0 for none."`
	Env       []string `help:"KEY=VALUE for the command. Repeatable."`
	Kind      string   `enum:",build,test,check,server,shell" default:"" help:"What it is, for history and build metrics: build, test, check, server or shell (default: server when --port is set, otherwise shell; pass build, test, check or server explicitly when classifying runs)."`
	Bench     bool     `help:"Run it as a measurement: alone among measurements on the node, once running work leaves it room, on the node's measurement CPUs, which no other command uses meanwhile, isolated for its whole run (cache, memory bandwidth, IRQs, others' clocks and disk IO where the node has them), never swapped or parked. It fails if the isolation was lost."`
	SoftLimit string   `help:"Pause the command (freeze it, its memory to swap) once it has run this long without ending, such as 10m: counted from its start and again each time you wait on it (attached run, hotbed logs -f), which resumes it. 0 for none (default: 10m attached, none detached, scheduled or --bench)."`
	HardLimit string   `help:"Stop the command this long after it started, paused or not, such as 2h; 0 for none, unless it has a soft limit (default: 2h attached or with a soft limit, else none)."`
	KeepIdle  bool     `help:"Keep the command running while it reserves over a quarter of the node's memory for work (its --mem, or the most it has held) and keeps less than a core busy for each 6 GiB of it, which otherwise stops it after 5 minutes: for one that rightly holds much memory for little CPU."`
	VMCPUs    int      `name:"vm-cpus" help:"vCPUs its VM's guest sees (default: the node's). It sizes the VM only: admission goes by what commands are measured to use. vm runtime only."`
	VMMem     string   `name:"vm-mem" default:"0" help:"Memory its VM's guest sees, such as 16G (default: the node's). It sizes the VM only, as --vm-cpus does. vm runtime only."`
}

// The time limits of an attached hotbed run, unless it says otherwise: the
// agent waiting on it gets control back after the soft limit, with the
// command paused, and a command no one comes back for is stopped after the
// hard limit.
const (
	defaultSoftLimit = 10 * time.Minute
	defaultHardLimit = 2 * time.Hour
)

// placementFlags narrow where a run may go; the controller picks among
// the nodes left.
type placementFlags struct {
	Arch    string `enum:",amd64,arm64" default:"" help:"Run only on nodes of this architecture: amd64 or arm64."`
	Runtime string `enum:",vm,cladefs" default:"" help:"Execution runtime: vm (Firecracker + cladefs) or cladefs (host FUSE + cladefs)."`
}

// placement is where a run of req may go, and what it needs there.
func (p placementFlags) placement(req *pb.RunCommandRequest) *pb.Placement {
	runtime := p.Runtime
	if runtime == "" && (req.VmCpus != 0 || req.VmMemoryBytes != 0) {
		runtime = "vm" // only a VM is sized
	}
	return &pb.Placement{
		Arch: p.Arch, Bench: req.Bench, MemoryLowBytes: req.GetLimits().GetMemoryLowBytes(), Runtime: runtime,
		VmCpus: req.VmCpus, VmMemoryBytes: req.VmMemoryBytes,
	}
}

// request is the command args (kong's passthrough, with or without its
// leading --) run with these flags, in no workspace yet. An attached one
// has the default time limits.
func (f commandFlags) request(args []string, example string, attached bool) (*pb.RunCommandRequest, error) {
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
	vmMem, err := parseSize(f.VMMem)
	if err != nil {
		return nil, err
	}
	if f.VMCPUs < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "--vm-cpus %d: a VM needs at least 1", f.VMCPUs)
	}
	priority, err := parsePriority(f.Priority)
	if err != nil {
		return nil, err
	}
	var softDefault, hardDefault time.Duration
	if attached {
		hardDefault = defaultHardLimit
		if !f.Bench {
			softDefault = defaultSoftLimit
		}
	}
	soft, err := parseLimit("--soft-limit", f.SoftLimit, softDefault)
	if err != nil {
		return nil, err
	}
	if soft > 0 && f.Bench {
		return nil, api.Errorf(api.CodeInvalidArgs, "a measurement has no soft limit: pausing it would spoil its numbers").
			WithSuggestion("drop --soft-limit, and bound it with --hard-limit")
	}
	if soft > 0 {
		hardDefault = defaultHardLimit
	}
	hard, err := parseLimit("--hard-limit", f.HardLimit, hardDefault)
	if err != nil {
		return nil, err
	}
	if soft > 0 && hard == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "a command with a soft limit needs a hard limit: paused, and no one coming back for it, it would hold its swap forever").
			WithSuggestion("give --hard-limit a duration, or drop it for the default 2h")
	}
	return &pb.RunCommandRequest{
		Args: args, Env: f.Env, Kind: f.Kind, Bench: f.Bench,
		VmCpus: int32(f.VMCPUs), VmMemoryBytes: vmMem,
		Limits: &pb.CommandLimits{
			CpuWeight: int32(f.CPUWeight), MemoryLowBytes: low, MemoryHighBytes: high,
			SoftLimitSeconds: int64(soft / time.Second), HardLimitSeconds: int64(hard / time.Second),
			Priority: priority, KeepIdle: f.KeepIdle,
		},
	}, nil
}

// parsePriority parses a scheduling priority: low (-10), normal (0), high (10),
// urgent (20), or an integer in [-100, 100]. Empty defaults to 0 (normal).
func parsePriority(s string) (int32, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", "normal":
		return 0, nil
	case "low":
		return -10, nil
	case "high":
		return 10, nil
	case "urgent":
		return 20, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < -100 || n > 100 {
		return 0, api.Errorf(api.CodeInvalidArgs, "invalid --priority %q", s).
			WithSuggestion("pass low (-10), normal (0), high (10), urgent (20), or an integer from -100 to 100")
	}
	return int32(n), nil
}

// parseLimit reads a time limit flag: a duration of whole seconds, 0 for
// none, or def when it was not given.
func parseLimit(flag, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := parseDuration(s)
	if err != nil {
		return 0, api.Errorf(api.CodeInvalidArgs, "%s %q: %v", flag, s, err).
			WithSuggestion(flag + " takes a duration such as 30m or 2h, or 0 for none")
	}
	return d, nil
}

// parseDuration reads a duration of whole seconds, such as 30m or 2h.
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err == nil && (d < 0 || d%time.Second != 0) {
		err = errors.New("not whole seconds")
	}
	return d, err
}

type runCmd struct {
	controllerClient
	sessionFlag
	repoFlag
	Args   []string `arg:"" optional:"" passthrough:"" help:"The command, after --."`
	NoSync bool     `help:"Do not sync the checkout this is run in: the run then stays in the workspace the session has, whose state lives on its node alone."`
	Fork   bool     `help:"Run in a throwaway workspace of its own: a new one, forked from the project's base with this checkout synced in, and deleted, with whatever the command left in it, once the command ends. It neither waits for the session's workspace nor changes it."`
	Detach bool     `help:"Return once the command is queued; it runs until it ends, hotbed stop, or a time limit given. For dev servers and long jobs: it needs --mem (unless an earlier run of the same command says), and runs in a terminal in a VM; read it with hotbed output."`
	Port   int      `help:"Port a dev server listens on, for its URL."`
	Wait   string   `help:"How long to wait on the command before leaving it running, detached (exit 125): it runs on, its output kept whole for hotbed output to read. Such as 10m; 0 to wait until it ends (default: 2m; with --soft-limit, until that pauses it)."`
	// MaxOutput bounds what prints as the command runs.
	MaxOutput string `name:"max-output" default:"32K" help:"Print at most this much of its output as it runs, such as 64K; past it, the rest is kept whole, its last lines print as it ends, and hotbed output reads all of it. 0 to print it all."`
	TTY       bool   `name:"tty" xor:"tty" help:"Run it in a terminal for its input too: it reads what hotbed send types, and a prompt waits for an answer. vm runtime only. (By default a detached run in a VM gets a terminal for its output only: tools draw progress and color, hotbed output settles what they redraw, and a prompt fails at once.)"`
	NoTTY     bool   `name:"no-tty" xor:"tty" help:"Run it without a terminal: output to a file, input from /dev/null."`
	Mem       string `help:"The memory it is expected to hold at its peak, such as 4G: the node reserves it for it. A detached run needs it, unless an earlier run of the same command says."`
	Expect    string `help:"How long it is expected to take, such as 20m: its end says how it compared."`
	Ready     string `help:"With --detach, return once a line of its output matches this regular expression (it is ready): a dev server's \"listening\". Waits at most --ready-timeout."`
	ReadyPort bool   `name:"ready-port" help:"With --detach and --port, return once its port takes connections."`
	// ReadyTimeout bounds --ready and --ready-port.
	ReadyTimeout string `name:"ready-timeout" default:"2m" help:"How long --ready and --ready-port wait at most."`
	commandFlags
	placementFlags
}

func (c *runCmd) SetDashArgs(args []string) (before []string) {
	before, c.Args = c.Args, args
	return before
}

// Raw asks the controller where to run, syncs the checkout it is run in
// there, runs the command, and streams its output to stdout. Interrupting
// stops it.
func (c *runCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	req, err := c.request(c.Args, "hotbed run -- cargo test", !c.Detach)
	if err != nil {
		return 0, false, err
	}
	wait, err := c.runWait()
	if err != nil {
		return 0, false, err
	}
	maxOutput, err := parseSize(c.MaxOutput)
	if err != nil {
		return 0, false, err
	}
	readyTimeout, err := parseLimit("--ready-timeout", c.ReadyTimeout, 2*time.Minute)
	if err != nil {
		return 0, false, err
	}
	if (c.Ready != "" || c.ReadyPort) && !c.Detach {
		return 0, false, api.Errorf(api.CodeInvalidArgs, "--ready and --ready-port wait for a detached run to be ready").
			WithSuggestion("add --detach")
	}
	if c.ReadyPort && c.Port == 0 {
		return 0, false, api.Errorf(api.CodeInvalidArgs, "--ready-port waits for --port: give the port it listens on")
	}
	req.Tty = c.ttyMode()
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return 0, false, err
	}
	repo, err := c.runRepo(ctx, d, session)
	if err != nil {
		return 0, false, err
	}
	var dir string
	if !c.NoSync && repo != "" {
		if dir, err = c.checkoutOf(ctx, d, session, c.Repo, repo); err != nil {
			return 0, false, err
		}
	}
	var memEst int64
	if c.Mem != "" {
		if memEst, err = parseSize(c.Mem); err != nil {
			return 0, false, err
		}
	}
	pl := c.placement(req)
	pl.MemoryEstimateBytes = memEst
	ws, w, err := c.workspaceFor(ctx, d, &pb.CreateWorkspaceRequest{
		SessionId: session, Repo: repo, Placement: pl, SyncDir: dir, Throwaway: c.Fork, WaitForCapacity: !c.Detach,
	})
	if err != nil {
		return 0, false, err
	}
	req.Placement = ws.Placement
	if req.Placement == nil {
		req.Placement = &pb.PlacementDecision{
			SessionId: session,
			Project:   ws.Project,
			Repo:      repo,
			NodeId:    ws.NodeId,
			Hostname:  ws.Node,
			Reason:    "controller unreachable; reused cached workspace",
			Request:   pl,
			NoSync:    dir == "",
			Throwaway: c.Fork,
		}
	}
	if req.Estimate, err = c.estimate(ctx, d, ws.Project, repo, req.Args); err != nil {
		if ws.Throwaway {
			c.discard(ctx, d, ws)
		}
		return 0, false, err
	}
	cmd, err := c.start(ctx, d, w, ws, req, repo, dir)
	if err != nil {
		if ws.Throwaway {
			c.discard(ctx, d, ws)
		}
		return 0, false, err
	}
	if cmd.Project == "" {
		cmd.Project = ws.Project
	}
	if c.Detach {
		if c.Ready != "" || c.ReadyPort {
			serving := func(ctx context.Context, workspace string, port int) bool {
				return c.portServing(ctx, d, workspace, port)
			}
			if err := waitServing(ctx, w, d, cmd, c.Ready, c.ReadyPort, readyTimeout, serving); err != nil {
				return 0, false, err
			}
		}
		d.cockpit.fill(cmd)
		if err := writeOutput(d.stdout, "run", c, cmd, d.output); err != nil {
			return 0, false, err
		}
		return exitOK, true, nil
	}
	where := ws.Id
	if ws.Throwaway {
		where = "throwaway workspace " + ws.Id
	}
	fmt.Fprintf(d.stderr, "hotbed: %s running in %s on %s (hotbed stop -s %s %s)\n", cmd.Id, where, ws.Node, session, cmd.Id)
	out := &outputBudget{w: d.stdout, limit: maxOutput}
	code, detached, err := attachFor(ctx, w, cmd.Id, session, wait, out, d.stderr)
	if out.held > 0 && (err == nil || detached) {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if terr := printTail(tctx, w, cmd.Id, d.stdout); terr != nil {
			fmt.Fprintf(d.stderr, "hotbed: could not read %s's last lines: %v; hotbed output %s reads them\n", cmd.Id, api.FromConnect(terr), cmd.Id)
		}
	}
	if detached {
		fmt.Fprintln(d.stderr, detachedHelp(cmd.Id, wait))
		return exitDetached, true, nil
	}
	return code, true, err
}

// start runs req in ws, once it is ready and dir, a checkout of repo, is
// synced to it.
func (c *runCmd) start(ctx context.Context, d deps, w workerClients, ws *pb.Workspace, req *pb.RunCommandRequest, repo, dir string) (*pb.Command, error) {
	// A run waits for its workspace to be ready (its base pulled and
	// freshened), saying so; a detached one that syncs nothing is queued
	// in it at once instead.
	var err error
	if dir != "" || !c.Detach {
		if ws, err = waitReady(ctx, w, ws, d.stderr); err != nil {
			return nil, err
		}
	}
	if dir != "" {
		res, err := syncTo(ctx, w, ws, repo, dir)
		if err != nil {
			return nil, err
		}
		if !res.Skipped {
			fmt.Fprintf(d.stderr, "hotbed: synced %d changed files (%s) from %s\n", res.FilesChanged, formatBytes(res.BundleBytes), dir)
		}
	}
	req.WorkspaceId, req.Attached, req.Port, req.Repo = ws.Id, !c.Detach, int32(c.Port), repo
	return runCommandWithRetry(ctx, d, w, req)
}

// newClientCommandID generates a command ID matching the worker's format
// (10 lowercase base32 characters) so RunCommand can safely retry across a
// wedged HTTP/2 connection without starting a duplicate command.
func newClientCommandID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.EncodeToString(b))[:10]
}

// runCommandWithRetry calls RunCommand with a client-generated ID and recovers
// from transient transport failures (such as http2: timeout awaiting response
// headers on an idle connection after waitReady/syncTo) by recycling the HTTP
// pool and checking whether the first attempt already landed via GetCommand
// before retrying RunCommand with the same ID.
func runCommandWithRetry(ctx context.Context, d deps, w workerClients, req *pb.RunCommandRequest) (*pb.Command, error) {
	if req.Id == "" {
		req.Id = newClientCommandID()
	}
	var since time.Time
	var noticed bool
	for {
		cmd, err := w.calls.RunCommand(ctx, req)
		if err == nil {
			return cmd, nil
		}
		if ctx.Err() != nil || d.rideOut <= 0 {
			return nil, err
		}
		ae := api.FromConnect(err)
		if ae.Code == api.CodeConflict {
			// A prior attempt with req.Id landed on the worker before its
			// response was lost; fetch that command instead of failing.
			if got, gerr := w.command(ctx, req.Id); gerr == nil {
				return got, nil
			}
			return nil, err
		}
		if !ae.Retryable || (ae.Code != api.CodeControllerUnreachable && ae.Code != api.CodeWorkerUnreachable) {
			return nil, err
		}
		if since.IsZero() {
			since = time.Now()
		}
		if time.Since(since) > d.rideOut {
			return nil, err
		}
		if !noticed {
			fmt.Fprintf(d.stderr, "hotbed: starting command %s hit %s; retrying\n", req.Id, ae.Message)
			noticed = true
		}
		api.ResetClientTransport(d.http)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(time.Second):
		}
		// Check whether the previous attempt actually landed before resending.
		if got, gerr := w.command(ctx, req.Id); gerr == nil {
			return got, nil
		}
	}
}

// discard releases a throwaway workspace no command got to run in, which
// its worker would otherwise keep until the session ends. It runs on after
// an interrupt, which is what stopped the run.
func (c *runCmd) discard(ctx context.Context, d deps, ws *pb.Workspace) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	cl, err := c.clients(d.http)
	if err == nil {
		_, err = cl.workspaces.ReleaseWorkspace(ctx, &pb.ReleaseWorkspaceRequest{Id: ws.Id})
	}
	if err != nil {
		fmt.Fprintf(d.stderr, "hotbed: could not release throwaway workspace %s: %v; the session's end releases it\n", ws.Id, api.FromConnect(err))
	}
}

// attach prints an attached command's output to w until it ends or is
// paused, and returns its exit status (follow). Interrupted, it stops the
// command, which ends with its client, and returns 130 as a shell would.
func attach(ctx context.Context, wk workerClients, id string, w, stderr io.Writer) (int, error) {
	code, err := follow(ctx, wk, id, w, stderr)
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

// runRepo is the project's repo a run syncs and runs in: the one named, or
// the one this checkout is of. Outside any checkout, with none named, it is
// none: the run starts in /work, where every repo of the project is.
func (c *runCmd) runRepo(ctx context.Context, d deps, session string) (string, error) {
	if c.Repo == "" {
		if in, err := d.inCheckout(ctx); err != nil || !in {
			return "", err
		}
	}
	return c.resolveRepo(ctx, d, session, c.Repo, "")
}

// followRideOut is how long follow waits for a worker that stopped answering:
// long enough for it to fetch and start a new build.
var followRideOut = 10 * time.Minute

// follow waits on a command, printing its output until it ends or is paused
// (followLog), and returns its exit status: a shell's 128 plus the signal for
// one a signal killed, and exitPaused, saying so on stderr, for one paused.
// One someone stopped or paused by hand says who, what they wrote, and how
// to read the diagnostics taken of it.
func follow(ctx context.Context, wk workerClients, id string, w, stderr io.Writer) (int, error) {
	c, err := followLog(ctx, wk, id, w, stderr)
	if err != nil {
		return 0, err
	}
	return followStatus(id, c, stderr)
}

func followTimed(ctx context.Context, wk workerClients, id string, w, stderr io.Writer) (int, error) {
	tw := &timestampLineWriter{w: w, now: time.Now()}
	c, err := followLogStream(ctx, wk, id, true, tw.WriteChunk, stderr)
	if ferr := tw.Flush(); err == nil && ferr != nil {
		err = ferr
	}
	if err != nil {
		return 0, err
	}
	return followStatus(id, c, stderr)
}

// timestampLineWriter buffers raw log chunks tagged with their received
// timestamp (unix ms) and writes each line prefixed with its formatted local
// wall time.
type timestampLineWriter struct {
	w      io.Writer
	now    time.Time
	buf    []byte
	lineTs int64
}

func (tw *timestampLineWriter) WriteChunk(p []byte, ts int64) error {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if ts > 0 {
				tw.lineTs = ts
			}
			tw.buf = append(tw.buf, p...)
			return nil
		}
		if ts > 0 {
			tw.lineTs = ts
		}
		tw.buf = append(tw.buf, p[:i]...)
		if err := tw.emit(); err != nil {
			return err
		}
		p = p[i+1:]
	}
	return nil
}

func (tw *timestampLineWriter) Flush() error {
	if len(tw.buf) == 0 {
		return nil
	}
	return tw.emit()
}

func (tw *timestampLineWriter) emit() error {
	if _, err := fmt.Fprintf(tw.w, "%s  ", formatLogTimestamp(tw.lineTs, tw.now)); err != nil {
		return err
	}
	if _, err := tw.w.Write(tw.buf); err != nil {
		return err
	}
	if _, err := io.WriteString(tw.w, "\n"); err != nil {
		return err
	}
	tw.buf = tw.buf[:0]
	tw.lineTs = 0
	return nil
}

func followStatus(id string, c *pb.Command, stderr io.Writer) (int, error) {
	if c == nil {
		return 0, api.Errorf(api.CodeInternal, "%s ended with no command status", id)
	}
	cmdID := c.Id
	if cmdID == "" {
		cmdID = id
	}
	switch {
	case api.PausedByHand(c):
		fmt.Fprintf(stderr, "hotbed: %s was %s\n", cmdID, api.InterventionNote(c, api.ByHand(c)))
		return exitPaused, nil
	case c.State == pb.CommandState_COMMAND_STATE_PAUSED:
		fmt.Fprintf(stderr, "hotbed: %s\n", pausedHelp(c))
		return exitPaused, nil
	case api.StoppedByHand(c):
		iv := api.ByHand(c)
		err := api.Errorf(api.CodeCommandStopped, "%s was %s", cmdID, api.InterventionNote(c, iv))
		if iv.GetDiagnosed() {
			err = err.WithSuggestion(fmt.Sprintf("read the diagnostics: hotbed diagnostics %s", cmdID))
		}
		return 0, err
	case api.Live(c.State):
		return 0, api.Errorf(api.CodeInternal, "%s is still %s", id, enumWord(c.State))
	case c.State == pb.CommandState_COMMAND_STATE_UNSPECIFIED:
		return 0, api.Errorf(api.CodeInternal, "%s returned unspecified state", id)
	case strings.HasPrefix(c.Reason, "killed by the OOM killer"):
		fmt.Fprintf(stderr, "hotbed: %s: %s\n", id, c.Reason)
		if c.ExitCode != nil && *c.ExitCode > 0 {
			return int(*c.ExitCode), nil
		}
		return 137, nil
	case strings.HasPrefix(c.Reason, "its VM "):
		// Its VM ended under it: the command never exited, so there is no
		// code of its own to return, and running it again can succeed.
		err := api.Errorf(api.CodeWorkspaceFailed, "%s failed: %s", cmdID, c.Reason).
			WithSuggestion("its VM failed, not the command: run it again")
		err.Retryable = true
		return 0, err
	case strings.HasPrefix(c.Reason, "killed by signal "):
		// Report it the way a shell would: 128 plus the signal.
		fmt.Fprintf(stderr, "hotbed: %s: %s\n", id, c.Reason)
		sigFields := strings.Fields(strings.TrimPrefix(c.Reason, "killed by signal "))
		sig := 0
		if len(sigFields) > 0 {
			sig, _ = strconv.Atoi(sigFields[0])
		}
		return 128 + sig, nil
	case c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED:
		if c.ExitCode != nil {
			return int(*c.ExitCode), nil
		}
		return 0, nil
	case c.ExitCode != nil && *c.ExitCode > 0:
		return int(*c.ExitCode), nil
	case c.State == pb.CommandState_COMMAND_STATE_CANCELLED:
		return 0, api.Errorf(api.CodeInternal, "%s was cancelled: %s", id, c.Reason)
	default:
		return 0, api.Errorf(api.CodeInternal, "%s failed: %s", id, c.Reason)
	}
}

// pausedHelp says that its soft time limit paused c, and what to do next.
func pausedHelp(c *pb.Command) string {
	soft := time.Duration(c.GetLimits().GetSoftLimitSeconds()) * time.Second
	stops := "it has no hard limit"
	if hard := c.GetLimits().GetHardLimitSeconds(); hard > 0 {
		stops = fmt.Sprintf("its hard limit stops it %s after it started", time.Duration(hard)*time.Second)
	}
	return fmt.Sprintf("%s is paused: it ran %s, its soft limit, without ending. "+
		"To wait on it %s more, which resumes it: hotbed logs -f -s %s %s (%s). To end it: hotbed stop -s %s %s",
		c.Id, soft, soft, c.SessionId, c.Id, stops, c.SessionId, c.Id)
}

var (
	followWaitNoticeAfter  = 3 * time.Second
	followWaitPollInterval = 5 * time.Second
)

// followLog waits on a command (FollowLog with wait, which resumes it if
// paused), copying its output to w until it ends or is paused, picking up
// where it left off if the stream drops, and returns it as it is then.
// When no output arrives for followWaitNoticeAfter and the command is queued or
// parked, it writes a deduplicated status line to stderr so the caller sees why
// it is waiting instead of staring at silence.
func followLog(ctx context.Context, wk workerClients, id string, w, stderr io.Writer) (*pb.Command, error) {
	return followLogStream(ctx, wk, id, false, func(p []byte, _ int64) error {
		_, err := w.Write(p)
		return err
	}, stderr)
}

func followLogStream(ctx context.Context, wk workerClients, id string, timestamps bool, writeChunk func([]byte, int64) error, stderr io.Writer) (*pb.Command, error) {
	var offset int64
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	watchCtx, cancelWatch := context.WithCancel(ctx)
	var watchWG sync.WaitGroup
	if stderr != nil {
		watchWG.Add(1)
		go func() {
			defer watchWG.Done()
			delay := followWaitNoticeAfter
			if delay <= 0 {
				delay = 3 * time.Second
			}
			interval := followWaitPollInterval
			if interval <= 0 {
				interval = 5 * time.Second
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			lastMsg := ""
			for {
				select {
				case <-watchCtx.Done():
					return
				case <-timer.C:
				}
				if time.Since(time.Unix(0, lastActivity.Load())) >= delay {
					qctx, qcancel := context.WithTimeout(watchCtx, 3*time.Second)
					cmd, err := wk.command(qctx, id)
					qcancel()
					if err == nil && cmd != nil && watchCtx.Err() == nil {
						switch cmd.State {
						case pb.CommandState_COMMAND_STATE_QUEUED, pb.CommandState_COMMAND_STATE_PARKED:
							stateWord := enumWord(cmd.State)
							msg := fmt.Sprintf("hotbed: %s %s\n", id, stateWord)
							if cmd.Reason != "" {
								msg = fmt.Sprintf("hotbed: %s %s: %s\n", id, stateWord, cmd.Reason)
							}
							if msg != lastMsg {
								_, _ = io.WriteString(stderr, msg)
								lastMsg = msg
							}
						case pb.CommandState_COMMAND_STATE_RUNNING:
							lastMsg = ""
						}
					}
				}
				timer.Reset(interval)
			}
		}()
	}
	defer func() {
		cancelWatch()
		watchWG.Wait()
	}()

	// A worker that stops answering is waited for (riding out its restart,
	// such as a new build's) for followRideOut, from its first failure.
	var down time.Time
	wait := func(err error) bool {
		if ctx.Err() != nil || !api.FromConnect(err).Retryable {
			return false
		}
		if down.IsZero() {
			down = time.Now()
		}
		if time.Since(down) > followRideOut {
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
		in, err := wk.stream.FollowLog(ctx, &pb.FollowLogRequest{Id: id, Offset: offset, Follow: true, Wait: true, Timestamps: timestamps})
		if err != nil {
			if wait(err) {
				continue
			}
			return nil, err
		}
		// Why the stream ended, unless it ended as it should: a wait that
		// could not resume the command is that, not a new pause.
		var broke error
		var terminalState pb.CommandState
		var terminalEndedBy *pb.CommandIntervention
		for {
			chunk, err := in.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					down = time.Time{}
				} else {
					broke = err
					if api.FromConnect(err).Retryable && down.IsZero() {
						down = time.Now()
					}
				}
				break
			}
			down = time.Time{}
			if len(chunk.Data) > 0 {
				lastActivity.Store(time.Now().UnixNano())
				if werr := writeChunk(chunk.Data, chunk.ReceivedAt); werr != nil {
					in.Close()
					return nil, werr
				}
			}
			if chunk.NextOffset > offset {
				offset = chunk.NextOffset
			}
			if chunk.State != pb.CommandState_COMMAND_STATE_UNSPECIFIED {
				terminalState = chunk.State
			}
			if chunk.EndedBy != nil {
				terminalEndedBy = chunk.EndedBy
			}
		}
		in.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// If the stream ended cleanly with a terminal pause or manual stop
		// chunk, preserve that terminal state/intervention even if a subsequent
		// GetCommand sees a quick resume (RUNNING/SUCCEEDED) or concurrent edit.
		terminalHold := broke == nil && (terminalState == pb.CommandState_COMMAND_STATE_PAUSED ||
			(terminalState == pb.CommandState_COMMAND_STATE_CANCELLED && terminalEndedBy != nil))
		var c *pb.Command
		for {
			c, err = wk.command(ctx, id)
			if err == nil {
				break
			}
			if wait(err) {
				if terminalHold {
					continue
				}
				break
			}
			return nil, err
		}
		if err != nil {
			continue
		}
		if broke == nil {
			down = time.Time{}
		}
		if terminalHold {
			return applyFollowTerminal(id, c, terminalState, terminalEndedBy), nil
		}
		if c.State == pb.CommandState_COMMAND_STATE_PAUSED && broke != nil && !api.FromConnect(broke).Retryable {
			return nil, broke
		}
		if api.Live(c.State) && c.State != pb.CommandState_COMMAND_STATE_PAUSED {
			if broke != nil {
				if !api.FromConnect(broke).Retryable {
					return nil, broke
				}
				if !wait(broke) {
					return nil, broke
				}
			}
			continue
		}
		if broke != nil || terminalState == pb.CommandState_COMMAND_STATE_UNSPECIFIED {
			// The stream ended before delivering a terminal chunk from the
			// worker (e.g. a worker restart, or a clean idle EOF from an edge
			// proxy while the command was queued before exiting quickly); drain
			// any output written after offset without setting Wait (which would
			// resume a paused command).
			if err := drainLog(ctx, wk, id, offset, timestamps, writeChunk, wait); err != nil {
				return nil, err
			}
		}
		return c, nil
	}
}

// applyFollowTerminal overlays a clean terminal LogChunk's state and manual
// intervention onto c so a quick resume after a hand pause (or a concurrent
// update after a hand stop) cannot hide the intervention that ended the wait.
func applyFollowTerminal(id string, c *pb.Command, state pb.CommandState, endedBy *pb.CommandIntervention) *pb.Command {
	if state == pb.CommandState_COMMAND_STATE_UNSPECIFIED && endedBy == nil {
		return c
	}
	var out *pb.Command
	if c != nil {
		out = proto.Clone(c).(*pb.Command)
	} else {
		out = &pb.Command{Id: id}
	}
	if out.Id == "" {
		out.Id = id
	}
	switch {
	case state == pb.CommandState_COMMAND_STATE_PAUSED && endedBy != nil:
		out.State = pb.CommandState_COMMAND_STATE_PAUSED
		out.Interventions = []*pb.CommandIntervention{proto.Clone(endedBy).(*pb.CommandIntervention)}
	case state == pb.CommandState_COMMAND_STATE_PAUSED:
		if out.State != pb.CommandState_COMMAND_STATE_PAUSED {
			out.State = pb.CommandState_COMMAND_STATE_PAUSED
			out.Interventions = nil
		}
	case state == pb.CommandState_COMMAND_STATE_CANCELLED && endedBy != nil:
		out.State = pb.CommandState_COMMAND_STATE_CANCELLED
		out.Interventions = []*pb.CommandIntervention{proto.Clone(endedBy).(*pb.CommandIntervention)}
	}
	return out
}

// drainLog reads any remaining output of id from offset without following or
// setting Wait (so it never resumes a paused command).
func drainLog(ctx context.Context, wk workerClients, id string, offset int64, timestamps bool, writeChunk func([]byte, int64) error, wait func(error) bool) error {
	for {
		in, err := wk.stream.FollowLog(ctx, &pb.FollowLogRequest{Id: id, Offset: offset, Timestamps: timestamps})
		if err != nil {
			if api.CodeOf(err) == api.CodeNotFound {
				return nil
			}
			if wait(err) {
				continue
			}
			return err
		}
		var recvErr error
		for {
			chunk, err := in.Receive()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					recvErr = err
				}
				break
			}
			if len(chunk.Data) > 0 {
				if werr := writeChunk(chunk.Data, chunk.ReceivedAt); werr != nil {
					in.Close()
					return werr
				}
			}
			offset = chunk.NextOffset
		}
		in.Close()
		if recvErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if api.CodeOf(recvErr) == api.CodeNotFound {
			return nil
		}
		if wait(recvErr) {
			continue
		}
		return recvErr
	}
}

type logsCmd struct {
	controllerClient
	sessionFlag
	Command    string `arg:"" help:"The command's ID."`
	Follow     bool   `short:"f" xor:"tail" help:"Wait on the command: print its output until it ends, and exit with its status (124 if its soft time limit pauses it; waiting resumes a paused one)."`
	Tail       int    `short:"n" xor:"tail" help:"Print only the last N lines."`
	Timestamps bool   `short:"t" help:"Prefix each line with the local time it was received (HH:MM:SS.mmm)."`
}

// Raw prints the command's output as is, from its worker: any session's
// command, or a base's freshen, whose log a failed add names. Waiting on
// one (-f) resumes it if paused and keeps its session in use, so it takes
// the session's own command, or a freshen.
func (c *logsCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	if c.Tail < 0 {
		return 0, false, api.Errorf(api.CodeInvalidArgs, "--tail %d is negative", c.Tail).WithSuggestion("pass --tail N with N > 0")
	}
	session, err := c.sessionFlag.optional(ctx, d)
	if err != nil {
		return 0, false, err
	}
	cmd, w, err := c.findCommand(ctx, d, session, c.Command)
	if err != nil {
		return 0, false, err
	}
	if c.Follow && cmd.SessionId != "" && cmd.SessionId != session {
		return 0, false, api.Errorf(api.CodeInvalidArgs, "waiting on %s would resume it and keep session %s in use", c.Command, cmd.SessionId).
			WithSuggestion(ownerSuggestion(cmd) + "; or read it without -f")
	}
	if c.Tail > 0 {
		log, err := w.calls.GetLogLines(ctx, &pb.GetLogLinesRequest{Id: c.Command, Tail: int32(c.Tail)})
		if err != nil {
			return 0, false, err
		}
		if log.Cut {
			fmt.Fprintf(d.stderr, "hotbed: only the last %d lines fit in one response; hotbed logs %s without -n prints it all\n", len(log.Lines), c.Command)
		}
		now := time.Now()
		for i, line := range log.Lines {
			if c.Timestamps {
				var ts int64
				if i < len(log.LineTimestamps) {
					ts = log.LineTimestamps[i]
				}
				if _, err := fmt.Fprintf(d.stdout, "%s  %s\n", formatLogTimestamp(ts, now), line); err != nil {
					return 0, true, err
				}
				continue
			}
			if _, err := fmt.Fprintln(d.stdout, line); err != nil {
				return 0, true, err
			}
		}
		return exitOK, true, nil
	}
	if c.Follow {
		// Until it ends or is paused, across its worker's restarts, exiting
		// with its status as attached run does.
		var code int
		var err error
		if c.Timestamps {
			code, err = followTimed(ctx, w, c.Command, d.stdout, d.stderr)
		} else {
			code, err = follow(ctx, w, c.Command, d.stdout, d.stderr)
		}
		if ctx.Err() != nil {
			return 130, true, nil
		}
		return code, true, err
	}
	in, err := w.stream.FollowLog(ctx, &pb.FollowLogRequest{Id: c.Command, Timestamps: c.Timestamps})
	if err != nil {
		return 0, false, err
	}
	defer in.Close()
	var tw *timestampLineWriter
	if c.Timestamps {
		tw = &timestampLineWriter{w: d.stdout, now: time.Now()}
	}
	for {
		chunk, err := in.Receive()
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			if tw != nil {
				if ferr := tw.Flush(); ferr != nil {
					return 0, true, ferr
				}
			}
			return exitOK, true, nil
		}
		if err != nil {
			return 0, true, err
		}
		if tw != nil {
			if err := tw.WriteChunk(chunk.Data, chunk.ReceivedAt); err != nil {
				return 0, true, err
			}
			continue
		}
		if _, err := d.stdout.Write(chunk.Data); err != nil {
			return 0, true, err
		}
	}
}

func (c *logsCmd) Exec(context.Context, deps) (any, error) { return nil, errors.New("logs is raw") }

// byHandFlags are what a stop or pause by hand says and does first.
type byHandFlags struct {
	Comment  string `short:"m" help:"Why, for whoever waits on the command: the agent waiting on it is told."`
	Diagnose bool   `help:"Take the command's diagnostics first (its activity sample and inspection), kept for hotbed diagnostics."`
}

// ownCommand is the command a stop, pause or resume acts on, as the session
// that runs it.
type ownCommand struct {
	controllerClient
	sessionFlag
	Command string `arg:"" help:"The command's ID."`
}

// locate is the session, the command and its worker.
func (c ownCommand) locate(ctx context.Context, d deps) (string, *pb.Command, workerClients, error) {
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return "", nil, workerClients{}, err
	}
	cmd, w, err := c.findOwnCommand(ctx, d, session, c.Command)
	return session, cmd, w, err
}

type stopCmd struct {
	ownCommand
	byHandFlags
}

func (c *stopCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, cmd, w, err := c.locate(ctx, d)
	if err != nil {
		return nil, err
	}
	if !api.Live(cmd.State) {
		// It has ended: as it ended, as stopping it would say, even once
		// its worker has forgotten it.
		return cmd, nil
	}
	callCtx, cancel, client := w.interventionClient(ctx, c.Diagnose)
	defer cancel()
	return client.StopCommand(callCtx, &pb.StopCommandRequest{
		Id:        c.Command,
		Comment:   c.Comment,
		Diagnose:  c.Diagnose,
		SessionId: session,
	})
}

type pauseCmd struct {
	ownCommand
	byHandFlags
}

// Exec pauses a command by hand: it stays paused, whoever waits on it, until
// hotbed resume.
func (c *pauseCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, _, w, err := c.locate(ctx, d)
	if err != nil {
		return nil, err
	}
	callCtx, cancel, client := w.interventionClient(ctx, c.Diagnose)
	defer cancel()
	return client.PauseCommand(callCtx, &pb.PauseCommandRequest{
		Id:        c.Command,
		Comment:   c.Comment,
		Diagnose:  c.Diagnose,
		SessionId: session,
	})
}

type resumeCmd struct {
	ownCommand
	Comment string `short:"m" help:"Why, for the command's record."`
}

func (c *resumeCmd) Exec(ctx context.Context, d deps) (any, error) {
	session, _, w, err := c.locate(ctx, d)
	if err != nil {
		return nil, err
	}
	return w.calls.ResumeCommand(ctx, &pb.ResumeCommandRequest{
		Id:        c.Command,
		Comment:   c.Comment,
		SessionId: session,
	})
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
