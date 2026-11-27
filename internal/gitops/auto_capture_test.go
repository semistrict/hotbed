package gitops

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBundleForSyncEmptyDirAndTreeOnlyPreservesMtimes(t *testing.T) {
	ctx := t.Context()

	// 1. Source repo with two files.
	src := t.TempDir()
	sg := Git{Dir: src, Env: []string{"GIT_CONFIG_GLOBAL=/dev/null"}}
	mustGit := func(g Git, args ...string) string {
		t.Helper()
		out, err := g.Run(ctx, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return out
	}
	mustGit(sg, "init", "-q", "-b", "main")
	mustGit(sg, "config", "user.name", "test")
	mustGit(sg, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("unchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "edit.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(sg, "add", ".")
	mustGit(sg, "commit", "-q", "-m", "initial")

	snap1, err := Snapshot(ctx, src)
	if err != nil {
		t.Fatalf("Snapshot 1: %v", err)
	}

	// 2. First sync into a brand-new, non-existent workspace directory (base == "").
	ws := filepath.Join(t.TempDir(), "fresh-repo")
	var b1 bytes.Buffer
	res1, err := BundleForSync(ctx, snap1, "", &b1)
	if err != nil {
		t.Fatalf("BundleForSync empty base: %v", err)
	}
	if res1.HasAnchor {
		t.Fatal("expected HasAnchor=false for empty base tree-only bundle")
	}
	f1 := filepath.Join(t.TempDir(), "b1.bundle")
	if err := os.WriteFile(f1, b1.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	wg := Git{Dir: ws, Env: []string{"GIT_CONFIG_GLOBAL=/dev/null"}}
	changed, err := Apply(ctx, wg, f1, res1.Commit, "", nil)
	if err != nil {
		t.Fatalf("Apply onto fresh dir: %v", err)
	}
	if changed != 2 {
		t.Fatalf("initial changed = %d, want 2", changed)
	}

	// Set an old mtime on both files so we can verify mtime preservation.
	oldTime := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	for _, name := range []string{"keep.txt", "edit.txt"} {
		if err := os.Chtimes(filepath.Join(ws, name), oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}

	// 3. Edit one file in src and sync with an unknown base SHA (simulating a
	// workspace captured from a commit this checkout has not fetched).
	if err := os.WriteFile(filepath.Join(src, "edit.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap2, err := Snapshot(ctx, src)
	if err != nil {
		t.Fatalf("Snapshot 2: %v", err)
	}
	unknownBase := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var b2 bytes.Buffer
	res2, err := BundleForSync(ctx, snap2, unknownBase, &b2)
	if err != nil {
		t.Fatalf("BundleForSync unknown base: %v", err)
	}
	if res2.HasAnchor {
		t.Fatal("expected HasAnchor=false for unknown base tree-only bundle")
	}
	f2 := filepath.Join(t.TempDir(), "b2.bundle")
	if err := os.WriteFile(f2, b2.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	changed2, err := Apply(ctx, wg, f2, res2.Commit, unknownBase, nil)
	if err != nil {
		t.Fatalf("Apply tree-only over existing checkout: %v", err)
	}
	if changed2 != 1 {
		t.Fatalf("changed2 = %d, want 1", changed2)
	}

	keepInfo, err := os.Stat(filepath.Join(ws, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !keepInfo.ModTime().Equal(oldTime) {
		t.Fatalf("keep.txt mtime changed (%v -> %v); tree-only checkout must preserve unchanged mtimes", oldTime, keepInfo.ModTime())
	}
	editInfo, err := os.Stat(filepath.Join(ws, "edit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !editInfo.ModTime().After(oldTime) {
		t.Fatalf("edit.txt mtime did not advance (%v)", editInfo.ModTime())
	}
	got, err := os.ReadFile(filepath.Join(ws, "edit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2\n" {
		t.Fatalf("edit.txt = %q, want v2", string(got))
	}
}

func TestAnchorCommitPrefersOriginMergeBase(t *testing.T) {
	ctx := t.Context()
	origin := t.TempDir()
	og := Git{Dir: origin, Env: []string{"GIT_CONFIG_GLOBAL=/dev/null"}}
	mustGit := func(g Git, args ...string) string {
		t.Helper()
		out, err := g.Run(ctx, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return out
	}
	mustGit(og, "init", "-q", "--bare", "-b", "main")

	work := t.TempDir()
	wg := Git{Dir: work, Env: []string{"GIT_CONFIG_GLOBAL=/dev/null"}}
	mustGit(wg, "init", "-q", "-b", "main")
	mustGit(wg, "config", "user.name", "test")
	mustGit(wg, "config", "user.email", "test@example.com")
	mustGit(wg, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(wg, "add", ".")
	mustGit(wg, "commit", "-q", "-m", "main commit")
	mustGit(wg, "push", "-q", "-u", "origin", "main")
	mainSHA := mustGit(wg, "rev-parse", "HEAD")

	// Branch off main with a local unpushed commit.
	mustGit(wg, "checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(wg, "add", ".")
	mustGit(wg, "commit", "-q", "-m", "feature commit")

	got, ts := AnchorCommit(ctx, work)
	if got != mainSHA {
		t.Fatalf("AnchorCommit = %q, want origin/main merge-base %q", got, mainSHA)
	}
	if ts <= 0 {
		t.Fatalf("AnchorCommit timestamp = %d, want > 0", ts)
	}
}
