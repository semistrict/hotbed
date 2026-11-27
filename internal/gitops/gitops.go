// Package gitops is the git that hotbed runs: snapshotting an agent's
// worktree into a bundle on the client, and applying it in a workspace on the
// worker.
package gitops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/api"
)

// Git runs git in Dir, optionally as another user.
type Git struct {
	Dir  string
	Env  []string // added to the environment
	Cred *syscall.Credential
	// Command, when set, makes the git command for args in Dir (it runs
	// git somewhere else: in a workspace's rootfs, say); Cred is then not
	// used.
	Command func(ctx context.Context, dir string, args []string) (*exec.Cmd, error)
}

// maxOutput is the most of git's stdout Run and Lines keep, and
// maxStderr of its stderr an error says: a git a sandbox may have replaced
// (Git.Command) could write without end into the worker's memory.
const (
	maxOutput = 128 << 20
	maxStderr = 64 << 10
)

// cappedBuffer keeps what is written to it up to max bytes. Past them it
// keeps no more, notes it, and calls onOver (run's: kill git, which might
// never stop writing); the writer is never blocked. The buffer is not
// embedded: its ReadFrom, which io.Copy prefers, would write past max.
type cappedBuffer struct {
	buf    bytes.Buffer
	max    int
	over   bool
	onOver func()
}

func newCappedBuffer(max int) *cappedBuffer { return &cappedBuffer{max: max} }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n, room := len(p), b.max-b.buf.Len()
	if n > room {
		if !b.over && b.onOver != nil {
			b.onOver()
		}
		b.over = true
		p = p[:max(room, 0)]
	}
	b.buf.Write(p)
	return n, nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// Run runs git and returns its trimmed stdout.
