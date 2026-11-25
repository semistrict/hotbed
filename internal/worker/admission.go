package worker

import (
	"fmt"
	"log/slog"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// CPU pressure (PSI some avg10, %) past which more work makes all of it
// slower: the node holds new work once pressure reaches cpuHoldAt, and
// starts it again only once it is under cpuResumeAt and has not reached
// cpuHoldAt for cpuWindow. Measurements, on CPUs of their own, are not held.
const (
	cpuHoldAt   = 60.0
	cpuResumeAt = 30.0
	cpuWindow   = 30 * time.Second

	// hostUsageTTL is how long a host-side usage sample from the controller's
	// heartbeat reply stays valid before it is treated as stale and ignored
	// (board 9281).
	hostUsageTTL = 45 * time.Second

	// Host-side starvation thresholds from the VM host supervisor (board 9281).
	// On macOS, memory pressure is discrete: normal=0, warn=50, critical=100.
	// macOS routinely sits at warn (level 2) with ample free RAM; hard-holding
	// at 50% blacks out vm1 for 45-50m stretches and dumps all load onto other
	// arm64 nodes. Hard-hold only on critical (100%); warn is handled as a soft
	// placement penalty in the controller.
	hostMemPressureHoldAt  = 100.0
	hostCPUBusyHoldAt      = 0.85 // host CPU utilization >= 85%
	hostCPUBusyResumeAt    = 0.70 // resume once host CPU drops under 70%
	hostLoadPerCPUHoldAt   = 1.25 // host 1m load >= 1.25 * host_cpus
	hostLoadPerCPUResumeAt = 1.00 // resume once host 1m load < 1.0 * host_cpus
	hostStealGuestMin      = 0.50 // guest CPU busy >= 50% when checking steal
	hostStealDeltaHoldAt   = 0.35 // guest_cpu_busy - vm_cpu_busy >= 35% steal
	hostStealDeltaResumeAt = 0.20 // resume once steal drops under 20%
)

// pressureAt is one CPU pressure reading.
type pressureAt struct {
	at time.Time
	v  float64
}

// cpuGate holds new work while the CPUs are saturated.
type cpuGate struct {
	holding bool
	// recent is the readings of the last cpuWindow, oldest first.
	recent []pressureAt
}

// peak is the highest reading of the last cpuWindow.
func (g *cpuGate) peak() float64 {
	var p float64
	for _, r := range g.recent {
		p = max(p, r.v)
	}
	return p
}

// weigh takes a reading and says whether the gate now holds new work.
func (g *cpuGate) weigh(now time.Time, v float64) bool {
	g.recent = append(g.recent, pressureAt{now, v})
	for len(g.recent) > 0 && now.Sub(g.recent[0].at) > cpuWindow {
		g.recent = g.recent[1:]
	}
	switch {
	case v >= cpuHoldAt:
		g.holding = true
	case g.holding && v < cpuResumeAt && g.peak() < cpuHoldAt:
		g.holding = false
	}
	return g.holding
}

// hold is why the node holds new work, and since when; zero when it
// does not.
type hold struct {
	kind, reason string
	since        time.Time
}

// SetHostUsage stores the latest host-side usage reported by the controller's
// heartbeat reply, re-evaluates admission holds, and wakes admission when a
// hold clears. Passing nil or an already-expired sample clears stored host
// usage. When RemainingTtlMs is populated (> 0), the sample expires at that
// remaining deadline (capped at hostUsageTTL) rather than extending by a full
// hostUsageTTL on each heartbeat.
func (e *Engine) SetHostUsage(u *pb.HostUsage) {
	e.mu.Lock()
	now := e.now()
	had := e.hostUsage != nil
	if u == nil || u.GetRemainingTtlMs() < 0 {
		e.hostUsage = nil
		e.hostUsageUntil = time.Time{}
	} else {
		ttl := hostUsageTTL
		if rem := u.GetRemainingTtlMs(); rem > 0 {
			ttl = min(time.Duration(rem)*time.Millisecond, hostUsageTTL)
		}
		e.hostUsage = clone(u)
		e.hostUsageUntil = now.Add(ttl)
	}
	wasHeld := e.held.kind != ""
	e.weighHold(now)
	cleared := wasHeld && e.held.kind == ""
	e.mu.Unlock()
	if cleared {
		e.stepSoon()
	}
	if had || u != nil {
		e.changed()
	}
}

// freshHostUsage returns the latest host-side usage if its freshness deadline
// has not passed, or nil if absent or stale. Call with e.mu held.
func (e *Engine) freshHostUsage(now time.Time) *pb.HostUsage {
	if e.hostUsage == nil || e.hostUsageUntil.IsZero() || now.After(e.hostUsageUntil) {
		return nil
	}
	return e.hostUsage
}

// hostMemoryStarved reports whether the VM's host is under memory pressure.
func hostMemoryStarved(hu *pb.HostUsage) (bool, string) {
	if hu == nil || hu.GetHostMemoryPressure() < hostMemPressureHoldAt {
		return false, ""
	}
	return true, fmt.Sprintf("host memory pressure %.0f%%; new work starts again under %.0f%%", hu.GetHostMemoryPressure(), hostMemPressureHoldAt)
}

// weighHostCPU updates e.hostCPUHolding with hysteresis from the current fresh
// host usage and guest CPU busy reading. Call with e.mu held.
func (e *Engine) weighHostCPU(now time.Time) {
	hu := e.freshHostUsage(now)
	if hu == nil {
		e.hostCPUHolding = false
		return
	}
	hostBusy := hu.GetHostCpuBusy()
	vmBusy := hu.GetVmCpuBusy()
	guestBusy := e.usage.GetCpuBusy()
	steal := max(0.0, guestBusy-vmBusy)
	hasCpus := hu.GetHostCpus() > 0
	loadHigh := hasCpus && hu.GetHostLoad_1M() >= float64(hu.GetHostCpus())*hostLoadPerCPUHoldAt
	busyHigh := hostBusy >= hostCPUBusyHoldAt
	stealHigh := guestBusy >= hostStealGuestMin && steal >= hostStealDeltaHoldAt

	if stealHigh || busyHigh || loadHigh {
		e.hostCPUHolding = true
		return
	}
	if e.hostCPUHolding {
		loadOK := !hasCpus || hu.GetHostLoad_1M() < float64(hu.GetHostCpus())*hostLoadPerCPUResumeAt
		busyOK := hostBusy < hostCPUBusyResumeAt
		stealOK := guestBusy < hostStealGuestMin || steal < hostStealDeltaResumeAt
		if loadOK && busyOK && stealOK {
			e.hostCPUHolding = false
		}
	}
}

// hostCPUStatus evaluates host CPU starvation for hu (which must be fresh, or
// nil when absent/stale). Call with e.mu held.
func (e *Engine) hostCPUStatus(hu *pb.HostUsage) (holding bool, value, limit, resume float64, reason, detail string) {
	if hu == nil {
		return false, 0, hostCPUBusyHoldAt * 100, hostCPUBusyResumeAt * 100, "", ""
	}
	hostBusy := hu.GetHostCpuBusy()
	vmBusy := hu.GetVmCpuBusy()
	guestBusy := e.usage.GetCpuBusy()
	steal := max(0.0, guestBusy-vmBusy)
	hasCpus := hu.GetHostCpus() > 0
	loadHigh := hasCpus && hu.GetHostLoad_1M() >= float64(hu.GetHostCpus())*hostLoadPerCPUHoldAt
	busyHigh := hostBusy >= hostCPUBusyHoldAt
	stealHigh := guestBusy >= hostStealGuestMin && steal >= hostStealDeltaHoldAt

	detail = fmt.Sprintf("host CPU %.0f%%, VM got %.0f%%, guest reports %.0f%%", hostBusy*100, vmBusy*100, guestBusy*100)
	if hasCpus {
		detail += fmt.Sprintf(", host load %.1f/%d", hu.GetHostLoad_1M(), hu.GetHostCpus())
	}

	switch {
	case stealHigh:
		return true, steal * 100, hostStealDeltaHoldAt * 100, hostStealDeltaResumeAt * 100,
			fmt.Sprintf("host CPU starvation: guest reports %.0f%% CPU busy while VM gets %.0f%% (%.0f%% steal, host %.0f%% busy)",
				guestBusy*100, vmBusy*100, steal*100, hostBusy*100),
			detail
	case busyHigh:
		return true, hostBusy * 100, hostCPUBusyHoldAt * 100, hostCPUBusyResumeAt * 100,
			fmt.Sprintf("host CPU starvation: host CPU %.0f%% busy; new work starts again under %.0f%%",
				hostBusy*100, hostCPUBusyResumeAt*100),
			detail
	case loadHigh:
		loadPct := (hu.GetHostLoad_1M() / float64(hu.GetHostCpus())) * 100
		return true, loadPct, hostLoadPerCPUHoldAt * 100, hostLoadPerCPUResumeAt * 100,
			fmt.Sprintf("host CPU starvation: host load %.1f across %d CPUs; new work starts again under %.0f%%",
				hu.GetHostLoad_1M(), hu.GetHostCpus(), hostLoadPerCPUResumeAt*100),
			detail
	case e.hostCPUHolding:
		if guestBusy >= hostStealGuestMin && steal >= hostStealDeltaResumeAt {
			return true, steal * 100, hostStealDeltaHoldAt * 100, hostStealDeltaResumeAt * 100,
				fmt.Sprintf("host CPU starvation: %.0f%% steal (guest %.0f%%, VM %.0f%%); new work starts again under %.0f%%",
					steal*100, guestBusy*100, vmBusy*100, hostStealDeltaResumeAt*100),
				detail
		}
		if hasCpus && hu.GetHostLoad_1M() >= float64(hu.GetHostCpus())*hostLoadPerCPUResumeAt {
			loadPct := (hu.GetHostLoad_1M() / float64(hu.GetHostCpus())) * 100
			return true, loadPct, hostLoadPerCPUHoldAt * 100, hostLoadPerCPUResumeAt * 100,
				fmt.Sprintf("host CPU starvation: host load %.1f across %d CPUs; new work starts again under %.0f%%",
					hu.GetHostLoad_1M(), hu.GetHostCpus(), hostLoadPerCPUResumeAt*100),
				detail
		}
		return true, hostBusy * 100, hostCPUBusyHoldAt * 100, hostCPUBusyResumeAt * 100,
			fmt.Sprintf("host CPU starvation: host CPU %.0f%% busy; new work starts again under %.0f%%",
				hostBusy*100, hostCPUBusyResumeAt*100),
			detail
	default:
		return false, hostBusy * 100, hostCPUBusyHoldAt * 100, hostCPUBusyResumeAt * 100, "", detail
	}
}

// isHostStarved reports whether fresh host-side metrics indicate host CPU or
// memory starvation. Call with e.mu held.
func (e *Engine) isHostStarved(now time.Time) bool {
	hu := e.freshHostUsage(now)
	if hu == nil {
		return false
	}
	if memHeld, _ := hostMemoryStarved(hu); memHeld {
		return true
	}
	cpuHeld, _, _, _, _, _ := e.hostCPUStatus(hu)
	return cpuHeld
}

// weighMemPressure updates e.memPressureHolding with hysteresis: holds once
// memory PSI (some avg10) reaches admitPressure and clears once it drops below
// memPressureResumeAt. Call with e.mu held.
func (e *Engine) weighMemPressure(p float64) bool {
	switch {
	case p >= admitPressure:
		e.memPressureHolding = true
	case e.memPressureHolding && p < memPressureResumeAt:
		e.memPressureHolding = false
	}
	return e.memPressureHolding
}

// cladefsMemoryStarved reports whether cladefsd's memory governor says the
// host is full (cache squeezed to its floor, refusing new cells) or actively
// evicting at/near its floor (within cladefsd's 25% hysteresis band).
// Call with e.mu held.
func (e *Engine) cladefsMemoryStarved() (bool, string) {
	os := e.objectStore
	if os == nil {
		return false, ""
	}
	floor := os.GetExtentCacheFloorBytes()
	limit := os.GetExtentCacheLimitBytes()
	used := os.GetExtentCacheBytes()
	if os.GetMemoryFull() {
		if floor > 0 {
			return true, fmt.Sprintf("cladefsd host memory full (DAX cache %s at %s floor); new work waits for cache headroom", gib(int64(used)), gib(int64(floor)))
		}
		return true, "cladefsd host memory full (DAX cache at floor); new work waits for cache headroom"
	}
	if os.GetMemoryPressure() && floor > 0 && limit > 0 && limit <= floor+floor/4 {
		return true, fmt.Sprintf("cladefsd memory pressure near floor (DAX cache limit %s, floor %s); new work waits for cache headroom", gib(int64(limit)), gib(int64(floor)))
	}
	return false, ""
}

// nodeHold is what holds every new command on the node now, whatever it
// would need, or "": host starvation, cladefsd memory starvation, saturated
// CPUs, memory pressure or a full disk. Call with e.mu held.
func (e *Engine) nodeHold(now time.Time) (kind, reason string) {
	hu := e.freshHostUsage(now)
	if memHeld, memReason := hostMemoryStarved(hu); memHeld {
		return "host-memory", memReason
	}
	if cpuHeld, _, _, _, cpuReason, _ := e.hostCPUStatus(hu); cpuHeld {
		return "host-cpu", cpuReason
	}
	if cladefsHeld, cladefsReason := e.cladefsMemoryStarved(); cladefsHeld {
		return "cladefs-memory", cladefsReason
	}
	switch u := e.usage; {
	case e.cpu.holding:
		return "cpu", fmt.Sprintf("CPU pressure %.0f%% (peak %.0f%% in the last %s); new work starts again under %.0f%%", u.GetCpuPressure(), e.cpu.peak(), cpuWindow, cpuResumeAt)
	case e.memPressureHolding || u.GetMemoryPressure() >= admitPressure:
		return "pressure", fmt.Sprintf("memory pressure %.0f%%; new work starts again under %.0f%%", u.GetMemoryPressure(), memPressureResumeAt)
	case e.diskLow():
		return "disk", fmt.Sprintf("%s of disk free; new work starts again once %s is", gib(e.disk.FreeBytes), gib(e.minFreeBytes()))
	}
	return "", ""
}

func (e *Engine) minFreeBytes() int64 { return int64(float64(e.diskBytes) * minFreeDisk) }

func (e *Engine) diskLow() bool {
	return e.diskBytes > 0 && e.disk.GetFreeBytes() < e.minFreeBytes()
}

// weighHold notes whether the node holds new work after a sample or host usage
// update, and says so in the events when that changes. Call with e.mu held.
func (e *Engine) weighHold(now time.Time) {
	e.weighHostCPU(now)
	kind, reason := e.nodeHold(now)
	was := e.held
	switch {
	case kind == was.kind:
		e.held.reason = reason // the numbers move; the hold does not
		return
	case kind == "":
		e.held = hold{}
		e.note(feed(feedUnhold, fmt.Sprintf("start new work again after holding it %s (%s)", now.Sub(was.since).Round(time.Second), was.kind), ref("node", e.cfg.NodeID)))
	default:
		e.held = hold{kind: kind, reason: reason, since: now}
		e.note(feed(feedHold, "hold new work: "+reason, ref("node", e.cfg.NodeID)))
	}
	slog.Info("admission changed", "holding", kind, "reason", reason)
}

// admission is how the node decides whether to start new work, for its
// heartbeat. Call with e.mu held.
func (e *Engine) admission() *pb.Admission {
	waiting := map[string]int32{}
	var nQueued int32
	for _, c := range e.commands {
		if c.cmd().State == queued {
			nQueued++
			waiting[c.waiting]++
		}
	}
	now := e.now()
	hu := e.freshHostUsage(now)
	holding := e.held.kind != ""
	reason := e.held.reason
	since := e.held.since
	if e.systemUpgrading {
		holding = true
		reason = "upgrading cladefs"
		if since.IsZero() {
			since = now
		}
	} else if (e.held.kind == "host-cpu" || e.held.kind == "host-memory") && hu == nil {
		// Host signal went stale past TTL before the next sample/step tick.
		kind, r := e.nodeHold(now)
		holding = kind != ""
		reason = r
		if !holding {
			since = time.Time{}
		}
	}
	u := e.usage
	a := &pb.Admission{Holding: holding, Reason: reason, Queued: nQueued}
	if a.Holding {
		a.Since = since.UnixMilli()
	}
	a.Signals = []*pb.AdmissionSignal{
		{
			Name: "cpu-pressure", Value: u.GetCpuPressure(), Limit: cpuHoldAt, Resume: cpuResumeAt, Unit: "%",
			Holding: e.cpu.holding, Waiting: waiting["cpu"],
			Detail: fmt.Sprintf("peak %.0f%% in the last %s; measurements are not held", e.cpu.peak(), cpuWindow),
		},
		{
			Name: "memory-pressure", Value: u.GetMemoryPressure(), Limit: admitPressure, Resume: memPressureResumeAt, Unit: "%",
			Holding: e.memPressureHolding || u.GetMemoryPressure() >= admitPressure, Waiting: waiting["pressure"],
		},
		{
			Name: "memory-reserved", Value: float64(e.reserved()), Limit: float64(e.admitLimit()), Unit: "bytes",
			Holding: waiting["memory"] > 0, Waiting: waiting["memory"],
			Detail: fmt.Sprintf("held at most or guaranteed by running and parked commands, base builds too, of the %.0f%% of RAM they may take", admitMemory*100),
		},
		{
			Name: "disk-free", Value: float64(e.disk.GetFreeBytes()), Limit: float64(e.minFreeBytes()), Unit: "bytes", Floor: true,
			Holding: e.diskLow(), Waiting: waiting["disk"],
		},
	}
	if e.diskBytes == 0 {
		a.Signals[3].Detail = "not measured yet"
	}
	if os := e.objectStore; os != nil && (os.GetMemoryFull() || os.GetExtentCacheFloorBytes() > 0) {
		cladefsHeld, _ := e.cladefsMemoryStarved()
		val := float64(os.GetExtentCacheLimitBytes())
		if val == 0 {
			val = float64(os.GetExtentCacheBytes())
		}
		floor := os.GetExtentCacheFloorBytes()
		resume := floor + floor/4
		detail := fmt.Sprintf("DAX cache holding %s", gib(int64(os.GetExtentCacheBytes())))
		if os.GetMemoryFull() {
			detail += " (host memory full)"
		} else if os.GetMemoryPressure() {
			detail += " (evicting)"
		}
		a.Signals = append(a.Signals, &pb.AdmissionSignal{
			Name: "cladefs-memory", Value: val, Limit: float64(floor), Resume: float64(resume), Unit: "bytes",
			Floor: true, Holding: cladefsHeld, Waiting: waiting["cladefs-memory"], Detail: detail,
		})
	}
	if hu != nil {
		cpuHeld, cpuVal, cpuLim, cpuResume, _, cpuDetail := e.hostCPUStatus(hu)
		memHeld, _ := hostMemoryStarved(hu)
		memDetail := "macOS memory pressure normal"
		if hu.GetHostMemoryPressure() >= hostMemPressureHoldAt {
			memDetail = "macOS memory pressure critical"
		} else if hu.GetHostMemoryPressure() >= 50 {
			memDetail = "macOS memory pressure warn (soft placement penalty only)"
		}
		a.Signals = append(a.Signals,
			&pb.AdmissionSignal{
				Name: "host-cpu-starvation", Value: cpuVal, Limit: cpuLim, Resume: cpuResume, Unit: "%",
				Holding: cpuHeld, Waiting: waiting["host-cpu"], Detail: cpuDetail,
			},
			&pb.AdmissionSignal{
				Name: "host-memory-pressure", Value: hu.GetHostMemoryPressure(), Limit: hostMemPressureHoldAt, Unit: "%",
				Holding: memHeld, Waiting: waiting["host-memory"], Detail: memDetail,
			},
		)
	}
	return a
}
