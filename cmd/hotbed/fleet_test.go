package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
	"github.com/semistrict/hotbed/internal/worker"
)

// cluster is a controller and its workers in this process: the real
// controller, engines and CLI over Connect, with fakes only where Linux and
// root would be needed (volumes are copied, commands are local processes).
// Tests take the controller down or restart it, restart workers, wipe their
// databases, and cut them off from the controller.
type cluster struct {
	t        *testing.T
	id       string
	tn       *tailnettest.Fake
	server   *controller.Server
	store    *controller.Store // the controller's database
	asWorker hotbedv1connect.ClusterServiceClient
	cli      deps
	clock    *clock
	ctrlSrv  *httptest.Server
	ctrl     *switchboard // in front of the controller; tests take it down
	// ctrlBack serves the controller once bounce brought it back.
	ctrlBack *http.Server
	nodes    []*node
	// reported is how the first worker's connection to the controller
	// ended.
	reported chan error
	// restart swaps in a new controller on the same database.
	serverCfg controller.ServerConfig
	handler   atomic.Pointer[http.Handler]
}

// node is a worker: its engine, its server, and what outlives a restart of
// it (its state directory, its volumes, its commands' units).
type node struct {
	c      *cluster
	peer   tailnet.Peer
	dir    string
	cfg    worker.EngineConfig
	store  *fake.Store
	units  *fake.Units
	system *fake.System
	srv    *httptest.Server
	// cut, set, drops every call between the worker and the controller,
	// both ways, as a network partition would; clients still reach it.
	cut     atomic.Bool
	handler atomic.Pointer[http.Handler]
	// halted is set while the worker is stopped: its server refuses
	// calls, as a stopped worker's port does, and refused counts them.
	halted  atomic.Bool
	refused atomic.Int64
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
	stop   func() // stops the engine and its connection, and waits
}

