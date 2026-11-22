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
	owner, ctrlNode, err := controllerIdentity(ctx, tn, controller)
	if err != nil {
		return JoinResult{}, err
	}
	cfg := statedir.WorkerConfig{
		ClusterID: joined.ClusterId, Controller: controller, User: o.User, Dataset: o.Dataset,
		UnitProperties: o.UnitProperties, Slice: o.Slice, TmpDir: o.TmpDir, Owner: owner, ControllerNode: ctrlNode,
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
	if cfg.Owner == "" || cfg.ControllerNode == "" {
		// Joined before workers recorded whom they serve: record it now, from
		// the tailnet's account of the controller's address.
		if cfg.Owner, cfg.ControllerNode, err = controllerIdentity(ctx, tn, cfg.Controller); err != nil {
			return err
		}
		if err := statedir.Rewrite(dir, statedir.Config{Worker: &cfg}); err != nil {
			return err
		}
		slog.Info("recorded the controller worker admits", "node", cfg.ControllerNode, "owner", cfg.Owner)
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

	// The controller alone may call a worker: it is the one that knows
	// which sessions may touch which workspaces.
	srv := &http.Server{Handler: Handler(tn, httpx.Policy{
		Owner: cfg.Owner, Nodes: []string{cfg.ControllerNode}, Self: self.Addrs,
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
	g.Go(func() error { return Reports(gctx, eng, cluster, cfg.ClusterID) })
	g.Go(func() error {
		<-gctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	})
	return g.Wait()
}

// Reports sends the engine's report every ReportInterval, and soon after
// anything changes. It rides out failures a retry can fix, such as the
// controller restarting, and stops at one it cannot.
func Reports(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string) error {
	t := time.NewTicker(api.ReportInterval)
	defer t.Stop()
	failing := false
	for {
		if err := ReportOnce(ctx, eng, cluster, clusterID); err != nil {
			e := api.FromConnect(err)
			switch {
			case ctx.Err() != nil:
				return nil
			case e.Retryable:
				if !failing {
					slog.Warn("report failed; retrying", "err", e)
				}
				failing = true
			default:
				return e
			}
		} else {
			if failing {
				slog.Info("reporting to controller again")
			}
			failing = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-eng.Kicked():
			// Let a burst of changes settle into one report.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
}

// ReportOnce sends one report and applies the reply.
func ReportOnce(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string) error {
	r, err := eng.Report(clusterID)
	if err != nil {
		return err
	}
	reply, err := cluster.Report(ctx, r)
	if err != nil {
		return err
	}
	eng.Acknowledged(reply.Ack)
	eng.SetRepos(reply.Repos)
	for _, id := range reply.Release {
		if err := eng.ReleaseWorkspace(ctx, id); err != nil && api.FromConnect(err).Code != api.CodeNotFound {
			slog.Error("release workspace of an ended session", "workspace", id, "err", err)
		}
	}
	return nil
}

// Handler serves an engine's WorkspaceService behind a gate.
func Handler(id httpx.Identifier, p httpx.Policy, eng *Engine) http.Handler {
	server := httpx.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(server, &Service{E: eng})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	return httpx.Gate(id, p, mux)
}

// controllerIdentity is who owns the controller at hostPort and its stable
// node ID, by the tailnet's account; the controller must be a user's node.
func controllerIdentity(ctx context.Context, tn Tailnet, hostPort string) (owner, node string, err error) {
	p, err := tailnet.NodeAt(ctx, tn, hostPort)
	if err != nil {
		return "", "", fmt.Errorf("identify the controller at %s: %w", hostPort, err)
	}
	if p.Login == "" || len(p.Tags) > 0 || p.Shared {
		return "", "", api.Errorf(api.CodeNotAuthorized, "the controller at %s (%s) is not a user's own node", hostPort, p.Hostname)
	}
	self, err := tn.Self(ctx)
	if err != nil {
		return "", "", err
	}
	if p.Login != self.Login {
		return "", "", api.Errorf(api.CodeNotAuthorized, "the controller at %s belongs to %s, this machine to %s", hostPort, p.Login, self.Login)
	}
	return p.Login, p.NodeID, nil
}
