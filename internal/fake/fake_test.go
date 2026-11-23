package fake

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/hotbed/internal/unit"
)

func TestRewriteBindsSinglePass(t *testing.T) {
	binds := []unit.Bind{
		{Source: "/tmp/test/vol/work", Target: "/work"},
		{Source: "/other/dir", Target: "/data"},
	}
	cases := []struct {
		in, want string
	}{
		{"/work", "/tmp/test/vol/work"},
		{"/work/app", "/tmp/test/vol/work/app"},
		{"/worker/app", "/worker/app"},
		{"PATH=/work/.hotbed/bin:/data/bin", "PATH=/tmp/test/vol/work/.hotbed/bin:/other/dir/bin"},
		{"git clone '/tmp/origin' '/work/app'", "git clone '/tmp/origin' '/tmp/test/vol/work/app'"},
	}
	for _, tc := range cases {
		if got := rewriteBinds(binds, tc.in); got != tc.want {
			t.Errorf("rewriteBinds(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUnitsStartWithTmpRootDirAndTmp(t *testing.T) {
	// Reproduce Linux CI where t.TempDir() lives under /tmp: RootDir and host
	// fixture paths must not double-rewrite or corrupt /tmp/*, and the
	// command's Tmp is its TMPDIR.
	base, err := os.MkdirTemp("/tmp", "hotbed-fake-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	rootDir := filepath.Join(base, "rootfs")
	workDir := filepath.Join(rootDir, "work")
	tmpHost := filepath.Join(base, "cmdtmp")
	fixture := filepath.Join(base, "fixture.txt")
	logPath := filepath.Join(base, "cmd.log")
	for _, d := range []string{workDir, tmpHost} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(fixture, []byte("from-host-tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	u := &Units{}
	err = u.Start(t.Context(), unit.Spec{
		Name:    "hotbed-c1",
		RootDir: rootDir,
		Dir:     "/work",
		Tmp:     tmpHost,
		Argv:    []string{"/bin/sh", "-c", `printf '%s|%s|%s\n' "$PWD" "$TMPDIR" "$(cat "$1")"`, "sh", fixture},
		Log:     logPath,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := u.Wait(t.Context(), "", "hotbed-c1"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	out, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Resolve symlinks on macOS (/tmp -> /private/tmp) when comparing $PWD.
	wantWork, _ := filepath.EvalSymlinks(workDir)
	got := strings.TrimSpace(string(out))
	want := wantWork + "|" + tmpHost + "|from-host-tmp"
	if got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// Freezing, thawing and stopping a unit race its process's exit, as the
// worker's do when an agent waits on a command that is just ending: each
// sees the unit whole, done or not, and never fails for it having just
// ended (board 7098). Run with -race.
func TestFreezeThawAndStopRaceTheUnitsExit(t *testing.T) {
	u := &Units{}
	dir := t.TempDir()
	for i := range 40 {
		name := fmt.Sprintf("u%d", i)
		if err := u.Start(t.Context(), unit.Spec{Name: name, RootDir: dir, Argv: []string{"true"}, Log: filepath.Join(dir, name+".log")}); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() {
			if err := u.Wait(t.Context(), "", name); err != nil {
				t.Error(err)
			}
			close(exited)
		}()
		// Thaw and look at it over and over until it has exited, so some
		// of them meet its exit.
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for {
					select {
					case <-exited:
						return
					default:
					}
					if err := u.freeze(name, false); err != nil {
						t.Errorf("thaw %s as it exits: %v", name, err)
						return
					}
					if _, err := u.Status(t.Context(), name); err != nil {
						t.Errorf("status of %s as it exits: %v", name, err)
						return
					}
				}
			})
		}
		wg.Wait()
		if err := u.Stop(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
}

// Close kills every running unit (including child processes in its process
// group) and waits for all processes and reaper goroutines to exit before
// returning, so t.TempDir's RemoveAll never races live units (board 7995).
func TestUnitsCloseStopsRunningUnitsAndProcessGroups(t *testing.T) {
	u := &Units{}
	dir := t.TempDir()
	marker := filepath.Join(dir, "alive")
	// Spawn a shell that forks a background child writing repeatedly into dir.
	if err := u.Start(t.Context(), unit.Spec{
		Name:    "hotbed-bg",
		RootDir: dir,
		Argv:    []string{"/bin/sh", "-c", `(while true; do printf x >> "$1"; sleep 0.01; done) & wait`, "sh", marker},
		Log:     filepath.Join(dir, "bg.log"),
	}); err != nil {
		t.Fatal(err)
	}
	u.Close()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll after Units.Close: %v", err)
	}
	if err := u.Start(t.Context(), unit.Spec{
		Name:    "hotbed-after",
		RootDir: t.TempDir(),
		Argv:    []string{"true"},
		Log:     filepath.Join(t.TempDir(), "after.log"),
	}); err == nil {
		t.Fatal("Start succeeded after Close, want error")
	}
}

func TestStoreListVolumesAndSnapshots(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := &Store{Root: root}

	if vols, err := s.ListVolumes(ctx); err != nil || len(vols) != 0 {
		t.Fatalf("empty ListVolumes = %v, %v", vols, err)
	}
	if snaps, err := s.ListSnapshots(ctx); err != nil || len(snaps) != 0 {
		t.Fatalf("empty ListSnapshots = %v, %v", snaps, err)
	}

	if err := s.Create(ctx, "bases/app/g1"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Path("bases/app/g1"), "work", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(ctx, "bases/app/g1", "app-g1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Clone(ctx, "app-g1", "ws/w-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "baseimages/baseimg-abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(ctx, "baseimages/baseimg-abc", "baseimg-abc"); err != nil {
		t.Fatal(err)
	}

	// Listing via a fresh Store instance (simulating worker restart) sees everything
	// and never mistakes subdirectories inside volumes for volumes.
	reopened := &Store{Root: root}
	vols, err := reopened.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	wantVols := []string{"baseimages/baseimg-abc", "bases/app/g1", "ws/w-1"}
	if !slices.Equal(vols, wantVols) {
		t.Errorf("ListVolumes = %q, want %q", vols, wantVols)
	}
	snaps, err := reopened.ListSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	wantSnaps := []string{"app-g1", "baseimg-abc"}
	if !slices.Equal(snaps, wantSnaps) {
		t.Errorf("ListSnapshots = %q, want %q", snaps, wantSnaps)
	}

	if err := reopened.Destroy(ctx, "ws/w-1"); err != nil {
		t.Fatal(err)
	}
	vols, err = reopened.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantVols = []string{"baseimages/baseimg-abc", "bases/app/g1"}
	if !slices.Equal(vols, wantVols) {
		t.Errorf("ListVolumes after Destroy = %q, want %q", vols, wantVols)
	}
}

func TestUnitsListBySlice(t *testing.T) {
	ctx := t.Context()
	u := &Units{}
	t.Cleanup(u.Close)
	dir := t.TempDir()

	for _, spec := range []unit.Spec{
		{Name: "hotbed-c-1", RootDir: dir, Argv: []string{"sleep", "30"}, Log: filepath.Join(dir, "1.log")},
		{Name: "hotbed-c-2", RootDir: dir, Slice: unit.DefaultSlice, Argv: []string{"sleep", "30"}, Log: filepath.Join(dir, "2.log")},
		{Name: "hotbed-b-1", RootDir: dir, Slice: "hotbed_bench.slice", Argv: []string{"sleep", "30"}, Log: filepath.Join(dir, "3.log")},
		{Name: "other-svc", RootDir: dir, Argv: []string{"sleep", "30"}, Log: filepath.Join(dir, "4.log")},
	} {
		if err := u.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}

	gotDefault, err := u.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotDefault, []string{"hotbed-c-1", "hotbed-c-2"}) {
		t.Errorf("List(default) = %q, want [hotbed-c-1 hotbed-c-2]", gotDefault)
	}

	gotBench, err := u.List(ctx, "hotbed_bench.slice")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotBench, []string{"hotbed-b-1"}) {
		t.Errorf("List(bench) = %q, want [hotbed-b-1]", gotBench)
	}

	if err := u.Stop(ctx, "hotbed-c-1"); err != nil {
		t.Fatal(err)
	}
	gotDefault, err = u.List(ctx, unit.DefaultSlice)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotDefault, []string{"hotbed-c-2"}) {
		t.Errorf("List after Stop = %q, want [hotbed-c-2]", gotDefault)
	}
}

// fake.Units matches real internal/unit by rejecting specs with empty RootDir
// in both Start and StartShell (board 9274).
func TestUnitsRejectEmptyRootDir(t *testing.T) {
	ctx := t.Context()
	u := &Units{}
	t.Cleanup(u.Close)
	dir := t.TempDir()

	if err := u.Start(ctx, unit.Spec{
		Name: "hotbed-no-root",
		Argv: []string{"true"},
		Log:  filepath.Join(dir, "out.log"),
	}); err == nil || !strings.Contains(err.Error(), "root directory is required") {
		t.Fatalf("Start with empty RootDir = %v, want root directory required error", err)
	}

	if h, err := u.StartShell(ctx, unit.Spec{
		Name: "hotbed-sh-no-root",
		Argv: []string{"true"},
	}, unit.ShellIO{}); err == nil || !strings.Contains(err.Error(), "root directory is required") {
		if h != nil {
			_ = h.Close()
		}
		t.Fatalf("StartShell with empty RootDir = %v, want root directory required error", err)
	}
}
