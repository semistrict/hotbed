package cfbundle

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func write(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureBuildsOnlyAMissingOrStaleBundle(t *testing.T) {
	root := t.TempDir()
	base := time.Unix(1795300000, 0)
	for _, src := range []string{"src/index.ts", "drizzle/0001.sql", "package.json", "pnpm-lock.yaml"} {
		write(t, filepath.Join(root, "cf-controller", src), base)
	}
	builds := 0
	build := func(controller string) error {
		if controller != filepath.Join(root, "cf-controller") {
			t.Fatalf("built %s, want the repo's cf-controller", controller)
		}
		builds++
		write(t, filepath.Join(root, Dir, "index.js"), base.Add(time.Duration(builds)*time.Hour))
		return nil
	}

	dir, err := ensure(root, build)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, Dir) || builds != 1 {
		t.Fatalf("missing bundle: dir %s, %d builds; want %s built once", dir, builds, filepath.Join(root, Dir))
	}
	if _, err := ensure(root, build); err != nil || builds != 1 {
		t.Fatalf("fresh bundle: %d builds (err %v), want no rebuild", builds, err)
	}
	// What tools write, and the dependencies, are no reason to rebuild.
	write(t, filepath.Join(root, "cf-controller", "node_modules", "x", "index.js"), base.Add(5*time.Hour))
	write(t, filepath.Join(root, "cf-controller", ".wrangler", "state.json"), base.Add(5*time.Hour))
	if _, err := ensure(root, build); err != nil || builds != 1 {
		t.Fatalf("only dependencies and tool state newer: %d builds (err %v), want no rebuild", builds, err)
	}
	write(t, filepath.Join(root, "cf-controller", "src", "gen", "hotbed_pb.ts"), base.Add(90*time.Minute))
	if _, err := ensure(root, build); err != nil || builds != 2 {
		t.Fatalf("source newer than the bundle: %d builds (err %v), want a rebuild", builds, err)
	}
	// Any build input counts, not only src: the lockfile, the build config.
	write(t, filepath.Join(root, "cf-controller", "pnpm-lock.yaml"), base.Add(3*time.Hour))
	if _, err := ensure(root, build); err != nil || builds != 3 {
		t.Fatalf("lockfile newer than the bundle: %d builds (err %v), want a rebuild", builds, err)
	}
}

// Two harnesses finding the bundle stale at once build it once: the second
// waits for the first's build, rather than clear the bundle under it, and
// then finds it fresh.
func TestEnsureBuildsOnceAcrossConcurrentCallers(t *testing.T) {
	root := t.TempDir()
	base := time.Unix(1795300000, 0)
	write(t, filepath.Join(root, "cf-controller", "src", "index.ts"), base)

	var secondBuilt atomic.Bool
	secondDone := make(chan error, 1)
	first := func(string) error {
		// While this build runs, another caller finds the bundle missing.
		go func() {
			_, err := ensure(root, func(string) error {
				secondBuilt.Store(true)
				write(t, filepath.Join(root, Dir, "index.js"), base.Add(2*time.Hour))
				return nil
			})
			secondDone <- err
		}()
		// It must wait for this build: its finishing first means it ran beside it.
		select {
		case err := <-secondDone:
			t.Fatalf("a second ensure finished during the first's build (err %v): it must wait for the build lock", err)
		case <-time.After(200 * time.Millisecond):
		}
		write(t, filepath.Join(root, Dir, "index.js"), base.Add(time.Hour))
		return nil
	}
	if _, err := ensure(root, first); err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if secondBuilt.Load() {
		t.Fatal("the second caller built again after the first's build: it must look again once it holds the lock")
	}
}
