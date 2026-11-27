// Package worker is a node that runs work for the cluster: it keeps warm
// bases of the cluster's repos, forks them into workspaces, and runs
// agents' commands in them.
package worker

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/daemonlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/version"
	"github.com/semistrict/hotbed/internal/vm"
)

// JoinOptions are how a new worker will run.
type JoinOptions struct {
	Dir            string
	Controller     string
	User           string
	Dataset        string
	UnitProperties []string
	Slice          string
	Token          string
}

// JoinResult describes a worker that joined.
type JoinResult struct {
	ClusterID  string   `json:"clusterId"`
	Dir        string   `json:"dir"`
	Controller string   `json:"controller"`
	Storage    string   `json:"storage"`
	User       string   `json:"user"`
	Node       string   `json:"node"`
	Next       []string `json:"next"`
}

// Check verifies a machine can be a worker and says which storage it has.
type Check func(ctx context.Context, o JoinOptions) (storage string, err error)

// lookupOSUser resolves a host username; swapped in tests to cover non-"root"
// accounts that resolve to uid 0 or gid 0.
var lookupOSUser = user.Lookup

// resolveCommandUser looks up the Unix user commands run as and rejects
// missing accounts or accounts resolving to uid 0 or gid 0.
func resolveCommandUser(name string) (uid, gid int, err error) {
	if name == "" {
		return 0, 0, api.Errorf(api.CodeInvalidArgs, "command user is required").
			WithSuggestion("pass a non-root user with --user")
	}
	u, err := lookupOSUser(name)
	if err != nil {
		return 0, 0, api.Errorf(api.CodeInvalidArgs, "no user %q to run commands as", name)
	}
	uid, err = strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid %q for user %q: %w", u.Uid, name, err)
	}
	gid, err = strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid %q for user %q: %w", u.Gid, name, err)
	}
	if uid == 0 {
		return 0, 0, api.Errorf(api.CodeInvalidArgs, "command user %q resolves to uid 0 (root); commands must run as a non-root user", name).
			WithSuggestion("pass a non-root user with --user")
	}
	if gid == 0 {
		return 0, 0, api.Errorf(api.CodeInvalidArgs, "command user %q resolves to gid 0 (root); commands must run as a non-root user", name).
			WithSuggestion("pass a non-root user with --user")
	}
	return uid, gid, nil
}

// CheckMachine is the real Check: Linux, root, a user to run commands as,
// and a state directory on a filesystem that can fork.
func CheckMachine(ctx context.Context, o JoinOptions) (string, error) {
	switch runtime.GOOS {
	case "linux":
		if os.Geteuid() != 0 {
			return "", api.Errorf(api.CodeInvalidArgs, "a worker runs as root, to fork volumes and start systemd units").
				WithSuggestion("run it with sudo; commands still run as --user")
		}
		if o.Slice != "" {
			if err := unit.CheckSlice(o.Slice); err != nil {
				return "", api.Errorf(api.CodeInvalidArgs, "%v", err)
			}
		}
	case "darwin":
		// A Mac's worker runs as its user: every command is a VM of
		// machined's, which needs no root (internal/macworker).
		if os.Geteuid() == 0 {
			return "", api.Errorf(api.CodeInvalidArgs, "a macOS worker runs as the Mac's user, not root").
				WithSuggestion("run it without sudo")
		}
	default:
		return "", api.Errorf(api.CodePlatformNotSupported, "workers need Linux or macOS; this is %s", runtime.GOOS).
			WithSuggestion("run the controller or the client here, and workers on Linux machines or Macs")
	}
	if _, _, err := resolveCommandUser(o.User); err != nil {
		return "", err
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return "", err
	}
	s, err := storage.Open(ctx, o.Dir, o.Dataset)
	if err != nil {
		return "", api.Errorf(api.CodeInvalidArgs, "%v", err).WithSuggestion("put the state directory on btrfs or zfs")
	}
	return s.Kind(), nil
}

