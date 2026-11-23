package baseimage_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	"github.com/semistrict/hotbed/internal/storage"
)

type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	mode     int64
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			if e.typeflag == tar.TypeDir {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if tf != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg && len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func TestExtractLayerWhiteoutsLinksAndPermissions(t *testing.T) {
	root := t.TempDir()

	// Layer 1: base files, directory, symlink, hardlink.
	layer1 := buildTar(t, []tarEntry{
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/keep.txt", body: "keep\n", mode: 0o644},
		{name: "etc/drop.txt", body: "drop\n", mode: 0o644},
		{name: "usr/bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/tool", body: "#!/bin/sh\necho hi\n", mode: 0o755},
		{name: "usr/bin/tool-sym", typeflag: tar.TypeSymlink, linkname: "tool"},
		{name: "usr/bin/tool-hard", typeflag: tar.TypeLink, linkname: "usr/bin/tool"},
		{name: "opt/dir/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "opt/dir/old1", body: "1"},
		{name: "opt/dir/old2", body: "2"},
	})
	if err := baseimage.ExtractLayer(root, layer1); err != nil {
		t.Fatalf("layer 1: %v", err)
	}

	// Layer 2: delete etc/drop.txt, opaque-replace opt/dir, overwrite etc/keep.txt.
	layer2 := buildTar(t, []tarEntry{
		{name: "etc/.wh.drop.txt"},
		{name: "etc/keep.txt", body: "updated\n"},
		{name: "opt/dir/.wh..wh..opq"},
		{name: "opt/dir/fresh", body: "new"},
	})
	if err := baseimage.ExtractLayer(root, layer2); err != nil {
		t.Fatalf("layer 2: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "etc/drop.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("etc/drop.txt still exists after whiteout: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "etc/keep.txt")); err != nil || string(b) != "updated\n" {
		t.Fatalf("etc/keep.txt = %q, %v; want updated", b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "opt/dir/old1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opt/dir/old1 survived opaque whiteout: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "opt/dir/fresh")); err != nil || string(b) != "new" {
		t.Fatalf("opt/dir/fresh = %q, %v; want new", b, err)
	}
	if target, err := os.Readlink(filepath.Join(root, "usr/bin/tool-sym")); err != nil || target != "tool" {
		t.Fatalf("symlink target = %q, %v; want tool", target, err)
	}
	fi, err := os.Stat(filepath.Join(root, "usr/bin/tool"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("usr/bin/tool mode = %v, %v; want 0755", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "usr/bin/tool-hard")); err != nil || !strings.Contains(string(b), "echo hi") {
		t.Fatalf("hardlink = %q, %v", b, err)
	}
}

func TestExtractLayerRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	evil := buildTar(t, []tarEntry{
		{name: "../escape.txt", body: "bad"},
	})
	if err := baseimage.ExtractLayer(root, evil); err == nil {
		t.Fatal("expected error for path traversal tar entry")
	}
	evilHardlink := buildTar(t, []tarEntry{
		{name: "ok", typeflag: tar.TypeLink, linkname: "../../etc/passwd"},
	})
	if err := baseimage.ExtractLayer(root, evilHardlink); err == nil {
		t.Fatal("expected error for path traversal hardlink")
	}
}

func TestExtractLayerRejectsSymlinkSlip(t *testing.T) {
	t.Run("regular file through absolute symlink", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "root")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		evil := buildTar(t, []tarEntry{
			{name: "sub", typeflag: tar.TypeSymlink, linkname: outside},
			{name: "sub/pwned", body: "owned"},
		})
		if err := baseimage.ExtractLayer(root, evil); err == nil {
			t.Fatal("expected error writing file through symlink")
		}
		if _, err := os.Lstat(filepath.Join(outside, "pwned")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink-slip created file outside root: %v", err)
		}
	})

	t.Run("regular file through relative symlink", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "root")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		evil := buildTar(t, []tarEntry{
			{name: "sub", typeflag: tar.TypeSymlink, linkname: "../outside"},
			{name: "sub/pwned", body: "owned"},
		})
		if err := baseimage.ExtractLayer(root, evil); err == nil {
			t.Fatal("expected error writing file through relative symlink")
		}
		if _, err := os.Lstat(filepath.Join(outside, "pwned")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("relative symlink-slip created file outside root: %v", err)
		}
	})

	t.Run("dir and symlink under symlinked parent", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "root")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		evilDir := buildTar(t, []tarEntry{
			{name: "sub", typeflag: tar.TypeSymlink, linkname: outside},
			{name: "sub/nested/", typeflag: tar.TypeDir, mode: 0o755},
		})
		if err := baseimage.ExtractLayer(root, evilDir); err == nil {
			t.Fatal("expected error creating directory through symlink")
		}
		if _, err := os.Lstat(filepath.Join(outside, "nested")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink-slip created dir outside root: %v", err)
		}

		evilSym := buildTar(t, []tarEntry{
			{name: "sub2", typeflag: tar.TypeSymlink, linkname: outside},
			{name: "sub2/link", typeflag: tar.TypeSymlink, linkname: "/etc/shadow"},
		})
		if err := baseimage.ExtractLayer(root, evilSym); err == nil {
			t.Fatal("expected error creating symlink through symlinked parent")
		}
		if _, err := os.Lstat(filepath.Join(outside, "link")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink-slip created symlink outside root: %v", err)
		}
	})

	t.Run("whiteout through symlinked parent", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "root")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(outside, "victim")
		if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
			t.Fatal(err)
		}
		evil := buildTar(t, []tarEntry{
			{name: "sub", typeflag: tar.TypeSymlink, linkname: outside},
			{name: "sub/.wh.victim"},
		})
		if err := baseimage.ExtractLayer(root, evil); err == nil {
			t.Fatal("expected error applying whiteout through symlink")
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("outside victim deleted through symlink whiteout: %v", err)
		}
	})

	t.Run("opaque whiteout through symlinked parent", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "root")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(outside, "victim")
		if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
			t.Fatal(err)
		}
		evil := buildTar(t, []tarEntry{
			{name: "sub", typeflag: tar.TypeSymlink, linkname: outside},
			{name: "sub/.wh..wh..opq"},
		})
		if err := baseimage.ExtractLayer(root, evil); err == nil {
			t.Fatal("expected error applying opaque whiteout through symlink")
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("outside victim deleted through symlink opaque whiteout: %v", err)
		}
	})

	t.Run("hardlink target through symlink or to symlink", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "root")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(outside, "secret")
		if err := os.WriteFile(victim, []byte("topsecret"), 0o600); err != nil {
			t.Fatal(err)
		}
		evilViaParent := buildTar(t, []tarEntry{
			{name: "sub", typeflag: tar.TypeSymlink, linkname: outside},
			{name: "stolen", typeflag: tar.TypeLink, linkname: "sub/secret"},
		})
		if err := baseimage.ExtractLayer(root, evilViaParent); err == nil {
			t.Fatal("expected error hardlinking through symlinked parent")
		}
		evilDirectSym := buildTar(t, []tarEntry{
			{name: "sym", typeflag: tar.TypeSymlink, linkname: victim},
			{name: "stolen2", typeflag: tar.TypeLink, linkname: "sym"},
		})
		if err := baseimage.ExtractLayer(root, evilDirectSym); err == nil {
			t.Fatal("expected error hardlinking directly to symlink")
		}
	})

	t.Run("dir replaced by symlink in same layer does not chmod target", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "root")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		layer := buildTar(t, []tarEntry{
			{name: "sub/", typeflag: tar.TypeDir, mode: 0o777},
			{name: "sub", typeflag: tar.TypeSymlink, linkname: outside},
		})
		if err := baseimage.ExtractLayer(root, layer); err != nil {
			t.Fatalf("ExtractLayer: %v", err)
		}
		fi, err := os.Stat(outside)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("outside dir permissions modified to %o, want 0700", fi.Mode().Perm())
		}
	})

	t.Run("legitimate absolute symlink and whiteout of symlink itself", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "root")
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(outside, "keep")
		if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		layer1 := buildTar(t, []tarEntry{
			{name: "usr/bin/", typeflag: tar.TypeDir, mode: 0o755},
			{name: "usr/bin/dash", body: "#!/bin/sh\n", mode: 0o755},
			{name: "usr/bin/sh", typeflag: tar.TypeSymlink, linkname: "dash"},
			{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
			{name: "etc/mtab", typeflag: tar.TypeSymlink, linkname: "/proc/mounts"},
			{name: "etc/extlink", typeflag: tar.TypeSymlink, linkname: outside},
		})
		if err := baseimage.ExtractLayer(root, layer1); err != nil {
			t.Fatalf("layer 1: %v", err)
		}
		// Whiteout of the symlink itself removes the symlink without touching outside.
		layer2 := buildTar(t, []tarEntry{
			{name: "etc/.wh.extlink"},
		})
		if err := baseimage.ExtractLayer(root, layer2); err != nil {
			t.Fatalf("layer 2: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "etc/extlink")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("etc/extlink symlink still exists after whiteout: %v", err)
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("outside file deleted when removing symlink whiteout: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(root, "usr/bin/sh")); err != nil || target != "dash" {
			t.Fatalf("usr/bin/sh = %q, %v; want dash", target, err)
		}
		if target, err := os.Readlink(filepath.Join(root, "etc/mtab")); err != nil || target != "/proc/mounts" {
			t.Fatalf("etc/mtab = %q, %v; want /proc/mounts", target, err)
		}
	})
}

