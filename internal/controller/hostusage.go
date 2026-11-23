package controller

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/httpx"
)

const (
	// hostUsageTTL is how long a host supervisor's ReportHostUsage reading
	// remains fresh before expiring. Matches worker heartbeat freshness window.
	hostUsageTTL = 45 * time.Second

	// maxHostClockSkew bounds how far ahead of the controller clock a host
	// supervisor's SampledAt timestamp may be.
	maxHostClockSkew = 10 * time.Second

	// Thresholds for evaluating host-side starvation on VM workers.
	hostMemPressureHoldAt = 50.0 // macOS warn=50, critical=100
	hostCPUBusyHoldAt     = 0.85 // 85% whole-host CPU busy
	hostLoadFactorHoldAt  = 1.25 // 1m load >= 1.25 * host CPUs
	hostStealGuestMin     = 0.50 // guest thinks it is >= 50% busy
	hostStealDiffHoldAt   = 0.35 // guest - vm >= 35% CPU steal

	// Placement cost thresholds for host CPU starvation penalty.
	hostStealCostGuestMin = 0.30
	hostStealCostDiffMin  = 0.10
	hostBusyCostStart     = 0.70
	hostLoadCostStart     = 1.00
)

type hostUsageEntry struct {
	usage      *pb.HostUsage
	receivedAt time.Time
	expiresAt  time.Time
}

type hostUsagePoint struct {
	at          int64
	expiresAt   int64
	vmCPUBusy   float64
	hostCPUBusy float64
}

type hostUsageState struct {
	mu      sync.Mutex
	latest  map[string]hostUsageEntry
	history map[string][]hostUsagePoint
}

func (h *hostUsageState) prune(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
}

func (h *hostUsageState) pruneLocked(now time.Time) {
	for host, e := range h.latest {
		if e.usage == nil || !now.Before(e.expiresAt) {
			delete(h.latest, host)
		}
	}
	cutoff := now.Add(-liveWindow).UnixMilli()
	for host, pts := range h.history {
		keep := 0
		for keep < len(pts) && pts[keep].expiresAt <= cutoff {
			keep++
		}
		if keep >= len(pts) {
			delete(h.history, host)
		} else if keep > 0 {
			h.history[host] = append([]hostUsagePoint(nil), pts[keep:]...)
		}
	}
}

// record stores u for host unless a newer reading (higher SampledAt) is
// already active. It returns true if the reading was recorded, or false if
// ignored as out of order or expired.
func (h *hostUsageState) record(host string, u *pb.HostUsage, effectiveAt, expiresAt, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	if prev, ok := h.latest[host]; ok && prev.usage != nil {
		if u.SampledAt < prev.usage.SampledAt {
			return false
		}
		if u.SampledAt == prev.usage.SampledAt && expiresAt.After(prev.expiresAt) {
			expiresAt = prev.expiresAt
		}
	}
	if !now.Before(expiresAt) {
		return false
	}
	if h.latest == nil {
		h.latest = map[string]hostUsageEntry{}
	}
	if h.history == nil {
		h.history = map[string][]hostUsagePoint{}
	}
	h.latest[host] = hostUsageEntry{
		usage:      u,
		receivedAt: now,
		expiresAt:  expiresAt,
	}
	h.appendHistoryLocked(host, hostUsagePoint{
		at:          effectiveAt.UnixMilli(),
		expiresAt:   expiresAt.UnixMilli(),
		vmCPUBusy:   u.VmCpuBusy,
		hostCPUBusy: u.HostCpuBusy,
	}, now)
	return true
}

func (h *hostUsageState) appendHistoryLocked(host string, pt hostUsagePoint, now time.Time) {
	pts := h.history[host]
	if n := len(pts); n > 0 {
		if pt.at < pts[n-1].at {
			return
		}
		if pts[n-1].at == pt.at {
			pts[n-1] = pt
		} else {
			pts = append(pts, pt)
		}
	} else {
		pts = append(pts, pt)
	}
	cutoff := now.Add(-liveWindow).UnixMilli()
	keep := 0
	for keep < len(pts) && pts[keep].expiresAt <= cutoff {
		keep++
	}
	if keep > 0 {
		pts = append([]hostUsagePoint(nil), pts[keep:]...)
	}
	if len(pts) == 0 {
		delete(h.history, host)
		return
	}
	h.history[host] = pts
}

func (h *hostUsageState) get(host string, now time.Time) *pb.HostUsage {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	e, ok := h.latest[host]
	if !ok || e.usage == nil || !now.Before(e.expiresAt) {
		return nil
	}
	cp := proto.Clone(e.usage).(*pb.HostUsage)
	cp.RemainingTtlMs = max(int64(1), e.expiresAt.Sub(now).Milliseconds())
	return cp
}

