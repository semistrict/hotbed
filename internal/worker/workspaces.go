package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/storage"
)

const released = pb.WorkspaceState_WORKSPACE_STATE_RELEASED

const (
	preparing = pb.WorkspaceState_WORKSPACE_STATE_PREPARING
	idle      = pb.WorkspaceState_WORKSPACE_STATE_IDLE
	failedWS  = pb.WorkspaceState_WORKSPACE_STATE_FAILED
	activeWS  = pb.WorkspaceState_WORKSPACE_STATE_ACTIVE
)

// CreateWorkspace gives a session a workspace of its project with every
// repo at its branch's head: a fork of the newest base when that is at the
// heads, or, while the base is brought up to date, a workspace that is
// preparing and becomes a fork of the new base when its freshen ends.
func (e *Engine) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	if req.Id == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "workspace has no ID")
	}
	if err := validateID(req.Id); err != nil {
		return nil, err
	}
	p := req.Project
	if p == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "workspace %s names no project", req.Id)
	}
	e.mu.Lock()
	if e.systemUpgrading {
		e.mu.Unlock()
		return nil, &api.Error{Code: api.CodeConflict, Message: "node is upgrading cladefs; try again shortly", Retryable: true}
	}
	// The controller's word that this node keeps the project, newer than its
	// last heartbeat's.
	e.vouched[p.Name] = e.now()
	e.recordUserActivityLocked(e.now())
	_, dup := e.workspaces[req.Id]
	e.mu.Unlock()
	if dup {
		return nil, api.Errorf(api.CodeConflict, "workspace %s exists", req.Id)
	}
	wsRuntime, err := e.resolveWorkspaceRuntime(req)
	if err != nil {
		return nil, err
	}
	if wsRuntime == "vm" && e.cfg.VM != nil {
		return e.createVMWorkspace(ctx, req)
	}
	if wsRuntime == "cladefs" && e.cfg.CladefsFUSE != nil {
		return e.createCladefsFUSEWorkspace(ctx, req)
	}
	lock := e.projectLock(p.Name)
	lock.Lock()
	base, note, deferBG, err := e.baseForWithOpts(ctx, p, req.GetSyncDir() != "", false)
	lock.Unlock()
	if err != nil {
		return nil, err
	}
	if deferBG {
		e.scheduleOptionalFreshen(p, wsRuntime, nil)
	}
	now := e.nowMs()
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: req.Id, Project: p.Name, BaseId: base.Id, NodeId: e.cfg.NodeID, Agent: req.Agent,
			Repos: reposOf(base), State: preparing, CreatedAt: now, LastActiveAt: now,
			SessionId: req.SessionId, SyncHost: req.SyncHost, Runtime: wsRuntime, Throwaway: req.Throwaway,
			Reason: fmt.Sprintf("waiting for base %s to freshen", base.Id),
		},
		Volume: "ws/" + req.Id, BaseSnapshot: base.Id, Note: note,
	}}
	e.mu.Lock()
	if _, dup := e.workspaces[req.Id]; dup {
		e.mu.Unlock()
		return nil, api.Errorf(api.CodeConflict, "workspace %s exists", req.Id)
	}
	e.workspaces[req.Id] = w
	e.saveWorkspace(w, change{what: "created", detail: w.ws().Reason})
	// The base's freshen may have ended between baseFor and now, before
	// this workspace was there to be forked or failed with it.
	state := base.State
	if b, ok := e.bases[base.Id]; ok {
		state = b.State
	}
	if b, ok := e.bases[base.Id]; ok && state == broken {
		e.baseFailed(b, baseFailure(b))
	}
	e.mu.Unlock()
	if state == warm {
		if err := e.fork(ctx, w); err != nil {
			return nil, err
		}
	}
	return e.Workspace(req.Id)
}

// reposOf is a new workspace's repos: each at its base's commit, nothing
// synced yet.
func reposOf(b *pb.Base) []*pb.WorkspaceRepo {
	out := make([]*pb.WorkspaceRepo, 0, len(b.Repos))
	for _, r := range b.Repos {
		out = append(out, &pb.WorkspaceRepo{Repo: r.Repo, Head: r.Commit, BaseCommit: r.Commit})
	}
	return out
}

// repoOf is one of a workspace's repos, or nil.
func repoOf(ws *pb.Workspace, repo string) *pb.WorkspaceRepo {
	for _, r := range ws.Repos {
		if r.Repo == repo {
			return r
		}
	}
	return nil
}

// fork makes a preparing workspace's volume from its base's snapshot. A
// note (such as why the base is not up to date) becomes its reason. Only one
// caller may fork a given workspace; if another fork is already in flight or
// the workspace is no longer preparing, fork is a no-op.
func (e *Engine) fork(ctx context.Context, w *workspace) error {
	e.mu.Lock()
	if w.forking || w.releasing || w.ws().State != preparing {
		e.mu.Unlock()
		return nil
	}
	w.forking = true
	done := make(chan struct{})
	w.forkDone = done
	snap, vol, note, wsID := w.rec.BaseSnapshot, w.rec.Volume, w.rec.Note, w.ws().Id
	e.mu.Unlock()
	start := e.now()
	err := e.cfg.Store.Clone(ctx, snap, vol)
	if err == nil && e.usesRuntime(wsID) {
		err = e.cfg.Runtime.EnsureReady(ctx, wsID)
	}
	forkMs := e.now().Sub(start).Milliseconds()
	e.mu.Lock()
	defer func() {
		w.forking = false
		w.forkDone = nil
		close(done)
		e.mu.Unlock()
	}()
	ws := w.ws()
	if ws.State == released {
		return nil
	}
	if w.releasing {
		// ReleaseWorkspace waits on forkDone before destroying the volume.
		// Record the completed outcome so that if ReleaseWorkspace fails and
		// rolls back, the workspace transitions cleanly instead of staying
		// stranded in PREPARING.
		w.pendingFork = &forkOutcome{snap: snap, note: note, forkMs: forkMs, err: err}
		return nil
	}
	if e.closing && errors.Is(err, context.Canceled) {
		return err
	}
	e.finishFork(w, snap, note, forkMs, err)
	return err
}