func TestSpecKeyAndDefaultProvisionScript(t *testing.T) {
	a := baseimage.Spec{User: "ramon", UID: 1000, GID: 1000, Arch: "amd64"}
	b := baseimage.Spec{User: "ramon", UID: 1000, GID: 1000, Arch: "arm64"}
	c := baseimage.Spec{User: "ramon", UID: 1001, GID: 1000, Arch: "amd64"}
	if a.Key() != a.Key() {
		t.Fatal("Key() not deterministic")
	}
	if a.Key() == b.Key() || a.Key() == c.Key() {
		t.Fatalf("keys collided: a=%s b=%s c=%s", a.Key(), b.Key(), c.Key())
	}
	if !strings.HasPrefix(a.Key(), "baseimg-") || !strings.HasPrefix(a.Volume(), "baseimages/baseimg-") {
		t.Fatalf("unexpected names: key=%s vol=%s", a.Key(), a.Volume())
	}

	script := a.Normalized().Script
	for _, want := range []string{"strace", "linux-perf", "bpftrace", "elfutils", "gdb", "rustup", "rustup-init", "sha256sum -c -", "build-essential", "git", " sudo "} {
		if !strings.Contains(script, want) {
			t.Errorf("default provision script missing %q", want)
		}
	}
	if strings.Contains(script, "sh.rustup.rs") {
		t.Errorf("default provision script must not pipe unverified sh.rustup.rs to sh")
	}
	if !strings.Contains(baseimage.DefaultImage, "@sha256:") {
		t.Errorf("baseimage.DefaultImage %q must be pinned to an immutable @sha256 digest", baseimage.DefaultImage)
	}
	if _, err := name.ParseReference(baseimage.DefaultImage); err != nil {
		t.Errorf("name.ParseReference(baseimage.DefaultImage): %v", err)
	}
	if bashPath, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bashPath, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("DefaultProvisionScript failed bash -n: %v\n%s", err, out)
		}
	}
	// The user gets sudo: root of its sandbox (board 8145).
	if !strings.Contains(script, "printf '#%d ALL=(ALL:ALL) NOPASSWD: ALL\\n' 1000 > /etc/sudoers.d/hotbed-user") {
		t.Errorf("the image doesn't grant uid 1000 sudo:\n%s", script)
	}
	// /etc/mtab -> /proc/self/mounts is required for fusermount3 inside the
	// user-namespace sandbox (board 7975).
	if !strings.Contains(script, "ln -snf /proc/self/mounts /etc/mtab") {
		t.Errorf("default provision script missing /etc/mtab symlink:\n%s", script)
	}
}

