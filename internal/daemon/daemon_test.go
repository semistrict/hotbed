package daemon

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/macworker"
)

func TestDaemonSnapshotAndMenubarActions(t *testing.T) {
	dir := t.TempDir()
	var openedURL string

	d, err := New(Config{
		BaseDir:         dir,
		DisableT3Bridge: true,
		DisableWorker:   true,
		DisableMenubar:  true,
		ControllerFunc:  func() (string, error) { return "https://hotbed.example.com", nil },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		OpenURL: func(_ context.Context, u string) error {
			openedURL = u
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Wait for initial snapshot file to be written.
	statusPath := filepath.Join(dir, "daemon-status.json")
	deadline := time.Now().Add(2 * time.Second)
	var live *Snapshot
	for time.Now().Before(deadline) {
		live = LoadLiveSnapshot(statusPath)
		if live != nil && live.Controller == "https://hotbed.example.com" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if live == nil || live.Controller != "https://hotbed.example.com" {
		t.Fatalf("expected live snapshot with controller, got %+v", live)
	}
	if live.T3Bridge.State != "disabled" {
		t.Fatalf("t3Bridge.state = %q, want disabled", live.T3Bridge.State)
	}

	d.handleMenubarAction(ctx, "open_cockpit")
	if openedURL != "https://hotbed.example.com" {
		t.Fatalf("open_cockpit opened %q, want https://hotbed.example.com", openedURL)
	}

	// Stop and Start Worker hold the Mac's worker stopped, and let it run
	// again, across the daemon's restarts.
	stopped := macworker.NewPaths(dir).Stopped
	d.handleMenubarAction(ctx, "stop_worker")
	if _, err := os.Stat(stopped); err != nil {
		t.Fatalf("stop_worker left no %s: %v", stopped, err)
	}
	d.handleMenubarAction(ctx, "start_worker")
	if _, err := os.Stat(stopped); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("start_worker left %s: %v", stopped, err)
	}

	// Restart action cleanly cancels Run.
	d.handleMenubarAction(ctx, "restart")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after restart: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for daemon Run to stop on restart action")
	}
}