func (h *hostUsageState) snapshotHistory(host string, now time.Time) []hostUsagePoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	pts := h.history[host]
	if len(pts) == 0 {
		return nil
	}
	return slices.Clone(pts)
}

// ReportHostUsage stores the latest host-side CPU and memory pressure metrics
// reported by a VM worker's host supervisor.
func (s *Server) ReportHostUsage(ctx context.Context, req *pb.ReportHostUsageRequest) (*pb.ReportHostUsageResponse, error) {
	if req.GetNode() == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "node is required")
	}
	if req.GetUsage() == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "usage is required")
	}
	if _, err := s.nodeNamed(ctx, req.Node); err != nil {
		return nil, err
	}
	now := s.now()
	if err := s.power.authorizeHostReporter(req.Node, httpx.Peer(ctx), now); err != nil {
		return nil, err
	}
	u, effectiveAt, expiresAt, err := prepareHostUsage(req.Usage, now)
	if err != nil {
		return nil, err
	}
	s.hostUsages.record(req.Node, u, effectiveAt, expiresAt, now)
	return &pb.ReportHostUsageResponse{}, nil
}

// hostUsage returns a fresh clone of host's latest HostUsage with
// RemainingTtlMs populated from its original ingestion deadline, or nil if
// absent or expired.
func (s *Server) hostUsage(host string, now time.Time) *pb.HostUsage {
	return s.hostUsages.get(host, now)
}

func prepareHostUsage(in *pb.HostUsage, now time.Time) (*pb.HostUsage, time.Time, time.Time, error) {
	if in.SampledAt < 0 {
		return nil, time.Time{}, time.Time{}, api.Errorf(api.CodeInvalidArgs, "host usage sampled_at %d is negative", in.SampledAt)
	}
	var sampledAt time.Time
	if in.SampledAt > 0 {
		sampledAt = time.UnixMilli(in.SampledAt)
		if sampledAt.After(now.Add(maxHostClockSkew)) {
			return nil, time.Time{}, time.Time{}, api.Errorf(api.CodeInvalidArgs,
				"host usage sampled_at is %s in the future (max skew %s)",
				sampledAt.Sub(now).Round(time.Millisecond), maxHostClockSkew)
		}
		if !now.Before(sampledAt.Add(hostUsageTTL)) {
			return nil, time.Time{}, time.Time{}, api.Errorf(api.CodeInvalidArgs,
				"host usage sample is expired (age %s >= ttl %s)",
				now.Sub(sampledAt).Round(time.Millisecond), hostUsageTTL)
		}
	} else {
		sampledAt = now
	}
	effectiveAt := sampledAt
	if effectiveAt.After(now) {
		effectiveAt = now
	}
	expiresAt := effectiveAt.Add(hostUsageTTL)
	return sanitizeHostUsage(in, now), effectiveAt, expiresAt, nil
}

func sanitizeHostUsage(in *pb.HostUsage, now time.Time) *pb.HostUsage {
	out := &pb.HostUsage{
		VmCpuBusy:          clampFinite(in.VmCpuBusy, 0, 1),
		HostCpuBusy:        clampFinite(in.HostCpuBusy, 0, 1),
		HostLoad_1M:        clampFinite(in.HostLoad_1M, 0, math.MaxFloat64),
		HostCpus:           max(0, in.HostCpus),
		HostMemoryPressure: clampFinite(in.HostMemoryPressure, 0, 100),
		SampledAt:          in.SampledAt,
	}
	if out.SampledAt <= 0 {
		out.SampledAt = now.UnixMilli()
	}
	return out
}

