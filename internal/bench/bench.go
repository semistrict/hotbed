// Package bench isolates a node for a measurement: the measurement's slice
// gets the node's measurement CPUs to itself, and, where the node has them,
// most of its L3 cache and memory bandwidth and the power budget the other
// cores' clocks leave it, while other work's IO on the scratch disk is
// capped. Off gives everything back, from what On saved of the host as it
// was, even when a worker other than the one that isolated asks.
//
// It does what fxjob-isolate did for fxjob --bench (the fleet skill's
// fxjob/retired), on hotbed's own slices:
//
//   - CPUs: a cpuset partition root of their own, the measurement's slice,
//     handed them through every ancestor's cpuset.cpus.exclusive (a remote
//     partition): the kernel takes them out of every other cgroup's CPUs.
//     Unbound workqueues and movable IRQs go to the other CPUs. Managed IRQs
//     (a device's per-CPU queues) can't move, and serve only what their CPU
//     submits; nor can an interrupt controller's that has no affinity
//     (Apple's AIC).
//   - Cache and bandwidth (resctrl, Intel CAT/MBA or AMD L3 CAT/MBA), by
//     CPU: a task of the default group takes the class of the CPU it runs
//     on, so the partition's CPUs give whatever runs on them the
//     measurement's class, which keeps the host's memory bandwidth.
//   - Clocks: the other CPUs capped at their nominal frequency (ACPI CPPC),
//     leaving the package's power budget for the measurement's to boost.
//     Refused where a clock is shared with a measurement CPU (a cluster's,
//     on Apple Silicon).
package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/unit"
)

// Config is a node's measurements, as its worker's config sets them (the
// host's setup step). A worker without one takes no measurements.
type Config struct {
	// CPUs is the measurement's CPUs, a cpulist such as 4-7,12-15.
	CPUs string `json:"cpus"`
	// L3 is the measurement's L3 ways, a capacity bitmask (fff0), and
	// WorkL3 everyone else's (000f), in every L3 domain; both or neither.
	L3     string `json:"l3,omitempty"`
	WorkL3 string `json:"workL3,omitempty"`
	// WorkMB is everyone else's memory bandwidth, in resctrl MBA's units
	// (AMD's: 1/8 GB/s); the measurement keeps the host's.
	WorkMB int `json:"workMB,omitempty"`
	// CapClocks caps the other CPUs at their nominal clock.
	CapClocks bool `json:"capClocks,omitempty"`
	// Disk is a path on the disk measurements keep their stores on (the
	// scratch disk): other work's IO on it is capped while one runs.
	Disk string `json:"disk,omitempty"`
}

// What other work may do on the scratch disk while a measurement runs (in
// systemd's units: 32M is 32 MB/s): a dd loop of work's wrote 186 MB/s
// beside one (fxjob's TASK-374).
var workIO = []string{"IOReadBandwidthMax=%s 64M", "IOWriteBandwidthMax=%s 32M", "IOReadIOPSMax=%s 2000", "IOWriteIOPSMax=%s 1000"}

// Systemd is what the isolation asks of the system manager.
type Systemd interface {
	StartSlice(ctx context.Context, slice string) error
	SetProperties(ctx context.Context, unit string, props ...string) error
}

// Options are where an Isolation works.
type Options struct {
	Root      string // where /sys and /proc are; "" for /
	State     string // directory for what On changed, as it was (under /run: it lasts as long as the kernel's settings)
	Slice     string // the measurement's slice
	WorkSlice string // every other command's slice, whose IO is capped
	Systemd   Systemd
	// Mount mounts resctrl at target, and Mkdir and Rmdir make and remove a
	// resctrl group; nil for the kernel's own.
	Mount func(target string) error
	Mkdir func(dir string) error
	Rmdir func(dir string) error
}

// Isolation isolates one node for its measurements.
type Isolation struct {
	cfg     Config
	o       Options
	cpus    CPUs // the measurement's
	work    CPUs // every other online CPU
	resctrl bool
}

