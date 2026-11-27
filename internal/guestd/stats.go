package guestd

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/semistrict/hotbed/internal/sysinfo"
)

type cmdCgroupStats struct {
	memAnon   int64
	memSwap   int64
	memPeak   int64
	swapPeak  int64
	cpuUsec   int64
	oomKilled bool
}

// readCgroupStats reads live or post-exit resource and OOM telemetry from a
// cgroup v2 directory before it is removed.
func readCgroupStats(cgDir string) cmdCgroupStats {
	if cgDir == "" {
		return cmdCgroupStats{}
	}
	var st cmdCgroupStats
	if f, err := os.Open(filepath.Join(cgDir, "memory.stat")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), " ")
			if ok && k == "anon" {
				st.memAnon, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				break
			}
		}
		_ = f.Close()
	}
	st.memSwap = readCgroupInt64(filepath.Join(cgDir, "memory.swap.current"))
	st.memPeak = readCgroupInt64(filepath.Join(cgDir, "memory.peak"))
	st.swapPeak = readCgroupInt64(filepath.Join(cgDir, "memory.swap.peak"))
	if f, err := os.Open(filepath.Join(cgDir, "cpu.stat")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), " ")
			if ok && k == "usage_usec" {
				st.cpuUsec, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				break
			}
		}
		_ = f.Close()
	}
	if b, err := os.ReadFile(filepath.Join(cgDir, "memory.events")); err == nil {
		st.oomKilled = sysinfo.ParseMemoryEventsOOM(string(b))
	}
	return st
}

func readCgroupInt64(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func exitStatusOf(err error) (exitCode, sig int) {
	if err == nil {
		return 0, 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				s := int(ws.Signal())
				return 128 + s, s
			}
			return ws.ExitStatus(), 0
		}
		return ee.ExitCode(), 0
	}
	return 1, 0
}

func classifyExitResult(exitCode, sig int, oomKilled bool) string {
	switch {
	case oomKilled:
		return "oom-kill"
	case sig != 0:
		return "signal"
	case exitCode != 0:
		return "exit-code"
	default:
		return "success"
	}
}
