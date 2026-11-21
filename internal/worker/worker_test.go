package worker_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
	"github.com/semistrict/hotbed/internal/worker"
)

var (
	asahi   = tailnet.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: "ramon@"}
	laptop  = tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}
	mallory = tailnet.Peer{NodeID: "nMallory", Hostname: "mallory-pc", Login: "mallory@"}

	asahiResources = api.Resources{Arch: "arm64", CPUs: 11, MemoryBytes: 17 << 30, Kernel: "6.17.0-asahi"}
	t0             = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
)

func probe() (api.Resources, error) { return asahiResources, nil }

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// cluster is a controller over loopback with a fake tailnet and clock.
type cluster struct {
	tailnet *tailnettest.Fake
	clock   *clock
	id      string
	addr    string // host:port
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	c := &cluster{
		tailnet: tailnettest.New(tailnet.Self{Peer: tailnet.Peer{NodeID: "nUbu1", Hostname: "ubu1", Login: "ramon@"}}),
		clock:   &clock{now: t0},
		id:      "c0ffee",
	}
	store, err := controller.OpenStore(t.Context(), filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	srv := httptest.NewServer(controller.NewServer(controller.ServerConfig{
		Store:      store,
		Identifier: c.tailnet,
		Cluster:    statedir.ControllerConfig{ClusterID: c.id, Owner: "ramon@", Port: api.DefaultPort},
		Now:        c.clock.Now,
	}).Handler())
	t.Cleanup(srv.Close)
	c.addr = strings.TrimPrefix(srv.URL, "http://")
	return c
}

func (c *cluster) client(t *testing.T, p tailnet.Peer) *api.Client {
	t.Helper()
	client, err := api.NewClient(c.addr, c.tailnet.Client(p))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (c *cluster) join(t *testing.T, p tailnet.Peer) (worker.JoinResult, *worker.Worker) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "worker")
	res, err := worker.Join(t.Context(), dir, c.addr, c.tailnet.Client(p), probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := statedir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(*cfg.Worker, c.tailnet.Client(p), probe)
	if err != nil {
		t.Fatal(err)
	}
	return res, w
}

func (c *cluster) nodes(t *testing.T) []api.Node {
	t.Helper()
	res, err := c.client(t, laptop).Nodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.ClusterID != c.id {
		t.Errorf("nodes cluster ID = %q, want %q", res.ClusterID, c.id)
	}
	return res.Nodes
}

func wantCode(t *testing.T, err error, code api.Code, retryable bool) {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v, want %s", err, code)
	}
	if e.Code != code || e.Retryable != retryable {
		t.Fatalf("error = %s (retryable %v): %s; want %s (retryable %v)", e.Code, e.Retryable, e.Message, code, retryable)
	}
}

func TestJoinWritesWorkerConfig(t *testing.T) {
	c := newCluster(t)
	dir := filepath.Join(t.TempDir(), "worker")
	res, err := worker.Join(t.Context(), dir, c.addr, c.tailnet.Client(asahi), probe)
	if err != nil {
		t.Fatal(err)
	}
	want := worker.JoinResult{
		ClusterID:  c.id,
		Dir:        dir,
		Controller: c.addr,
		Node:       api.Node{ID: "nAsahi", Hostname: "asahimbp", Login: "ramon@", JoinedAt: t0},
		Next:       []string{"hotbed run " + dir},
	}
	if diff := cmp.Diff(want, res); diff != "" {
		t.Errorf("Join (-want +got):\n%s", diff)
	}
	cfg, err := statedir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(statedir.Config{Worker: &statedir.WorkerConfig{ClusterID: c.id, Controller: c.addr}}, cfg); diff != "" {
		t.Errorf("worker config (-want +got):\n%s", diff)
	}
}

func TestHeartbeatMakesNodeOnline(t *testing.T) {
	c := newCluster(t)
	_, w := c.join(t, asahi)
	if diff := cmp.Diff([]api.Node{{ID: "nAsahi", Hostname: "asahimbp", Login: "ramon@", JoinedAt: t0}}, c.nodes(t)); diff != "" {
		t.Errorf("nodes before heartbeat (-want +got):\n%s", diff)
	}

	c.clock.advance(time.Second)
	if err := w.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	seen := t0.Add(time.Second)
	want := []api.Node{{
		ID: "nAsahi", Hostname: "asahimbp", Login: "ramon@", JoinedAt: t0,
		LastSeen: &seen, Online: true, Resources: &asahiResources,
	}}
	if diff := cmp.Diff(want, c.nodes(t)); diff != "" {
		t.Errorf("nodes after heartbeat (-want +got):\n%s", diff)
	}
}

