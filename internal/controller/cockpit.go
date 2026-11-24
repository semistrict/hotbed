package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

const (
	liveWindow    = 15 * time.Minute
	sampleSeconds = 5
	eventsShown   = 80
	// placementsShown and admissionEventsShown cap the placement decisions
	// and the holds, resumes and waits a snapshot carries.
	placementsShown      = 300
	admissionEventsShown = 500
	// samplesCacheTTL bounds how long GetSnapshot reuses the aggregated 24h
	// samples and heatmap when no new heartbeats have arrived. New heartbeats
	// bump samplesGen and hour rollover checks curHour, so 30s avoids redundant
	// 24h SQLite scans on quiet 1s cockpit polls while staying fresh on writes.
	samplesCacheTTL = 30 * time.Second
)

type samplesSnapshot struct {
	gen        uint64
	at         time.Time
	hour       int64
	series     map[string]*pb.Samples
	load       []*pb.LoadSample
	nodeHourly map[string]*pb.NodeHours
	oomKills   int32
}

type samplesFill struct {
	gen  uint64
	hour int64
	done chan struct{}
	snap *samplesSnapshot
	err  error
}

func (s *Server) cachedSamples(ctx context.Context, now time.Time) (map[string]*pb.Samples, []*pb.LoadSample, map[string]*pb.NodeHours, int32, error) {
	curHour := hourOf(now.UnixMilli())
	for {
		gen := s.cfg.Store.samplesVersion()
		s.samplesMu.Lock()
		if c := s.samplesCache; c != nil && c.gen == gen && c.hour == curHour && !now.Before(c.at) && now.Sub(c.at) < samplesCacheTTL {
			s.samplesMu.Unlock()
			return c.series, c.load, c.nodeHourly, c.oomKills, nil
		}
		if f := s.samplesInFlight; f != nil && f.gen == gen && f.hour == curHour {
			s.samplesMu.Unlock()
			select {
			case <-f.done:
				if f.err != nil {
					if ctx.Err() == nil && (errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) {
						continue
					}
					return nil, nil, nil, 0, f.err
				}
				return f.snap.series, f.snap.load, f.snap.nodeHourly, f.snap.oomKills, nil
			case <-ctx.Done():
				return nil, nil, nil, 0, ctx.Err()
			}
		}
		f := &samplesFill{gen: gen, hour: curHour, done: make(chan struct{})}
		s.samplesInFlight = f
		s.samplesMu.Unlock()

		finished := false
		defer func() {
			if !finished {
				s.samplesMu.Lock()
				f.err = fmt.Errorf("samples cache fill aborted")
				if s.samplesInFlight == f {
					s.samplesInFlight = nil
				}
				close(f.done)
				s.samplesMu.Unlock()
			}
		}()

		samples, err := s.cfg.Store.Samples(ctx, now.Add(-24*time.Hour))
		var snap *samplesSnapshot
		if err == nil {
			ser, load, nodeHourly := series(samples, now)
			oom := oomKills(samples)
			snap = &samplesSnapshot{
				gen:        gen,
				at:         now,
				hour:       curHour,
				series:     ser,
				load:       load,
				nodeHourly: nodeHourly,
				oomKills:   oom,
			}
		}

		s.samplesMu.Lock()
		f.snap, f.err = snap, err
		if err == nil && (s.samplesCache == nil || snap.gen >= s.samplesCache.gen) {
			s.samplesCache = snap
		}
		if s.samplesInFlight == f {
			s.samplesInFlight = nil
		}
		finished = true
		close(f.done)
		s.samplesMu.Unlock()

		if err != nil {
			return nil, nil, nil, 0, err
		}
		return snap.series, snap.load, snap.nodeHourly, snap.oomKills, nil
	}
}

