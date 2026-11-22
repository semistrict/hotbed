//go:build linux && benchtest

// Run as root on a systemd machine, with the host's measurements as a
// worker's config gives them; built for its architecture elsewhere:
//
//	GOOS=linux GOARCH=arm64 go test -c -tags benchtest -o bench.test ./internal/bench/
//	sudo HOTBED_BENCH='{"cpus":"7-10","disk":"/mnt/fxdax-scratch"}' ./bench.test -test.v
//
// It isolates the host for real, in slices of its own (hotbedtest.slice),
// and checks that everything it changed is as it was once it is done, also
// when it fails.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/unit"
)

const (
	testWork  = "hotbedtest-work.slice"
	testBench = "hotbedtest-work_bench.slice"
)

type realSystemd struct{}

func (realSystemd) StartSlice(ctx context.Context, slice string) error {
	return unit.StartSlice(ctx, slice)
}
func (realSystemd) SetProperties(ctx context.Context, name string, props ...string) error {
	return unit.SetProperties(ctx, name, props...)
}

func hostConfig(t *testing.T) Config {
	t.Helper()
	var cfg Config
	if err := json.Unmarshal([]byte(os.Getenv("HOTBED_BENCH")), &cfg); err != nil {
		t.Fatalf("set HOTBED_BENCH to the host's measurements, as a worker's config has them: %v", err)
	}
	return cfg
}

// testSlices starts the test's work slice and, once the test and every
// Off it registered are done, stops its slices and removes what
// set-property left of them, as the host was.
func testSlices(t *testing.T) {
	t.Helper()
	if err := unit.StartSlice(t.Context(), testWork); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("systemctl", "stop", testBench, testWork, "hotbedtest.slice").CombinedOutput(); err != nil {
			t.Errorf("stop the test's slices: %v: %s", err, out)
		}
		if out, err := exec.Command("systemctl", "revert", testBench, testWork).CombinedOutput(); err != nil {
			t.Errorf("revert the test's slices: %v: %s", err, out)
		}
	})
}