func TestManagerEnsureCachesDeduplicatesAndRecovers(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	var pulls, provisions atomic.Int32
	failProvision := atomic.Bool{}
	failProvision.Store(true)

	mgr := &baseimage.Manager{
		Store: store,
		Spec:  baseimage.Spec{Image: "test:latest", Arch: "amd64", User: "ramon", UID: 1000, GID: 1000, Script: "echo ok"},
		Pull: func(_ context.Context, _, _, rootDir string) error {
			pulls.Add(1)
			return os.WriteFile(filepath.Join(rootDir, "from-image"), []byte("ok"), 0o644)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, rootDir, _ string) error {
			provisions.Add(1)
			if failProvision.Load() {
				return errors.New("boom")
			}
			return os.WriteFile(filepath.Join(rootDir, "provisioned"), []byte("yes"), 0o644)
		}),
	}
	t.Cleanup(mgr.Wait)

	// First attempt fails during provisioning and must clean up its partial volume.
	if _, _, err := mgr.Ensure(t.Context()); err == nil {
		t.Fatal("expected provision error")
	}
	if mgr.ReadyRootDir() != "" {
		t.Fatalf("ReadyRootDir = %q after failure, want empty", mgr.ReadyRootDir())
	}
	if _, err := os.Stat(store.Path(mgr.Spec.Volume())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial volume survived failure: %v", err)
	}

	// Next attempt succeeds, and concurrent callers share one pull+provision.
	failProvision.Store(false)
	var wg sync.WaitGroup
	snaps := make([]string, 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			snaps[idx], _, errs[idx] = mgr.Ensure(t.Context())
		}(i)
	}
	wg.Wait()
	for i := range 4 {
		if errs[i] != nil {
			t.Fatalf("caller %d failed: %v", i, errs[i])
		}
		if snaps[i] != mgr.Spec.Key() {
			t.Fatalf("caller %d got snapshot %q, want %q", i, snaps[i], mgr.Spec.Key())
		}
	}
	if pulls.Load() != 2 || provisions.Load() != 2 {
		t.Fatalf("pulls=%d provisions=%d, want 2 each (1 failed + 1 shared)", pulls.Load(), provisions.Load())
	}
	if mgr.ReadyRootDir() == "" {
		t.Fatal("ReadyRootDir empty after successful Ensure")
	}

	// Cloning a project base from the snapshot produces the full provisioned rootfs.
	if err := store.Clone(t.Context(), snaps[0], "bases/cladefs/g1"); err != nil {
		t.Fatalf("clone from base-image snapshot: %v", err)
	}
	for _, f := range []string{"from-image", "provisioned", "etc/resolv.conf", "work"} {
		if _, err := os.Stat(filepath.Join(store.Path("bases/cladefs/g1"), f)); err != nil {
			t.Errorf("cloned base missing %s: %v", f, err)
		}
	}
}

