package worker

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/semistrict/hotbed/internal/api"
)

// Probe measures this machine: the CPUs this process may run on, total
// memory, and the kernel release.
func Probe() (api.Resources, error) {
	mem, err := memTotal()
	if err != nil {
		return api.Resources{}, err
	}
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return api.Resources{}, fmt.Errorf("uname: %w", err)
	}
	return api.Resources{
		Arch:        runtime.GOARCH,
		CPUs:        runtime.NumCPU(),
		MemoryBytes: mem,
		Kernel:      string(bytes.TrimRight(u.Release[:], "\x00")),
	}, nil
}

func memTotal() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// MemTotal:       16384000 kB
		rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		kb, err := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse /proc/meminfo MemTotal %q: %w", rest, err)
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return 0, fmt.Errorf("/proc/meminfo has no MemTotal")
}
