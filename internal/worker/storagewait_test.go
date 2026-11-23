package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/version"
)

// storageCluster records the heartbeats a worker waiting on its storage sends.
type storageCluster struct {
	hotbedv1connect.ClusterServiceClient
	err   error
	build *pb.Build

	mu    sync.Mutex
	beats []*pb.HeartbeatRequest
}

func (c *storageCluster) Heartbeat(_ context.Context, h *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beats = append(c.beats, h)
	if c.err != nil {
		return nil, c.err
	}
	return &pb.HeartbeatResponse{Build: c.build}, nil
}

// failingOpen fails its first n calls with err, then opens store.
func failingOpen(n int, err error, store storage.Store) (func(context.Context) (storage.Store, error), *int) {
	calls := 0
	return func(context.Context) (storage.Store, error) {
		calls++
		if calls <= n {
			return nil, err
		}
		return store, nil
	}, &calls
}

// A worker whose storage cannot open at start (a pool on a disk that
// came up at 0 B, board 9141) does not exit: it tells the controller why on
// each try, and starts once its storage opens.
func TestAwaitStorageReportsUntilStorageOpens(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	openErr := errors.New("ZFS pool not imported: pool hotbed, of dataset hotbed/hotbed, is not imported")
	open, calls := failingOpen(2, openErr, store)
	cl := &storageCluster{}

	got, err := awaitStorage(t.Context(), open, cl, "cl", nil, time.Millisecond)
	if err != nil {
		t.Fatalf("awaitStorage = %v, want nil", err)
	}
	if got != store {
		t.Fatalf("awaitStorage store = %v, want the opened store", got)
	}
	if *calls != 3 {
		t.Fatalf("open calls = %d, want 3", *calls)
	}
	want := &pb.HeartbeatRequest{
		ClusterId: "cl", StorageError: openErr.Error(),
		Resources: &pb.Resources{Arch: runtime.GOARCH, Cpus: int32(runtime.NumCPU()), Version: version.String()},
	}
	if len(cl.beats) != 2 {
		t.Fatalf("heartbeats = %d, want 2 (one per failed open)", len(cl.beats))
	}
	for i, h := range cl.beats {
		if h.String() != want.String() {
			t.Fatalf("heartbeat %d = %v, want %v", i, h, want)
		}
	}
}

// Storage that opens at once costs no heartbeat.
func TestAwaitStorageOpenAtOnce(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	open, _ := failingOpen(0, nil, store)
	cl := &storageCluster{}
	if _, err := awaitStorage(t.Context(), open, cl, "cl", nil, time.Millisecond); err != nil {
		t.Fatalf("awaitStorage = %v, want nil", err)
	}
	if len(cl.beats) != 0 {
		t.Fatalf("heartbeats = %d, want 0", len(cl.beats))
	}
}

// A controller that does not know the worker ends the wait: no retry fixes it.
func TestAwaitStorageStopsWhenNotJoined(t *testing.T) {
	open, _ := failingOpen(1000, errors.New("no pool"), nil)
	cl := &storageCluster{err: api.ToConnect(api.Errorf(api.CodeNotJoined, "ubu1 has not joined this cluster"))}
	_, err := awaitStorage(t.Context(), open, cl, "cl", nil, time.Millisecond)
	if got := api.CodeOf(err); got != api.CodeNotJoined {
		t.Fatalf("awaitStorage error code = %q (%v), want %q", got, err, api.CodeNotJoined)
	}
}

// A controller out of reach is ridden out: the worker keeps waiting on its
// storage, and starts once it opens.
func TestAwaitStorageRidesOutUnreachableController(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	open, _ := failingOpen(3, errors.New("no pool"), store)
	cl := &storageCluster{err: api.ToConnect(api.Errorf(api.CodeControllerUnreachable, "controller restarting"))}
	got, err := awaitStorage(t.Context(), open, cl, "cl", nil, time.Millisecond)
	if err != nil || got != store {
		t.Fatalf("awaitStorage = %v, %v; want the store, nil", got, err)
	}
	if len(cl.beats) != 3 {
		t.Fatalf("heartbeats = %d, want 3", len(cl.beats))
	}
}

// A waiting worker still installs the controller's build, so a fix reaches it.
func TestAwaitStorageInstallsControllerBuild(t *testing.T) {
	payload := []byte("hotbed build 9 binary")
	sum := sha256.Sum256(payload)
	const path = "/builds/9/hotbed-linux-amd64"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "hotbed")
	if err := os.WriteFile(exe, []byte("old build 8"), 0o755); err != nil {
		t.Fatal(err)
	}
	follow := &Follow{URL: srv.URL, Client: srv.Client(), Exe: exe, Build: 8}
	open, _ := failingOpen(1000, errors.New("no pool"), nil)
	cl := &storageCluster{build: &pb.Build{Number: 9, Sha256: hex.EncodeToString(sum[:]), Path: path}}

	_, err := awaitStorage(t.Context(), open, cl, "cl", follow, time.Millisecond)
	if !errors.Is(err, update.ErrRestart) {
		t.Fatalf("awaitStorage = %v, want update.ErrRestart", err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(payload) {
		t.Fatalf("exe = %q, want %q", b, payload)
	}
}
