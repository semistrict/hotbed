package worker

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/storage"
)

const released = pb.WorkspaceState_WORKSPACE_STATE_RELEASED

const (
	preparing = pb.WorkspaceState_WORKSPACE_STATE_PREPARING
	idle      = pb.WorkspaceState_WORKSPACE_STATE_IDLE
	failedWS  = pb.WorkspaceState_WORKSPACE_STATE_FAILED
	activeWS  = pb.WorkspaceState_WORKSPACE_STATE_ACTIVE
)

// CreateWorkspace gives a session a workspace of a repo at its branch's
// head: a fork of the newest base when that is at the head, or, while the
// base is brought up to date, a workspace that is preparing and becomes a
// fork of the new base when its freshen ends.
func (e *Engine) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	if req.Id == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "workspace has no ID")
	}
	r := req.RepoConfig
	if r == nil {
		e.mu.Lock()
		r = e.repos[req.Repo]
		e.mu.Unlock()
	}
	if r == nil {
		return nil, api.Errorf(api.CodeNotFound, "this node does not host %s", req.Repo)
	}
	e.mu.Lock()
	_, dup := e.workspaces[req.Id]
	e.mu.Unlock()
	if dup {
		return nil, api.Errorf(api.CodeConflict, "workspace %s exists", req.Id)
	}
	lock := e.repoLock(r.Id)
	lock.Lock()
	base, err := e.baseFor(ctx, r)
	lock.Unlock()
	if err != nil {
		return nil, err
	}
	now := e.nowMs()
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: req.Id, RepoId: r.Id, BaseId: base.Id, NodeId: e.cfg.NodeID, Agent: req.Agent,
			Branch: req.Branch, Head: base.Commit, BaseCommit: base.Commit, State: preparing,
			CreatedAt: now, LastActiveAt: now, Path: WorkPath + "/" + r.Id,
			SessionId: req.SessionId, SyncDir: req.SyncDir, SyncHost: req.SyncHost,
			Reason: fmt.Sprintf("waiting for base %s to freshen", base.Id),
		},
		Volume: "ws/" + req.Id, BaseSnapshot: base.Id,
	}}
	e.mu.Lock()
	e.workspaces[req.Id] = w
	e.saveWorkspace(w)
	// The base's freshen may have ended between baseFor and now, before
	// this workspace was there to be forked or failed with it.
	state := base.State
	if b, ok := e.bases[base.Id]; ok {
		state = b.State
	}
	if state == broken {
		e.failPreparing(base.Id, "its base "+base.Id+" failed to freshen")
	}
	e.mu.Unlock()
	if state == warm {
		if err := e.fork(ctx, w); err != nil {
			return nil, err
		}
	}
	return e.Workspace(req.Id)
}

// fork makes a preparing workspace's volume from its base's snapshot.
func (e *Engine) fork(ctx context.Context, w *workspace) error {
	e.mu.Lock()
	snap, vol := w.rec.BaseSnapshot, w.rec.Volume
	e.mu.Unlock()
	start := e.now()
	err := e.cfg.Store.Clone(ctx, snap, vol)
	forkMs := e.now().Sub(start).Milliseconds()
	e.mu.Lock()
	defer e.mu.Unlock()
	ws := w.ws()
	if err != nil {
		ws.State, ws.Reason = failedWS, "could not fork its base: "+err.Error()
		e.saveWorkspace(w)
		e.workspaceEvent(ws, "failed", ws.Reason)
		return err
	}
	ws.State, ws.Reason, ws.ForkMillis = idle, "", forkMs
	if b, ok := e.bases[snap]; ok {
		ws.SharedBytes = b.SizeBytes
	}
	ws.LastActiveAt = e.nowMs()
	e.saveWorkspace(w)
	e.workspaceEvent(ws, "ready", fmt.Sprintf("forked from %s in %d ms", snap, forkMs))
	e.event("fork", fmt.Sprintf("fork %s ← %s in %d ms · %s", ws.Id, snap, forkMs, ws.Agent), ref("workspace", ws.Id))
	return nil
}

