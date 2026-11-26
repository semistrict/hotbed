//go:build linux

package guestd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Regression test for the os.WriteFile + EAGAIN epoll-spin bug on cgroup v2
// memory.reclaim: a raw syscall write to a non-blocking fd that immediately
// returns EAGAIN must return nil at once instead of blocking/spinning in Go's
// runtime netpoller until the HTTP client's 3s deadline expires.
func TestWriteCgroupReclaimReturnsImmediatelyOnEAGAIN(t *testing.T) {
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		t.Fatalf("Pipe2: %v", err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	// Fill the pipe buffer so the next write deterministically returns EAGAIN.
	chunk := make([]byte, 4096)
	for {
		_, err := syscall.Write(fds[1], chunk)
		if err == syscall.EAGAIN {
			break
		}
		if err != nil {
			t.Fatalf("fill pipe: %v", err)
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- writeCgroupReclaimFD(fds[1], "536870912")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writeCgroupReclaimFD on EAGAIN = %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("writeCgroupReclaimFD blocked on EAGAIN instead of returning immediately")
	}

	// Regular file path writes payload cleanly, and missing path returns nil.
	p := filepath.Join(t.TempDir(), "memory.reclaim")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeCgroupReclaim(p, "4096 swappiness=0"); err != nil {
		t.Fatalf("writeCgroupReclaim(file): %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "4096 swappiness=0" {
		t.Fatalf("wrote %q, want %q", string(got), "4096 swappiness=0")
	}
	if err := writeCgroupReclaim(filepath.Join(t.TempDir(), "missing"), "4096"); err != nil {
		t.Fatalf("writeCgroupReclaim(missing) = %v, want nil", err)
	}
}

func TestWriteProcVM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "compact_memory")
	if err := writeProcVM(p, "1\n"); err != nil {
		t.Fatalf("writeProcVM: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "1\n" {
		t.Fatalf("wrote %q, want %q", string(got), "1\n")
	}
	if err := writeProcVM(filepath.Join(t.TempDir(), "no-such-dir", "compact_memory"), "1\n"); err != nil {
		t.Fatalf("writeProcVM(missing) = %v, want nil", err)
	}
}
