package gitops

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git{Dir: dir, Env: identity}.Run(t.Context(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// agentRepo is an agent's checkout of a repo whose main is the base.
func agentRepo(t *testing.T) (dir, base string) {
	dir = t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "main")
	write(t, filepath.Join(dir, ".gitignore"), "target/\n")
	write(t, filepath.Join(dir, "src/lib.rs"), "pub fn a() {}\n")
	write(t, filepath.Join(dir, "src/keep.rs"), "pub fn keep() {}\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "base")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

func TestSnapshotCarriesDirtyAndUntrackedNotIgnored(t *testing.T) {
	dir, base := agentRepo(t)
	write(t, filepath.Join(dir, "src/lib.rs"), "pub fn a() { b() }\n")
	write(t, filepath.Join(dir, "src/new.rs"), "pub fn b() {}\n")
	write(t, filepath.Join(dir, "target/debug/big"), "artifact")
	indexBefore := git(t, dir, "ls-files", "--stage")

	snap, err := Snapshot(t.Context(), filepath.Join(dir, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "ls-tree", "-r", "--name-only", snap.Tree); got != ".gitignore\nsrc/keep.rs\nsrc/lib.rs\nsrc/new.rs" {
		t.Errorf("snapshot tree:\n%s", got)
	}
	if got := git(t, dir, "rev-parse", snap.Commit+"^"); got != base {
		t.Errorf("snapshot parent %s, want HEAD %s", got, base)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != base {
		t.Errorf("HEAD moved to %s", got)
	}
	if got := git(t, dir, "ls-files", "--stage"); got != indexBefore {
		t.Errorf("index changed:\n%s", got)
	}
	if got := git(t, dir, "status", "--porcelain"); got != "M src/lib.rs\n?? src/new.rs" {
		t.Errorf("status after snapshot:\n%s", got)
	}
}

func TestSnapshotRefusesHiddenEdits(t *testing.T) {
	dir, _ := agentRepo(t)
	git(t, dir, "update-index", "--skip-worktree", "src/keep.rs")
	_, err := Snapshot(t.Context(), dir)
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeSyncRefused {
		t.Errorf("Snapshot = %v, want SYNC_REFUSED", err)
	}
}

func TestBundleAppliesOnlyChangedFiles(t *testing.T) {
	dir, base := agentRepo(t)
	// The workspace: a clone at the base, as a fork of the base volume is.
	ws := filepath.Join(t.TempDir(), "ws")
	git(t, filepath.Dir(ws), "clone", "--quiet", dir, ws)
	old := time.Now().Add(-time.Hour)
	for _, f := range []string{"src/lib.rs", "src/keep.rs"} {
		if err := os.Chtimes(filepath.Join(ws, f), old, old); err != nil {
			t.Fatal(err)
		}
	}

	write(t, filepath.Join(dir, "src/lib.rs"), "pub fn a() { b() }\n")
	write(t, filepath.Join(dir, "src/new.rs"), "pub fn b() {}\n")
	snap, err := Snapshot(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if err := Bundle(t.Context(), snap, base, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Len() > 2000 {
		t.Errorf("bundle is %d bytes; it should carry only the change", bundle.Len())
	}
	if out := git(t, dir, "for-each-ref", "refs/hotbed"); out != "" {
		t.Errorf("Bundle left a ref behind: %s", out)
	}
	path := filepath.Join(t.TempDir(), "sync.bundle")
	if err := os.WriteFile(path, bundle.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := Apply(t.Context(), Git{Dir: ws, Env: identity}, path, snap.Commit, base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Apply changed %d files, want 2", n)
	}
	b, err := os.ReadFile(filepath.Join(ws, "src/lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "pub fn a() { b() }\n" {
		t.Errorf("lib.rs = %q", b)
	}
	if _, err := os.Stat(filepath.Join(ws, "src/new.rs")); err != nil {
		t.Errorf("new.rs: %v", err)
	}
	keep, err := os.Stat(filepath.Join(ws, "src/keep.rs"))
	if err != nil {
		t.Fatal(err)
	}
	if !keep.ModTime().Equal(old) {
		t.Errorf("keep.rs mtime %v, want untouched %v", keep.ModTime(), old)
	}
	lib, err := os.Stat(filepath.Join(ws, "src/lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	if !lib.ModTime().After(old) {
		t.Errorf("lib.rs mtime %v, want newer than the base's", lib.ModTime())
	}
}

func TestBundleNeedsBaseCommit(t *testing.T) {
	dir, _ := agentRepo(t)
	snap, err := Snapshot(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var e *api.Error
	err = Bundle(t.Context(), snap, "0123456789abcdef0123456789abcdef01234567", &bytes.Buffer{})
	if !errors.As(err, &e) || e.Code != api.CodeSyncRefused {
		t.Errorf("Bundle = %v, want SYNC_REFUSED", err)
	}
}