func TestManagerEnsureSurvivesCallerCancel(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var builds atomic.Int32
	mgr := &baseimage.Manager{
		Store: store,
		Spec:  baseimage.Spec{Image: "test:cancel", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"},
		Pull: func(_ context.Context, _, _, rootDir string) error {
			builds.Add(1)
			close(started)
			<-release
			return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
	}
	// Cleanups run LIFO: unblock the build first, then wait for the detached
	// build goroutine to finish before t.TempDir() removes the store root.
	t.Cleanup(mgr.Wait)
	t.Cleanup(unblock)

	ctx1, cancel1 := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := mgr.Ensure(ctx1)
		errCh <- err
	}()
	<-started
	cancel1()
	if err := <-errCh; err != context.Canceled {
		t.Fatalf("first caller err = %v, want context.Canceled", err)
	}

	// Second caller joins the still-running shared build and completes once released.
	unblock()
	snap, root, err := mgr.Ensure(t.Context())
	if err != nil {
		t.Fatalf("second caller Ensure: %v", err)
	}
	if snap == "" || !baseimage.MatchesKey(root, snap) {
		t.Fatalf("unexpected ready rootfs: snap=%q root=%q", snap, root)
	}
	if builds.Load() != 1 {
		t.Fatalf("builds = %d, want 1", builds.Load())
	}
}

type blockingSnapshotStore struct {
	*fake.Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockingSnapshotStore) Snapshot(ctx context.Context, vol, snap string) error {
	close(s.entered)
	<-s.release
	return s.Store.Snapshot(ctx, vol, snap)
}

// Regression test for board 9287: a second Ensure caller (and ReadyRootDir)
// must not observe the base image as ready as soon as writeReadyMarker runs
// while Store.Snapshot is still in flight; returning early raced both callers
// cloning the snapshot and t.TempDir cleanup.
func TestManagerEnsureWaitsForSnapshotAfterReadyMarker(t *testing.T) {
	raw := &fake.Store{Root: t.TempDir()}
	store := &blockingSnapshotStore{
		Store:   raw,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(store.release) }) }

	mgr := &baseimage.Manager{
		Store: store,
		Spec:  baseimage.Spec{Image: "test:snap-race", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"},
		Pull: func(_ context.Context, _, _, rootDir string) error {
			return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
	}
	t.Cleanup(mgr.Wait)
	t.Cleanup(unblock)

	ctx1, cancel1 := context.WithCancel(t.Context())
	err1Ch := make(chan error, 1)
	go func() {
		_, _, err := mgr.Ensure(ctx1)
		err1Ch <- err
	}()
	// Wait until writeReadyMarker has already run and Store.Snapshot is active.
	<-store.entered
	cancel1()
	if err := <-err1Ch; err != context.Canceled {
		t.Fatalf("first caller err = %v, want context.Canceled", err)
	}

	if got := mgr.ReadyRootDir(); got != "" {
		t.Fatalf("ReadyRootDir() = %q while Store.Snapshot is still in flight, want empty", got)
	}

	type ensureResult struct {
		snap string
		root string
		err  error
	}
	resCh := make(chan ensureResult, 1)
	go func() {
		snap, root, err := mgr.Ensure(t.Context())
		resCh <- ensureResult{snap: snap, root: root, err: err}
	}()

	select {
	case res := <-resCh:
		t.Fatalf("second Ensure returned early while Store.Snapshot was still running: %+v", res)
	case <-time.After(50 * time.Millisecond):
	}

	unblock()
	res := <-resCh
	if res.err != nil {
		t.Fatalf("second Ensure: %v", res.err)
	}
	if err := raw.Clone(t.Context(), res.snap, "bases/test/g1"); err != nil {
		t.Fatalf("clone after Ensure returned: %v", err)
	}
	if mgr.ReadyRootDir() != res.root {
		t.Fatalf("ReadyRootDir() = %q, want %q", mgr.ReadyRootDir(), res.root)
	}
}

// Wait drains a detached build left behind after its sole caller cancelled
// (board 9287), so test teardown never races t.TempDir() removal.
func TestManagerWaitDrainsCancelledBuild(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	mgr := &baseimage.Manager{
		Store: store,
		Spec:  baseimage.Spec{Image: "test:wait-drain", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"},
		Pull: func(_ context.Context, _, _, rootDir string) error {
			close(started)
			<-release
			return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error {
			finished.Store(true)
			return nil
		}),
	}

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := mgr.Ensure(ctx)
		errCh <- err
	}()
	<-started
	cancel()
	if err := <-errCh; err != context.Canceled {
		t.Fatalf("Ensure err = %v, want context.Canceled", err)
	}

	waitDone := make(chan struct{})
	go func() {
		mgr.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("Wait returned before detached build finished")
	case <-time.After(30 * time.Millisecond):
	}

	close(release)
	<-waitDone
	if !finished.Load() {
		t.Fatal("detached build did not run to completion before Wait returned")
	}
}

// A caller whose context is done before the shared build has even begun
// returns at once, and the build goes on undisturbed: the build must not
// share Ensure's results, which that return overwrites (a race the -race
// detector caught in cmd/hotbed's reach tests).
func TestManagerEnsureCallerGoneBeforeTheBuildStarts(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	release := make(chan struct{})
	mgr := &baseimage.Manager{
		Store: store,
		Spec:  baseimage.Spec{Image: "test:gone", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"},
		Pull: func(_ context.Context, _, _, rootDir string) error {
			<-release
			return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := mgr.Ensure(gone); err != context.Canceled {
		t.Fatalf("a caller already gone: err = %v, want context.Canceled", err)
	}
	close(release)
	snap, root, err := mgr.Ensure(t.Context())
	if err != nil || !baseimage.MatchesKey(root, snap) {
		t.Fatalf("the build after: snap=%q root=%q err=%v", snap, root, err)
	}
}

// leftoverProvisioner is a provisioning an earlier worker process started
// and left running: it holds the image's volume busy until abandoned.
type leftoverProvisioner struct {
	store     *fake.Store
	vol       string
	abandoned atomic.Bool
}

func (p *leftoverProvisioner) Provision(_ context.Context, _, rootDir, _ string) error {
	return os.WriteFile(filepath.Join(rootDir, "provisioned"), []byte("yes"), 0o644)
}

func (p *leftoverProvisioner) Abandon(context.Context, string) error {
	p.abandoned.Store(true)
	p.store.Let(p.vol)
	return nil
}

// A build stops the provisioning a previous worker process left running
// before it clears that build's volume, which it holds busy (board 7276).
func TestManagerEnsureAbandonsALeftoverProvisioning(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	spec := baseimage.Spec{Image: "test:leftover", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"}
	vol := spec.Normalized().Volume()
	if err := store.Create(t.Context(), vol); err != nil {
		t.Fatal(err)
	}
	store.Hold(vol)
	prov := &leftoverProvisioner{store: store, vol: vol}
	mgr := &baseimage.Manager{
		Store: store,
		Spec:  spec,
		Pull: func(_ context.Context, _, _, rootDir string) error {
			return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
		},
		Provision: prov,
	}
	t.Cleanup(mgr.Wait)
	snap, _, err := mgr.Ensure(t.Context())
	if err != nil {
		t.Fatalf("Ensure with a leftover provisioning holding the volume: %v", err)
	}
	if !prov.abandoned.Load() {
		t.Error("the leftover provisioning was not abandoned")
	}
	if snap != spec.Normalized().Key() {
		t.Errorf("snapshot = %q, want %q", snap, spec.Normalized().Key())
	}
}

// Writing the ready marker must stay inside rootDir via os.Root and never
// follow an image-supplied symlink outside rootDir; reading it must reject
// symlinks, FIFOs, and oversized files without blocking (board 8876).
func TestReadyMarkerSymlinkAndSpecialFileSafety(t *testing.T) {
	t.Run("Ensure replaces image-supplied symlink without touching outside victim", func(t *testing.T) {
		parent := t.TempDir()
		store := &fake.Store{Root: filepath.Join(parent, "store")}
		outside := filepath.Join(parent, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(outside, "victim")
		if err := os.WriteFile(victim, []byte("precious-victim\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		mgr := &baseimage.Manager{
			Store: store,
			Spec:  baseimage.Spec{Image: "test:symlink-marker", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"},
			Pull: func(_ context.Context, _, _, rootDir string) error {
				layer := buildTar(t, []tarEntry{
					{name: "usr/bin/", typeflag: tar.TypeDir, mode: 0o755},
					{name: baseimage.ReadyMarker, typeflag: tar.TypeSymlink, linkname: victim},
				})
				return baseimage.ExtractLayer(rootDir, layer)
			},
			Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
		}
		t.Cleanup(mgr.Wait)

		snap, root, err := mgr.Ensure(t.Context())
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if got, err := os.ReadFile(victim); err != nil || string(got) != "precious-victim\n" {
			t.Fatalf("outside victim modified: %q, err=%v", got, err)
		}
		fi, err := os.Lstat(filepath.Join(root, baseimage.ReadyMarker))
		if err != nil {
			t.Fatalf("Lstat ready marker: %v", err)
		}
		if !fi.Mode().IsRegular() {
			t.Fatalf("ready marker mode = %v, want regular file", fi.Mode())
		}
		if !baseimage.MatchesKey(root, snap) {
			t.Fatalf("baseimage.MatchesKey(%q, %q) = false, want true", root, snap)
		}
	})

	t.Run("WriteReadyMarker replaces pre-existing directory", func(t *testing.T) {
		root := t.TempDir()
		dirMarker := filepath.Join(root, baseimage.ReadyMarker)
		if err := os.MkdirAll(filepath.Join(dirMarker, "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := baseimage.WriteReadyMarker(root, "baseimg-dirtest"); err != nil {
			t.Fatalf("baseimage.WriteReadyMarker: %v", err)
		}
		if !baseimage.MatchesKey(root, "baseimg-dirtest") {
			t.Fatal("MatchesKey returned false after replacing dir marker")
		}
	})

	t.Run("readers reject symlink, FIFO, and oversized marker without blocking", func(t *testing.T) {
		parent := t.TempDir()
		outside := filepath.Join(parent, "outside-key")
		if err := os.WriteFile(outside, []byte("baseimg-outside\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		symRoot := filepath.Join(parent, "sym-root")
		if err := os.MkdirAll(symRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(symRoot, baseimage.ReadyMarker)); err != nil {
			t.Fatal(err)
		}
		if baseimage.MatchesKey(symRoot, "baseimg-outside") {
			t.Fatal("MatchesKey followed symlink ready marker")
		}
		if _, err := baseimage.ReadReadyMarker(symRoot); err == nil {
			t.Fatal("baseimage.ReadReadyMarker followed symlink ready marker")
		}

		fifoRoot := filepath.Join(parent, "fifo-root")
		if err := os.MkdirAll(fifoRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(fifoRoot, baseimage.ReadyMarker), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if baseimage.MatchesKey(fifoRoot, "baseimg-fifo") {
				t.Error("MatchesKey accepted FIFO ready marker")
			}
			if _, err := baseimage.ReadReadyMarker(fifoRoot); err == nil {
				t.Error("baseimage.ReadReadyMarker accepted FIFO ready marker")
			}
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("reading FIFO ready marker blocked")
		}

		bigRoot := filepath.Join(parent, "big-root")
		if err := os.MkdirAll(bigRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		big := strings.Repeat("a", baseimage.MaxReadyMarkerBytes+16)
		if err := os.WriteFile(filepath.Join(bigRoot, baseimage.ReadyMarker), []byte(big), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := baseimage.ReadReadyMarker(bigRoot); err == nil {
			t.Fatal("baseimage.ReadReadyMarker accepted oversized ready marker")
		}
		if baseimage.MatchesKey(bigRoot, big) {
			t.Fatal("MatchesKey accepted oversized ready marker")
		}
	})
}

func TestParseSubmounts(t *testing.T) {
	mountinfo := strings.Join([]string{
		"100 20 0:42 / /var/lib/hotbed/volumes/baseimages/baseimg-abc rw,nosuid - zfs rpool/hotbed/baseimages/baseimg-abc rw",
		"101 100 0:4 / /var/lib/hotbed/volumes/baseimages/baseimg-abc/proc rw - proc proc rw",
		"102 100 0:5 / /var/lib/hotbed/volumes/baseimages/baseimg-abc/dev rw - devtmpfs devtmpfs rw",
		"103 102 0:6 / /var/lib/hotbed/volumes/baseimages/baseimg-abc/dev/pts rw - devpts devpts rw",
		"104 100 0:7 / /var/lib/hotbed/volumes/baseimages/baseimg-abc/etc/resolv.conf ro - tmpfs tmpfs ro",
		"105 100 0:8 / /var/lib/hotbed/volumes/baseimages/baseimg-abc/mnt/my\\040dir rw - tmpfs tmpfs rw",
		// Sibling with matching string prefix must NOT match.
		"106 20 0:9 / /var/lib/hotbed/volumes/baseimages/baseimg-abc-other/proc rw - proc proc rw",
	}, "\n")

	got := baseimage.ParseSubmounts(mountinfo, "/var/lib/hotbed/volumes/baseimages/baseimg-abc")
	want := []string{
		"/var/lib/hotbed/volumes/baseimages/baseimg-abc/etc/resolv.conf",
		"/var/lib/hotbed/volumes/baseimages/baseimg-abc/mnt/my dir",
		"/var/lib/hotbed/volumes/baseimages/baseimg-abc/dev/pts",
		"/var/lib/hotbed/volumes/baseimages/baseimg-abc/proc",
		"/var/lib/hotbed/volumes/baseimages/baseimg-abc/dev",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseSubmounts =\n  %q\nwant:\n  %q", got, want)
	}
	if got := baseimage.ParseSubmounts(mountinfo, "/"); len(got) != 0 {
		t.Fatalf("baseimage.ParseSubmounts(\"/\") = %q, want empty", got)
	}
	if got := baseimage.ParseSubmounts(mountinfo, ""); len(got) != 0 {
		t.Fatalf("baseimage.ParseSubmounts(\"\") = %q, want empty", got)
	}

	// Symlinked rootDir resolves to its canonical target before matching mountinfo.
	realDir := filepath.Join(t.TempDir(), "real", "baseimg-sym")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalReal, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "link-baseimg")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	symMountinfo := strings.Join([]string{
		"200 20 0:42 / " + canonicalReal + " rw - zfs rpool/hotbed/baseimages/baseimg-sym rw",
		"201 200 0:4 / " + canonicalReal + "/proc rw - proc proc rw",
		"202 200 0:5 / " + canonicalReal + "/dev/pts rw - devpts devpts rw",
	}, "\n")
	gotSym := baseimage.ParseSubmounts(symMountinfo, linkDir)
	wantSym := []string{
		canonicalReal + "/dev/pts",
		canonicalReal + "/proc",
	}
	if !slices.Equal(gotSym, wantSym) {
		t.Fatalf("parseSubmounts via symlink = %q, want %q", gotSym, wantSym)
	}
}

type cancellingProvisioner struct {
	store          *fake.Store
	vol            string
	block          atomic.Bool
	abandonCtxLive atomic.Bool
	abandons       atomic.Int32
}

func (p *cancellingProvisioner) Provision(ctx context.Context, _, rootDir, _ string) error {
	if p.block.Load() {
		// Simulate an active bind mount holding the volume busy while provisioning runs.
		p.store.Hold(p.vol)
		<-ctx.Done()
		return ctx.Err()
	}
	return os.WriteFile(filepath.Join(rootDir, "provisioned"), []byte("yes"), 0o644)
}

func (p *cancellingProvisioner) Abandon(ctx context.Context, _ string) error {
	p.abandons.Add(1)
	if ctx.Err() == nil {
		p.abandonCtxLive.Store(true)
	}
	return nil
}

type transientBusyStore struct {
	*fake.Store
	busyRemaining atomic.Int32
	destroyCalls  atomic.Int32
}

func (s *transientBusyStore) Destroy(ctx context.Context, name string) error {
	s.destroyCalls.Add(1)
	if s.busyRemaining.Add(-1) >= 0 {
		return storage.ErrBusy
	}
	return s.Store.Destroy(ctx, name)
}

// Mid-flight build cancellation/timeout must abandon provisioning, unmount
// leftover submounts, and destroy the partial volume using context.WithoutCancel,
// and a transiently busy leftover volume must be retried cleanly (board 7965).
func TestManagerEnsureCleansUpInterruptedBuildAndBusyLeftover(t *testing.T) {
	t.Run("mid-flight build timeout cleans up mounts and partial volume", func(t *testing.T) {
		store := &fake.Store{Root: t.TempDir()}
		spec := baseimage.Spec{Image: "test:cancel-cleanup", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"}
		vol := spec.Normalized().Volume()
		prov := &cancellingProvisioner{store: store, vol: vol}
		prov.block.Store(true)

		var unmountCalls atomic.Int32
		mgr := &baseimage.Manager{
			Store:        store,
			Spec:         spec,
			BuildTimeout: 50 * time.Millisecond,
			Pull: func(_ context.Context, _, _, rootDir string) error {
				return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
			},
			Provision: prov,
		}
		t.Cleanup(mgr.Wait)
		mgr.SetTestHooks(func(rootDir string) error {
			unmountCalls.Add(1)
			// Unmounting the leftover bind mount releases the busy hold on the volume.
			if rootDir == store.Path(vol) {
				store.Let(vol)
			}
			return nil
		}, 5*time.Millisecond)

		if _, _, err := mgr.Ensure(t.Context()); err == nil {
			t.Fatal("expected build timeout error")
		}
		if !prov.abandonCtxLive.Load() {
			t.Error("Abandon during cancellation cleanup did not receive a non-cancelled context")
		}
		if unmountCalls.Load() == 0 {
			t.Error("unmount hook was not called during cleanup")
		}
		if _, err := os.Stat(store.Path(vol)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial volume survived interrupted build: %v", err)
		}

		// Subsequent Ensure succeeds cleanly.
		prov.block.Store(false)
		mgr.BuildTimeout = 5 * time.Second
		snap, root, err := mgr.Ensure(t.Context())
		if err != nil {
			t.Fatalf("subsequent Ensure after interrupted build: %v", err)
		}
		if snap != spec.Normalized().Key() || !baseimage.MatchesKey(root, snap) {
			t.Fatalf("unexpected ready rootfs: snap=%q root=%q", snap, root)
		}
	})

	t.Run("recovers from transiently busy leftover volume", func(t *testing.T) {
		raw := &fake.Store{Root: t.TempDir()}
		store := &transientBusyStore{Store: raw}
		spec := baseimage.Spec{Image: "test:transient-busy", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"}
		vol := spec.Normalized().Volume()
		if err := raw.Create(t.Context(), vol); err != nil {
			t.Fatal(err)
		}
		// Simulate 2 transient ErrBusy responses while kernel tears down mounts.
		store.busyRemaining.Store(2)

		var unmountCalls atomic.Int32
		mgr := &baseimage.Manager{
			Store: store,
			Spec:  spec,
			Pull: func(_ context.Context, _, _, rootDir string) error {
				return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
			},
			Provision: baseimage.ProvisionFunc(func(_ context.Context, rootDir, _ string) error {
				return os.WriteFile(filepath.Join(rootDir, "provisioned"), []byte("yes"), 0o644)
			}),
		}
		t.Cleanup(mgr.Wait)
		mgr.SetTestHooks(func(string) error {
			unmountCalls.Add(1)
			return nil
		}, 5*time.Millisecond)

		snap, root, err := mgr.Ensure(t.Context())
		if err != nil {
			t.Fatalf("Ensure with transiently busy leftover volume: %v", err)
		}
		if snap != spec.Normalized().Key() || !baseimage.MatchesKey(root, snap) {
			t.Fatalf("unexpected ready rootfs: snap=%q root=%q", snap, root)
		}
		if store.destroyCalls.Load() < 3 {
			t.Errorf("destroyCalls = %d, want >= 3 (2 ErrBusy retries + 1 success)", store.destroyCalls.Load())
		}
		if unmountCalls.Load() < 3 {
			t.Errorf("unmountCalls = %d, want >= 3 (initial + before each retry)", unmountCalls.Load())
		}
	})
}

func TestSpecValidateRejectsRoot(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	for _, tc := range []struct {
		name string
		spec baseimage.Spec
	}{
		{"root user with uid 0", baseimage.Spec{User: "root", UID: 0, GID: 0}},
		{"root user with non-zero uid", baseimage.Spec{User: "root", UID: 1000, GID: 1000}},
		{"explicit user with uid 0", baseimage.Spec{User: "toor", UID: 0, GID: 1000}},
		{"negative uid", baseimage.Spec{User: "hotbed", UID: -1, GID: 1000}},
		{"negative gid", baseimage.Spec{User: "hotbed", UID: 1000, GID: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.spec.Validate(); err == nil {
				t.Fatalf("Spec(%+v).Validate() = nil, want error", tc.spec)
			}
			mgr := &baseimage.Manager{
				Store: store,
				Spec:  tc.spec,
				Pull: func(context.Context, string, string, string) error {
					t.Fatal("Pull must not be called for invalid spec")
					return nil
				},
				Provision: baseimage.ProvisionFunc(func(context.Context, string, string) error {
					t.Fatal("Provision must not be called for invalid spec")
					return nil
				}),
			}
			if _, _, err := mgr.Ensure(t.Context()); err == nil {
				t.Fatalf("Ensure(%+v) = nil, want error", tc.spec)
			}
		})
	}
	if err := (baseimage.Spec{}).Validate(); err != nil {
		t.Fatalf("zero Spec.Validate() = %v, want nil", err)
	}
	if err := (baseimage.Spec{User: "hotbed", UID: 1000, GID: 1000}).Validate(); err != nil {
		t.Fatalf("non-root Spec.Validate() = %v, want nil", err)
	}
}

type countingSnapshotStore struct {
	*fake.Store
	listCalls atomic.Int32
	onSnap    func()
	snapped   atomic.Bool
}

func (s *countingSnapshotStore) ListSnapshots(ctx context.Context) ([]string, error) {
	s.listCalls.Add(1)
	return s.Store.ListSnapshots(ctx)
}

func (s *countingSnapshotStore) Snapshot(ctx context.Context, vol, snap string) error {
	if s.onSnap != nil {
		s.onSnap()
	}
	err := s.Store.Snapshot(ctx, vol, snap)
	if err == nil {
		s.snapped.Store(true)
	}
	return err
}

// A crash between writeReadyMarker and Store.Snapshot leaves a ready marker on
// disk whose backing snapshot does not exist. On recovery, ReadyRootDir must
// report "" and Ensure must rebuild cleanly instead of returning a missing
// snapshot. Once verified on cold start, snapshot existence is cached in memory.
func TestManagerRebuildsWhenReadyMarkerLacksSnapshot(t *testing.T) {
	raw := &fake.Store{Root: t.TempDir()}
	store := &countingSnapshotStore{Store: raw}
	spec := baseimage.Spec{Image: "test:missing-snap", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"}
	key := spec.Key()
	vol := spec.Volume()

	// Simulate a crash after writeReadyMarker ran, before Store.Snapshot completed.
	if err := raw.Create(t.Context(), vol); err != nil {
		t.Fatal(err)
	}
	if err := baseimage.WriteReadyMarker(raw.Path(vol), key); err != nil {
		t.Fatal(err)
	}

	var pulls, provisions atomic.Int32
	mgr := &baseimage.Manager{
		Store: store,
		Spec:  spec,
		Pull: func(_ context.Context, _, _, rootDir string) error {
			pulls.Add(1)
			return os.WriteFile(filepath.Join(rootDir, "fresh-pull"), []byte("ok"), 0o644)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, rootDir, _ string) error {
			provisions.Add(1)
			return os.WriteFile(filepath.Join(rootDir, "fresh-prov"), []byte("ok"), 0o644)
		}),
	}
	t.Cleanup(mgr.Wait)

	if got := mgr.ReadyRootDir(); got != "" {
		t.Fatalf("ReadyRootDir() = %q with missing snapshot, want empty", got)
	}

	snap, root, err := mgr.Ensure(t.Context())
	if err != nil {
		t.Fatalf("Ensure with leftover ready marker and missing snapshot: %v", err)
	}
	if snap != key {
		t.Fatalf("snap = %q, want %q", snap, key)
	}
	if pulls.Load() != 1 || provisions.Load() != 1 {
		t.Fatalf("pulls=%d provisions=%d, want 1 each (must rebuild when snapshot missing)", pulls.Load(), provisions.Load())
	}
	if err := raw.Clone(t.Context(), snap, "bases/recovered/g1"); err != nil {
		t.Fatalf("clone after recovery Ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(raw.Path("bases/recovered/g1"), "fresh-prov")); err != nil {
		t.Fatalf("cloned base missing rebuilt file: %v", err)
	}
	if got := mgr.ReadyRootDir(); got != root {
		t.Fatalf("ReadyRootDir() = %q after rebuild, want %q", got, root)
	}

	// Cold restart against the now-complete store verifies snapshot existence
	// once and caches it for subsequent hot-path calls without rebuilding.
	store.listCalls.Store(0)
	coldMgr := &baseimage.Manager{
		Store: store,
		Spec:  spec,
		Pull: func(context.Context, string, string, string) error {
			t.Fatal("cold restart unexpectedly rebuilt ready base image")
			return nil
		},
		Provision: baseimage.ProvisionFunc(func(context.Context, string, string) error {
			t.Fatal("cold restart unexpectedly reprovisioned ready base image")
			return nil
		}),
	}
	t.Cleanup(coldMgr.Wait)

	if got := coldMgr.ReadyRootDir(); got != root {
		t.Fatalf("cold ReadyRootDir() = %q, want %q", got, root)
	}
	if got := coldMgr.ReadyRootDir(); got != root {
		t.Fatalf("second cold ReadyRootDir() = %q, want %q", got, root)
	}
	if gotSnap, gotRoot, err := coldMgr.Ensure(t.Context()); err != nil || gotSnap != key || gotRoot != root {
		t.Fatalf("cold Ensure() = (%q, %q, %v), want (%q, %q, nil)", gotSnap, gotRoot, err, key, root)
	}
	if calls := store.listCalls.Load(); calls != 1 {
		t.Fatalf("ListSnapshots calls on cold restart + hot path = %d, want 1", calls)
	}
}

// Concurrent ReadyRootDir and Ensure calls must not race or suffer a TOCTOU
// where ReadyRootDir observes a ready marker before Store.Snapshot completes.
func TestManagerConcurrentReadyRootDirAndEnsure(t *testing.T) {
	spec := baseimage.Spec{Image: "test:concurrent-ready", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo ok"}

	for range 12 {
		raw := &fake.Store{Root: t.TempDir()}
		store := &countingSnapshotStore{
			Store: raw,
			onSnap: func() {
				// Widen the window between writeReadyMarker and Store.Snapshot
				// so concurrent ReadyRootDir readers hammer right through it.
				time.Sleep(2 * time.Millisecond)
			},
		}
		mgr := &baseimage.Manager{
			Store: store,
			Spec:  spec,
			Pull: func(_ context.Context, _, _, rootDir string) error {
				return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
			},
			Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						if dir := mgr.ReadyRootDir(); dir != "" {
							if !store.snapped.Load() {
								t.Errorf("ReadyRootDir() = %q before Store.Snapshot completed", dir)
								return
							}
						}
					}
				}
			}()
		}

		var ensureWG sync.WaitGroup
		for range 2 {
			ensureWG.Add(1)
			go func() {
				defer ensureWG.Done()
				if _, _, err := mgr.Ensure(t.Context()); err != nil {
					t.Errorf("Ensure: %v", err)
				}
			}()
		}
		ensureWG.Wait()
		close(stop)
		wg.Wait()
		mgr.Wait()

		if got := mgr.ReadyRootDir(); got != raw.Path(spec.Volume()) {
			t.Fatalf("ReadyRootDir() after Ensure = %q, want %q", got, raw.Path(spec.Volume()))
		}
	}
}
