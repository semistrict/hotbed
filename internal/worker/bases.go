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

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/storage"
)

const (
	building = pb.BaseState_BASE_STATE_BUILDING
	warm     = pb.BaseState_BASE_STATE_WARM
	stale    = pb.BaseState_BASE_STATE_STALE
	broken   = pb.BaseState_BASE_STATE_FAILED
)

func baseVolume(repo string, gen int32) string { return fmt.Sprintf("bases/%s/g%d", repo, gen) }

func baseSnapshot(repo string, gen int32) string { return fmt.Sprintf("%s-g%d", repo, gen) }

// baseFor returns the base a new workspace of r forks: the newest warm base
// when it is at the branch's head, or the generation being built to bring
// it there, started now when none is. Bases are only ever brought up to date
// here, when a workspace asks: nothing builds in the background.
func (e *Engine) baseFor(ctx context.Context, r *pb.Repo) (*pb.Base, error) {
	head, err := e.remoteHead(ctx, r)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	var newest, newestWarm *pb.Base
	for _, b := range e.bases {
		if b.RepoId != r.Id {
			continue
		}
		if b.State == building && b.Commit == head && b.ScratchEpoch >= r.ScratchEpoch {
			e.mu.Unlock()
			return clone(b), nil // already on its way
		}
		if newest == nil || b.Generation > newest.Generation {
			newest = b
		}
		if b.State == warm && (newestWarm == nil || b.Generation > newestWarm.Generation) {
			newestWarm = b
		}
	}
	scratch := newestWarm != nil && r.ScratchEpoch > newestWarm.ScratchEpoch
	if !scratch && newestWarm != nil && newestWarm.Commit == head {
		defer e.mu.Unlock()
		return clone(newestWarm), nil
	}
	gen := int32(1)
	if newest != nil {
		gen = newest.Generation + 1
	}
	from := newestWarm
	if scratch {
		from = nil // asked to build from nothing
	}
	e.mu.Unlock()
	return e.startFreshen(ctx, r, gen, from, head)
}

// remoteHead asks the remote for the commit the repo's branch is at.
func (e *Engine) remoteHead(ctx context.Context, r *pb.Repo) (string, error) {
	ref := "HEAD"
	if r.Branch != "" {
		ref = "refs/heads/" + r.Branch
	}
	out, err := e.git("").Run(ctx, "ls-remote", r.Url, ref)
	if err != nil {
		return "", (&api.Error{Code: api.CodeRepoUnreachable, Message: fmt.Sprintf("cannot read %s's remote: %v", r.Id, err), Retryable: true}).
			WithSuggestion("check the repo's URL (hotbed repo ls) and that this node can reach it; retry once it can")
	}
	commit, _, ok := strings.Cut(out, "\t")
	if !ok || len(commit) < 40 {
		return "", fmt.Errorf("%s has no %s", r.Url, ref)
	}
	return commit, nil
}

// startFreshen makes generation gen's volume, as a fork of prev (the newest
// warm base) when there is one, and queues the freshen that pulls commit and
// builds it: incrementally, unless there is nothing to fork.
func (e *Engine) startFreshen(ctx context.Context, r *pb.Repo, gen int32, prev *pb.Base, commit string) (*pb.Base, error) {
	cold := true
	vol := baseVolume(r.Id, gen)
	if err := e.cfg.Store.Destroy(ctx, vol); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("clear leftover %s: %w", vol, err)
	}
	if prev != nil {
		if err := e.cfg.Store.Clone(ctx, prev.Id, vol); err != nil {
			return nil, err
		}
		cold = false
	} else {
		if err := e.cfg.Store.Create(ctx, vol); err != nil {
			return nil, err
		}
		if e.cred != nil {
			if err := os.Chown(e.cfg.Store.Path(vol), int(e.cred.Uid), int(e.cred.Gid)); err != nil {
				return nil, err
			}
		}
	}
	id := fmt.Sprintf("f-%s-g%d-%s", r.Id, gen, randomHex(3))
	now := e.nowMs()
	b := &pb.Base{
		Id: baseSnapshot(r.Id, gen), RepoId: r.Id, NodeId: e.cfg.NodeID, Generation: gen,
		Commit: commit, State: building, Cold: cold, Baked: r.Freshen, Command: id,
		ScratchEpoch: r.ScratchEpoch,
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
	e.changed()
	return clone(b), nil
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
		now := e.nowMs()
		b.State = broken
		b.BuiltAt = &now
		e.saveBase(b)
		e.event("fail", fmt.Sprintf("base %s g%d failed to freshen; see its log", b.RepoId, b.Generation), ref("command", c.cmd().Id))
		e.failPreparing(b.Id, fmt.Sprintf("its base %s failed to freshen; its log is command %s in the cockpit", b.Id, c.cmd().Id))
		return
	}
	vol := baseVolume(b.RepoId, b.Generation)
	if err := e.cfg.Store.Snapshot(context.Background(), vol, b.Id); err != nil {
		slog.Error("snapshot base", "base", b.Id, "err", err)
		b.State = broken
		e.saveBase(b)
		e.event("fail", fmt.Sprintf("base %s g%d could not be snapshotted: %v", b.RepoId, b.Generation, err), nil)
		e.failPreparing(b.Id, "its base could not be snapshotted: "+err.Error())
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
	if e.repos != nil && e.repos[b.RepoId] == nil {
		b.State = stale // its repo was removed while it freshened
	}
	e.saveBase(b)
	e.event("base", fmt.Sprintf("base %s g%d warm · built in %s", b.RepoId, b.Generation, dur(seconds)), nil)
	e.collectSoon()
	go e.forkPrepared(b.Id)
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
// live workspace was forked from. A repo's newest generation stays recorded
// even when it failed, so the same commit is not built again and again; only
// its volume goes. A removed repo's records go too.
func (e *Engine) collectBases(ctx context.Context) {
	e.mu.Lock()
	newest := map[string]int32{}
	for _, b := range e.bases {
		newest[b.RepoId] = max(newest[b.RepoId], b.Generation)
	}
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
		// e.repos is nil until the controller has said which repos exist.
		if b.Generation < newest[b.RepoId] || (e.repos != nil && e.repos[b.RepoId] == nil) {
			delete(e.bases, b.Id)
			if err := e.db.delete("base", b.Id); err != nil {
				slog.Error("forget base", "base", b.Id, "err", err)
			}
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
