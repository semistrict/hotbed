package worker

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// pushBatch is how many events one push carries at most.
const pushBatch = 500

// Connect keeps the controller up to date until ctx ends: the event stream,
// in order, as soon as events are written, and a heartbeat every
// ReportInterval, whose reply says which repos to host and which workspaces
// to release. Both ride out failures a retry can fix (the controller
// restarting, or out of reach for days: the worker works on, and its events
// wait), and stop at one they cannot.
func Connect(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string) error {
	g, ctx := errgroup.WithContext(ctx)
	p := &pusher{eng: eng, cluster: cluster, clusterID: clusterID}
	g.Go(func() error { return keep(ctx, "push events", eng.Kicked(), p.once) })
	g.Go(func() error {
		return keep(ctx, "heartbeat", nil, func(ctx context.Context) (bool, error) {
			return false, HeartbeatOnce(ctx, eng, cluster, clusterID)
		})
	})
	return g.Wait()
}

// keep runs once until ctx ends: again at once while it says there is more,
// otherwise after ReportInterval or when kicked. It logs the first of a run
// of retryable failures and backs off from them.
func keep(ctx context.Context, what string, kicked <-chan struct{}, once func(context.Context) (bool, error)) error {
	failing := false
	for {
		more, err := once(ctx)
		wait := api.ReportInterval
		if err != nil {
			e := api.FromConnect(err)
			switch {
			case ctx.Err() != nil:
				return nil
			case !e.Retryable:
				return e
			case !failing:
				slog.Warn(what+" failed; retrying", "err", e)
			}
			failing, more, wait = true, false, time.Second
		} else if failing {
			slog.Info(what + " works again")
			failing = false
		}
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		case <-kicked:
		}
	}
}

// pusher delivers the engine's events in order, from after what the
// controller has applied.
type pusher struct {
	eng       *Engine
	cluster   hotbedv1connect.ClusterServiceClient
	clusterID string

	known      bool  // whether applied is the controller's, as it last said
	applied    int64 // the last event the controller applied
	checkpoint bool  // the controller asked for one
	drained    bool  // the old outbox is empty
}

// once makes one push and says whether there is more to send at once.
func (p *pusher) once(ctx context.Context) (bool, error) {
	if !p.drained {
		return p.drain(ctx)
	}
	if !p.known {
		return true, p.push(ctx, &pb.PushEventsRequest{})
	}
	oldest, newest, err := p.eng.db.eventRange()
	if err != nil {
		return false, err
	}
	if p.applied > newest {
		// The controller has applied events this database never wrote: the
		// database is an older copy. A new epoch makes it start again from a
		// checkpoint, rather than have the controller drop the events it
		// numbers again as ones it has.
		slog.Warn("the controller is ahead of this worker's database; starting a new epoch", "applied", p.applied, "newest", newest)
		if err := p.eng.db.newEpoch(); err != nil {
			return false, err
		}
		p.known = false
		return true, nil
	}
	if p.checkpoint || (newest > p.applied && oldest > p.applied+1) {
		cp, err := p.eng.Checkpoint()
		if err != nil {
			return false, err
		}
		return true, p.push(ctx, &pb.PushEventsRequest{Checkpoint: cp})
	}
	events, err := p.eng.Events(p.applied, pushBatch)
	if err != nil || len(events) == 0 {
		return false, err
	}
	if err := p.push(ctx, &pb.PushEventsRequest{Events: events}); err != nil {
		return false, err
	}
	p.eng.pruneEvents(p.applied)
	return len(events) == pushBatch || p.checkpoint, nil
}

func (p *pusher) push(ctx context.Context, req *pb.PushEventsRequest) error {
	req.ClusterId, req.Epoch = p.clusterID, p.eng.Epoch()
	res, err := p.cluster.PushEvents(ctx, req)
	if err != nil {
		return err
	}
	p.known, p.applied, p.checkpoint = true, res.Applied, res.CheckpointNeeded
	return nil
}

// drain delivers what a worker from before the event stream left in its
// outbox, the way that worker would have.
func (p *pusher) drain(ctx context.Context) (bool, error) {
	items, err := p.eng.db.pending(maxLegacyItems)
	if err != nil {
		return false, err
	}
	if len(items) == 0 {
		p.drained = true
		return true, nil
	}
	r, err := p.eng.legacyReport(p.clusterID, items)
	if err != nil {
		return false, err
	}
	reply, err := p.cluster.Report(ctx, r)
	if err != nil {
		return false, err
	}
	if err := p.eng.db.ack(reply.Ack); err != nil {
		return false, err
	}
	return true, nil
}

// HeartbeatOnce sends one heartbeat and applies the reply.
func HeartbeatOnce(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string) error {
	reply, err := cluster.Heartbeat(ctx, eng.Heartbeat(clusterID))
	if err != nil {
		return err
	}
	eng.SetRepos(reply.Repos)
	for _, id := range reply.Release {
		if err := eng.ReleaseWorkspace(ctx, id); err != nil && api.FromConnect(err).Code != api.CodeNotFound {
			slog.Error("release workspace of an ended session", "workspace", id, "err", err)
		}
	}
	return nil
}