// runtimeEnabled says cfg enables a runtime the worker can serve with.
func runtimeEnabled(cfg statedir.WorkerConfig) bool {
	return vmEnabled(cfg) || (cfg.CladefsFUSE != nil && cfg.CladefsFUSE.Enabled)
}

// vmEnabled says cfg runs workspaces in VMs. It wins over cladefs_fuse when
// both are enabled.
func vmEnabled(cfg statedir.WorkerConfig) bool { return cfg.VM != nil && cfg.VM.Enabled }

// openLocalRootfs opens what a worker of cfg's runtime keeps on its own
// filesystem in dir: the store of its workspaces' volumes, and the shared
// base rootfs cold bases clone from (nil when it keeps none).
//
// A VM worker's workspaces are all cells cladefsd makes: their rootfs is
// imported into cladefsd and inspected inside hotbed-guestd, never on the
// host. It keeps no volumes and no local rootfs, so its state directory may
// be on any filesystem (ext4 and xfs too) and it waits on no ZFS pool. A
// cladefs host-FUSE worker clones bases from a local base image and traces
// its commands against that rootfs by host chroot, so it needs btrfs or ZFS:
// openCoW opens them, waiting while it cannot.
func openLocalRootfs(ctx context.Context, cfg statedir.WorkerConfig, dir string, spec baseimage.Spec, openCoW func(context.Context) (storage.Store, error)) (storage.Store, *baseimage.Manager, error) {
	if vmEnabled(cfg) {
		return storage.NewDir(dir), nil, nil
	}
	store, err := openCoW(ctx)
	if err != nil {
		return nil, nil, err
	}
	return store, &baseimage.Manager{
		Store:     store,
		Spec:      spec,
		Provision: baseimage.SystemdProvisioner(cmp.Or(cfg.Slice, unit.DefaultSlice), filepath.Join(dir, "logs")),
	}, nil
}

