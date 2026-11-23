package setup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// DefaultSleepAfter is how long a worker VM has to have had no work before
// the controller has it powered off, unless its config says otherwise.
const DefaultSleepAfter = 30 * time.Minute

// hostUsageInterval is how often Standby samples and reports host-side
// metrics for a running VM. Overridable in tests.
var hostUsageInterval = 5 * time.Second

// hostUsageRPCTimeout bounds each host-usage sample + ReportHostUsage tick so
// a hung unary RPC cannot stall the reporting goroutine forever. Overridable in
// tests.
var hostUsageRPCTimeout = 10 * time.Second

// VM is a worker's VM as its host runs it.
type VM interface {
	// Running says whether the VM runs.
	Running(ctx context.Context) bool
	// Start boots it, returning once it runs; Stop powers it off cleanly,
	// returning once it no longer runs.
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// HostMetricsVM is an optional interface implemented by VMs that can sample
// host-side CPU and memory pressure metrics while Standby supervises them.
type HostMetricsVM interface {
	SampleHostUsage(ctx context.Context) (*pb.HostUsage, error)
}

// Standby has vm run as the controller wants node to, until ctx ends: it
// holds the controller's Standby call open, saying whether vm runs and how
// long node is to be idle before it sleeps, and starts or stops vm when the
// controller says to, then calls again with how vm is now. While the
// controller is out of reach vm stays as it is.
func Standby(ctx context.Context, cluster hotbedv1connect.ClusterServiceClient, node string, sleepAfter time.Duration, vm VM, log *slog.Logger) {
	if hm, ok := vm.(HostMetricsVM); ok {
		reportCtx, cancelReport := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			reportHostUsageLoop(reportCtx, cluster, node, vm, hm, log)
		}()
		defer func() {
			cancelReport()
			wg.Wait()
		}()
	}
	// Under the controller's standbyGrace, so a node asleep stays so.
	const maxBackoff = 15 * time.Second
	backoff := time.Second
	for ctx.Err() == nil {
		err := standbyOnce(ctx, cluster, node, sleepAfter, vm, log)
		if err == nil {
			backoff = time.Second
			continue
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("standby: call the controller again later", "node", node, "in", backoff, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

func reportHostUsageLoop(ctx context.Context, cluster hotbedv1connect.ClusterServiceClient, node string, vm VM, hm HostMetricsVM, log *slog.Logger) {
	interval := hostUsageInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	timeout := hostUsageRPCTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	report := func() {
		if ctx.Err() != nil {
			return
		}
		tickCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if !vm.Running(tickCtx) {
			return
		}
		u, err := hm.SampleHostUsage(tickCtx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("standby: sample host usage failed", "node", node, "err", err)
			}
			return
		}
		if u == nil {
			return
		}
		if _, err := cluster.ReportHostUsage(tickCtx, &pb.ReportHostUsageRequest{Node: node, Usage: u}); err != nil && ctx.Err() == nil {
			log.Debug("standby: report host usage", "node", node, "err", err)
		}
	}
	report()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			report()
		}
	}
}

// standbyOnce is one Standby call: nil when it ended or vm changed, which
// calls for another.
func standbyOnce(ctx context.Context, cluster hotbedv1connect.ClusterServiceClient, node string, sleepAfter time.Duration, vm VM, log *slog.Logger) error {
	running := vm.Running(ctx)
	stream, err := cluster.Standby(ctx, &pb.StandbyRequest{Node: node, Running: running, SleepAfterSeconds: int64(sleepAfter / time.Second)})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		want, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case want.Run && !running:
			log.Info("standby: starting the VM", "node", node, "why", want.Reason)
			return vm.Start(ctx)
		case !want.Run && running:
			log.Info("standby: stopping the VM", "node", node, "why", want.Reason)
			return vm.Stop(ctx)
		}
	}
}
