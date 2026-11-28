// Package sysinfo reads what a Linux node has and how busy it is, from /proc
// and /sys.
package sysinfo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Root is where /proc and /sys are; tests point it at fixtures.
var Root = "/"

func path(p string) string { return strings.TrimSuffix(Root, "/") + p }

// Resources measures the machine, as commands in the cgroup at sliceDir
// (under /sys/fs/cgroup) get it: their memory is the tightest of the
// host's and every enclosing cgroup's limit.
func Resources(sliceDir string) (*pb.Resources, error) {
	ds, err := domains(sliceDir)
	if err != nil {
		return nil, err
	}
	mem := ds[0]
	for _, d := range ds[1:] {
		mem.Total = min(mem.Total, d.Total)
	}
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return nil, fmt.Errorf("uname: %w", err)
	}
	return &pb.Resources{
		Os:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Cpus:        int32(runtime.NumCPU()),
		MemoryBytes: mem.Total,
		SwapBytes:   mem.SwapTotal,
		Kernel:      string(bytes.TrimRight(u.Release[:], "\x00")),
		Machine:     Machine(),
	}, nil
}

// Machine names the hardware as a person would: the device tree's model on
// Apple Silicon, the DMI product name and CPU elsewhere.
func Machine() string {
	if b, err := os.ReadFile(path("/proc/device-tree/model")); err == nil {
		if m := strings.TrimSpace(strings.TrimRight(string(b), "\x00")); m != "" {
			return strings.TrimPrefix(m, "Apple ")
		}
	}
	product := readTrim("/sys/devices/virtual/dmi/id/product_name")
	cpu := ""
	if b, err := os.ReadFile(path("/proc/cpuinfo")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				cpu = strings.TrimSpace(v)
				break
			}
		}
	}
	switch {
	case product != "" && cpu != "":
		return product + " (" + cpu + ")"
	case product != "":
		return product
	default:
		return cpu
	}
}

func readTrim(p string) string {
	b, err := os.ReadFile(path(p))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Mem is memory, in bytes: the host's (/proc/meminfo), or a cgroup's that
// limits it more.
type Mem struct {
	Total, Available, SwapTotal, SwapFree int64
	// SwapDynamic is true when the OS allocates swapfiles on demand (macOS),
	// so SwapTotal is only the currently allocated swapfiles, not a fixed cap.
	SwapDynamic bool
	// Domain is what Total and Available are of: "" for the host, or a
	// cgroup, such as /user.slice.
	Domain string
}

// Used is memory in use: what is not available to new work.
func (m Mem) Used() int64 { return m.Total - m.Available }

// Meminfo reads /proc/meminfo.
func Meminfo() (Mem, error) {
	f, err := os.Open(path("/proc/meminfo"))
	if err != nil {
		return Mem{}, err
	}
	defer f.Close()
	var m Mem
	fields := map[string]*int64{"MemTotal": &m.Total, "MemAvailable": &m.Available, "SwapTotal": &m.SwapTotal, "SwapFree": &m.SwapFree}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		dst, want := fields[k]
		if !ok || !want {
			continue
		}
		kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		if err != nil {
			return Mem{}, fmt.Errorf("parse /proc/meminfo %s %q: %w", k, v, err)
		}
		*dst = kb * 1024
	}
	if err := sc.Err(); err != nil {
		return Mem{}, err
	}
	if m.Total == 0 {
		return Mem{}, fmt.Errorf("/proc/meminfo has no MemTotal")
	}
	return m, nil
}

// Swap summarizes the host's active swap devices from /proc/swaps, separating
// in-RAM compressed swap (/dev/zram*) from disk-backed swap (partitions and
// files).
type Swap struct {
	ZramBytes int64
	DiskBytes int64
}

// Total returns all active swap in bytes.
func (s Swap) Total() int64 { return s.ZramBytes + s.DiskBytes }

// Swaps reads /proc/swaps. A missing /proc/swaps (e.g. swap disabled in the
// kernel) returns zero Swap and nil error.
func Swaps() (Swap, error) {
	f, err := os.Open(path("/proc/swaps"))
	if errors.Is(err, os.ErrNotExist) {
		return Swap{}, nil
	}
	if err != nil {
		return Swap{}, err
	}
	defer f.Close()
	return ParseSwaps(f)
}

