//go:build linux && storagetest

// These tests need root and a real filesystem. Run them on a worker:
//
//	sudo HOTBED_TEST_BTRFS_DIR=/path/on/btrfs go test -tags storagetest -run Btrfs ./internal/storage/
//	sudo HOTBED_TEST_ZFS_DATASET=rpool/hotbed-test HOTBED_TEST_ZFS_MOUNT=/tmp/hotbed-zfs-test \
//	    go test -tags storagetest -run ZFS ./internal/storage/
package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBtrfs(t *testing.T) {
	dir := os.Getenv("HOTBED_TEST_BTRFS_DIR")
	if dir == "" {
		t.Fatal("set HOTBED_TEST_BTRFS_DIR to a directory on btrfs")
	}
	root, err := os.MkdirTemp(dir, "hotbed-store-test-")
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewBtrfs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context is cancelled by the time cleanups run.
		ctx := context.WithoutCancel(t.Context())
		for _, v := range []string{"bases/app/g1", "bases/app/g2", "ws/ws-1", "ws/ws-2", "ws/ws-3"} {
			if err := s.Destroy(ctx, v); err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("clean up %s: %v", v, err)
			}
		}
		for _, sn := range []string{"app-g1", "app-g2"} {
			if err := s.DestroySnapshot(ctx, sn); err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("clean up snapshot %s: %v", sn, err)
			}
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("clean up %s: %v", root, err)
		}
	})
	conformance(t, s)
}

func TestZFS(t *testing.T) {
	ds, mount := os.Getenv("HOTBED_TEST_ZFS_DATASET"), os.Getenv("HOTBED_TEST_ZFS_MOUNT")
	if ds == "" || mount == "" {
		t.Fatal("set HOTBED_TEST_ZFS_DATASET (a dataset that does not exist yet) and HOTBED_TEST_ZFS_MOUNT")
	}
	s, err := NewZFS(t.Context(), ds, mount)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := run(context.WithoutCancel(t.Context()), "zfs", "destroy", "-r", ds); err != nil {
			t.Errorf("clean up %s: %v", ds, err)
		}
	})
	conformance(t, s)
}

// conformance is what hotbed needs of any store: a base, built and frozen,
// forked into workspaces that diverge, a newer base built from the old one,
// and the old one deleted while its descendants live on.
func conformance(t *testing.T, s Store) {
	ctx := t.Context()
	write := func(vol, file, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(s.Path(vol), file), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(vol, file string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(s.Path(vol), file))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Generation 1: built, then frozen.
	must(s.Create(ctx, "bases/app/g1"))
	// Random, so compression (zfs's default on many pools) cannot shrink it.
	big := make([]byte, 8<<20)
	rand.Read(big)
	must(os.WriteFile(filepath.Join(s.Path("bases/app/g1"), "target.bin"), big, 0o644))
	write("bases/app/g1", "main.rs", "fn main() {}")
	must(s.Snapshot(ctx, "bases/app/g1", "app-g1"))
	if err := s.Snapshot(ctx, "bases/app/g1", "app-g1"); !errors.Is(err, ErrExists) {
		t.Errorf("second snapshot with the same name = %v, want ErrExists", err)
	}

	// Two workspaces fork it and diverge.
	must(s.Clone(ctx, "app-g1", "ws/ws-1"))
	must(s.Clone(ctx, "app-g1", "ws/ws-2"))
	write("ws/ws-1", "main.rs", "fn main() { one() }")
	if got := read("ws/ws-2", "main.rs"); got != "fn main() {}" {
		t.Errorf("ws-2 sees ws-1's edit: %q", got)
	}
	if got := read("bases/app/g1", "main.rs"); got != "fn main() {}" {
		t.Errorf("base sees a workspace's edit: %q", got)
	}

	// A fork shares the base's blocks: it holds the 8 MiB, owns almost none.
	u, err := s.Usage(ctx, "ws/ws-2")
	must(err)
	if u.Referenced < 8<<20 {
		t.Errorf("ws-2 references %d bytes, want at least the base's 8 MiB", u.Referenced)
	}
	if u.Exclusive > 1<<20 {
		t.Errorf("ws-2 owns %d bytes, want under 1 MiB: it copied the base", u.Exclusive)
	}

	// Generation 2 is built from generation 1, then generation 1 goes while
	// its workspaces and generation 2 still use its snapshot.
	must(s.Clone(ctx, "app-g1", "bases/app/g2"))
	write("bases/app/g2", "main.rs", "fn main() { two() }")
	must(s.Snapshot(ctx, "bases/app/g2", "app-g2"))
	must(s.Destroy(ctx, "bases/app/g1"))
	if got := read("ws/ws-2", "main.rs"); got != "fn main() {}" {
		t.Errorf("ws-2 after its base volume went: %q", got)
	}
	if got := read("bases/app/g2", "main.rs"); got != "fn main() { two() }" {
		t.Errorf("g2 after g1 went: %q", got)
	}
	must(s.Clone(ctx, "app-g2", "ws/ws-3"))
	if got := read("ws/ws-3", "main.rs"); got != "fn main() { two() }" {
		t.Errorf("fork of g2: %q", got)
	}

	// Once nothing uses generation 1's snapshot it can go.
	must(s.Destroy(ctx, "ws/ws-1"))
	must(s.Destroy(ctx, "ws/ws-2"))
	must(s.Destroy(ctx, "ws/ws-3"))
	sp, err := s.Space(ctx)
	must(err)
	if sp.Size <= 0 || sp.Free <= 0 || sp.Free > sp.Size {
		t.Errorf("space = %+v", sp)
	}
	if _, err := s.Usage(ctx, "ws/ws-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("usage of destroyed volume = %v, want ErrNotFound", err)
	}
}
