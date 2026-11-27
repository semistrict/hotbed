package unit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A cgroup can empty and go with no inotify event reaching its
// cgroup.events' watcher (kernfs raises none on removal): waitEmptied still
// sees it gone. A rename, which raises no IN_MODIFY, stands in for that
// silent removal.
func TestWaitEmptiedSeesACgroupGoneWithoutAnEvent(t *testing.T) {
	events := filepath.Join(t.TempDir(), "cgroup.events")
	if err := os.WriteFile(events, []byte("populated 1\nfrozen 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- waitEmptied(t.Context(), events) }()
	select {
	case err := <-done:
		t.Fatalf("waitEmptied of a populated cgroup returned %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.Rename(events, events+".gone"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitEmptied of a cgroup gone = %v, want nil", err)
		}
	case <-time.After(5 * waitRecheck):
		t.Fatalf("waitEmptied still waits %s after its cgroup went", 5*waitRecheck)
	}
}

// Its caller giving up ends it at once, rechecks or not.
func TestWaitEmptiedEndsWithItsContext(t *testing.T) {
	events := filepath.Join(t.TempDir(), "cgroup.events")
	if err := os.WriteFile(events, []byte("populated 1\nfrozen 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- waitEmptied(ctx, events) }()
	select {
	case err := <-done:
		t.Fatalf("waitEmptied of a populated cgroup returned %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitEmptied after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * waitRecheck):
		t.Fatal("waitEmptied outlived its context")
	}
}