// GetSnapshot is the cockpit's whole view of the cluster.
func (s *Server) GetSnapshot(ctx context.Context, _ *pb.GetSnapshotRequest) (*pb.Snapshot, error) {
	now := s.now()
	day := now.Add(-24 * time.Hour)
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	history, err := s.cfg.Store.History(ctx, day)
	if err != nil {
		return nil, err
	}
	events, err := s.cfg.Store.Events(ctx, eventsShown)
	if err != nil {
		return nil, err
	}
	ser, load, nodeHourly, oom, err := s.cachedSamples(ctx, now)
	if err != nil {
		return nil, err
	}
	forks, err := s.cfg.Store.Forks(ctx, day)
	if err != nil {
		return nil, err
	}
	sessions, err := s.cfg.Store.RecentSessions(ctx, day, "", "")
	if err != nil {
		return nil, err
	}
	schedules, err := s.allSchedules(ctx, "")
	if err != nil {
		return nil, err
	}
	open, err := s.cfg.Store.OpenQuestions(ctx)
	if err != nil {
		return nil, err
	}
	projects, err := s.projects(ctx)
	if err != nil {
		return nil, err
	}

	snap := &pb.Snapshot{
		ClusterId: s.cfg.Cluster.ClusterID, Controller: s.cfg.Controller,
		Now: now.UnixMilli(), SampleSeconds: sampleSeconds, Events: events, History: history,
		Sessions: sessions, Schedules: schedules, OpenQuestions: open, Projects: projects,
		ControllerLogs: s.cfg.DaemonLogs.Summary("controller", "controller", s.cfg.Controller),
	}
	for _, nv := range nvs {
		node := nv.node
		if !nv.online {
			node.Usage, node.Admission, node.HostUsage = &pb.NodeUsage{}, nil, nil
		}
		snap.Nodes = append(snap.Nodes, node)
		snap.Bases = append(snap.Bases, nv.bases...)
		snap.Workspaces = append(snap.Workspaces, nv.workspaces...)
		if nv.online {
			for _, c := range nv.commands {
				if c.Kind != "freshen" {
					snap.Commands = append(snap.Commands, c)
				}
			}
		}
	}
	slices.SortFunc(snap.Commands, func(a, b *pb.Command) int { return cmp.Compare(a.QueuedAt, b.QueuedAt) })
	for _, repo := range repos {
		snap.Repos = append(snap.Repos, summarize(repo, snap.Bases))
	}
	snap.Series, snap.Load, snap.NodeHourly = s.overlayHostSamples(ser, nvs, now), load, nodeHourly
	snap.Hourly = hourly(history, forks, now)
	snap.Totals24H = totals(history, forks, oom)
	if snap.Placements, err = s.cfg.Store.Placements(ctx, day); err != nil {
		return nil, err
	}
	if len(snap.Placements) > placementsShown {
		snap.Placements = snap.Placements[:placementsShown]
	}
	if snap.AdmissionEvents, err = s.events(ctx, "", admissionKinds, admissionEventsShown); err != nil {
		return nil, err
	}
	return snap, nil
}

// summarize describes a repo by the newest cold base that checked it out:
// what it is written in, and how many units that base built and how long
// it took from nothing.
func summarize(repo *pb.Repo, bases []*pb.Base) *pb.RepoSummary {
	out := &pb.RepoSummary{Id: repo.Id, Name: strings.TrimSuffix(nameOf(repo.Url), ".git"), Remote: gitops.CanonicalRemote(repo.Url)}
	var cold *pb.Base
	var checkout *pb.BaseRepo
	for _, b := range bases {
		if !b.Cold || (b.State != pb.BaseState_BASE_STATE_WARM && b.State != pb.BaseState_BASE_STATE_STALE) {
			continue
		}
		i := slices.IndexFunc(b.Repos, func(r *pb.BaseRepo) bool { return r.Repo == repo.Id })
		if i >= 0 && (cold == nil || b.Generation > cold.Generation) {
			cold, checkout = b, b.Repos[i]
		}
	}
	if cold == nil {
		return out
	}
	out.Units, out.ColdBuildSeconds = cold.Units, cold.BuildSeconds
	switch {
	case strings.HasPrefix(checkout.Toolchain, "rust"):
		out.Language = "rust"
	case strings.HasPrefix(checkout.Toolchain, "go"):
		out.Language = "go"
	case strings.HasPrefix(checkout.Toolchain, "node"):
		out.Language = "typescript"
	}
	return out
}

func nameOf(u string) string {
	parts := strings.FieldsFunc(strings.TrimSuffix(u, "/"), func(r rune) bool { return r == '/' || r == ':' })
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return filepath.Base(u)
}