// finishFork applies the result of Store.Clone to a preparing workspace and
// wakes any waiting commands or watchers. Call with e.mu held.
func (e *Engine) finishFork(w *workspace, snap, note string, forkMs int64, err error) {
	ws := w.ws()
	if err != nil {
		ws.State, ws.Reason = failedWS, "could not fork its base: "+err.Error()
		e.saveWorkspace(w, change{what: "failed", detail: ws.Reason})
		for _, c := range e.commands {
			if c.cmd().WorkspaceId == ws.Id && c.cmd().State == queued {
				e.end(c, failed, nil, "its workspace failed: "+ws.Reason)
			}
		}
		return
	}
	ws.State = idle
	if e.workspaceBusy(ws.Id) {
		ws.State = activeWS
	}
	ws.Reason, ws.ForkMillis = note, forkMs
	if b, ok := e.bases[snap]; ok {
		ws.SharedBytes = b.SizeBytes
	}
	ws.LastActiveAt = e.nowMs()
	e.saveWorkspace(w, change{
		what: "ready", detail: fmt.Sprintf("forked from %s in %d ms", snap, forkMs),
		feed: feed(feedFork, fmt.Sprintf("fork %s ← %s in %d ms · %s", ws.Id, snap, forkMs, ws.Agent), ref("workspace", ws.Id)),
	})
	e.stepSoon()
}

// forkPrepared forks every workspace that waited for base.
func (e *Engine) forkPrepared(ctx context.Context, base string) {
	e.mu.Lock()
	var waiting []*workspace
	for _, w := range e.workspaces {
		if w.ws().State == preparing && !w.forking && !w.releasing && w.ws().BaseId == base {
			waiting = append(waiting, w)
		}
	}
	e.mu.Unlock()
	for _, w := range waiting {
		if ctx.Err() != nil {
			return
		}
		if err := e.fork(ctx, w); err != nil && ctx.Err() == nil {
			slog.Error("fork prepared workspace", "workspace", w.ws().Id, "err", err)
		}
	}
}

// baseFailed deals with the workspaces that waited for base b, which failed
// to freshen: they fork the project's newest warm base instead, with a note
// saying why it is not up to date, or fail when there is none. One broken
// step does not stop agents from working. Call with e.mu held.
func (e *Engine) baseFailed(b *pb.Base, why string) {
	var fallback *pb.Base
	for _, o := range e.bases {
		if o.Project == b.Project && !isCladefsBase(o) && o.State == warm && (fallback == nil || o.Generation > fallback.Generation) {
			fallback = o
		}
	}
	for _, w := range e.workspaces {
		ws := w.ws()
		if ws.State != preparing || w.forking || w.releasing || ws.BaseId != b.Id {
			continue
		}
		if fallback != nil {
			ws.BaseId, ws.Repos = fallback.Id, reposOf(fallback)
			w.rec.BaseSnapshot = fallback.Id
			w.rec.Note = fmt.Sprintf("on %s, not up to date: %s", fallback.Id, why)
			e.saveWorkspace(w, change{what: "rebased", detail: w.rec.Note})
			wsID, w := ws.Id, w
			e.startBgLocked(func(ctx context.Context) {
				if err := e.fork(ctx, w); err != nil && ctx.Err() == nil {
					slog.Error("fork workspace onto an older base", "workspace", wsID, "err", err)
				}
			})
			continue
		}
		ws.State, ws.Reason = failedWS, why
		e.saveWorkspace(w, change{what: "failed", detail: why})
		for _, c := range e.commands {
			if c.cmd().WorkspaceId == ws.Id && c.cmd().State == queued {
				e.end(c, failed, nil, "its workspace failed: "+why)
			}
		}
	}
}

// workspaceView returns a clone of w's workspace enriched with live freshen
// step / queue status while it is preparing. Call with e.mu held.
func (e *Engine) workspaceView(w *workspace) *pb.Workspace {
	ws := clone(w.ws())
	if ws.State != preparing {
		return ws
	}
	b, ok := e.bases[ws.BaseId]
	if !ok || b.Command == "" {
		return ws
	}
	c, ok := e.commands[b.Command]
	if !ok {
		return ws
	}
	switch cmd := c.cmd(); {
	case cmd.State == queued && cmd.Reason != "":
		ws.Reason = fmt.Sprintf("base %s queued: %s", ws.BaseId, cmd.Reason)
	case cmd.State == parked && cmd.Reason != "":
		ws.Reason = fmt.Sprintf("base %s parked: %s", ws.BaseId, cmd.Reason)
	default:
		if step, steps, name, allDone := c.out.freshenStep(); allDone {
			if isCladefsBase(b) {
				ws.Reason = fmt.Sprintf("syncing base %s to R2", ws.BaseId)
			} else {
				ws.Reason = fmt.Sprintf("snapshotting base %s", ws.BaseId)
			}
		} else if step > 0 {
			ws.Reason = fmt.Sprintf("freshening base %s: step %d/%d %s", ws.BaseId, step, steps, name)
		}
	}
	return ws
}

// WorkspaceFreshenCommand returns the ID of the freshen command building a
// preparing workspace's base, or "" if the workspace is not waiting on one.
func (e *Engine) WorkspaceFreshenCommand(id string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok || w.ws().State != preparing {
		return ""
	}
	if b, ok := e.bases[w.ws().BaseId]; ok {
		return b.Command
	}
	return ""
}

// Workspace returns a workspace this node has.
func (e *Engine) Workspace(id string) (*pb.Workspace, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	return e.workspaceView(w), nil
}

