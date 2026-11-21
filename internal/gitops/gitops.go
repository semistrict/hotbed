// Package gitops is the git that hotbed runs: snapshotting an agent's
// worktree into a bundle on the client, and applying it in a workspace on the
// worker.
package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/semistrict/hotbed/internal/api"
)

// Git runs git in Dir, optionally as another user.
type Git struct {
	Dir  string
	Env  []string // added to the environment
	Cred *syscall.Credential
}

// Run runs git and returns its trimmed stdout.
func (g Git) Run(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	if err := g.run(ctx, nil, &out, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func (g Git) run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = append(os.Environ(), g.Env...)
	if g.Cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: g.Cred}
	}
	var stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Snap is an agent's worktree, frozen as a commit that no branch points to.
type Snap struct {
	Root   string // the worktree's top directory
	Commit string
	Tree   string
}

// Snapshot commits everything in the worktree containing dir (tracked and
// untracked files, minus ignored ones) without touching the agent's index,
// HEAD or branches: it stages into a copy of the index and writes a commit
// whose parent is HEAD.
func Snapshot(ctx context.Context, dir string) (Snap, error) {
	g := Git{Dir: dir}
	root, err := g.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return Snap{}, refused("%s is not in a git worktree", dir)
	}
	g.Dir = root
	// Entries marked skip-worktree or assume-unchanged hide real edits from
	// git add; a snapshot would silently leave them out.
	flags, err := g.Run(ctx, "ls-files", "-v")
	if err != nil {
		return Snap{}, err
	}
	for _, line := range strings.Split(flags, "\n") {
		if line == "" {
			continue
		}
		if tag := line[0]; tag == 'S' || (tag >= 'a' && tag <= 'z') {
			return Snap{}, refused("%s is marked skip-worktree or assume-unchanged, so its edits would not sync", strings.TrimSpace(line[1:])).
				WithSuggestion("clear the marks with git update-index --no-skip-worktree --no-assume-unchanged")
		}
	}
	if dirty, err := g.Run(ctx, "submodule", "foreach", "--quiet", "--recursive", "git status --porcelain"); err != nil {
		return Snap{}, err
	} else if dirty != "" {
		return Snap{}, refused("a submodule has uncommitted changes, which sync cannot carry").
			WithSuggestion("commit them in the submodule; sync carries the new submodule commit")
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
	tree, err := staged.Run(ctx, "write-tree")
	if err != nil {
		return Snap{}, err
	}
	args := []string{"commit-tree", tree, "-m", "hotbed sync"}
	if head, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil && head != "" {
		args = append(args, "-p", head)
	}
	commit, err := Git{Dir: root, Env: identity}.Run(ctx, args...)
	if err != nil {
		return Snap{}, err
	}
	return Snap{Root: root, Commit: commit, Tree: tree}, nil
}

// identity lets commit-tree run where no user.name is configured.
var identity = []string{
	"GIT_AUTHOR_NAME=hotbed", "GIT_AUTHOR_EMAIL=hotbed@localhost",
	"GIT_COMMITTER_NAME=hotbed", "GIT_COMMITTER_EMAIL=hotbed@localhost",
}

// SyncRef is the ref a bundle carries a snapshot under.
const SyncRef = "refs/hotbed/sync"

// Bundle writes a git bundle of snap's commit that leaves out everything the
// workspace's base already has. The worktree must have the base commit.
func Bundle(ctx context.Context, snap Snap, base string, w io.Writer) error {
	g := Git{Dir: snap.Root}
	if _, err := g.Run(ctx, "cat-file", "-e", base+"^{commit}"); err != nil {
		return refused("this checkout does not have the workspace's base commit %s", base).
			WithSuggestion("git fetch the branch the base follows, then sync again")
	}
	if _, err := g.Run(ctx, "update-ref", SyncRef, snap.Commit); err != nil {
		return err
	}
	defer g.Run(context.WithoutCancel(ctx), "update-ref", "-d", SyncRef)
	return g.run(ctx, nil, w, "bundle", "create", "--quiet", "-", SyncRef, "^"+base)
}

func refused(format string, args ...any) *api.Error {
	return api.Errorf(api.CodeSyncRefused, format, args...)
}

// Apply checks out a snapshot from a bundle in the workspace checkout g.Dir,
// and returns how many files differ from the base. git rewrites only files
// whose content changed, so everything else keeps the mtime the base's build
// saw.
func Apply(ctx context.Context, g Git, bundle, commit, base string) (int, error) {
	if _, err := g.Run(ctx, "fetch", "--quiet", "--no-write-fetch-head", bundle, "+"+SyncRef+":"+SyncRef); err != nil {
		return 0, err
	}
	// Re-stat the index first: a file whose stat info drifted (a copy, a
	// touch) but whose content is unchanged would otherwise be rewritten by
	// the forced checkout, and get a new mtime for nothing.
	if _, err := g.Run(ctx, "update-index", "-q", "--refresh"); err != nil {
		// update-index exits 1 when files differ from the index, which a
		// workspace with local edits does; checkout below handles them.
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return 0, err
		}
	}
	if _, err := g.Run(ctx, "checkout", "--quiet", "--detach", "--force", commit); err != nil {
		return 0, err
	}
	if _, err := g.Run(ctx, "submodule", "update", "--quiet", "--init", "--recursive", "--force"); err != nil {
		return 0, err
	}
	names, err := g.Run(ctx, "diff", "--name-only", base, commit)
	if err != nil {
		return 0, err
	}
	if names == "" {
		return 0, nil
	}
	return strings.Count(names, "\n") + 1, nil
}
