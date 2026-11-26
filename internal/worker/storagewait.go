package worker

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/version"
)

// awaitStorage opens the worker's storage, and while it cannot (a ZFS pool
// whose disk is late or gone, board 9141) waits for it rather than exit:
// every interval it tells the controller why (a heartbeat with
// storage_error, which keeps the node listed offline with the reason) and
// tries again. It still installs the controller's build meanwhile (it
// returns update.ErrRestart then), so a fix reaches a waiting worker. It
// stops at what no retry fixes: a worker the controller does not know, or
// of another cluster.
func awaitStorage(
	ctx context.Context,
	open func(context.Context) (storage.Store, error),
	cluster hotbedv1connect.ClusterServiceClient,
	clusterID string,
	follow *Follow,
	interval time.Duration,
) (storage.Store, error) {
	var last string
	beatFailing := false
	for {
		store, err := open(ctx)
		if err == nil {
			if last != "" {
				slog.Info("storage open; the worker starts", "storage", store.Kind())
			}
			return store, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if msg := err.Error(); msg != last {
			slog.Error("cannot open the worker's storage; waiting for it, reporting it to the controller", "err", err, "retry", interval)
			last = msg
		}
		reply, herr := cluster.Heartbeat(ctx, &pb.HeartbeatRequest{
			ClusterId:    clusterID,
			StorageError: last,
			Resources:    &pb.Resources{Os: runtime.GOOS, Arch: runtime.GOARCH, Cpus: int32(runtime.NumCPU()), Version: version.String()},
		})
		switch e := api.FromConnect(herr); {
		case herr == nil:
			beatFailing = false
			if err := follow.toWhenIdle(ctx, nil, reply.GetBuild()); err != nil {
				return nil, err
			}
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case e.Code == api.CodeNotJoined || e.Code == api.CodeClusterMismatch:
			return nil, e
		case !beatFailing:
			slog.Warn("report storage failure to the controller; retrying", "err", e)
			beatFailing = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}
