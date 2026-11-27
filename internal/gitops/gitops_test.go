package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// A snapshot sees an edit that leaves a file's size and mtime as the index
// recorded them, as an edit in the same second as the index was written
// does: git takes a file whose mtime is not older than the index file's as
// racily clean and hashes it again, so the copy of the index a snapshot
// works on must keep the index's mtime, not take the time it was copied.
func TestSnapshotSeesARacilyCleanEdit(t *testing.T) {
	dir, base := agentRepo(t)
	// Stat data alone must not give the edit away: ctime moves with any
	// write, and git compares it unless told not to.
	git(t, dir, "config", "core.trustctime", "false")
	then := time.Now().Add(-time.Minute).Truncate(time.Second)
	lib := filepath.Join(dir, "src/lib.rs")
	if err := os.Chtimes(lib, then, then); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "update-index", "--refresh") // the index records lib.rs at then
	if err := os.Chtimes(filepath.Join(dir, ".git", "index"), then, then); err != nil {
		t.Fatal(err)
	}
	write(t, lib, "pub fn z() {}\n") // the same size as before
	if err := os.Chtimes(lib, then, then); err != nil {
		t.Fatal(err)
	}

	snap, err := Snapshot(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Tree == git(t, dir, "rev-parse", base+"^{tree}") {
		t.Fatal("the snapshot's tree is HEAD's: it missed the edit to src/lib.rs")
	}
	if got := git(t, dir, "show", snap.Commit+":src/lib.rs"); got != "pub fn z() {}" {
		t.Errorf("snapshot src/lib.rs = %q, want the edit", got)
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
	if subs, err := Bundle(t.Context(), snap, base, &bundle); err != nil || len(subs) != 0 {
		t.Fatalf("Bundle = %v, %v", subs, err)
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

	n, err := Apply(t.Context(), Git{Dir: ws, Env: identity}, path, snap.Commit, base, nil)
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
	_, err = Bundle(t.Context(), snap, "0123456789abcdef0123456789abcdef01234567", &bytes.Buffer{})
	if !errors.As(err, &e) || e.Code != api.CodeSyncRefused {
		t.Errorf("Bundle = %v, want SYNC_REFUSED", err)
	}
}

// Concurrent syncs from one repo (two agents in linked worktrees, which
// share its refs, or two runs in one checkout) each bundle their own
// snapshot: a bundle never carries another sync's commit (board 9224).
func TestConcurrentBundlesCarryTheirOwnCommit(t *testing.T) {
	dir, base := agentRepo(t)
	other := filepath.Join(t.TempDir(), "other")
	git(t, dir, "worktree", "add", "--quiet", "--detach", other, base)
	write(t, filepath.Join(dir, "src/lib.rs"), "pub fn a() { 1 }\n")
	write(t, filepath.Join(other, "src/lib.rs"), "pub fn a() { 2 }\n")
	var snaps [2]Snap
	for i, root := range []string{dir, other} {
		var err error
		if snaps[i], err = Snapshot(t.Context(), root); err != nil {
			t.Fatal(err)
		}
	}
	if snaps[0].Commit == snaps[1].Commit {
		t.Fatal("the two worktrees snapshot the same commit")
	}

	const rounds = 20
	errs := make(chan error, 2*rounds)
	var wg sync.WaitGroup
	bundles := t.TempDir()
	for i := range rounds {
		for j, snap := range snaps {
			wg.Go(func() {
				var b bytes.Buffer
				if _, err := Bundle(t.Context(), snap, base, &b); err != nil {
					errs <- err
					return
				}
				path := filepath.Join(bundles, fmt.Sprintf("%d-%d.bundle", i, j))
				if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
					errs <- err
					return
				}
				heads, err := (Git{Dir: dir}).Run(t.Context(), "bundle", "list-heads", path)
				if err != nil {
					errs <- err
					return
				}
				if want := snap.Commit + " " + SyncRef; heads != want {
					errs <- fmt.Errorf("bundle of %s lists %q, want %q", snap.Root, heads, want)
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if out := git(t, dir, "for-each-ref", "refs/hotbed"); out != "" {
		t.Errorf("Bundle left a ref behind: %s", out)
	}
}

// A shallow checkout bundles too, against a base or whole (as a new
// submodule's snapshot goes): the bundle's walk stops where the checkout's
// history does.
func TestBundleFromShallowCheckout(t *testing.T) {
	origin, _ := agentRepo(t)
	write(t, filepath.Join(origin, "src/lib.rs"), "pub fn a() { 1 }\n")
	git(t, origin, "commit", "--quiet", "-am", "second")
	base := git(t, origin, "rev-parse", "HEAD")
	dir := filepath.Join(t.TempDir(), "shallow")
	git(t, filepath.Dir(dir), "clone", "--quiet", "--depth=1", "file://"+origin, dir)
	write(t, filepath.Join(dir, "src/lib.rs"), "pub fn a() { 2 }\n")
	snap, err := Snapshot(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, against := range []string{base, ""} {
		var b bytes.Buffer
		if err := bundle(t.Context(), Git{Dir: dir}, snap.Commit, against, &b); err != nil {
			t.Fatalf("bundle against %q: %v", against, err)
		}
		path := filepath.Join(t.TempDir(), "sync.bundle")
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if heads := git(t, dir, "bundle", "list-heads", path); heads != snap.Commit+" "+SyncRef {
			t.Errorf("bundle against %q lists %q, want %q", against, heads, snap.Commit+" "+SyncRef)
		}
	}
}

// board 9337: worker base checkouts are blobless partial clones
// (--filter=blob:none) checked out at base, and Apply runs with no network or
// credentials. Bundles must never omit or thin-delta against blobs from older
// ancestors of base that are absent from base's HEAD tree.
func TestBundleAndApplyOnOfflinePartialClone(t *testing.T) {
	// setup builds an origin with commit E -> base (modifying/deleting E's
	// files), a full agent clone, and an offline blobless worker clone at base
	// whose origin is removed and whose transfer.unpackLimit=1 forces
	// index-pack (reproducing production's `error: index-pack died` path).
	setup := func(t *testing.T) (agent, ws, ancestorE, base string, wsGit Git) {
		t.Helper()
		origin := t.TempDir()
		git(t, origin, "init", "--quiet", "-b", "main")
		git(t, origin, "config", "uploadpack.allowFilter", "true")
		git(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

		repeated := strings.Repeat("shared content line for delta compression\n", 80)
		write(t, filepath.Join(origin, "shared.txt"), repeated+"version E\n")
		write(t, filepath.Join(origin, "kept_from_e.txt"), "kept from E\n")
		write(t, filepath.Join(origin, "deleted_in_base.txt"), "only in E\n")
		write(t, filepath.Join(origin, "nested/sub.txt"), "nested E\n")
		write(t, filepath.Join(origin, "nested/run.sh"), "#!/bin/sh\necho E\n")
		if err := os.Chmod(filepath.Join(origin, "nested/run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("shared.txt", filepath.Join(origin, "link.txt")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(origin, "unchanged.txt"), "same everywhere\n")
		git(t, origin, "add", "-A")
		git(t, origin, "commit", "--quiet", "-m", "ancestor E")
		ancestorE = git(t, origin, "rev-parse", "HEAD")
		eSharedBlob := git(t, origin, "rev-parse", ancestorE+":shared.txt")
		eKeptBlob := git(t, origin, "rev-parse", ancestorE+":kept_from_e.txt")

		// base modifies shared.txt, kept_from_e.txt, nested/*, link.txt, and
		// removes deleted_in_base.txt so none of E's old blobs live in base's tree.
		write(t, filepath.Join(origin, "shared.txt"), repeated+"version base\n")
		write(t, filepath.Join(origin, "kept_from_e.txt"), "changed in base\n")
		if err := os.Remove(filepath.Join(origin, "deleted_in_base.txt")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(origin, "nested/sub.txt"), "nested base\n")
		write(t, filepath.Join(origin, "nested/run.sh"), "#!/bin/sh\necho base\n")
		if err := os.Remove(filepath.Join(origin, "link.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("unchanged.txt", filepath.Join(origin, "link.txt")); err != nil {
			t.Fatal(err)
		}
		git(t, origin, "add", "-A")
		git(t, origin, "commit", "--quiet", "-m", "base")
		base = git(t, origin, "rev-parse", "HEAD")

		// Worker: blobless partial clone at base, matching freshenScript.
		ws = filepath.Join(t.TempDir(), "ws")
		git(t, filepath.Dir(ws), "clone", "--quiet", "--filter=blob:none", "--no-checkout", "file://"+origin, ws)
		git(t, ws, "checkout", "--quiet", "--detach", "--force", base)

		// Agent: full clone of origin.
		agent = filepath.Join(t.TempDir(), "agent")
		git(t, filepath.Dir(agent), "clone", "--quiet", "file://"+origin, agent)

		// Cut off origin so any promisor lazy fetch fails hard.
		if err := os.Rename(origin, origin+".gone"); err != nil {
			t.Fatal(err)
		}

		wsEnv := append(slices.Clone(identity),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=transfer.unpackLimit",
			"GIT_CONFIG_VALUE_0=1",
		)
		wsGit = Git{Dir: ws, Env: wsEnv}

		// Verify E's older blobs are genuinely missing in ws (confirming a real
		// partial clone, not a full clone).
		for name, blob := range map[string]string{"shared.txt": eSharedBlob, "kept_from_e.txt": eKeptBlob} {
			if _, err := wsGit.Run(t.Context(), "cat-file", "-e", blob); err == nil {
				t.Fatalf("worker partial clone unexpectedly already has E's blob for %s (%s)", name, blob)
			}
		}
		return agent, ws, ancestorE, base, wsGit
	}

	syncAndVerify := func(t *testing.T, agent, ws, base string, wsGit Git) Snap {
		t.Helper()
		snap, err := Snapshot(t.Context(), agent)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		var b bytes.Buffer
		subs, err := Bundle(t.Context(), snap, base, &b)
		if err != nil {
			t.Fatalf("Bundle: %v", err)
		}
		if len(subs) != 0 {
			t.Fatalf("unexpected submodule bundles: %v", subs)
		}
		bundlePath := filepath.Join(t.TempDir(), "sync.bundle")
		if err := os.WriteFile(bundlePath, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := Apply(t.Context(), wsGit, bundlePath, snap.Commit, base, nil)
		if err != nil {
			t.Fatalf("Apply on offline partial clone failed: %v", err)
		}
		if got := git(t, ws, "rev-parse", "HEAD"); got != snap.Commit {
			t.Errorf("ws HEAD = %s, want %s", got, snap.Commit)
		}
		wantDiff, err := (Git{Dir: agent}).Lines(t.Context(), "diff", "--name-only", base, snap.Commit, "--")
		if err != nil {
			t.Fatal(err)
		}
		if n != len(wantDiff) {
			t.Errorf("Apply FilesChanged = %d, want %d (%v)", n, len(wantDiff), wantDiff)
		}
		mirror, err := Snapshot(t.Context(), ws)
		if err != nil {
			t.Fatalf("Snapshot(ws): %v", err)
		}
		if mirror.Tree != snap.Tree {
			t.Errorf("ws tree = %s, want agent tree %s", mirror.Tree, snap.Tree)
		}
		return snap
	}

	t.Run("merge_from_older_ancestor", func(t *testing.T) {
		agent, ws, ancestorE, base, wsGit := setup(t)
		// Feature branch off E edits shared.txt slightly from E's version
		// (delta candidate against E's missing blob), keeps E's kept_from_e.txt,
		// nested/*, link.txt, deleted_in_base.txt, and merges base using -s ours
		// so the merge commit still references E's blobs while having base as a parent.
		git(t, agent, "checkout", "--quiet", "-b", "feat", ancestorE)
		repeated := strings.Repeat("shared content line for delta compression\n", 80)
		write(t, filepath.Join(agent, "shared.txt"), repeated+"version E feat\n")
		write(t, filepath.Join(agent, "feat.txt"), "feature only\n")
		git(t, agent, "add", "-A")
		git(t, agent, "commit", "--quiet", "-m", "feat off E")
		git(t, agent, "merge", "--quiet", "-s", "ours", "--no-edit", base)

		syncAndVerify(t, agent, ws, base, wsGit)

		if got := readFile(t, filepath.Join(ws, "kept_from_e.txt")); got != "kept from E\n" {
			t.Errorf("kept_from_e.txt = %q", got)
		}
		if got := readFile(t, filepath.Join(ws, "deleted_in_base.txt")); got != "only in E\n" {
			t.Errorf("deleted_in_base.txt = %q", got)
		}
		if got, err := os.Readlink(filepath.Join(ws, "link.txt")); err != nil || got != "shared.txt" {
			t.Errorf("link.txt = %q, %v; want shared.txt", got, err)
		}
		runPath := filepath.Join(ws, "nested/run.sh")
		fi, err := os.Stat(runPath)
		if err != nil {
			t.Fatalf("stat %s: %v", runPath, err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("%s mode = %v; want executable", runPath, fi.Mode())
		}

		// Subsequent sync on the same offline partial clone must also succeed.
		write(t, filepath.Join(agent, "feat.txt"), "feature edited again\n")
		syncAndVerify(t, agent, ws, base, wsGit)
	})

	t.Run("uncommitted_merge_in_progress", func(t *testing.T) {
		agent, ws, ancestorE, base, wsGit := setup(t)
		git(t, agent, "checkout", "--quiet", "-b", "feat", ancestorE)
		write(t, filepath.Join(agent, "feat.txt"), "feature only\n")
		git(t, agent, "add", "feat.txt")
		git(t, agent, "commit", "--quiet", "-m", "feat off E")
		// Start a merge with base without committing (keeps MERGE_HEAD and E's files).
		git(t, agent, "merge", "--quiet", "--no-commit", "-s", "ours", base)
		write(t, filepath.Join(agent, "feat.txt"), "edited during merge\n")

		syncAndVerify(t, agent, ws, base, wsGit)
	})

	t.Run("divergent_branch_from_older_ancestor", func(t *testing.T) {
		agent, ws, ancestorE, base, wsGit := setup(t)
		git(t, agent, "checkout", "--quiet", "-b", "div", ancestorE)
		repeated := strings.Repeat("shared content line for delta compression\n", 80)
		write(t, filepath.Join(agent, "shared.txt"), repeated+"version E div\n")
		git(t, agent, "commit", "--quiet", "-am", "div commit")
		write(t, filepath.Join(agent, "uncommitted.txt"), "dirty on div\n")

		syncAndVerify(t, agent, ws, base, wsGit)
	})

	t.Run("linear_revert_with_reachability_bitmap", func(t *testing.T) {
		agent, ws, ancestorE, base, wsGit := setup(t)
		// Linear history on base, reverting files back to E's exact blobs, with
		// a reachability bitmap built on the agent repo (which without
		// pack.useBitmaps=false would exclude E's blobs as reachable from base).
		git(t, agent, "checkout", ancestorE, "--", "shared.txt", "kept_from_e.txt", "deleted_in_base.txt")
		git(t, agent, "commit", "--quiet", "-m", "revert to E")
		git(t, agent, "repack", "-adb", "--quiet")

		syncAndVerify(t, agent, ws, base, wsGit)
	})

	t.Run("orphan_branch", func(t *testing.T) {
		agent, ws, _, base, wsGit := setup(t)
		git(t, agent, "checkout", "--quiet", "--orphan", "standalone")
		git(t, agent, "rm", "-rf", "--quiet", ".")
		write(t, filepath.Join(agent, "fresh.txt"), "unrelated history\n")
		git(t, agent, "add", "fresh.txt")
		git(t, agent, "commit", "--quiet", "-m", "orphan root")

		syncAndVerify(t, agent, ws, base, wsGit)
	})
}

func TestBundleSubmoduleOnOfflinePartialClone(t *testing.T) {
	subOrigin := t.TempDir()
	subGit(t, subOrigin, "init", "--quiet", "-b", "main")
	subGit(t, subOrigin, "config", "uploadpack.allowFilter", "true")
	subGit(t, subOrigin, "config", "uploadpack.allowAnySHA1InWant", "true")
	repeated := strings.Repeat("submodule delta line\n", 80)
	write(t, filepath.Join(subOrigin, "s1.txt"), repeated+"sub E\n")
	write(t, filepath.Join(subOrigin, "s2.txt"), "sub kept from E\n")
	subGit(t, subOrigin, "add", "-A")
	subGit(t, subOrigin, "commit", "--quiet", "-m", "sub E")
	subE := subGit(t, subOrigin, "rev-parse", "HEAD")
	subEBlob := subGit(t, subOrigin, "rev-parse", subE+":s2.txt")

	write(t, filepath.Join(subOrigin, "s1.txt"), repeated+"sub base\n")
	write(t, filepath.Join(subOrigin, "s2.txt"), "sub base\n")
	subGit(t, subOrigin, "commit", "--quiet", "-am", "sub base")
	subBase := subGit(t, subOrigin, "rev-parse", "HEAD")

	appOrigin := origin(t, map[string]string{"app.txt": "app\n"})
	subGit(t, appOrigin, "config", "uploadpack.allowFilter", "true")
	subGit(t, appOrigin, "config", "uploadpack.allowAnySHA1InWant", "true")
	addSubmodule(t, appOrigin, "file://"+subOrigin, "lib")
	appBase := subGit(t, appOrigin, "rev-parse", "HEAD")

	agent := clone(t, "file://"+appOrigin)

	// Worker: blobless clone of both top-level and submodule lib at base.
	ws := filepath.Join(t.TempDir(), "ws")
	subGit(t, filepath.Dir(ws), "clone", "--quiet", "--filter=blob:none", "--no-checkout", "file://"+appOrigin, ws)
	subGit(t, ws, "checkout", "--quiet", "--detach", "--force", appBase)
	subGit(t, ws, "submodule", "update", "--quiet", "--init", "--filter=blob:none", "--", "lib")
	if got := subGit(t, filepath.Join(ws, "lib"), "rev-parse", "HEAD"); got != subBase {
		t.Fatalf("ws lib HEAD = %s, want %s", got, subBase)
	}

	// Cut off both origins.
	if err := os.Rename(appOrigin, appOrigin+".gone"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(subOrigin, subOrigin+".gone"); err != nil {
		t.Fatal(err)
	}

	wsEnv := append(slices.Clone(identity),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=transfer.unpackLimit",
		"GIT_CONFIG_VALUE_0=1",
	)
	wsGit := Git{Dir: ws, Env: wsEnv}
	if _, err := (Git{Dir: filepath.Join(ws, "lib"), Env: wsEnv}).Run(t.Context(), "cat-file", "-e", subEBlob); err == nil {
		t.Fatalf("worker submodule partial clone unexpectedly has subE's blob %s", subEBlob)
	}

	// Agent branches submodule lib off subE and edits s1.txt.
	agentLib := filepath.Join(agent, "lib")
	subGit(t, agentLib, "checkout", "--quiet", "-b", "div", subE)
	write(t, filepath.Join(agentLib, "s1.txt"), repeated+"sub E edited\n")
	subGit(t, agentLib, "commit", "--quiet", "-am", "sub div")

	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var top bytes.Buffer
	subs, err := Bundle(t.Context(), snap, appBase, &top)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	paths, checkouts := fileBundles(t, subs)
	if paths != "lib" {
		t.Fatalf("bundled submodules = %q, want lib", paths)
	}
	if _, err := Apply(t.Context(), wsGit, writeBundle(t, top.Bytes()), snap.Commit, appBase, checkouts); err != nil {
		t.Fatalf("Apply with partial submodule failed: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "lib/s1.txt")); got != repeated+"sub E edited\n" {
		t.Errorf("lib/s1.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(ws, "lib/s2.txt")); got != "sub kept from E\n" {
		t.Errorf("lib/s2.txt = %q", got)
	}
}

// Rewinding a clean submodule to an ancestor of the workspace's base submodule
// commit must still bundle its missing tree objects so an offline
// --filter=blob:none partial clone can check it out without fetching from the
// promisor remote.
func TestBundleCleanSubmoduleRewindOnOfflinePartialClone(t *testing.T) {
	deepOrigin := t.TempDir()
	subGit(t, deepOrigin, "init", "--quiet", "-b", "main")
	subGit(t, deepOrigin, "config", "uploadpack.allowFilter", "true")
	subGit(t, deepOrigin, "config", "uploadpack.allowAnySHA1InWant", "true")
	write(t, filepath.Join(deepOrigin, "d.txt"), "deep E\n")
	subGit(t, deepOrigin, "add", "-A")
	subGit(t, deepOrigin, "commit", "--quiet", "-m", "deep E")
	deepE := subGit(t, deepOrigin, "rev-parse", "HEAD")
	deepEBlob := subGit(t, deepOrigin, "rev-parse", deepE+":d.txt")

	write(t, filepath.Join(deepOrigin, "d.txt"), "deep base\n")
	subGit(t, deepOrigin, "commit", "--quiet", "-am", "deep base")

	subOrigin := t.TempDir()
	subGit(t, subOrigin, "init", "--quiet", "-b", "main")
	subGit(t, subOrigin, "config", "uploadpack.allowFilter", "true")
	subGit(t, subOrigin, "config", "uploadpack.allowAnySHA1InWant", "true")
	write(t, filepath.Join(subOrigin, "s1.txt"), "sub E v1\n")
	write(t, filepath.Join(subOrigin, "s2.txt"), "sub only in E\n")
	subGit(t, subOrigin, "add", "-A")
	subGit(t, subOrigin, "commit", "--quiet", "-m", "sub E init")
	addSubmodule(t, subOrigin, "file://"+deepOrigin, "deep")
	subGit(t, filepath.Join(subOrigin, "deep"), "checkout", "--quiet", "--detach", deepE)
	subGit(t, subOrigin, "commit", "--quiet", "-am", "sub E with deep E")
	subE := subGit(t, subOrigin, "rev-parse", "HEAD")
	subEBlob := subGit(t, subOrigin, "rev-parse", subE+":s2.txt")

	write(t, filepath.Join(subOrigin, "s1.txt"), "sub base v2\n")
	if err := os.Remove(filepath.Join(subOrigin, "s2.txt")); err != nil {
		t.Fatal(err)
	}
	subGit(t, filepath.Join(subOrigin, "deep"), "checkout", "--quiet", "main")
	subGit(t, subOrigin, "add", "-A")
	subGit(t, subOrigin, "commit", "--quiet", "-m", "sub base")
	subBase := subGit(t, subOrigin, "rev-parse", "HEAD")

	appOrigin := origin(t, map[string]string{"app.txt": "app\n"})
	subGit(t, appOrigin, "config", "uploadpack.allowFilter", "true")
	subGit(t, appOrigin, "config", "uploadpack.allowAnySHA1InWant", "true")
	addSubmodule(t, appOrigin, "file://"+subOrigin, "lib")
	appBase := subGit(t, appOrigin, "rev-parse", "HEAD")

	agent := clone(t, "file://"+appOrigin)

	// Worker: blobless partial clone of top-level, lib, and lib/deep at base.
	ws := filepath.Join(t.TempDir(), "ws")
	subGit(t, filepath.Dir(ws), "clone", "--quiet", "--filter=blob:none", "--no-checkout", "file://"+appOrigin, ws)
	subGit(t, ws, "checkout", "--quiet", "--detach", "--force", appBase)
	subGit(t, ws, "submodule", "update", "--quiet", "--init", "--recursive", "--filter=blob:none")
	if got := subGit(t, filepath.Join(ws, "lib"), "rev-parse", "HEAD"); got != subBase {
		t.Fatalf("ws lib HEAD = %s, want %s", got, subBase)
	}

	// Cut off every origin so any promisor lazy fetch fails hard.
	for _, o := range []string{appOrigin, subOrigin, deepOrigin} {
		if err := os.Rename(o, o+".gone"); err != nil {
			t.Fatal(err)
		}
	}

	wsEnv := append(slices.Clone(identity),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=transfer.unpackLimit",
		"GIT_CONFIG_VALUE_0=1",
	)
	wsGit := Git{Dir: ws, Env: wsEnv}

	// Verify both historical blobs are absent on the worker partial clone,
	// while the ancestor commits themselves are present.
	if _, err := (Git{Dir: filepath.Join(ws, "lib"), Env: wsEnv}).Run(t.Context(), "cat-file", "-e", subE+"^{commit}"); err != nil {
		t.Fatalf("ws lib missing ancestor commit %s: %v", subE, err)
	}
	if _, err := (Git{Dir: filepath.Join(ws, "lib"), Env: wsEnv}).Run(t.Context(), "cat-file", "-e", subEBlob); err == nil {
		t.Fatalf("ws lib unexpectedly already has subE blob %s", subEBlob)
	}
	if _, err := (Git{Dir: filepath.Join(ws, "lib/deep"), Env: wsEnv}).Run(t.Context(), "cat-file", "-e", deepEBlob); err == nil {
		t.Fatalf("ws lib/deep unexpectedly already has deepE blob %s", deepEBlob)
	}

	// Cleanly rewind agent's lib (and its nested deep) to ancestor subE / deepE.
	agentLib := filepath.Join(agent, "lib")
	subGit(t, agentLib, "checkout", "--quiet", "--detach", subE)
	subGit(t, agentLib, "submodule", "update", "--quiet", "--recursive", "--force")
	if status := subGit(t, agentLib, "status", "--porcelain"); status != "" {
		t.Fatalf("agent lib should be clean, got status %q", status)
	}

	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Subs) != 1 || snap.Subs[0].Commit != subE {
		t.Fatalf("snap.Subs = %+v, want clean lib at %s", snap.Subs, subE)
	}

	var top bytes.Buffer
	subs, err := Bundle(t.Context(), snap, appBase, &top)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	paths, checkouts := fileBundles(t, subs)
	if paths != "lib lib/deep" {
		t.Fatalf("bundled submodules = %q, want %q", paths, "lib lib/deep")
	}

	if _, err := Apply(t.Context(), wsGit, writeBundle(t, top.Bytes()), snap.Commit, appBase, checkouts); err != nil {
		t.Fatalf("Apply clean submodule rewind on offline partial clone failed: %v", err)
	}
	if got := subGit(t, filepath.Join(ws, "lib"), "rev-parse", "HEAD"); got != subE {
		t.Errorf("ws lib HEAD = %s, want %s", got, subE)
	}
	if got := readFile(t, filepath.Join(ws, "lib/s1.txt")); got != "sub E v1\n" {
		t.Errorf("lib/s1.txt = %q, want %q", got, "sub E v1\n")
	}
	if got := readFile(t, filepath.Join(ws, "lib/s2.txt")); got != "sub only in E\n" {
		t.Errorf("lib/s2.txt = %q, want %q", got, "sub only in E\n")
	}
	if got := subGit(t, filepath.Join(ws, "lib/deep"), "rev-parse", "HEAD"); got != deepE {
		t.Errorf("ws lib/deep HEAD = %s, want %s", got, deepE)
	}
	if got := readFile(t, filepath.Join(ws, "lib/deep/d.txt")); got != "deep E\n" {
		t.Errorf("lib/deep/d.txt = %q, want %q", got, "deep E\n")
	}
	mirror, err := Snapshot(t.Context(), ws)
	if err != nil {
		t.Fatalf("Snapshot(ws): %v", err)
	}
	if mirror.Tree != snap.Tree {
		t.Errorf("ws tree = %s, want agent tree %s", mirror.Tree, snap.Tree)
	}

	// Moving lib back to subBase on the same offline workspace must also work.
	subGit(t, agentLib, "checkout", "--quiet", "--detach", subBase)
	subGit(t, agentLib, "submodule", "update", "--quiet", "--recursive", "--force")
	snapBack, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatalf("Snapshot back to base: %v", err)
	}
	var topBack bytes.Buffer
	subsBack, err := Bundle(t.Context(), snapBack, appBase, &topBack)
	if err != nil {
		t.Fatalf("Bundle back to base: %v", err)
	}
	if len(subsBack) != 0 {
		t.Fatalf("subsBack = %v, want no submodule bundles when back at base", subsBack)
	}
	if _, err := Apply(t.Context(), wsGit, writeBundle(t, topBack.Bytes()), snapBack.Commit, appBase, nil); err != nil {
		t.Fatalf("Apply back to base failed: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "lib/s1.txt")); got != "sub base v2\n" {
		t.Errorf("lib/s1.txt after return to base = %q", got)
	}
}

// Verify streaming writeSelfContainedBundle and treeObjects work cleanly on
// SHA-256 (64-hex OID) repositories as well as SHA-1.
func TestBundleAndApplySHA256OfflinePartialClone(t *testing.T) {
	origin := t.TempDir()
	git(t, origin, "init", "--quiet", "--object-format=sha256", "-b", "main")
	git(t, origin, "config", "uploadpack.allowFilter", "true")
	git(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	repeated := strings.Repeat("sha256 shared content line\n", 80)
	write(t, filepath.Join(origin, "shared.txt"), repeated+"version E\n")
	write(t, filepath.Join(origin, "only_e.txt"), "only in E\n")
	git(t, origin, "add", "-A")
	git(t, origin, "commit", "--quiet", "-m", "ancestor E")
	ancestorE := git(t, origin, "rev-parse", "HEAD")
	if len(ancestorE) != 64 {
		t.Fatalf("expected 64-char SHA-256 commit, got %q (len %d)", ancestorE, len(ancestorE))
	}
	eOnlyBlob := git(t, origin, "rev-parse", ancestorE+":only_e.txt")

	write(t, filepath.Join(origin, "shared.txt"), repeated+"version base\n")
	if err := os.Remove(filepath.Join(origin, "only_e.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "-A")
	git(t, origin, "commit", "--quiet", "-m", "base")
	base := git(t, origin, "rev-parse", "HEAD")

	ws := filepath.Join(t.TempDir(), "ws")
	git(t, filepath.Dir(ws), "clone", "--quiet", "--filter=blob:none", "--no-checkout", "file://"+origin, ws)
	git(t, ws, "checkout", "--quiet", "--detach", "--force", base)

	agent := filepath.Join(t.TempDir(), "agent")
	git(t, filepath.Dir(agent), "clone", "--quiet", "file://"+origin, agent)

	if err := os.Rename(origin, origin+".gone"); err != nil {
		t.Fatal(err)
	}

	wsEnv := append(slices.Clone(identity),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=transfer.unpackLimit",
		"GIT_CONFIG_VALUE_0=1",
	)
	wsGit := Git{Dir: ws, Env: wsEnv}
	if _, err := wsGit.Run(t.Context(), "cat-file", "-e", eOnlyBlob); err == nil {
		t.Fatalf("worker SHA-256 partial clone unexpectedly has E's blob %s", eOnlyBlob)
	}

	// Diverge off ancestor E to exercise streaming writeSelfContainedBundle + treeObjects.
	git(t, agent, "checkout", "--quiet", "-b", "div", ancestorE)
	write(t, filepath.Join(agent, "shared.txt"), repeated+"version E div\n")
	git(t, agent, "commit", "--quiet", "-am", "div commit")

	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var b bytes.Buffer
	if subs, err := Bundle(t.Context(), snap, base, &b); err != nil || len(subs) != 0 {
		t.Fatalf("Bundle = %v, %v", subs, err)
	}
	if !bytes.HasPrefix(b.Bytes(), []byte("# v3 git bundle\n@object-format=sha256\n")) {
		t.Errorf("expected v3 sha256 bundle header, got %q", string(b.Bytes()[:min(64, b.Len())]))
	}
	bundlePath := writeBundle(t, b.Bytes())
	if _, err := Apply(t.Context(), wsGit, bundlePath, snap.Commit, base, nil); err != nil {
		t.Fatalf("Apply SHA-256 bundle failed: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "only_e.txt")); got != "only in E\n" {
		t.Errorf("only_e.txt = %q, want %q", got, "only in E\n")
	}
	if got := readFile(t, filepath.Join(ws, "shared.txt")); got != repeated+"version E div\n" {
		t.Errorf("shared.txt mismatch")
	}
}

// Verify treeObjects streams ls-tree output without buffering paths or failing
// on paths exceeding bufio.Reader / bufio.Scanner buffer sizes, for both SHA-1
// and SHA-256 object formats, while skipping gitlink (commit) entries.
func TestTreeObjectsStreamsArbitraryPathLengths(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rootHex   string
		treeHex   string
		blobHex   string
		commitHex string
	}{
		{
			name:      "sha1",
			rootHex:   strings.Repeat("a", 40),
			treeHex:   strings.Repeat("b", 40),
			blobHex:   strings.Repeat("c", 40),
			commitHex: strings.Repeat("d", 40),
		},
		{
			name:      "sha256",
			rootHex:   strings.Repeat("1", 64),
			treeHex:   strings.Repeat("2", 64),
			blobHex:   strings.Repeat("3", 64),
			commitHex: strings.Repeat("4", 64),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Emit paths > 128 KiB (exceeding the 64 KiB bufio.Reader buffer)
			// including tabs and newlines in the path portion.
			longPath := strings.Repeat("dir/", 35000) + "file\twith\nweird.txt"
			payload := fmt.Sprintf(
				"040000 tree %s\t%s\x00100644 blob %s\t%s\x00160000 commit %s\tsub\x00",
				tc.treeHex, longPath, tc.blobHex, longPath, tc.commitHex,
			)
			payloadFile := filepath.Join(t.TempDir(), "lstree.bin")
			if err := os.WriteFile(payloadFile, []byte(payload), 0o644); err != nil {
				t.Fatal(err)
			}
			g := Git{
				Dir: t.TempDir(),
				Command: func(ctx context.Context, _ string, args []string) (*exec.Cmd, error) {
					if slices.Contains(args, "rev-parse") {
						return exec.CommandContext(ctx, "printf", "%s\n", tc.rootHex), nil
					}
					return exec.CommandContext(ctx, "cat", payloadFile), nil
				},
			}
			objs, err := treeObjects(t.Context(), g, tc.rootHex)
			if err != nil {
				t.Fatalf("treeObjects: %v", err)
			}
			if len(objs) != 3 {
				t.Fatalf("len(objs) = %d, want 3 (root, tree, blob; skipping commit)", len(objs))
			}
			for _, wantHex := range []string{tc.rootHex, tc.treeHex, tc.blobHex} {
				id, err := parseOID([]byte(wantHex), len(wantHex))
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := objs[id]; !ok {
					t.Errorf("missing expected OID %s", wantHex)
				}
			}
			commitID, err := parseOID([]byte(tc.commitHex), len(tc.commitHex))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := objs[commitID]; ok {
				t.Errorf("gitlink commit OID %s should be skipped", tc.commitHex)
			}
		})
	}
}