// New checks cfg against the node.
func New(cfg Config, o Options) (*Isolation, error) {
	cpus, err := ParseCPUs(cfg.CPUs)
	if err != nil {
		return nil, fmt.Errorf("bench cpus: %w", err)
	}
	if len(cpus) == 0 {
		return nil, errors.New("bench cpus: none named")
	}
	if o.Root == "" {
		o.Root = "/"
	}
	if o.Mount == nil {
		o.Mount = mountResctrl
	}
	if o.Mkdir == nil {
		o.Mkdir = func(dir string) error { return os.Mkdir(dir, 0o755) }
	}
	if o.Rmdir == nil {
		o.Rmdir = os.Remove
	}
	i := &Isolation{cfg: cfg, o: o, cpus: cpus, resctrl: cfg.L3 != "" || cfg.WorkMB != 0}
	online, err := i.readCPUs("/sys/devices/system/cpu/online")
	if err != nil {
		return nil, err
	}
	if len(cpus.Minus(online)) > 0 {
		return nil, fmt.Errorf("bench cpus %s are not all online (%s)", cpus, online)
	}
	if i.work = online.Minus(cpus); len(i.work) == 0 {
		return nil, fmt.Errorf("bench cpus %s leave no CPU for other work", cpus)
	}
	if (cfg.L3 == "") != (cfg.WorkL3 == "") {
		return nil, errors.New("bench l3 and workL3 go together")
	}
	return i, nil
}

// Slice is the measurement's slice.
func (i *Isolation) Slice() string { return i.o.Slice }

// CPUs is the measurement's CPUs.
func (i *Isolation) CPUs() CPUs { return i.cpus }

// Count is how many CPUs the measurement takes, and how many it leaves.
func (i *Isolation) Count() (bench, work int) { return len(i.cpus), len(i.work) }

// Active says whether the node is isolated, or partly: On saved the host's
// settings and Off has not yet given them back.
func (i *Isolation) Active() bool {
	_, err := os.Stat(i.statePath())
	return err == nil
}

