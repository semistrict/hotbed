package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cfbundle"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/worker"
)

// cluster is a real cf-controller Durable Object (running locally in workerd
// via Miniflare) and its Go workers in this process, with fakes only where
// Linux and root would be needed (volumes are copied, commands are local
// processes). Tests take the controller down or restart it, restart workers,
// wipe their databases, and cut them off from the controller.
type cluster struct {
	t        *testing.T
	id       string
	doName   string
	harness  *cfbundle.Harness
	server   *testServer
	store    *testStore
	asWorker hotbedv1connect.ClusterServiceClient
	cli      deps
	clock    *clock
	ctrlSrv  *httptest.Server
	ctrl     *switchboard // in front of the controller; tests take it down
	// ctrlBack serves the controller once bounce brought it back.
	ctrlBack  *http.Server
	nodes     []*node
	reported  chan error
	serverCfg testServerConfig
	handler   atomic.Pointer[http.Handler]
	// where is where the cluster's CLI runs (c.cli.here): set it with
	// setenv and chdir, never the process's (t.Setenv, t.Chdir), so tests
	// run side by side.
	where *testHere
}

// testHere is a test CLI's environment and working directory, which the test
// changes as it goes, while the CLI may be running.
type testHere struct {
	mu  sync.Mutex
	env map[string]string
	wd  string
}

func (h *testHere) getenv(k string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.env[k]
}

func (h *testHere) getwd() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wd, nil
}

func (h *testHere) setenv(k, v string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.env[k] = v
}

func (h *testHere) chdir(dir string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wd = dir
}

// here is the CLI's view of h.
func (h *testHere) here() here { return here{getenv: h.getenv, getwd: h.getwd} }

// newTestHere is where a test's CLI starts: in a directory of its own, outside
// any checkout, with nothing in its environment but what is given.
func newTestHere(t *testing.T, env map[string]string) *testHere {
	t.Helper()
	h := &testHere{env: map[string]string{}, wd: t.TempDir()}
	maps.Copy(h.env, env)
	return h
}

// setenv sets the variable k of the environment the cluster's CLI runs in.
func (c *cluster) setenv(k, v string) { c.where.setenv(k, v) }

// chdir moves the cluster's CLI into dir.
func (c *cluster) chdir(dir string) { c.where.chdir(dir) }

// node is a worker: its engine, its server, its controller WebSocket tunnel,
// and what outlives a restart of it (its state directory, its volumes, its
// commands' units).
type node struct {
	c      *cluster
	peer   httpx.Peer
	dir    string
	cfg    worker.EngineConfig
	store  *fake.Store
	units  *fake.Units
	system *fake.System
	// cut, set, drops every call between the worker and the controller,
	// both ways, as a network partition would.
	cut atomic.Bool
	// tunnelHook, set, wraps the worker's handler of the calls the
	// controller relays to it over its tunnel: clients' calls, as the
	// controller passes them on.
	tunnelHook atomic.Pointer[func(http.Handler) http.Handler]
	// dropTunnel ends the worker's tunnel to the controller, which it
	// dials again at once, as a dropped connection does.
	dropTunnel atomic.Pointer[context.CancelFunc]
	// halted is set while the worker is stopped: the calls the
	// controller relays to it fail, as a stopped worker's would.
	halted atomic.Bool
	// follows counts the streams of output clients opened to it.
	follows atomic.Int64
	// createGate, set, holds every CreateWorkspace call until it is closed,
	// as a node slow to make a workspace does.
	createGate atomic.Pointer[chan struct{}]
	cluster    hotbedv1connect.ClusterServiceClient // the controller, as this worker
	follow     *worker.Follow                       // how it follows the controller's build; nil for not
	// reported is how its connection to the controller ended, each time.
	reported chan error

	mu     sync.Mutex
	engine *worker.Engine
	stop   func() // stops the engine and its connections, and waits
}

var fleetPeers = []httpx.Peer{
	asahi,
	{NodeID: "nMini", Hostname: "mini", Login: "ramon@"},
	{NodeID: "nOldpc", Hostname: "oldpc", Login: "ramon@"},
}

// newCluster is a controller with one worker, asahimbp.
func newCluster(t *testing.T, with ...func(*worker.EngineConfig)) *cluster {
	t.Helper()
	return newFleetWith(t, 1, clusterOpts{engine: with})
}

