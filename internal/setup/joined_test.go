//go:build linux || darwin

package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJoinedControllerIsWhatWorkerInitRecorded(t *testing.T) {
	dir := t.TempDir()
	path := workerRecord(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"controller": "ubu1:7120", "user": "ramon"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := JoinedController(dir)
	if err != nil || got != "ubu1:7120" {
		t.Fatalf("JoinedController = %q, %v; want ubu1:7120", got, err)
	}
}

func TestJoinedControllerIsNoneWithoutAWorker(t *testing.T) {
	got, err := JoinedController(t.TempDir())
	if err != nil || got != "" {
		t.Fatalf("JoinedController = %q, %v; want none", got, err)
	}
}

func TestJoinedControllerRefusesACorruptRecord(t *testing.T) {
	dir := t.TempDir()
	path := workerRecord(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"controller":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := JoinedController(dir); err == nil {
		t.Fatalf("JoinedController = %q, nil; want an error for a corrupt record", got)
	}
}