// ParseSwaps parses Linux /proc/swaps content into zram and disk-backed swap
// totals in bytes.
func ParseSwaps(r io.Reader) (Swap, error) {
	var s Swap
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if fields[0] == "Filename" {
			continue
		}
		if len(fields) < 5 {
			return Swap{}, fmt.Errorf("parse /proc/swaps line %q: expected at least 5 fields, got %d", line, len(fields))
		}
		kb, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || kb < 0 {
			return Swap{}, fmt.Errorf("parse /proc/swaps size %q on line %q", fields[2], line)
		}
		if kb > math.MaxInt64/1024 {
			return Swap{}, fmt.Errorf("parse /proc/swaps size %q overflows int64", fields[2])
		}
		sizeBytes := kb * 1024
		// Guard ZramBytes, DiskBytes, and their combined Total() against int64 overflow.
		if sizeBytes > math.MaxInt64-s.Total() {
			return Swap{}, fmt.Errorf("parse /proc/swaps total size overflows int64 on line %q", line)
		}
		if isZramSwap(fields[0]) {
			s.ZramBytes += sizeBytes
		} else {
			s.DiskBytes += sizeBytes
		}
	}
	if err := sc.Err(); err != nil {
		return Swap{}, err
	}
	return s, nil
}

// isZramSwap reports whether a /proc/swaps filename names a zram block device
// (/dev/zram0, /dev/zram1, ...).
func isZramSwap(filename string) bool {
	rest, ok := strings.CutPrefix(filename, "/dev/zram")
	if !ok || rest == "" {
		return false
	}
	for i := range len(rest) {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}

// Memory is the memory commands in the cgroup at sliceDir can use: the
// host's, or that of an enclosing cgroup with a memory.max that is fuller,
// such as a user.slice capped to keep the system room. A cgroup's memory in
// use is what it holds less the file cache it could drop (inactive_file),
// as the host's MemAvailable leaves that out too. Swap is the host's.
func Memory(sliceDir string) (Mem, error) {
	ds, err := domains(sliceDir)
	if err != nil {
		return Mem{}, err
	}
	best := ds[0]
	for _, d := range ds[1:] {
		if fuller(d, best) {
			best = d
		}
	}
	return best, nil
}

// domains is the host's memory, then that of each cgroup enclosing (or at)
// sliceDir that has a limit, outermost first, each clamped to the tightest
// enclosing cgroup swap limit (memory.swap.max / memory.swap.current).
func domains(sliceDir string) ([]Mem, error) {
	host, err := Meminfo()
	if err != nil {
		return nil, err
	}
	out := []Mem{host}
	swapTotal, swapFree := host.SwapTotal, host.SwapFree
	dir := ""
	for _, part := range strings.Split(strings.Trim(sliceDir, "/"), "/") {
		if part == "" {
			continue
		}
		dir += "/" + part
		if st, sf, ok, err := cgroupSwap(dir); err != nil {
			return nil, err
		} else if ok {
			swapTotal = min(swapTotal, st)
			swapFree = min(swapFree, sf)
		}
		m, ok, err := cgroupMemory(dir)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, m)
		}
	}
	for i := range out {
		out[i].SwapTotal = swapTotal
		out[i].SwapFree = min(swapFree, swapTotal)
	}
	return out, nil
}

// fuller says whether a has less room left than b, as a share of its size.
func fuller(a, b Mem) bool {
	return float64(a.Used())/float64(a.Total) > float64(b.Used())/float64(b.Total)
}

// readCgroupLimit reads a cgroup v2 byte limit file (memory.max or
// memory.high). Returns ok=false when the file does not exist or is "max".
func readCgroupLimit(p string) (int64, bool, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", p, err)
	}
	if n <= 0 {
		return 0, false, nil
	}
	return n, true, nil
}

// cgroupSwap reads a cgroup v2 directory's finite memory.swap.max and
// memory.swap.current (including 0, which disables swap for the cgroup).
// Returns ok=false when memory.swap.max is missing or "max".
func cgroupSwap(dir string) (total, free int64, ok bool, err error) {
	base := path("/sys/fs/cgroup" + dir)
	b, err := os.ReadFile(base + "/memory.swap.max")
	if os.IsNotExist(err) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, 0, false, nil
	}
	limit, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, 0, false, fmt.Errorf("%s/memory.swap.max: %w", dir, err)
	}
	if limit < 0 {
		return 0, 0, false, nil
	}
	if limit == 0 {
		return 0, 0, true, nil
	}
	curB, err := os.ReadFile(base + "/memory.swap.current")
	if os.IsNotExist(err) {
		return limit, limit, true, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	current, err := strconv.ParseInt(strings.TrimSpace(string(curB)), 10, 64)
	if err != nil {
		return 0, 0, false, fmt.Errorf("%s/memory.swap.current: %w", dir, err)
	}
	return limit, max(0, limit-current), true, nil
}