// newClusterWith is newCluster with opts.
func newClusterWith(t *testing.T, opts clusterOpts) *cluster {
	t.Helper()
	return newFleetWith(t, 1, opts)
}

// newControllerOnly is a controller with no workers, for tests of what the
// controller alone keeps (questions, reviews, memories, the board...).
func newControllerOnly(t *testing.T) *cluster {
	t.Helper()
	return newFleetWith(t, 0, clusterOpts{})
}

// newFleet is a controller with n workers: asahimbp, mini and oldpc.
func newFleet(t *testing.T, n int) *cluster {
	t.Helper()
	return newFleetWith(t, n, clusterOpts{})
}

// clusterOpts change a cluster before it starts.
type clusterOpts struct {
	engine []func(*worker.EngineConfig)
	server func(*testServerConfig)
	// follow is how a worker follows the controller's build at ctrl.
	follow func(ctrl string, hc *http.Client) *worker.Follow
}

func newFleetWith(t *testing.T, n int, opts clusterOpts) *cluster {
	t.Helper()
	h, err := getCFHarness()
	if err != nil {
		t.Fatal(err)
	}
	doName := fmt.Sprintf("test-%d-%d", os.Getpid(), harnessSeq.Add(1))

	c := &cluster{
		t:       t,
		doName:  doName,
		harness: h,
		server:  &testServer{h: h, doName: doName},
		store:   &testStore{h: h, doName: doName},
	}
	c.clock = &clock{
		onChange: func(offset time.Duration) {
			if err := h.DoJSON(context.Background(), doName, http.MethodPost, "/__test/clock", map[string]any{
				"offsetMs": offset.Milliseconds(),
			}, nil); err != nil {
				t.Errorf("sync controller test clock: %v", err)
			}
		},
	}

	if opts.server != nil {
		opts.server(&c.serverCfg)
	}
	c.applyServerConfig()

	for i := range n {
		nd := &node{c: c, peer: fleetPeers[i], store: &fake.Store{Root: t.TempDir()}, units: &fake.Units{}, system: fake.NewSystem(), reported: make(chan error, 1)}
		nd.dir = filepath.Join(t.TempDir(), "worker")
		// Registered after the store and dir TempDirs (and before nd.halt) so
		// LIFO cleanup stops the engine first, then kills every surviving fake
		// unit and waits for its processes to exit before TempDir's RemoveAll
		// runs (board 7995).
		t.Cleanup(nd.units.Close)
		nd.cfg = worker.EngineConfig{
			Dir: nd.dir, NodeID: nd.peer.NodeID, Host: nd.peer.Hostname,
			Store: nd.store, BaseImage: fake.BaseImage(nd.store), Units: nd.units, System: nd.system,
			ControllerNodeID: controllerNode, AllowLocalRepos: true,
		}
		for _, w := range opts.engine {
			w(&nd.cfg)
		}
		c.nodes = append(c.nodes, nd)
	}

	target, err := url.Parse(h.URL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	origDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		origDirector(r)
		r.Host = target.Host
	}
	var baseHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every test client says which node it is (peerClient): one that
		// does not is no test peer.
		if r.Header.Get("X-Hotbed-Node-Id") == "" {
			http.Error(w, "unknown test peer", http.StatusForbidden)
			return
		}
		r2 := r.Clone(r.Context())
		r2.Header.Set("X-Hotbed-Test-DO", doName)
		if r2.Header.Get("Authorization") == "" {
			r2.Header.Set("Authorization", "Bearer "+h.Token())
		}
		proxy.ServeHTTP(w, r2)
	})
	c.handler.Store(&baseHandler)
	c.ctrl = &switchboard{
		next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			(*c.handler.Load()).ServeHTTP(w, r)
		}),
	}
	c.ctrlSrv = httptest.NewServer(c.ctrl)
	t.Cleanup(c.ctrlSrv.Close)
	ctrl := strings.TrimPrefix(c.ctrlSrv.URL, "http://")

	for _, nd := range c.nodes {
		hc := &http.Client{Transport: partition{next: peerClient(nd.peer).Transport, cut: func(string) bool { return nd.cut.Load() }}}
		if nd.cluster, _, err = api.Controller(ctrl, hc); err != nil {
			t.Fatal(err)
		}
		if nd.cfg.GitAuth == nil {
			conn, err := api.Dial(ctrl, api.DefaultPort, hc)
			if err != nil {
				t.Fatal(err)
			}
			nd.cfg.GitAuth = hotbedv1connect.NewGitAuthServiceClient(conn)
		}
		// Joining is not cut off: it is how a worker comes to be.
		joiner, _, err := api.Controller(ctrl, peerClient(nd.peer))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := joiner.Join(t.Context(), &pb.JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		if opts.follow != nil {
			nd.follow = opts.follow(ctrl, peerClient(nd.peer))
		}
		nd.start()
		t.Cleanup(nd.halt)
	}
	if n > 0 {
		c.asWorker, c.reported = c.nodes[0].cluster, c.nodes[0].reported
	}
	// Send an initial heartbeat synchronously so every node is online with its
	// resources populated before the test starts, avoiding CPU-load races
	// against worker.Connect's first background heartbeat (board 7949).
	// Skip when follow is configured so Connect drives the follow check itself.
	if opts.follow == nil {
		c.heartbeat()
	}

	// Sessions start in the tests' project unless a test names another.
	if _, err := c.store.AddProject(t.Context(), "test", nil, &pb.Stamp{At: time.Now().UnixMilli(), Reason: "the tests' project"}); err != nil {
		t.Fatal(err)
	}
	// Where the tests run says nothing of where sessions bind: they start
	// outside any T3 thread or checkout, so a session binds to nothing and
	// a command names its session, unless a test goes somewhere itself.
	c.where = newTestHere(t, map[string]string{"HOTBED_PROJECT": "test"})
	c.cli = deps{
		here:       c.where.here(),
		controller: at(ctrl),
		// The CLI runs on the laptop: its calls come from the laptop.
		http: recording(peerClient(laptop)), stream: recording(peerClient(laptop)), cache: t.TempDir(),
		sessionsFile: filepath.Join(t.TempDir(), "sessions.json"),
		exe:          func() (string, error) { return "", errors.New("the tests' client has no binary to upgrade") },
		reexec:       func(...string) error { return errors.New("the tests' client runs nothing") },
	}
	return c
}