// On isolates the node. It either isolates or fails, having undone what it
// did: a measurement doesn't run unisolated.
func (i *Isolation) On(ctx context.Context) (err error) {
	if i.cfg.CapClocks {
		for _, c := range i.work {
			related, err := i.read(cpufreq(c, "related_cpus"))
			if err != nil {
				return err
			}
			shared, err := ParseCPUs(strings.Join(strings.Fields(related), ","))
			if err != nil {
				return fmt.Errorf("cpu%d's related_cpus: %w", c, err)
			}
			if shared.Intersects(i.cpus) {
				return fmt.Errorf("cpu%d shares its clock with the measurement's CPUs (%s): capping the others' clocks would cap the measurement", c, i.cpus)
			}
		}
	}
	if i.resctrl && !i.exists(resctrl+"/schemata") {
		if err := i.o.Mount(i.path(resctrl)); err != nil {
			return fmt.Errorf("mount resctrl: %w", err)
		}
	}
	if err := i.save(); err != nil {
		return fmt.Errorf("save the host's settings: %w", err)
	}
	defer func() {
		if err != nil {
			if undo := i.Off(context.WithoutCancel(ctx)); undo != nil {
				err = errors.Join(err, fmt.Errorf("undo: %w", undo))
			}
		}
	}()

	// systemd writes a slice's AllowedCPUs to its cpuset.cpus whenever it
	// realizes it: given the partition's, it keeps them.
	if err := i.o.Systemd.StartSlice(ctx, i.o.Slice); err != nil {
		return err
	}
	if err := i.o.Systemd.SetProperties(ctx, i.o.Slice, "AllowedCPUs="+i.cpus.String()); err != nil {
		return err
	}
	for _, c := range i.chain() {
		if err := i.write(c+"/cpuset.cpus.exclusive", i.cpus.String()); err != nil {
			return err
		}
	}
	slice := i.sliceDir()
	if err := i.write(slice+"/cpuset.cpus", i.cpus.String()); err != nil {
		return err
	}
	if err := i.write(slice+"/cpuset.cpus.partition", "root"); err != nil {
		return err
	}
	if err := i.partitioned(); err != nil {
		return err
	}

	if err := i.write(workqueues, i.work.Mask()); err != nil {
		return err
	}
	if err := i.write(defaultAffinity, i.work.Mask()); err != nil {
		return err
	}
	irqs, err := i.irqs()
	if err != nil {
		return err
	}
	for _, irq := range irqs {
		// Managed IRQs refuse; they serve only their own CPU's requests.
		_ = i.write(irqAffinity(irq), i.work.String())
	}

	if i.cfg.CapClocks {
		caps := map[int]string{}
		for _, c := range i.work {
			if caps[c], err = i.clockCap(c); err != nil {
				return err
			}
		}
		if err := i.setClocks(caps); err != nil {
			return err
		}
	}

	if i.resctrl {
		host, err := i.read(resctrl + "/schemata")
		if err != nil {
			return err
		}
		if err := i.o.Mkdir(i.path(resctrlGroup)); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := i.write(resctrlGroup+"/cpus_list", i.cpus.String()); err != nil {
			return err
		}
		var bench, work []string
		if i.cfg.L3 != "" {
			bench = append(bench, schemata(host, "L3", i.cfg.L3))
			work = append(work, schemata(host, "L3", i.cfg.WorkL3))
		}
		if i.cfg.WorkMB != 0 {
			bench = append(bench, line(host, "MB"))
			work = append(work, schemata(host, "MB", strconv.Itoa(i.cfg.WorkMB)))
		}
		if err := i.write(resctrlGroup+"/schemata", strings.Join(bench, "\n")); err != nil {
			return err
		}
		if err := i.write(resctrl+"/schemata", strings.Join(work, "\n")); err != nil {
			return err
		}
	}

	if i.cfg.Disk != "" {
		props := make([]string, len(workIO))
		for n, p := range workIO {
			props[n] = fmt.Sprintf(p, i.cfg.Disk)
		}
		if err := i.o.Systemd.SetProperties(ctx, i.o.WorkSlice, props...); err != nil {
			return err
		}
	}
	return nil
}

// Off gives back what On took, from what it saved. It does what it can and
// keeps the saved settings while anything failed, for the next Off.
func (i *Isolation) Off(ctx context.Context) error {
	var errs []error
	try := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	// Each file only where it is: systemd takes a slice's cpuset files away
	// once nothing in it asks for CPUs.
	slice := i.sliceDir()
	for _, f := range []struct{ name, value string }{{"cpuset.cpus.partition", "member"}, {"cpuset.cpus", ""}} {
		if i.exists(slice + "/" + f.name) {
			try(i.write(slice+"/"+f.name, f.value))
		}
	}
	chain := i.chain()
	for n := len(chain) - 1; n >= 0; n-- {
		if i.exists(chain[n] + "/cpuset.cpus.exclusive") {
			try(i.write(chain[n]+"/cpuset.cpus.exclusive", ""))
		}
	}
	try(i.o.Systemd.SetProperties(ctx, i.o.Slice, "AllowedCPUs="))
	if i.cfg.Disk != "" {
		props := make([]string, len(workIO))
		for n, p := range workIO {
			props[n], _, _ = strings.Cut(p, "=")
			props[n] += "="
		}
		try(i.o.Systemd.SetProperties(ctx, i.o.WorkSlice, props...))
	}
	if i.exists(resctrlGroup) {
		try(i.o.Rmdir(i.path(resctrlGroup)))
	}
	s, err := i.load()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.Join(errs...)
	case err != nil:
		return errors.Join(append(errs, err)...)
	}
	if s.Schemata != "" {
		try(i.write(resctrl+"/schemata", s.Schemata))
	}
	try(i.write(workqueues, s.Workqueue))
	try(i.write(defaultAffinity, s.DefaultAffinity))
	for irq, aff := range s.IRQs {
		// An IRQ gone since, or one that refused to move.
		_ = i.write(irqAffinity(irq), aff)
	}
	if len(s.MaxFreq) > 0 {
		try(i.setClocks(s.MaxFreq))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return os.RemoveAll(i.o.State)
}

