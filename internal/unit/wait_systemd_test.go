//go:build linux && systemdtest

package unit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A real cgroup that empties and is removed at once, as systemd removes an
// OOM-killed unit's, ends the wait though the kernel never says it emptied:
// it sends at most one cgroup.events notification per 10ms and puts a later
// one off, and removing the cgroup drops one put off. Without waitRecheck
// this hung in about half of its runs (TestWaitEmptiedSeesACgroupGoneWithoutAnEvent
// stands in for it where there is no root).
func TestWaitEndsWhenTheCgroupIsRemovedUnannounced(t *testing.T) {
	parent := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("hotbed-wait-test-%d", time.Now().UnixNano()))
	dir := filepath.Join(parent, "unit")
	for _, d := range []string{parent, dir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = os.Remove(dir)
		_ = os.Remove(parent)
	})
	cg, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cg.Close()
	sleeper := exec.Command("sleep", "60")
	sleeper.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd())}
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Wait() }()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- waitEmptied(ctx, filepath.Join(dir, "cgroup.events")) }()
	// Once the waiter watches, the freeze below is announced, and the empty
	// that follows it within 10ms is put off.
	for !watched(t, filepath.Join(dir, "cgroup.events")) {
		select {
		case err := <-waited:
			t.Fatalf("waitEmptied returned %v before the cgroup emptied", err)
		default:
		}
	}
	write := func(f, v string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	write("cgroup.freeze", "1")
	write("cgroup.kill", "1")
	for {
		if p, err := populated(filepath.Join(dir, "cgroup.events")); err != nil {
			t.Fatal(err)
		} else if !p {
			break
		}
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("waitEmptied: %v, want it to end once the cgroup was removed", err)
	}
}

// watched says whether an inotify descriptor of this process watches path.
func watched(t *testing.T, path string) bool {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	ino := fmt.Sprintf(" ino:%x ", st.Ino)
	fds, err := os.ReadDir("/proc/self/fdinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		b, err := os.ReadFile(filepath.Join("/proc/self/fdinfo", fd.Name()))
		if err != nil {
			continue // closed since it was listed
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "inotify ") && strings.Contains(line+" ", ino) {
				return true
			}
		}
	}
	return false
}
