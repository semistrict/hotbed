package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDaemonSnapshotAndMenubarActions(t *testing.T) {
	dir := t.TempDir()
	var openedURL string
	var startedVM, stoppedVM atomic.Int32

	d, err := New(Config{
		BaseDir:         dir,
		DisableT3Bridge: true,
		DisableWorkerVM: true,
		DisableMenubar:  true,
		ControllerFunc:  func() (string, error) { return "https://hotbed.example.com", nil },
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		OpenURL: func(_ context.Context, u string) error {
			openedURL = u
			return nil
		},
		StartVM: func(context.Context, string) error {
			startedVM.Add(1)
			return nil
		},
		StopVM: func(context.Context, string) error {
			stoppedVM.Add(1)
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

	d.handleMenubarAction(ctx, "start_vm")
	d.handleMenubarAction(ctx, "stop_vm")
	deadline = time.Now().Add(2 * time.Second)
	for (startedVM.Load() < 1 || stoppedVM.Load() < 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if startedVM.Load() != 1 || stoppedVM.Load() != 1 {
		t.Fatalf("start/stop counts = %d/%d, want 1/1", startedVM.Load(), stoppedVM.Load())
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
