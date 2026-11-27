package vm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// vcpuThreadPrefix is how Firecracker names a vCPU's thread: "fc_vcpu 3"
// runs vCPU 3.
const vcpuThreadPrefix = "fc_vcpu "

// PinVCPUs implements VCPUPinner: the thread of each of unit's vCPUs, found
// by its name among the threads of its unit's cgroup, pinned to its CPU.
func (f *firecrackerVMM) PinVCPUs(ctx context.Context, unit string, cpus []int) error {
	st, err := f.launcher.Status(ctx, unit)
	if err != nil {
		return err
	}
	if !st.Running || st.Cgroup == "" {
		return fmt.Errorf("%s is not running in a cgroup to find its vCPUs in", unit)
	}
	threads, err := vcpuThreads(f.root(), st.Cgroup)
	if err != nil {
		return err
	}
	if len(threads) != len(cpus) {
		return fmt.Errorf("%s runs %d vCPU threads, not the %d it booted with", unit, len(threads), len(cpus))
	}
	pin := f.setAffinity
	if pin == nil {
		pin = setThreadAffinity
	}
	for vcpu, cpu := range cpus {
		tid, ok := threads[vcpu]
		if !ok {
			return fmt.Errorf("%s runs no thread for vCPU %d", unit, vcpu)
		}
		if err := pin(tid, cpu); err != nil {
			return fmt.Errorf("pin vCPU %d (thread %d) to CPU %d: %w", vcpu, tid, cpu, err)
		}
	}
	return nil
}

// root is where f finds /sys and /proc.
func (f *firecrackerVMM) root() string {
	if f.fsRoot == "" {
		return "/"
	}
	return f.fsRoot
}

// vcpuThreads is the thread of each vCPU of the VM in cgroup (a path under
// /sys/fs/cgroup), by vCPU, as the files under root say.
func vcpuThreads(root, cgroup string) (map[int]int, error) {
	list, err := os.ReadFile(filepath.Join(root, "sys/fs/cgroup", cgroup, "cgroup.threads"))
	if err != nil {
		return nil, err
	}
	out := map[int]int{}
	for _, field := range strings.Fields(string(list)) {
		tid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("%s's cgroup.threads: %q: %w", cgroup, field, err)
		}
		comm, err := os.ReadFile(filepath.Join(root, "proc", field, "comm"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // a thread that ended meanwhile
		}
		if err != nil {
			return nil, err
		}
		n, ok := strings.CutPrefix(strings.TrimSpace(string(comm)), vcpuThreadPrefix)
		if !ok {
			continue
		}
		vcpu, err := strconv.Atoi(n)
		if err != nil {
			return nil, fmt.Errorf("thread %d is named %q, not a vCPU's", tid, strings.TrimSpace(string(comm)))
		}
		out[vcpu] = tid
	}
	return out, nil
}