// refreshControllerConfig fetches the controller's effective config for
// this worker and caches it in dir, as a heartbeat does.
func refreshControllerConfig(ctx context.Context, cluster hotbedv1connect.ClusterServiceClient, dir, hostname string) (*pb.WorkerConfig, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := cluster.GetWorkerConfig(cctx, &pb.GetWorkerConfigRequest{Node: hostname})
	if err != nil {
		return nil, err
	}
	fresh := resp.GetEffective()
	if err := SaveControllerConfig(dir, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

// Join makes o.Dir the state directory of a worker in the cluster that the
// controller at o.Controller runs.
func Join(ctx context.Context, o JoinOptions, hc *http.Client, check Check) (JoinResult, error) {
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return JoinResult{}, err
	}
	o.Dir = dir
	if !api.IsRemoteURL(o.Controller) {
		return JoinResult{}, api.Errorf(api.CodeInvalidArgs, "the controller is a URL (https://...): %q is none", o.Controller)
	}
	controller, err := api.ControllerAddr(o.Controller)
	if err != nil {
		return JoinResult{}, err
	}
	if _, _, err := resolveCommandUser(o.User); err != nil {
		return JoinResult{}, err
	}
	if err := statedir.Prepare(dir); err != nil {
		return JoinResult{}, err
	}
	kind, err := check(ctx, o)
	if err != nil {
		return JoinResult{}, err
	}
	// The node's own ID and name, which the controller binds its worker
	// token to.
	nodeID, err := newNodeID()
	if err != nil {
		return JoinResult{}, err
	}
	host := hostName()
	cluster, _, err := api.Controller(controller, hc, api.AuthInterceptor(o.Token, nodeID, host))
	if err != nil {
		return JoinResult{}, err
	}
	joined, err := cluster.Join(ctx, &pb.JoinRequest{})
	if err != nil {
		return JoinResult{}, api.FromConnect(err)
	}
	workerToken := o.Token
	if joined.WorkerToken != "" {
		workerToken = joined.WorkerToken
	}
	cfg := statedir.WorkerConfig{
		ClusterID: joined.ClusterId, Controller: controller, User: o.User, Dataset: o.Dataset,
		UnitProperties: o.UnitProperties, Slice: o.Slice, ControllerNodeID: "cf:" + joined.ClusterId,
		Token: workerToken, NodeID: nodeID, Host: host,
	}
	if err := statedir.Commit(dir, statedir.Config{Worker: &cfg}); err != nil {
		return JoinResult{}, err
	}
	return JoinResult{
		ClusterID: joined.ClusterId, Dir: dir, Controller: controller, Storage: kind, User: o.User,
		Node: joined.Node.GetHostname(), Next: []string{"sudo hotbed serve " + dir},
	}, nil
}

// Run runs the worker in dir until ctx ends: the engine, the server the
// controller calls, and the reports to the controller.
func Run(ctx context.Context, dir string, cfg statedir.WorkerConfig, hc *http.Client) error {
	uid, gid, err := resolveCommandUser(cfg.User)
	if err != nil {
		return err
	}
	if !api.IsRemoteURL(cfg.Controller) {
		return api.Errorf(api.CodeInvalidArgs, "this worker joined a controller by host name (%s), which is retired", cfg.Controller).
			WithSuggestion("join the Cloudflare controller: hotbed worker init https://...")
	}
	ctrlBaseURL, err := api.ControllerBaseURL(cfg.Controller)
	if err != nil {
		return err
	}
	if cfg.NodeID == "" || cfg.Host == "" || cfg.ControllerNodeID == "" {
		return api.Errorf(api.CodeInvalidArgs, "this worker's %s records no node ID, host or controller node: it joined before workers named themselves", statedir.Path(dir)).
			WithSuggestion("join again: hotbed worker init " + cfg.Controller)
	}

	dlogs, err := daemonlog.Open(filepath.Join(dir, "daemon-logs"), daemonlog.DefaultConfig())
	if err != nil {
		return err
	}
	restoreLogs := daemonlog.Install(dlogs)
	defer func() {
		restoreLogs()
		_ = dlogs.Close()
	}()

	self := httpx.Peer{NodeID: cfg.NodeID, Hostname: cfg.Host, Login: cfg.Owner}
	if hc == nil {
		hc = api.NewResilientHTTPClient(60 * time.Second)
	} else if hc.Transport == nil {
		cloned := *hc
		cloned.Transport = api.NewResilientTransport(nil)
		hc = &cloned
	}
	var ctrlInterceptors []connect.ClientInterceptor
	if cfg.Token != "" {
		ctrlInterceptors = append(ctrlInterceptors, api.AuthInterceptor(cfg.Token, self.NodeID, self.Hostname))
	}
	cluster, _, err := api.Controller(cfg.Controller, hc, ctrlInterceptors...)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	// A binary takes longer than an API call's minute over a slow network.
	// Direct GitHub downloads use a clean client with no controller auth headers.
	followTransport := hc.Transport
	if cfg.Token != "" {
		followTransport = api.AuthRoundTripper(followTransport, cfg.Token, self.NodeID, self.Hostname)
	}
	follow := &Follow{
		URL:            ctrlBaseURL,
		Client:         &http.Client{Transport: followTransport, Timeout: 10 * time.Minute},
		DownloadClient: &http.Client{Timeout: 10 * time.Minute},
		Exe:            exe,
		Build:          version.Number(),
	}
	artifacts := &ArtifactStore{
		WorkerDir:      dir,
		ControllerURL:  ctrlBaseURL,
		AuthClient:     follow.Client,
		DownloadClient: follow.DownloadClient,
	}
	cachedRemote, cerr := LoadControllerConfig(dir)
	if cerr != nil {
		slog.Warn("load cached controller worker config failed; using local hotbed.json", "err", cerr)
	}
	effCfg := ApplyCachedArtifacts(artifacts, runtime.GOARCH, MergeWorkerConfig(cfg, cachedRemote), cachedRemote)
	if !runtimeEnabled(effCfg) {
		// The cached config enables no runtime, which a worker cannot serve
		// without, and heartbeats bring the controller's config only once it
		// serves: ask the controller for its config now, so turning a runtime
		// on with hotbed config set reaches a worker that had none.
		if fresh, err := refreshControllerConfig(ctx, cluster, dir, self.Hostname); err != nil {
			slog.Warn("fetch controller worker config for a worker with no runtime failed", "err", err)
		} else {
			cachedRemote = fresh
			effCfg = ApplyCachedArtifacts(artifacts, runtime.GOARCH, MergeWorkerConfig(cfg, cachedRemote), cachedRemote)
		}
	}
	follow.ConfigSync = NewConfigSyncer(dir, cfg, cachedRemote, effCfg, artifacts)

	if !runtimeEnabled(effCfg) {
		return api.Errorf(api.CodePlatformNotSupported, "worker requires either vm or cladefs_fuse runtime to be enabled").
			WithSuggestion("enable vm or cladefs_fuse in hotbed.json or via hotbed config set")
	}
	baseSpec := baseimage.Spec{User: effCfg.User, UID: uid, GID: gid}
	store, baseImg, err := openLocalRootfs(ctx, effCfg, dir, baseSpec, func(ctx context.Context) (storage.Store, error) {
		return awaitStorage(ctx, func(ctx context.Context) (storage.Store, error) {
			return storage.Open(ctx, dir, effCfg.Dataset)
		}, cluster, effCfg.ClusterID, follow, api.ReportInterval)
	})
	if err != nil {
		return err
	}
	prof := profiler.NewOTel(profiler.DefaultSampleHz)
	if err := prof.Start(ctx); err != nil {
		slog.Warn("always-on OpenTelemetry eBPF profiler could not start; continuing without continuous profiling", "err", err)
	}
	defer prof.Stop()

	workSlice := cmp.Or(effCfg.Slice, unit.DefaultSlice)
	// Tracers run against the local base rootfs by host chroot; a VM
	// worker's run inside hotbed-guestd.
	var inspector inspect.Linux
	if baseImg != nil {
		inspector.BaseRootDir = baseImg.ReadyRootDir
	}

	gitInterceptors := append(append([]connect.ClientInterceptor{}, ctrlInterceptors...), api.RideOut(30*time.Second, func(err error) {
		slog.Warn("waiting for controller to mint git token", "err", err)
	}))
	ctrlConn, err := api.Dial(effCfg.Controller, api.DefaultPort, hc, gitInterceptors...)
	if err != nil {
		return err
	}
	gitAuth := hotbedv1connect.NewGitAuthServiceClient(ctrlConn)

	ecfg := EngineConfig{
		Dir: dir, NodeID: self.NodeID, Host: self.Hostname, User: effCfg.User,
		Slice: effCfg.Slice, UnitProperties: effCfg.UnitProperties, Store: store,
		BaseSpec: baseSpec, BaseImage: baseImg, GitAuth: gitAuth,
		Inspector: inspector, Profiler: prof,
		Units: hostUnits(), System: hostSystem(effCfg.Slice),
		DaemonLogs: dlogs, ControllerNodeID: effCfg.ControllerNodeID, RequireController: true,
		Artifacts: artifacts, Freshen: effCfg.Freshen,
	}
	if effCfg.Bench != nil {
		ecfg.ScratchDisk = effCfg.Bench.Disk
	}
	if vmEnabled(effCfg) {
		if err := EnsureKVMAccess(ctx, cmp.Or(effCfg.VM.VMUser, vm.DefaultVMUser)); err != nil {
			return fmt.Errorf("give Firecracker /dev/kvm: %w", err)
		}
		vmb, err := OpenVMBackend(ctx, dir, *effCfg.VM, workSlice, "")
		if err != nil {
			return fmt.Errorf("start cladefs Firecracker VM backend: %w", err)
		}
		defer vmb.Close()
		ecfg.VM = vmb
		slog.Info("cladefs + Firecracker VM runtime enabled",
			"defaultRuntime", vmb.DefaultRuntime(),
			"peerAddr", effCfg.VM.CladefsPeerAddr,
			"cellRoot", effCfg.VM.CellRoot,
		)
	} else {
		cfb, err := OpenCladefsFUSEBackend(ctx, dir, *effCfg.CladefsFUSE)
		if err != nil {
			return fmt.Errorf("start cladefs host-FUSE backend: %w", err)
		}
		defer cfb.Close()
		ecfg.CladefsFUSE = cfb
		slog.Info("cladefs host-FUSE runtime enabled",
			"defaultRuntime", cfb.DefaultRuntime(),
			"peerAddr", effCfg.CladefsFUSE.CladefsPeerAddr,
			"mountRoot", effCfg.CladefsFUSE.MountRoot,
		)
	}
	if err := prepareHost(ctx, hostPrep{cfg: cfg, effCfg: effCfg, ecfg: &ecfg, store: store, workSlice: workSlice}); err != nil {
		return err
	}
	eng, err := NewEngine(ctx, ecfg)
	if err != nil {
		return err
	}
	defer eng.Close()

	// Clients reach the worker only through the controller, over the
	// worker's own tunnel to it; on its machine, through its Unix socket too,
	// if it has one, whose file mode says who may connect: its callers are
	// this node itself.
	srv := &http.Server{Handler: Handler(self, eng), ReadHeaderTimeout: 10 * time.Second, Protocols: httpx.Protocols()}
	srv.RegisterOnShutdown(eng.DrainStreams)
	// Serves the shells the controller relays (OpenPipe "ssh").
	shellSrv := NewShellServer(eng)
	eng.SetShellServer(shellSrv)
	var listeners []net.Listener
	if cfg.Socket != "" {
		l, err := listenUnix(cfg.Socket)
		if err != nil {
			return err
		}
		slog.Info("worker listening", "socket", cfg.Socket, "storage", store.Kind())
		listeners = append(listeners, l)
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, l := range listeners {
		g.Go(func() error {
			if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}
	g.Go(func() error {
		eng.Run(gctx)
		return nil
	})
	// Warm the local base rootfs snapshot, if the worker keeps one, in the
	// background on startup so the first cold workspace and tracer
	// capabilities are ready quickly. Retry on transient startup failures
	// (e.g. DNS right after boot).
	go func() {
		if baseImg == nil {
			return
		}
		backoff := 5 * time.Second
		for {
			if _, _, err := baseImg.Ensure(gctx); err == nil {
				return
			} else if gctx.Err() != nil {
				return
			} else {
				slog.Warn("warm shared base rootfs image; retrying", "backoff", backoff, "err", err)
			}
			select {
			case <-gctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, time.Minute)
			}
		}
	}()
	// Its update.ErrRestart ends the others, and Run with it.
	g.Go(func() error {
		return connectWithReset(gctx, eng, cluster, cfg.ClusterID, follow, func() {
			if api.ResetClientTransport(hc) {
				slog.Warn("reset controller HTTP transport after RPC failure")
			}
		})
	})
	g.Go(func() error {
		return Tunnel(gctx, eng, ctrlBaseURL, cfg.Token, self, cfg.ControllerNodeID, cfg.Owner, hc)
	})
	g.Go(func() error {
		<-gctx.Done()
		_ = shellSrv.Close()
		eng.DrainStreams()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	})
	return g.Wait()
}

// listenUnix listens on the Unix socket at path, its owner's alone (0600):
// clients on this machine call the worker there.
func listenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove the worker's old socket: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on the worker's socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("make the worker's socket its owner's alone: %w", err)
	}
	return l, nil
}

// newNodeID is a new worker's node ID, the controller's name for it: random,
// as no two may share one.
func newNodeID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "w" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

// hostName is this machine's name, without its domain.
func hostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "worker"
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

// Handler serves an engine's WorkspaceService to callers who are all peer:
// the controller over the worker's tunnel, or this node over its Unix socket.
func Handler(peer httpx.Peer, eng *Engine) http.Handler {
	server := httpx.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(server, &Service{E: eng})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	return httpx.WithStaticPeer(peer, mux)
}