func (c *cluster) applyServerConfig() {
	c.t.Helper()
	// The controller's maintenance alarm runs on the wall clock, not the
	// tests' clock: paused, it never races a test that jumps the clock or
	// drives maintenance itself (testServer's ticks).
	payload := map[string]any{"maintenanceAlarm": false}
	if c.serverCfg.WakeWait > 0 {
		payload["wakeWaitMs"] = c.serverCfg.WakeWait.Milliseconds()
	}
	if c.serverCfg.AllowLocalRepos != nil {
		payload["allowLocalRepos"] = *c.serverCfg.AllowLocalRepos
	}
	if c.serverCfg.UpdatesRepo != "" {
		payload["updatesRepo"] = c.serverCfg.UpdatesRepo
	}
	if c.serverCfg.UpdatesAPIBase != "" {
		payload["updatesApiBase"] = c.serverCfg.UpdatesAPIBase
	}
	if c.serverCfg.UpdatesDownloadBase != "" {
		payload["updatesDownloadBase"] = c.serverCfg.UpdatesDownloadBase
	}
	if c.serverCfg.UpdatesToken != "" {
		payload["updatesToken"] = c.serverCfg.UpdatesToken
	}
	if c.serverCfg.ControllerBuild > 0 {
		payload["controllerBuild"] = c.serverCfg.ControllerBuild
	}
	if c.serverCfg.ControllerCommit != "" {
		payload["controllerCommit"] = c.serverCfg.ControllerCommit
	}
	if c.serverCfg.ControllerSha256Sums != "" {
		payload["controllerSha256Sums"] = c.serverCfg.ControllerSha256Sums
	}
	if c.serverCfg.Build > 0 && c.serverCfg.Builds != nil {
		mb, err := buildMockPayload(c.serverCfg.Builds, c.serverCfg.Build)
		if err != nil {
			c.t.Fatal(err)
		}
		payload["mockBuild"] = mb
	}
	var res struct {
		OK        bool   `json:"ok"`
		ClusterID string `json:"clusterId"`
	}
	if err := c.harness.DoJSON(c.t.Context(), c.doName, http.MethodPost, "/__test/config", payload, &res); err != nil {
		c.t.Fatal(err)
	}
	if res.ClusterID != "" {
		c.id = res.ClusterID
	}
}

