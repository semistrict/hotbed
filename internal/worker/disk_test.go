package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/storage"
)

type failingUsageStore struct {
	*fake.Store
	usageErr map[string]error
	onUsage  func(vol string)
}

func (s *failingUsageStore) Usage(ctx context.Context, name string) (storage.Usage, error) {
	if s.onUsage != nil {
		s.onUsage(name)
	}
	if err, ok := s.usageErr[name]; ok {
		return storage.Usage{}, err
	}
	return s.Store.Usage(ctx, name)
}

// measureDisk must not warn on failed bases (whose volume collectBases removes
// while keeping the record), preparing/failed/released workspaces, concurrently
// removed volumes, or context cancellation during shutdown; it must still warn
// on genuine storage errors or a missing warm base volume (board 7697).
func TestMeasureDiskSkipsExpectedMissingVolumesAndCanceledShutdown(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	baseStore := &fake.Store{Root: t.TempDir()}
	store := &failingUsageStore{
		Store:    baseStore,
		usageErr: map[string]error{},
	}
	now := time.Unix(1795400000, 0)
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	// Create one real warm base and one real idle workspace on disk.
	if err := os.MkdirAll(baseStore.Path("bases/app/g1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseStore.Path("bases/app/g1"), "f"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(baseStore.Path("ws/live1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseStore.Path("ws/live1"), "f"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	e.mu.Lock()
	e.bases["app-g1"] = &pb.Base{Id: "app-g1", Project: "app", Generation: 1, State: warm}
	// Failed base whose volume was deleted by collectBases.
	e.bases["app-g2"] = &pb.Base{Id: "app-g2", Project: "app", Generation: 2, State: broken, SizeBytes: 999}
	// Workspaces with no volume on disk: preparing, failed, released.
	e.workspaces["live1"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "live1", State: idle}, Volume: "ws/live1"}}
	e.workspaces["prep1"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "prep1", State: preparing}, Volume: "ws/prep1"}}
	e.workspaces["fail1"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "fail1", State: failedWS}, Volume: "ws/fail1"}}
	e.workspaces["rel1"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "rel1", State: pb.WorkspaceState_WORKSPACE_STATE_RELEASED}, Volume: "ws/rel1"}}
	e.mu.Unlock()

	e.measureDisk(t.Context())
	if logs.Len() != 0 {
		t.Fatalf("unexpected warnings for failed base / preparing / failed / released workspaces:\n%s", logs.String())
	}
	if e.bases["app-g2"].SizeBytes != 0 {
		t.Errorf("failed base SizeBytes = %d, want 0", e.bases["app-g2"].SizeBytes)
	}

	// Shutdown cancellation mid-walk stops immediately with zero warnings.
	cctx, cancel := context.WithCancel(t.Context())
	store.onUsage = func(string) { cancel() }
	store.usageErr["bases/app/g1"] = context.Canceled
	store.usageErr["ws/live1"] = context.Canceled
	e.measureDisk(cctx)
	if logs.Len() != 0 {
		t.Fatalf("unexpected warnings on canceled shutdown context:\n%s", logs.String())
	}

	// Genuine I/O error on a live volume still logs a WARN.
	store.onUsage = nil
	store.usageErr = map[string]error{"ws/live1": errors.New("btrfs io error")}
	e.measureDisk(t.Context())
	if !strings.Contains(logs.String(), "btrfs io error") {
		t.Errorf("expected WARN on genuine storage error, got:\n%s", logs.String())
	}

	// Missing volume for a WARM base still logs a WARN (not silently hidden).
	logs.Reset()
	store.usageErr = map[string]error{"bases/app/g1": storage.ErrNotFound}
	e.measureDisk(t.Context())
	if !strings.Contains(logs.String(), "measure volume") {
		t.Errorf("expected WARN when a warm base's volume unexpectedly disappears, got:\n%s", logs.String())
	}
}