// realIsolation is the host's isolation for the test's slices, given back
// at the end.
func realIsolation(t *testing.T, cfg Config, state string) *Isolation {
	t.Helper()
	iso, err := New(cfg, Options{State: state, Slice: testBench, WorkSlice: testWork, Systemd: realSystemd{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := iso.Off(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Off at the end: %v", err)
		}
	})
	return iso
}

// hostState is what an isolation changes, read from the kernel.
func hostState(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{}
	add := func(p string) {
		if b, err := os.ReadFile(p); err == nil {
			m[p] = strings.TrimSpace(string(b))
		}
	}
	add("/sys/devices/virtual/workqueue/cpumask")
	add("/proc/irq/default_smp_affinity")
	add("/sys/fs/resctrl/schemata")
	add("/sys/fs/cgroup/user.slice/cpuset.cpus.exclusive")
	add("/sys/fs/cgroup/user.slice/cpuset.cpus.effective")
	irqs, _ := filepath.Glob("/proc/irq/[0-9]*/smp_affinity_list")
	freqs, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_max_freq")
	for _, p := range append(irqs, freqs...) {
		add(p)
	}
	return m
}

// allowed is the CPUs a new process in slice may run on. (Its output goes
// to a file: systemd-run --pipe fails under SELinux's unconfined_service_t,
// as a Tailscale SSH session's sudo is.)
func allowed(t *testing.T, slice string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	if b, err := exec.Command("systemd-run", "--quiet", "--wait", "--slice="+slice, "--property=StandardOutput=file:"+out,
		"--", "grep", "Cpus_allowed_list", "/proc/self/status").CombinedOutput(); err != nil {
		t.Fatalf("run in %s: %v: %s", slice, err, b)
	}
	return strings.TrimSpace(strings.TrimPrefix(read(t, out), "Cpus_allowed_list:"))
}

// everyDomain says whether a schemata line has value, in base, in each of
// its domains: L3's masks are hex, MB's bandwidths decimal.
func everyDomain(line, value string, base int) bool {
	want, err := strconv.ParseUint(value, base, 64)
	_, domains, ok := strings.Cut(line, ":")
	if err != nil || !ok {
		return false
	}
	for _, d := range strings.Split(domains, ";") {
		_, v, _ := strings.Cut(d, "=")
		if got, err := strconv.ParseUint(v, base, 64); err != nil || got != want {
			return false
		}
	}
	return true
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestIsolateForReal(t *testing.T) {
	cfg := hostConfig(t)
	testSlices(t)
	before := hostState(t)
	iso := realIsolation(t, cfg, filepath.Join(t.TempDir(), "bench"))
	if err := iso.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := iso.Held(); err != nil {
		t.Errorf("Held after On: %v", err)
	}
	cpus, work := iso.cpus.String(), iso.work.String()
	if got := allowed(t, testBench); got != cpus {
		t.Errorf("a measurement may run on %s, want %s", got, cpus)
	}
	if got := allowed(t, testWork); got != work {
		t.Errorf("other work may run on %s, want %s", got, work)
	}
	if got := allowed(t, "system.slice"); got != work {
		t.Errorf("the system's services may run on %s, want %s", got, work)
	}
	if got, _ := ParseMask(read(t, "/sys/devices/virtual/workqueue/cpumask")); !slices.Equal(got, iso.work) {
		t.Errorf("unbound workqueues on %s, want %s", got, work)
	}
	if cfg.Disk != "" {
		io := read(t, "/sys/fs/cgroup/hotbedtest.slice/"+testWork+"/io.max")
		// systemd's M is 1000², a disk's.
		if !strings.Contains(io, "rbps=64000000 wbps=32000000 riops=2000 wiops=1000") {
			t.Errorf("work's io.max = %q, want its scratch disk capped", io)
		}
	}
	if cfg.L3 != "" {
		if got := line(read(t, "/sys/fs/resctrl/hotbed-bench/schemata"), "L3"); !everyDomain(got, cfg.L3, 16) {
			t.Errorf("the measurement's L3 = %q, want %s in every domain", got, cfg.L3)
		}
		if got := line(read(t, "/sys/fs/resctrl/schemata"), "L3"); !everyDomain(got, cfg.WorkL3, 16) {
			t.Errorf("work's L3 = %q, want %s in every domain", got, cfg.WorkL3)
		}
	}
	if cfg.WorkMB != 0 {
		if got := line(read(t, "/sys/fs/resctrl/schemata"), "MB"); !everyDomain(got, strconv.Itoa(cfg.WorkMB), 10) {
			t.Errorf("work's MB = %q, want %d in every domain", got, cfg.WorkMB)
		}
	}
	if cfg.CapClocks {
		c := iso.work[0]
		nominal := read(t, fmt.Sprintf("/sys/devices/system/cpu/cpu%d/acpi_cppc/nominal_freq", c))
		if got := read(t, cpufreq(c, "scaling_max_freq")); got != nominal+"000" {
			t.Errorf("cpu%d's clock capped at %s kHz, want its nominal %s MHz", c, got, nominal)
		}
	}
	if err := iso.Off(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := hostState(t); !mapsEqual(before, after) {
		t.Errorf("Off left the host changed:\n%s", diff(before, after))
	}
	all := append(slices.Clone(iso.work), iso.cpus...)
	slices.Sort(all)
	if got := allowed(t, testWork); got != all.String() {
		t.Errorf("after Off other work may run on %s, want every online CPU, %s", got, all)
	}
}

// A parent that no longer hands the partition all its CPUs leaves it short:
// Held says so.
func TestHeldNoticesAPartitionCut(t *testing.T) {
	testSlices(t)
	before := hostState(t)
	iso := realIsolation(t, hostConfig(t), filepath.Join(t.TempDir(), "bench"))
	if err := iso.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	chain := iso.chain()
	short := iso.cpus[:len(iso.cpus)-1]
	if err := iso.write(chain[len(chain)-2]+"/cpuset.cpus.exclusive", short.String()); err != nil {
		t.Fatal(err)
	}
	if err := iso.Held(); err == nil || err.Error() != "the measurement's slice is not a partition of "+iso.cpus.String()+": root, its CPUs "+short.String() {
		t.Errorf("Held = %v, want the partition cut short", err)
	}
	if err := iso.Off(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := hostState(t); !mapsEqual(before, after) {
		t.Errorf("Off left the host changed:\n%s", diff(before, after))
	}
}

// A worker restarted mid-measurement gives the host back from what the one
// before saved.
func TestOffByAnotherWorker(t *testing.T) {
	cfg := hostConfig(t)
	testSlices(t)
	before := hostState(t)
	state := filepath.Join(t.TempDir(), "bench")
	if err := realIsolation(t, cfg, state).On(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := realIsolation(t, cfg, state)
	if !next.Active() {
		t.Fatal("the next worker doesn't see the isolation")
	}
	if err := next.Off(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := hostState(t); !mapsEqual(before, after) {
		t.Errorf("Off left the host changed:\n%s", diff(before, after))
	}
}
