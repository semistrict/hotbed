// Package worker is a node that runs work for the cluster: it keeps warm
// bases of the cluster's repos, forks them into workspaces, and runs
// agents' commands in them.
package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/bench"
	"github.com/semistrict/hotbed/internal/daemonlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/version"
)

// Tailnet is what a worker needs from this machine's tailscaled.
type Tailnet interface {
	httpx.Identifier
	Self(ctx context.Context) (tailnet.Self, error)
}

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
	if runtime.GOOS != "linux" {
		return "", api.Errorf(api.CodePlatformNotSupported, "workers need Linux; this is %s", runtime.GOOS).
			WithSuggestion("run the controller or the client here, and workers on Linux machines")
	}
	if os.Geteuid() != 0 {
		return "", api.Errorf(api.CodeInvalidArgs, "a worker runs as root, to fork volumes and start systemd units").
			WithSuggestion("run it with sudo; commands still run as --user")
	}
	if o.Slice != "" {
		if err := unit.CheckSlice(o.Slice); err != nil {
			return "", api.Errorf(api.CodeInvalidArgs, "%v", err)
		}
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

// Join makes o.Dir the state directory of a worker in the cluster that the
// controller at o.Controller runs.
func Join(ctx context.Context, o JoinOptions, tn Tailnet, hc *http.Client, check Check) (JoinResult, error) {
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return JoinResult{}, err
	}
	o.Dir = dir
	remote := api.IsRemoteURL(o.Controller)
	var controller string
	if remote {
		controller, err = api.ControllerBaseURL(o.Controller)
	} else {
		controller, err = api.ControllerHostPort(o.Controller)
	}
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
	var interceptors []connect.ClientInterceptor
	var self tailnet.Self
	if remote || o.Token != "" {
		self, err = tn.Self(ctx)
		if err != nil {
			return JoinResult{}, err
		}
		interceptors = append(interceptors, api.AuthInterceptor(o.Token, self.NodeID, self.Hostname, workerAdvertiseAddr(self)))
	}
	cluster, _, err := api.Controller(controller, hc, interceptors...)
	if err != nil {
		return JoinResult{}, err
	}
	joined, err := cluster.Join(ctx, &pb.JoinRequest{})
	if err != nil {
		return JoinResult{}, api.FromConnect(err)
	}
	var owner, ctrlNodeID string
	if remote {
		owner = self.Login
		ctrlNodeID = "cf:" + joined.ClusterId
	} else {
		ctrl, err := controllerPeer(ctx, tn, controller)
		if err != nil {
			return JoinResult{}, err
		}
		owner = ctrl.Login
		ctrlNodeID = ctrl.NodeID
	}
	cfg := statedir.WorkerConfig{
		ClusterID: joined.ClusterId, Controller: controller, User: o.User, Dataset: o.Dataset,
		UnitProperties: o.UnitProperties, Slice: o.Slice, Owner: owner, ControllerNodeID: ctrlNodeID,
		Token: o.Token,
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
func Run(ctx context.Context, dir string, cfg statedir.WorkerConfig, tn Tailnet, hc *http.Client) error {
	uid, gid, err := resolveCommandUser(cfg.User)
	if err != nil {
		return err
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

	self, err := tn.Self(ctx)
	if err != nil {
		return err
	}
	remote := api.IsRemoteURL(cfg.Controller)
	ctrlBaseURL, err := api.ControllerBaseURL(cfg.Controller)
	if err != nil {
		return err
	}
	if cfg.Owner == "" || cfg.ControllerNodeID == "" {
		if remote {
			if cfg.Owner == "" {
				cfg.Owner = self.Login
			}
			if cfg.ControllerNodeID == "" {
				cfg.ControllerNodeID = "cf:" + cfg.ClusterID
			}
		} else {
			// Joined before workers recorded whom they serve / their controller ID:
			// record it now from the tailnet's account of the controller's address.
			ctrl, err := controllerPeer(ctx, tn, cfg.Controller)
			if err != nil {
				return err
			}
			if cfg.Owner == "" {
				cfg.Owner = ctrl.Login
			}
			if cfg.ControllerNodeID == "" {
				cfg.ControllerNodeID = ctrl.NodeID
			}
		}
		if err := statedir.Rewrite(dir, statedir.Config{Worker: &cfg}); err != nil {
			return err
		}
		slog.Info("recorded controller identity", "owner", cfg.Owner, "controllerNodeId", cfg.ControllerNodeID)
	}
	if self.Login != cfg.Owner {
		return api.Errorf(api.CodeNotAuthorized, "this machine is on the tailnet as %s, but the cluster belongs to %s: refusing to serve", self.Login, cfg.Owner).
			WithSuggestion("log this machine into the owner's tailnet account (tailscale up)")
	}
	workerAddr := workerAdvertiseAddr(self)
	var ctrlInterceptors []connect.ClientInterceptor
	if remote || cfg.Token != "" {
		ctrlInterceptors = append(ctrlInterceptors, api.AuthInterceptor(cfg.Token, self.NodeID, self.Hostname, workerAddr))
	}
	cluster, _, err := api.Controller(cfg.Controller, hc, ctrlInterceptors...)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// A binary takes longer than an API call's minute over a slow tailnet.
	followTransport := hc.Transport
	if remote || cfg.Token != "" {
		followTransport = api.AuthRoundTripper(followTransport, cfg.Token, self.NodeID, self.Hostname, workerAddr)
	}
	follow := &Follow{
		URL:    ctrlBaseURL,
		Client: &http.Client{Transport: followTransport, Timeout: 10 * time.Minute},
		Exe:    exe,
		Build:  version.Number(),
		OnMigrate: func(m *pb.ControllerMigration) error {
			return applyControllerMigration(dir, cfg, m)
		},
	}
	store, err := awaitStorage(ctx, func(ctx context.Context) (storage.Store, error) {
		return storage.Open(ctx, dir, cfg.Dataset)
	}, cluster, cfg.ClusterID, follow, api.ReportInterval)
	if err != nil {
		return err
	}
	prof := profiler.NewOTel(profiler.DefaultSampleHz)
	if err := prof.Start(ctx); err != nil {
		slog.Warn("always-on OpenTelemetry eBPF profiler could not start; continuing without continuous profiling", "err", err)
	}
	defer prof.Stop()

	workSlice := cmp.Or(cfg.Slice, unit.DefaultSlice)
	baseImg := &baseimage.Manager{
		Store: store,
		Spec: baseimage.Spec{
			User: cfg.User,
			UID:  uid,
			GID:  gid,
		},
		Provision: baseimage.SystemdProvisioner(workSlice, filepath.Join(dir, "logs")),
	}
	baseRootFn := baseImg.ReadyRootDir

	gitInterceptors := append(append([]connect.ClientInterceptor{}, ctrlInterceptors...), api.RideOut(30*time.Second, func(err error) {
		slog.Warn("waiting for controller to mint git token", "err", err)
	}))
	ctrlConn, err := api.Dial(cfg.Controller, api.DefaultPort, hc, gitInterceptors...)
	if err != nil {
		return err
	}
	gitAuth := hotbedv1connect.NewGitAuthServiceClient(ctrlConn)

	ecfg := EngineConfig{
		Dir: dir, NodeID: self.NodeID, Host: self.Hostname, User: cfg.User,
		Slice: cfg.Slice, UnitProperties: cfg.UnitProperties, Store: store,
		BaseImage: baseImg, GitAuth: gitAuth,
		Inspector: inspect.Linux{BaseRootDir: baseRootFn}, Profiler: prof,
		Units: Systemd{}, System: &Linux{Slice: cfg.Slice},
		DaemonLogs: dlogs, ControllerNodeID: cfg.ControllerNodeID, RequireController: true,
	}
	if cfg.Bench != nil {
		ecfg.ScratchDisk = cfg.Bench.Disk
	}
	if cfg.VM != nil && cfg.VM.Enabled {
		vmb, err := OpenVMBackend(ctx, dir, *cfg.VM, workSlice, "")
		if err != nil {
			return fmt.Errorf("start cladefs Firecracker VM backend: %w", err)
		}
		defer vmb.Close()
		ecfg.VM = vmb
		slog.Info("cladefs + Firecracker VM runtime enabled",
			"defaultRuntime", vmb.DefaultRuntime(),
			"peerAddr", cfg.VM.CladefsPeerAddr,
			"cellRoot", cfg.VM.CellRoot,
		)
	}
	// Lower the work slice's CPU and IO priority so normal commands yield to
	// desktop and system processes while still sharing spare CPU among
	// themselves by their per-command CPUWeight=. Measurements run in a
	// sibling slice (*_bench.slice) at normal priority.
	if err := unit.StopLeftExecs(ctx); err != nil {
		return fmt.Errorf("stop the last worker's chroot git: %w", err)
	}
	if err := unit.StartSlice(ctx, workSlice); err != nil {
		return fmt.Errorf("start work slice %s: %w", workSlice, err)
	}
	if err := unit.SetProperties(ctx, workSlice, "CPUWeight=idle", "IOWeight=1"); err != nil {
		return fmt.Errorf("set work slice %s priority: %w", workSlice, err)
	}
	// Bound what the commands take together, base builds and work alike,
	// leaving the host room to stay up (board 9128). On zram hosts, swap
	// sits in kernel RAM outside the slice's memcg, so the backstop also
	// caps MemorySwapMax and accounts for zram's RAM footprint (board 9361).
	// The engine measures its memory, and admits and parks by it, within
	// this bound. The measurement slice is its sibling, outside it.
	host, err := sysinfo.Meminfo()
	if err != nil {
		return fmt.Errorf("read the host's memory: %w", err)
	}
	swaps, err := sysinfo.Swaps()
	if err != nil {
		return fmt.Errorf("read the host's swap: %w", err)
	}
	backstop := unit.BackstopFor(host.Total, unit.Swap{Zram: swaps.ZramBytes, Disk: swaps.DiskBytes})
	if static, err := unit.StaticMemoryLimits(ctx, workSlice, host.Total); err != nil {
		slog.Warn("read static work slice memory limits", "slice", workSlice, "err", err)
	} else {
		backstop = backstop.Clamp(static)
	}
	if err := unit.SetProperties(ctx, workSlice, backstop.Properties()...); err != nil {
		return fmt.Errorf("bound work slice %s's memory: %w", workSlice, err)
	}
	slog.Info("work slice memory bounded",
		"slice", workSlice,
		"high", backstop.High,
		"max", backstop.Max,
		"swapMax", backstop.SwapMax,
		"headroom", backstop.Headroom,
		"zram", swaps.ZramBytes,
		"diskSwap", swaps.DiskBytes,
	)
	// Under /run: what an isolation saves of the host lasts as long as the
	// kernel's settings it changed.
	benchOpts := bench.Options{
		State: "/run/hotbed/bench", Slice: BenchSlice(cfg.Slice),
		WorkSlice: workSlice, Systemd: Systemd{},
	}
	// Other work's writes to the volumes are kept quiet beside a
	// measurement too: capped on a block device (btrfs's), held back on a
	// ZFS pool, which no cap reaches.
	if z, ok := store.(storage.Pooled); ok {
		benchOpts.Pool = z.Dataset()
	} else {
		benchOpts.StoreDisk = store.Path("")
	}
	if cfg.Bench != nil {
		iso, err := bench.New(*cfg.Bench, benchOpts)
		if err != nil {
			return fmt.Errorf("the worker's measurements: %w", err)
		}
		ecfg.Bench = iso
	} else if err := bench.GiveBack(ctx, benchOpts); err != nil {
		// A worker that took measurements before its config stopped them.
		slog.Error("give back a measurement's isolation", "err", err)
	}
	eng, err := NewEngine(ctx, ecfg)
	if err != nil {
		return err
	}
	defer eng.Close()

	// A worker admits what the controller admits: the owner's machines,
	// whose clients call it directly to sync, run commands and read their
	// output, and the controller among them. On this machine itself, root
	// and the user commands run as.
	local, err := localUsers(cfg.User)
	if err != nil {
		return err
	}
	policy := httpx.Policy{
		Owner: cfg.Owner, Self: self.Addrs, Hosts: []string{self.Hostname, self.DNSName}, LocalUsers: local,
	}
	srv := &http.Server{Handler: Handler(tn, policy, eng), ReadHeaderTimeout: 10 * time.Second, Protocols: httpx.Protocols()}
	srv.RegisterOnShutdown(eng.DrainStreams)
	shellSrv := NewShellServer(tn, policy, eng)
	var listeners []net.Listener
	var sshListeners []net.Listener
	closeAll := func() {
		for _, l := range listeners {
			l.Close()
		}
		for _, l := range sshListeners {
			l.Close()
		}
	}
	for _, addr := range self.Addrs {
		l, err := net.Listen("tcp", net.JoinHostPort(addr.String(), strconv.Itoa(api.WorkerPort)))
		if err != nil {
			closeAll()
			return fmt.Errorf("listen on tailnet address: %w", err)
		}
		slog.Info("worker listening", "addr", l.Addr().String(), "storage", store.Kind())
		listeners = append(listeners, l)

		sl, err := net.Listen("tcp", net.JoinHostPort(addr.String(), strconv.Itoa(api.WorkerSSHPort)))
		if err != nil {
			closeAll()
			return fmt.Errorf("listen ssh on tailnet address: %w", err)
		}
		slog.Info("worker ssh listening", "addr", sl.Addr().String())
		sshListeners = append(sshListeners, sl)
	}
	if len(sshListeners) > 0 {
		eng.SetShellListen(sshListeners[0].Addr().String(), api.WorkerSSHPort)
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
	for _, sl := range sshListeners {
		g.Go(func() error {
			return shellSrv.Serve(sl)
		})
	}
	g.Go(func() error {
		eng.Run(gctx)
		return nil
	})
	// Warm the shared base rootfs snapshot in the background on startup so
	// the first cold workspace and tracer capabilities are ready quickly.
	// Retry on transient startup failures (e.g. DNS right after boot).
	go func() {
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
	g.Go(func() error { return Connect(gctx, eng, cluster, cfg.ClusterID, follow) })
	if remote {
		g.Go(func() error {
			return Tunnel(gctx, eng, ctrlBaseURL, cfg.Token, self, workerAddr, cfg.ControllerNodeID, cfg.Owner, hc)
		})
	}
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

// applyControllerMigration rewrites dir's hotbed.json to point at m's target
// controller and returns update.ErrRestart so `hotbed serve` re-execs into the
// new controller without disrupting systemd-managed workloads or warm bases.
func applyControllerMigration(dir string, cfg statedir.WorkerConfig, m *pb.ControllerMigration) error {
	if m == nil || m.GetController() == "" {
		return nil
	}
	target := m.GetController()
	if api.IsRemoteURL(target) {
		norm, err := api.ControllerBaseURL(target)
		if err != nil {
			return fmt.Errorf("invalid migration target %q: %w", target, err)
		}
		target = norm
	} else {
		hp, err := api.ControllerHostPort(target)
		if err != nil {
			return fmt.Errorf("invalid migration target %q: %w", target, err)
		}
		target = hp
	}
	if cfg.Controller == target && cfg.Token == m.GetToken() && (m.GetControllerNodeId() == "" || cfg.ControllerNodeID == m.GetControllerNodeId()) {
		return nil
	}
	next := cfg
	next.Controller = target
	next.Token = m.GetToken()
	if m.GetControllerNodeId() != "" {
		next.ControllerNodeID = m.GetControllerNodeId()
	}
	if err := statedir.Rewrite(dir, statedir.Config{Worker: &next}); err != nil {
		return fmt.Errorf("rewrite worker config for controller migration to %s: %w", target, err)
	}
	slog.Info("controller migrated: updated worker config and restarting to reconnect",
		"from", cfg.Controller, "to", target, "controllerNodeId", next.ControllerNodeID)
	return update.ErrRestart
}

// workerAdvertiseAddr returns this worker's direct tailnet address (host:7121)
// advertised to remote HTTPS controllers so clients can still reach the worker
// directly over Tailscale for Sync, RunCommand, FollowLog, and SSH.
func workerAdvertiseAddr(self tailnet.Self) string {
	if len(self.Addrs) > 0 {
		return net.JoinHostPort(self.Addrs[0].String(), strconv.Itoa(api.WorkerPort))
	}
	if self.DNSName != "" {
		return net.JoinHostPort(self.DNSName, strconv.Itoa(api.WorkerPort))
	}
	if self.Hostname != "" {
		return net.JoinHostPort(self.Hostname, strconv.Itoa(api.WorkerPort))
	}
	return ""
}

// localUsers is the unix user commands run as, which may call the worker
// from this machine besides root.
func localUsers(name string) ([]uint32, error) {
	cred, _, err := lookupUser(name)
	if err != nil || cred == nil {
		return nil, err
	}
	return []uint32{cred.Uid}, nil
}

// Handler serves an engine's WorkspaceService behind a gate.
func Handler(id httpx.Identifier, p httpx.Policy, eng *Engine) http.Handler {
	server := httpx.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(server, &Service{E: eng})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	return httpx.Gate(id, p, mux)
}

// controllerPeer identifies the controller at hostPort via the tailnet; the
// controller must be one of this user's own nodes.
func controllerPeer(ctx context.Context, tn Tailnet, hostPort string) (tailnet.Peer, error) {
	p, err := tailnet.NodeAt(ctx, tn, hostPort)
	if err != nil {
		return tailnet.Peer{}, fmt.Errorf("identify the controller at %s: %w", hostPort, err)
	}
	if p.Login == "" || len(p.Tags) > 0 || p.Shared {
		return tailnet.Peer{}, api.Errorf(api.CodeNotAuthorized, "the controller at %s (%s) is not a user's own node", hostPort, p.Hostname)
	}
	if p.NodeID == "" {
		return tailnet.Peer{}, api.Errorf(api.CodeNotAuthorized, "the tailnet did not report a node ID for the controller at %s", hostPort)
	}
	self, err := tn.Self(ctx)
	if err != nil {
		return tailnet.Peer{}, err
	}
	if p.Login != self.Login {
		return tailnet.Peer{}, api.Errorf(api.CodeNotAuthorized, "the controller at %s belongs to %s, this machine to %s", hostPort, p.Login, self.Login)
	}
	return p, nil
}