// WatchWorkspace returns the workspace now and a channel closed when it, or
// anything else of the engine's, next changes.
func (e *Engine) WatchWorkspace(id string) (*pb.Workspace, <-chan struct{}, error) {
	if err := validateID(id); err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok {
		return nil, nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	return e.workspaceView(w), e.updated, nil
}

// Workspaces lists the workspaces this node has, released ones included for a while.
func (e *Engine) Workspaces() []*pb.Workspace {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*pb.Workspace, 0, len(e.workspaces))
	for _, w := range e.workspaces {
		out = append(out, e.workspaceView(w))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// ReleaseWorkspace stops the workspace's commands and deletes its volume.
// ReleaseWorkspaceAsync schedules ReleaseWorkspace in a tracked background
// goroutine so heartbeat replies never block on a workspace that is still
// waiting on an in-flight fork/base build or slow storage teardown.
func (e *Engine) ReleaseWorkspaceAsync(id string) {
	if err := validateID(id); err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.releaseWorkspaceAsyncLocked(id)
}

// releaseWorkspaceAsyncLocked is ReleaseWorkspaceAsync, called with e.mu
// held.
func (e *Engine) releaseWorkspaceAsyncLocked(id string) {
	w, ok := e.workspaces[id]
	if !ok || w.ws().State == released || w.releasing || w.releaseQueued {
		return
	}
	w.releaseQueued = true
	started := e.startBgLocked(func(bgCtx context.Context) {
		defer func() {
			e.mu.Lock()
			w.releaseQueued = false
			e.mu.Unlock()
		}()
		if err := e.ReleaseWorkspace(bgCtx, id); err != nil && api.CodeOf(err) != api.CodeNotFound && bgCtx.Err() == nil {
			slog.Error("release workspace in the background", "workspace", id, "err", err)
		}
	})
	if !started {
		w.releaseQueued = false
	}
}

func (e *Engine) ReleaseWorkspace(ctx context.Context, id string) (retErr error) {
	if err := validateID(id); err != nil {
		return err
	}
	var w *workspace
	var releaseDone, forkDone chan struct{}
	var running []string
	var shells []*activeShell
	for {
		e.mu.Lock()
		var ok bool
		w, ok = e.workspaces[id]
		if !ok || w.ws().State == released {
			e.mu.Unlock()
			return api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
		}
		if w.releasing {
			done := w.releaseDone
			e.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
			e.mu.Lock()
			if w.ws().State == released {
				e.mu.Unlock()
				return api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
			}
			err := w.releaseErr
			releasingAgain := w.releasing
			e.mu.Unlock()
			if err != nil {
				return err
			}
			if releasingAgain {
				continue
			}
			return api.Errorf(api.CodeInternal, "release of workspace %s did not complete", id)
		}
		w.releasing = true
		releaseDone = make(chan struct{})
		w.releaseDone = releaseDone
		w.releaseErr = nil
		forkDone = w.forkDone
		for _, c := range e.commands {
			if c.cmd().WorkspaceId == id && api.Live(c.cmd().State) {
				running = append(running, c.cmd().Id)
			}
		}
		for _, sh := range e.shells {
			if sh.workspaceID == id {
				shells = append(shells, sh)
			}
		}
		e.mu.Unlock()
		break
	}

	releasedOK := false
	defer func() {
		refork := false
		e.mu.Lock()
		if !releasedOK {
			w.releasing = false
			w.releaseErr = retErr
			if w.releaseErr == nil {
				w.releaseErr = errors.New("workspace release aborted")
			}
			if pf := w.pendingFork; pf != nil {
				w.pendingFork = nil
				if w.ws().State == preparing {
					e.finishFork(w, pf.snap, pf.note, pf.forkMs, pf.err)
				}
			} else if w.ws().State == preparing && !w.forking {
				if b, ok := e.bases[w.ws().BaseId]; ok {
					switch b.State {
					case warm:
						refork = true
					case broken:
						e.baseFailed(b, baseFailure(b))
					}
				}
			}
			if w.ws().State != preparing && e.workspaceBusy(id) {
				e.stepSoon()
			}
		} else {
			w.releaseErr = nil
		}
		if w.releaseDone == releaseDone {
			w.releaseDone = nil
		}
		close(releaseDone)
		if refork {
			e.startBgLocked(func(ctx context.Context) {
				if err := e.fork(ctx, w); err != nil && ctx.Err() == nil {
					slog.Error("fork workspace after failed release", "workspace", id, "err", err)
				}
			})
		}
		e.mu.Unlock()
	}()

	// Wait for any in-flight Store.Clone on this workspace before destroying
	// its volume.
	if forkDone != nil {
		select {
		case <-forkDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, cid := range running {
		if _, err := e.Stop(ctx, cid, "workspace released"); err != nil {
			return err
		}
	}
	for _, sh := range shells {
		sh.cancel()
	}
	for _, sh := range shells {
		select {
		case <-sh.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// A mount left in a command's tmp would keep the volume busy.
	if err := e.tmpsGone(ctx, id); err != nil {
		return err
	}
	// Coordinate volume teardown with any in-flight Sync so we never destroy
	// a volume mid-gitops.Apply.
	w.sync.Lock()
	defer w.sync.Unlock()

	e.mu.Lock()
	if w.ws().State == released {
		e.mu.Unlock()
		return api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	vol := w.rec.Volume
	wsRT := w.ws().Runtime
	e.mu.Unlock()

	if e.usesRuntime(id) {
		if err := e.cfg.Runtime.Teardown(ctx, id); err != nil {
			slog.Warn("teardown workspace runtime on release", "workspace", id, "err", err)
		}
		if e.cfg.VM != nil {
			if err := e.cfg.VM.DeleteWorkspaceCell(ctx, id); err != nil {
				slog.Warn("delete cladefs workspace cell on release", "workspace", id, "err", err)
			}
			vol = "" // cladefs cell deleted directly; no ZFS/btrfs volume
		}
	} else if wsRT == "cladefs" && e.cfg.CladefsFUSE != nil {
		if err := e.cfg.CladefsFUSE.DeleteWorkspace(ctx, id); err != nil {
			slog.Warn("delete cladefs FUSE workspace on release", "workspace", id, "err", err)
		}
		vol = "" // cladefs FUSE cell deleted directly; no ZFS/btrfs volume
	}

	freed := true
	if vol != "" {
		var err error
		freed, err = e.destroyVolume(ctx, vol)
		if err != nil {
			return err
		}
		if !freed {
			slog.Warn("workspace volume busy on release; collection retries it", "workspace", id, "volume", vol)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if w.ws().State == released {
		return api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	w.ws().State = released
	w.releasing = false
	w.pendingFork = nil
	releasedOK = true
	w.ws().LastActiveAt = e.nowMs()
	if freed {
		w.rec.Volume = ""
	}
	e.saveWorkspace(w, change{
		what: "released",
		feed: feed(feedRelease, fmt.Sprintf("release %s · %s · %s freed", id, w.ws().Agent, gib(w.ws().ExclusiveBytes)), ref("workspace", id)),
	})
	e.dropSuperseded(w.ws().Project)
	e.collectSoon()
	return nil
}

const syncInProgressPrefix = "syncing "

func syncInProgressReason(repo string) string { return syncInProgressPrefix + repo }

func isSyncInProgress(reason string) bool { return strings.HasPrefix(reason, syncInProgressPrefix) }

func syncInProgressRepo(reason string) string {
	return strings.TrimPrefix(reason, syncInProgressPrefix)
}

// reconcileLoaded resolves persisted crash-window states on worker startup:
//   - Workspaces whose sync was interrupted mid-Apply (still marked with
//     syncInProgressPrefix) are transitioned to WORKSPACE_STATE_FAILED with
//     cleared repo metadata so no command runs on an unknown checkout and no
//     equal-tree sync is skipped (board 8890).
//   - Queued commands belonging to failed, released, or missing workspaces are
//     ended immediately so they can never be admitted after restart (board 8892).
func (e *Engine) reconcileLoaded() {
	e.mu.Lock()
	defer e.mu.Unlock()
	baseIDs := make([]string, 0, len(e.bases))
	for id := range e.bases {
		baseIDs = append(baseIDs, id)
	}
	sort.Strings(baseIDs)
	for _, id := range baseIDs {
		b := e.bases[id]
		if !isCladefsBase(b) || b.State != building {
			continue
		}
		// Managed VM / cladefs-FUSE base builds finalize inside an in-memory
		// EnsureProjectBase goroutine that does not survive a worker restart.
		now := e.nowMs()
		b.State = broken
		b.BuiltAt = &now
		why := fmt.Sprintf("base %s build interrupted by worker restart", b.Id)
		e.saveBase(b, change{what: "failed", detail: why})
		if b.Command != "" {
			if c, ok := e.commands[b.Command]; ok && api.Live(c.cmd().State) {
				e.end(c, cancelled, nil, "interrupted by worker restart")
			}
		}
	}
	wsIDs := make([]string, 0, len(e.workspaces))
	for id := range e.workspaces {
		wsIDs = append(wsIDs, id)
	}
	sort.Strings(wsIDs)
	for _, id := range wsIDs {
		w := e.workspaces[id]
		ws := w.ws()
		if ws.State == preparing && (ws.Runtime == runtime.KindVM || ws.Runtime == runtime.KindCladefs) {
			// VM / cladefs-FUSE workspace preparation runs in a background
			// goroutine that died with the previous worker process.
			ws.State = failedWS
			ws.Reason = "interrupted by worker restart while preparing workspace"
			ws.LastActiveAt = e.nowMs()
			e.saveWorkspace(w, change{what: "failed", detail: ws.Reason})
			continue
		}
		if ws.State == released || ws.State == failedWS || !isSyncInProgress(ws.Reason) {
			continue
		}
		repo := syncInProgressRepo(ws.Reason)
		if cur := repoOf(ws, repo); cur != nil {
			cur.Tree = ""
			cur.Head = ""
			cur.FilesChanged = 0
		} else {
			for _, r := range ws.Repos {
				r.Tree = ""
				r.Head = ""
				r.FilesChanged = 0
			}
		}
		ws.State = failedWS
		if repo != "" {
			ws.Reason = fmt.Sprintf("interrupted during sync of %s; checkout state is unknown", repo)
		} else {
			ws.Reason = "interrupted during sync; checkout state is unknown"
		}
		ws.LastActiveAt = e.nowMs()
		e.saveWorkspace(w, change{what: "failed", detail: ws.Reason})
	}
	cmdIDs := make([]string, 0, len(e.commands))
	for id := range e.commands {
		cmdIDs = append(cmdIDs, id)
	}
	sort.Strings(cmdIDs)
	for _, id := range cmdIDs {
		c := e.commands[id]
		if c.cmd().State != queued {
			continue
		}
		if state, why, terminal := e.terminalWorkspaceForCommand(c); terminal {
			e.end(c, state, nil, why)
		}
	}
}

func (e *Engine) maxSyncBundleBytes() int64 {
	if e.cfg.MaxSyncBundleBytes > 0 {
		return e.cfg.MaxSyncBundleBytes
	}
	return DefaultMaxSyncBundleBytes
}

// activeSyncBlockersLocked returns the sorted IDs of non-server, non-shell
// commands currently running, held, paused, or mid-admit in workspace wsID.
// Call with e.mu held.
func (e *Engine) activeSyncBlockersLocked(wsID string) []string {
	var ids []string
	for _, c := range e.commands {
		cmd := c.cmd()
		if cmd.WorkspaceId != wsID || cmd.Kind == "server" || cmd.Kind == "shell" {
			continue
		}
		if started(cmd.State) || (cmd.State == queued && c.admitting) {
			ids = append(ids, cmd.Id)
		}
	}
	sort.Strings(ids)
	return ids
}

func syncConflictError(wsID, sessionID string, ids []string) *api.Error {
	label := "command " + ids[0]
	if len(ids) > 1 {
		label = "commands " + strings.Join(ids, ", ")
	}
	return (&api.Error{
		Code:      api.CodeConflict,
		Message:   fmt.Sprintf("cannot sync workspace %s while %s is active", wsID, label),
		Retryable: true,
	}).WithSuggestion("wait for it to finish, stop it (`" + sessionScopedCmd("stop", sessionID, ids[0]) + "`), or pass --no-sync")
}

// Sync checks out a snapshot of an agent's worktree, carried by git
// bundles (the repo's, and its submodules' where they changed), in the
// workspace's checkout of its repo. One sync per workspace
// runs at a time.
func (e *Engine) Sync(ctx context.Context, h *pb.SyncHeader, bundle io.Reader) (*pb.SyncResponse, error) {
	if err := validateID(h.WorkspaceId); err != nil {
		return nil, err
	}
	e.mu.Lock()
	w, ok := e.workspaces[h.WorkspaceId]
	if !ok || w.ws().State == released || w.releasing {
		e.mu.Unlock()
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", h.WorkspaceId)
	}
	e.recordUserActivityLocked(e.now())
	e.mu.Unlock()
	w.sync.Lock()
	defer w.sync.Unlock()
	e.mu.Lock()
	if w.ws().State == released || w.releasing {
		e.mu.Unlock()
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", h.WorkspaceId)
	}
	ws := clone(w.ws())
	e.mu.Unlock()
	switch ws.State {
	case preparing:
		return nil, &api.Error{Code: api.CodeConflict, Message: "workspace " + ws.Id + " is still preparing: " + ws.Reason, Retryable: true}
	case failedWS:
		return nil, api.Errorf(api.CodeWorkspaceFailed, "workspace %s failed: %s", ws.Id, ws.Reason)
	}
	r := repoOf(ws, h.Repo)
	if r == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "workspace %s of project %s has no repo %q", ws.Id, ws.Project, h.Repo).
			WithSuggestion("hotbed project ls shows each project's repos; hotbed repo add puts a repo in one")
	}
	maxBytes := e.maxSyncBundleBytes()
	if h.Tree != "" && h.Tree == r.Tree {
		src := bundle
		if maxBytes < math.MaxInt64 {
			src = io.LimitReader(bundle, maxBytes+1)
		}
		n, err := io.Copy(io.Discard, src)
		if err != nil {
			return nil, err
		}
		if n > maxBytes {
			return nil, api.Errorf(api.CodeInvalidArgs, "sync bundle exceeds maximum size of %d bytes", maxBytes)
		}
		return &pb.SyncResponse{WorkspaceId: ws.Id, Head: r.Head, Tree: r.Tree, FilesChanged: r.FilesChanged, Skipped: true}, nil
	}
	// Reject a sync whose tree differs while a non-server, non-shell command
	// is running, held, paused, or mid-admit in the workspace, so git
	// checkout --force cannot pull files out from under an active or frozen
	// build or test (board 7919, board 9782).
	e.mu.Lock()
	if blockers := e.activeSyncBlockersLocked(ws.Id); len(blockers) > 0 {
		e.mu.Unlock()
		return nil, syncConflictError(ws.Id, ws.SessionId, blockers)
	}
	e.mu.Unlock()
	// The stream holds each submodule's bundle, its size given, then the
	// repo's own to the end. Enforce the total byte cap across all bundles.
	var subs []gitops.SubCheckout
	var total int64
	for _, sm := range h.Submodules {
		if sm.BundleBytes < 0 {
			return nil, api.Errorf(api.CodeInvalidArgs, "submodule %s has negative bundle size %d", sm.Path, sm.BundleBytes)
		}
		if sm.BundleBytes > maxBytes-total {
			return nil, api.Errorf(api.CodeInvalidArgs, "sync bundle exceeds maximum size of %d bytes", maxBytes)
		}
		f, n, err := e.receiveBundle(ws.Id, io.LimitReader(bundle, sm.BundleBytes), sm.BundleBytes)
		if f != "" {
			defer os.Remove(f)
		}
		if err == nil && n != sm.BundleBytes {
			err = fmt.Errorf("stream ended %d bytes into submodule %s's %d", n, sm.Path, sm.BundleBytes)
		}
		if err != nil {
			return nil, err
		}
		total += n
		subs = append(subs, gitops.SubCheckout{Path: sm.Path, Commit: sm.Commit, Bundle: f})
	}
	f, n, err := e.receiveBundle(ws.Id, bundle, maxBytes-total)
	if f != "" {
		defer os.Remove(f)
	}
	if err != nil {
		return nil, err
	}
	total += n
	e.mu.Lock()
	if w.ws().State == released || w.releasing || w.rec.Volume == "" {
		e.mu.Unlock()
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", h.WorkspaceId)
	}
	if w.ws().State == failedWS {
		reason := w.ws().Reason
		e.mu.Unlock()
		return nil, api.Errorf(api.CodeWorkspaceFailed, "workspace %s failed: %s", h.WorkspaceId, reason)
	}
	dir := filepath.Join(e.workRoot(w.rec.Volume), r.Repo)
	volPath := e.volPath(w.rec.Volume)
	var prevTree, prevReason string
	if n > 0 {
		// Re-check under e.mu before setting sync-in-progress and mutating disk,
		// closing the race where a command was admitted while the bundle streamed.
		if blockers := e.activeSyncBlockersLocked(ws.Id); len(blockers) > 0 {
			e.mu.Unlock()
			return nil, syncConflictError(ws.Id, ws.SessionId, blockers)
		}
		// Persist the apply intent and invalidate the cached tree BEFORE
		// gitops.Apply can mutate disk so a crash or restart mid-apply never
		// leaves a stale healthy tree behind (board 8890).
		cur := repoOf(w.ws(), r.Repo)
		prevTree = cur.Tree
		prevReason = w.ws().Reason
		cur.Tree = ""
		w.ws().Reason = syncInProgressReason(r.Repo)
		w.ws().LastActiveAt = e.nowMs()
		dirtyBefore := e.dirtyGen
		e.saveWorkspace(w, change{what: "syncing", detail: r.Repo})
		if e.dirtyGen != dirtyBefore {
			cur.Tree = prevTree
			w.ws().Reason = prevReason
			e.mu.Unlock()
			return nil, api.Errorf(api.CodeInternal, "persist sync intent for %s/%s failed", ws.Id, r.Repo)
		}
	}
	e.mu.Unlock()
	commit, changed := h.Commit, 0
	if n > 0 {
		var c int
		var err error
		if e.usesRuntime(ws.Id) {
			c, err = e.applyViaRuntime(ctx, ws.Id, r.Repo, f, commit, r.BaseCommit, subs)
		} else {
			c, err = e.apply(ctx, volPath, dir, f, commit, r.BaseCommit, subs)
		}
		if err != nil {
			var applyErr *gitops.ApplyError
			if errors.As(err, &applyErr) && applyErr.Mutated {
				e.recordFailedSync(w, r.Repo, prevReason, applyErr)
			} else {
				e.restoreUnmutatedSync(w, r.Repo, prevTree, prevReason, err)
			}
			return nil, api.Errorf(api.CodeSyncRefused, "apply sync: %v", err)
		}
		changed = c
	} else if len(subs) > 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "sync carries submodules but no bundle of the repo")
	} else {
		// An empty bundle carries nothing beyond the base.
		commit = r.BaseCommit
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if w.ws().State == released {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", h.WorkspaceId)
	}
	cur := repoOf(w.ws(), r.Repo)
	cur.Head, cur.Tree, cur.FilesChanged, cur.SyncDir = commit, h.Tree, int32(changed), h.Dir
	if n > 0 {
		w.ws().Reason = prevReason
	}
	w.ws().LastActiveAt = e.nowMs()
	e.saveWorkspace(w, change{what: "synced", detail: fmt.Sprintf("%s: %d files changed", r.Repo, changed)})
	if n > 0 {
		e.stepSoon()
	}
	if w.releasing {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", h.WorkspaceId)
	}
	return &pb.SyncResponse{WorkspaceId: ws.Id, Head: commit, Tree: h.Tree, FilesChanged: int32(changed), BundleBytes: total}, nil
}

// restoreUnmutatedSync clears the persisted sync-in-progress marker and
// restores the previous cached tree when gitops.Apply failed before mutating
// the checkout on disk.
func (e *Engine) restoreUnmutatedSync(w *workspace, repo, prevTree, prevReason string, applyErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ws := w.ws()
	if ws.State == released || ws.State == failedWS {
		return
	}
	if cur := repoOf(ws, repo); cur != nil {
		cur.Tree = prevTree
	}
	ws.Reason = prevReason
	ws.LastActiveAt = e.nowMs()
	e.saveWorkspace(w, change{what: "sync refused", detail: fmt.Sprintf("%s: %v", repo, applyErr)})
	e.stepSoon()
}

// recordFailedSync updates workspace metadata after a gitops.Apply failure that
// mutated the checkout on disk. Even when rollback restored the previous
// checkout, keeping the cached tree cleared ensures the next sync for the old
// tree is never skipped. When rollback failed, the checkout is left
// inconsistent so the workspace is marked failed (blocking commands, shells,
// and further syncs until replaced).
func (e *Engine) recordFailedSync(w *workspace, repo, prevReason string, applyErr *gitops.ApplyError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ws := w.ws()
	if ws.State == released {
		return
	}
	if cur := repoOf(ws, repo); cur != nil {
		cur.Tree = ""
		if applyErr.Inconsistent() {
			cur.Head = ""
			cur.FilesChanged = 0
		}
	}
	ws.LastActiveAt = e.nowMs()
	if applyErr.Inconsistent() {
		ws.State = failedWS
		ws.Reason = fmt.Sprintf("sync of %s failed and left checkout inconsistent: %v", repo, applyErr)
		e.saveWorkspace(w, change{what: "failed", detail: ws.Reason})
		for _, c := range e.commands {
			if c.cmd().WorkspaceId == ws.Id && c.cmd().State == queued {
				e.end(c, failed, nil, "its workspace failed: "+ws.Reason)
			}
		}
		return
	}
	ws.Reason = prevReason
	e.saveWorkspace(w, change{what: "sync rolled back", detail: fmt.Sprintf("%s: %v", repo, applyErr)})
	e.stepSoon()
}

// receiveBundle writes a bundle from r to a file in the bundles directory,
// readable by what applies it (bundleOwner), and returns its path (to remove, even on
// error) and size. If r yields more than limit bytes, it removes the temp
// file and returns an InvalidArgs error.
func (e *Engine) receiveBundle(workspace string, r io.Reader, limit int64) (string, int64, error) {
	if err := validateID(workspace); err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp(filepath.Join(e.cfg.Dir, "bundles"), workspace+"-*.bundle")
	if err != nil {
		return "", 0, err
	}
	src := r
	if limit < math.MaxInt64 {
		src = io.LimitReader(r, limit+1)
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return f.Name(), n, fmt.Errorf("receive bundle: %w", err)
	}
	if n > limit {
		_ = os.Remove(f.Name())
		return "", n, api.Errorf(api.CodeInvalidArgs, "sync bundle exceeds maximum size of %d bytes", e.maxSyncBundleBytes())
	}
	if uid, gid, ok := e.bundleOwner(); ok {
		if err := os.Chown(f.Name(), uid, gid); err != nil {
			return f.Name(), n, err
		}
	}
	return f.Name(), n, nil
}

// bundleOwner is who a sync's bundle must be owned by on disk for what
// applies it to read it (it is 0600): the rootfs's user, or false when the
// worker's own uid already owns it.
func (e *Engine) bundleOwner() (uid, gid int, ok bool) {
	return e.bundleOwnerAs(os.Geteuid())
}

// bundleOwnerAs is bundleOwner for a worker running as euid.
func (e *Engine) bundleOwnerAs(euid int) (uid, gid int, ok bool) {
	if euid != 0 || e.cfg.User == "" {
		return 0, 0, false
	}
	spec := e.cfg.BaseImage.Spec.Normalized()
	return spec.UID, spec.GID, true
}

var errNotRegularFile = errors.New("not a regular file")

// openWorkRoot opens the volume's /work root anchored at the trusted volume
// path volPath via os.OpenRoot, verifying "work" is a real directory (not a
// symlink) before descending into it.
func openWorkRoot(volPath string) (workRoot *os.Root, workDir string, err error) {
	volRoot, err := os.OpenRoot(volPath)
	if err != nil {
		return nil, "", err
	}
	defer volRoot.Close()
	fi, err := volRoot.Lstat("work")
	if err != nil {
		return nil, "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, "", fmt.Errorf("work in %s is not a directory", volPath)
	}
	wRoot, err := volRoot.OpenRoot("work")
	if err != nil {
		return nil, "", err
	}
	dotFi, err := wRoot.Stat(".")
	if err != nil {
		wRoot.Close()
		return nil, "", err
	}
	if !os.SameFile(fi, dotFi) {
		wRoot.Close()
		return nil, "", fmt.Errorf("work in %s changed while opening", volPath)
	}
	return wRoot, filepath.Join(volPath, "work"), nil
}

// openRepoFromWorkRoot opens repo beneath an already-confined workRoot,
// verifying via Lstat + SameFile that repo is a real directory and not a symlink.
func openRepoFromWorkRoot(workRoot *os.Root, workDir, repo string) (*os.Root, string, error) {
	if repo == "" || repo == "." || repo == ".." || strings.ContainsAny(repo, `/\`) {
		return nil, "", fmt.Errorf("invalid repo name %q", repo)
	}
	fi, err := workRoot.Lstat(repo)
	if err != nil {
		return nil, "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, "", fmt.Errorf("repo %q is not a directory", repo)
	}
	repoRoot, err := workRoot.OpenRoot(repo)
	if err != nil {
		return nil, "", err
	}
	dotFi, err := repoRoot.Stat(".")
	if err != nil {
		repoRoot.Close()
		return nil, "", err
	}
	if !os.SameFile(fi, dotFi) {
		repoRoot.Close()
		return nil, "", fmt.Errorf("repo %q changed while opening", repo)
	}
	return repoRoot, filepath.Join(workDir, repo), nil
}

// openRepoRoot opens a repo checkout confined from the trusted volume root
// volPath through "work" and repo.
func openRepoRoot(volPath, repo string) (repoRoot *os.Root, repoDir string, err error) {
	workRoot, workDir, err := openWorkRoot(volPath)
	if err != nil {
		return nil, "", err
	}
	defer workRoot.Close()
	return openRepoFromWorkRoot(workRoot, workDir, repo)
}

// verifyCheckoutPath verifies that no component beneath volPath is a symlink
// and that dir still refers to the same directory opened as repoRoot before
// handing dir to a git subprocess.
func verifyCheckoutPath(volPath string, repoRoot *os.Root, dir string) error {
	rootFi, err := repoRoot.Stat(".")
	if err != nil {
		return err
	}
	workFi, err := os.Lstat(filepath.Join(volPath, "work"))
	if err != nil {
		return err
	}
	if workFi.Mode()&os.ModeSymlink != 0 || !workFi.IsDir() {
		return fmt.Errorf("workspace work directory is not a real directory")
	}
	dirFi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if dirFi.Mode()&os.ModeSymlink != 0 || !dirFi.IsDir() {
		return fmt.Errorf("checkout %q is not a real directory", dir)
	}
	if !os.SameFile(rootFi, dirFi) {
		return fmt.Errorf("checkout %q changed while opening", dir)
	}
	return nil
}

// openRegularFileNoBlock opens rel inside root without blocking on special
// files (FIFOs, sockets, devices): it checks Lstat/Stat first, opens with
// O_NONBLOCK, and re-verifies f.Stat().Mode().IsRegular().
func openRegularFileNoBlock(root *os.Root, rel string) (*os.File, error) {
	lfi, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		sfi, err := root.Stat(rel)
		if err != nil {
			return nil, err
		}
		if !sfi.Mode().IsRegular() {
			return nil, errNotRegularFile
		}
	} else if !lfi.Mode().IsRegular() {
		return nil, errNotRegularFile
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	ffi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ffi.Mode().IsRegular() {
		f.Close()
		return nil, errNotRegularFile
	}
	return f, nil
}

// Files lists every repo's tracked files, below /work (so each path starts
// with its repo), marked by how they differ from the workspace's base.
func (e *Engine) Files(ctx context.Context, id string) ([]*pb.FileEntry, error) {
	volPath, repos, err := e.checkouts(id)
	if err != nil {
		return nil, err
	}
	if e.usesRuntime(id) {
		return e.filesViaRuntime(ctx, id, repos)
	}
	workRoot, workDir, err := openWorkRoot(volPath)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	defer workRoot.Close()
	var out []*pb.FileEntry
	for _, r := range repos {
		repoRoot, dir, err := openRepoFromWorkRoot(workRoot, workDir, r.Repo)
		if err != nil {
			return nil, fmt.Errorf("open repo %s: %w", r.Repo, err)
		}
		files, err := e.repoFiles(ctx, volPath, repoRoot, dir, r.BaseCommit)
		repoRoot.Close()
		if err != nil {
			return nil, fmt.Errorf("list %s's files: %w", r.Repo, err)
		}
		out = append(out, &pb.FileEntry{Path: r.Repo, Kind: "dir"})
		for _, f := range files {
			f.Path = r.Repo + "/" + f.Path
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// repoFiles lists one checkout's tracked files, relative to it, marked by
// how they differ from base.
func (e *Engine) repoFiles(ctx context.Context, volPath string, repoRoot *os.Root, dir, base string) ([]*pb.FileEntry, error) {
	if err := verifyCheckoutPath(volPath, repoRoot, dir); err != nil {
		return nil, err
	}
	g, err := e.gitIn(volPath, dir)
	if err != nil {
		return nil, err
	}
	list, err := g.Run(ctx, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	status, err := g.Run(ctx, "diff", "--name-status", "-z", base)
	if err != nil {
		return nil, err
	}
	changes := map[string]string{}
	parts := strings.Split(status, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		switch parts[i] {
		case "A":
			changes[parts[i+1]] = "added"
		case "M":
			changes[parts[i+1]] = "modified"
		}
	}
	dirs := map[string]bool{}
	var out []*pb.FileEntry
	for _, p := range strings.Split(list, "\x00") {
		if p == "" {
			continue
		}
		var size int64
		if st, err := repoRoot.Lstat(p); err == nil {
			size = st.Size()
		}
		out = append(out, &pb.FileEntry{Path: p, Kind: "file", SizeBytes: size, Change: changes[p]})
		for d := filepath.Dir(p); d != "."; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		out = append(out, &pb.FileEntry{Path: d, Kind: "dir"})
	}
	return out, nil
}

const maxFileBytes = 1 << 20

// File reads one file of the workspace, by its path below /work, and the
// lines it changed against its base.
func (e *Engine) File(ctx context.Context, id, path string) (*pb.FileContent, error) {
	volPath, repos, err := e.checkouts(id)
	if err != nil {
		return nil, err
	}
	if e.usesRuntime(id) {
		return e.fileViaRuntime(ctx, id, path, repos)
	}
	clean := strings.TrimPrefix(filepath.Clean("/"+path), "/")
	name, rel, _ := strings.Cut(clean, "/")
	var r *pb.WorkspaceRepo
	for _, o := range repos {
		if o.Repo == name {
			r = o
		}
	}
	if r == nil || rel == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "path %q is in none of the workspace's repos", path).
			WithSuggestion("paths start with the repo, such as hotbed/go.mod; hotbed workspace files lists them")
	}
	repoRoot, dir, err := openRepoRoot(volPath, r.Repo)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, api.Errorf(api.CodeNotFound, "%s: %v", path, err)
		}
		return nil, api.Errorf(api.CodeInvalidArgs, "%s: %v", path, err)
	}
	defer repoRoot.Close()
	f, err := openRegularFileNoBlock(repoRoot, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, api.Errorf(api.CodeNotFound, "%s: %v", path, err)
		}
		if errors.Is(err, errNotRegularFile) {
			return nil, api.Errorf(api.CodeInvalidArgs, "%s is not a regular file", path)
		}
		return nil, api.Errorf(api.CodeInvalidArgs, "%s: %v", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return nil, err
	}
	if err := verifyCheckoutPath(volPath, repoRoot, dir); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "%s: %v", path, err)
	}
	g, err := e.gitIn(volPath, dir)
	if err != nil {
		return nil, err
	}
	diff, err := g.Run(ctx, "diff", "-U0", r.BaseCommit, "--", rel)
	if err != nil {
		return nil, err
	}
	return &pb.FileContent{Path: path, Text: string(data), ChangedLines: changedLines(diff, lineCount(data))}, nil
}

var hunk = regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)

// changedLines reads the new side's line numbers from a -U0 diff of a
// file of n lines: those in the file, once each, in order. The diff is the
// rootfs's git's, which the rootfs's root may have replaced (board 8333):
// a hunk claims no line the file does not have, and the work is the
// diff's and the file's size, whatever the hunks claim.
func changedLines(diff string, n int) []int32 {
	// starts[l] - ends[l]: the hunks open over line l, by prefix sums.
	delta := make([]int, n+2)
	// By newlines, not a bufio.Scanner: a line of a minified file is past
	// its limit, and every hunk after it would be lost.
	for line := range strings.SplitSeq(diff, "\n") {
		m := hunk.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		count := 1
		if m[2] != "" {
			if count, err = strconv.Atoi(m[2]); err != nil {
				continue
			}
		}
		lo, hi := max(start, 1), min(start+count-1, n)
		if count <= 0 || lo > hi {
			continue
		}
		delta[lo]++
		delta[hi+1]--
	}
	lines := []int32{}
	open := 0
	for l := 1; l <= n; l++ {
		open += delta[l]
		if open > 0 {
			lines = append(lines, int32(l))
		}
	}
	return lines
}

// lineCount is how many lines text has, a last one without a newline
// included.
func lineCount(text []byte) int {
	n := bytes.Count(text, []byte("\n"))
	if len(text) > 0 && text[len(text)-1] != '\n' {
		n++
	}
	return n
}

// checkouts is a live workspace's trusted volume path and its repos.
func (e *Engine) checkouts(id string) (string, []*pb.WorkspaceRepo, error) {
	if err := validateID(id); err != nil {
		return "", nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok || w.ws().State == released || w.releasing {
		return "", nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	return e.volPath(w.rec.Volume), clone(w.ws()).Repos, nil
}

// forgetReleased drops workspaces released a while ago, once their volume is
// gone: a record still naming one is how collection finds it. Call with e.mu
// held.
func (e *Engine) forgetReleased() {
	for id, w := range e.workspaces {
		if w.ws().State != released || w.rec.Volume != "" {
			continue
		}
		if time.Duration(e.nowMs()-w.ws().LastActiveAt)*time.Millisecond > keepReleased {
			delete(e.workspaces, id)
			e.deleteWorkspace(w)
		}
	}
}

// destroyVolume deletes a workspace's volume. It reports false, with no
// error, when the kernel still holds the volume busy (a mount namespace that
// has not let go of it yet): the workspace keeps naming the volume, and
// collectWorkspaces tries again. A workspace that never got its fork has no
// volume, which counts as freed.
func (e *Engine) destroyVolume(ctx context.Context, vol string) (bool, error) {
	err := e.cfg.Store.Destroy(ctx, vol)
	switch {
	case err == nil, errors.Is(err, storage.ErrNotFound):
		return true, nil
	case errors.Is(err, storage.ErrBusy):
		return false, nil
	default:
		return false, err
	}
}

// collectWorkspaces deletes the volumes released workspaces still name:
// those the kernel held busy on release.
func (e *Engine) collectWorkspaces(ctx context.Context) {
	e.mu.Lock()
	vols := map[string]string{}
	for id, w := range e.workspaces {
		if w.ws().State == released && w.rec.Volume != "" {
			vols[id] = w.rec.Volume
		}
	}
	e.mu.Unlock()
	for id, vol := range vols {
		freed, err := e.destroyVolume(ctx, vol)
		if err != nil {
			slog.Error("delete released workspace volume", "workspace", id, "volume", vol, "err", err)
			continue
		}
		if !freed {
			slog.Warn("released workspace volume still busy", "workspace", id, "volume", vol)
			continue
		}
		e.mu.Lock()
		if w, ok := e.workspaces[id]; ok && w.rec.Volume == vol {
			w.rec.Volume = ""
			e.saveWorkspace(w, change{what: "volume freed"})
		}
		e.mu.Unlock()
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
