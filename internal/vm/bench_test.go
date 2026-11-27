package vm

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const benchVMCgroup = "/user.slice/user-1000.slice/user-1000-hotbed_bench.slice/hotbed-vm-ws-m.service"

// benchVMHost is a Firecracker VM manager whose host's /sys and /proc are
// a test's files: the VM unit's cgroup holds Firecracker's threads, named
// as Firecracker names them, and pins records what was pinned where.
type benchVMHost struct {
	mgr  *Manager
	fl   *fakeLauncher
	pins map[int]int // thread -> CPU
}

func newBenchVMHost(t *testing.T, benchSlice string) *benchVMHost {
	t.Helper()
	root := t.TempDir()
	threads := map[int]string{900: "firecracker", 901: "fc_vcpu 0", 902: "fc_vcpu 1", 903: "fc_extent_q0", 904: "dax faults"}
	var tids []string
	for tid, comm := range threads {
		tids = append(tids, strconv.Itoa(tid))
		writeTestFile(t, filepath.Join(root, "proc", strconv.Itoa(tid), "comm"), comm+"\n")
	}
	slices.Sort(tids)
	writeTestFile(t, filepath.Join(root, "sys/fs/cgroup", benchVMCgroup, "cgroup.threads"), strings.Join(tids, "\n")+"\n")

	h := &benchVMHost{fl: newFakeLauncher(), pins: map[int]int{}}
	h.fl.onStart = func(s ProcessSpec) error {
		h.fl.customStatus[s.Name] = ProcessStatus{Loaded: true, Running: true, Cgroup: benchVMCgroup}
		return nil
	}
	mgr, err := NewManager(Config{
		RunDir:          shortSockDir(t),
		Slice:           "user-1000-hotbed.slice",
		BenchSlice:      benchSlice,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 2 * time.Second,
	}, WithLauncher(h.fl), WithDialerFactory(startTestGuestd(t)), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatal(err)
	}
	fc := mgr.vmm.(*firecrackerVMM)
	fc.fsRoot = root
	fc.setAffinity = func(tid, cpu int) error {
		h.pins[tid] = cpu
		return nil
	}
	h.mgr = mgr
	return h
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A measurement's VM boots in the measurement's slice, never swapping, and
// each of its vCPUs is pinned to a measurement CPU of its own, in order;
// Firecracker's other threads are left to the slice's partition. It says
// it booted so, after a worker restart too, and the orphan reaper looks
// for VMs in the measurement's slice as well.
func TestManagerBootsAMeasurementsVMPinned(t *testing.T) {
	ctx := context.Background()
	h := newBenchVMHost(t, "user-1000-hotbed_bench.slice")
	inst, err := h.mgr.Start(ctx, LaunchSpec{ID: "ws-m", CellHostPath: "5/40", CladefsVMSock: "/run/cladefsd/vm.sock", VCPUs: 2, BenchCPUs: "4-7,12-15"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := h.fl.slices["hotbed-vm-ws-m"]; got != "user-1000-hotbed_bench.slice" {
		t.Errorf("its unit is in slice %q, want the measurement's", got)
	}
	if got := h.fl.props["hotbed-vm-ws-m"]; !slices.Equal(got, []string{"MemorySwapMax=0"}) {
		t.Errorf("its unit's properties = %q, want MemorySwapMax=0", got)
	}
	if want := map[int]int{901: 4, 902: 5}; !maps.Equal(h.pins, want) {
		t.Errorf("pinned threads to CPUs %v, want %v", h.pins, want)
	}
	if inst.BenchCPUs != "4-7,12-15" {
		t.Errorf("instance BenchCPUs = %q", inst.BenchCPUs)
	}

	again, err := NewManager(h.mgr.configSnapshot(), WithLauncher(h.fl), WithDialerFactory(h.mgr.dialerFn), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := again.Reconnect(ctx); err != nil || n != 1 {
		t.Fatalf("Reconnect = %d, %v", n, err)
	}
	if got := again.Get("ws-m").BenchCPUs; got != "4-7,12-15" {
		t.Errorf("reconnected BenchCPUs = %q", got)
	}
	again.ReconcileOrphans(ctx, map[string]bool{"ws-m": true})
	if !slices.Contains(h.fl.lists, "user-1000-hotbed_bench.slice") {
		t.Errorf("the orphan reaper listed slices %q, not the measurement's", h.fl.lists)
	}
}

// A measurement's VM that can't be booted as one fails before anything of
// it runs: more vCPUs than measurement CPUs, or no measurement slice.
func TestManagerRefusesAMeasurementsVMItCannotIsolate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, slice string
		vcpus       int
		want        string
	}{
		{"more vCPUs than CPUs", "hotbed_bench.slice", 5, "vm ws-m: a measurement's VM of 5 vCPUs is more than its 4 measurement CPUs (4-7)"},
		{"no measurement slice", "", 2, "vm ws-m: a measurement's VM needs the measurement slice, which the VM manager's config names none of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBenchVMHost(t, tc.slice)
			_, err := h.mgr.Start(ctx, LaunchSpec{ID: "ws-m", CellHostPath: "5/40", CladefsVMSock: "/run/cladefsd/vm.sock", VCPUs: tc.vcpus, BenchCPUs: "4-7"})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Start = %v, want %q", err, tc.want)
			}
			if len(h.fl.slices) != 0 {
				t.Errorf("launched %v, want nothing", h.fl.slices)
			}
		})
	}
}

// A VM whose vCPU threads aren't all there to pin fails, rather than run a
// measurement unpinned.
func TestPinVCPUsFailsWithoutEveryVCPUThread(t *testing.T) {
	h := newBenchVMHost(t, "hotbed_bench.slice")
	h.fl.customStatus["hotbed-vm-ws-m"] = ProcessStatus{Loaded: true, Running: true, Cgroup: benchVMCgroup}
	err := h.mgr.vmm.(*firecrackerVMM).PinVCPUs(context.Background(), "hotbed-vm-ws-m", []int{4, 5, 6})
	if want := "hotbed-vm-ws-m runs 2 vCPU threads, not the 3 it booted with"; err == nil || err.Error() != want {
		t.Fatalf("PinVCPUs = %v, want %q", err, want)
	}
}