// cgroupMemory reads a cgroup's effective memory limit and use; ok is false
// when it has neither a finite memory.max nor memory.high, or is not there (a
// slice with no unit running). When both memory.max and memory.high are finite,
// the tighter limit (typically memory.high) is used so admission and parking
// trigger BEFORE memory.current crosses memory.high and causes permanent
// kernel throttling / memory PSI stalls.
func cgroupMemory(dir string) (m Mem, ok bool, err error) {
	base := path("/sys/fs/cgroup" + dir)
	maxLim, hasMax, err := readCgroupLimit(base + "/memory.max")
	if err != nil {
		return Mem{}, false, err
	}
	highLim, hasHigh, err := readCgroupLimit(base + "/memory.high")
	if err != nil {
		return Mem{}, false, err
	}
	var limit int64
	switch {
	case hasMax && hasHigh:
		limit = min(maxLim, highLim)
	case hasHigh:
		limit = highLim
	case hasMax:
		limit = maxLim
	default:
		return Mem{}, false, nil
	}
	cur, err := os.ReadFile(base + "/memory.current")
	if err != nil {
		return Mem{}, false, err
	}
	current, err := strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64)
	if err != nil {
		return Mem{}, false, fmt.Errorf("%s/memory.current: %w", dir, err)
	}
	stat, err := os.ReadFile(base + "/memory.stat")
	if err != nil {
		return Mem{}, false, err
	}
	var inactiveFile int64
	for _, line := range strings.Split(string(stat), "\n") {
		if v, ok := strings.CutPrefix(line, "inactive_file "); ok {
			inactiveFile, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	used := max(0, current-inactiveFile)
	return Mem{Total: limit, Available: max(0, limit-used), Domain: dir}, true, nil
}

// Pressure reads a PSI file's "some avg10": the share of the last ten
// seconds, in percent, that at least one task stalled on the resource.
func Pressure(resource string) (float64, error) {
	b, err := os.ReadFile(path("/proc/pressure/" + resource))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "some ")
		if !ok {
			continue
		}
		for _, f := range strings.Fields(rest) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				return strconv.ParseFloat(v, 64)
			}
		}
	}
	return 0, fmt.Errorf("/proc/pressure/%s has no some avg10", resource)
}

// CPU tracks /proc/stat between samples, to report how busy the CPUs were.
type CPU struct {
	mu   sync.Mutex
	last cpuTicks
}

// CPUShare is how CPU time, across all CPUs, was spent since the last sample.
type CPUShare struct {
	// Busy is the share that was not idle, nested VMs' vCPUs included.
	Busy float64
	// Nested is the share that ran nested VMs' vCPUs (/proc/stat guest and
	// guest_nice). This machine's kernel counts it busy for as long as a
	// vCPU thread is inside the nested VM, idle or not, so it says nothing
	// of how much CPU the machine's own host gave it.
	Nested float64
}

// Own is the share of CPU time busy outside nested VMs.
func (s CPUShare) Own() float64 { return max(0, s.Busy-s.Nested) }

// Sample is how CPU time was spent since the last call. The first call
// measures since boot.
func (c *CPU) Sample() (CPUShare, error) {
	t, err := readCPUTicks()
	if err != nil {
		return CPUShare{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d := cpuTicks{busy: t.busy - c.last.busy, nested: t.nested - c.last.nested, all: t.all - c.last.all}
	c.last = t
	if d.all == 0 {
		return CPUShare{}, nil
	}
	return CPUShare{Busy: float64(d.busy) / float64(d.all), Nested: float64(d.nested) / float64(d.all)}, nil
}

// cpuTicks is cumulative CPU time from /proc/stat, in clock ticks.
type cpuTicks struct{ busy, nested, all uint64 }

// readCPUTicks reads the first line of /proc/stat:
// cpu user nice system idle iowait irq softirq steal guest guest_nice
func readCPUTicks() (cpuTicks, error) {
	f, err := os.Open(path("/proc/stat"))
	if err != nil {
		return cpuTicks{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuTicks{}, fmt.Errorf("/proc/stat is empty")
	}
	fs := strings.Fields(sc.Text())
	if len(fs) < 11 || fs[0] != "cpu" {
		return cpuTicks{}, fmt.Errorf("/proc/stat starts %q", sc.Text())
	}
	var v [10]uint64
	for i := range v {
		v[i], err = strconv.ParseUint(fs[i+1], 10, 64)
		if err != nil {
			return cpuTicks{}, fmt.Errorf("parse /proc/stat: %w", err)
		}
	}
	var t cpuTicks
	// guest and guest_nice are already counted in user and nice, so only
	// the first eight add to the total.
	for _, x := range v[:8] {
		t.all += x
	}
	t.busy = t.all - v[3] - v[4]
	t.nested = v[8] + v[9]
	return t, nil
}

// OOMKills is how many processes the kernel's OOM killer has killed since
// boot (/proc/vmstat oom_kill).
func OOMKills() (int64, error) {
	b, err := os.ReadFile(path("/proc/vmstat"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("/proc/vmstat has no oom_kill")
}
