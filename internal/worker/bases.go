package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"weak"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/baseimage"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/shell"
	"github.com/semistrict/hotbed/internal/storage"
)

// engineBaseImages tracks each base ID's parent shared base-image key in
// worker-owned state so GC and freshness checks do not need to trust
// project-controlled volume markers for bases created during this worker's
// lifetime. Keyed by weak.Pointer[Engine] with a GC cleanup so test engines
// are never retained.
var (
	engineBaseImagesMu sync.Mutex
	engineBaseImages   = map[weak.Pointer[Engine]]map[string]string{}
)

func (e *Engine) setBaseImage(baseID, key string) {
	if baseID == "" || key == "" {
		return
	}
	wp := weak.Make(e)
	engineBaseImagesMu.Lock()
	defer engineBaseImagesMu.Unlock()
	m := engineBaseImages[wp]
	if m == nil {
		m = map[string]string{}
		engineBaseImages[wp] = m
		runtime.AddCleanup(e, func(k weak.Pointer[Engine]) {
			engineBaseImagesMu.Lock()
			delete(engineBaseImages, k)
			engineBaseImagesMu.Unlock()
		}, wp)
	}
	m[baseID] = key
}

func (e *Engine) baseImageOf(baseID string) (string, bool) {
	wp := weak.Make(e)
	engineBaseImagesMu.Lock()
	defer engineBaseImagesMu.Unlock()
	m := engineBaseImages[wp]
	if m == nil {
		return "", false
	}
	k, ok := m[baseID]
	return k, ok && k != ""
}

func (e *Engine) forgetBaseImage(baseID string) {
	wp := weak.Make(e)
	engineBaseImagesMu.Lock()
	defer engineBaseImagesMu.Unlock()
	if m := engineBaseImages[wp]; m != nil {
		delete(m, baseID)
	}
}

const (
	building = pb.BaseState_BASE_STATE_BUILDING
	warm     = pb.BaseState_BASE_STATE_WARM
	stale    = pb.BaseState_BASE_STATE_STALE
	broken   = pb.BaseState_BASE_STATE_FAILED
)

func baseVolume(project string, gen int32) string { return fmt.Sprintf("bases/%s/g%d", project, gen) }

func baseSnapshot(project string, gen int32) string { return fmt.Sprintf("%s-g%d", project, gen) }

// baseFor returns the base a new workspace of p forks: the newest warm base
// when it has every repo at its branch's head and has run the project's
// steps as they are, or the generation being built to bring it there,
// started now when none is. Bases are only ever brought up to date here,
// when a workspace asks: nothing builds in the background.
//
// When the newest generation already failed for the same commits and
// steps, nothing is built again: the workspace gets the newest warm base,
// and note says why it is not up to date.
func (e *Engine) baseFor(ctx context.Context, p *pb.HostedProject) (b *pb.Base, note string, err error) {
	h, err := e.remoteHeads(ctx, p)
	if err != nil {
		return nil, "", err
	}
	baseImgKey := e.baseImageKey()
	want := recipe(p, h, baseImgKey)
	e.mu.Lock()
	var newest, newestWarm *pb.Base
	for _, b := range e.bases {
		if b.Project != p.Name {
			continue
		}
		if b.State == building && b.Recipe == want {
			out := clone(b)
			e.mu.Unlock()
			return out, "", nil // already on its way
		}
		if newest == nil || b.Generation > newest.Generation {
			newest = b
		}
		if b.State == warm && (newestWarm == nil || b.Generation > newestWarm.Generation) {
			newestWarm = b
		}
	}
	scratch := newestWarm != nil && p.ScratchEpoch > newestWarm.ScratchEpoch
	if !scratch && newestWarm != nil {
		if got, ok := e.baseImageOf(newestWarm.Id); ok {
			if got != baseImgKey {
				scratch = true
			}
		} else if !baseimage.MatchesKey(e.cfg.Store.Path(baseVolume(p.Name, newestWarm.Generation)), baseImgKey) {
			scratch = true
		}
	}
	if !scratch && h.at(newestWarm) && len(toRun(p, newestWarm, h)) == 0 {
		defer e.mu.Unlock()
		return clone(newestWarm), "", nil
	}
	if newest != nil && newest.State == broken && newest.Recipe == want {
		defer e.mu.Unlock()
		why := baseFailure(newest)
		if newestWarm == nil {
			return nil, "", api.Errorf(api.CodeWorkspaceFailed, "%s", why).
				WithSuggestion("fix the step with hotbed step add (hotbed step ls shows them), then run again")
		}
		return clone(newestWarm), fmt.Sprintf("on %s, not up to date: %s", newestWarm.Id, why), nil
	}
	gen := int32(1)
	if newest != nil {
		gen = newest.Generation + 1
	}
	var from *pb.Base
	if !scratch && newestWarm != nil {
		from = clone(newestWarm)
	}
	e.mu.Unlock()
	b, err = e.startFreshen(ctx, p, gen, from, h, want)
	return b, "", err
}