func TestNodeGoesOfflineAfterMissedHeartbeats(t *testing.T) {
	c := newCluster(t)
	_, w := c.join(t, asahi)
	if err := w.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.clock.advance(api.OnlineWindow)
	if got := c.nodes(t)[0].Online; !got {
		t.Errorf("online %v after OnlineWindow, want true", got)
	}
	c.clock.advance(time.Nanosecond)
	if got := c.nodes(t)[0].Online; got {
		t.Errorf("online %v past OnlineWindow, want false", got)
	}
}

func TestJoinAgainKeepsJoinTime(t *testing.T) {
	c := newCluster(t)
	c.join(t, asahi)
	c.clock.advance(time.Hour)
	res, _ := c.join(t, asahi)
	if !res.Node.JoinedAt.Equal(t0) {
		t.Errorf("joined at %v after joining again, want %v", res.Node.JoinedAt, t0)
	}
	if n := len(c.nodes(t)); n != 1 {
		t.Errorf("%d nodes after joining twice, want 1", n)
	}
}

func TestControllerRefusesOtherOwner(t *testing.T) {
	c := newCluster(t)
	dir := filepath.Join(t.TempDir(), "worker")
	_, err := worker.Join(t.Context(), dir, c.addr, c.tailnet.Client(mallory), probe)
	wantCode(t, err, api.CodeNotAuthorized, false)
	if _, err := statedir.Load(dir); !errors.Is(err, statedir.ErrNotInitialized) {
		t.Errorf("refused join left state: Load = %v, want ErrNotInitialized", err)
	}
	if n := len(c.nodes(t)); n != 0 {
		t.Errorf("%d nodes after refused join, want 0", n)
	}
}

func TestControllerRefusesCallerOffTailnet(t *testing.T) {
	c := newCluster(t)
	client, err := api.NewClient(c.addr, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Nodes(t.Context())
	wantCode(t, err, api.CodeNotAuthorized, false)
}

func TestHeartbeatFromNodeThatNeverJoined(t *testing.T) {
	c := newCluster(t)
	w, err := worker.New(statedir.WorkerConfig{ClusterID: c.id, Controller: c.addr}, c.tailnet.Client(asahi), probe)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, w.Beat(t.Context()), api.CodeNotJoined, false)
}

func TestRunStopsOnClusterMismatch(t *testing.T) {
	c := newCluster(t)
	c.join(t, asahi)
	w, err := worker.New(statedir.WorkerConfig{ClusterID: "another", Controller: c.addr}, c.tailnet.Client(asahi), probe)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, w.Run(t.Context()), api.CodeClusterMismatch, false)
}

func TestRunStopsWhenContextEnds(t *testing.T) {
	c := newCluster(t)
	_, w := c.join(t, asahi)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Errorf("Run with ended context = %v, want nil", err)
	}
}

func TestBeatToStoppedControllerIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	w, err := worker.New(statedir.WorkerConfig{ClusterID: "c0ffee", Controller: addr}, &http.Client{}, probe)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, w.Beat(t.Context()), api.CodeControllerUnreachable, true)
}

func TestJoinRefusesInitializedDir(t *testing.T) {
	c := newCluster(t)
	dir := filepath.Join(t.TempDir(), "worker")
	if _, err := worker.Join(t.Context(), dir, c.addr, c.tailnet.Client(asahi), probe); err != nil {
		t.Fatal(err)
	}
	_, err := worker.Join(t.Context(), dir, c.addr, c.tailnet.Client(asahi), probe)
	if !errors.Is(err, statedir.ErrAlreadyInitialized) {
		t.Errorf("second Join = %v, want ErrAlreadyInitialized", err)
	}
}

func TestJoinFailsBeforeTouchingAnythingWhenProbeFails(t *testing.T) {
	c := newCluster(t)
	dir := filepath.Join(t.TempDir(), "worker")
	noLinux := func() (api.Resources, error) {
		return api.Resources{}, api.Errorf(api.CodePlatformNotSupported, "workers need Linux")
	}
	_, err := worker.Join(t.Context(), dir, c.addr, c.tailnet.Client(asahi), noLinux)
	wantCode(t, err, api.CodePlatformNotSupported, false)
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state directory after failed probe: Stat = %v, want not exist", err)
	}
	if n := len(c.nodes(t)); n != 0 {
		t.Errorf("%d nodes after failed probe, want 0", n)
	}
}
