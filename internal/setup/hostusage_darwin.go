//go:build darwin

package setup

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func newDarwinHostUsageSampler() *hostUsageSampler {
	return newHostUsageSampler(hostUsageProbes{
		procCPU:         darwinProbeProcCPU,
		hostCPUBusy:     darwinProbeHostCPUBusy,
		hostLoad1m:      darwinProbeHostLoad1m,
		hostMemPressure: darwinProbeHostMemPressure,
		hostCPUs:        runtime.NumCPU,
		now:             time.Now,
	})
}

func darwinProbeProcCPU(ctx context.Context, pid int) (time.Duration, float64, bool, error) {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	bin := findInPaths("ps", "/bin", "/usr/bin")
	if bin == "" {
		bin = "/bin/ps"
	}
	out, err := exec.CommandContext(pctx, bin, "-p", strconv.Itoa(pid), "-o", "cputime=", "-o", "%cpu=").Output()
	if err != nil {
		return 0, 0, false, err
	}
	return parsePSProcCPU(string(out))
}

func darwinProbeHostCPUBusy(ctx context.Context) (float64, error) {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	bin := findInPaths("iostat", "/usr/sbin", "/usr/bin", "/bin")
	if bin == "" {
		bin = "/usr/sbin/iostat"
	}
	out, err := exec.CommandContext(pctx, bin, "-C", "-n", "0", "-c", "2", "-w", "1").Output()
	if err != nil {
		return 0, err
	}
	return parseIOStatCPUBusy(string(out))
}

func darwinProbeHostLoad1m(ctx context.Context) (float64, error) {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	bin := findInPaths("sysctl", "/usr/sbin", "/sbin", "/usr/bin")
	if bin == "" {
		bin = "/usr/sbin/sysctl"
	}
	out, err := exec.CommandContext(pctx, bin, "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, err
	}
	return parseSysctlLoadAvg(string(out))
}

func darwinProbeHostMemPressure(ctx context.Context) (float64, error) {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	bin := findInPaths("sysctl", "/usr/sbin", "/sbin", "/usr/bin")
	if bin == "" {
		bin = "/usr/sbin/sysctl"
	}
	out, err := exec.CommandContext(pctx, bin, "-n", "kern.memorystatus_vm_pressure_level").Output()
	if err != nil {
		return 0, err
	}
	return parseMemoryPressureLevel(string(out))
}

// SampleHostUsage implements HostMetricsVM for qemuVM on macOS.
func (vm qemuVM) SampleHostUsage(ctx context.Context) (*pb.HostUsage, error) {
	if vm.sampler == nil {
		return nil, nil
	}
	pid, state, err := vmProcessState(ctx, vm.p)
	if err != nil || state != vmProcRunning || pid <= 0 {
		return nil, nil
	}
	return vm.sampler.Sample(ctx, pid, vm.cfg.CPUs)
}
