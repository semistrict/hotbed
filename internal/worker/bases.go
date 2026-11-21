package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/storage"
)

const (
	checkRemoteEvery = time.Minute

	building = pb.BaseState_BASE_STATE_BUILDING
	warm     = pb.BaseState_BASE_STATE_WARM
	stale    = pb.BaseState_BASE_STATE_STALE
	broken   = pb.BaseState_BASE_STATE_FAILED
)

func baseVolume(repo string, gen int32) string { return fmt.Sprintf("bases/%s/g%d", repo, gen) }

func baseSnapshot(repo string, gen int32) string { return fmt.Sprintf("%s-g%d", repo, gen) }

// maintainBases keeps a warm base of each hosted repo's branch: when the
// branch moves, it builds the next generation from the newest base, and it
// deletes old generations nothing uses any more.
func (e *Engine) maintainBases(ctx context.Context) {
	e.mu.Lock()
	repos := make([]*pb.Repo, 0, len(e.repos))
	for _, r := range e.repos {
		repos = append(repos, r)
	}
	e.mu.Unlock()
	for _, r := range repos {
		if err := e.maintainBase(ctx, r); err != nil {
			slog.Error("maintain base", "repo", r.Id, "err", err)
		}
	}
	e.collectBases(ctx)
}

func (e *Engine) maintainBase(ctx context.Context, r *pb.Repo) error {
	e.mu.Lock()
	var newest *pb.Base
	for _, b := range e.bases {
		if b.RepoId != r.Id {
			continue
		}
		if b.State == building {
			e.mu.Unlock()
			return nil // one generation at a time
		}
		if newest == nil || b.Generation > newest.Generation {
			newest = b
		}
	}
	h := e.heads[r.Id]
	fresh := e.now().Sub(h.at) < checkRemoteEvery
	e.mu.Unlock()

	head := h.commit
	if !fresh {
		var err error
		if head, err = e.remoteHead(ctx, r); err != nil {
			return err
		}
		e.mu.Lock()
		e.heads[r.Id] = remoteHead{commit: head, at: e.now()}
		e.mu.Unlock()
	}
	if newest != nil && newest.Commit == head {
		return nil // up to date, or this commit already failed to build
	}
	return e.startFreshen(ctx, r, newest, head)
}

// remoteHead asks the remote for the commit the repo's branch is at.
func (e *Engine) remoteHead(ctx context.Context, r *pb.Repo) (string, error) {
	ref := "HEAD"
	if r.Branch != "" {
		ref = "refs/heads/" + r.Branch
	}
	out, err := e.git("").Run(ctx, "ls-remote", r.Url, ref)
	if err != nil {
		return "", err
	}
	commit, _, ok := strings.Cut(out, "\t")
	if !ok || len(commit) < 40 {
		return "", fmt.Errorf("%s has no %s", r.Url, ref)
	}
	return commit, nil
}