// Held says why the isolation On made no longer holds, or nil while it
// does: the partition the kernel invalidated (a CPU taken offline, a cpuset
// written over its CPUs), workqueues let back onto its CPUs, its cache class
// gone.
func (i *Isolation) Held() error {
	if err := i.partitioned(); err != nil {
		return err
	}
	mask, err := i.read(workqueues)
	if err != nil {
		return err
	}
	if wq, err := ParseMask(mask); err != nil || !slices.Equal(wq, i.work) {
		return fmt.Errorf("unbound workqueues may run on %s, not only %s", wq, i.work)
	}
	if i.resctrl {
		got, err := i.readCPUs(resctrlGroup + "/cpus_list")
		if err != nil {
			return fmt.Errorf("the measurement's cache and bandwidth class: %w", err)
		}
		if !slices.Equal(got, i.cpus) {
			return fmt.Errorf("the measurement's cache and bandwidth class is on CPUs %s, not %s", got, i.cpus)
		}
	}
	return nil
}

func (i *Isolation) partitioned() error {
	slice := i.sliceDir()
	partition, err := i.read(slice + "/cpuset.cpus.partition")
	if err != nil {
		return err
	}
	effective, err := i.readCPUs(slice + "/cpuset.cpus.effective")
	if err != nil {
		return err
	}
	if partition != "root" || !slices.Equal(effective, i.cpus) {
		return fmt.Errorf("the measurement's slice is not a partition of %s: %s, its CPUs %s", i.cpus, partition, effective)
	}
	return nil
}

// saved is the host's settings that On changes, as they were.
type saved struct {
	Workqueue       string            `json:"workqueue"`
	DefaultAffinity string            `json:"defaultAffinity"`
	IRQs            map[string]string `json:"irqs"`
	MaxFreq         map[int]string    `json:"maxFreq,omitempty"`
	// Schemata is the root resctrl group's L3 and MB lines.
	Schemata string `json:"schemata,omitempty"`
}

func (i *Isolation) statePath() string { return filepath.Join(i.o.State, "saved.json") }

// save keeps the host's settings, once: after a worker that stopped before
// its Off, what is set is the isolation's, not the host's.
func (i *Isolation) save() error {
	if i.Active() {
		return nil
	}
	var s saved
	var err error
	if s.Workqueue, err = i.read(workqueues); err != nil {
		return err
	}
	if s.DefaultAffinity, err = i.read(defaultAffinity); err != nil {
		return err
	}
	irqs, err := i.irqs()
	if err != nil {
		return err
	}
	s.IRQs = map[string]string{}
	for _, irq := range irqs {
		if s.IRQs[irq], err = i.read(irqAffinity(irq)); err != nil {
			return err
		}
	}
	if i.cfg.CapClocks {
		s.MaxFreq = map[int]string{}
		for _, c := range i.work {
			if s.MaxFreq[c], err = i.read(cpufreq(c, "scaling_max_freq")); err != nil {
				return err
			}
		}
	}
	if i.resctrl {
		host, err := i.read(resctrl + "/schemata")
		if err != nil {
			return err
		}
		var lines []string
		for _, l := range strings.Split(host, "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "L3:") || strings.HasPrefix(l, "MB:") {
				lines = append(lines, l)
			}
		}
		s.Schemata = strings.Join(lines, "\n")
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(i.o.State, 0o700); err != nil {
		return err
	}
	tmp := i.statePath() + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, i.statePath())
}

func (i *Isolation) load() (saved, error) {
	var s saved
	b, err := os.ReadFile(i.statePath())
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", i.statePath(), err)
	}
	return s, nil
}

