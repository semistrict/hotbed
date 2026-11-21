package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/storage"
)

const checkRemoteEvery = time.Minute

func baseVolume(repo string, gen int) string { return fmt.Sprintf("bases/%s/g%d", repo, gen) }

func baseSnapshot(repo string, gen int) string { return fmt.Sprintf("%s-g%d", repo, gen) }

// maintainBases keeps a warm base of each hosted repo's branch: when the
// branch moves, it builds the next generation from the newest base, and it
// deletes old generations nothing uses any more.
func (e *Engine) maintainBases(ctx context.Context) {
	e.mu.Lock()
	repos := make([]api.Repo, 0, len(e.repos))
	for _, r := range e.repos {
		repos = append(repos, r)
	}
	e.mu.Unlock()
	for _, r := range repos {
		if err := e.maintainBase(ctx, r); err != nil {
			slog.Error("maintain base", "repo", r.ID, "err", err)
		}
	}
	e.collectBases(ctx)
}

func (e *Engine) maintainBase(ctx context.Context, r api.Repo) error {
	e.mu.Lock()
	var newest *api.Base
	for _, b := range e.bases {
		if b.RepoID != r.ID {
			continue
		}
		if b.State == api.BaseBuilding {
			e.mu.Unlock()
			return nil // one generation at a time
		}
		if newest == nil || b.Generation > newest.Generation {
			newest = b
		}
	}
	h, fresh := e.heads[r.ID], e.now().Sub(e.heads[r.ID].at) < checkRemoteEvery
	e.mu.Unlock()

	head := h.commit
	if !fresh {
		var err error
		if head, err = e.remoteHead(ctx, r); err != nil {
			return err
		}
		e.mu.Lock()
		e.heads[r.ID] = remoteHead{commit: head, at: e.now()}
		e.mu.Unlock()
	}
	if newest != nil && newest.Commit == head {
		return nil // up to date, or this commit already failed to build
	}
	return e.startFreshen(ctx, r, newest, head)
}

// remoteHead asks the remote for the commit the repo's branch is at.
func (e *Engine) remoteHead(ctx context.Context, r api.Repo) (string, error) {
	ref := "HEAD"
	if r.Branch != "" {
		ref = "refs/heads/" + r.Branch
	}
	out, err := e.git("").Run(ctx, "ls-remote", r.URL, ref)
	if err != nil {
		return "", err
	}
	commit, _, ok := strings.Cut(out, "\t")
	if !ok || len(commit) < 40 {
		return "", fmt.Errorf("%s has no %s", r.URL, ref)
	}
	return commit, nil
}

// startFreshen makes the next generation's volume, from the newest warm base
// when there is one, and queues the freshen that checks out commit and
// builds it.
func (e *Engine) startFreshen(ctx context.Context, r api.Repo, prev *api.Base, commit string) error {
	gen, cold := 1, true
	if prev != nil {
		gen = prev.Generation + 1
	}
	vol := baseVolume(r.ID, gen)
	if err := e.cfg.Store.Destroy(ctx, vol); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("clear leftover %s: %w", vol, err)
	}
	if prev != nil && prev.State == api.BaseWarm {
		if err := e.cfg.Store.Clone(ctx, baseSnapshot(r.ID, prev.Generation), vol); err != nil {
			return err
		}
		cold = false
	} else {
		if err := e.cfg.Store.Create(ctx, vol); err != nil {
			return err
		}
		if e.cred != nil {
			if err := os.Chown(e.cfg.Store.Path(vol), int(e.cred.Uid), int(e.cred.Gid)); err != nil {
				return err
			}
		}
	}
	id := fmt.Sprintf("f-%s-g%d-%s", r.ID, gen, randomHex(3))
	b := &api.Base{
		ID: baseSnapshot(r.ID, gen), RepoID: r.ID, NodeID: e.cfg.NodeID, Generation: gen,
		Commit: commit, State: api.BaseBuilding, Cold: cold, Baked: r.Freshen,
		Toolchain: "", Command: id,
	}
	c := &command{
		Command: api.Command{
			ID: id, NodeID: e.cfg.NodeID, Argv: "freshen " + r.ID + " g" + fmt.Sprint(gen),
			Args: []string{"/bin/bash", "-c", freshenScript(r, commit)}, Kind: "freshen",
			State: api.CommandQueued, QueuedAt: e.stamp(),
			Limits:   api.CommandLimits{CPUWeight: 100},
			Timeline: []api.CommandEvent{{At: e.stamp(), What: "queued", Detail: "base " + b.ID}},
		},
		RepoID: r.ID, Agent: "hotbed", Volume: vol, Dir: WorkPath, BaseID: b.ID, lastFollow: e.now(),
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bases[b.ID] = b
	e.saveBase(b)
	e.commands[id] = c
	e.saveCommand(c)
	from := "from nothing"
	if !cold {
		from = fmt.Sprintf("from g%d", prev.Generation)
	}
	e.event("base", fmt.Sprintf("base %s g%d building %s at %s", r.ID, gen, from, short(commit)), nil)
	return nil
}