var fleetPeers = []tailnet.Peer{
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

// newFleet is a controller with n workers: asahimbp, mini and oldpc.
func newFleet(t *testing.T, n int) *cluster {
	t.Helper()
	return newFleetWith(t, n, clusterOpts{})
}

// clusterOpts change a cluster before it starts.
type clusterOpts struct {
	engine []func(*worker.EngineConfig)
	server func(*controller.ServerConfig)
	// follow is how a worker follows the controller's build at ctrl.
	follow func(ctrl string, hc *http.Client) *worker.Follow
}

func newFleetWith(t *testing.T, n int, opts clusterOpts) *cluster {
	t.Helper()
	tn := tailnettest.New(ubu1)
	dir := t.TempDir()
	init, err := controller.Init(t.Context(), dir, api.DefaultPort, "", tn)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := statedir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := controller.OpenStore(t.Context(), filepath.Join(dir, "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	c := &cluster{t: t, id: init.ClusterID, tn: tn, store: store, clock: &clock{}}
	addrs := map[string]string{} // worker host name -> its server's address
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
			ControllerNodeID: ubu1.Peer.NodeID, AllowLocalRepos: true,
		}
		for _, w := range opts.engine {
			w(&nd.cfg)
		}
		nd.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if nd.halted.Load() {
				nd.refused.Add(1)
				http.Error(w, "worker stopped", http.StatusServiceUnavailable)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/FollowLog") {
				nd.follows.Add(1)
			}
			if gate := nd.createGate.Load(); gate != nil && strings.HasSuffix(r.URL.Path, "/CreateWorkspace") {
				<-*gate
			}
			(*nd.handler.Load()).ServeHTTP(w, r)
		}))
		t.Cleanup(nd.srv.Close)
		addrs[nd.peer.Hostname] = strings.TrimPrefix(nd.srv.URL, "http://")
		c.nodes = append(c.nodes, nd)
	}
	c.serverCfg = controller.ServerConfig{
		Store: store, Identifier: tn, Cluster: *cfg.Controller, Controller: "ubu1", Now: c.clock.Now, Self: loopback,
		AllowLocalRepos: true,
		// The controller's calls to a worker that is cut off fail.
		Workers:    &http.Client{Transport: partition{next: tn.Client(ubu1.Peer).Transport, cut: c.cutAt}},
		WorkerAddr: func(host string) string { return addrs[host] },
	}
	if opts.server != nil {
		opts.server(&c.serverCfg)
	}
	c.restart()
	c.ctrl = &switchboard{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*c.handler.Load()).ServeHTTP(w, r) })}
	c.ctrlSrv = httptest.NewServer(c.ctrl)
	t.Cleanup(c.ctrlSrv.Close)
	ctrl := strings.TrimPrefix(c.ctrlSrv.URL, "http://")

	for _, nd := range c.nodes {
		hc := &http.Client{Transport: partition{next: tn.Client(nd.peer).Transport, cut: func(string) bool { return nd.cut.Load() }}}
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
		joiner, _, err := api.Controller(ctrl, tn.Client(nd.peer))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := joiner.Join(t.Context(), &pb.JoinRequest{}); err != nil {
			t.Fatal(err)
		}
		if opts.follow != nil {
			nd.follow = opts.follow(ctrl, tn.Client(nd.peer))
		}
		nd.start()
		t.Cleanup(nd.halt)
	}
	c.asWorker, c.reported = c.nodes[0].cluster, c.nodes[0].reported
	// Send an initial heartbeat synchronously so every node is online with its
	// resources populated before the test starts, avoiding CPU-load races
	// against worker.Connect's first background heartbeat (board 7949).
	// Skip when follow is configured so Connect drives the follow check itself.
	if opts.follow == nil {
		c.heartbeat()
	}

	// Sessions start in the tests' project unless a test names another.
	if _, err := store.AddProject(t.Context(), "test", nil, &pb.Stamp{At: time.Now().UnixMilli(), Reason: "the tests' project"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOTBED_PROJECT", "test")
	// Where the tests run says nothing of where sessions bind: they start
	// outside any T3 thread or checkout, so a session binds to nothing and
	// a command names its session, unless a test goes somewhere itself.
	for _, k := range []string{"T3CODE_THREAD_ID", "HOTBED_SESSION", "HOTBED_REPO", "HOTBED_AGENT"} {
		t.Setenv(k, "")
	}
	t.Chdir(t.TempDir())
	c.cli = deps{
		controller: at(ctrl),
		// The CLI runs on the laptop: its tailnet says so, and its calls
		// come from the laptop.
		tailnet: tailnettest.New(tailnet.Self{Peer: laptop}),
		http:    recording(tn.Client(laptop)), stream: recording(tn.Client(laptop)), cache: t.TempDir(),
		sessionsFile: filepath.Join(t.TempDir(), "sessions.json"),
		exe:          func() (string, error) { return "", errors.New("the tests' client has no binary to upgrade") },
		reexec:       func(...string) error { return errors.New("the tests' client runs nothing") },
	}
	return c
}

// cutAt says whether the worker serving at addr is cut off.
func (c *cluster) cutAt(addr string) bool {
	for _, nd := range c.nodes {
		if strings.TrimPrefix(nd.srv.URL, "http://") == addr {
			return nd.cut.Load()
		}
	}
	return false
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

// restart replaces the controller with a new one on the same database, as a
// deploy does: whatever it knows must come from there.
func (c *cluster) restart() {
	c.server = controller.NewServer(c.serverCfg)
	h := c.server.Handler()
	c.handler.Store(&h)
}

// engine is the first worker's engine as it is now.
func (c *cluster) engine() *worker.Engine { return c.nodes[0].eng() }

func (nd *node) eng() *worker.Engine {
	nd.mu.Lock()
	defer nd.mu.Unlock()
	return nd.engine
}

// sshTestID wraps tailnettest.Fake for the test cluster's SSH shell listener:
// external ssh subprocesses open their own loopback TCP sockets without going
// through tn.Client, so WhoIs attributes loopback callers to laptop.
type sshTestID struct {
	*tailnettest.Fake
}

func (s sshTestID) WhoIs(ctx context.Context, remote string) (tailnet.Peer, error) {
	if p, err := s.Fake.WhoIs(ctx, remote); err == nil {
		return p, nil
	}
	return laptop, nil
}

// start opens the worker's engine on its state directory and runs it and
// its connection to the controller.
func (nd *node) start() {
	t := nd.c.t
	eng, err := worker.NewEngine(t.Context(), nd.cfg)
	if err != nil {
		t.Fatal(err)
	}
	sshLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		eng.Close()
		t.Fatal(err)
	}
	sshAddr := sshLn.Addr().(*net.TCPAddr)
	eng.SetShellListen(sshAddr.String(), sshAddr.Port)
	shellSrv := worker.NewShellServer(sshTestID{nd.c.tn}, httpx.Policy{Owner: "ramon@", Self: loopback}, eng)
	h := workerHandler(nd.c.tn, eng)
	nd.handler.Store(&h)
	nd.halted.Store(false)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = shellSrv.Serve(sshLn) }()
	go func() { defer wg.Done(); eng.Run(ctx) }()
	go func() {
		defer wg.Done()
		err := worker.Connect(ctx, eng, nd.cluster, nd.c.id, nd.follow)
		select {
		case nd.reported <- err:
		default: // an earlier end not read yet
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
}

// halt stops the worker; its commands run on, as systemd units do.
func (nd *node) halt() {
	nd.halted.Store(true)
	// Its connections end with it, clients' streams of output too.
	nd.srv.CloseClientConnections()
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