// startFreshen makes the next generation's volume, from the newest warm base
// when there is one, and queues the freshen that checks out commit and
// builds it.
func (e *Engine) startFreshen(ctx context.Context, r *pb.Repo, prev *pb.Base, commit string) error {
	gen, cold := int32(1), true
	if prev != nil {
		gen = prev.Generation + 1
	}
	vol := baseVolume(r.Id, gen)
	if err := e.cfg.Store.Destroy(ctx, vol); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("clear leftover %s: %w", vol, err)
	}
	if prev != nil && prev.State == warm {
		if err := e.cfg.Store.Clone(ctx, prev.Id, vol); err != nil {
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
	id := fmt.Sprintf("f-%s-g%d-%s", r.Id, gen, randomHex(3))
	now := e.nowMs()
	b := &pb.Base{
		Id: baseSnapshot(r.Id, gen), RepoId: r.Id, NodeId: e.cfg.NodeID, Generation: gen,
		Commit: commit, State: building, Cold: cold, Baked: r.Freshen, Command: id,
	}
	c := &command{
		rec: &pb.CommandRecord{
			Command: &pb.Command{
				Id: id, NodeId: e.cfg.NodeID, Argv: fmt.Sprintf("freshen %s g%d", r.Id, gen),
				Args: []string{"/bin/bash", "-c", freshenScript(r, commit)}, Kind: "freshen",
				State: queued, QueuedAt: now, Limits: &pb.CommandLimits{CpuWeight: 100},
				Timeline: []*pb.CommandEvent{{At: now, What: "queued", Detail: "base " + b.Id}},
			},
			RepoId: r.Id, Agent: "hotbed", Volume: vol, Dir: WorkPath, BaseId: b.Id,
		},
		lastFollow: e.now(),
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bases[b.Id] = b
	e.saveBase(b)
	e.commands[id] = c
	e.saveCommand(c)
	from := "from nothing"
	if !cold {
		from = fmt.Sprintf("from g%d", prev.Generation)
	}
	e.event("base", fmt.Sprintf("base %s g%d building %s at %s", r.Id, gen, from, short(commit)), ref("command", id))
	return nil
}

// freshenScript checks out commit with its submodules and runs the repo's
// freshen commands, printing a marker before each so progress can be read
// from the log.
func freshenScript(r *pb.Repo, commit string) string {
	var b strings.Builder
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	fmt.Fprintf(&b, "set -euo pipefail\ncd %s\n", WorkPath)
	fmt.Fprintf(&b, "if [ ! -d %s/.git ]; then git clone --no-checkout %s %s; fi\n", q(r.Id), q(r.Url), q(r.Id))
	fmt.Fprintf(&b, "cd %s\n", q(r.Id))
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

// freshened finishes a base whose freshen ended: a snapshot makes it warm.
// Call with e.mu held; snapshotting is one ioctl or one zfs command.
func (e *Engine) freshened(c *command, ok bool, seconds float64, units int) {
	b, found := e.bases[c.rec.BaseId]
	if !found {
		return
	}
	if !ok {
		b.State = broken
		e.saveBase(b)
		e.event("fail", fmt.Sprintf("base %s g%d failed to freshen; see its log", b.RepoId, b.Generation), ref("command", c.cmd().Id))
		return
	}
	vol := baseVolume(b.RepoId, b.Generation)
	if err := e.cfg.Store.Snapshot(context.Background(), vol, b.Id); err != nil {
		slog.Error("snapshot base", "base", b.Id, "err", err)
		b.State = broken
		e.saveBase(b)
		e.event("fail", fmt.Sprintf("base %s g%d could not be snapshotted: %v", b.RepoId, b.Generation, err), nil)
		return
	}
	now := e.nowMs()
	b.State = warm
	b.BuiltAt = &now
	b.BuildSeconds = seconds
	b.Progress = 1
	if b.Cold {
		b.Units = int32(units)
	} else if prev := e.previousBase(b); prev != nil {
		b.Units = prev.Units
	}
	src := filepath.Join(e.cfg.Store.Path(vol), b.RepoId)
	if msg, err := e.git(src).Run(context.Background(), "log", "-1", "--format=%s"); err == nil {
		b.Message = msg
	} else {
		slog.Warn("read base commit message", "base", b.Id, "err", err)
	}
	b.Toolchain = toolchain(src)
	for _, o := range e.bases {
		if o != b && o.RepoId == b.RepoId && o.State == warm {
			o.State = stale
			e.saveBase(o)
		}
	}
	e.saveBase(b)
	e.event("base", fmt.Sprintf("base %s g%d warm · built in %s", b.RepoId, b.Generation, dur(seconds)), nil)
}

func (e *Engine) previousBase(b *pb.Base) *pb.Base {
	var prev *pb.Base
	for _, o := range e.bases {
		if o.RepoId == b.RepoId && o.Generation < b.Generation && (prev == nil || o.Generation > prev.Generation) {
			prev = o
		}
	}
	return prev
}

// collectBases deletes generations that are stale or failed and that no
// live workspace was forked from.
func (e *Engine) collectBases(ctx context.Context) {
	e.mu.Lock()
	var dead []*pb.Base
	for _, b := range e.bases {
		if b.State != stale && b.State != broken {
			continue
		}
		used := false
		for _, w := range e.workspaces {
			if w.ws().BaseId == b.Id && w.ws().State != pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
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
		if err := e.cfg.Store.Destroy(ctx, baseVolume(b.RepoId, b.Generation)); err != nil && !errors.Is(err, storage.ErrNotFound) {
			slog.Error("delete base volume", "base", b.Id, "err", err)
			continue
		}
		if err := e.cfg.Store.DestroySnapshot(ctx, b.Id); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrInUse) {
			slog.Error("delete base snapshot", "base", b.Id, "err", err)
			continue
		}
		e.mu.Lock()
		delete(e.bases, b.Id)
		if err := e.db.delete("base", b.Id); err != nil {
			slog.Error("forget base", "base", b.Id, "err", err)
		}
		e.mu.Unlock()
	}
}

// baseProgress reads how far a building base's freshen got from its log.
func (e *Engine) baseProgress(b *pb.Base) float64 {
	data, err := os.ReadFile(e.logPath(b.Command))
	if err != nil {
		return 0
	}
	m := freshenStep.FindAllSubmatch(data, -1)
	if len(m) == 0 {
		return 0
	}
	last := m[len(m)-1]
	done, _ := strconv.Atoi(string(last[1]))
	total, _ := strconv.Atoi(string(last[2]))
	if total == 0 {
		return 0
	}
	// Step k of n starting means k-1 are done.
	return float64(done-1) / float64(total)
}

var (
	channel = regexp.MustCompile(`channel\s*=\s*"([^"]+)"`)
	goLine  = regexp.MustCompile(`(?m)^go (\S+)`)
)

// toolchain names what the checkout builds with, from its pin files.
func toolchain(dir string) string {
	for _, f := range []string{"rust-toolchain.toml", "rust-toolchain"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if m := channel.FindSubmatch(b); m != nil {
				return "rust " + string(m[1])
			}
			return "rust " + strings.TrimSpace(string(b))
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		if m := goLine.FindSubmatch(b); m != nil {
			return "go " + string(m[1])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err == nil {
		return "rust stable"
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		return "node"
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