// freshenScript checks out commit with its submodules and runs the repo's
// freshen commands, printing a marker before each so progress can be read
// from the log.
func freshenScript(r api.Repo, commit string) string {
	var b strings.Builder
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	fmt.Fprintf(&b, "set -euo pipefail\ncd %s\n", WorkPath)
	fmt.Fprintf(&b, "if [ ! -d %s/.git ]; then git clone --no-checkout %s %s; fi\n", q(r.ID), q(r.URL), q(r.ID))
	fmt.Fprintf(&b, "cd %s\n", q(r.ID))
	fmt.Fprintf(&b, "git fetch --quiet --force origin %s || git fetch --quiet --force origin\n", q(commit))
	fmt.Fprintf(&b, "git checkout --quiet --detach --force %s\n", q(commit))
	b.WriteString("git submodule update --quiet --init --recursive --force\n")
	// A workspace's own toolchain home starts empty; give rustup a default
	// when the repo does not pin one.
	b.WriteString(`if [ -f Cargo.toml ] && [ ! -f rust-toolchain.toml ] && [ ! -f rust-toolchain ] && command -v rustup >/dev/null; then rustup show active-toolchain >/dev/null 2>&1 || rustup default stable; fi` + "\n")
	for i, cmd := range r.Freshen {
		fmt.Fprintf(&b, "echo %s\n", q(fmt.Sprintf("::hotbed:: freshen %d/%d: %s", i+1, len(r.Freshen), cmd)))
		b.WriteString(cmd + "\n")
	}
	fmt.Fprintf(&b, "echo %s\n", q(fmt.Sprintf("::hotbed:: freshen %d/%d: done", len(r.Freshen), len(r.Freshen))))
	return b.String()
}

var freshenStep = regexp.MustCompile(`::hotbed:: freshen (\d+)/(\d+)`)

// freshened finishes a base whose freshen ended. Call with e.mu held.
func (e *Engine) freshened(c *command, ok bool, seconds float64, units int) {
	b, found := e.bases[c.BaseID]
	if !found {
		return
	}
	if !ok {
		b.State = api.BaseFailed
		e.saveBase(b)
		e.event("fail", fmt.Sprintf("base %s g%d failed to freshen; see command %s", b.RepoID, b.Generation, c.ID), &api.EventRef{Type: "command", ID: c.ID})
		return
	}
	vol := baseVolume(b.RepoID, b.Generation)
	// Snapshotting is quick but must not run under the lock's callers'
	// assumptions about time; it is one ioctl or one zfs command.
	if err := e.cfg.Store.Snapshot(context.Background(), vol, b.ID); err != nil {
		slog.Error("snapshot base", "base", b.ID, "err", err)
		b.State = api.BaseFailed
		e.saveBase(b)
		e.event("fail", fmt.Sprintf("base %s g%d could not be snapshotted: %v", b.RepoID, b.Generation, err), nil)
		return
	}
	s := e.stamp()
	b.State = api.BaseWarm
	b.BuiltAt = &s
	b.BuildSeconds = seconds
	b.Progress = 1
	if b.Cold {
		b.Units = units
	} else if prev := e.previousBase(b); prev != nil {
		b.Units = prev.Units
	}
	src := filepath.Join(e.cfg.Store.Path(vol), b.RepoID)
	if msg, err := e.git(src).Run(context.Background(), "log", "-1", "--format=%s"); err == nil {
		b.Message = msg
	}
	b.Toolchain = toolchain(src)
	for _, o := range e.bases {
		if o != b && o.RepoID == b.RepoID && o.State == api.BaseWarm {
			o.State = api.BaseStale
			e.saveBase(o)
		}
	}
	e.saveBase(b)
	e.event("base", fmt.Sprintf("base %s g%d warm · built in %s", b.RepoID, b.Generation, dur(seconds)), nil)
}

