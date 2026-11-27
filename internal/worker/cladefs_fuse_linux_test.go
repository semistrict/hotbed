//go:build linux

package worker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestFindMountUsers(t *testing.T) {
	procRoot := t.TempDir()
	mntDir := "/var/lib/cladefs/mnt/t0/c1"

	mkLink := func(rel, target string) {
		t.Helper()
		full := filepath.Join(procRoot, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}

	// PID 1 and selfPID must always be skipped even if they match.
	mkLink("1/cwd", mntDir)
	mkLink("100/cwd", mntDir)
	// PID 101: root matches mntDir.
	mkLink("101/root", mntDir)
	// PID 102: cwd inside mntDir.
	mkLink("102/cwd", mntDir+"/work/repo")
	// PID 103: deleted open file inside mntDir.
	mkLink("103/fd/7", mntDir+"/tmp/deleted.lock (deleted)")
	// PID 104: sibling cell with shared prefix must NOT match!
	mkLink("104/cwd", mntDir+"0/work")
	mkLink("104/fd/3", mntDir+"-other/file")
	// PID 105: unrelated process.
	mkLink("105/cwd", "/tmp")

	got := findMountUsers(procRoot, mntDir, 100)
	want := []int{101, 102, 103}
	if !slices.Equal(got, want) {
		t.Fatalf("findMountUsers = %v, want %v", got, want)
	}
}

func TestCleanupFUSEMountDirSyncFS(t *testing.T) {
	dir := t.TempDir()
	// Exercises syncfs + fd close on a real directory on Linux.
	cleanupFUSEMountDir(dir, true)
	cleanupFUSEMountDir(dir, false)
	cleanupFUSEMountDir("", true)
	cleanupFUSEMountDir("/", true)
}
