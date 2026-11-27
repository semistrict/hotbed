package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/gitauth"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/unit"
)

// What the worker runs in a workspace's checkouts runs as the workspace's
// user, chrooted into its rootfs (Units.Exec), using the rootfs's git and
// accounted under the commands' slice.

// syncExeInRootfs is where the worker's own binary is bound read-only inside
// a rootfs to run __sync-apply.
const syncExeInRootfs = "/run/hotbed/exe"

// bundlesInSandbox is where a sync's bundles are bound, read-only, for it
// to be applied in a rootfs: 0.bundle the repo's, then its submodules', in
// order. The names repeat, so the mountpoints a sync leaves in the rootfs
// are only ever a few empty files.
const bundlesInSandbox = "/run/hotbed/sync"

// syncApplyArg runs gitops.Apply in a rootfs: hotbed __sync-apply JSON,
// as Units.Exec runs it. It prints how it went, a syncApplied, and exits 0
// whether or not the apply did.
const syncApplyArg = "__sync-apply"

// Main runs the worker stage args (os.Args[1:]) name and exits. It returns
// only when they name none.
func Main(args []string) {
	if len(args) == 0 || args[0] != syncApplyArg {
		return
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "hotbed sync-apply: want one argument")
		os.Exit(2)
	}
	var a syncApply
	if err := json.Unmarshal([]byte(args[1]), &a); err != nil {
		fmt.Fprintf(os.Stderr, "hotbed sync-apply: %v\n", err)
		os.Exit(2)
	}
	n, err := gitops.Apply(context.Background(), gitops.Git{Dir: a.Dir}, a.Bundle, a.Commit, a.Base, a.Subs)
	if err := json.NewEncoder(os.Stdout).Encode(appliedOf(n, err)); err != nil {
		fmt.Fprintf(os.Stderr, "hotbed sync-apply: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// syncApply is gitops.Apply's arguments, as the rootfs sees them.
type syncApply struct {
	Dir    string
	Bundle string
	Commit string
	Base   string
	Subs   []gitops.SubCheckout
}

// syncApplied is how a gitops.Apply in a rootfs went: the files it
// changed, or its error, a gitops.ApplyError's fields if it was one.
type syncApplied struct {
	Files       int    `json:"files"`
	Err         string `json:"err,omitempty"`
	Mutated     bool   `json:"mutated,omitempty"`
	RolledBack  bool   `json:"rolledBack,omitempty"`
	RollbackErr string `json:"rollbackErr,omitempty"`
}

func appliedOf(n int, err error) syncApplied {
	var ae *gitops.ApplyError
	switch {
	case err == nil:
		return syncApplied{Files: n}
	case errors.As(err, &ae):
		a := syncApplied{Err: ae.Err.Error(), Mutated: ae.Mutated, RolledBack: ae.RolledBack}
		if ae.RollbackErr != nil {
			a.RollbackErr = ae.RollbackErr.Error()
		}
		return a
	default:
		return syncApplied{Err: err.Error()}
	}
}

// result is a's files changed, or its error as gitops.Apply returned it.
func (a syncApplied) result() (int, error) {
	if a.Err == "" {
		return a.Files, nil
	}
	if !a.Mutated {
		return 0, errors.New(a.Err)
	}
	ae := &gitops.ApplyError{Err: errors.New(a.Err), Mutated: true, RolledBack: a.RolledBack}
	if a.RollbackErr != "" {
		ae.RollbackErr = errors.New(a.RollbackErr)
	}
	return 0, ae
}

// inRootfs is dir, a path in volume volPath on the host, as the volume's
// rootfs sees it.
func inRootfs(volPath, dir string) (string, error) {
	rel, err := filepath.Rel(volPath, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is not in volume %s", dir, volPath)
	}
	return filepath.Join("/", rel), nil
}

// execSpec builds a chrooted ExecSpec for argv in volPath as the workspace user,
// with the worker's binary, git credentials, and any sync bundles bound read-only.
func (e *Engine) execSpec(volPath, dir string, argv []string, bundles ...string) (unit.ExecSpec, error) {
	exe, err := os.Executable()
	if err != nil {
		return unit.ExecSpec{}, err
	}
	roBinds := []unit.Bind{{Source: exe, Target: syncExeInRootfs}}
	if e.gitAuth != nil {
		// A checkout is a partial clone: git fetches what it lacks from
		// the repo's remote, with the worker's credentials, as a
		// command's git does (inRootfs).
		roBinds = append(roBinds, unit.Bind{Source: e.gitAuth.Dir(), Target: gitauth.ChrootDir})
	}
	for i, b := range bundles {
		roBinds = append(roBinds, unit.Bind{Source: b, Target: bundleInSandbox(i)})
	}
	return unit.ExecSpec{Root: volPath, ReadOnlyBinds: roBinds, Dir: dir, User: e.cfg.User, Argv: argv}, nil
}

// bundleInSandbox is where a rootfs sees a sync's i'th bundle.
func bundleInSandbox(i int) string {
	return filepath.Join(bundlesInSandbox, strconv.Itoa(i)+".bundle")
}

// sandboxEnv is the environment of what the worker runs in a rootfs.
func (e *Engine) sandboxEnv() []string {
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	var env []string
	if e.gitAuth != nil {
		env = e.gitAuth.ChrootEnv()
		path = gitauth.ChrootDir + "/bin:" + path
	}
	return append(env, "HOME="+e.chrootHome(), "GIT_TERMINAL_PROMPT=0", "PATH="+path)
}

// gitIn runs git in dir, a checkout in volume volPath, in its rootfs as its user.
func (e *Engine) gitIn(volPath, dir string) (gitops.Git, error) {
	inner, err := inRootfs(volPath, dir)
	if err != nil {
		return gitops.Git{}, err
	}
	return gitops.Git{
		Dir: inner,
		Env: e.sandboxEnv(),
		Command: func(ctx context.Context, dir string, args []string) (*exec.Cmd, error) {
			c, err := e.execSpec(volPath, dir, append([]string{"git"}, args...))
			if err != nil {
				return nil, err
			}
			return e.host.Exec(ctx, unit.ExecPrefix+newID(), e.cfg.Slice, c)
		},
	}, nil
}

// apply runs gitops.Apply in dir, a checkout in volume volPath, in its rootfs:
// its file operations as well as its git, with bundle and subs' bundles (in the
// worker's bundles directory) as the rootfs sees them.
func (e *Engine) apply(ctx context.Context, volPath, dir, bundle, commit, base string, subs []gitops.SubCheckout) (int, error) {
	c, err := e.applyConfig(volPath, dir, bundle, commit, base, subs)
	if err != nil {
		return 0, err
	}
	return e.runApply(ctx, c)
}

// applyConfig is the chroot ExecSpec in which apply runs gitops.Apply in a rootfs.
func (e *Engine) applyConfig(volPath, dir, bundle, commit, base string, subs []gitops.SubCheckout) (unit.ExecSpec, error) {
	inner, err := inRootfs(volPath, dir)
	if err != nil {
		return unit.ExecSpec{}, err
	}
	a := syncApply{Dir: inner, Bundle: bundleInSandbox(0), Commit: commit, Base: base}
	bundles := []string{bundle}
	for _, s := range subs {
		bundles = append(bundles, s.Bundle)
		s.Bundle = bundleInSandbox(len(bundles) - 1)
		a.Subs = append(a.Subs, s)
	}
	arg, err := json.Marshal(a)
	if err != nil {
		return unit.ExecSpec{}, err
	}
	return e.execSpec(volPath, "/", []string{syncExeInRootfs, syncApplyArg, string(arg)}, bundles...)
}

// runApply runs c, applyConfig's chroot ExecSpec, and returns what its apply did.
func (e *Engine) runApply(ctx context.Context, c unit.ExecSpec) (int, error) {
	cmd, err := e.host.Exec(ctx, unit.ExecPrefix+newID(), e.cfg.Slice, c)
	if err != nil {
		return 0, err
	}
	cmd.Env = append(os.Environ(), e.sandboxEnv()...)
	// What the rootfs says is read only so far: its root may have made
	// the apply say anything (ptrace is root's in its namespace).
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	var said []byte
	saidDone := make(chan struct{})
	go func() {
		defer close(saidDone)
		said = readAtMost(stderr, maxSaid)
	}()
	out := readAtMost(stdout, maxSaid)
	<-saidDone
	runErr := cmd.Wait()
	var applied syncApplied
	if err := json.Unmarshal(out, &applied); runErr != nil || err != nil {
		// Whatever ended it may have ended it mid-apply: the checkout is
		// as good as left inconsistent.
		return 0, &gitops.ApplyError{
			Err:     fmt.Errorf("apply in the rootfs: %w: %s", errors.Join(runErr, err), strings.TrimSpace(string(said))),
			Mutated: true,
		}
	}
	return applied.result()
}

// maxSaid is the most of an apply's output the worker reads.
const maxSaid = 64 << 10

// readAtMost reads r to its end, keeping its first n bytes.
func readAtMost(r io.Reader, n int64) []byte {
	b, err := io.ReadAll(io.LimitReader(r, n))
	if err == nil {
		_, err = io.Copy(io.Discard, r)
	}
	if err != nil {
		slog.Warn("read an apply's output", "err", err)
	}
	return b
}

// gitDeadline is how long the worker's git in a checkout may take where no
// caller is waiting to give up on it: the rootfs's git is its root's to
// replace, with one that never ends.
const gitDeadline = time.Minute

// runWithin runs f with ctx ended after d.
func runWithin[T any](ctx context.Context, d time.Duration, f func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx)
}
