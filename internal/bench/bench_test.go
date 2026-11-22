//go:build !benchtest

// The isolation against a fixture of /sys and /proc, as any user; the root
// tests (benchtest) isolate a host for real.
package bench

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSystemd records what the isolation asks of systemd; props is what
// Show reports of a unit's properties.
type fakeSystemd struct {
	calls []string
	props map[string]map[string][]string
}

func (f *fakeSystemd) Show(_ context.Context, unit string, props ...string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, p := range props {
		if v := f.props[unit][p]; len(v) > 0 {
			out[p] = v
		}
	}
	return out, nil
}

func (f *fakeSystemd) StartSlice(_ context.Context, slice string) error {
	f.calls = append(f.calls, "start "+slice)
	return nil
}

func (f *fakeSystemd) SetProperties(_ context.Context, unit string, props ...string) error {
	f.calls = append(f.calls, "set "+unit+" "+strings.Join(props, " "))
	return nil
}

// host is a fixture of the files the isolation reads and writes, as a
// kernel would have them before any measurement: an 11-CPU machine (16 with
// clocks) whose user-1000.slice holds the slices.
type host struct {
	t    *testing.T
	root string
	sd   *fakeSystemd
}

const benchSlice = "user-1000-hotbed_bench.slice"

var testChain = []string{"user.slice", "user.slice/user-1000.slice", "user.slice/user-1000.slice/" + benchSlice}

