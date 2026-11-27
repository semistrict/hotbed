// Command dev-cockpit runs a self-contained local cf-controller (in workerd
// via Miniflare) and two fake workers populated with realistic sample data, so
// UI work against the real ConnectRPC API needs no live cluster (board 7862).
// The cockpit is served by the Worker itself, from ui/dist, through the same
// asset path as production; every request reaches it through one proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/worker"
)

const devOwner = "dev@local"

type devWorker struct {
	peer   httpx.Peer
	eng    *worker.Engine
	units  *fake.Units
	prof   *fake.Profiler
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type devCluster struct {
	addr    string
	root    string
	doName  string
	harness *cfbundle.Harness
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

	uiDir, err := builtUIDir()
	if err != nil {
		slog.Error("find the cockpit UI", "err", err)
		os.Exit(1)
	}
	dc, err := startDevCluster(ctx, *addr, uiDir)
	if err != nil {
		slog.Error("start dev-cockpit", "err", err)
		os.Exit(1)
	}
	defer dc.Close()

	fmt.Printf("dev-cockpit listening on http://%s\n", dc.addr)
	fmt.Printf("run `HOTBED_CONTROLLER=%s pnpm -C ui dev` for Vite HMR against this controller\n", dc.addr)
	<-ctx.Done()
}

// builtUIDir is ui/dist, where `make ui` builds the cockpit, or "" (with a
// warning: the API still works, which is all `pnpm -C ui dev` needs) when it
// is not built.
func builtUIDir() (string, error) {
	root, err := cfbundle.FindRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "ui", "dist")
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		slog.Warn("the cockpit UI is not built, so only the API is served: run `make ui` to serve the cockpit too", "dir", dir, "err", err)
		return "", nil
	}
	return dir, nil
}

