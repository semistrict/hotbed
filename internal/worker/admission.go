package worker

import (
	"fmt"
	"log/slog"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
)

// CPU pressure (PSI some avg10, %) past which more work makes all of it
// slower: the node holds new work once pressure reaches cpuHoldAt, and
// starts it again only once it is under cpuResumeAt and has not reached
// cpuHoldAt for cpuWindow. Measurements, on CPUs of their own, are not held.
const (
	cpuHoldAt   = 60.0
	cpuResumeAt = 30.0
	cpuWindow   = 30 * time.Second
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
// evicting at/near its floor (within a 12.5% hysteresis band above floor).
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
	if os.GetMemoryPressure() && floor > 0 && limit > 0 && limit <= floor+floor/8 {
		return true, fmt.Sprintf("cladefsd memory pressure near floor (DAX cache limit %s, floor %s); new work waits for cache headroom", gib(int64(limit)), gib(int64(floor)))
	}
	return false, ""
}

// swapFullLocked reports whether a fixed swap cap (non-dynamic swap) is
// >= 95% full while effective RAM is >= swapFullRAM and work is running.
// Call with e.mu held.
func (e *Engine) swapFullLocked(mem sysinfo.Mem, hasMem bool) (bool, string) {
	if !hasMem {
		mem = e.lastMem
		hasMem = mem.Total > 0
	}
	if !hasMem || mem.Total <= 0 || mem.SwapDynamic || mem.SwapTotal <= 0 {
		return false, ""
	}
	memUsed := e.effectiveMemUsedLocked(mem)
	if float64(mem.SwapTotal-mem.SwapFree) >= float64(mem.SwapTotal)*0.95 &&
		float64(memUsed) >= float64(mem.Total)*swapFullRAM && e.hasRunningWorkLocked() {
		return true, fmt.Sprintf("swap %.0f%% full (%s free) while RAM is %.0f%% in use",
			100*float64(mem.SwapTotal-mem.SwapFree)/float64(mem.SwapTotal),
			gib(max(0, mem.SwapFree)),
			100*float64(memUsed)/float64(mem.Total))
	}
	return false, ""
}

// nodeHold is what holds every new command on the node now, whatever it
// would need, or "": cladefsd memory starvation, saturated CPUs, memory
// pressure, a full fixed swap cap, or a full disk. Call with e.mu held.
func (e *Engine) nodeHold() (kind, reason string) {
	if cladefsHeld, cladefsReason := e.cladefsMemoryStarved(); cladefsHeld {
		return "cladefs-memory", cladefsReason
	}
	if swapHeld, swapReason := e.swapFullLocked(sysinfo.Mem{}, false); swapHeld {
		return "memory", swapReason
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

// weighHold notes whether the node holds new work after a sample, and says
// so in the events when that changes. Call with e.mu held.
func (e *Engine) weighHold(now time.Time) {
	kind, reason := e.nodeHold()
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
	holding := e.held.kind != ""
	reason := e.held.reason
	since := e.held.since
	if e.systemUpgrading {
		holding = true
		reason = "upgrading cladefs"
		if since.IsZero() {
			since = now
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
			Holding: waiting["memory"] > 0 || e.held.kind == "memory", Waiting: waiting["memory"],
			Detail: "held at most or guaranteed by running and parked commands, base builds too, plus the growth their estimates still expect, pooled (not all peak at once), of the work slice's memory budget",
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
		resume := floor + floor/8
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
	return a
}
