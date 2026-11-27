// Command dev-cockpit runs a self-contained local cf-controller (in workerd
// via Miniflare) and two fake workers populated with realistic sample data, so
// UI work against the real ConnectRPC API needs no live cluster (board 7862).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cfbundle"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/peercred"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
	"github.com/semistrict/hotbed/internal/worker"
	"github.com/semistrict/hotbed/ui"
)

const devOwner = "dev@local"

var loopbackAddrs = []netip.Addr{
	netip.MustParseAddr("127.0.0.1"),
	netip.MustParseAddr("::1"),
}

type uidLookupFunc func(client, server netip.AddrPort) (uint32, error)

// devTailnet wraps tailnettest.Fake so internal worker clients keep their node
// identities while local browser / Vite proxy requests on loopback are
// authenticated by real OS peer credentials (os.Getuid() only).
type devTailnet struct {
	*tailnettest.Fake
	fallback tailnet.Peer
	mu       sync.RWMutex
	localUID uidLookupFunc
}

func (d *devTailnet) WhoIs(ctx context.Context, remote string) (tailnet.Peer, error) {
	if p, err := d.Fake.WhoIs(ctx, remote); err == nil {
		return p, nil
	}
	if ap, err := netip.ParseAddrPort(remote); err == nil && ap.Addr().Unmap().IsLoopback() {
		return d.fallback, nil
	}
	return tailnet.Peer{}, fmt.Errorf("no tailnet node at %s", remote)
}

func (d *devTailnet) LocalUID(client, server netip.AddrPort) (uint32, error) {
	d.mu.RLock()
	fn := d.localUID
	d.mu.RUnlock()
	return fn(client, server)
}

func (d *devTailnet) setLocalUID(fn uidLookupFunc) {
	d.mu.Lock()
	d.localUID = fn
	d.mu.Unlock()
}

// workerTailnet restricts worker listeners strictly to sockets dialed by the
// in-process controller/seed client (no loopback fallback) and still verifies
// real OS peer credentials so another local user cannot spoof a closed port.
type workerTailnet struct {
	*tailnettest.Fake
	ownerUID uint32
	ctrlTN   *devTailnet
}

func (w *workerTailnet) LocalUID(client, server netip.AddrPort) (uint32, error) {
	uid, err := w.ctrlTN.LocalUID(client, server)
	if err != nil {
		return 0, err
	}
	if uid != w.ownerUID && uid != 0 {
		return uid, nil
	}
	return 0, nil
}

type devHarness struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	url   string
	token string
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "cf-controller", "scripts", "harness.mjs")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not locate cf-controller/scripts/harness.mjs from working directory")
		}
		dir = parent
	}
}

func startDevHarness() (*devHarness, error) {
	root, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	bundleDir, err := cfbundle.Ensure(root)
	if err != nil {
		return nil, err
	}
	script := filepath.Join(root, "cf-controller", "scripts", "harness.mjs")
	cmd := exec.Command("node", script, "--bundle", bundleDir)
	cmd.Dir = filepath.Join(root, "cf-controller")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cf-controller harness: %w", err)
	}
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		if sc.Scan() {
			lineCh <- sc.Text()
			for sc.Scan() {
			}
			return
		}
		if err := sc.Err(); err != nil {
			errCh <- err
		} else {
			errCh <- io.EOF
		}
	}()
	select {
	case line := <-lineCh:
		var ready struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(line), &ready); err != nil || ready.URL == "" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("parse cf-controller harness output %q: %w (stderr: %s)", line, err, stderr.String())
		}
		return &devHarness{cmd: cmd, stdin: stdin, url: ready.URL, token: ready.Token}, nil
	case err := <-errCh:
		_ = cmd.Wait()
		return nil, fmt.Errorf("cf-controller harness exited early (%w): %s", err, stderr.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("timed out waiting for cf-controller harness: %s", stderr.String())
	}
}

func (h *devHarness) Close() {
	if h == nil {
		return
	}
	_ = h.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = h.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
}

