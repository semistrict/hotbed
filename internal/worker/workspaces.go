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

// CreateWorkspace forks the newest warm base of a repo.
func (e *Engine) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	if req.Id == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "workspace has no ID")
	}
	e.mu.Lock()
	if _, dup := e.workspaces[req.Id]; dup {
		e.mu.Unlock()
		return nil, api.Errorf(api.CodeConflict, "workspace %s exists", req.Id)
	}
	base := e.warmBase(req.Repo)
	e.mu.Unlock()
	if base == nil {
		return nil, (&api.Error{Code: api.CodeNoWarmBase, Message: fmt.Sprintf("no warm base of %s on this node yet", req.Repo), Retryable: true}).
			WithSuggestion("its first base is still building; the cockpit shows its progress")
	}
	vol := "ws/" + req.Id
	start := e.now()
	if err := e.cfg.Store.Clone(ctx, base.Id, vol); err != nil {
		return nil, err
	}
	forkMs := e.now().Sub(start).Milliseconds()
	now := e.nowMs()
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: req.Id, RepoId: req.Repo, BaseId: base.Id, NodeId: e.cfg.NodeID, Agent: req.Agent,
			Branch: req.Branch, Head: base.Commit, BaseCommit: base.Commit,
			State: pb.WorkspaceState_WORKSPACE_STATE_IDLE, CreatedAt: now, ForkMillis: forkMs,
			SharedBytes: base.SizeBytes, LastActiveAt: now, Path: WorkPath + "/" + req.Repo,
			SessionId: req.SessionId, SyncDir: req.SyncDir, SyncHost: req.SyncHost,
		},
		Volume: vol, BaseSnapshot: base.Id,
	}}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workspaces[req.Id] = w
	e.saveWorkspace(w)
	e.event("fork", fmt.Sprintf("fork %s ← %s@g%d in %d ms · %s", req.Id, req.Repo, base.Generation, forkMs, req.Agent), ref("workspace", req.Id))
	return clone(w.ws()), nil
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

// warmBase is the newest warm base of repo. Call with e.mu held.
func (e *Engine) warmBase(repo string) *pb.Base {
	var best *pb.Base
	for _, b := range e.bases {
		if b.RepoId == repo && b.State == warm && (best == nil || b.Generation > best.Generation) {
			best = b
		}
	}
	return best
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
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w.ws().State = released
	w.ws().LastActiveAt = e.nowMs()
	e.saveWorkspace(w)
	e.event("release", fmt.Sprintf("release %s · %s · %s freed", id, w.ws().Agent, gib(w.ws().ExclusiveBytes)), ref("workspace", id))
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
