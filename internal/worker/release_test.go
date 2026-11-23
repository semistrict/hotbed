package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

// A volume the kernel still holds busy when its workspace is released (a
// mount of it left in another mount namespace) does not fail the release:
// the workspace is released, and its volume goes once it is let go, even
// across a restart of the worker (board 7210).
func TestReleaseOfBusyVolume(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	start := func() (*worker.Engine, func()) {
		eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
			Dir: dir, NodeID: "nAsahi", Host: "asahimbp",
			Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
			AllowLocalRepos: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { eng.Run(ctx); close(done) }()
		return eng, func() {
			cancel()
			<-done
			eng.Close()
		}
	}

	eng, stop := start()
	project := &pb.HostedProject{Name: "app", Repos: []*pb.Repo{{Id: "app", Url: origin(t)}}, Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}}}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the workspace to be ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})
	vol := store.Path("ws/ws1")
	store.Hold("ws/ws1")
	if err := eng.ReleaseWorkspace(t.Context(), "ws1"); err != nil {
		t.Fatalf("release with its volume busy: %v", err)
	}
	ws, err := eng.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.State != pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
		t.Errorf("workspace is %v after release, want released", ws.State)
	}
	if _, err := os.Stat(vol); err != nil {
		t.Fatalf("busy volume: %v, want it still there", err)
	}
	stop()

	store.Let("ws/ws1")
	_, stop = start()
	defer stop()
	eventually(t, "the restarted worker to delete the let-go volume", func() bool {
		_, err := os.Stat(vol)
		return os.IsNotExist(err)
	})
}