func (h *devHarness) doJSON(ctx context.Context, doName, method, path string, reqBody any, out any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.url+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Hotbed-Test-DO", doName)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type devWorker struct {
	peer   tailnet.Peer
	eng    *worker.Engine
	units  *fake.Units
	prof   *fake.Profiler
	srv    *http.Server
	ln     net.Listener
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type devCluster struct {
	addr    string
	root    string
	doName  string
	harness *devHarness
	tn      *devTailnet
	ctrlSrv *http.Server
	ctrlLn  net.Listener
	workers []*devWorker
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func main() {
	addr := flag.String("addr", "127.0.0.1:7120", "listen address for the dev cockpit controller")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dc, err := startDevCluster(ctx, *addr)
	if err != nil {
		slog.Error("start dev-cockpit", "err", err)
		os.Exit(1)
	}
	defer dc.Close()

	fmt.Printf("dev-cockpit listening on http://%s\n", dc.addr)
	fmt.Printf("run `HOTBED_CONTROLLER=%s pnpm -C ui dev` for Vite HMR against this controller\n", dc.addr)
	<-ctx.Done()
}

func startDevCluster(parent context.Context, addr string) (*devCluster, error) {
	root, err := os.MkdirTemp("", "hotbed-dev-cockpit-*")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	dc := &devCluster{
		root:   root,
		doName: fmt.Sprintf("dev-cockpit-%d-%d", os.Getpid(), time.Now().UnixNano()),
		cancel: cancel,
	}
	ok := false
	defer func() {
		if !ok {
			dc.Close()
		}
	}()

	harness, err := startDevHarness()
	if err != nil {
		return nil, err
	}
	dc.harness = harness

	var cfgRes struct {
		ClusterID string `json:"clusterId"`
	}
	if err := harness.doJSON(ctx, dc.doName, http.MethodPost, "/__test/config", map[string]any{
		"allowLocalRepos": true,
	}, &cfgRes); err != nil {
		return nil, fmt.Errorf("configure dev controller DO: %w", err)
	}
	clusterID := cfgRes.ClusterID

	ownerUID := uint32(os.Getuid())
	ubu1Peer := tailnet.Peer{NodeID: "nUbu1", Hostname: "ubu1", Login: devOwner}
	asahiPeer := tailnet.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: devOwner}
	browserPeer := tailnet.Peer{NodeID: "nDevBrowser", Hostname: "localhost", Login: devOwner}

	tn := &devTailnet{
		Fake:     tailnettest.New(tailnet.Self{Peer: ubu1Peer, Addrs: loopbackAddrs}),
		fallback: browserPeer,
		localUID: peercred.SocketUID,
	}
	dc.tn = tn
	workerID := &workerTailnet{Fake: tn.Fake, ownerUID: ownerUID, ctrlTN: tn}

	workerAddrs := map[string]string{}
	specs := []struct {
		peer  tailnet.Peer
		sys   *fake.System
		bench worker.Isolator
	}{
		{
			peer: ubu1Peer,
			sys: &fake.System{
				Arch:     "amd64",
				Mem:      sysinfo.Mem{Total: 64 << 30, Available: 44 << 30, SwapTotal: 16 << 30, SwapFree: 16 << 30},
				CPUShare: sysinfo.CPUShare{Busy: 0.34},
			},
			bench: fake.NewIsolation("12-15", 12),
		},
		{
			peer: asahiPeer,
			sys: &fake.System{
				Arch:     "arm64",
				Mem:      sysinfo.Mem{Total: 24 << 30, Available: 15 << 30, SwapTotal: 8 << 30, SwapFree: 8 << 30},
				CPUShare: sysinfo.CPUShare{Busy: 0.19},
			},
		},
	}

	// Start worker listeners first so the controller knows their addresses.
	for _, sp := range specs {
		units := &fake.Units{}
		prof := fake.NewProfiler()
		wstore := &fake.Store{Root: filepath.Join(root, "vol-"+sp.peer.Hostname)}
		eng, err := worker.NewEngine(ctx, worker.EngineConfig{
			Dir:              filepath.Join(root, "worker-"+sp.peer.Hostname),
			NodeID:           sp.peer.NodeID,
			Host:             sp.peer.Hostname,
			Store:            wstore,
			BaseImage:        fake.BaseImage(wstore),
			Units:            units,
			System:           sp.sys,
			Bench:            sp.bench,
			Inspector:        fake.Inspector{},
			Profiler:         prof,
			ControllerNodeID: ubu1Peer.NodeID,
			AllowLocalRepos:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("new worker %s: %w", sp.peer.Hostname, err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			eng.Close()
			return nil, fmt.Errorf("listen worker %s: %w", sp.peer.Hostname, err)
		}
		srv := &http.Server{
			Handler: worker.Handler(workerID, httpx.Policy{
				Owner: devOwner,
				Self:  loopbackAddrs,
				Nodes: []string{ubu1Peer.NodeID},
			}, eng),
			Protocols:         httpx.Protocols(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		wctx, wcancel := context.WithCancel(ctx)
		dw := &devWorker{peer: sp.peer, eng: eng, units: units, prof: prof, srv: srv, ln: ln, cancel: wcancel}
		dw.wg.Add(2)
		go func() {
			defer dw.wg.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Warn("dev worker server exited", "node", sp.peer.Hostname, "err", err)
			}
		}()
		go func() {
			defer dw.wg.Done()
			eng.Run(wctx)
		}()
		workerAddrs[sp.peer.Hostname] = ln.Addr().String()
		dc.workers = append(dc.workers, dw)
	}

	target, err := url.Parse(harness.url)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	origDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		origDirector(r)
		r.Host = target.Host
	}
	uiHandler := ui.Handler()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/hotbed.v1.") || strings.HasPrefix(p, "/ws/") || p == "/healthz" ||
			strings.HasPrefix(p, "/builds/") || strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/auth/") {
			peer := httpx.Peer(r.Context())
			r2 := r.Clone(r.Context())
			r2.Header.Set("X-Hotbed-Test-DO", dc.doName)
			if r2.Header.Get("Authorization") == "" {
				r2.Header.Set("Authorization", "Bearer "+harness.token)
			}
			r2.Header.Set("X-Hotbed-Node-Id", peer.NodeID)
			r2.Header.Set("X-Hotbed-Host", peer.Hostname)
			if wa := workerAddrs[peer.Hostname]; wa != "" {
				r2.Header.Set("X-Hotbed-Addr", wa)
			}
			proxy.ServeHTTP(w, r2)
			return
		}
		uiHandler.ServeHTTP(w, r)
	})
	gated := httpx.Gate(tn, httpx.Policy{
		Owner:      devOwner,
		Self:       loopbackAddrs,
		Hosts:      []string{"ubu1", "localhost"},
		LocalUsers: []uint32{ownerUID},
	}, inner)

	ctrlLn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen controller %s: %w", addr, err)
	}
	dc.ctrlLn = ctrlLn
	dc.addr = ctrlLn.Addr().String()
	dc.ctrlSrv = &http.Server{
		Handler:           gated,
		Protocols:         httpx.Protocols(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	dc.wg.Add(1)
	go func() {
		defer dc.wg.Done()
		if err := dc.ctrlSrv.Serve(ctrlLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("dev controller server exited", "err", err)
		}
	}()

	// Join, connect, and open the reverse-RPC tunnel for each worker.
	for _, dw := range dc.workers {
		cl, _, err := api.Controller(dc.addr, tn.Client(dw.peer))
		if err != nil {
			return nil, fmt.Errorf("dial controller for %s: %w", dw.peer.Hostname, err)
		}
		if _, err := cl.Join(ctx, &pb.JoinRequest{}); err != nil {
			return nil, fmt.Errorf("join %s: %w", dw.peer.Hostname, err)
		}
		dw.wg.Add(2)
		go func(dw *devWorker, cl hotbedv1connect.ClusterServiceClient) {
			defer dw.wg.Done()
			_ = worker.Connect(ctx, dw.eng, cl, clusterID, nil)
		}(dw, cl)
		go func(dw *devWorker) {
			defer dw.wg.Done()
			_ = worker.Tunnel(
				ctx,
				dw.eng,
				"http://"+dc.addr,
				harness.token,
				tailnet.Self{Peer: dw.peer},
				workerAddrs[dw.peer.Hostname],
				ubu1Peer.NodeID,
				devOwner,
				tn.Client(dw.peer),
			)
		}(dw)
	}

	if err := waitUntil(ctx, 15*time.Second, func() (bool, error) {
		var res struct {
			NodeIDs []string `json:"nodeIds"`
		}
		if err := harness.doJSON(ctx, dc.doName, http.MethodGet, "/__test/inspect?kind=connectedWorkers", nil, &res); err != nil {
			return false, err
		}
		for _, dw := range dc.workers {
			if !slices.Contains(res.NodeIDs, dw.peer.NodeID) {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("wait for worker tunnels: %w", err)
	}

	if err := dc.seed(ctx, tn.Client(browserPeer), tn.Client(ubu1Peer)); err != nil {
		return nil, fmt.Errorf("seed dev cluster: %w", err)
	}
	ok = true
	return dc, nil
}

func (dc *devCluster) Close() {
	if dc.cancel != nil {
		dc.cancel()
	}
	for _, dw := range dc.workers {
		for _, c := range dw.eng.Commands() {
			if api.Live(c.State) {
				_ = dw.units.Stop(context.Background(), "hotbed-"+c.Id)
			}
		}
		dw.cancel()
		_ = dw.srv.Close()
		dw.wg.Wait()
		_ = dw.eng.Close()
	}
	if dc.ctrlSrv != nil {
		_ = dc.ctrlSrv.Close()
	}
	dc.wg.Wait()
	if dc.harness != nil {
		dc.harness.Close()
	}
	if dc.root != "" {
		_ = os.RemoveAll(dc.root)
	}
}

func (dc *devCluster) seed(ctx context.Context, hc, workerHC *http.Client) error {
	conn, err := api.Dial(dc.addr, api.DefaultPort, hc)
	if err != nil {
		return err
	}
	cluster := hotbedv1connect.NewClusterServiceClient(conn)
	sessions := hotbedv1connect.NewSessionServiceClient(conn)
	workspaces := hotbedv1connect.NewWorkspaceServiceClient(conn)
	questions := hotbedv1connect.NewQuestionServiceClient(conn)
	reviews := hotbedv1connect.NewReviewServiceClient(conn)
	schedules := hotbedv1connect.NewScheduleServiceClient(conn)

	// Wait until both workers have reported online.
	if err := waitUntil(ctx, 15*time.Second, func() (bool, error) {
		res, err := cluster.ListNodes(ctx, &pb.ListNodesRequest{})
		if err != nil || len(res.Nodes) < len(dc.workers) {
			return false, err
		}
		for _, n := range res.Nodes {
			if !n.Online {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("wait for workers online: %w", err)
	}

	originDir, headSHA, err := initSampleRepo(filepath.Join(dc.root, "origin", "cladefs"))
	if err != nil {
		return err
	}

	if _, err := cluster.AddProject(ctx, &pb.AddProjectRequest{Name: "cladefs", Repos: []string{originDir}}); err != nil {
		return fmt.Errorf("add project: %w", err)
	}
	if _, err := cluster.PutRepo(ctx, &pb.PutRepoRequest{
		Project: "cladefs",
		Repo:    &pb.Repo{Id: "cladefs", Url: originDir, Branch: "main"},
	}); err != nil {
		return fmt.Errorf("put repo: %w", err)
	}
	if _, err := cluster.PutStep(ctx, &pb.PutStepRequest{
		Project: "cladefs",
		Step: &pb.Step{
			Name:      "build",
			Script:    "printf '   Compiling cladefs-core v0.4.0\\n   Compiling cladefs-fuse v0.4.0\\n   Compiling cladefs-cli v0.4.0\\n'",
			EveryPull: true,
		},
	}); err != nil {
		return fmt.Errorf("put step: %w", err)
	}

	// Start two live sessions and place a workspace on each worker.
	if _, err := sessions.StartSession(ctx, &pb.StartSessionRequest{Id: "brave-otter", Project: "cladefs", Agent: "builder"}); err != nil {
		return fmt.Errorf("start brave-otter: %w", err)
	}
	if _, err := sessions.StartSession(ctx, &pb.StartSessionRequest{Id: "calm-heron", Project: "cladefs", Agent: "reviewer"}); err != nil {
		return fmt.Errorf("start calm-heron: %w", err)
	}

	wsUbu1, err := workspaces.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		SessionId: "brave-otter",
		Placement: &pb.Placement{Node: "ubu1"},
	})
	if err != nil {
		return fmt.Errorf("create ubu1 workspace: %w", err)
	}
	wsAsahi, err := workspaces.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		SessionId: "calm-heron",
		Placement: &pb.Placement{Node: "asahimbp"},
	})
	if err != nil {
		return fmt.Errorf("create asahimbp workspace: %w", err)
	}

	for _, id := range []string{wsUbu1.Id, wsAsahi.Id} {
		if err := waitUntil(ctx, 20*time.Second, func() (bool, error) {
			w, err := workspaces.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: id})
			if err != nil {
				return false, err
			}
			if w.State == pb.WorkspaceState_WORKSPACE_STATE_FAILED {
				return false, fmt.Errorf("workspace %s failed: %s", id, w.Reason)
			}
			return w.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE || w.State == pb.WorkspaceState_WORKSPACE_STATE_ACTIVE, nil
		}); err != nil {
			return fmt.Errorf("wait workspace %s ready: %w", id, err)
		}
	}

	// Run two short commands (one succeeded build, one failed test) so history is populated.
	// Through the controller, which relays it to the workspace's worker, as
	// every client's call is.
	runOnWorker := func(_ string, req *pb.RunCommandRequest) (*pb.Command, error) {
		return workspaces.RunCommand(ctx, req)
	}
	if _, err := runOnWorker(wsUbu1.NodeAddr, &pb.RunCommandRequest{
		WorkspaceId: wsUbu1.Id,
		Repo:        "cladefs",
		Kind:        "build",
		Args:        []string{"sh", "-c", "printf '   Compiling cladefs-core v0.4.0\\n   Compiling cladefs-fuse v0.4.0\\nFinished dev profile in 1.4s\\n'"},
	}); err != nil {
		return fmt.Errorf("run finished build: %w", err)
	}
	if _, err := runOnWorker(wsAsahi.NodeAddr, &pb.RunCommandRequest{
		WorkspaceId: wsAsahi.Id,
		Repo:        "cladefs",
		Kind:        "test",
		Args:        []string{"sh", "-c", "printf 'running 4 tests\\n--- FAIL: TestPageCacheEviction (0.04s)\\n    eviction_test.go:88: stale pin after concurrent read\\ntest result: FAILED. 3 passed; 1 failed\\n'; exit 1"},
	}); err != nil {
		return fmt.Errorf("run finished test: %w", err)
	}
	if err := waitUntil(ctx, 15*time.Second, func() (bool, error) {
		snap, err := cluster.GetSnapshot(ctx, &pb.GetSnapshotRequest{})
		return len(snap.GetHistory()) >= 2, err
	}); err != nil {
		return fmt.Errorf("wait finished commands in history: %w", err)
	}

	// Start two long-running live commands (a test suite + a simulated dev
	// server). The suite streams a line every half second, so following a
	// live log, and reading back through one as it grows, can be tried.
	liveTest, err := runOnWorker(wsUbu1.NodeAddr, &pb.RunCommandRequest{
		WorkspaceId: wsUbu1.Id,
		Repo:        "cladefs",
		Kind:        "test",
		Args:        []string{"sh", "-c", `echo 'running integration suite...'; i=0; while :; do i=$((i+1)); echo "=== RUN   TestIntegration/case_$i"; echo "--- PASS: TestIntegration/case_$i (0.0$((i % 10))s)"; sleep 0.5; done`},
	})
	if err != nil {
		return fmt.Errorf("run live test: %w", err)
	}
	liveServer, err := runOnWorker(wsAsahi.NodeAddr, &pb.RunCommandRequest{
		WorkspaceId: wsAsahi.Id,
		Repo:        "cladefs",
		Kind:        "server",
		Args:        []string{"sh", "-c", "echo 'dev server running (simulated fixture)'; sleep 3600"},
	})
	if err != nil {
		return fmt.Errorf("run live server: %w", err)
	}
	if err := waitUntil(ctx, 15*time.Second, func() (bool, error) {
		snap, err := cluster.GetSnapshot(ctx, &pb.GetSnapshotRequest{})
		if err != nil {
			return false, err
		}
		running := 0
		for _, c := range snap.Commands {
			if c.State == pb.CommandState_COMMAND_STATE_RUNNING {
				running++
			}
		}
		return running >= 2, nil
	}); err != nil {
		return fmt.Errorf("wait live commands running: %w", err)
	}

	dc.seedProfiles(liveTest.Id, liveServer.Id)

	// Seed two open questions across the two sessions.
	if _, err := questions.AddQuestions(ctx, &pb.AddQuestionsRequest{
		SessionId: "brave-otter",
		Questions: []*pb.Prompt{{
			Question: "How should eviction handle pinned pages during flush?",
			Header:   "Eviction",
			Body:     "Under concurrent `write` + `fsync`, the page cache can hold a pin while the sweeper scans the LRU tail.",
			Options: []*pb.PromptOption{
				{Label: "Skip pinned pages", Description: "Keep sweep lock-free and retry on the next pass.", Implemented: true},
				{Label: "Drain pins before sweep", Description: "Wait on in-flight writes so memory drops deterministically.", Recommended: true},
			},
		}},
	}); err != nil {
		return fmt.Errorf("add question 1: %w", err)
	}
	if _, err := questions.AddQuestions(ctx, &pb.AddQuestionsRequest{
		SessionId: "calm-heron",
		Questions: []*pb.Prompt{{
			Question:    "Which target architectures should nightly CI build?",
			Header:      "Targets",
			MultiSelect: true,
			Options: []*pb.PromptOption{
				{Label: "linux/amd64", Description: "Primary bench and build hosts.", Implemented: true},
				{Label: "linux/arm64", Description: "Apple Silicon Linux workers.", Implemented: true},
				{Label: "darwin/arm64", Description: "Local developer CLI builds.", Recommended: true},
			},
		}},
	}); err != nil {
		return fmt.Errorf("add question 2: %w", err)
	}

	// Seed one finished review with two reviewers and two findings.
	rev, err := reviews.RequestReview(ctx, &pb.RequestReviewRequest{
		SessionId:    "brave-otter",
		Model:        "gemini-4-neon",
		Repo:         originDir,
		Title:        "Lock-free page cache eviction",
		Branch:       "main",
		Head:         headSHA,
		Instructions: "Check concurrency around page pin release and LRU tail scans.",
	})
	if err != nil {
		return fmt.Errorf("request review: %w", err)
	}
	if _, err := reviews.StartReview(ctx, &pb.StartReviewRequest{
		Id:        rev.Id,
		SessionId: "calm-heron",
		Model:     "claude-opus-4-6",
	}); err != nil {
		return fmt.Errorf("start review: %w", err)
	}
	if _, err := reviews.AddFindings(ctx, &pb.AddFindingsRequest{
		ReviewId:  rev.Id,
		SessionId: "calm-heron",
		Findings: []*pb.ReportedFinding{
			{
				ShortSummary:    "Eviction drops dirty page before writeback completes",
				Summary:         "The LRU sweeper checks the pin count without acquiring the writeback fence, allowing a page queued for flush to be evicted early.",
				FailureScenario: "Concurrent fsync and memory pressure silently loses recently written blocks.",
				Category:        "concurrency",
				Severity:        "important",
				Verdict:         "CONFIRMED",
				File:            "src/cache/evict.rs",
				Line:            142,
			},
			{
				ShortSummary: "Rename sweep_budget parameter to max_pages",
				Summary:      "The parameter counts pages rather than bytes; naming it max_pages makes the unit clear at call sites.",
				Category:     "clarity",
				Severity:     "nit",
				Verdict:      "PLAUSIBLE",
				File:         "src/cache/evict.rs",
				Line:         38,
			},
		},
	}); err != nil {
		return fmt.Errorf("add findings: %w", err)
	}
	if _, err := reviews.FinishReview(ctx, &pb.FinishReviewRequest{
		Id:        rev.Id,
		SessionId: "calm-heron",
		Summary:   "Found one confirmed eviction race during flush and one minor naming nit.",
	}); err != nil {
		return fmt.Errorf("finish review: %w", err)
	}

	// Seed one scheduled command.
	if _, err := schedules.PutSchedule(ctx, &pb.PutScheduleRequest{
		Schedule: &pb.ScheduledCommand{
			Name:    "nightly-nextest",
			Project: "cladefs",
			Repo:    "cladefs",
			Cron:    "0 3 * * *",
			Command: &pb.RunCommandRequest{
				Args: []string{"cargo", "nextest", "run", "--workspace"},
				Kind: "test",
			},
		},
	}); err != nil {
		return fmt.Errorf("put schedule: %w", err)
	}

	return nil
}

func (dc *devCluster) seedProfiles(ubu1CmdID, asahiCmdID string) {
	now := time.Now()
	record := func(p *fake.Profiler, cmdID, comm string, count int, frames []profiler.ResolvedFrame) {
		for i := range count {
			p.Store.Record(profiler.Sample{
				CommandID: cmdID,
				Timestamp: now.Add(-time.Duration(count-i) * 250 * time.Millisecond),
				Comm:      comm,
				Frames:    frames,
			})
		}
	}
	for _, dw := range dc.workers {
		switch dw.peer.Hostname {
		case "ubu1":
			record(dw.prof, ubu1CmdID, "cladefs-test", 24, []profiler.ResolvedFrame{
				{Name: "std::rt::lang_start", Module: "cladefs-test", FrameType: "native"},
				{Name: "cladefs_core::cache::LruCache::evict_batch", Module: "cladefs-test", FrameType: "native"},
				{Name: "cladefs_core::cache::PageTable::unpin", Module: "cladefs-test", FrameType: "native"},
			})
			record(dw.prof, ubu1CmdID, "cladefs-test", 14, []profiler.ResolvedFrame{
				{Name: "std::rt::lang_start", Module: "cladefs-test", FrameType: "native"},
				{Name: "cladefs_fuse::mount::Session::dispatch", Module: "cladefs-test", FrameType: "native"},
				{Name: "cladefs_core::wal::Journal::append", Module: "cladefs-test", FrameType: "native"},
				{Name: "crc32c_hw::update", Module: "cladefs-test", FrameType: "native"},
			})
			record(dw.prof, ubu1CmdID, "cladefs-test", 8, []profiler.ResolvedFrame{
				{Name: "cladefs_core::wal::Journal::sync", Module: "cladefs-test", FrameType: "native"},
				{Name: "__x64_sys_fdatasync", Module: "vmlinux", FrameType: "kernel"},
				{Name: "vfs_fsync_range", Module: "vmlinux", FrameType: "kernel"},
			})
			record(dw.prof, "", "hotbed-worker", 6, []profiler.ResolvedFrame{
				{Name: "runtime.mcall", Module: "hotbed", FrameType: "go"},
				{Name: "ep_poll", Module: "vmlinux", FrameType: "kernel"},
			})
		case "asahimbp":
			record(dw.prof, asahiCmdID, "cladefs-dev", 18, []profiler.ResolvedFrame{
				{Name: "tokio::runtime::worker::run", Module: "cladefs-dev", FrameType: "native"},
				{Name: "cladefs_http::router::handle_request", Module: "cladefs-dev", FrameType: "native"},
				{Name: "cladefs_core::index::Snapshot::lookup", Module: "cladefs-dev", FrameType: "native"},
			})
			record(dw.prof, asahiCmdID, "cladefs-dev", 7, []profiler.ResolvedFrame{
				{Name: "tokio::runtime::worker::run", Module: "cladefs-dev", FrameType: "native"},
				{Name: "vfs_read", Module: "vmlinux", FrameType: "kernel"},
			})
			record(dw.prof, "", "hotbed-worker", 5, []profiler.ResolvedFrame{
				{Name: "runtime.mcall", Module: "hotbed", FrameType: "go"},
				{Name: "do_epoll_wait", Module: "vmlinux", FrameType: "kernel"},
			})
		}
	}
}

func initSampleRepo(dir string) (string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# cladefs\n\nSample repository for dev-cockpit.\n"), 0o644); err != nil {
		return "", "", err
	}
	runGit := func(args ...string) (string, error) {
		fullArgs := append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)
		cmd := exec.Command("git", fullArgs...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=dev", "GIT_AUTHOR_EMAIL=dev@local",
			"GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL=dev@local",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := runGit("init", "-q", "-b", "main"); err != nil {
		return "", "", err
	}
	if _, err := runGit("add", "README.md"); err != nil {
		return "", "", err
	}
	if _, err := runGit("commit", "-qm", "initial sample commit"); err != nil {
		return "", "", err
	}
	sha, err := runGit("rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	return dir, sha, nil
}

func waitUntil(ctx context.Context, timeout time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