// clockCap is what On caps a CPU's clock at, in kHz: its nominal clock.
func (i *Isolation) clockCap(cpu int) (string, error) {
	nominal, err := i.read(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/acpi_cppc/nominal_freq", cpu))
	if err != nil {
		return "", err
	}
	mhz, err := strconv.ParseInt(nominal, 10, 64)
	if err != nil {
		return "", fmt.Errorf("cpu%d's nominal_freq %q: %w", cpu, nominal, err)
	}
	return strconv.FormatInt(mhz*1000, 10), nil
}

// clockSettle is how long setClocks waits for the kernel to apply new
// clock limits.
const clockSettle = 2 * time.Second

// setClocks sets CPUs' max clocks (kHz) and waits until each reads back as
// set. cpufreq applies a new limit from a work item after the write
// returns, and until then reads the old one: an On straight after an Off
// (a worker between two measurements) would save a cap being lifted as the
// host's, and keep it for good (seen on ubu1).
func (i *Isolation) setClocks(khz map[int]string) error {
	for c, v := range khz {
		if err := i.write(cpufreq(c, "scaling_max_freq"), v); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(clockSettle)
	for c, v := range khz {
		for {
			got, err := i.read(cpufreq(c, "scaling_max_freq"))
			if err != nil {
				return err
			}
			if got == v {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("cpu%d's max clock still reads %s %v after setting it to %s", c, got, clockSettle, v)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return nil
}

const (
	workqueues      = "/sys/devices/virtual/workqueue/cpumask"
	defaultAffinity = "/proc/irq/default_smp_affinity"
	resctrl         = "/sys/fs/resctrl"
	resctrlGroup    = resctrl + "/hotbed-bench"
)

func irqAffinity(irq string) string { return "/proc/irq/" + irq + "/smp_affinity_list" }

func cpufreq(cpu int, file string) string {
	return fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/%s", cpu, file)
}

// sliceDir is the measurement slice's cgroup.
func (i *Isolation) sliceDir() string { return "/sys/fs/cgroup" + unit.SliceDir(i.o.Slice) }

// chain is the measurement slice's cgroup and each of its ancestors below
// the root, outermost first.
func (i *Isolation) chain() []string {
	var out []string
	dir := "/sys/fs/cgroup"
	for _, part := range strings.Split(strings.TrimPrefix(unit.SliceDir(i.o.Slice), "/"), "/") {
		dir += "/" + part
		out = append(out, dir)
	}
	return out
}

// irqs is every IRQ the kernel lists.
func (i *Isolation) irqs() ([]string, error) {
	entries, err := os.ReadDir(i.path("/proc/irq"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err == nil && e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (i *Isolation) path(p string) string { return filepath.Join(i.o.Root, p) }

func (i *Isolation) exists(p string) bool {
	_, err := os.Stat(i.path(p))
	return err == nil
}

func (i *Isolation) read(p string) (string, error) {
	b, err := os.ReadFile(i.path(p))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (i *Isolation) readCPUs(p string) (CPUs, error) {
	s, err := i.read(p)
	if err != nil {
		return nil, err
	}
	cpus, err := ParseCPUs(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return cpus, nil
}

// write writes s and a newline to a kernel file in one write, as echo does.
// Not os.WriteFile: cgroup files are pollable, so Go's runtime would take an
// EAGAIN for "try again when writable" and wait forever.
func (i *Isolation) write(p, s string) error {
	fd, err := syscall.Open(i.path(p), syscall.O_WRONLY|syscall.O_TRUNC|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	defer syscall.Close(fd)
	for {
		_, err = syscall.Write(fd, []byte(s+"\n"))
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("write %q to %s: %w", s, p, err)
	}
	return nil
}

// schemata is a resctrl schemata line giving resource value in every one of
// its domains, as the root group's schemata (host) lists them.
func schemata(host, resource, value string) string {
	l := line(host, resource)
	_, domains, _ := strings.Cut(l, ":")
	var parts []string
	for _, d := range strings.Split(domains, ";") {
		id, _, _ := strings.Cut(d, "=")
		parts = append(parts, id+"="+value)
	}
	return resource + ":" + strings.Join(parts, ";")
}

// line is resource's line of a schemata file.
func line(host, resource string) string {
	for _, l := range strings.Split(host, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, resource+":") {
			return l
		}
	}
	return ""
}
