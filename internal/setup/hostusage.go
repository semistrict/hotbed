package setup

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// hostUsageProbes holds the OS-level metric readers used by hostUsageSampler.
// Fields are injectable for deterministic unit tests without executing OS tools.
type hostUsageProbes struct {
	// procCPU reads a process's cumulative CPU time and optional instantaneous %CPU.
	procCPU func(ctx context.Context, pid int) (cpuTime time.Duration, pctCPU float64, hasPct bool, err error)
	// hostCPUBusy reads whole-host CPU busy fraction in [0, 1].
	hostCPUBusy func(ctx context.Context) (float64, error)
	// hostLoad1m reads host 1-minute load average.
	hostLoad1m func(ctx context.Context) (float64, error)
	// hostMemPressure reads host memory pressure in [0, 100] (macOS: 0/50/100).
	hostMemPressure func(ctx context.Context) (float64, error)
	// hostCPUs returns logical CPU count on the host.
	hostCPUs func() int
	// now returns the current wall time.
	now func() time.Time
}

// hostUsageSampler computes host-side CPU and memory pressure metrics for a
// QEMU VM process, using cumulative process CPU time deltas across samples so
// interval averages are free of instantaneous spikes.
type hostUsageSampler struct {
	mu           sync.Mutex
	probes       hostUsageProbes
	lastPID      int
	lastCPUTime  time.Duration
	lastSampleAt time.Time
}

func newHostUsageSampler(probes hostUsageProbes) *hostUsageSampler {
	if probes.now == nil {
		probes.now = time.Now
	}
	return &hostUsageSampler{probes: probes}
}

// Sample reads host-side metrics for QEMU pid configured with vcpus.
// All critical probes (QEMU CPU, host CPU busy, host load, and host memory
// pressure) must succeed so a failing probe never sends a synthetic zero that
// could clear an active starvation hold.
func (s *hostUsageSampler) Sample(ctx context.Context, pid, vcpus int) (*pb.HostUsage, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid VM pid %d", pid)
	}
	if s.probes.procCPU == nil {
		return nil, fmt.Errorf("procCPU probe not configured")
	}
	if s.probes.hostCPUBusy == nil {
		return nil, fmt.Errorf("hostCPUBusy probe not configured")
	}
	if s.probes.hostLoad1m == nil {
		return nil, fmt.Errorf("hostLoad1m probe not configured")
	}
	if s.probes.hostMemPressure == nil {
		return nil, fmt.Errorf("hostMemPressure probe not configured")
	}
	cpuTime, pctCPU, hasPct, err := s.probes.procCPU(ctx, pid)
	if err != nil {
		return nil, fmt.Errorf("sample VM pid %d CPU: %w", pid, err)
	}

	rawHostBusy, err := s.probes.hostCPUBusy(ctx)
	if err != nil {
		return nil, fmt.Errorf("sample host CPU busy: %w", err)
	}
	if math.IsNaN(rawHostBusy) || math.IsInf(rawHostBusy, 0) {
		return nil, fmt.Errorf("invalid host CPU busy %v", rawHostBusy)
	}
	hostBusy := clampHostUsage(rawHostBusy, 0, 1)

	rawLoad1m, err := s.probes.hostLoad1m(ctx)
	if err != nil {
		return nil, fmt.Errorf("sample host load: %w", err)
	}
	if math.IsNaN(rawLoad1m) || math.IsInf(rawLoad1m, 0) || rawLoad1m < 0 {
		return nil, fmt.Errorf("invalid host load %v", rawLoad1m)
	}
	load1m := rawLoad1m

	rawMemPressure, err := s.probes.hostMemPressure(ctx)
	if err != nil {
		return nil, fmt.Errorf("sample host memory pressure: %w", err)
	}
	if math.IsNaN(rawMemPressure) || math.IsInf(rawMemPressure, 0) {
		return nil, fmt.Errorf("invalid host memory pressure %v", rawMemPressure)
	}
	memPressure := clampHostUsage(rawMemPressure, 0, 100)

	var hostCPUs int
	if s.probes.hostCPUs != nil {
		hostCPUs = max(0, s.probes.hostCPUs())
	}

	denom := float64(max(vcpus, 1))
	now := s.probes.now()

	s.mu.Lock()
	var vmCPUBusy float64
	hasDelta := false
	if s.lastPID == pid && !s.lastSampleAt.IsZero() {
		dt := now.Sub(s.lastSampleAt)
		dc := cpuTime - s.lastCPUTime
		if dt > 0 && dc >= 0 {
			vmCPUBusy = clampHostUsage((dc.Seconds()/dt.Seconds())/denom, 0, 1)
			hasDelta = true
		}
	}
	if !hasDelta && hasPct {
		vmCPUBusy = clampHostUsage((pctCPU/100.0)/denom, 0, 1)
	}
	s.lastPID = pid
	s.lastCPUTime = cpuTime
	s.lastSampleAt = now
	s.mu.Unlock()

	return &pb.HostUsage{
		VmCpuBusy:          vmCPUBusy,
		HostCpuBusy:        hostBusy,
		HostLoad_1M:        load1m,
		HostCpus:           int32(hostCPUs),
		HostMemoryPressure: memPressure,
		SampledAt:          now.UnixMilli(),
	}, nil
}

