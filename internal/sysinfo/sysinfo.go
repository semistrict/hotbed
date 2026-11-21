// Package sysinfo reads what a Linux node has and how busy it is, from /proc
// and /sys.
package sysinfo

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

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
// sliceDir that has a limit, outermost first, each with the host's swap.
func domains(sliceDir string) ([]Mem, error) {
	host, err := Meminfo()
	if err != nil {
		return nil, err
	}
	out := []Mem{host}
	dir := ""
	for _, part := range strings.Split(strings.Trim(sliceDir, "/"), "/") {
		if part == "" {
			continue
		}
		dir += "/" + part
		m, ok, err := cgroupMemory(dir)
		if err != nil {
			return nil, err
		}
		if ok {
			m.SwapTotal, m.SwapFree = host.SwapTotal, host.SwapFree
			out = append(out, m)
		}
	}
	return out, nil
}

// fuller says whether a has less room left than b, as a share of its size.
func fuller(a, b Mem) bool {
	return float64(a.Used())/float64(a.Total) > float64(b.Used())/float64(b.Total)
}

// cgroupMemory reads a cgroup's limit and use; ok is false when it has no
// limit, or is not there (a slice with no unit running).
func cgroupMemory(dir string) (m Mem, ok bool, err error) {
	base := path("/sys/fs/cgroup" + dir)
	maxb, err := os.ReadFile(base + "/memory.max")
	if os.IsNotExist(err) {
		return Mem{}, false, nil
	}
	if err != nil {
		return Mem{}, false, err
	}
	if strings.TrimSpace(string(maxb)) == "max" {
		return Mem{}, false, nil
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(maxb)), 10, 64)
	if err != nil {
		return Mem{}, false, fmt.Errorf("%s/memory.max: %w", dir, err)
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
	mu         sync.Mutex
	busy, all  uint64
	lastSample time.Time
}

// Busy is the share of CPU time, across all CPUs, that was not idle since
// the last call. The first call measures since boot.
func (c *CPU) Busy() (float64, error) {
	busy, all, err := cpuTimes()
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	db, da := busy-c.busy, all-c.all
	c.busy, c.all, c.lastSample = busy, all, time.Now()
	if da == 0 {
		return 0, nil
	}
	return float64(db) / float64(da), nil
}

// cpuTimes reads the first line of /proc/stat:
// cpu user nice system idle iowait irq softirq steal guest guest_nice
func cpuTimes() (busy, all uint64, err error) {
	f, err := os.Open(path("/proc/stat"))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, fmt.Errorf("/proc/stat is empty")
	}
	fs := strings.Fields(sc.Text())
	if len(fs) < 9 || fs[0] != "cpu" {
		return 0, 0, fmt.Errorf("/proc/stat starts %q", sc.Text())
	}
	var v [8]uint64
	for i := range v {
		v[i], err = strconv.ParseUint(fs[i+1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse /proc/stat: %w", err)
		}
	}
	// guest time is already counted in user, so it is left out.
	for _, x := range v {
		all += x
	}
	idle := v[3] + v[4]
	return all - idle, all, nil
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
