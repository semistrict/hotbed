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
