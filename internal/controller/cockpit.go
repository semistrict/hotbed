package controller

import (
	"context"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const (
	liveWindow    = 15 * time.Minute
	sampleSeconds = 5
	eventsShown   = 80
)

// GetSnapshot is the cockpit's whole view of the cluster.
func (s *Server) GetSnapshot(ctx context.Context, _ *pb.GetSnapshotRequest) (*pb.Snapshot, error) {
	now := s.now()
	day := now.Add(-24 * time.Hour)
	rows, err := s.cfg.Store.Nodes(ctx)
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
	samples, err := s.cfg.Store.Samples(ctx, day)
	if err != nil {
		return nil, err
	}
	forks, err := s.cfg.Store.Forks(ctx, day)
	if err != nil {
		return nil, err
	}
	sessions, err := s.cfg.Store.RecentSessions(ctx, day)
	if err != nil {
		return nil, err
	}
	schedules, err := s.allSchedules(ctx)
	if err != nil {
		return nil, err
	}

	snap := &pb.Snapshot{
		ClusterId: s.cfg.Cluster.ClusterID, Controller: s.cfg.Controller,
		Now: now.UnixMilli(), SampleSeconds: sampleSeconds, Events: events, History: history,
		Sessions: sessions, Schedules: schedules,
	}
	for _, n := range rows {
		node := s.withOnline(n.node)
		if !node.Online {
			node.Usage = &pb.NodeUsage{}
		}
		snap.Nodes = append(snap.Nodes, node)
		if r := n.report; r != nil {
			snap.Bases = append(snap.Bases, r.Bases...)
			snap.Workspaces = append(snap.Workspaces, r.Workspaces...)
			if node.Online {
				snap.Commands = append(snap.Commands, r.Commands...)
			}
		}
	}
	for _, repo := range repos {
		snap.Repos = append(snap.Repos, summarize(repo, snap.Bases))
	}
	snap.Series, snap.Load, snap.NodeHourly = series(samples, now)
	snap.Hourly = hourly(history, forks, now)
	snap.Totals24H = totals(history, forks, samples)
	return snap, nil
}

// summarize describes a repo by its newest cold base: what it is written
// in, how many units it builds, how long that takes from nothing.
func summarize(repo *pb.Repo, bases []*pb.Base) *pb.RepoSummary {
	out := &pb.RepoSummary{Id: repo.Id, Name: strings.TrimSuffix(nameOf(repo.Url), ".git")}
	var cold *pb.Base
	for _, b := range bases {
		if b.RepoId == repo.Id && b.Cold && (b.State == pb.BaseState_BASE_STATE_WARM || b.State == pb.BaseState_BASE_STATE_STALE) {
			if cold == nil || b.Generation > cold.Generation {
				cold = b
			}
		}
	}
	if cold != nil {
		out.Units, out.ColdBuildSeconds = cold.Units, cold.BuildSeconds
		switch {
		case strings.HasPrefix(cold.Toolchain, "rust"):
			out.Language = "rust"
		case strings.HasPrefix(cold.Toolchain, "go"):
			out.Language = "go"
		case strings.HasPrefix(cold.Toolchain, "node"):
			out.Language = "typescript"
		}
	}
	if out.Language == "" {
		var scripts []string
		for _, s := range repo.Steps {
			scripts = append(scripts, s.Script)
		}
		all := strings.Join(scripts, " ")
		switch {
		case strings.Contains(all, "cargo"):
			out.Language = "rust"
		case strings.Contains(all, "go "):
			out.Language = "go"
		default:
			out.Language = "typescript"
		}
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
	load := map[int64]*pb.LoadSample{}
	type acc struct {
		cpu, mem float64
		n        int
	}
	hours := map[string]map[int64]*acc{}
	since := now.Add(-liveWindow).UnixMilli()
	for _, s := range samples {
		if s.t >= since {
			if live[s.node] == nil {
				live[s.node] = &pb.Samples{}
			}
			live[s.node].Samples = append(live[s.node].Samples, &pb.Sample{T: s.t, CpuBusy: s.cpu, MemoryUsed: s.memory, MemoryPressure: s.psi})
			b := s.t / (sampleSeconds * 1000) * (sampleSeconds * 1000)
			l, ok := load[b]
			if !ok {
				l = &pb.LoadSample{T: b}
				load[b] = l
			}
			l.Running += int32(s.running)
			l.Queued += int32(s.queued)
			l.Parked += int32(s.parked)
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
		a.n++
	}
	loads := make([]*pb.LoadSample, 0, len(load))
	for _, l := range load {
		loads = append(loads, l)
	}
	sort.Slice(loads, func(i, j int) bool { return loads[i].T < loads[j].T })
	nodeHourly := map[string]*pb.NodeHours{}
	for node, hs := range hours {
		out := &pb.NodeHours{}
		for _, h := range dayHours(now) {
			nh := &pb.NodeHour{Hour: h}
			if a := hs[h]; a != nil {
				nh.CpuBusy, nh.MemoryUsed = a.cpu/float64(a.n), a.mem/float64(a.n)
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

func totals(history []*pb.FinishedCommand, forks []forkRow, samples []sampleRow) *pb.Totals {
	t := &pb.Totals{}
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
	for n, l := range last {
		t.OomKills += int32(l - first[n])
	}
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
