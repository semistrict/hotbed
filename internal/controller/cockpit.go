package controller

import (
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/httpx"
)

const (
	liveWindow    = 15 * time.Minute
	sampleSeconds = 5
	eventsShown   = 80
)

// snapshot is the cockpit's whole view of the cluster, in the shapes of
// ui/src/api/types.ts.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	now := s.cfg.Now()
	day := now.Add(-24 * time.Hour)
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return err
	}
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return err
	}
	history, err := s.cfg.Store.History(ctx, day)
	if err != nil {
		return err
	}
	events, err := s.cfg.Store.Events(ctx, eventsShown)
	if err != nil {
		return err
	}
	samples, err := s.cfg.Store.Samples(ctx, day)
	if err != nil {
		return err
	}
	forks, err := s.cfg.Store.Forks(ctx, day)
	if err != nil {
		return err
	}

	snap := api.Snapshot{
		ClusterID: s.cfg.Cluster.ClusterID, Controller: s.cfg.Controller,
		Now: now.UTC().Format(time.RFC3339Nano), SampleSeconds: sampleSeconds,
		Nodes: []api.CockpitNode{}, Repos: []api.CockpitRepo{}, Bases: []api.Base{},
		Workspaces: []api.Workspace{}, Commands: []api.Command{}, Events: events,
		Series: map[string][]api.Sample{}, Load: []api.LoadSample{},
		NodeHourly: map[string][]api.NodeHour{}, History: history,
	}
	for _, n := range rows {
		online := s.withOnline(n.Node).Online
		cn := api.CockpitNode{ID: n.ID, Hostname: n.Hostname, Online: online, JoinedAt: n.JoinedAt.Format(time.RFC3339Nano)}
		if n.LastSeen != nil {
			cn.LastSeen = n.LastSeen.Format(time.RFC3339Nano)
		}
		if rep := n.Report; rep != nil {
			cn.Arch, cn.Machine, cn.Kernel = rep.Resources.Arch, rep.Resources.Machine, rep.Resources.Kernel
			cn.CPUs, cn.MemoryBytes, cn.SwapBytes = rep.Resources.CPUs, rep.Resources.MemoryBytes, rep.Resources.SwapBytes
			cn.DiskBytes, cn.Disk, cn.ReservedMemoryBytes = rep.DiskBytes, rep.Disk, rep.Reserved
			if online {
				cn.Usage = rep.Usage
			}
			snap.Bases = append(snap.Bases, rep.Bases...)
			snap.Workspaces = append(snap.Workspaces, rep.Workspaces...)
			if online {
				snap.Commands = append(snap.Commands, rep.Commands...)
			}
		}
		snap.Nodes = append(snap.Nodes, cn)
	}
	for _, repo := range repos {
		snap.Repos = append(snap.Repos, cockpitRepo(repo, snap.Bases, rows))
	}
	snap.Series, snap.Load, snap.NodeHourly = series(samples, now)
	snap.Hourly = hourly(history, forks, samples, now)
	snap.Totals24h = totals(history, forks, samples)
	return httpx.JSON(w, snap)
}