// baseFailure says why a broken base broke.
func baseFailure(b *pb.Base) string {
	if s := failedStep(b); s != "" {
		return fmt.Sprintf("base %s failed at step %s (its log is command %s)", b.Id, s, b.Command)
	}
	return fmt.Sprintf("base %s failed to freshen (its log is command %s)", b.Id, b.Command)
}

// remoteHeads asks each of the project's remotes for the commit its branch
// is at.
func (e *Engine) remoteHeads(ctx context.Context, p *pb.HostedProject) (heads, error) {
	h := heads{}
	for _, r := range p.Repos {
		commit, err := e.remoteHead(ctx, r)
		if err != nil {
			return nil, err
		}
		h[r.Id] = commit
	}
	return h, nil
}

func (e *Engine) validateRepoURL(raw string) error {
	if e.cfg.AllowLocalRepos {
		return gitops.ValidateRepoURLAllowLocal(raw)
	}
	return gitops.ValidateRepoURL(raw)
}

func validateBranchName(branch string) error {
	if branch == "" {
		return nil
	}
	if strings.HasPrefix(branch, "-") ||
		strings.HasPrefix(branch, "/") ||
		strings.HasSuffix(branch, "/") ||
		strings.HasSuffix(branch, ".") ||
		strings.HasSuffix(branch, ".lock") ||
		strings.Contains(branch, "..") ||
		strings.Contains(branch, "//") ||
		strings.Contains(branch, "@{") ||
		strings.ContainsAny(branch, `*?[]~^:\`) {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	for _, r := range branch {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("invalid branch name %q", branch)
		}
	}
	return nil
}

// parseLSRemote extracts the single exact commit SHA matching ref from
// `git ls-remote` output, rejecting missing, malformed, or ambiguous matches.
func parseLSRemote(out, url, ref string) (string, error) {
	var commit string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sha, gotRef, ok := strings.Cut(line, "\t")
		if !ok {
			return "", fmt.Errorf("unexpected ls-remote output from %s for %s", url, ref)
		}
		if gotRef != ref {
			continue
		}
		if gitops.ValidateSHA(sha) != nil {
			return "", fmt.Errorf("%s returned invalid commit %q for %s", url, sha, ref)
		}
		if commit != "" {
			return "", fmt.Errorf("%s returned ambiguous matches for %s", url, ref)
		}
		commit = sha
	}
	if commit == "" {
		return "", fmt.Errorf("%s has no %s", url, ref)
	}
	return commit, nil
}

// remoteHead asks the remote for the commit the repo's branch is at.
func (e *Engine) remoteHead(ctx context.Context, r *pb.Repo) (string, error) {
	if err := e.validateRepoURL(r.Url); err != nil {
		return "", api.Errorf(api.CodeInvalidArgs, "invalid repo URL for %s: %v", r.Id, err)
	}
	if err := validateBranchName(r.Branch); err != nil {
		return "", api.Errorf(api.CodeInvalidArgs, "invalid branch for %s: %v", r.Id, err)
	}
	ref := "HEAD"
	if r.Branch != "" {
		ref = "refs/heads/" + r.Branch
	}
	out, err := e.git("").Run(ctx, "ls-remote", "--", r.Url, ref)
	if err != nil {
		if reason, ok := extractGitAuthError(err); ok {
			return "", api.Errorf(api.CodeNotAuthorized, "cannot authenticate %s (%s): %s", r.Id, r.Url, reason).
				WithSuggestion("configure centralized git credentials on the controller: hotbed auth login (or gh auth token | hotbed auth set github --token-file -)")
		}
		if s := err.Error(); strings.Contains(s, "Authentication failed") || strings.Contains(s, "Invalid username or token") || strings.Contains(s, "Invalid username or password") {
			return "", api.Errorf(api.CodeNotAuthorized, "remote rejected git credentials for %s (%s): %v", r.Id, r.Url, err).
				WithSuggestion("update centralized git credentials on the controller with: hotbed auth login (or hotbed auth set)")
		}
		return "", (&api.Error{Code: api.CodeRepoUnreachable, Message: fmt.Sprintf("cannot read %s's remote: %v", r.Id, err), Retryable: true}).
			WithSuggestion("check the repo's URL (hotbed repo ls) and that this node can reach it; retry once it can")
	}
	return parseLSRemote(out, r.Url, ref)
}

// startFreshen makes generation gen's volume, as a fork of prev (the newest
// warm base) when there is one, and queues the freshen that checks out each
// repo at its head and builds it: incrementally, unless there is nothing to
// fork.
func (e *Engine) startFreshen(ctx context.Context, p *pb.HostedProject, gen int32, prev *pb.Base, h heads, recipe string) (*pb.Base, error) {
	cold := true
	vol := baseVolume(p.Name, gen)
	// No record names generation gen, but its snapshot and volume may be
	// there: left by a build that stopped midway, or by a database since
	// lost. Nothing uses them; they go first.
	snap := baseSnapshot(p.Name, gen)
	if err := e.cfg.Store.DestroySnapshot(ctx, snap); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("clear leftover snapshot %s (workspaces of a lost worker database may still be its clones): %w", snap, err)
	}
	if err := e.cfg.Store.Destroy(ctx, vol); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("clear leftover %s: %w", vol, err)
	}
	var baseImgKey string
	if prev != nil {
		if err := e.cfg.Store.Clone(ctx, prev.Id, vol); err != nil {
			return nil, err
		}
		cold = false
		if k, ok := e.baseImageOf(prev.Id); ok {
			baseImgKey = k
		} else if k, ok := readBaseImageMarkerSafe(e.cfg.Store.Path(baseVolume(prev.Project, prev.Generation))); ok {
			baseImgKey = k
		}
	} else {
		baseSnap, _, err := e.cfg.BaseImage.Ensure(ctx)
		if err != nil {
			return nil, fmt.Errorf("ensure shared base image: %w", err)
		}
		if err := e.cfg.Store.Clone(ctx, baseSnap, vol); err != nil {
			return nil, fmt.Errorf("clone base image %s into %s: %w", baseSnap, vol, err)
		}
		baseImgKey = baseSnap
	}
	id := fmt.Sprintf("f-%s-g%d-%s", p.Name, gen, randomHex(3))
	now := e.nowMs()
	run := toRun(p, prev, h)
	b := &pb.Base{
		Id: snap, Project: p.Name, NodeId: e.cfg.NodeID, Generation: gen,
		State: building, Cold: cold, Command: id,
		ScratchEpoch: p.ScratchEpoch, Steps: plannedSteps(p, prev, run), Recipe: recipe,
	}
	for _, r := range p.Repos {
		b.Repos = append(b.Repos, &pb.BaseRepo{Repo: r.Id, Commit: h[r.Id]})
	}
	c := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id: id, NodeId: e.cfg.NodeID, Argv: fmt.Sprintf("freshen %s g%d", p.Name, gen), Project: p.Name,
			Args: []string{"/bin/bash", "-c", freshenScript(p, h, prev, run)}, Kind: "freshen",
			State: queued, QueuedAt: now, Limits: &pb.CommandLimits{CpuWeight: 100},
			Timeline: []*pb.CommandEvent{{At: now, What: "queued", Detail: "base " + b.Id}},
		},
		Project: p.Name, Agent: "hotbed", Volume: vol, Dir: WorkPath, BaseId: b.Id,
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	c.cmd().Limits.MemoryLowBytes = e.freshenGuarantee(p.Name)
	from := "from nothing"
	if !cold {
		from = fmt.Sprintf("from g%d", prev.Generation)
	}
	if baseImgKey != "" {
		e.setBaseImage(b.Id, baseImgKey)
	}
	e.bases[b.Id] = b
	e.saveBase(b, change{
		what: "building", detail: from,
		feed: feed(feedBase, fmt.Sprintf("base %s g%d building %s at %s", p.Name, gen, from, describeHeads(b.Repos)), ref("command", id)),
	})
	e.commands[id] = c
	e.saveCommand(c, change{what: "queued", detail: "base " + b.Id})
	e.dropSuperseded(p.Name)
	e.stepSoon()
	return clone(b), nil
}

// freshenGuarantee is the memory a freshen of project is guaranteed: the
// most an earlier one on this node held, so the node admits no more work
// beside it than fits, from its first step on, before it has held any
// itself. It is at most what reservations may take, so it can always
// start once the node drains. Call with e.mu held.
func (e *Engine) freshenGuarantee(project string) int64 {
	var peak int64
	for _, b := range e.bases {
		if b.Project == project {
			peak = max(peak, b.PeakMemoryBytes)
		}
	}
	if lim := e.admitLimit(); lim > 0 {
		peak = min(peak, lim)
	}
	return peak
}

// describeHeads names each repo's commit, shortened: hotbed 1a2b3c4d5e.
func describeHeads(repos []*pb.BaseRepo) string {
	if len(repos) == 0 {
		return "no repos"
	}
	parts := make([]string, len(repos))
	for i, r := range repos {
		parts[i] = r.Repo + " " + short(r.Commit)
	}
	return strings.Join(parts, ", ")
}

// freshenScript checks out each of the project's repos at its head, with
// its submodules, in /work/<repo>, removes the checkouts of repos the
// project no longer holds, and runs the steps the generation needs in
// /work.
func freshenScript(p *pb.HostedProject, h heads, prev *pb.Base, run []*pb.Step) string {
	var b strings.Builder
	q := shell.Quote
	fmt.Fprintf(&b, "set -euo pipefail\n")
	held := map[string]bool{}
	for _, r := range p.Repos {
		held[r.Id] = true
	}
	for _, r := range prev.GetRepos() {
		if !held[r.Repo] {
			fmt.Fprintf(&b, "rm -rf %s\n", q(WorkPath+"/"+r.Repo))
		}
	}
	for _, r := range p.Repos {
		commit := h[r.Id]
		fmt.Fprintf(&b, "cd %s\n", WorkPath)
		// Blobless: a base needs the files of the commits it checks out, not every version of
		// every file (cladefs's history is 2.5 GiB of packs); git fetches others when asked for.
		fmt.Fprintf(&b, "if [ ! -d %s/.git ]; then git clone --quiet --filter=blob:none --no-checkout -- %s %s; fi\n", q(r.Id), q(r.Url), q(r.Id))
		fmt.Fprintf(&b, "cd %s\n", q(r.Id))
		fmt.Fprintf(&b, "git fetch --quiet --force origin %s || git fetch --quiet --force origin\n", q(commit))
		fmt.Fprintf(&b, "git checkout --quiet --detach --force %s\n", q(commit))
		b.WriteString("git submodule update --quiet --init --recursive --force\n")
		// A workspace's own toolchain home starts empty; give rustup a default
		// when the repo does not pin one.
		b.WriteString(`if [ -f Cargo.toml ] && [ ! -f rust-toolchain.toml ] && [ ! -f rust-toolchain ] && command -v rustup >/dev/null; then rustup show active-toolchain >/dev/null 2>&1 || rustup default stable; fi` + "\n")
	}
	fmt.Fprintf(&b, "cd %s\n", WorkPath)
	b.WriteString(stepsScript(run))
	return b.String()
}

// freshened finishes a base whose freshen ended: a snapshot makes it warm.
// Call without e.mu held; Store.Snapshot, git log, and toolchain inspection
// run outside e.mu while b.State stays building so worker RPCs do not stall
// and no CreateWorkspace can race cloning an unfinished snapshot.
func (e *Engine) freshened(ctx context.Context, c *command, ok bool, seconds float64, units int) {
	e.mu.Lock()
	b, found := e.bases[c.rec.BaseId]
	if !found || b.State != building || e.finalizingBases[b.Id] {
		e.mu.Unlock()
		return // dropped as superseded while it built, or already finalizing
	}
	b.PeakMemoryBytes = c.rec.PeakHeldBytes
	b.Steps = c.out.settle(b.Steps)
	b.Baked = nil
	for _, s := range b.Steps {
		if s.Ok {
			b.Baked = append(b.Baked, s.Name)
		}
	}
	if !ok {
		now := e.nowMs()
		b.State = broken
		b.BuiltAt = &now
		why := baseFailure(b)
		e.saveBase(b, change{what: "failed", detail: why, feed: feed(feedFail, why, ref("command", c.cmd().Id))})
		e.baseFailed(b, why)
		e.mu.Unlock()
		return
	}
	baseID := b.Id
	vol := baseVolume(b.Project, b.Generation)
	volPath := e.cfg.Store.Path(vol)
	repoNames := make([]string, len(b.Repos))
	for i, r := range b.Repos {
		repoNames[i] = r.Repo
	}
	e.finalizingBases[baseID] = true
	e.mu.Unlock()

	snapErr := e.cfg.Store.Snapshot(ctx, vol, baseID)
	type repoMetadata struct {
		message   string
		toolchain string
	}
	var repoMeta map[string]repoMetadata
	if snapErr == nil {
		repoMeta = make(map[string]repoMetadata, len(repoNames))
		for _, repo := range repoNames {
			repoRoot, src, err := openRepoRoot(volPath, repo)
			if err != nil {
				slog.Warn("open base repo", "base", baseID, "repo", repo, "err", err)
				continue
			}
			var meta repoMetadata
			if err := verifyCheckoutPath(volPath, repoRoot, src); err != nil {
				slog.Warn("verify base repo path", "base", baseID, "repo", repo, "err", err)
			} else if g, err := e.gitIn(volPath, src); err != nil {
				slog.Warn("read base commit message", "base", baseID, "repo", repo, "err", err)
			} else if msg, err := runWithin(ctx, gitDeadline, func(ctx context.Context) (string, error) {
				return g.Run(ctx, "log", "-1", "--format=%s")
			}); err == nil {
				meta.message = msg
			} else if ctx.Err() == nil {
				slog.Warn("read base commit message", "base", baseID, "repo", repo, "err", err)
			}
			meta.toolchain = toolchainFromRoot(repoRoot)
			repoRoot.Close()
			repoMeta[repo] = meta
		}
	}

	e.mu.Lock()
	delete(e.finalizingBases, baseID)
	b, found = e.bases[baseID]
	if !found || b.State != building {
		e.mu.Unlock()
		if !found && snapErr == nil {
			if err := e.cfg.Store.DestroySnapshot(ctx, baseID); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrInUse) && ctx.Err() == nil {
				slog.Warn("delete orphaned base snapshot", "base", baseID, "err", err)
			}
		} else if found {
			e.collectSoon()
		}
		return
	}
	if snapErr != nil {
		if e.closing && errors.Is(snapErr, context.Canceled) {
			e.mu.Unlock()
			return
		}
		slog.Error("snapshot base", "base", b.Id, "err", snapErr)
		b.State = broken
		why := fmt.Sprintf("base %s could not be snapshotted: %v", b.Id, snapErr)
		e.saveBase(b, change{what: "failed", detail: why, feed: feed(feedFail, why, nil)})
		e.baseFailed(b, why)
		e.mu.Unlock()
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
	for _, r := range b.Repos {
		if m, ok := repoMeta[r.Repo]; ok {
			r.Message = m.message
			r.Toolchain = m.toolchain
		}
	}
	// It takes over from older generations; one that finished behind a
	// newer warm one is stale itself.
	for _, o := range e.bases {
		if o == b || o.Project != b.Project || o.State != warm {
			continue
		}
		if o.Generation < b.Generation {
			o.State = stale
			e.saveBase(o, change{what: "stale", detail: "superseded by " + b.Id})
		} else {
			b.State = stale
		}
	}
	if e.projectRemoved(b.Project) {
		b.State = stale // the node stopped keeping its project while it freshened
	}
	what := "warm"
	if b.State == stale {
		what = "stale"
	}
	e.saveBase(b, change{what: what, feed: feed(feedBase, fmt.Sprintf("base %s g%d warm · built in %s", b.Project, b.Generation, dur(seconds)), nil)})
	e.collectSoon()
	e.startBgLocked(func(ctx context.Context) {
		e.forkPrepared(ctx, baseID)
	})
	e.mu.Unlock()
}

func (e *Engine) previousBase(b *pb.Base) *pb.Base {
	var prev *pb.Base
	for _, o := range e.bases {
		if o.Project == b.Project && o.Generation < b.Generation && (prev == nil || o.Generation > prev.Generation) {
			prev = o
		}
	}
	return prev
}

// collectBases deletes generations that are stale or failed and that no
// live workspace was forked from. A project's newest generation stays
// recorded even when it failed, so the same commits are not built again and
// again; only its volume goes. The records of a project the node no longer
// keeps go too.
func (e *Engine) collectBases(ctx context.Context) {
	e.mu.Lock()
	newest := map[string]int32{}
	for _, b := range e.bases {
		newest[b.Project] = max(newest[b.Project], b.Generation)
	}
	var dead []*pb.Base
	for _, b := range e.bases {
		if (b.State != stale && b.State != broken) || e.finalizingBases[b.Id] {
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
		if err := e.cfg.Store.Destroy(ctx, baseVolume(b.Project, b.Generation)); err != nil && !errors.Is(err, storage.ErrNotFound) {
			slog.Error("delete base volume", "base", b.Id, "err", err)
			continue
		}
		if err := e.cfg.Store.DestroySnapshot(ctx, b.Id); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrInUse) {
			slog.Error("delete base snapshot", "base", b.Id, "err", err)
			continue
		}
		e.mu.Lock()
		if b.Generation < newest[b.Project] || e.projectRemoved(b.Project) {
			delete(e.bases, b.Id)
			e.forgetBaseImage(b.Id)
			e.deleteBase(b, "collected")
		}
		e.mu.Unlock()
	}
}

const maxBaseImageMarkerBytes = 256

// readBaseImageMarkerSafe reads the shared base-image key from
// <volPath>/.hotbed-baseimage-ready without following symlinks, without
// blocking on FIFOs or device nodes, and with a strict size cap.
func readBaseImageMarkerSafe(volPath string) (string, bool) {
	r, err := os.OpenRoot(volPath)
	if err != nil {
		return "", false
	}
	defer r.Close()

	lfi, err := r.Lstat(baseimage.ReadyMarker)
	if err != nil || !lfi.Mode().IsRegular() {
		return "", false
	}
	f, err := r.OpenFile(baseimage.ReadyMarker, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer f.Close()

	ffi, err := f.Stat()
	if err != nil || !ffi.Mode().IsRegular() || !os.SameFile(lfi, ffi) {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBaseImageMarkerBytes+1))
	if err != nil || len(b) > maxBaseImageMarkerBytes {
		return "", false
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", false
	}
	return key, true
}

// collectBaseImages removes superseded shared base-image volumes and snapshots
// under baseimages/ that are neither the worker's current base image nor still
// referenced by any warm, building, or finalizing project base. Snapshots that
// still have storage clones (e.g. on ZFS) are skipped until those clones go.
func (e *Engine) collectBaseImages(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	entries, err := os.ReadDir(e.cfg.Store.Path("baseimages"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("list base images", "err", err)
		}
		return
	}
	if len(entries) == 0 {
		return
	}

	live := map[string]bool{}
	if cur := e.baseImageKey(); cur != "" {
		live[cur] = true
	}

	type liveBase struct {
		id      string
		volPath string
	}
	e.mu.Lock()
	var liveBases []liveBase
	for _, b := range e.bases {
		if b.State == warm || b.State == building || e.finalizingBases[b.Id] {
			liveBases = append(liveBases, liveBase{
				id:      b.Id,
				volPath: e.cfg.Store.Path(baseVolume(b.Project, b.Generation)),
			})
		}
	}
	e.mu.Unlock()

	for _, lb := range liveBases {
		if k, ok := e.baseImageOf(lb.id); ok {
			live[k] = true
			continue
		}
		k, ok := readBaseImageMarkerSafe(lb.volPath)
		if !ok {
			slog.Warn("skip base-image gc: cannot safely determine parent base image for live base",
				"base", lb.id, "volume", lb.volPath)
			return
		}
		live[k] = true
	}

	for _, ent := range entries {
		if ctx.Err() != nil {
			return
		}
		key := ent.Name()
		if !ent.IsDir() || !strings.HasPrefix(key, "baseimg-") {
			continue
		}
		if live[key] {
			continue
		}
		if key == e.baseImageKey() {
			continue
		}
		// Destroy the snapshot before the backing volume so ZFS never promotes
		// a dependent clone and orphans the snapshot; if clones still depend on
		// it, ErrInUse skips both until those clones are collected.
		if err := e.cfg.Store.DestroySnapshot(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			if errors.Is(err, storage.ErrInUse) || errors.Is(err, storage.ErrBusy) {
				slog.Debug("skip superseded base image snapshot still in use", "key", key, "err", err)
			} else {
				slog.Info("skip superseded base image snapshot", "key", key, "err", err)
			}
			continue
		}
		if err := e.cfg.Store.Destroy(ctx, "baseimages/"+key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			if errors.Is(err, storage.ErrInUse) || errors.Is(err, storage.ErrBusy) {
				slog.Debug("skip superseded base image volume still in use", "key", key, "err", err)
			} else {
				slog.Info("skip superseded base image volume", "key", key, "err", err)
			}
			continue
		}
		slog.Info("collected superseded base image", "key", key)
	}
}

// baseProgress reads how far a building base's freshen got from its log.
// Call with e.mu held.
func (e *Engine) baseProgress(b *pb.Base) float64 {
	c, ok := e.commands[b.Command]
	if !ok {
		return 0
	}
	return c.out.progress()
}

var (
	channel = regexp.MustCompile(`channel\s*=\s*"([^"]+)"`)
	goLine  = regexp.MustCompile(`(?m)^go (\S+)`)
)