// forkPrepared forks every workspace that waited for base.
func (e *Engine) forkPrepared(base string) {
	e.mu.Lock()
	var waiting []*workspace
	for _, w := range e.workspaces {
		if w.ws().State == preparing && w.ws().BaseId == base {
			waiting = append(waiting, w)
		}
	}
	e.mu.Unlock()
	for _, w := range waiting {
		if err := e.fork(context.Background(), w); err != nil {
			slog.Error("fork prepared workspace", "workspace", w.ws().Id, "err", err)
		}
	}
}

// failPreparing fails every workspace that waited for base. Call with e.mu held.
func (e *Engine) failPreparing(base, why string) {
	for _, w := range e.workspaces {
		ws := w.ws()
		if ws.State == preparing && ws.BaseId == base {
			ws.State, ws.Reason = failedWS, why
			e.saveWorkspace(w)
			e.workspaceEvent(ws, "failed", why)
			for _, c := range e.commands {
				if c.cmd().WorkspaceId == ws.Id && c.cmd().State == queued {
					e.end(c, failed, nil, "its workspace failed: "+why)
				}
			}
		}
	}
}

// Workspace returns a workspace this node has.
func (e *Engine) Workspace(id string) (*pb.Workspace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	return clone(w.ws()), nil
}

// WatchWorkspace returns the workspace now and a channel closed when it, or
// anything else of the engine's, next changes.
func (e *Engine) WatchWorkspace(id string) (*pb.Workspace, <-chan struct{}, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok {
		return nil, nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	return clone(w.ws()), e.updated, nil
}

// Workspaces lists the workspaces this node has, released ones included for a while.
func (e *Engine) Workspaces() []*pb.Workspace {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*pb.Workspace, 0, len(e.workspaces))
	for _, w := range e.workspaces {
		out = append(out, clone(w.ws()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// ReleaseWorkspace stops the workspace's commands and deletes its volume.
func (e *Engine) ReleaseWorkspace(ctx context.Context, id string) error {
	e.mu.Lock()
	w, ok := e.workspaces[id]
	if !ok || w.ws().State == released {
		e.mu.Unlock()
		return api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	var running []string
	for _, c := range e.commands {
		if c.cmd().WorkspaceId == id && live(c.cmd().State) {
			running = append(running, c.cmd().Id)
		}
	}
	e.mu.Unlock()
	for _, cid := range running {
		if _, err := e.Stop(ctx, cid, "workspace released"); err != nil {
			return err
		}
	}
	if err := e.cfg.Store.Destroy(ctx, w.rec.Volume); err != nil && !errors.Is(err, storage.ErrNotFound) {
		// A workspace that never got its fork has no volume.
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w.ws().State = released
	w.ws().LastActiveAt = e.nowMs()
	e.saveWorkspace(w)
	e.event("release", fmt.Sprintf("release %s · %s · %s freed", id, w.ws().Agent, gib(w.ws().ExclusiveBytes)), ref("workspace", id))
	e.collectSoon()
	return nil
}

// Sync checks out a snapshot of an agent's worktree, carried by a git
// bundle, in the workspace. One sync per workspace runs at a time.
func (e *Engine) Sync(ctx context.Context, h *pb.SyncHeader, bundle io.Reader) (*pb.SyncResponse, error) {
	e.mu.Lock()
	w, ok := e.workspaces[h.WorkspaceId]
	e.mu.Unlock()
	if !ok || w.ws().State == released {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", h.WorkspaceId)
	}
	w.sync.Lock()
	defer w.sync.Unlock()
	e.mu.Lock()
	ws := clone(w.ws())
	e.mu.Unlock()
	switch ws.State {
	case preparing:
		return nil, &api.Error{Code: api.CodeConflict, Message: "workspace " + ws.Id + " is still preparing: " + ws.Reason, Retryable: true}
	case failedWS:
		return nil, api.Errorf(api.CodeWorkspaceFailed, "workspace %s failed: %s", ws.Id, ws.Reason)
	}
	if h.Tree != "" && h.Tree == ws.Tree {
		if _, err := io.Copy(io.Discard, bundle); err != nil {
			return nil, err
		}
		return &pb.SyncResponse{WorkspaceId: ws.Id, Head: ws.Head, Tree: ws.Tree, FilesChanged: ws.FilesChanged, Skipped: true}, nil
	}
	f, err := os.CreateTemp(filepath.Join(e.cfg.Dir, "bundles"), ws.Id+"-*.bundle")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, bundle)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("receive bundle: %w", err)
	}
	if e.cred != nil {
		if err := os.Chown(f.Name(), int(e.cred.Uid), int(e.cred.Gid)); err != nil {
			return nil, err
		}
	}
	e.mu.Lock()
	dir := filepath.Join(e.cfg.Store.Path(w.rec.Volume), ws.RepoId)
	e.mu.Unlock()
	commit, changed := h.Commit, 0
	if n > 0 {
		c, err := gitops.Apply(ctx, e.git(dir), f.Name(), commit, ws.BaseCommit)
		if err != nil {
			return nil, api.Errorf(api.CodeSyncRefused, "apply sync: %v", err)
		}
		changed = c
	} else {
		// An empty bundle carries nothing beyond the base.
		commit = ws.BaseCommit
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := w.ws()
	cur.Head, cur.Tree, cur.FilesChanged = commit, h.Tree, int32(changed)
	cur.LastActiveAt = e.nowMs()
	e.saveWorkspace(w)
	e.changed()
	return &pb.SyncResponse{WorkspaceId: ws.Id, Head: commit, Tree: h.Tree, FilesChanged: int32(changed), BundleBytes: n}, nil
}

// Files lists the workspace's tracked files, marked by how they differ from
// its base.
func (e *Engine) Files(ctx context.Context, id string) ([]*pb.FileEntry, error) {
	base, dir, err := e.checkout(id)
	if err != nil {
		return nil, err
	}
	g := e.git(dir)
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
		if st, err := os.Lstat(filepath.Join(dir, p)); err == nil {
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
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

const maxFileBytes = 1 << 20

// File reads one file of the workspace and the lines it changed against its base.
func (e *Engine) File(ctx context.Context, id, path string) (*pb.FileContent, error) {
	base, dir, err := e.checkout(id)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(dir, filepath.Clean("/"+path))
	if !strings.HasPrefix(full, dir+string(filepath.Separator)) {
		return nil, api.Errorf(api.CodeInvalidArgs, "path %q leaves the workspace", path)
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, api.Errorf(api.CodeNotFound, "%s: %v", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return nil, err
	}
	diff, err := e.git(dir).Run(ctx, "diff", "-U0", base, "--", path)
	if err != nil {
		return nil, err
	}
	return &pb.FileContent{Path: path, Text: string(data), ChangedLines: changedLines(diff)}, nil
}

var hunk = regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)

// changedLines reads the new side's line numbers from a -U0 diff.
func changedLines(diff string) []int32 {
	lines := []int32{}
	sc := bufio.NewScanner(strings.NewReader(diff))
	for sc.Scan() {
		m := hunk.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		count := 1
		if m[2] != "" {
			count, _ = strconv.Atoi(m[2])
		}
		for i := range count {
			lines = append(lines, int32(start+i))
		}
	}
	return lines
}

// checkout is a live workspace's base commit and checkout directory.
func (e *Engine) checkout(id string) (string, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok || w.ws().State == released {
		return "", "", api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	return w.ws().BaseCommit, filepath.Join(e.cfg.Store.Path(w.rec.Volume), w.ws().RepoId), nil
}

// forgetReleased drops workspaces released a while ago. Call with e.mu held.
func (e *Engine) forgetReleased() {
	for id, w := range e.workspaces {
		if w.ws().State != released {
			continue
		}
		if time.Duration(e.nowMs()-w.ws().LastActiveAt)*time.Millisecond > keepReleased {
			delete(e.workspaces, id)
			if err := e.db.delete("workspace", id); err != nil {
				slog.Error("forget workspace", "workspace", id, "err", err)
			}
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