// series turns samples into the live charts (the last 15 minutes per node,
// and the cluster's command counts) and the 24-hour heatmap.
func series(samples []sampleRow, now time.Time) (map[string]*pb.Samples, []*pb.LoadSample, map[string]*pb.NodeHours) {
	live := map[string]*pb.Samples{}
	type loadCounts struct {
		running, queued, parked int32
	}
	// Keep one sample per node per 5s bucket (the latest, since samples are
	// oldest first) so two heartbeats from the same node in one bucket never
	// double-count running/queued/parked.
	bucketNodes := map[int64]map[string]loadCounts{}
	type acc struct {
		cpu, mem float64
		held, n  int
	}
	hours := map[string]map[int64]*acc{}
	since := now.Add(-liveWindow).UnixMilli()
	for _, s := range samples {
		if s.t >= since {
			if live[s.node] == nil {
				live[s.node] = &pb.Samples{}
			}
			live[s.node].Samples = append(live[s.node].Samples, &pb.Sample{T: s.t, CpuBusy: s.cpu, MemoryUsed: s.memory, MemoryPressure: s.psi, CpuPressure: s.cpuPsi, Holding: s.holding})
			b := s.t / (sampleSeconds * 1000) * (sampleSeconds * 1000)
			nodes := bucketNodes[b]
			if nodes == nil {
				nodes = map[string]loadCounts{}
				bucketNodes[b] = nodes
			}
			nodes[s.node] = loadCounts{running: int32(s.running), queued: int32(s.queued), parked: int32(s.parked)}
		}
		if hours[s.node] == nil {
			hours[s.node] = map[int64]*acc{}
		}
		h := hourOf(s.t)
		a := hours[s.node][h]
		if a == nil {
			a = &acc{}
			hours[s.node][h] = a
		}
		a.cpu += s.cpu
		a.mem += s.memory
		if s.holding {
			a.held++
		}
		a.n++
	}
	loads := make([]*pb.LoadSample, 0, len(bucketNodes))
	for b, nodes := range bucketNodes {
		l := &pb.LoadSample{T: b}
		for _, c := range nodes {
			l.Running += c.running
			l.Queued += c.queued
			l.Parked += c.parked
		}
		loads = append(loads, l)
	}
	sort.Slice(loads, func(i, j int) bool { return loads[i].T < loads[j].T })
	nodeHourly := map[string]*pb.NodeHours{}
	for node, hs := range hours {
		out := &pb.NodeHours{}
		for _, h := range dayHours(now) {
			nh := &pb.NodeHour{Hour: h}
			if a := hs[h]; a != nil {
				nh.CpuBusy, nh.MemoryUsed, nh.Holding = a.cpu/float64(a.n), a.mem/float64(a.n), float64(a.held)/float64(a.n)
			}
			out.Hours = append(out.Hours, nh)
		}
		nodeHourly[node] = out
	}
	return live, loads, nodeHourly
}

// hourly is the last 24 hours, one bucket per hour, from history and forks.
func hourly(history []*pb.FinishedCommand, forks []forkRow, now time.Time) []*pb.HourBucket {
	idx := map[int64]int{}
	var out []*pb.HourBucket
	for i, h := range dayHours(now) {
		idx[h] = i
		out = append(out, &pb.HourBucket{Hour: h})
	}
	waits := make([][]float64, len(out))
	for _, c := range history {
		i, ok := idx[hourOf(c.EndedAt)]
		if !ok {
			continue
		}
		b := out[i]
		waits[i] = append(waits[i], c.QueuedSeconds)
		b.Parks += c.Parks
		if c.ColdSeconds == nil || c.State == pb.CommandState_COMMAND_STATE_CANCELLED {
			continue
		}
		b.Builds++
		b.SpentSeconds += c.Seconds
		b.SavedSeconds += math.Max(0, *c.ColdSeconds-c.Seconds)
	}
	for _, f := range forks {
		if i, ok := idx[hourOf(f.at)]; ok {
			out[i].Forks++
		}
	}
	for i := range out {
		out[i].QueueWaitP95Seconds = quantile(waits[i], 0.95)
	}
	return out
}

func oomKills(samples []sampleRow) int32 {
	// The kernel's OOM-kill counter only grows until a reboot; sum each
	// node's growth over the day.
	first, last := map[string]int64{}, map[string]int64{}
	for _, s := range samples {
		if _, ok := first[s.node]; !ok {
			first[s.node] = s.oom
		}
		if s.oom < last[s.node] {
			first[s.node] -= last[s.node] // rebooted: keep what came before
		}
		last[s.node] = s.oom
	}
	var total int32
	for n, l := range last {
		total += int32(l - first[n])
	}
	return total
}