func (e *Engine) previousBase(b *api.Base) *api.Base {
	var prev *api.Base
	for _, o := range e.bases {
		if o.RepoID == b.RepoID && o.Generation < b.Generation && (prev == nil || o.Generation > prev.Generation) {
			prev = o
		}
	}
	return prev
}

// collectBases deletes generations that are stale or failed and that no
// live workspace was forked from.
func (e *Engine) collectBases(ctx context.Context) {
	e.mu.Lock()
	var dead []*api.Base
	for _, b := range e.bases {
		if b.State != api.BaseStale && b.State != api.BaseFailed {
			continue
		}
		used := false
		for _, w := range e.workspaces {
			if w.BaseID == b.ID && w.State != api.WorkspaceReleased {
				used = true
				break
			}
		}
		if !used {
			dead = append(dead, b)
		}
	}
	e.mu.Unlock()
	for _, b := range dead {
		vol := baseVolume(b.RepoID, b.Generation)
		if err := e.cfg.Store.Destroy(ctx, vol); err != nil && !errors.Is(err, storage.ErrNotFound) {
			slog.Error("delete base volume", "base", b.ID, "err", err)
			continue
		}
		if err := e.cfg.Store.DestroySnapshot(ctx, b.ID); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrInUse) {
			slog.Error("delete base snapshot", "base", b.ID, "err", err)
			continue
		}
		e.mu.Lock()
		delete(e.bases, b.ID)
		if err := e.db.delete("base", b.ID); err != nil {
			slog.Error("forget base", "base", b.ID, "err", err)
		}
		e.mu.Unlock()
	}
}

// baseProgress reads how far a building base's freshen got from its log.
// Call with e.mu held.
func (e *Engine) baseProgress(b *api.Base) float64 {
	data, err := os.ReadFile(e.logPath(b.Command))
	if err != nil {
		return 0
	}
	m := freshenStep.FindAllSubmatch(data, -1)
	if len(m) == 0 {
		return 0
	}
	last := m[len(m)-1]
	var done, total int
	fmt.Sscan(string(last[1]), &done)
	fmt.Sscan(string(last[2]), &total)
	if total == 0 {
		return 0
	}
	// Step k of n starting means k-1 are done.
	return float64(done-1) / float64(total)
}

// toolchain names what the checkout builds with, from its pin files.
func toolchain(dir string) string {
	for _, f := range []string{"rust-toolchain.toml", "rust-toolchain"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if m := regexp.MustCompile(`channel\s*=\s*"([^"]+)"`).FindSubmatch(b); m != nil {
				return "rust " + string(m[1])
			}
			return "rust " + strings.TrimSpace(string(b))
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		if m := regexp.MustCompile(`(?m)^go (\S+)`).FindSubmatch(b); m != nil {
			return "go " + string(m[1])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err == nil {
		return "rust stable"
	}
	return ""
}

func short(commit string) string {
	if len(commit) > 10 {
		return commit[:10]
	}
	return commit
}

// git runs git as the commands' user, in dir.
func (e *Engine) git(dir string) gitops.Git {
	return gitops.Git{Dir: dir, Cred: e.cred, Env: []string{"HOME=" + e.home, "GIT_TERMINAL_PROMPT=0"}}
}
