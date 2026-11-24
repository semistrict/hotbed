package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/version"
)

// pushBatch is how many events one push carries at most, and
// checkpointEvents how many a checkpoint brings along. pushBytes is what a
// push's events encode to at most, well under the 4 MiB Connect lets the
// controller read: the largest event, a finished command's, carries a
// profile of at most profiler.MaxProfileBytes.
const (
	pushBatch        = 500
	checkpointEvents = 2000
	pushBytes        = 2 << 20
)

// firstFitting is the longest run of events from the first that encodes to
// at most budget bytes, or the first alone, so a push always gets on.
func firstFitting(events []*pb.Event, budget int) []*pb.Event {
	size := 0
	for i, ev := range events {
		if size += proto.Size(ev); size > budget && i > 0 {
			return events[:i]
		}
	}
	return events
}

// lastFitting is the longest run of events to the last that encodes to at
// most budget bytes, which may be none.
func lastFitting(events []*pb.Event, budget int) []*pb.Event {
	size := 0
	for i := len(events) - 1; i >= 0; i-- {
		if size += proto.Size(events[i]); size > budget {
			return events[i+1:]
		}
	}
	return events
}

// maxRetryWait caps exponential backoff after controller RPC failures so a
// worker never stays offline past OnlineWindow once the controller returns,
// while still retrying at half the healthy ReportInterval rate.
const (
	maxRetryWait          = 2 * api.ReportInterval
	heartbeatRPCTimeout   = 15 * time.Second
	cladefsHeartbeatLimit = 10 * time.Second
)

// Connect keeps the controller up to date until ctx ends: the event stream,
// in order, as soon as events are written, and a heartbeat every
// ReportInterval, whose reply says which repos to host and which workspaces
// to release, and the controller's build, which follow installs over the
// worker (it returns update.ErrRestart then; a nil follow installs none).
// Both ride out failures a retry can fix (the controller restarting, or out
// of reach for days: the worker works on, and its events wait), and stop at
// one they cannot. When either loop succeeds, it wakes the other out of any
// failure backoff so online status, events, and workspace releases recover
// together.
func Connect(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string, follow *Follow) error {
	return connectWithReset(ctx, eng, cluster, clusterID, follow, nil)
}