func (g Git) Run(ctx context.Context, args ...string) (string, error) {
	out := newCappedBuffer(maxOutput)
	err := g.run(ctx, nil, out, args...)
	if out.over {
		// Killed for it: what it wrote is the error, not how it ended.
		return "", fmt.Errorf("git %s wrote over %d bytes", strings.Join(args, " "), maxOutput)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// Lines runs git and returns its stdout's lines, as written: leading
// spaces, which say something in git status --porcelain, are kept.
func (g Git) Lines(ctx context.Context, args ...string) ([]string, error) {
	out := newCappedBuffer(maxOutput)
	err := g.run(ctx, nil, out, args...)
	if out.over {
		// Killed for it: what it wrote is the error, not how it ended.
		return nil, fmt.Errorf("git %s wrote over %d bytes", strings.Join(args, " "), maxOutput)
	}
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(out.String(), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

func (g Git) command(ctx context.Context, args ...string) (*exec.Cmd, *cappedBuffer, error) {
	fullArgs := make([]string, 0, len(args)+2)
	fullArgs = append(fullArgs, "-c", "protocol.ext.allow=never")
	fullArgs = append(fullArgs, args...)
	var cmd *exec.Cmd
	if g.Command != nil {
		c, err := g.Command(ctx, g.Dir, fullArgs)
		if err != nil {
			return nil, nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		cmd = c
	} else {
		cmd = exec.CommandContext(ctx, "git", fullArgs...)
		cmd.Dir = g.Dir
		if g.Cred != nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: g.Cred}
		}
	}
	cmd.Env = append(os.Environ(), g.Env...)
	stderr := newCappedBuffer(maxStderr)
	cmd.Stderr = stderr
	return cmd, stderr, nil
}

func (g Git) run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if out, ok := stdout.(*cappedBuffer); ok {
		out.onOver = cancel
	}
	cmd, stderr, err := g.command(ctx, args...)
	if err != nil {
		return err
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// stream runs git and passes its stdout pipe to fn without buffering all
// output in memory. If fn returns early with an error, git is canceled and
// waited on before stream returns.
func (g Git) stream(ctx context.Context, fn func(r io.Reader) error, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, stderr, err := g.command(ctx, args...)
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	fnErr := fn(stdout)
	if fnErr != nil {
		cancel()
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	if fnErr != nil {
		return fnErr
	}
	if waitErr != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Snap is an agent's worktree, frozen as a commit that no branch points to,
// with each of its populated submodules frozen the same way, recursively:
// the commit's gitlinks name the submodules' snapshots, so its tree says
// the whole state.
type Snap struct {
	Root   string // the worktree's top directory
	Commit string
	Tree   string
	Head   string // the worktree's HEAD, "" if unborn; Commit when it matches HEAD
	Subs   []Sub  // its populated submodules
}

// Sub is a submodule's snapshot, at Path in the worktree holding it.
type Sub struct {
	Path string // slash-separated, relative to the parent's Root
	Name string // its name in the parent's .gitmodules
	Snap
}

// Snapshot commits everything in the worktree containing dir (tracked and
// untracked files, minus ignored ones) without touching the agent's index,
// HEAD or branches: it stages into a copy of the index and writes a commit
// whose parent is HEAD. Each populated submodule is snapshotted first, the
// same way, and the commit records the submodule's snapshot.
func Snapshot(ctx context.Context, dir string) (Snap, error) {
	root, err := Git{Dir: dir}.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return Snap{}, refused("%s is not in a git worktree", dir)
	}
	return snapshot(ctx, root, true)
}

// snapshot freezes the worktree at root. A worktree that matches HEAD is
// HEAD itself, unless always asks for a commit of it anyway. A snapshot
// commit takes HEAD's date, so the same state always makes the same commit,
// and a parent's tree changes only when a submodule's state does.
func snapshot(ctx context.Context, root string, always bool) (Snap, error) {
	g := Git{Dir: root}
	if err := refuseHiddenEdits(ctx, g); err != nil {
		return Snap{}, err
	}
	snap := Snap{Root: root}
	paths, err := populatedSubmodules(ctx, g)
	if err != nil {
		return Snap{}, err
	}
	var names map[string]string
	if len(paths) > 0 {
		if names, err = submoduleNames(ctx, g, ""); err != nil {
			return Snap{}, err
		}
	}
	var gitlinks strings.Builder
	for _, p := range paths {
		name, ok := names[p]
		if !ok {
			return Snap{}, refused("%s is a repo in the worktree that no .gitmodules entry names, so a workspace could not check it out", filepath.Join(root, p)).
				WithSuggestion("make it a submodule with git submodule add, or move it out of the worktree")
		}
		s, err := snapshot(ctx, filepath.Join(root, filepath.FromSlash(p)), false)
		if err != nil {
			return Snap{}, err
		}
		snap.Subs = append(snap.Subs, Sub{Path: p, Name: name, Snap: s})
		fmt.Fprintf(&gitlinks, "160000 %s\t%s\x00", s.Commit, p)
	}

	gitDir, err := g.Run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return Snap{}, err
	}
	tmp, err := os.CreateTemp("", "hotbed-index-*")
	if err != nil {
		return Snap{}, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	// Starting from the real index keeps its stat cache, so git add hashes
	// only files that changed.
	if b, err := os.ReadFile(filepath.Join(gitDir, "index")); err == nil {
		if err := os.WriteFile(tmp.Name(), b, 0o600); err != nil {
			return Snap{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snap{}, err
	} else {
		os.Remove(tmp.Name())
	}
	staged := Git{Dir: root, Env: []string{"GIT_INDEX_FILE=" + tmp.Name()}}
	if _, err := staged.Run(ctx, "add", "--all", "--", "."); err != nil {
		return Snap{}, err
	}
	// git add records a submodule's HEAD; the snapshot records its state.
	if gitlinks.Len() > 0 {
		if err := staged.run(ctx, strings.NewReader(gitlinks.String()), io.Discard, "update-index", "-z", "--index-info"); err != nil {
			return Snap{}, err
		}
	}
	if snap.Tree, err = staged.Run(ctx, "write-tree"); err != nil {
		return Snap{}, err
	}

	// Unsigned: a signature would differ each time, and so would the commit.
	args := []string{"commit-tree", "--no-gpg-sign", snap.Tree, "-m", "hotbed sync"}
	date := "@0 +0000"
	if head, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil && head != "" {
		info, err := g.Run(ctx, "show", "--no-patch", "--format=%T %ct", head, "--")
		if err != nil {
			return Snap{}, err
		}
		snap.Head = head
		headTree, when, _ := strings.Cut(info, " ")
		if headTree == snap.Tree && !always {
			snap.Commit = head
			return snap, nil
		}
		args = append(args, "-p", head)
		date = "@" + when + " +0000"
	}
	env := append([]string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, identity...)
	if snap.Commit, err = (Git{Dir: root, Env: env}).Run(ctx, args...); err != nil {
		return Snap{}, err
	}
	return snap, nil
}

// PinSubs keeps each submodule snapshot that is not its submodule's HEAD
// from gc, under ref prefix+commit in the submodule's repo, recursively:
// nothing else holds it, though the snapshot's commit names it.
func (s Snap) PinSubs(ctx context.Context, prefix string) error {
	if strings.HasPrefix(prefix, "-") {
		return fmt.Errorf("invalid ref prefix %q: must not start with '-'", prefix)
	}
	for _, sub := range s.Subs {
		if sub.Commit == sub.Head {
			continue // so are its submodules: its tree pins them
		}
		if err := ValidateSHA(sub.Commit); err != nil {
			return fmt.Errorf("submodule %s: %w", sub.Path, err)
		}
		if _, err := (Git{Dir: sub.Root}).Run(ctx, "update-ref", "--", prefix+sub.Commit, sub.Commit); err != nil {
			return err
		}
		if err := sub.PinSubs(ctx, prefix); err != nil {
			return err
		}
	}
	return nil
}

// refuseHiddenEdits refuses a worktree with entries marked skip-worktree or
// assume-unchanged: they hide real edits from git add, so a snapshot would
// silently leave them out.
func refuseHiddenEdits(ctx context.Context, g Git) error {
	flags, err := g.Lines(ctx, "ls-files", "-v")
	if err != nil {
		return err
	}
	for _, line := range flags {
		if tag := line[0]; tag == 'S' || (tag >= 'a' && tag <= 'z') {
			return refused("%s is marked skip-worktree or assume-unchanged, so its edits would not sync", filepath.Join(g.Dir, strings.TrimSpace(line[1:]))).
				WithSuggestion("clear the marks with git update-index --no-skip-worktree --no-assume-unchanged")
		}
	}
	return nil
}

// populatedSubmodules lists the submodules checked out in g.Dir's worktree:
// the gitlinks in its index whose directory holds a repo.
func populatedSubmodules(ctx context.Context, g Git) ([]string, error) {
	var out bytes.Buffer
	if err := g.run(ctx, nil, &out, "ls-files", "--stage", "-z"); err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range strings.Split(out.String(), "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		// A conflicted gitlink is listed once per stage.
		if !ok || !strings.HasPrefix(meta, "160000 ") || slices.Contains(paths, path) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(g.Dir, filepath.FromSlash(path), ".git")); err == nil {
			paths = append(paths, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return paths, nil
}

// submoduleNames maps each submodule's path to its name, as .gitmodules
// says: the worktree's, or with rev, the one in that commit.
func submoduleNames(ctx context.Context, g Git, rev string) (map[string]string, error) {
	src := []string{"--file", filepath.Join(g.Dir, ".gitmodules")}
	if rev != "" {
		if err := ValidateSHA(rev); err != nil {
			return nil, fmt.Errorf("invalid revision: %w", err)
		}
		if _, err := g.Run(ctx, "cat-file", "-e", rev+":.gitmodules"); err != nil {
			return map[string]string{}, nil
		}
		src = []string{"--blob", rev + ":.gitmodules"}
	} else if _, err := os.Stat(src[1]); errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	var out bytes.Buffer
	args := append(append([]string{"config", "-z"}, src...), "--get-regexp", `^submodule\..*\.path$`)
	if err := g.run(ctx, nil, &out, args...); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return map[string]string{}, nil // no submodule in it
		}
		return nil, err
	}
	names := map[string]string{}
	for _, rec := range strings.Split(out.String(), "\x00") {
		key, path, ok := strings.Cut(rec, "\n")
		if !ok {
			continue
		}
		names[path] = strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
	}
	return names, nil
}

// identity lets commit-tree run where no user.name is configured.
var identity = []string{
	"GIT_AUTHOR_NAME=hotbed", "GIT_AUTHOR_EMAIL=hotbed@localhost",
	"GIT_COMMITTER_NAME=hotbed", "GIT_COMMITTER_EMAIL=hotbed@localhost",
}

// SyncRef is the ref a bundle carries a snapshot under.
const SyncRef = "refs/hotbed/sync"

// SubBundle carries a submodule's snapshot that the workspace could not get
// otherwise.
type SubBundle struct {
	Path   string // slash-separated, relative to the top worktree
	Commit string
	Data   []byte // a git bundle of Commit
}

// AnchorCommit returns the upstream anchor commit of the worktree at dir —
// merge-base(HEAD, origin/<default>) when an origin default ref exists, else
// HEAD — along with its committer timestamp (unix seconds). Best-effort:
// returns ("", 0) when dir is not a repo or has no commits.
func AnchorCommit(ctx context.Context, dir string) (string, int64) {
	g := Git{Dir: dir}
	var commit string
	for _, ref := range defaultUpstreamRefs(ctx, g) {
		if mb, err := g.Run(ctx, "merge-base", "HEAD", ref); err == nil && ValidateSHA(mb) == nil {
			commit = mb
			break
		}
	}
	if commit == "" {
		if head, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil && ValidateSHA(head) == nil {
			commit = head
		}
	}
	if commit == "" {
		return "", 0
	}
	tsStr, err := g.Run(ctx, "show", "--no-patch", "--format=%ct", commit, "--")
	if err != nil {
		return commit, 0
	}
	ts, _ := strconv.ParseInt(strings.TrimSpace(tsStr), 10, 64)
	return commit, ts
}

func defaultUpstreamRefs(ctx context.Context, g Git) []string {
	var refs []string
	if sym, err := g.Run(ctx, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && sym != "" {
		refs = append(refs, sym)
	}
	for _, cand := range []string{"refs/remotes/origin/main", "refs/remotes/origin/master"} {
		if !slices.Contains(refs, cand) {
			if _, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", cand); err == nil {
				refs = append(refs, cand)
			}
		}
	}
	return refs
}

// BundleResult is what BundleForSync wrote to w.
type BundleResult struct {
	// Commit is the commit carried under SyncRef in the bundle: snap.Commit
	// for an incremental bundle, or a parentless tree-only commit when base
	// was empty or absent locally.
	Commit string
	// HasAnchor is true when the bundle preserves the caller's commit history
	// (including its upstream anchor commit), false for a tree-only bundle.
	HasAnchor bool
	Subs      []SubBundle
}

// BundleForSync writes a git bundle of snap to w. When base is non-empty and
// present in snap's repo, it writes a compact incremental bundle against base.
// Otherwise (brand-new workspace without a checkout yet, or a workspace
// captured from a commit this checkout has not fetched), it writes a
// parentless tree-only commit of snap.Tree so sync never fails or ships full
// repository history, while git checkout still preserves mtimes of unchanged
// files on disk.
func BundleForSync(ctx context.Context, snap Snap, base string, w io.Writer) (BundleResult, error) {
	if err := ValidateSHA(snap.Commit); err != nil {
		return BundleResult{}, fmt.Errorf("invalid snapshot commit: %w", err)
	}
	g := Git{Dir: snap.Root}
	if base != "" && ValidateSHA(base) == nil && hasCommit(ctx, g, base) {
		if err := bundle(ctx, g, snap.Commit, base, w); err != nil {
			return BundleResult{}, err
		}
		var subs []SubBundle
		if err := bundleSubs(ctx, snap, base, "", &subs); err != nil {
			return BundleResult{}, err
		}
		return BundleResult{Commit: snap.Commit, HasAnchor: true, Subs: subs}, nil
	}
	if err := ValidateSHA(snap.Tree); err != nil {
		return BundleResult{}, fmt.Errorf("invalid snapshot tree: %w", err)
	}
	date := "@0 +0000"
	if when, err := g.Run(ctx, "show", "--no-patch", "--format=%ct", snap.Commit, "--"); err == nil && when != "" {
		date = "@" + strings.TrimSpace(when) + " +0000"
	}
	env := append([]string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, identity...)
	treeCommit, err := (Git{Dir: snap.Root, Env: env}).Run(ctx, "commit-tree", "--no-gpg-sign", snap.Tree, "-m", "hotbed sync")
	if err != nil {
		return BundleResult{}, fmt.Errorf("create tree-only sync commit: %w", err)
	}
	if err := bundle(ctx, g, treeCommit, "", w); err != nil {
		return BundleResult{}, err
	}
	var subs []SubBundle
	if err := bundleSubs(ctx, snap, "", "", &subs); err != nil {
		return BundleResult{}, err
	}
	return BundleResult{Commit: treeCommit, HasAnchor: false, Subs: subs}, nil
}

// Bundle writes to w a git bundle of snap's commit that leaves out
// everything the workspace's base already has, and returns a bundle for
// each submodule snapshot the workspace needs, parents before their
// submodules. The worktree must have the base commit.
func Bundle(ctx context.Context, snap Snap, base string, w io.Writer) ([]SubBundle, error) {
	if err := ValidateSHA(snap.Commit); err != nil {
		return nil, fmt.Errorf("invalid snapshot commit: %w", err)
	}
	if err := ValidateSHA(base); err != nil {
		return nil, fmt.Errorf("invalid base commit: %w", err)
	}
	g := Git{Dir: snap.Root}
	if !hasCommit(ctx, g, base) {
		return nil, refused("this checkout does not have the workspace's base commit %s", base).
			WithSuggestion("git fetch the branch the base follows, then sync again")
	}
	if err := bundle(ctx, g, snap.Commit, base, w); err != nil {
		return nil, err
	}
	var subs []SubBundle
	if err := bundleSubs(ctx, snap, base, "", &subs); err != nil {
		return nil, err
	}
	return subs, nil
}

// bundleSubs adds a bundle of each of snap's submodule snapshots,
// recursively, that the workspace has no other way to get. base is the
// commit of snap's repo that the workspace has, or "" if that is unknown
// here.
//
// The workspace's base checked out every submodule at the commit base
// records for it (found by name, which a move keeps), so a bundle leaves
// that out when this checkout has it. Without
// it, a commit already on the submodule's origin is left for the
// workspace's git submodule update to fetch, and anything else, such as a
// new submodule's snapshot, goes whole.
func bundleSubs(ctx context.Context, snap Snap, base, prefix string, out *[]SubBundle) error {
	g := Git{Dir: snap.Root}
	basePaths := map[string]string{} // name to path, in base
	if base != "" && len(snap.Subs) > 0 {
		names, err := submoduleNames(ctx, g, base)
		if err != nil {
			return err
		}
		for path, name := range names {
			basePaths[name] = path
		}
	}
	for _, s := range snap.Subs {
		sg := Git{Dir: s.Root}
		had := ""
		if path, ok := basePaths[s.Name]; ok {
			var err error
			if had, err = gitlink(ctx, g, base, path); err != nil {
				return err
			}
		}
		if had != "" && !hasCommit(ctx, sg, had) {
			had = ""
		}
		if had != "" {
			if s.Commit == had {
				continue // unchanged from the workspace's base
			}
			missing, err := sg.Run(ctx, "rev-list", "--max-count=1", s.Commit, "--not", had, "--")
			if err != nil {
				return err
			}
			if missing == "" {
				// s.Commit is an ancestor of had (e.g. a clean rewind). On a
				// --filter=blob:none workspace at had, commit s.Commit is
				// present locally but blobs/trees from s.Commit^{tree} that
				// changed before had are missing, so bundle them unless the
				// two commits have identical trees.
				commitTree, err := sg.Run(ctx, "rev-parse", "--verify", "--quiet", s.Commit+"^{tree}")
				if err != nil {
					return err
				}
				hadTree, err := sg.Run(ctx, "rev-parse", "--verify", "--quiet", had+"^{tree}")
				if err != nil {
					return err
				}
				if commitTree == hadTree {
					continue
				}
			}
		} else {
			missing, err := sg.Run(ctx, "rev-list", "--max-count=1", s.Commit, "--not", "--remotes=origin", "--")
			if err != nil {
				return err
			}
			if missing == "" {
				continue // its origin has it
			}
		}
		var b bytes.Buffer
		if err := bundle(ctx, sg, s.Commit, had, &b); err != nil {
			return fmt.Errorf("bundle submodule %s: %w", prefix+s.Path, err)
		}
		*out = append(*out, SubBundle{Path: prefix + s.Path, Commit: s.Commit, Data: b.Bytes()})
		if err := bundleSubs(ctx, s.Snap, had, prefix+s.Path+"/", out); err != nil {
			return err
		}
	}
	return nil
}

// bundle writes a git bundle of commit, carried under SyncRef, without what
// base (if given) has.
//
// git bundle create takes refs, not commits, so SyncRef must name commit
// in some repo, but never in g's: its refs are shared by every sync from it
// (each of its linked worktrees, each concurrent run), and one sync's
// SyncRef would land in another's bundle (board 9224). Each bundle is made
// in a throwaway repo of its own that borrows g's objects.
//
// Worker base checkouts are blobless partial clones (--filter=blob:none)
// checked out at base: they have all commits and trees reachable from base,
// and the blobs in base's tree, but NOT blobs from older ancestors of base
// that were modified or deleted before base. Apply runs without network or
// credentials, so the bundle must be self-contained against base's tree alone
// and must never omit or delta-compress against an ancestor blob (board 9337):
//   - When base is the sole boundary commit of base..commit, disabling
//     reachability bitmaps (pack.useBitmaps=false) ensures git bundle create
//     only excludes and deltas against base's tree (keeping thin-pack deltas
//     for the common linear case).
//   - When any other boundary commit appears (a merge with or branch off an
//     ancestor of base), we build a non-thin bundle carrying base..commit plus
//     every tree and blob in commit's tree not already in base's tree.
//   - When commit has no common history with base (orphan branch), we emit a
//     full bundle with no prerequisites.
func bundle(ctx context.Context, g Git, commit, base string, w io.Writer) error {
	if err := ValidateSHA(commit); err != nil {
		return fmt.Errorf("invalid commit: %w", err)
	}
	if base != "" {
		if err := ValidateSHA(base); err != nil {
			return fmt.Errorf("invalid base: %w", err)
		}
	}
	scratch, err := borrowObjects(ctx, g, len(commit) == 64)
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch.Dir)
	if base == "" {
		if _, err := scratch.Run(ctx, "update-ref", "--", SyncRef, commit); err != nil {
			return err
		}
		return scratch.run(ctx, nil, w, "-c", "pack.useBitmaps=false", "bundle", "create", "--quiet", "-", SyncRef)
	}

	var boundaries []string
	interesting := 0
	if err := scratch.stream(ctx, func(r io.Reader) error {
		br := bufio.NewReaderSize(r, 64*1024)
		for {
			line, err := br.ReadSlice('\n')
			if len(line) > 0 {
				line = bytes.TrimRight(line, "\r\n")
				if sha, ok := bytes.CutPrefix(line, []byte("-")); ok {
					if len(sha) > 0 {
						boundaries = append(boundaries, string(sha))
					}
				} else if len(line) > 0 {
					interesting++
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read rev-list --boundary: %w", err)
			}
		}
	}, "-c", "pack.useBitmaps=false", "rev-list", "--boundary", commit, "^"+base, "--"); err != nil {
		return err
	}
	// If commit is a strict ancestor of base (interesting == 0, commit != base),
	// git bundle create would refuse with an empty bundle, yet a
	// --filter=blob:none clone at base may lack blobs from commit^{tree}.
	if interesting == 0 && !strings.EqualFold(commit, base) {
		return writeSelfContainedBundle(ctx, scratch, commit, base, []string{base}, w)
	}
	// If commit == base (letting git bundle create report its standard
	// empty-bundle error) or base is the sole boundary commit, git bundle
	// create without bitmaps only excludes and thin-deltas against base^{tree}.
	if interesting == 0 || (len(boundaries) == 1 && boundaries[0] == base) {
		if _, err := scratch.Run(ctx, "update-ref", "--", SyncRef, commit); err != nil {
			return err
		}
		return scratch.run(ctx, nil, w, "-c", "pack.useBitmaps=false", "bundle", "create", "--quiet", "-", SyncRef, "^"+base)
	}
	// Orphan history with no common ancestor with base: pack the full history
	// with no prerequisites.
	if len(boundaries) == 0 {
		if _, err := scratch.Run(ctx, "update-ref", "--", SyncRef, commit); err != nil {
			return err
		}
		return scratch.run(ctx, nil, w, "-c", "pack.useBitmaps=false", "bundle", "create", "--quiet", "-", SyncRef)
	}
	return writeSelfContainedBundle(ctx, scratch, commit, base, boundaries, w)
}

// oid is a compact binary git object ID: 20 bytes for SHA-1 (trailing 12 bytes
// zero) or 32 bytes for SHA-256. Fixed-size byte arrays avoid per-OID heap
// allocations and GC pointer scanning on large trees and histories.
type oid [32]byte

func parseOID(hexBytes []byte, expectedHexLen int) (oid, error) {
	if len(hexBytes) != expectedHexLen || (expectedHexLen != 40 && expectedHexLen != 64) {
		return oid{}, fmt.Errorf("unexpected git object ID %q (len %d, want %d)", string(hexBytes), len(hexBytes), expectedHexLen)
	}
	var id oid
	if _, err := hex.Decode(id[:expectedHexLen/2], hexBytes); err != nil {
		return oid{}, fmt.Errorf("invalid git object ID %q: %w", string(hexBytes), err)
	}
	return id, nil
}

// writeSelfContainedBundle writes a non-thin git bundle of commit against base
// with prerequisites boundaries. Every object in base..commit plus every tree
// and blob in commit^{tree} absent from base^{tree} is streamed directly into
// pack-objects without --thin, so index-pack and checkout on a --filter=blob:none
// clone at base never need to fetch any ancestor blob from a promisor remote.
func writeSelfContainedBundle(ctx context.Context, scratch Git, commit, base string, boundaries []string, w io.Writer) error {
	hexLen := len(commit)
	baseObjs, err := treeObjects(ctx, scratch, base)
	if err != nil {
		return err
	}

	var hdr bytes.Buffer
	if hexLen == 64 {
		hdr.WriteString("# v3 git bundle\n@object-format=sha256\n")
	} else {
		hdr.WriteString("# v2 git bundle\n")
	}
	for _, b := range boundaries {
		fmt.Fprintf(&hdr, "-%s\n", b)
	}
	fmt.Fprintf(&hdr, "%s %s\n\n", commit, SyncRef)
	if _, err := w.Write(hdr.Bytes()); err != nil {
		return err
	}

	packCtx, cancelPack := context.WithCancel(ctx)
	defer cancelPack()

	packCmd, packStderr, err := scratch.command(packCtx, "-c", "pack.useBitmaps=false", "pack-objects", "--stdout", "--quiet")
	if err != nil {
		return err
	}
	packCmd.Stdout = w
	packIn, err := packCmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("git pack-objects: %w", err)
	}
	if err := packCmd.Start(); err != nil {
		return fmt.Errorf("git pack-objects: %w", err)
	}

	bw := bufio.NewWriterSize(packIn, 64*1024)
	var writeErr error
	emittedCount := 0
	writeHex := func(hexBytes []byte) error {
		if _, err := bw.Write(hexBytes); err != nil {
			writeErr = err
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			writeErr = err
			return err
		}
		emittedCount++
		return nil
	}

	// Phase 1: stream commit^{tree} \ base^{tree} into pack-objects, recording
	// only those tree-diff OIDs in treeEmitted (bounded by working tree size)
	// so Phase 2 can skip duplicates without holding the full divergent
	// history in memory.
	treeEmitted := make(map[oid]struct{})
	feedErr := streamTreeObjects(ctx, scratch, commit, func(id oid, hexBytes []byte) error {
		if _, inBase := baseObjs[id]; inBase {
			return nil
		}
		if _, dup := treeEmitted[id]; dup {
			return nil
		}
		treeEmitted[id] = struct{}{}
		return writeHex(hexBytes)
	})
	baseObjs = nil // allow GC before streaming rev-list

	// Phase 2: stream base..commit directly from rev-list into pack-objects.
	// git rev-list --objects deduplicates its own output, so we only skip OIDs
	// already emitted in Phase 1.
	if feedErr == nil {
		feedErr = scratch.stream(ctx, func(r io.Reader) error {
			br := bufio.NewReaderSize(r, 64*1024)
			for {
				line, err := br.ReadSlice('\n')
				if len(line) > 0 {
					line = bytes.TrimRight(line, "\r\n")
					if len(line) > 0 {
						id, parseErr := parseOID(line, hexLen)
						if parseErr != nil {
							return parseErr
						}
						if _, dup := treeEmitted[id]; !dup {
							if werr := writeHex(line); werr != nil {
								return werr
							}
						}
					}
				}
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("read rev-list output: %w", err)
				}
			}
		}, "-c", "pack.useBitmaps=false", "rev-list", "--objects", "--no-object-names", commit, "^"+base, "--")
	}

	// If neither phase emitted anything (e.g. a rewind whose tree objects all
	// exist under a subtree of base), emit commit itself so the bundle carries
	// a non-empty packfile.
	if feedErr == nil && emittedCount == 0 {
		feedErr = writeHex([]byte(commit))
	}

	if feedErr == nil {
		if err := bw.Flush(); err != nil {
			writeErr = err
			feedErr = err
		}
	}
	if feedErr != nil {
		cancelPack()
		_ = packIn.Close()
		waitErr := packCmd.Wait()
		if writeErr != nil && waitErr != nil {
			return fmt.Errorf("git pack-objects: %w: %s", waitErr, strings.TrimSpace(packStderr.String()))
		}
		return feedErr
	}
	if err := packIn.Close(); err != nil {
		cancelPack()
		_ = packCmd.Wait()
		return fmt.Errorf("git pack-objects: %w", err)
	}
	if err := packCmd.Wait(); err != nil {
		return fmt.Errorf("git pack-objects: %w: %s", err, strings.TrimSpace(packStderr.String()))
	}
	return nil
}

// streamTreeObjects streams every tree and blob OID in rev^{tree} (including
// the root tree itself), skipping submodule gitlinks (160000 commit). File
// paths are discarded on the fly without allocating or buffering full output.
func streamTreeObjects(ctx context.Context, g Git, rev string, fn func(id oid, hexBytes []byte) error) error {
	rootTree, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{tree}")
	if err != nil {
		return err
	}
	hexLen := len(rootTree)
	rootID, err := parseOID([]byte(rootTree), hexLen)
	if err != nil {
		return err
	}
	if err := fn(rootID, []byte(rootTree)); err != nil {
		return err
	}
	return g.stream(ctx, func(r io.Reader) error {
		br := bufio.NewReaderSize(r, 64*1024)
		var hexBuf [64]byte
		for {
			// Each NUL-terminated record is "<mode> <type> <oid>\t<path>\x00".
			// Parse the fixed-size metadata before discarding the path so
			// ReadSlice's internal buffer is never overwritten while reading meta.
			meta, err := br.ReadSlice('\t')
			if errors.Is(err, io.EOF) && len(meta) == 0 {
				return nil
			}
			if err != nil {
				return fmt.Errorf("parse ls-tree metadata: %w", err)
			}
			meta = meta[:len(meta)-1] // strip trailing '\t'
			var (
				want   bool
				id     oid
				oidLen int
			)
			if sp1 := bytes.IndexByte(meta, ' '); sp1 >= 0 {
				rest := meta[sp1+1:]
				if sp2 := bytes.IndexByte(rest, ' '); sp2 >= 0 {
					objType := rest[:sp2]
					oidHex := rest[sp2+1:]
					if bytes.Equal(objType, []byte("blob")) || bytes.Equal(objType, []byte("tree")) {
						id, err = parseOID(oidHex, hexLen)
						if err != nil {
							return err
						}
						oidLen = copy(hexBuf[:], oidHex)
						want = true
					}
				}
			}
			// Discard the path up to NUL in O(1) memory regardless of length.
			for {
				_, pathErr := br.ReadSlice(0)
				if pathErr == nil {
					break
				}
				if errors.Is(pathErr, bufio.ErrBufferFull) {
					continue
				}
				return fmt.Errorf("parse ls-tree path: %w", pathErr)
			}
			if want {
				if err := fn(id, hexBuf[:oidLen]); err != nil {
					return err
				}
			}
		}
	}, "ls-tree", "-r", "-t", "-z", rev, "--")
}

// treeObjects returns the set of all tree and blob OIDs in rev^{tree}
// (including the root tree itself), skipping submodule gitlinks (160000 commit).
func treeObjects(ctx context.Context, g Git, rev string) (map[oid]struct{}, error) {
	objs := make(map[oid]struct{})
	if err := streamTreeObjects(ctx, g, rev, func(id oid, _ []byte) error {
		objs[id] = struct{}{}
		return nil
	}); err != nil {
		return nil, err
	}
	return objs, nil
}

// borrowObjects makes a bare repo in a new temporary directory, which the
// caller removes, that reads g's objects (and those g borrows) through
// alternates and has no refs of its own.
func borrowObjects(ctx context.Context, g Git, sha256 bool) (scratch Git, retErr error) {
	common, err := g.Run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Git{}, err
	}
	objects, err := g.Run(ctx, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return Git{}, err
	}
	dir, err := os.MkdirTemp("", "hotbed-bundle-")
	if err != nil {
		return Git{}, err
	}
	defer func() {
		if retErr != nil {
			os.RemoveAll(dir)
		}
	}()
	scratch = Git{Dir: dir, Env: g.Env, Cred: g.Cred}
	initArgs := []string{"init", "--quiet", "--bare", "--template="}
	if sha256 {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	initArgs = append(initArgs, ".")
	if _, err := scratch.Run(ctx, initArgs...); err != nil {
		return Git{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return Git{}, err
	}
	// A shallow repo's walk must stop where its history does.
	if b, err := os.ReadFile(filepath.Join(common, "shallow")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "shallow"), b, 0o600); err != nil {
			return Git{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Git{}, err
	}
	return scratch, nil
}

func hasCommit(ctx context.Context, g Git, commit string) bool {
	if ValidateSHA(commit) != nil {
		return false
	}
	_, err := g.Run(ctx, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// gitlink is the commit that commit records for the submodule at path, or
// "" if it records none.
func gitlink(ctx context.Context, g Git, commit, path string) (string, error) {
	entry, err := g.Run(ctx, "--literal-pathspecs", "ls-tree", "-z", commit, "--", path)
	if err != nil {
		return "", err
	}
	meta, got, _ := strings.Cut(strings.TrimSuffix(entry, "\x00"), "\t")
	if f := strings.Fields(meta); len(f) == 3 && f[0] == "160000" && got == path {
		return f[2], nil
	}
	return "", nil
}

func refused(format string, args ...any) *api.Error {
	return api.Errorf(api.CodeSyncRefused, format, args...)
}

// SubCheckout is a submodule snapshot to check out, from a bundle file.
type SubCheckout struct {
	Path   string // slash-separated, relative to the top checkout
	Commit string
	Bundle string
}

// ApplyError reports a failed Apply along with whether the checkout was mutated
// and whether rollback cleanly restored its previous state.
type ApplyError struct {
	Err         error // the underlying apply failure
	Mutated     bool  // true if the checkout was mutated before failing
	RolledBack  bool  // true if Mutated and rollback cleanly restored the previous checkout
	RollbackErr error // non-nil when rollback was attempted and failed
}

// Inconsistent reports whether the checkout was mutated and could not be
// cleanly restored to its previous state.
func (e *ApplyError) Inconsistent() bool {
	return e.Mutated && !e.RolledBack
}

func (e *ApplyError) Error() string {
	switch {
	case !e.Mutated:
		return e.Err.Error()
	case e.RolledBack:
		return fmt.Sprintf("%v (rolled back to previous checkout)", e.Err)
	case e.RollbackErr != nil:
		return fmt.Sprintf("%v (checkout left inconsistent; rollback failed: %v)", e.Err, e.RollbackErr)
	default:
		return fmt.Sprintf("%v (checkout left inconsistent)", e.Err)
	}
}

func (e *ApplyError) Unwrap() error { return e.Err }

// Apply checks out a snapshot from a bundle in the workspace checkout g.Dir,
// then each submodule snapshot in subs (parents before their submodules),
// then every other submodule at the commit the snapshot records, and
// returns how many files differ from the base. git rewrites only files
// whose content changed, so everything else keeps the mtime the base's
// build saw.
//
// If Apply fails after mutating the checkout, it attempts to roll back the
// checkout and its submodules to the previous HEAD and returns an *ApplyError
// reporting whether rollback succeeded or the checkout remains inconsistent.
func Apply(ctx context.Context, g Git, bundle, commit, base string, subs []SubCheckout) (int, error) {
	if err := ValidateSHA(commit); err != nil {
		return 0, fmt.Errorf("invalid commit: %w", err)
	}
	if base != "" {
		if err := ValidateSHA(base); err != nil {
			return 0, fmt.Errorf("invalid base: %w", err)
		}
	}
	if err := validateBundlePath(bundle); err != nil {
		return 0, fmt.Errorf("invalid bundle: %w", err)
	}
	for _, s := range subs {
		if !filepath.IsLocal(filepath.FromSlash(s.Path)) {
			return 0, fmt.Errorf("submodule path %q is outside the checkout", s.Path)
		}
		if err := ValidateSHA(s.Commit); err != nil {
			return 0, fmt.Errorf("submodule %s: invalid commit: %w", s.Path, err)
		}
		if err := validateBundlePath(s.Bundle); err != nil {
			return 0, fmt.Errorf("submodule %s: invalid bundle: %w", s.Path, err)
		}
	}
	// A fresh workspace starting from the bare OS base image has no repo
	// directory yet; initialize it so the first sync populates /work/<repo>.
	if _, err := os.Stat(filepath.Join(g.Dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(g.Dir, 0o755); err != nil {
			return 0, fmt.Errorf("create checkout dir %s: %w", g.Dir, err)
		}
		if _, err := g.Run(ctx, "init", "--quiet"); err != nil {
			return 0, fmt.Errorf("init checkout %s: %w", g.Dir, err)
		}
	}
	// Sync is serialized per workspace with no active commands, so any leftover
	// git lock files (e.g. .git/index.lock from a previously timed-out or
	// cancelled Sync whose git child was SIGKILLed) are stale and would wedge
	// every subsequent run in this workspace.
	clearStaleGitLocks(ctx, g)

	prevHead, _ := g.Run(ctx, "rev-parse", "--verify", "--quiet", "HEAD")

	// Fetch the top-level bundle and confirm its SyncRef resolves to commit
	// before mutating the checkout or any submodules.
	if err := fetchBundleCommit(ctx, g, bundle, commit); err != nil {
		return 0, err
	}

	failAfterMutation := func(err error) (int, error) {
		rbErr := rollbackApply(ctx, g, prevHead)
		if errors.Is(err, errSubmoduleCleanup) {
			rbErr = errors.Join(rbErr, errors.New("failed submodule cleanup did not complete"))
		}
		return 0, &ApplyError{
			Err:         err,
			Mutated:     true,
			RolledBack:  rbErr == nil,
			RollbackErr: rbErr,
		}
	}

	if err := checkoutFetched(ctx, g, commit); err != nil {
		return failAfterMutation(err)
	}
	var done []string
	for _, s := range subs {
		parent := ""
		for _, d := range done {
			if strings.HasPrefix(s.Path, d+"/") && len(d) > len(parent) {
				parent = d
			}
		}
		rel := s.Path
		if parent != "" {
			rel = strings.TrimPrefix(s.Path, parent+"/")
		}
		pg := g
		pg.Dir = filepath.Join(g.Dir, filepath.FromSlash(parent))
		if err := checkoutSub(ctx, pg, rel, s); err != nil {
			return failAfterMutation(fmt.Errorf("submodule %s: %w", s.Path, err))
		}
		done = append(done, s.Path)
	}
	if _, err := g.Run(ctx, "submodule", "update", "--quiet", "--init", "--recursive", "--force"); err != nil {
		return failAfterMutation(err)
	}
	diffFrom := ""
	switch {
	case base != "" && hasCommit(ctx, g, base):
		diffFrom = base
	case prevHead != "" && hasCommit(ctx, g, prevHead):
		diffFrom = prevHead
	}
	var names []string
	var err error
	if diffFrom != "" {
		names, err = g.Lines(ctx, "diff", "--name-only", diffFrom, commit, "--")
	} else {
		names, err = g.Lines(ctx, "ls-tree", "-r", "--name-only", commit, "--")
	}
	if err != nil {
		return failAfterMutation(err)
	}
	return len(names), nil
}

// rollbackApply restores g.Dir and its submodules to prevHead after a failed
// Apply that mutated the checkout, verifying HEAD and every submodule match.
func rollbackApply(ctx context.Context, g Git, prevHead string) error {
	if prevHead == "" {
		return errors.New("no previous HEAD to roll back to")
	}
	if err := ValidateSHA(prevHead); err != nil {
		return fmt.Errorf("invalid previous HEAD: %w", err)
	}
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	// Clear any lock files left by a killed/cancelled git process in Apply
	// before rolling back; otherwise update-index and checkout fail immediately.
	clearStaleGitLocks(rbCtx, g)

	if err := deinitDropped(rbCtx, g, prevHead); err != nil {
		return fmt.Errorf("deinit dropped submodules: %w", err)
	}
	if _, err := g.Run(rbCtx, "update-index", "-q", "--refresh"); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return fmt.Errorf("refresh index: %w", err)
		}
	}
	if _, err := g.Run(rbCtx, "checkout", "--quiet", "--detach", "--force", prevHead, "--"); err != nil {
		return fmt.Errorf("checkout previous HEAD: %w", err)
	}
	if _, err := g.Run(rbCtx, "submodule", "sync", "--quiet", "--recursive"); err != nil {
		return fmt.Errorf("sync submodules: %w", err)
	}
	if err := deinitDroppedInSubs(rbCtx, g, prevHead); err != nil {
		return fmt.Errorf("deinit dropped nested submodules: %w", err)
	}
	if _, err := g.Run(rbCtx, "submodule", "update", "--quiet", "--init", "--recursive", "--force", "--no-fetch"); err != nil {
		return fmt.Errorf("restore submodules: %w", err)
	}
	gotHead, err := g.Run(rbCtx, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return fmt.Errorf("verify HEAD: %w", err)
	}
	if gotHead != prevHead {
		return fmt.Errorf("HEAD is %s after rollback, want %s", gotHead, prevHead)
	}
	status, err := g.Lines(rbCtx, "submodule", "status", "--recursive")
	if err != nil {
		return fmt.Errorf("verify submodule status: %w", err)
	}
	for _, line := range status {
		if !strings.HasPrefix(line, " ") {
			return fmt.Errorf("submodule out of step after rollback: %q", line)
		}
	}
	return nil
}

// staleGitLockNames are git lock files that a SIGKILLed Sync (checkout,
// update-index, update-ref, submodule update) can leave behind in a gitdir.
var staleGitLockNames = map[string]bool{
	"index.lock":       true,
	"HEAD.lock":        true,
	"shallow.lock":     true,
	"packed-refs.lock": true,
	"config.lock":      true,
}

// clearStaleGitLocks removes leftover git lock files in g's gitdir (and any
// submodule gitdirs under its modules/ tree). Best-effort: Sync is serialized
// per workspace with no active commands, so any lock file present when Apply
// begins or rolls back belongs to an aborted prior git process.
func clearStaleGitLocks(ctx context.Context, g Git) {
	gitDir, err := g.Run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil || gitDir == "" {
		return
	}
	removeGitDirLocks(gitDir)
	modulesDir := filepath.Join(gitDir, "modules")
	if fi, err := os.Lstat(modulesDir); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return
	}
	_ = filepath.WalkDir(modulesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Skip git object/pack/logs/worktrees subtrees to keep the walk fast.
			switch d.Name() {
			case "objects", "logs", "hooks", "info":
				return filepath.SkipDir
			}
			return nil
		}
		if staleGitLockNames[d.Name()] || strings.HasSuffix(filepath.ToSlash(path), "/"+SyncRef+".lock") {
			_ = os.Remove(path)
		} else if strings.HasSuffix(filepath.ToSlash(path), "/"+SyncRef) {
			removeBrokenLooseRef(path)
		}
		return nil
	})
}

func removeGitDirLocks(gitDir string) {
	for name := range staleGitLockNames {
		p := filepath.Join(gitDir, name)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			_ = os.Remove(p)
		}
	}
	syncLock := filepath.Join(gitDir, filepath.FromSlash(SyncRef)+".lock")
	if fi, err := os.Lstat(syncLock); err == nil && fi.Mode().IsRegular() {
		_ = os.Remove(syncLock)
	}
	removeBrokenLooseRef(filepath.Join(gitDir, filepath.FromSlash(SyncRef)))
}

// removeBrokenLooseRef removes a loose ref file (such as refs/hotbed/sync) if
// a prior I/O error or ENOSPC left it empty or truncated, which otherwise
// makes every subsequent `git update-ref` fail with "reference broken".
func removeBrokenLooseRef(path string) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := ValidateSHA(strings.TrimSpace(string(raw))); err != nil {
		_ = os.Remove(path)
	}
}

// deinitDroppedInSubs walks populated submodules of g bottom-up and deinits any
// nested submodules absent from the commit parentRev records for each submodule.
func deinitDroppedInSubs(ctx context.Context, g Git, parentRev string) error {
	populated, err := populatedSubmodules(ctx, g)
	if err != nil {
		return err
	}
	for _, p := range populated {
		target, err := gitlink(ctx, g, parentRev, p)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		sg := g
		sg.Dir = filepath.Join(g.Dir, filepath.FromSlash(p))
		if !hasCommit(ctx, sg, target) {
			continue
		}
		if err := deinitDroppedInSubs(ctx, sg, target); err != nil {
			return err
		}
		if err := deinitDropped(ctx, sg, target); err != nil {
			return err
		}
	}
	return nil
}

var errSubmoduleCleanup = errors.New("cleanup failed submodule")

// fetchBundleCommit unbundles bundle into g, verifies that SyncRef in the
// bundle resolves to commit, and updates SyncRef rather than merely checking
// whether commit already exists somewhere in g's local object database.
// Using "git bundle unbundle" instead of "git fetch" guarantees the packfile
// is unpacked even when commit already exists locally (such as a rewound
// submodule on a --filter=blob:none clone whose historical blobs are missing).
func fetchBundleCommit(ctx context.Context, g Git, bundle, commit string) error {
	if err := ValidateSHA(commit); err != nil {
		return fmt.Errorf("invalid commit: %w", err)
	}
	if err := validateBundlePath(bundle); err != nil {
		return fmt.Errorf("invalid bundle: %w", err)
	}
	lines, err := g.Lines(ctx, "bundle", "unbundle", "--", bundle)
	if err != nil {
		return err
	}
	var refOID string
	for _, line := range lines {
		id, ref, ok := strings.Cut(line, " ")
		if ok && ref == SyncRef {
			refOID = id
			break
		}
	}
	if refOID == "" {
		return fmt.Errorf("bundle %s did not contain %s", bundle, SyncRef)
	}
	if err := ValidateSHA(refOID); err != nil {
		return fmt.Errorf("bundle %s has invalid OID for %s: %w", bundle, SyncRef, err)
	}
	got, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", refOID+"^{commit}")
	if err != nil {
		return fmt.Errorf("bundle %s did not unbundle commit for %s: %w", bundle, SyncRef, err)
	}
	if !strings.EqualFold(got, commit) {
		return fmt.Errorf("bundle %s carries commit %s for %s, want %s", bundle, got, SyncRef, commit)
	}
	if _, err := g.Run(ctx, "update-ref", "--", SyncRef, got); err != nil {
		return err
	}
	return nil
}

func checkoutFetched(ctx context.Context, g Git, commit string) error {
	if err := ValidateSHA(commit); err != nil {
		return fmt.Errorf("invalid commit: %w", err)
	}
	if err := deinitDropped(ctx, g, commit); err != nil {
		return err
	}
	// Re-stat the index first: a file whose stat info drifted (a copy, a
	// touch) but whose content is unchanged would otherwise be rewritten by
	// the forced checkout, and get a new mtime for nothing.
	if _, err := g.Run(ctx, "update-index", "-q", "--refresh"); err != nil {
		// update-index exits 1 when files differ from the index, which a
		// workspace with local edits does; checkout below handles them.
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return err
		}
	}
	_, err := g.Run(ctx, "checkout", "--quiet", "--detach", "--force", commit, "--")
	return err
}

// checkout fetches commit from bundle into g.Dir's repo and checks it out.
func checkout(ctx context.Context, g Git, bundle, commit string) error {
	if err := fetchBundleCommit(ctx, g, bundle, commit); err != nil {
		return err
	}
	return checkoutFetched(ctx, g, commit)
}

// deinitDropped removes the checkouts of submodules that commit no longer
// has at their paths, which git checkout leaves behind. Their repos stay
// in the parent's, for a submodule moved elsewhere to use.
func deinitDropped(ctx context.Context, g Git, commit string) error {
	if err := ValidateSHA(commit); err != nil {
		return fmt.Errorf("invalid commit: %w", err)
	}
	populated, err := populatedSubmodules(ctx, g)
	if err != nil || len(populated) == 0 {
		return err
	}
	var out bytes.Buffer
	if err := g.run(ctx, nil, &out, "ls-tree", "-r", "-z", "--full-tree", commit, "--"); err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, entry := range strings.Split(out.String(), "\x00") {
		if meta, path, ok := strings.Cut(entry, "\t"); ok && strings.HasPrefix(meta, "160000 ") {
			kept[path] = true
		}
	}
	dropped := []string{"submodule", "deinit", "--quiet", "--force", "--"}
	for _, p := range populated {
		if !kept[p] {
			dropped = append(dropped, p)
		}
	}
	if len(dropped) == 5 {
		return nil
	}
	_, err = g.Run(ctx, append([]string{"--literal-pathspecs"}, dropped...)...)
	return err
}

// checkoutSub checks out s in the submodule at rel in parent's checkout
// pg.Dir, which must record s's commit there. A submodule with no checkout
// gets one as git submodule update would make it: in the repo the parent
// keeps for it (one moved since the base still has it), or a new one, its
// origin the one .gitmodules names, moved into the parent's.
func checkoutSub(ctx context.Context, pg Git, rel string, s SubCheckout) (retErr error) {
	if got, err := gitlink(ctx, pg, "HEAD", rel); err != nil {
		return err
	} else if got != s.Commit {
		return fmt.Errorf("the parent records %q there, not the snapshot %s", got, s.Commit)
	}
	sg := pg
	sg.Dir = filepath.Join(pg.Dir, filepath.FromSlash(rel))
	if _, err := os.Lstat(filepath.Join(sg.Dir, ".git")); err == nil {
		return checkout(ctx, sg, s.Bundle, s.Commit)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Absence of .git does not establish ownership of sg.Dir: it may be a
	// pre-existing directory holding untracked workspace/build files. Refuse
	// any non-directory, symlink, or non-empty directory before mutating it.
	var preDirInfo os.FileInfo
	if fi, err := os.Lstat(sg.Dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("destination %q is not a directory", rel)
		}
		entries, err := os.ReadDir(sg.Dir)
		if err != nil {
			return fmt.Errorf("read destination %q: %w", rel, err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("destination %q is not empty", rel)
		}
		preDirInfo = fi
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	names, err := submoduleNames(ctx, pg, "")
	if err != nil {
		return err
	}
	name, ok := names[rel]
	if !ok {
		return errors.New("no .gitmodules entry names it")
	}
	if _, err := pg.Run(ctx, "submodule", "init", "--quiet", "--", rel); err != nil {
		return err
	}
	gitDir, err := pg.Run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	kept := filepath.Join(gitDir, "modules", filepath.FromSlash(name))
	if _, err := os.Stat(kept); err == nil {
		mg := pg
		mg.Dir = kept
		if err := fetchBundleCommit(ctx, mg, s.Bundle, s.Commit); err != nil {
			return err
		}
		_, err := pg.Run(ctx, "submodule", "update", "--quiet", "--no-fetch", "--force", "--", rel)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	url, err := pg.Run(ctx, "config", "--get", "--", "submodule."+name+".url")
	if err != nil {
		return fmt.Errorf("no URL for it: %w", err)
	}
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("submodule URL %q must not start with '-'", url)
	}
	if _, err := pg.Run(ctx, "init", "--quiet", "--", rel); err != nil {
		return err
	}
	cleanupNew := true
	defer func() {
		if !cleanupNew {
			return
		}
		if cleanErr := cleanupNewSubmoduleDir(sg.Dir, preDirInfo); cleanErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("%w %s: %w", errSubmoduleCleanup, rel, cleanErr))
		}
	}()
	if _, err := sg.Run(ctx, "remote", "add", "--", "origin", url); err != nil {
		return err
	}
	if err := checkout(ctx, sg, s.Bundle, s.Commit); err != nil {
		return err
	}
	if _, err := pg.Run(ctx, "submodule", "absorbgitdirs", "--", rel); err != nil {
		return err
	}
	cleanupNew = false
	return nil
}

// cleanupNewSubmoduleDir removes only artifacts created by a failed brand-new
// submodule initialization in dir. If dir did not exist before git init
// (preDirInfo == nil), it removes dir itself; if dir already existed and was
// verified empty (such as an empty gitlink directory created by git checkout),
// it verifies dir is still the same real directory and removes only its
// entries so git rollback can cleanly remove or keep the directory.
func cleanupNewSubmoduleDir(dir string, preDirInfo os.FileInfo) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	if preDirInfo == nil {
		return os.RemoveAll(dir)
	}
	if !os.SameFile(preDirInfo, fi) {
		return fmt.Errorf("%q changed during submodule initialization", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
