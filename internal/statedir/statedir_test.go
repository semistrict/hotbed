package statedir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCommitThenLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	want := Config{Worker: &WorkerConfig{ClusterID: "c1", Controller: "ubu1:7120"}}
	if err := Commit(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Load (-want +got):\n%s", diff)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "hotbed.json" {
		t.Errorf("state directory holds %v, want only hotbed.json", entries)
	}
}

func TestPrepareLeavesParentsPassable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "lib", "hotbed", "controller")
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(root, "lib"):           0o755,
		filepath.Join(root, "lib", "hotbed"): 0o755,
		dir:                                  0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// The parents' mode is less the umask, which may take more than 0o022.
		if got := info.Mode().Perm(); got&0o111 != want&0o111 || got&^want != 0 {
			t.Errorf("%s: mode %o, want %o (less the umask)", path, got, want)
		}
	}
}

func TestCommitRefusesSecondConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Controller: &ControllerConfig{ClusterID: "c1", Owner: "ramon@", Port: 7120}}
	if err := Commit(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := Commit(dir, cfg); !errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("second Commit = %v, want ErrAlreadyInitialized", err)
	}
	if err := Prepare(dir); !errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("Prepare = %v, want ErrAlreadyInitialized", err)
	}
}

func TestCommitNeedsExactlyOneRole(t *testing.T) {
	both := Config{
		Controller: &ControllerConfig{ClusterID: "c1"},
		Worker:     &WorkerConfig{ClusterID: "c1"},
	}
	for name, cfg := range map[string]Config{"none": {}, "both": both} {
		if err := Commit(t.TempDir(), cfg); err == nil {
			t.Errorf("Commit with %s roles succeeded", name)
		}
	}
}

func TestLoadUninitialized(t *testing.T) {
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNotInitialized) {
		t.Errorf("Load = %v, want ErrNotInitialized", err)
	}
}
