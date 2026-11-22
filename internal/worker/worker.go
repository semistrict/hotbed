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

	"connectrpc.com/connect/v2/connecthttp"
	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/bench"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/unit"
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
	TmpDir         string
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
	if o.TmpDir != "" && !filepath.IsAbs(o.TmpDir) {
		return "", api.Errorf(api.CodeInvalidArgs, "--tmp-dir %s is not an absolute path", o.TmpDir)
	}
	if o.Slice != "" {
		if err := unit.CheckSlice(o.Slice); err != nil {
			return "", api.Errorf(api.CodeInvalidArgs, "%v", err)
		}
	}
	if _, err := user.Lookup(o.User); err != nil {
		return "", api.Errorf(api.CodeInvalidArgs, "no user %q to run commands as", o.User)
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
	controller, err := api.ControllerHostPort(o.Controller)
	if err != nil {
		return JoinResult{}, err
	}
	if err := statedir.Prepare(dir); err != nil {
		return JoinResult{}, err
	}
	kind, err := check(ctx, o)
	if err != nil {
		return JoinResult{}, err
	}
	cluster, _, err := api.Controller(controller, hc)
	if err != nil {
		return JoinResult{}, err
	}
	joined, err := cluster.Join(ctx, &pb.JoinRequest{})
	if err != nil {
		return JoinResult{}, api.FromConnect(err)
	}
	owner, err := controllerOwner(ctx, tn, controller)
	if err != nil {
		return JoinResult{}, err
	}
	cfg := statedir.WorkerConfig{
		ClusterID: joined.ClusterId, Controller: controller, User: o.User, Dataset: o.Dataset,
		UnitProperties: o.UnitProperties, Slice: o.Slice, TmpDir: o.TmpDir, Owner: owner,
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
	self, err := tn.Self(ctx)
	if err != nil {
		return err
	}
	if cfg.Owner == "" {
		// Joined before workers recorded whom they serve: record it now, from
		// the tailnet's account of the controller's address.
		if cfg.Owner, err = controllerOwner(ctx, tn, cfg.Controller); err != nil {
			return err
		}
		if err := statedir.Rewrite(dir, statedir.Config{Worker: &cfg}); err != nil {
			return err
		}
		slog.Info("recorded whom the worker serves", "owner", cfg.Owner)
	}
	if self.Login != cfg.Owner {
		return api.Errorf(api.CodeNotAuthorized, "this machine is on the tailnet as %s, but the cluster belongs to %s: refusing to serve", self.Login, cfg.Owner).
			WithSuggestion("log this machine into the owner's tailnet account (tailscale up)")
	}
	store, err := storage.Open(ctx, dir, cfg.Dataset)
	if err != nil {
		return err
	}
	ecfg := EngineConfig{
		Dir: dir, NodeID: self.NodeID, Host: self.Hostname, User: cfg.User,
		Slice: cfg.Slice, TmpDir: cfg.TmpDir, UnitProperties: cfg.UnitProperties, Store: store,
		Units: Systemd{}, System: &Linux{Slice: cfg.Slice},
	}
	// Under /run: what an isolation saves of the host lasts as long as the
	// kernel's settings it changed.
	benchOpts := bench.Options{
		State: "/run/hotbed/bench", Slice: BenchSlice(cfg.Slice),
		WorkSlice: cmp.Or(cfg.Slice, unit.DefaultSlice), Systemd: Systemd{},
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
	cluster, _, err := api.Controller(cfg.Controller, hc)
	if err != nil {
		return err
	}

	// A worker admits what the controller admits: the owner's machines,
	// whose clients call it directly to sync, run commands and read their
	// output, and the controller among them. On this machine itself, root
	// and the user commands run as.
	local, err := localUsers(cfg.User)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: Handler(tn, httpx.Policy{
		Owner: cfg.Owner, Self: self.Addrs, LocalUsers: local,
	}, eng), ReadHeaderTimeout: 10 * time.Second, Protocols: httpx.Protocols()}
	var listeners []net.Listener
	for _, addr := range self.Addrs {
		l, err := net.Listen("tcp", net.JoinHostPort(addr.String(), strconv.Itoa(api.WorkerPort)))
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return fmt.Errorf("listen on tailnet address: %w", err)
		}
		slog.Info("worker listening", "addr", l.Addr().String(), "storage", store.Kind())
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
	g.Go(func() error { return Connect(gctx, eng, cluster, cfg.ClusterID) })
	g.Go(func() error {
		<-gctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	})
	return g.Wait()
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

// controllerOwner is who owns the controller at hostPort, by the tailnet's
// account; the controller must be a user's own node, and this machine that
// user's too.
func controllerOwner(ctx context.Context, tn Tailnet, hostPort string) (string, error) {
	p, err := tailnet.NodeAt(ctx, tn, hostPort)
	if err != nil {
		return "", fmt.Errorf("identify the controller at %s: %w", hostPort, err)
	}
	if p.Login == "" || len(p.Tags) > 0 || p.Shared {
		return "", api.Errorf(api.CodeNotAuthorized, "the controller at %s (%s) is not a user's own node", hostPort, p.Hostname)
	}
	self, err := tn.Self(ctx)
	if err != nil {
		return "", err
	}
	if p.Login != self.Login {
		return "", api.Errorf(api.CodeNotAuthorized, "the controller at %s belongs to %s, this machine to %s", hostPort, p.Login, self.Login)
	}
	return p.Login, nil
}
