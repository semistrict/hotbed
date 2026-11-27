package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyClearsStaleGitLocks(t *testing.T) {
	ctx := t.Context()
	// Source repo with a base commit and an edited commit.
	src := t.TempDir()
	sg := Git{Dir: src}
	mustGit(t, sg, "init", "-b", "main")
	mustGit(t, sg, "config", "user.email", "test@example.com")
	mustGit(t, sg, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, sg, "add", "a.txt")
	mustGit(t, sg, "commit", "-m", "base")
	base := mustGit(t, sg, "rev-parse", "HEAD")

	// Workspace clone at base.
	ws := t.TempDir()
	mustGit(t, Git{Dir: ws}, "clone", "--quiet", src, ".")
	wg := Git{Dir: ws}

	// Make a new commit in src and bundle it.
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, sg, "add", "a.txt")
	mustGit(t, sg, "commit", "-m", "v2")
	head := mustGit(t, sg, "rev-parse", "HEAD")
	mustGit(t, sg, "update-ref", SyncRef, head)
	bundle := filepath.Join(t.TempDir(), "sync.bundle")
	mustGit(t, sg, "bundle", "create", bundle, SyncRef)

	// Simulate a timed-out/SIGKILLed prior Sync leaving stale locks behind.
	for _, rel := range []string{"index.lock", "HEAD.lock", filepath.FromSlash(SyncRef) + ".lock"} {
		p := filepath.Join(ws, ".git", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Also simulate a stale submodule lock under .git/modules/sub/index.lock.
	subLock := filepath.Join(ws, ".git", "modules", "sub", "index.lock")
	if err := os.MkdirAll(filepath.Dir(subLock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subLock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := Apply(ctx, wg, bundle, head, base, nil)
	if err != nil {
		t.Fatalf("Apply with stale locks failed: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	got, err := os.ReadFile(filepath.Join(ws, "a.txt"))
	if err != nil || string(got) != "v2\n" {
		t.Fatalf("a.txt = %q (%v), want v2", got, err)
	}
	if _, err := os.Lstat(subLock); !os.IsNotExist(err) {
		t.Fatalf("submodule lock %s was not removed (err=%v)", subLock, err)
	}
}

func TestRollbackApplyClearsStaleIndexLock(t *testing.T) {
	ctx := t.Context()
	ws := t.TempDir()
	wg := Git{Dir: ws}
	mustGit(t, wg, "init", "-b", "main")
	mustGit(t, wg, "config", "user.email", "test@example.com")
	mustGit(t, wg, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wg, "add", "a.txt")
	mustGit(t, wg, "commit", "-m", "v1")
	prevHead := mustGit(t, wg, "rev-parse", "HEAD")

	// Leave a stale index.lock as a SIGKILLed git checkout would on timeout.
	lockPath := filepath.Join(ws, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("killed"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate rollback under an already-cancelled Apply context.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := rollbackApply(cancelled, wg, prevHead); err != nil {
		t.Fatalf("rollbackApply with stale index.lock failed: %v", err)
	}
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("index.lock still exists after rollbackApply (err=%v)", err)
	}
}

func mustGit(t *testing.T, g Git, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