// startDevCluster starts the dev cluster listening on addr. uiDir, when not
// empty, is the built cockpit the Worker serves as its static assets.
func startDevCluster(parent context.Context, addr, uiDir string) (*devCluster, error) {
	// It admits whoever reaches it as the owner, and its workers run real
	// processes: only this machine may.
	if host, _, err := net.SplitHostPort(addr); err != nil || !isLoopbackHost(host) {
		return nil, fmt.Errorf("dev-cockpit listens only on loopback, not %q: give -addr 127.0.0.1:PORT", addr)
	}
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

	harness, err := cfbundle.StartHarness(ctx, cfbundle.HarnessOptions{Assets: uiDir})
	if err != nil {
		return nil, err
	}
	dc.harness = harness

	var cfgRes struct {
		ClusterID string `json:"clusterId"`
	}
	if err := harness.DoJSON(ctx, dc.doName, http.MethodPost, "/__test/config", map[string]any{
		"allowLocalRepos": true,
	}, &cfgRes); err != nil {
		return nil, fmt.Errorf("configure dev controller DO: %w", err)
	}
	clusterID := cfgRes.ClusterID

	ubu1Peer := httpx.Peer{NodeID: "nUbu1", Hostname: "ubu1", Login: devOwner}
	asahiPeer := httpx.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: devOwner}
	browserPeer := httpx.Peer{NodeID: "nDevBrowser", Hostname: "localhost", Login: devOwner}
	// as is a client whose calls name peer's node to the controller, as a
	// worker's do; a call that names none is the browser's.
	as := func(peer httpx.Peer) *http.Client {
		return &http.Client{Transport: api.AuthRoundTripper(&http.Transport{}, harness.Token(), peer.NodeID, peer.Hostname)}
	}

	specs := []struct {
		peer  httpx.Peer
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

	// Each worker reaches the controller, and the controller it, only through
	// the worker's tunnel: none listens of its own.
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
		wctx, wcancel := context.WithCancel(ctx)
		dw := &devWorker{peer: sp.peer, eng: eng, units: units, prof: prof, cancel: wcancel}
		dw.wg.Add(1)
		go func() {
			defer dw.wg.Done()
			eng.Run(wctx)
		}()
		dc.workers = append(dc.workers, dw)
	}

	target, err := url.Parse(harness.URL())
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
	// Every request, the cockpit's pages and assets included, goes to the
	// Worker, which serves the UI itself. The proxy speaks for its caller:
	// as the node a worker's request names, or as the owner (a browser).
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.Header.Set("X-Hotbed-Test-DO", dc.doName)
		if r2.Header.Get("Authorization") == "" {
			r2.Header.Set("Authorization", "Bearer "+harness.Token())
		}
		if r2.Header.Get("X-Hotbed-Node-Id") == "" {
			r2.Header.Set("X-Hotbed-Node-Id", browserPeer.NodeID)
			r2.Header.Set("X-Hotbed-Host", browserPeer.Hostname)
		}
		proxy.ServeHTTP(w, r2)
	})

	ctrlLn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen controller %s: %w", addr, err)
	}
	dc.ctrlLn = ctrlLn
	dc.addr = ctrlLn.Addr().String()
	dc.ctrlSrv = &http.Server{
		Handler:           sameMachineBrowser(inner),
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
		cl, _, err := api.Controller(dc.addr, as(dw.peer))
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
				harness.Token(),
				dw.peer,
				ubu1Peer.NodeID,
				devOwner,
				as(dw.peer),
			)
		}(dw)
	}

	if err := waitUntil(ctx, 15*time.Second, func() (bool, error) {
		var res struct {
			NodeIDs []string `json:"nodeIds"`
		}
		if err := harness.DoJSON(ctx, dc.doName, http.MethodGet, "/__test/inspect?kind=connectedWorkers", nil, &res); err != nil {
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

	if err := dc.seed(ctx, &http.Client{}, as(ubu1Peer)); err != nil {
		return nil, fmt.Errorf("seed dev cluster: %w", err)
	}
	ok = true
	return dc, nil
}

// sameMachineBrowser refuses what a web page in the owner's browser could
// send this loopback server from another site: a request whose Host is not
// a loopback name (DNS rebinding) or that the browser marks cross-site.
func sameMachineBrowser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, fmt.Sprintf("host %q is not this machine's loopback", r.Host), http.StatusForbidden)
			return
		}
		if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, fmt.Sprintf("cross-site browser request (%s) is not allowed", site), http.StatusForbidden)
			return
		}
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !isLoopbackHost(u.Host) {
				http.Error(w, fmt.Sprintf("origin %q is not this machine's loopback", origin), http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost says whether hostport names this machine's loopback:
// localhost or a loopback address, with or without a port.
func isLoopbackHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
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
		dw.wg.Wait()
		_ = dw.eng.Close()
	}
	if dc.ctrlSrv != nil {
		_ = dc.ctrlSrv.Close()
	}
	dc.wg.Wait()
	if dc.harness != nil {
		if err := dc.harness.Close(); err != nil {
			slog.Warn("stop the cf-controller harness", "err", err)
		}
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
	board := hotbedv1connect.NewBoardServiceClient(conn)

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

	// Start two live sessions and place a workspace on each worker. brave-otter
	// runs in a T3 Code thread, so a session badge's card has a thread to open.
	if _, err := sessions.StartSession(ctx, &pb.StartSessionRequest{
		Id:              "brave-otter",
		Project:         "cladefs",
		Agent:           "builder",
		Model:           "claude-opus-5-5",
		T3CodeThreadId:  "b7299e2a-f886-4442-9868-accf5d6075e3",
		T3CodeThreadUrl: "https://app.t3.codes/threads/dev-env/b7299e2a-f886-4442-9868-accf5d6075e3",
	}); err != nil {
		return fmt.Errorf("start brave-otter: %w", err)
	}
	if _, err := sessions.StartSession(ctx, &pb.StartSessionRequest{Id: "calm-heron", Project: "cladefs", Agent: "reviewer"}); err != nil {
		return fmt.Errorf("start calm-heron: %w", err)
	}

	wsUbu1, err := workspaces.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		SessionId: "brave-otter",
		Placement: &pb.Placement{Arch: "amd64"},
	})
	if err != nil {
		return fmt.Errorf("create ubu1 workspace: %w", err)
	}
	wsAsahi, err := workspaces.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		SessionId: "calm-heron",
		Placement: &pb.Placement{Arch: "arm64"},
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
	runOnWorker := func(req *pb.RunCommandRequest) (*pb.Command, error) {
		return workspaces.RunCommand(ctx, req)
	}
	if _, err := runOnWorker(&pb.RunCommandRequest{
		WorkspaceId: wsUbu1.Id,
		Repo:        "cladefs",
		Kind:        "build",
		Args:        []string{"sh", "-c", "printf '   Compiling cladefs-core v0.4.0\\n   Compiling cladefs-fuse v0.4.0\\nFinished dev profile in 1.4s\\n'"},
	}); err != nil {
		return fmt.Errorf("run finished build: %w", err)
	}
	if _, err := runOnWorker(&pb.RunCommandRequest{
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
	liveTest, err := runOnWorker(&pb.RunCommandRequest{
		WorkspaceId: wsUbu1.Id,
		Repo:        "cladefs",
		Kind:        "test",
		Args:        []string{"sh", "-c", `echo 'running integration suite...'; i=0; while :; do i=$((i+1)); echo "=== RUN   TestIntegration/case_$i"; echo "--- PASS: TestIntegration/case_$i (0.0$((i % 10))s)"; sleep 0.5; done`},
	})
	if err != nil {
		return fmt.Errorf("run live test: %w", err)
	}
	liveServer, err := runOnWorker(&pb.RunCommandRequest{
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

	// Seed a board task brave-otter has claimed.
	if _, err := board.CreateBoardTask(ctx, &pb.CreateBoardTaskRequest{
		Project:   "cladefs",
		Text:      "cladefsd: serve the extent device over vhost-user\n\nA stock front-end with memfd guest RAM.",
		Claim:     true,
		SessionId: "brave-otter",
	}); err != nil {
		return fmt.Errorf("create board task: %w", err)
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