const maxToolchainBytes = 64 << 10

// toolchain names what the checkout builds with, from its pin files, anchored
// at the trusted volume root volPath so symlinked work/ or repo dirs are rejected.
func toolchain(volPath, repo string) string {
	root, _, err := openRepoRoot(volPath, repo)
	if err != nil {
		return ""
	}
	defer root.Close()
	return toolchainFromRoot(root)
}

func readSmallRegularFile(root *os.Root, rel string) ([]byte, error) {
	f, err := openRegularFileNoBlock(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxToolchainBytes))
}

func toolchainFromRoot(root *os.Root) string {
	for _, f := range []string{"rust-toolchain.toml", "rust-toolchain"} {
		if b, err := readSmallRegularFile(root, f); err == nil {
			if m := channel.FindSubmatch(b); m != nil {
				return "rust " + string(m[1])
			}
			return "rust " + strings.TrimSpace(string(b))
		}
	}
	if b, err := readSmallRegularFile(root, "go.mod"); err == nil {
		if m := goLine.FindSubmatch(b); m != nil {
			return "go " + string(m[1])
		}
	}
	if fi, err := root.Stat("Cargo.toml"); err == nil && fi.Mode().IsRegular() {
		return "rust stable"
	}
	if fi, err := root.Stat("package.json"); err == nil && fi.Mode().IsRegular() {
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
	env := []string{"HOME=" + e.home, "GIT_TERMINAL_PROMPT=0"}
	if e.gitAuth != nil {
		env = append(env, e.gitAuth.hostEnv()...)
	}
	return gitops.Git{Dir: dir, Cred: e.cred, Env: env}
}

// dropSuperseded stops the base builds of a project that no workspace waits
// for any more and that a newer generation follows: they would only hold
// memory and CPUs, or, parked on a full machine, never finish. Call with
// e.mu held.
func (e *Engine) dropSuperseded(project string) {
	var newest int32
	for _, b := range e.bases {
		if b.Project == project {
			newest = max(newest, b.Generation)
		}
	}
	for _, b := range e.bases {
		if b.Project != project || b.State != building || b.Generation >= newest || e.waitedFor(b.Id) {
			continue
		}
		b.State = stale
		e.saveBase(b, change{
			what: "dropped", detail: "superseded, and no workspace waits for it",
			feed: feed(feedBase, fmt.Sprintf("base %s g%d dropped: superseded, and no workspace waits for it", b.Project, b.Generation), ref("command", b.Command)),
		})
		cmdID := b.Command
		e.startBgLocked(func(ctx context.Context) {
			if _, err := e.Stop(ctx, cmdID, "its base was superseded"); err != nil && ctx.Err() == nil {
				slog.Warn("stop a superseded base build", "command", cmdID, "err", err)
			}
			e.collectSoon()
		})
	}
}

// waitedFor says whether a workspace is preparing on base. Call with e.mu held.
func (e *Engine) waitedFor(base string) bool {
	for _, w := range e.workspaces {
		if w.ws().State == preparing && w.ws().BaseId == base {
			return true
		}
	}
	return false
}
