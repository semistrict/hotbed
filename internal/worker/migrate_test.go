package worker

import (
	"context"
	"errors"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/fake"
)

type migratedClusterClient struct {
	hotbedv1connect.ClusterServiceClient
	mig *pb.ControllerMigration
}

func (m migratedClusterClient) Heartbeat(_ context.Context, _ *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return &pb.HeartbeatResponse{MigratedTo: m.mig}, nil
}

func (m migratedClusterClient) PushEvents(_ context.Context, _ *pb.PushEventsRequest) (*pb.PushEventsResponse, error) {
	return &pb.PushEventsResponse{MigratedTo: m.mig}, nil
}

func TestWorkerAppliesControllerMigrationAndRestarts(t *testing.T) {
	dir := t.TempDir()
	orig := statedir.WorkerConfig{
		ClusterID:        "cid-1",
		Controller:       "ubu1:7120",
		User:             "ramon",
		Owner:            "ramon@example.com",
		ControllerNodeID: "node-old-ctrl",
	}
	if err := statedir.Commit(dir, statedir.Config{Worker: &orig}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	mig := &pb.ControllerMigration{
		Controller:       "https://hotbed-controller.example.workers.dev/",
		Token:            "hbw_new_secret",
		ControllerNodeId: "cf:cid-1",
	}

	// Calling applyControllerMigration rewrites hotbed.json and returns update.ErrRestart.
	err := applyControllerMigration(dir, orig, mig)
	if !errors.Is(err, update.ErrRestart) {
		t.Fatalf("applyControllerMigration = %v, want update.ErrRestart", err)
	}
	loaded, err := statedir.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w := loaded.Worker
	if w.Controller != "https://hotbed-controller.example.workers.dev" || w.Token != "hbw_new_secret" || w.ControllerNodeID != "cf:cid-1" {
		t.Fatalf("rewritten WorkerConfig = %+v", w)
	}

	// Already on the target with the same token & controllerNodeId: no-op (prevents restart loops).
	if err := applyControllerMigration(dir, *w, mig); err != nil {
		t.Fatalf("second applyControllerMigration = %v, want nil", err)
	}

	// Verify HeartbeatOnce and pusher.push invoke Follow.OnMigrate and return update.ErrRestart.
	store := &fake.Store{Root: t.TempDir()}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "node-1", Host: "ubu1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { eng.Close() })
	called := 0
	follow := &Follow{
		OnMigrate: func(got *pb.ControllerMigration) error {
			called++
			if got.GetController() != mig.Controller || got.GetToken() != mig.Token {
				t.Errorf("OnMigrate got %+v", got)
			}
			return update.ErrRestart
		},
	}
	stub := migratedClusterClient{mig: mig}
	if err := HeartbeatOnce(t.Context(), eng, stub, "cid-1", follow); !errors.Is(err, update.ErrRestart) {
		t.Fatalf("HeartbeatOnce = %v, want update.ErrRestart", err)
	}
	p := &pusher{eng: eng, cluster: stub, clusterID: "cid-1", follow: follow}
	if err := p.push(t.Context(), &pb.PushEventsRequest{}); !errors.Is(err, update.ErrRestart) {
		t.Fatalf("pusher.push = %v, want update.ErrRestart", err)
	}
	if called != 2 {
		t.Fatalf("OnMigrate called %d times, want 2", called)
	}
}
