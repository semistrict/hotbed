// Package worker is a node that runs work for the cluster: it keeps warm
// bases of the cluster's repos, forks them into workspaces, and runs
// agents' commands in them.
package worker

import (
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

	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/tailnet"
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
}

// JoinResult describes a worker that joined.
type JoinResult struct {
	ClusterID  string   `json:"clusterId"`
	Dir        string   `json:"dir"`
	Controller string   `json:"controller"`
	Storage    string   `json:"storage"`
	User       string   `json:"user"`
	Node       api.Node `json:"node"`
	Next       []string `json:"next"`
}

// Check verifies a machine can be a worker: Linux, root, a user to run
// commands as, and a state directory on a filesystem that can fork.
type Check func(ctx context.Context, o JoinOptions) (storage string, err error)

// CheckMachine is the real Check.
func CheckMachine(ctx context.Context, o JoinOptions) (string, error) {
	if runtime.GOOS != "linux" {
		return "", api.Errorf(api.CodePlatformNotSupported, "workers need Linux; this is %s", runtime.GOOS).
			WithSuggestion("run the controller or the client here, and workers on Linux machines")
	}
	if os.Geteuid() != 0 {
		return "", api.Errorf(api.CodeInvalidArgs, "a worker runs as root, to fork volumes and start systemd units").
			WithSuggestion("run it with sudo; commands still run as --user")
	}
	if _, err := user.Lookup(o.User); err != nil {
		return "", api.Errorf(api.CodeInvalidArgs, "no user %q to run commands as", o.User)
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return "", err
	}
	s, err := storage.Open(ctx, o.Dir, o.Dataset)
	if err != nil {
		return "", api.Errorf(api.CodeInvalidArgs, "%v", err).
			WithSuggestion("put the state directory on btrfs or zfs")
	}
	return s.Kind(), nil
}

// Join makes o.Dir the state directory of a worker in the cluster that the
// controller at o.Controller runs.
func Join(ctx context.Context, o JoinOptions, hc *http.Client, check Check) (JoinResult, error) {
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
	client, err := api.NewClient(controller, hc)
	if err != nil {
		return JoinResult{}, err
	}
	joined, err := client.Join(ctx)
	if err != nil {
		return JoinResult{}, err
	}
	cfg := statedir.WorkerConfig{ClusterID: joined.ClusterID, Controller: controller, User: o.User, Dataset: o.Dataset, UnitProperties: o.UnitProperties}
	if err := statedir.Commit(dir, statedir.Config{Worker: &cfg}); err != nil {
		return JoinResult{}, err
	}
	return JoinResult{
		ClusterID: joined.ClusterID, Dir: dir, Controller: controller, Storage: kind, User: o.User,
		Node: joined.Node, Next: []string{"sudo hotbed run " + dir},
	}, nil
}

// Run runs the worker in dir until ctx ends: the engine, the server the
// controller calls, and the reports to the controller.
func Run(ctx context.Context, dir string, cfg statedir.WorkerConfig, tn Tailnet, hc *http.Client) error {
	self, err := tn.Self(ctx)
	if err != nil {
		return err
	}
	store, err := storage.Open(ctx, dir, cfg.Dataset)
	if err != nil {
		return err
	}
	eng, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: self.NodeID, Host: self.Hostname, User: cfg.User,
		UnitProperties: cfg.UnitProperties, Store: store, Units: Systemd{}, System: &Linux{},
	})
	if err != nil {
		return err
	}
	defer eng.Close()
	client, err := api.NewClient(cfg.Controller, hc)
	if err != nil {
		return err
	}

	srv := &http.Server{Handler: httpx.Owner(tn, self.Login, eng.Handler()), ReadHeaderTimeout: 10 * time.Second}
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
	g.Go(func() error { return report(gctx, eng, client, cfg.ClusterID) })
	g.Go(func() error {
		<-gctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	})
	return g.Wait()
}

// report sends the engine's report every ReportInterval, and soon after
// anything changes. It rides out failures a retry can fix, such as the
// controller restarting, and stops at one it cannot.
func report(ctx context.Context, eng *Engine, client *api.Client, cluster string) error {
	t := time.NewTicker(api.ReportInterval)
	defer t.Stop()
	failing := false
	for {
		r, err := eng.Report(cluster)
		if err != nil {
			return err
		}
		reply, err := client.Report(ctx, r)
		switch {
		case err == nil:
			if failing {
				slog.Info("reporting to controller again")
			}
			failing = false
			eng.Acknowledged(reply.Ack)
			eng.SetRepos(reply.Repos)
		case ctx.Err() != nil:
			return nil
		case retryable(err):
			if !failing {
				slog.Warn("report failed; retrying", "err", err)
			}
			failing = true
		default:
			return err
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

func retryable(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Retryable
}
