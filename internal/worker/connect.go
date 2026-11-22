package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/version"
)

// pushBatch is how many events one push carries at most, and
// checkpointEvents how many a checkpoint brings along.
const (
	pushBatch        = 500
	checkpointEvents = 2000
)

// Connect keeps the controller up to date until ctx ends: the event stream,
// in order, as soon as events are written, and a heartbeat every
// ReportInterval, whose reply says which repos to host and which workspaces
// to release, and the controller's build, which follow installs over the
// worker (it returns update.ErrRestart then; a nil follow installs none).
// Both ride out failures a retry can fix (the controller restarting, or out
// of reach for days: the worker works on, and its events wait), and stop at
// one they cannot.
func Connect(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string, follow *Follow) error {
	g, ctx := errgroup.WithContext(ctx)
	p := &pusher{eng: eng, cluster: cluster, clusterID: clusterID}
	g.Go(func() error { return keep(ctx, "push events", eng.Kicked(), p.once) })
	g.Go(func() error {
		return keep(ctx, "heartbeat", nil, func(ctx context.Context) (bool, error) {
			return false, HeartbeatOnce(ctx, eng, cluster, clusterID, follow)
		})
	})
	return g.Wait()
}

// keep runs once until ctx ends: again at once while it says there is more,
// otherwise after ReportInterval or when kicked. Failures it backs off from
// and retries, logging the first of a run, all but those no retry can fix:
// a worker the controller does not know, or of another cluster. A
// controller older than the worker (PushEvents unimplemented) is waited
// out, not a reason to stop working.
func keep(ctx context.Context, what string, kicked <-chan struct{}, once func(context.Context) (bool, error)) error {
	failures := 0
	for {
		more, err := once(ctx)
		wait := api.ReportInterval
		if err != nil {
			e := api.FromConnect(err)
			switch {
			case errors.Is(err, update.ErrRestart):
				return err
			case ctx.Err() != nil:
				return nil
			case e.Code == api.CodeNotJoined || e.Code == api.CodeClusterMismatch:
				return e
			case failures == 0:
				slog.Warn(what+" failed; retrying", "err", e)
			}
			failures++
			more, wait = false, min(time.Second<<min(failures-1, 5), 30*time.Second)
		} else if failures > 0 {
			slog.Info(what+" works again", "failures", failures)
			failures = 0
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
	if cp, err := p.eng.unsaved(); err != nil || cp != nil {
		if err != nil {
			return false, err
		}
		return true, p.push(ctx, &pb.PushEventsRequest{Checkpoint: cp})
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
		// With the events it includes that the controller may lack, the
		// newest of them, for what derives from them there.
		events, err := p.eng.Events(max(p.applied, cp.Seq-checkpointEvents), checkpointEvents)
		if err != nil {
			return false, err
		}
		events = slices.DeleteFunc(events, func(ev *pb.Event) bool { return ev.Seq > cp.Seq })
		return true, p.push(ctx, &pb.PushEventsRequest{Checkpoint: cp, Events: events})
	}
	events, err := p.eng.Events(p.applied, pushBatch)
	if err != nil || len(events) == 0 {
		return false, err
	}
	afterAt, err := p.eng.db.eventAt(p.applied)
	if err != nil {
		return false, err
	}
	if err := p.push(ctx, &pb.PushEventsRequest{Events: events, AfterAt: afterAt}); err != nil {
		return false, err
	}
	return len(events) == pushBatch || p.checkpoint, nil
}

func (p *pusher) push(ctx context.Context, req *pb.PushEventsRequest) error {
	req.ClusterId, req.Epoch = p.clusterID, p.eng.Epoch()
	res, err := p.cluster.PushEvents(ctx, req)
	if err != nil {
		return err
	}
	p.known, p.applied, p.checkpoint = true, res.Applied, res.CheckpointNeeded
	p.eng.applied.Store(res.Applied)
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

// HeartbeatOnce sends one heartbeat and applies the reply, which may hand
// it the controller's build to install over itself (update.ErrRestart).
func HeartbeatOnce(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string, follow *Follow) error {
	sent := eng.now()
	h := eng.Heartbeat(clusterID)
	reply, err := cluster.Heartbeat(ctx, h)
	if err != nil {
		return err
	}
	eng.delivered(h)
	eng.setRepos(reply.Repos, sent)
	for _, id := range reply.Release {
		if err := eng.ReleaseWorkspace(ctx, id); err != nil && api.FromConnect(err).Code != api.CodeNotFound {
			slog.Error("release workspace of an ended session", "workspace", id, "err", err)
		}
	}
	return follow.To(ctx, reply.Build)
}

// Follow installs the controller's build over the worker, from the
// controller, which keeps it: a worker never reaches GitHub.
type Follow struct {
	URL    string // the controller's, http://host:port
	Client *http.Client
	Exe    string // the worker's binary
	Build  int64  // the build it runs, 0 for a dev build
}

// To installs b unless the worker runs it, returning update.ErrRestart once
// it did. A failure to is logged, and tried again at the next report.
func (f *Follow) To(ctx context.Context, b *pb.Build) error {
	if f == nil || b == nil || b.Number == f.Build {
		return nil
	}
	if err := f.install(ctx, b); err != nil {
		slog.Error("install the controller's build", "build", b.Number, "err", err)
		return nil
	}
	slog.Info("installed the controller's build: restarting as it", "build", b.Number, "was", version.String())
	return update.ErrRestart
}

func (f *Follow) install(ctx context.Context, b *pb.Build) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL+b.Path, nil)
	if err != nil {
		return err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", req.URL, resp.Status)
	}
	return update.Install(resp.Body, b.Sha256, f.Exe)
}
