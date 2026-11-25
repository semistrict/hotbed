//go:build darwin

package setup

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

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

// SampleHostUsage implements HostMetricsVM for vfkitVM on macOS. The guest
// runs in the Virtualization framework's XPC process, not in vfkit, so that is
// the process whose CPU it samples.
func (vm vfkitVM) SampleHostUsage(ctx context.Context) (*pb.HostUsage, error) {
	if vm.sampler == nil {
		return nil, nil
	}
	pid, state, err := vmProcessState(ctx, vm.p)
	if err != nil || state != vmProcRunning || pid <= 0 {
		return nil, nil
	}
	vzPID, err := vzProcessFor(pid)
	if err != nil {
		return nil, err
	}
	return vm.sampler.Sample(ctx, vzPID, vm.cfg.CPUs)
}

// vzProcessName is the executable of the Virtualization framework's XPC
// service that runs a VM's vCPUs and memory.
const vzProcessName = "com.apple.Virtualization.VirtualMachine"

// vzStartWindow is how soon after vfkit starts the XPC service for its VM
// does: vfkit creates its VM as it starts.
const vzStartWindow = 10 * time.Second

// procInfo is what vzProcessFor knows of a process.
type procInfo struct {
	pid   int
	exe   string
	start time.Time
}

// vzProcessFor finds the Virtualization framework XPC process that runs the
// VM of the vfkit process vfkitPID. launchd, not vfkit, is its parent, so it
// is the one that started within vzStartWindow of vfkit.
func vzProcessFor(vfkitPID int) (int, error) {
	vf, err := unix.SysctlKinfoProc("kern.proc.pid", vfkitPID)
	if err != nil {
		return 0, fmt.Errorf("read vfkit process %d: %w", vfkitPID, err)
	}
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return 0, fmt.Errorf("list processes: %w", err)
	}
	var procs []procInfo
	for _, kp := range all {
		// p_comm holds the executable's first 16 bytes.
		if !strings.HasPrefix(vzProcessName, unix.ByteSliceToString(kp.Proc.P_comm[:])) {
			continue
		}
		pid := int(kp.Proc.P_pid)
		exe, err := processExecutable(pid)
		if err != nil {
			continue // gone since it was listed
		}
		procs = append(procs, procInfo{pid: pid, exe: exe, start: timevalTime(kp.Proc.P_starttime)})
	}
	return pickVZProcess(timevalTime(vf.Proc.P_starttime), procs)
}

// pickVZProcess picks the one process of procs that is the Virtualization
// framework's VM service and started within vzStartWindow after vfkitStart.
// Sampling another VM's process would misreport the host, so anything but
// exactly one such process is an error.
func pickVZProcess(vfkitStart time.Time, procs []procInfo) (int, error) {
	var found []int
	for _, pr := range procs {
		if filepath.Base(pr.exe) != vzProcessName {
			continue
		}
		if pr.start.Before(vfkitStart) || pr.start.After(vfkitStart.Add(vzStartWindow)) {
			continue
		}
		found = append(found, pr.pid)
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return 0, fmt.Errorf("no %s process started within %s of vfkit (started %s)", vzProcessName, vzStartWindow, vfkitStart.Format(time.RFC3339))
	default:
		return 0, fmt.Errorf("%d %s processes started within %s of vfkit (pids %v); cannot tell which runs the worker VM", len(found), vzProcessName, vzStartWindow, found)
	}
}

// processExecutable is pid's executable path, from kern.procargs2.
func processExecutable(pid int) (string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	// argc (int32), then the executable path, NUL-terminated.
	if len(b) < 5 {
		return "", fmt.Errorf("kern.procargs2 for %d is %d bytes", pid, len(b))
	}
	exe, _, _ := strings.Cut(string(b[4:]), "\x00")
	return exe, nil
}

func timevalTime(tv unix.Timeval) time.Time {
	return time.Unix(tv.Sec, int64(tv.Usec)*1000)
}