func clampHostUsage(v, lo, hi float64) float64 {
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

// parsePSProcCPU parses the output of `ps -p <pid> -o cputime= -o %cpu=`
// (or single-column `ps -p <pid> -o cputime=`).
func parsePSProcCPU(out string) (cpuTime time.Duration, pctCPU float64, hasPct bool, err error) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, 0, false, fmt.Errorf("empty ps output")
	}
	cpuTime, err = parsePSCPUTime(fields[0])
	if err != nil {
		return 0, 0, false, err
	}
	if len(fields) >= 2 {
		v, perr := strconv.ParseFloat(fields[1], 64)
		if perr == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 {
			pctCPU = v
			hasPct = true
		}
	}
	return cpuTime, pctCPU, hasPct, nil
}

// parsePSCPUTime parses BSD/macOS and POSIX ps cputime formats:
// `MM:SS.cc`, `MM:SS`, `HH:MM:SS`, `HH:MM:SS.cc`, `D-HH:MM:SS`, `D-HH:MM:SS.cc`.
func parsePSCPUTime(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty cputime")
	}
	var days float64
	if dash := strings.IndexByte(s, '-'); dash >= 0 {
		d, err := strconv.ParseFloat(s[:dash], 64)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("invalid cputime days in %q", raw)
		}
		days = d
		s = s[dash+1:]
	}
	parts := strings.Split(s, ":")
	var totalSeconds float64
	switch len(parts) {
	case 2:
		mins, err := strconv.ParseFloat(parts[0], 64)
		if err != nil || mins < 0 {
			return 0, fmt.Errorf("invalid cputime minutes in %q", raw)
		}
		secs, err := strconv.ParseFloat(parts[1], 64)
		if err != nil || secs < 0 {
			return 0, fmt.Errorf("invalid cputime seconds in %q", raw)
		}
		totalSeconds = days*86400 + mins*60 + secs
	case 3:
		hours, err := strconv.ParseFloat(parts[0], 64)
		if err != nil || hours < 0 {
			return 0, fmt.Errorf("invalid cputime hours in %q", raw)
		}
		mins, err := strconv.ParseFloat(parts[1], 64)
		if err != nil || mins < 0 {
			return 0, fmt.Errorf("invalid cputime minutes in %q", raw)
		}
		secs, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || secs < 0 {
			return 0, fmt.Errorf("invalid cputime seconds in %q", raw)
		}
		totalSeconds = days*86400 + hours*3600 + mins*60 + secs
	default:
		return 0, fmt.Errorf("unrecognized cputime format %q", raw)
	}
	if math.IsNaN(totalSeconds) || math.IsInf(totalSeconds, 0) {
		return 0, fmt.Errorf("invalid cputime %q", raw)
	}
	return time.Duration(math.Round(totalSeconds * float64(time.Second))), nil
}

// parseIOStatCPUBusy parses macOS `iostat -C -n 0 -c 2 -w 1` output and returns
// the host CPU busy fraction (0..1) from the latest sample row.
func parseIOStatCPUBusy(out string) (float64, error) {
	lines := strings.Split(out, "\n")
	usIdx, syIdx, idIdx := -1, -1, -1
	headerLine := -1
	for i, line := range lines {
		fields := strings.Fields(line)
		for j := 0; j+2 < len(fields); j++ {
			if fields[j] == "us" && fields[j+1] == "sy" && fields[j+2] == "id" {
				usIdx, syIdx, idIdx = j, j+1, j+2
				headerLine = i
				break
			}
		}
		if headerLine >= 0 {
			break
		}
	}
	if headerLine < 0 {
		return 0, fmt.Errorf("missing us/sy/id header in iostat output")
	}
	var lastBusy float64
	found := false
	for _, line := range lines[headerLine+1:] {
		fields := strings.Fields(line)
		if len(fields) <= idIdx {
			continue
		}
		us, err1 := strconv.ParseFloat(fields[usIdx], 64)
		sy, err2 := strconv.ParseFloat(fields[syIdx], 64)
		id, err3 := strconv.ParseFloat(fields[idIdx], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		total := us + sy + id
		if total <= 0 {
			continue
		}
		lastBusy = clampHostUsage((us+sy)/total, 0, 1)
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no sample rows in iostat output")
	}
	return lastBusy, nil
}

// parseSysctlLoadAvg parses macOS `sysctl -n vm.loadavg` output (e.g. "{ 1.23 4.56 7.89 }")
// and returns the 1-minute load average.
func parseSysctlLoadAvg(out string) (float64, error) {
	cleaned := strings.TrimSpace(strings.Trim(strings.TrimSpace(out), "{}"))
	fields := strings.Fields(cleaned)
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty vm.loadavg output")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("invalid vm.loadavg value %q", out)
	}
	return v, nil
}

// parseMemoryPressureLevel maps macOS `sysctl -n kern.memorystatus_vm_pressure_level`
// (1=normal, 2=warn, 4=critical) onto 0..100.
func parseMemoryPressureLevel(out string) (float64, error) {
	s := strings.TrimSpace(out)
	if s == "" {
		return 0, fmt.Errorf("empty memory pressure level")
	}
	lvl, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid memory pressure level %q: %w", s, err)
	}
	switch {
	case lvl < 1:
		return 0, fmt.Errorf("invalid memory pressure level %d", lvl)
	case lvl == 1:
		return 0, nil
	case lvl == 2:
		return 50, nil
	case lvl == 3:
		return 75, nil
	default:
		return 100, nil
	}
}