func clampFinite(v, lo, hi float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// HostStarvation evaluates whether a VM worker is suffering host-side CPU
// starvation or host memory pressure, returning whether new work should be
// held, a human-readable reason, and the admission signals representing host
// truth.
func HostStarvation(guestUsage *pb.NodeUsage, hostUsage *pb.HostUsage) (holding bool, reason string, signals []*pb.AdmissionSignal) {
	if hostUsage == nil {
		return false, "", nil
	}
	guestBusy := clampFinite(guestUsage.GetCpuBusy(), 0, 1)
	vmBusy := clampFinite(hostUsage.GetVmCpuBusy(), 0, 1)
	hostBusy := clampFinite(hostUsage.GetHostCpuBusy(), 0, 1)
	load1m := clampFinite(hostUsage.GetHostLoad_1M(), 0, math.MaxFloat64)
	hostCPUs := hostUsage.GetHostCpus()
	memPressure := clampFinite(hostUsage.GetHostMemoryPressure(), 0, 100)

	steal := max(0.0, guestBusy-vmBusy)
	highHostCPU := hostBusy >= hostCPUBusyHoldAt
	highHostLoad := hostCPUs > 0 && load1m >= float64(hostCPUs)*hostLoadFactorHoldAt
	cpuSteal := guestBusy >= hostStealGuestMin && steal >= hostStealDiffHoldAt
	cpuHolding := highHostCPU || highHostLoad || cpuSteal

	cpuSig := &pb.AdmissionSignal{
		Name:    "host-cpu-starvation",
		Unit:    "%",
		Holding: cpuHolding,
	}
	var cpuReason string
	switch {
	case cpuSteal:
		cpuSig.Value = steal * 100
		cpuSig.Limit = hostStealDiffHoldAt * 100
		cpuSig.Detail = fmt.Sprintf("guest %.0f%% vs VM %.0f%% on host (%.0f%% stolen); host %.0f%% busy, load %.1f/%d",
			guestBusy*100, vmBusy*100, steal*100, hostBusy*100, load1m, hostCPUs)
		cpuReason = fmt.Sprintf("host CPU starvation: guest %.0f%% vs VM %.0f%% on host (%.0f%% stolen)",
			guestBusy*100, vmBusy*100, steal*100)
	case highHostCPU:
		cpuSig.Value = hostBusy * 100
		cpuSig.Limit = hostCPUBusyHoldAt * 100
		cpuSig.Detail = fmt.Sprintf("host %.0f%% busy, load %.1f/%d (VM %.0f%%)",
			hostBusy*100, load1m, hostCPUs, vmBusy*100)
		cpuReason = fmt.Sprintf("host CPU busy %.0f%% (limit %.0f%%)", hostBusy*100, hostCPUBusyHoldAt*100)
	case highHostLoad:
		loadPct := (load1m / float64(hostCPUs)) * 100
		cpuSig.Value = loadPct
		cpuSig.Limit = hostLoadFactorHoldAt * 100
		cpuSig.Detail = fmt.Sprintf("host load %.1f/%d (host %.0f%% busy, VM %.0f%%)",
			load1m, hostCPUs, hostBusy*100, vmBusy*100)
		cpuReason = fmt.Sprintf("host load %.1f on %d CPUs (limit %.1f)",
			load1m, hostCPUs, float64(hostCPUs)*hostLoadFactorHoldAt)
	default:
		cpuSig.Value = hostBusy * 100
		cpuSig.Limit = hostCPUBusyHoldAt * 100
		if hostCPUs > 0 {
			cpuSig.Detail = fmt.Sprintf("VM %.0f%% on host; host %.0f%% busy, load %.1f/%d",
				vmBusy*100, hostBusy*100, load1m, hostCPUs)
		} else {
			cpuSig.Detail = fmt.Sprintf("VM %.0f%% on host; host %.0f%% busy", vmBusy*100, hostBusy*100)
		}
	}

	memHolding := memPressure >= hostMemPressureHoldAt
	memDetail := "macOS memory pressure normal"
	switch {
	case memPressure >= 100:
		memDetail = "macOS memory pressure critical"
	case memPressure >= hostMemPressureHoldAt:
		memDetail = "macOS memory pressure warn"
	}
	memSig := &pb.AdmissionSignal{
		Name:    "host-memory-pressure",
		Value:   memPressure,
		Limit:   hostMemPressureHoldAt,
		Unit:    "%",
		Holding: memHolding,
		Detail:  memDetail,
	}

	signals = []*pb.AdmissionSignal{cpuSig, memSig}
	holding = cpuHolding || memHolding
	switch {
	case cpuHolding && memHolding:
		reason = fmt.Sprintf("%s; host memory pressure %.0f%%", cpuReason, memPressure)
	case cpuHolding:
		reason = cpuReason
	case memHolding:
		reason = fmt.Sprintf("host memory pressure %.0f%% (limit %.0f%%)", memPressure, hostMemPressureHoldAt)
	}
	return holding, reason, signals
}

// mergeHostAdmission merges controller-computed host starvation signals into a
// worker's Admission so Cockpit, CLI, and placement see host holds immediately,
// even before the worker's next heartbeat reflects them.
func mergeHostAdmission(existing *pb.Admission, guestUsage *pb.NodeUsage, hostUsage *pb.HostUsage, now time.Time) *pb.Admission {
	if hostUsage == nil {
		return existing
	}
	holding, reason, hostSignals := HostStarvation(guestUsage, hostUsage)
	if existing == nil && !holding {
		return nil
	}
	var out *pb.Admission
	if existing != nil {
		out = proto.Clone(existing).(*pb.Admission)
	} else {
		out = &pb.Admission{}
	}
	for _, hs := range hostSignals {
		idx := slices.IndexFunc(out.Signals, func(s *pb.AdmissionSignal) bool { return s.Name == hs.Name })
		if idx < 0 {
			out.Signals = append(out.Signals, hs)
		} else if hs.Holding && !out.Signals[idx].Holding {
			// Fresh host starvation takes effect immediately while preserving
			// any worker-side waiting count.
			hs.Waiting = out.Signals[idx].Waiting
			out.Signals[idx] = hs
		}
	}
	if holding {
		out.Holding = true
		if out.Reason == "" {
			out.Reason = reason
		}
		if out.Since == 0 {
			if hostUsage.SampledAt > 0 {
				out.Since = hostUsage.SampledAt
			} else if !now.IsZero() {
				out.Since = now.UnixMilli()
			}
		}
	}
	return out
}

// hostStarvationCost computes an additive placement cost part (in units of a
// whole node's CPUs) reflecting host CPU steal or host saturation on a VM node.
func hostStarvationCost(guestBusy float64, hostUsage *pb.HostUsage) (float64, string) {
	if hostUsage == nil {
		return 0, ""
	}
	guestBusy = clampFinite(guestBusy, 0, 1)
	vmBusy := clampFinite(hostUsage.GetVmCpuBusy(), 0, 1)
	hostBusy := clampFinite(hostUsage.GetHostCpuBusy(), 0, 1)
	load1m := clampFinite(hostUsage.GetHostLoad_1M(), 0, math.MaxFloat64)
	hostCPUs := hostUsage.GetHostCpus()

	var stealPenalty, hostPenalty, loadPenalty float64
	steal := max(0.0, guestBusy-vmBusy)
	if guestBusy >= hostStealCostGuestMin && steal > hostStealCostDiffMin {
		stealPenalty = steal
	}
	if hostBusy > hostBusyCostStart {
		hostPenalty = min(1.0, (hostBusy-hostBusyCostStart)/(1.0-hostBusyCostStart))
	}
	if hostCPUs > 0 {
		loadFrac := load1m / float64(hostCPUs)
		if loadFrac > hostLoadCostStart {
			loadPenalty = min(1.0, (loadFrac-hostLoadCostStart)/0.5)
		}
	}
	v := max(stealPenalty, max(hostPenalty, loadPenalty))
	if v <= 0 {
		return 0, ""
	}
	if hostCPUs > 0 {
		return v, fmt.Sprintf("VM %.0f%% vs guest %.0f%%, host %.0f%% busy, load %.1f/%d",
			vmBusy*100, guestBusy*100, hostBusy*100, load1m, hostCPUs)
	}
	return v, fmt.Sprintf("VM %.0f%% vs guest %.0f%%, host %.0f%% busy",
		vmBusy*100, guestBusy*100, hostBusy*100)
}

// overlayHostSamples returns a map of node series with fresh host-side
// VmCpuBusy and HostCpuBusy readings overlaid onto VM nodes' samples, without
// mutating any cached Samples slices in place.
func (s *Server) overlayHostSamples(ser map[string]*pb.Samples, nvs []nodeView, now time.Time) map[string]*pb.Samples {
	if len(ser) == 0 || len(nvs) == 0 {
		return ser
	}
	var out map[string]*pb.Samples
	for _, nv := range nvs {
		nodeID := nv.node.Id
		src := ser[nodeID]
		if src == nil || len(src.Samples) == 0 {
			continue
		}
		pts := s.hostUsages.snapshotHistory(nv.node.Hostname, now)
		if len(pts) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]*pb.Samples, len(ser))
			for k, v := range ser {
				out[k] = v
			}
		}
		cloned := &pb.Samples{Samples: make([]*pb.Sample, len(src.Samples))}
		j := 0
		for i, sp := range src.Samples {
			cp := proto.Clone(sp).(*pb.Sample)
			for j+1 < len(pts) && pts[j+1].at <= cp.T {
				j++
			}
			if pts[j].at <= cp.T && cp.T < pts[j].expiresAt {
				cp.VmCpuBusy = proto.Float64(pts[j].vmCPUBusy)
				cp.HostCpuBusy = proto.Float64(pts[j].hostCPUBusy)
			}
			cloned.Samples[i] = cp
		}
		out[nodeID] = cloned
	}
	if out == nil {
		return ser
	}
	return out
}