func newHost(t *testing.T, cpus string) *host {
	h := &host{t: t, root: t.TempDir(), sd: &fakeSystemd{}}
	h.write("sys/devices/system/cpu/online", cpus+"\n")
	all, err := ParseCPUs(cpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range testChain {
		h.write("sys/fs/cgroup/"+c+"/cpuset.cpus.exclusive", "\n")
		h.write("sys/fs/cgroup/"+c+"/cpuset.cpus", "\n")
		h.write("sys/fs/cgroup/"+c+"/cpuset.cpus.partition", "member\n")
		h.write("sys/fs/cgroup/"+c+"/cpuset.cpus.effective", cpus+"\n")
	}
	h.write("sys/devices/virtual/workqueue/cpumask", all.Mask()+"\n")
	h.write("proc/irq/default_smp_affinity", all.Mask()+"\n")
	h.write("proc/irq/1/smp_affinity_list", cpus+"\n")
	h.write("proc/irq/24/smp_affinity_list", "3\n")
	// A managed IRQ, which the kernel won't let anyone move.
	h.write("proc/irq/30/smp_affinity_list", "5\n")
	if err := os.Chmod(h.path("proc/irq/30/smp_affinity_list"), 0o444); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *host) path(p string) string { return filepath.Join(h.root, p) }

func (h *host) write(p, s string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.path(p)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.path(p), []byte(s), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) read(p string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.path(p))
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// snapshot is every fixture file's content, to compare before and after.
func (h *host) snapshot() map[string]string {
	h.t.Helper()
	m := map[string]string{}
	err := filepath.Walk(h.root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(h.root, p)
		if info.IsDir() {
			m[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		m[rel] = strings.TrimSpace(string(b))
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

// kernelPartitions has the fixture's bench slice take its CPUs, as the
// kernel does once it is a valid partition root.
func (h *host) kernelPartitions(cpus string) {
	h.write("sys/fs/cgroup/"+testChain[2]+"/cpuset.cpus.effective", cpus+"\n")
}

func (h *host) isolation(t *testing.T, cfg Config, state string) *Isolation {
	t.Helper()
	iso, err := New(cfg, Options{
		Root: h.root, State: state, Slice: benchSlice, WorkSlice: "user-1000-hotbed.slice",
		Systemd: h.sd, Rmdir: os.RemoveAll, Mkdir: func(dir string) error {
			// The kernel gives a new resctrl group its files.
			rel := strings.TrimPrefix(dir, h.root+"/")
			h.write(rel+"/cpus_list", "\n")
			h.write(rel+"/schemata", "\n")
			return nil
		}, Mount: func(target string) error {
			h.write(strings.TrimPrefix(target, h.root+"/")+"/schemata", "    MB:0=2048;1=2048\n    L3:0=ffff;1=ffff\n")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return iso
}

func TestOnAndOffOnAppleSilicon(t *testing.T) {
	h := newHost(t, "0-10")
	h.kernelPartitions("7-10")
	before := h.snapshot()
	state := filepath.Join(t.TempDir(), "bench")
	iso := h.isolation(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"}, state)
	if iso.Active() {
		t.Fatal("active before On")
	}
	if b, w := iso.Count(); b != 4 || w != 7 {
		t.Fatalf("Count() = %d, %d, want 4 bench CPUs and 7 for work", b, w)
	}
	if err := iso.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range testChain {
		if got := h.read("sys/fs/cgroup/" + c + "/cpuset.cpus.exclusive"); got != "7-10" {
			t.Errorf("%s exclusive = %q, want 7-10", c, got)
		}
	}
	for file, want := range map[string]string{
		"sys/fs/cgroup/" + testChain[2] + "/cpuset.cpus":           "7-10",
		"sys/fs/cgroup/" + testChain[2] + "/cpuset.cpus.partition": "root",
		"sys/devices/virtual/workqueue/cpumask":                    "0000007f",
		"proc/irq/default_smp_affinity":                            "0000007f",
		"proc/irq/1/smp_affinity_list":                             "0-6",
		"proc/irq/24/smp_affinity_list":                            "0-6",
		"proc/irq/30/smp_affinity_list":                            "5",
	} {
		if got := h.read(file); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	if !iso.Active() {
		t.Error("not active after On")
	}
	if err := iso.Held(); err != nil {
		t.Errorf("Held after On: %v", err)
	}
	wantCalls := []string{
		"start " + benchSlice,
		"set " + benchSlice + " AllowedCPUs=7-10",
		"set user-1000-hotbed.slice IOReadBandwidthMax=/mnt/fxdax-scratch 64M IOWriteBandwidthMax=/mnt/fxdax-scratch 32M IOReadIOPSMax=/mnt/fxdax-scratch 2000 IOWriteIOPSMax=/mnt/fxdax-scratch 1000",
	}
	if !slices.Equal(h.sd.calls, wantCalls) {
		t.Errorf("systemd calls\n got %q\nwant %q", h.sd.calls, wantCalls)
	}

	h.sd.calls = nil
	if err := iso.Off(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := h.snapshot(); !mapsEqual(before, after) {
		t.Errorf("Off left the host changed:\n%s", diff(before, after))
	}
	wantCalls = []string{
		"set " + benchSlice + " AllowedCPUs=",
		"set user-1000-hotbed.slice IOReadBandwidthMax= IOWriteBandwidthMax= IOReadIOPSMax= IOWriteIOPSMax=",
	}
	if !slices.Equal(h.sd.calls, wantCalls) {
		t.Errorf("systemd calls\n got %q\nwant %q", h.sd.calls, wantCalls)
	}
	if iso.Active() {
		t.Error("active after Off")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("saved state left after Off: %v", err)
	}
}

// ubu1's: resctrl's cache and bandwidth classes, and the other cores'
// clocks capped at nominal.
func TestOnAndOffWithCacheBandwidthAndClocks(t *testing.T) {
	h := newHost(t, "0-15")
	h.kernelPartitions("4-7,12-15")
	for c := range 16 {
		cpu := "sys/devices/system/cpu/cpu" + itoa(c)
		h.write(cpu+"/cpufreq/scaling_max_freq", "4200000\n")
		h.write(cpu+"/cpufreq/cpuinfo_max_freq", "4200000\n")
		h.write(cpu+"/cpufreq/related_cpus", itoa(c)+"\n")
		h.write(cpu+"/acpi_cppc/nominal_freq", "3000\n")
	}
	h.write("sys/fs/resctrl/schemata", "    MB:0=2048;1=2048\n    L3:0=ffff;1=ffff\n")
	before := h.snapshot()
	iso := h.isolation(t, Config{CPUs: "4-7,12-15", L3: "fff0", WorkL3: "000f", WorkMB: 40, CapClocks: true}, filepath.Join(t.TempDir(), "bench"))
	if err := iso.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		"sys/fs/resctrl/schemata":                               "L3:0=000f;1=000f\nMB:0=40;1=40",
		"sys/fs/resctrl/hotbed-bench/schemata":                  "L3:0=fff0;1=fff0\nMB:0=2048;1=2048",
		"sys/fs/resctrl/hotbed-bench/cpus_list":                 "4-7,12-15",
		"sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq":  "3000000",
		"sys/devices/system/cpu/cpu11/cpufreq/scaling_max_freq": "3000000",
		"sys/devices/system/cpu/cpu4/cpufreq/scaling_max_freq":  "4200000",
		"sys/devices/virtual/workqueue/cpumask":                 "00000f0f",
	} {
		if got := h.read(file); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	if err := iso.Held(); err != nil {
		t.Errorf("Held after On: %v", err)
	}
	if err := iso.Off(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The kernel merges what is written to schemata into each resource's
	// line; the fixture keeps the last write, which is the host's two lines.
	after := h.snapshot()
	after["sys/fs/resctrl/schemata"] = before["sys/fs/resctrl/schemata"]
	if got := h.read("sys/fs/resctrl/schemata"); got != "MB:0=2048;1=2048\nL3:0=ffff;1=ffff" {
		t.Errorf("resctrl schemata after Off = %q, want the host's", got)
	}
	if !mapsEqual(before, after) {
		t.Errorf("Off left the host changed:\n%s", diff(before, after))
	}
}

// A measurement's CPUs sharing a clock with the others' (a cluster's, on
// Apple Silicon) would be capped with them: On refuses, changing nothing.
func TestCapClocksRefusesASharedClock(t *testing.T) {
	h := newHost(t, "0-10")
	for c := range 11 {
		related := "1 2 3 4 5 6"
		if c == 0 || c >= 7 {
			related = "0 7 8 9 10"
		}
		h.write("sys/devices/system/cpu/cpu"+itoa(c)+"/cpufreq/related_cpus", related+"\n")
	}
	before := h.snapshot()
	state := filepath.Join(t.TempDir(), "bench")
	iso := h.isolation(t, Config{CPUs: "7-10", CapClocks: true}, state)
	err := iso.On(t.Context())
	if err == nil || err.Error() != "cpu0 shares its clock with the measurement's CPUs (7-10): capping the others' clocks would cap the measurement" {
		t.Fatalf("On = %v, want it refused for cpu0's clock", err)
	}
	if after := h.snapshot(); !mapsEqual(before, after) {
		t.Errorf("a refused On changed the host:\n%s", diff(before, after))
	}
	if iso.Active() {
		t.Error("active after a refused On")
	}
}

// A partition the kernel won't make (its CPUs not all its own) fails On,
// which undoes what it did.
func TestOnFailsWhenThePartitionIsInvalid(t *testing.T) {
	h := newHost(t, "0-10")
	before := h.snapshot()
	iso := h.isolation(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"}, filepath.Join(t.TempDir(), "bench"))
	err := iso.On(t.Context())
	if err == nil || err.Error() != "the measurement's slice is not a partition of 7-10: root, its CPUs 0-10" {
		t.Fatalf("On = %v, want the invalid partition", err)
	}
	if after := h.snapshot(); !mapsEqual(before, after) {
		t.Errorf("a failed On left the host changed:\n%s", diff(before, after))
	}
	if iso.Active() {
		t.Error("active after a failed On")
	}
}

// Off gives back what On changed, where it changed it, whatever the config
// says by then (a new slice, no disk); and a worker whose config no longer
// takes measurements gives it back all the same.
func TestOffGivesBackWhereOnChanged(t *testing.T) {
	for _, giveBack := range []func(h *host, state string) error{
		func(h *host, state string) error {
			return h.isolation(t, Config{CPUs: "1-2"}, state).Off(t.Context())
		},
		func(h *host, state string) error {
			return GiveBack(t.Context(), Options{Root: h.root, State: state, Systemd: h.sd, Rmdir: os.RemoveAll})
		},
	} {
		h := newHost(t, "0-10")
		h.kernelPartitions("7-10")
		before := h.snapshot()
		state := filepath.Join(t.TempDir(), "bench")
		if err := h.isolation(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"}, state).On(t.Context()); err != nil {
			t.Fatal(err)
		}
		h.sd.calls = nil
		if err := giveBack(h, state); err != nil {
			t.Fatal(err)
		}
		if after := h.snapshot(); !mapsEqual(before, after) {
			t.Errorf("Off left the host changed:\n%s", diff(before, after))
		}
		want := []string{
			"set " + benchSlice + " AllowedCPUs=",
			"set user-1000-hotbed.slice IOReadBandwidthMax= IOWriteBandwidthMax= IOReadIOPSMax= IOWriteIOPSMax=",
		}
		if !slices.Equal(h.sd.calls, want) {
			t.Errorf("systemd calls\n got %q\nwant %q", h.sd.calls, want)
		}
	}
}

// The work slice's own IO limits, its unit files', are as they were after
// Off: it resets what it capped to them, not to none.
func TestOffKeepsTheWorkSlicesOwnIOLimits(t *testing.T) {
	h := newHost(t, "0-10")
	h.kernelPartitions("7-10")
	h.sd.props = map[string]map[string][]string{"user-1000-hotbed.slice": {"IOReadBandwidthMax": {"/dev/sda 10000000", "/dev/sdb 20000000"}}}
	iso := h.isolation(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"}, filepath.Join(t.TempDir(), "bench"))
	if err := iso.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.sd.calls = nil
	if err := iso.Off(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := "set user-1000-hotbed.slice IOReadBandwidthMax= IOReadBandwidthMax=/dev/sda 10000000 IOReadBandwidthMax=/dev/sdb 20000000 IOWriteBandwidthMax= IOReadIOPSMax= IOWriteIOPSMax="
	if len(h.sd.calls) != 2 || h.sd.calls[1] != want {
		t.Errorf("systemd calls %q, want the last\n%q", h.sd.calls, want)
	}
}

// With nothing saved, nothing was changed: Off leaves the host, and systemd,
// alone.
func TestOffWithNothingSavedDoesNothing(t *testing.T) {
	h := newHost(t, "0-10")
	before := h.snapshot()
	if err := h.isolation(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"}, filepath.Join(t.TempDir(), "bench")).Off(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := h.snapshot(); !mapsEqual(before, after) || len(h.sd.calls) > 0 {
		t.Errorf("Off with nothing saved changed the host:\n%s\nsystemd: %q", diff(before, after), h.sd.calls)
	}
}

// An On over an isolation an Off failed to give back takes the host's
// bandwidth from what was saved, not from the root group, which has the
// others' by then.
func TestOnAgainKeepsTheHostsBandwidth(t *testing.T) {
	h := newHost(t, "0-15")
	h.kernelPartitions("4-7,12-15")
	h.write("sys/fs/resctrl/schemata", "    MB:0=2048;1=2048\n    L3:0=ffff;1=ffff\n")
	state := filepath.Join(t.TempDir(), "bench")
	iso := h.isolation(t, Config{CPUs: "4-7,12-15", L3: "fff0", WorkL3: "000f", WorkMB: 40}, state)
	for range 2 {
		if err := iso.On(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.read("sys/fs/resctrl/hotbed-bench/schemata"); got != "L3:0=fff0;1=fff0\nMB:0=2048;1=2048" {
		t.Errorf("the measurement's class after a second On = %q, want the host's bandwidth", got)
	}
}

// The host's settings are saved once: an On over an isolation an Off failed
// to give back keeps the first's, not what it finds. And a new worker
// undoes what an old one did, from what that one saved.
func TestSavedStateOutlivesTheWorker(t *testing.T) {
	h := newHost(t, "0-10")
	h.kernelPartitions("7-10")
	before := h.snapshot()
	state := filepath.Join(t.TempDir(), "bench")
	if err := h.isolation(t, Config{CPUs: "7-10"}, state).On(t.Context()); err != nil {
		t.Fatal(err)
	}
	again := h.isolation(t, Config{CPUs: "7-10"}, state)
	if !again.Active() {
		t.Fatal("a new worker doesn't see the isolation the old one left")
	}
	if err := again.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.isolation(t, Config{CPUs: "7-10"}, state).Off(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := h.snapshot(); !mapsEqual(before, after) {
		t.Errorf("Off after a restart left the host changed:\n%s", diff(before, after))
	}
}

// systemd takes a slice's cpuset files away once nothing in it asks for
// CPUs: an Off after one (a second Off, or one after the worker reset
// AllowedCPUs) gives back the rest.
func TestOffWhereTheCpusetFilesAreGone(t *testing.T) {
	h := newHost(t, "0-10")
	h.kernelPartitions("7-10")
	before := h.snapshot()
	iso := h.isolation(t, Config{CPUs: "7-10"}, filepath.Join(t.TempDir(), "bench"))
	if err := iso.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"cpuset.cpus.partition", "cpuset.cpus", "cpuset.cpus.effective", "cpuset.cpus.exclusive"} {
		if err := os.Remove(h.path("sys/fs/cgroup/" + testChain[2] + "/" + f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := iso.Off(t.Context()); err != nil {
		t.Fatalf("Off: %v", err)
	}
	after := h.snapshot()
	for _, f := range []string{"cpuset.cpus.partition", "cpuset.cpus", "cpuset.cpus.effective", "cpuset.cpus.exclusive"} {
		delete(before, "sys/fs/cgroup/"+testChain[2]+"/"+f)
	}
	if !mapsEqual(before, after) {
		t.Errorf("Off left the host changed:\n%s", diff(before, after))
	}
}

func TestHeldSaysWhatWasLost(t *testing.T) {
	h := newHost(t, "0-10")
	h.kernelPartitions("7-10")
	iso := h.isolation(t, Config{CPUs: "7-10"}, filepath.Join(t.TempDir(), "bench"))
	if err := iso.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.write("sys/fs/cgroup/"+testChain[2]+"/cpuset.cpus.partition", "root invalid (Cpu list in cpuset.cpus not exclusive)\n")
	if err := iso.Held(); err == nil || err.Error() != "the measurement's slice is not a partition of 7-10: root invalid (Cpu list in cpuset.cpus not exclusive), its CPUs 7-10" {
		t.Errorf("Held = %v", err)
	}
	h.write("sys/fs/cgroup/"+testChain[2]+"/cpuset.cpus.partition", "root\n")
	h.write("sys/devices/virtual/workqueue/cpumask", "7ff\n")
	if err := iso.Held(); err == nil || err.Error() != "unbound workqueues may run on 0-10, not only 0-6" {
		t.Errorf("Held = %v", err)
	}
}

func TestNewChecksTheConfig(t *testing.T) {
	h := newHost(t, "0-10")
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{CPUs: "7-"}, `bench cpus: "7-" is not a cpulist, such as 4-7,12-15`},
		{Config{CPUs: ""}, "bench cpus: none named"},
		{Config{CPUs: "0-10"}, "bench cpus 0-10 leave no CPU for other work"},
		{Config{CPUs: "9-12"}, "bench cpus 9-12 are not all online (0-10)"},
		{Config{CPUs: "7-10", L3: "fff0"}, "bench l3 and workL3 go together"},
	} {
		_, err := New(tc.cfg, Options{Root: h.root, State: t.TempDir(), Slice: benchSlice, WorkSlice: "w.slice", Systemd: h.sd})
		if err == nil || err.Error() != tc.want {
			t.Errorf("New(%+v) = %v, want %q", tc.cfg, err, tc.want)
		}
	}
}

func TestSchemata(t *testing.T) {
	root := "    MB:0=2048;1=2048\n    L3:0=ffff;1=ffff\n"
	if got := schemata(root, "L3", "000f"); got != "L3:0=000f;1=000f" {
		t.Errorf("L3 = %q", got)
	}
	if got := schemata(root, "MB", "40"); got != "MB:0=40;1=40" {
		t.Errorf("MB = %q", got)
	}
	if got := line(root, "MB"); got != "MB:0=2048;1=2048" {
		t.Errorf("line MB = %q", got)
	}
}

func itoa(i int) string { return CPUs{i}.String() }