// partition fails the calls of a client while cut says the destination is
// cut off, as an unreachable machine would.
type partition struct {
	next http.RoundTripper
	cut  func(addr string) bool
}

func (p partition) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.cut(r.URL.Host) {
		return nil, errors.New("network is unreachable")
	}
	return p.next.RoundTrip(r)
}

// restart rebuilds the controller DO's in-memory runtime from its SQLite
// database, as a deploy/eviction does: whatever it knows must come from there.
func (c *cluster) restart() {
	c.t.Helper()
	if err := c.harness.DoJSON(c.t.Context(), c.doName, http.MethodPost, "/__test/restart", map[string]any{}, nil); err != nil {
		c.t.Fatal(err)
	}
	c.applyServerConfig()
}

// engine is the first worker's engine as it is now.
func (c *cluster) engine() *worker.Engine {
	c.t.Helper()
	if len(c.nodes) == 0 {
		c.t.Fatal("the cluster has no workers (newControllerOnly): its test needs one")
	}
	return c.nodes[0].eng()
}

func (nd *node) eng() *worker.Engine {
	nd.mu.Lock()
	defer nd.mu.Unlock()
	return nd.engine
}

// start opens the worker's engine on its state directory and runs it, its
// heartbeat/event connection, and its reverse-RPC WebSocket tunnel to the
// controller.
func (nd *node) start() {
	t := nd.c.t
	eng, err := worker.NewEngine(t.Context(), nd.cfg)
	if err != nil {
		t.Fatal(err)
	}
	shellSrv := worker.NewShellServer(eng)
	eng.SetShellServer(shellSrv)
	nd.halted.Store(false)

	ctrlURL := nd.c.ctrlSrv.URL
	tunnelWrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if nd.halted.Load() || nd.cut.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":"unavailable","message":"network is unreachable"}`))
				return
			}
			if strings.HasSuffix(r.URL.Path, "/FollowLog") {
				nd.follows.Add(1)
			}
			if gate := nd.createGate.Load(); gate != nil && strings.HasSuffix(r.URL.Path, "/CreateWorkspace") {
				<-*gate
			}
			if hook := nd.tunnelHook.Load(); hook != nil {
				(*hook)(next).ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); eng.Run(ctx) }()
	go func() {
		defer wg.Done()
		err := worker.Connect(ctx, eng, nd.cluster, nd.c.id, nd.follow)
		select {
		case nd.reported <- err:
		default: // an earlier end not read yet
		}
	}()
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			tctx, drop := context.WithCancel(ctx)
			nd.dropTunnel.Store(&drop)
			_ = worker.TunnelWithMiddleware(
				tctx,
				eng,
				ctrlURL,
				nd.c.harness.Token(),
				nd.peer,
				controllerNode,
				"ramon@",
				peerClient(nd.peer),
				tunnelWrap,
			)
			drop()
		}
	}()
	nd.mu.Lock()
	nd.engine = eng
	nd.stop = func() {
		cancel()
		_ = shellSrv.Close()
		wg.Wait()
		eng.Close()
	}
	nd.mu.Unlock()

	// Wait for the worker's reverse-RPC WebSocket tunnel to register with the
	// controller DO before returning so controller->worker RPCs never race boot.
	nd.c.ctrl.mu.Lock()
	ctrlDown := nd.c.ctrl.down
	nd.c.ctrl.mu.Unlock()
	if !ctrlDown && !nd.cut.Load() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			ids, err := nd.c.store.ConnectedWorkers(t.Context())
			if err == nil && slices.Contains(ids, nd.peer.NodeID) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("worker %s (%s) never registered its controller WebSocket tunnel", nd.peer.Hostname, nd.peer.NodeID)
	}
}

// halt stops the worker; its commands run on, as systemd units do.
func (nd *node) halt() {
	nd.halted.Store(true)
	nd.mu.Lock()
	stop := nd.stop
	nd.stop = nil
	nd.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// restart stops the worker and starts it again on the same state, as a
// deploy does.
func (nd *node) restart() {
	nd.halt()
	nd.start()
}

// wipe restarts the worker without its database, as if it were lost: its
// volumes and its commands' units are still there.
func (nd *node) wipe() {
	nd.halt()
	for _, f := range []string{"worker.db", "worker.db-wal", "worker.db-shm"} {
		if err := os.Remove(filepath.Join(nd.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			nd.c.t.Fatal(err)
		}
	}
	nd.start()
}