// cockpitRepo describes a repo by its newest cold base: what it is written
// in, how many units it builds, how long that takes from nothing.
func cockpitRepo(repo api.Repo, bases []api.Base, rows []nodeRow) api.CockpitRepo {
	out := api.CockpitRepo{ID: repo.ID, Name: strings.TrimSuffix(nameOf(repo.URL), ".git")}
	var cold *api.Base
	for i, b := range bases {
		if b.RepoID == repo.ID && b.Cold && (b.State == api.BaseWarm || b.State == api.BaseStale) {
			if cold == nil || b.Generation > cold.Generation {
				cold = &bases[i]
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
		}
	}
	if out.Language == "" {
		out.Language = guessLanguage(repo.Freshen)
	}
	return out
}

func nameOf(u string) string {
	u = strings.TrimSuffix(u, "/")
	parts := strings.FieldsFunc(u, func(r rune) bool { return r == '/' || r == ':' })
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return filepath.Base(u)
}

func guessLanguage(freshen []string) string {
	all := strings.Join(freshen, " ")
	switch {
	case strings.Contains(all, "cargo"):
		return "rust"
	case strings.Contains(all, "go "):
		return "go"
	default:
		return "typescript"
	}
}

// series turns samples into the cockpit's live charts (the last 15 minutes
// per node, and the cluster's command counts) and its 24-hour heatmap.
func series(samples []sampleRow, now time.Time) (map[string][]api.Sample, []api.LoadSample, map[string][]api.NodeHour) {
	live := map[string][]api.Sample{}
	load := map[int64]*api.LoadSample{}
	type acc struct {
		cpu, mem float64
		n        int
	}
	hours := map[string]map[int64]*acc{}
	since := now.Add(-liveWindow).UnixMilli()
	for _, s := range samples {
		if s.t >= since {
			live[s.node] = append(live[s.node], api.Sample{T: s.t, CPUBusy: s.cpu, MemoryUsed: s.memory, MemoryPressure: s.psi})
			b := s.t / (sampleSeconds * 1000) * (sampleSeconds * 1000)
			l, ok := load[b]
			if !ok {
				l = &api.LoadSample{T: b}
				load[b] = l
			}
			l.Running += s.running
			l.Queued += s.queued
			l.Parked += s.parked
		}
		h := hourOf(s.t)
		if hours[s.node] == nil {
			hours[s.node] = map[int64]*acc{}
		}
		a := hours[s.node][h]
		if a == nil {
			a = &acc{}
			hours[s.node][h] = a
		}
		a.cpu += s.cpu
		a.mem += s.memory
		a.n++
	}
	loads := make([]api.LoadSample, 0, len(load))
	for _, l := range load {
		loads = append(loads, *l)
	}
	sort.Slice(loads, func(i, j int) bool { return loads[i].T < loads[j].T })
	nodeHourly := map[string][]api.NodeHour{}
	for node, hs := range hours {
		for _, h := range dayHours(now) {
			nh := api.NodeHour{Hour: h}
			if a := hs[h]; a != nil {
				nh.CPUBusy, nh.MemoryUsed = a.cpu/float64(a.n), a.mem/float64(a.n)
			}
			nodeHourly[node] = append(nodeHourly[node], nh)
		}
	}
	return live, loads, nodeHourly
}

// hourly is the last 24 hours, one bucket per hour, from history and forks.
func hourly(history []api.FinishedCommand, forks []forkRow, samples []sampleRow, now time.Time) []api.HourBucket {
	idx := map[int64]int{}
	out := []api.HourBucket{}
	for i, h := range dayHours(now) {
		idx[h] = i
		out = append(out, api.HourBucket{Hour: h})
	}
	waits := make([][]float64, len(out))
	for _, c := range history {
		i, ok := idx[hourOf(c.EndedAt)]
		if !ok {
			continue
		}
		b := &out[i]
		waits[i] = append(waits[i], c.QueuedSeconds)
		b.Parks += c.Parks
		if c.ColdSeconds == nil || c.State == api.CommandCancelled {
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

func totals(history []api.FinishedCommand, forks []forkRow, samples []sampleRow) api.Totals {
	var t api.Totals
	for _, c := range history {
		t.Parks += c.Parks
		if c.ColdSeconds == nil || c.State == api.CommandCancelled {
			continue
		}
		t.Builds++
		t.SavedSeconds += math.Max(0, *c.ColdSeconds-c.Seconds)
	}
	t.Forks = len(forks)
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
			first[s.node] -= last[s.node] // rebooted: count what came before
		}
		last[s.node] = s.oom
	}
	for n, l := range last {
		t.OOMKills += int(l - first[n])
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