func connectWithReset(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string, follow *Follow, resetTransport func()) error {
	g, ctx := errgroup.WithContext(ctx)
	p := &pusher{eng: eng, cluster: cluster, clusterID: clusterID, follow: follow}
	wakePush := make(chan struct{}, 1)
	wakeBeat := make(chan struct{}, 1)
	beatKick := make(chan struct{}, 1)
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	onFail := func(err error) {
		if resetTransport == nil {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || api.CodeOf(err) == api.CodeControllerUnreachable {
			resetTransport()
		}
	}
	g.Go(func() error {
		return keepWithFailureHook(ctx, "push events", eng.Kicked(), wakePush, func() {
			signal(wakeBeat)
			if follow != nil && follow.deferred() != 0 && (!eng.hasActiveUpgradeWork() || follow.deferExpired()) {
				signal(beatKick)
			}
		}, onFail, p.once)
	})
	g.Go(func() error {
		return keepWithFailureHook(ctx, "heartbeat", beatKick, wakeBeat, func() { signal(wakePush) }, onFail, func(ctx context.Context) (bool, error) {
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
//
// While backing off (failures > 0), local kicks are ignored so event bursts
// do not hammer a down controller, while peer recovery on wake retries at once.
func keep(ctx context.Context, what string, kicked, wake <-chan struct{}, onOK func(), once func(context.Context) (bool, error)) error {
	return keepWithFailureHook(ctx, what, kicked, wake, onOK, nil, once)
}

func keepWithFailureHook(ctx context.Context, what string, kicked, wake <-chan struct{}, onOK func(), onFail func(error), once func(context.Context) (bool, error)) error {
	failures := 0
	for {
		if wake != nil {
			select {
			case <-wake:
			default:
			}
		}
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
			case failures == 0 || failures%10 == 0:
				slog.Warn(what+" failed; retrying", "err", e, "failures", failures+1)
			}
			if onFail != nil {
				onFail(err)
			}
			failures++
			more, wait = false, min(time.Second<<min(failures-1, 4), maxRetryWait)
		} else {
			if failures > 0 {
				slog.Info(what+" works again", "failures", failures)
				failures = 0
			}
			if onOK != nil {
				onOK()
			}
		}
		if more {
			continue
		}
		k := kicked
		var w <-chan struct{}
		if failures > 0 {
			// Back off rather than letting every local event hammer an
			// unreachable controller and jump straight to the backoff cap, but
			// wake immediately if our sibling loop proved the controller is up.
			k = nil
			w = wake
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		case <-k:
		case <-w:
		}
	}
}

// pusher delivers the engine's events in order, from after what the
// controller has applied.
type pusher struct {
	eng       *Engine
	cluster   hotbedv1connect.ClusterServiceClient
	clusterID string
	follow    *Follow

	known      bool  // whether applied is the controller's, as it last said
	applied    int64 // the last event the controller applied
	checkpoint bool  // the controller asked for one
}

// once makes one push and says whether there is more to send at once.
func (p *pusher) once(ctx context.Context) (bool, error) {
	if !p.known {
		return true, p.push(ctx, &pb.PushEventsRequest{})
	}
	if cp, gen, err := p.eng.unsaved(); err != nil || cp != nil {
		if err != nil {
			return false, err
		}
		if err := p.pushCheckpoint(ctx, cp); err != nil {
			return false, err
		}
		p.eng.markSaved(gen)
		return true, nil
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
		return true, p.pushCheckpoint(ctx, cp)
	}
	read, err := p.eng.Events(p.applied, pushBatch)
	if err != nil {
		return false, err
	}
	if len(read) == 0 {
		return p.pushDiagnostics(ctx)
	}
	events := firstFitting(read, pushBytes)
	shed := false
	if len(events) == 1 && proto.Size(events[0]) > pushBytes {
		if trimmed, ok := shedOversizedEvent(events[0]); ok {
			slog.Error("dropping oversized payload from event so the stream keeps moving", "seq", events[0].Seq, "bytes", proto.Size(events[0]), "trimmed_bytes", proto.Size(trimmed))
			events = []*pb.Event{trimmed}
			shed = true
		} else {
			slog.Error("an event is too big for one push; the controller may refuse it", "seq", events[0].Seq, "bytes", proto.Size(events[0]))
		}
	}
	afterAt, err := p.eng.db.eventAt(p.applied)
	if err != nil {
		return false, err
	}
	if err := p.push(ctx, &pb.PushEventsRequest{Events: events, AfterAt: afterAt}); err != nil {
		if connect.CodeOf(err) == connect.CodeResourceExhausted {
			if len(events) > 1 {
				// Retry just the first event so a smaller controller limit or
				// batch overhead does not trigger a checkpoint unnecessarily.
				events = events[:1]
				if err2 := p.push(ctx, &pb.PushEventsRequest{Events: events, AfterAt: afterAt}); err2 == nil {
					return true, nil
				} else if connect.CodeOf(err2) != connect.CodeResourceExhausted {
					return false, err2
				}
			}
			if !shed {
				if trimmed, ok := shedOversizedEvent(events[0]); ok {
					slog.Error("controller refused oversized event; retrying without profile", "seq", events[0].Seq, "bytes", proto.Size(events[0]), "err", err)
					if err2 := p.push(ctx, &pb.PushEventsRequest{Events: []*pb.Event{trimmed}, AfterAt: afterAt}); err2 == nil {
						more := len(read) == pushBatch || len(events) < len(read) || p.checkpoint
						if more {
							return true, nil
						}
						return p.pushDiagnostics(ctx)
					} else if connect.CodeOf(err2) != connect.CodeResourceExhausted {
						return false, err2
					}
				}
			}
			// Never wedge the node's entire event stream behind one unpushable
			// event: send a checkpoint next to skip past it and keep controller
			// views fresh.
			slog.Error("controller refused single oversized event; sending checkpoint to keep stream moving", "seq", events[0].Seq, "bytes", proto.Size(events[0]), "err", err)
			p.checkpoint = true
			return true, nil
		}
		return false, err
	}
	more := len(read) == pushBatch || len(events) < len(read) || p.checkpoint
	if more {
		return true, nil
	}
	return p.pushDiagnostics(ctx)
}

// pushDiagnostics delivers pending_diag records stored in worker.db to the
// controller via SaveCommandDiagnostics, deleting each record only after the
// controller explicitly acknowledges it.
func (p *pusher) pushDiagnostics(ctx context.Context) (bool, error) {
	pending, err := p.eng.db.listPendingDiagnostics()
	if err != nil || len(pending) == 0 {
		return false, err
	}
	for _, diag := range pending {
		if _, err := p.cluster.SaveCommandDiagnostics(ctx, &pb.SaveCommandDiagnosticsRequest{Diagnostics: diag}); err != nil {
			return false, err
		}
		if err := p.eng.db.deletePendingDiagnostics(diag.CommandId, diag.GetIntervention().GetSeq()); err != nil {
			slog.Error("delete acknowledged pending diagnostics", "command", diag.CommandId, "seq", diag.GetIntervention().GetSeq(), "err", err)
			return false, err
		}
	}
	return false, nil
}

// shedOversizedEvent drops optional bulk payloads (command profiles and any
// legacy inline diagnostics) from an event that would otherwise exceed the RPC
// size limit and wedge the stream.
func shedOversizedEvent(ev *pb.Event) (*pb.Event, bool) {
	c := ev.GetCommand()
	if c == nil || (c.Profile == nil && c.Diagnostics == nil) {
		return ev, false
	}
	cp := proto.Clone(ev).(*pb.Event)
	cp.GetCommand().Profile = nil
	cp.GetCommand().Diagnostics = nil
	return cp, true
}

func (p *pusher) pushCheckpoint(ctx context.Context, cp *pb.Checkpoint) error {
	// With the events it includes that the controller may lack, the
	// newest of them, for what derives from them there.
	events, err := p.eng.Events(max(p.applied, cp.Seq-checkpointEvents), checkpointEvents)
	if err != nil {
		return err
	}
	events = slices.DeleteFunc(events, func(ev *pb.Event) bool { return ev.Seq > cp.Seq })
	// The newest of them, as many as the checkpoint leaves room for.
	kept := lastFitting(events, pushBytes-proto.Size(cp))
	if len(kept) < len(events) {
		slog.Warn("a checkpoint leaves out older events it includes, too big to bring along: the controller loses what derives from them (history, profiles)", "left_out", len(events)-len(kept), "kept", len(kept))
	}
	err = p.push(ctx, &pb.PushEventsRequest{Checkpoint: cp, Events: kept})
	if err != nil && connect.CodeOf(err) == connect.CodeResourceExhausted && len(kept) > 0 {
		slog.Warn("controller refused checkpoint with trailing events; retrying checkpoint alone", "events", len(kept), "err", err)
		err = p.push(ctx, &pb.PushEventsRequest{Checkpoint: cp})
	}
	return err
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

// HeartbeatOnce sends one heartbeat and applies the reply, which may hand
// it the controller's build to install over itself (update.ErrRestart).
func HeartbeatOnce(ctx context.Context, eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string, follow *Follow) error {
	sent := eng.now()
	h := eng.Heartbeat(clusterID)
	hbCtx, cancelHB := context.WithTimeout(ctx, heartbeatRPCTimeout)
	reply, err := cluster.Heartbeat(hbCtx, h)
	cancelHB()
	if err != nil {
		return err
	}
	eng.delivered(h)
	eng.setProjects(reply.Projects, sent)
	eng.SetHostUsage(reply.GetHostUsage())
	cfCtx, cancelCF := context.WithTimeout(ctx, cladefsHeartbeatLimit)
	eng.applyCladefsHeartbeat(cfCtx, reply)
	cancelCF()
	for _, id := range reply.Release {
		eng.ReleaseWorkspaceAsync(id)
	}
	return follow.toWhenIdle(ctx, eng, reply.Build)
}

// defaultMaxUpgradeDefer caps how long a worker defers an upgrade while active
// work runs, so a stuck job or long-lived session cannot strand a node in
// "upgrading" (refused by controller placement) indefinitely.
const defaultMaxUpgradeDefer = 15 * time.Minute

// activeUpgradeWork reports how many active commands, interactive shells, and
// in-flight base/fork builds would be disrupted if the worker restarted now.
// Paused commands and long-lived server commands (Kind == "server", which
// survive worker restarts under systemd) are excluded so they do not hold back
// upgrades indefinitely.
func (e *Engine) activeUpgradeWork() (commands, shells, bases int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.commands {
		cmd := c.cmd()
		if api.Live(cmd.State) && cmd.State != paused && cmd.Kind != "server" {
			commands++
		}
	}
	shells = len(e.shells)
	for _, b := range e.bases {
		if b.State == building || e.finalizingBases[b.Id] {
			bases++
		}
	}
	for _, w := range e.workspaces {
		if w.forking {
			bases++
		}
	}
	return commands, shells, bases
}

func (e *Engine) hasActiveUpgradeWork() bool {
	cmds, shells, bases := e.activeUpgradeWork()
	return cmds > 0 || shells > 0 || bases > 0
}

// Follow updates the worker to at least the controller's build, fetching
// directly from GitHub when the controller supplies a download URL (and
// otherwise from the controller's /builds/ endpoint), verifying sha256 before
// atomically replacing Exe and returning update.ErrRestart so serve re-execs.
type Follow struct {
	URL            string           // the controller's base URL (used when Build.DownloadUrl is empty)
	Client         *http.Client     // controller-authenticated client for Build.Path fallback
	DownloadClient *http.Client     // clean client (no controller auth) for Build.DownloadUrl (GitHub)
	Exe            string           // the worker's binary
	Build          int64            // the build it runs, 0 for a dev build
	MaxDefer       time.Duration    // cap on deferring an upgrade while active work runs; 0 uses defaultMaxUpgradeDefer
	Now            func() time.Time // clock for deferral cap; nil uses time.Now

	mu            sync.Mutex
	deferredBuild int64     // target build deferred while active work drains
	deferredSince time.Time // first moment this worker began deferring an upgrade
}

func (f *Follow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Follow) maxDefer() time.Duration {
	if f.MaxDefer > 0 {
		return f.MaxDefer
	}
	return defaultMaxUpgradeDefer
}

func (f *Follow) deferred() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deferredBuild
}

func (f *Follow) deferExpired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deferredBuild != 0 && !f.deferredSince.IsZero() && f.now().Sub(f.deferredSince) >= f.maxDefer()
}

func (f *Follow) clearDeferred() {
	f.mu.Lock()
	f.deferredBuild = 0
	f.deferredSince = time.Time{}
	f.mu.Unlock()
}

// checkDefer reports whether upgrading to b should be deferred because eng has
// active work and the deferral cap has not elapsed. When eng is idle, any
// prior deferral state is cleared. When active work is present, deferredSince
// stays anchored to the first deferral even if a newer build arrives, so rapid
// pushes cannot reset the cap indefinitely.
func (f *Follow) checkDefer(eng *Engine, b *pb.Build) (deferNow, capReached bool) {
	if eng == nil {
		return false, false
	}
	cmds, shells, bases := eng.activeUpgradeWork()
	if cmds == 0 && shells == 0 && bases == 0 {
		f.clearDeferred()
		return false, false
	}
	now := f.now()
	maxDefer := f.maxDefer()

	f.mu.Lock()
	if f.deferredSince.IsZero() {
		f.deferredSince = now
	}
	elapsed := now.Sub(f.deferredSince)
	if elapsed >= maxDefer {
		f.mu.Unlock()
		slog.Warn("upgrade deferral cap reached; upgrading with active work",
			"target_build", b.Number, "was", version.String(),
			"deferred_for", elapsed, "commands", cmds, "shells", shells, "bases", bases)
		return false, true
	}
	prev := f.deferredBuild
	f.deferredBuild = b.Number
	f.mu.Unlock()
	if prev != b.Number {
		slog.Info("deferring worker upgrade until active work finishes",
			"target_build", b.Number, "was", version.String(),
			"commands", cmds, "shells", shells, "bases", bases)
	}
	return true, false
}

// toWhenIdle defers both replacing f.Exe and returning update.ErrRestart until
// the engine has no active commands, interactive shells, or in-flight base/fork
// builds, up to f.maxDefer(). Workers update whenever their build is older than
// the controller's target build, and never downgrade when already on a newer build.
func (f *Follow) toWhenIdle(ctx context.Context, eng *Engine, b *pb.Build) error {
	if f == nil || b == nil || b.Number <= 0 || f.Build >= b.Number {
		if f != nil {
			f.clearDeferred()
		}
		return nil
	}
	deferNow, forced := f.checkDefer(eng, b)
	if deferNow {
		return nil
	}
	tmp, source, err := f.downloadStaged(ctx, b)
	if err != nil {
		slog.Error("install the controller's build", "build", b.Number, "err", err)
		return nil
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmp)
		}
	}()

	// Re-check for active work that started during the download before
	// atomically swapping f.Exe, unless the deferral cap has already elapsed.
	if !forced {
		if deferNow, _ := f.checkDefer(eng, b); deferNow {
			return nil
		}
	}

	if err := os.Rename(tmp, f.Exe); err != nil {
		slog.Error("install the controller's build", "build", b.Number, "err", err)
		return nil
	}
	cleanup = false
	f.clearDeferred()
	slog.Info("installed the controller's build: restarting as it", "build", b.Number, "was", version.String(), "source", source)
	return update.ErrRestart
}

// To installs b when the worker runs an older build, returning update.ErrRestart
// once it did. A failure to is logged, and tried again at the next report.
func (f *Follow) To(ctx context.Context, b *pb.Build) error {
	return f.toWhenIdle(ctx, nil, b)
}

func (f *Follow) downloadClient() *http.Client {
	if f.DownloadClient != nil {
		return f.DownloadClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (f *Follow) downloadStaged(ctx context.Context, b *pb.Build) (path, source string, err error) {
	if b.Sha256 == "" {
		return "", "", fmt.Errorf("build %d has no sha256 checksum", b.Number)
	}
	var downloadURL string
	var client *http.Client
	if b.DownloadUrl != "" {
		u, uerr := url.Parse(b.DownloadUrl)
		if uerr != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "", "", fmt.Errorf("build %d has invalid download URL %q", b.Number, b.DownloadUrl)
		}
		downloadURL = b.DownloadUrl
		client = f.downloadClient()
	} else {
		if b.Path == "" {
			return "", "", fmt.Errorf("build %d has neither download_url nor path", b.Number)
		}
		downloadURL = f.URL + b.Path
		client = f.Client
		if client == nil {
			client = f.downloadClient()
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "hotbed-worker")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET %s: %s", req.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Exe), "."+filepath.Base(f.Exe)+".staged-")
	if err != nil {
		return "", "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", "", err
	}
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()
	if err = update.Install(resp.Body, b.Sha256, tmpPath); err != nil {
		return "", "", err
	}
	return tmpPath, downloadURL, nil
}