func totals(history []*pb.FinishedCommand, forks []forkRow, oom int32) *pb.Totals {
	t := &pb.Totals{OomKills: oom}
	for _, c := range history {
		t.Parks += c.Parks
		if c.ColdSeconds == nil || c.State == pb.CommandState_COMMAND_STATE_CANCELLED {
			continue
		}
		t.Builds++
		t.SavedSeconds += math.Max(0, *c.ColdSeconds-c.Seconds)
	}
	t.Forks = int32(len(forks))
	var ms []float64
	for _, f := range forks {
		if f.millis >= 0 {
			ms = append(ms, float64(f.millis))
		}
	}
	t.MedianForkMillis = int64(quantile(ms, 0.5))
	return t
}

func dayHours(now time.Time) []int64 {
	cur := hourOf(now.UnixMilli())
	out := make([]int64, 24)
	for i := range out {
		out[i] = cur - int64(23-i)*3600_000
	}
	return out
}

func hourOf(ms int64) int64 { return ms / 3600_000 * 3600_000 }

func quantile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[int(math.Round(p*float64(len(s)-1)))]
}

// maxDiagnosticsResponseBytes caps a single GetCommandDiagnostics page well
// below Connect's 4 MiB default message limit while always admitting at least
// one report so pagination makes progress.
const maxDiagnosticsResponseBytes = 2 << 20

// SaveCommandDiagnostics persists a worker's captured intervention diagnostics
// idempotently so they survive event-stream checkpoints and worker resets.
func (s *Server) SaveCommandDiagnostics(ctx context.Context, req *pb.SaveCommandDiagnosticsRequest) (*pb.SaveCommandDiagnosticsResponse, error) {
	peer, err := s.joined(ctx)
	if err != nil {
		return nil, err
	}
	d := req.GetDiagnostics()
	if d == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "diagnostics is required")
	}
	if strings.TrimSpace(d.CommandId) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "command ID is required")
	}
	if d.Intervention == nil || d.Intervention.Seq <= 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "intervention with positive seq is required")
	}
	if err := s.cfg.Store.SaveCommandDiagnostics(ctx, peer.NodeID, d); err != nil {
		return nil, err
	}
	return &pb.SaveCommandDiagnosticsResponse{}, nil
}

// GetCommandDiagnostics returns the diagnostics taken of a command as someone
// stopped or paused it by hand, oldest first, from the controller's own record.
// It outlives the command on its worker.
func (s *Server) GetCommandDiagnostics(ctx context.Context, req *pb.GetCommandDiagnosticsRequest) (*pb.GetCommandDiagnosticsResponse, error) {
	if strings.TrimSpace(req.Id) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "command ID is required")
	}
	if req.Seq < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "seq must be non-negative")
	}
	if req.AfterSeq < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "after_seq must be non-negative")
	}
	if req.Seq > 0 {
		d, ok, err := s.cfg.Store.CommandDiagnostic(ctx, req.Id, req.Seq)
		if err != nil {
			return nil, err
		}
		if ok {
			return &pb.GetCommandDiagnosticsResponse{Diagnostics: []*pb.CommandDiagnostics{d}}, nil
		}
		if err := s.ensureCommandOrDiagnosticsExist(ctx, req.Id); err != nil {
			return nil, err
		}
		return &pb.GetCommandDiagnosticsResponse{}, nil
	}
	ds, err := s.cfg.Store.CommandDiagnosticsAfter(ctx, req.Id, req.AfterSeq)
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		if err := s.ensureCommandOrDiagnosticsExist(ctx, req.Id); err != nil {
			return nil, err
		}
		return &pb.GetCommandDiagnosticsResponse{}, nil
	}
	var page []*pb.CommandDiagnostics
	var totalBytes int
	var hasMore bool
	for _, d := range ds {
		sz := proto.Size(d)
		if len(page) > 0 && totalBytes+sz > maxDiagnosticsResponseBytes {
			hasMore = true
			break
		}
		page = append(page, d)
		totalBytes += sz
	}
	res := &pb.GetCommandDiagnosticsResponse{
		Diagnostics: page,
		HasMore:     hasMore,
	}
	if hasMore && len(page) > 0 {
		res.NextAfterSeq = page[len(page)-1].GetIntervention().GetSeq()
	}
	return res, nil
}

func (s *Server) ensureCommandOrDiagnosticsExist(ctx context.Context, id string) error {
	hasAny, err := s.cfg.Store.HasCommandDiagnostics(ctx, id)
	if err != nil {
		return err
	}
	if hasAny {
		return nil
	}
	_, _, err = s.cfg.Store.Command(ctx, id)
	return err
}
