package cfbundle

import (
	"os"
	"path/filepath"
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
	for _, src := range []string{"src/index.ts", "drizzle/0001.sql", "package.json", "cloudflare.config.ts"} {
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
	write(t, filepath.Join(root, "cf-controller", "src", "gen", "hotbed_pb.ts"), base.Add(90*time.Minute))
	if _, err := ensure(root, build); err != nil || builds != 2 {
		t.Fatalf("source newer than the bundle: %d builds (err %v), want a rebuild", builds, err)
	}
}
