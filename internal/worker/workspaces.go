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
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/storage"
)

// CreateWorkspace forks the newest warm base of a repo.
func (e *Engine) CreateWorkspace(ctx context.Context, id string, req api.CreateWorkspace) (api.Workspace, error) {
	e.mu.Lock()
	if _, dup := e.workspaces[id]; dup {
		e.mu.Unlock()
		return api.Workspace{}, api.Errorf(api.CodeConflict, "workspace %s exists", id)
	}
	base := e.warmBase(req.Repo)
	e.mu.Unlock()
	if base == nil {
		return api.Workspace{}, (&api.Error{Code: api.CodeNoWarmBase, Message: fmt.Sprintf("no warm base of %s on this node yet", req.Repo), Retryable: true}).
			WithSuggestion("its first base is still building; hotbed workspaces show its progress")
	}
	vol := "ws/" + id
	start := e.now()
	if err := e.cfg.Store.Clone(ctx, base.ID, vol); err != nil {
		return api.Workspace{}, err
	}
	forkMs := e.now().Sub(start).Milliseconds()
	s := e.stamp()
	w := &workspace{
		Workspace: api.Workspace{
			ID: id, RepoID: req.Repo, BaseID: base.ID, NodeID: e.cfg.NodeID, Agent: req.Agent,
			Branch: req.Branch, Head: base.Commit, BaseCommit: base.Commit, State: api.WorkspaceIdle,
			CreatedAt: s, ForkMillis: forkMs, SharedBytes: base.SizeBytes, LastActiveAt: s,
			Path: WorkPath + "/" + req.Repo,
		},
		Volume: vol, BaseSnap: base.ID,
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workspaces[id] = w
	e.saveWorkspace(w)
	e.event("fork", fmt.Sprintf("fork %s ← %s@g%d in %d ms · %s", id, req.Repo, base.Generation, forkMs, req.Agent), &api.EventRef{Type: "workspace", ID: id})
	return w.Workspace, nil
}

// Workspace returns a workspace this node has.
func (e *Engine) Workspace(id string) (api.Workspace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok {
		return api.Workspace{}, api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	return w.Workspace, nil
}

// warmBase is the newest warm base of repo. Call with e.mu held.
func (e *Engine) warmBase(repo string) *api.Base {
	var best *api.Base
	for _, b := range e.bases {
		if b.RepoID == repo && b.State == api.BaseWarm && (best == nil || b.Generation > best.Generation) {
			best = b
		}
	}
	return best
}

// ReleaseWorkspace stops the workspace's commands and deletes its volume.
func (e *Engine) ReleaseWorkspace(ctx context.Context, id string) error {
	e.mu.Lock()
	w, ok := e.workspaces[id]
	if !ok || w.State == api.WorkspaceReleased {
		e.mu.Unlock()
		return api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	var live []string
	for _, c := range e.commands {
		if c.WorkspaceID == id && c.State.Live() {
			live = append(live, c.ID)
		}
	}
	e.mu.Unlock()
	for _, cid := range live {
		if err := e.Stop(ctx, cid, "workspace released"); err != nil {
			return err
		}
	}
	if err := e.cfg.Store.Destroy(ctx, w.Volume); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w.State = api.WorkspaceReleased
	w.LastActiveAt = e.stamp()
	e.saveWorkspace(w)
	e.event("release", fmt.Sprintf("release %s · %s · %s freed", id, w.Agent, gib(w.ExclusiveBytes)), &api.EventRef{Type: "workspace", ID: id})
	return nil
}

// Sync checks out a snapshot of an agent's worktree, carried by a git
// bundle, in the workspace. One sync per workspace runs at a time.
func (e *Engine) Sync(ctx context.Context, id string, bundle io.Reader, commit, tree string) (api.SyncResult, error) {
	e.mu.Lock()
	w, ok := e.workspaces[id]
	e.mu.Unlock()
	if !ok || w.State == api.WorkspaceReleased {
		return api.SyncResult{}, api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	w.sync.Lock()
	defer w.sync.Unlock()
	if tree != "" && tree == w.Tree {
		io.Copy(io.Discard, bundle)
		return api.SyncResult{Workspace: id, Head: w.Head, Tree: w.Tree, FilesChanged: w.FilesChanged, Skipped: true}, nil
	}
	f, err := os.CreateTemp(filepath.Join(e.cfg.Dir, "bundles"), id+"-*.bundle")
	if err != nil {
		return api.SyncResult{}, err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, bundle)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return api.SyncResult{}, fmt.Errorf("receive bundle: %w", err)
	}
	if e.cred != nil {
		if err := os.Chown(f.Name(), int(e.cred.Uid), int(e.cred.Gid)); err != nil {
			return api.SyncResult{}, err
		}
	}
	dir := filepath.Join(e.cfg.Store.Path(w.Volume), w.RepoID)
	changed := 0
	if n > 0 {
		changed, err = gitops.Apply(ctx, e.git(dir), f.Name(), commit, w.BaseCommit)
		if err != nil {
			return api.SyncResult{}, api.Errorf(api.CodeSyncRefused, "apply sync: %v", err)
		}
	} else if commit != w.BaseCommit {
		// An empty bundle carries nothing beyond the base: the agent's tree
		// is the base's.
		commit = w.BaseCommit
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w.Head, w.Tree, w.FilesChanged = commit, tree, changed
	w.LastActiveAt = e.stamp()
	e.saveWorkspace(w)
	e.changed()
	return api.SyncResult{Workspace: id, Head: commit, Tree: tree, FilesChanged: changed, BundleBytes: n}, nil
}

// Files lists the workspace's tracked files, marked by how they differ from
// its base.
func (e *Engine) Files(ctx context.Context, id string) ([]api.FileEntry, error) {
	w, dir, err := e.checkout(id)
	if err != nil {
		return nil, err
	}
	g := e.git(dir)
	list, err := g.Run(ctx, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	status, err := g.Run(ctx, "diff", "--name-status", "-z", w.BaseCommit)
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
	var out []api.FileEntry
	for _, p := range strings.Split(list, "\x00") {
		if p == "" {
			continue
		}
		var size int64
		if st, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			size = st.Size()
		}
		var change *string
		if c, ok := changes[p]; ok {
			change = &c
		}
		out = append(out, api.FileEntry{Path: p, Kind: "file", SizeBytes: size, Change: change})
		for d := filepath.Dir(p); d != "."; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		out = append(out, api.FileEntry{Path: d, Kind: "dir"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

const maxFileBytes = 1 << 20

// File reads one file of the workspace and the lines it changed against its base.
func (e *Engine) File(ctx context.Context, id, path string) (api.FileContent, error) {
	w, dir, err := e.checkout(id)
	if err != nil {
		return api.FileContent{}, err
	}
	full := filepath.Join(dir, filepath.Clean("/"+path))
	if !strings.HasPrefix(full, dir+string(filepath.Separator)) {
		return api.FileContent{}, api.Errorf(api.CodeInvalidArgs, "path %q leaves the workspace", path)
	}
	f, err := os.Open(full)
	if err != nil {
		return api.FileContent{}, api.Errorf(api.CodeNotFound, "%s: %v", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return api.FileContent{}, err
	}
	diff, err := e.git(dir).Run(ctx, "diff", "-U0", w.BaseCommit, "--", path)
	if err != nil {
		return api.FileContent{}, err
	}
	return api.FileContent{Path: path, Text: string(data), ChangedLines: changedLines(diff)}, nil
}

var hunk = regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)

// changedLines reads the new side's line numbers from a -U0 diff.
func changedLines(diff string) []int {
	lines := []int{}
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
			lines = append(lines, start+i)
		}
	}
	return lines
}

func (e *Engine) checkout(id string) (*workspace, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[id]
	if !ok || w.State == api.WorkspaceReleased {
		return nil, "", api.Errorf(api.CodeNotFound, "no workspace %s on this node", id)
	}
	return w, filepath.Join(e.cfg.Store.Path(w.Volume), w.RepoID), nil
}

// forgetReleased drops workspaces released a while ago. Call with e.mu held.
func (e *Engine) forgetReleased() {
	for id, w := range e.workspaces {
		if w.State != api.WorkspaceReleased {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, w.LastActiveAt); err == nil && e.now().Sub(t) > keepReleased {
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
