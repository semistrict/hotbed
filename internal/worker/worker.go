// Package worker is a node that runs work for the cluster.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/statedir"
)

// Prober measures this machine's resources.
type Prober func() (api.Resources, error)

// JoinResult describes a worker that joined.
type JoinResult struct {
	ClusterID  string   `json:"cluster_id"`
	Dir        string   `json:"dir"`
	Controller string   `json:"controller"`
	Node       api.Node `json:"node"`
	Next       []string `json:"next"`
}

// Join makes dir the state directory of a worker in the cluster that the
// controller at addr runs.
func Join(ctx context.Context, dir, addr string, hc *http.Client, probe Prober) (JoinResult, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return JoinResult{}, err
	}
	// A machine that cannot run work fails here, not at its first heartbeat.
	if _, err := probe(); err != nil {
		return JoinResult{}, err
	}
	controller, err := api.ControllerHostPort(addr)
	if err != nil {
		return JoinResult{}, err
	}
	client, err := api.NewClient(controller, hc)
	if err != nil {
		return JoinResult{}, err
	}
	if err := statedir.Prepare(dir); err != nil {
		return JoinResult{}, err
	}
	joined, err := client.Join(ctx)
	if err != nil {
		return JoinResult{}, err
	}
	cfg := statedir.WorkerConfig{ClusterID: joined.ClusterID, Controller: controller}
	if err := statedir.Commit(dir, statedir.Config{Worker: &cfg}); err != nil {
		return JoinResult{}, err
	}
	return JoinResult{
		ClusterID:  joined.ClusterID,
		Dir:        dir,
		Controller: controller,
		Node:       joined.Node,
		Next:       []string{"hotbed run " + dir},
	}, nil
}

// Worker reports to its controller.
type Worker struct {
	client  *api.Client
	cluster string
	probe   Prober
}

func New(cfg statedir.WorkerConfig, hc *http.Client, probe Prober) (*Worker, error) {
	client, err := api.NewClient(cfg.Controller, hc)
	if err != nil {
		return nil, err
	}
	return &Worker{client: client, cluster: cfg.ClusterID, probe: probe}, nil
}

// Beat sends one heartbeat.
func (w *Worker) Beat(ctx context.Context) error {
	r, err := w.probe()
	if err != nil {
		return err
	}
	return w.client.Heartbeat(ctx, api.Heartbeat{ClusterID: w.cluster, Resources: r})
}

// Run sends a heartbeat every api.HeartbeatInterval until ctx ends. It
// rides out failures a retry can fix, such as the controller restarting, and
// stops at one it cannot, such as the controller not knowing this worker.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(api.HeartbeatInterval)
	defer t.Stop()
	// Log only changes, so an hour of the controller being down is two lines.
	first, failing := true, false
	for {
		err := w.Beat(ctx)
		switch {
		case err == nil:
			if first || failing {
				slog.Info("reporting to controller")
			}
			first, failing = false, false
		case ctx.Err() != nil:
			return nil
		case retryable(err):
			if !failing {
				slog.Warn("heartbeat failed; retrying every interval", "interval", api.HeartbeatInterval, "err", err)
			}
			first, failing = false, true
		default:
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func retryable(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Retryable
}
