//go:build linux

package main

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestPID1MountFlags(t *testing.T) {
	byTarget := map[string]pid1Mount{}
	for _, m := range pid1Mounts {
		byTarget[m.target] = m
	}

	shm, ok := byTarget["/dev/shm"]
	if !ok {
		t.Fatal("pid1Mounts missing /dev/shm")
	}
	wantShmFlags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
	if shm.flags&wantShmFlags != wantShmFlags {
		t.Errorf("/dev/shm flags = %#x, want MS_NOSUID|MS_NODEV (%#x)", shm.flags, wantShmFlags)
	}
	if shm.flags&syscall.MS_NOEXEC != 0 {
		t.Errorf("/dev/shm must not set MS_NOEXEC (flags=%#x)", shm.flags)
	}

	run, ok := byTarget["/run"]
	if !ok {
		t.Fatal("pid1Mounts missing /run")
	}
	if run.flags&wantShmFlags != wantShmFlags {
		t.Errorf("/run flags = %#x, want MS_NOSUID|MS_NODEV (%#x)", run.flags, wantShmFlags)
	}
	if run.flags&syscall.MS_NOEXEC != 0 {
		t.Errorf("/run must not set MS_NOEXEC so /run/hotbed/git-auth helpers stay executable (flags=%#x)", run.flags)
	}

	if _, hasTmp := byTarget["/tmp"]; hasTmp {
		t.Errorf("/tmp must stay on cladefs rootfs (not tmpfs) for O_DIRECT and disk capacity")
	}
	if m, ok := byTarget["/sys/kernel/tracing"]; !ok || m.fstype != "tracefs" {
		t.Errorf("pid1Mounts missing tracefs at /sys/kernel/tracing: %+v", m)
	}
	if m, ok := byTarget["/sys/fs/bpf"]; !ok || m.fstype != "bpf" {
		t.Errorf("pid1Mounts missing bpf at /sys/fs/bpf: %+v", m)
	}
}

func TestPrepareGuestTmpDirs(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "tmp", "stale-dir", "file")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	varKeep := filepath.Join(root, "var", "tmp", "keep")
	if err := os.MkdirAll(filepath.Dir(varKeep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(varKeep, []byte("persist"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prepareGuestTmpDirs(slog.New(slog.NewTextHandler(&logs, nil)), root, true)
	if logs.Len() != 0 {
		t.Fatalf("unexpected warnings: %s", logs.String())
	}

	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("/tmp still has stale entries: %v", entries)
	}
	if _, err := os.Stat(varKeep); err != nil {
		t.Errorf("/var/tmp file should persist across boot: %v", err)
	}
	wantMode := os.ModePerm | os.ModeSticky
	for _, rel := range []string{"tmp", "var/tmp"} {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (os.ModePerm | os.ModeSticky); got != wantMode {
			t.Errorf("%s mode = %v, want %v", rel, got, wantMode)
		}
	}
}

func TestEnsureBootRuntimeDirs(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	passwdBody := strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin",
		"ramon:x:1000:1000::/work/.hotbed/home:/bin/bash",
		"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin",
	}, "\n") + "\n"
	if err := os.WriteFile(passwd, []byte(passwdBody), 0o644); err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(dir, "run")

	var logs bytes.Buffer
	ensureBootRuntimeDirs(slog.New(slog.NewTextHandler(&logs, nil)), passwd, runRoot)
	if logs.Len() != 0 {
		t.Fatalf("unexpected warnings: %s", logs.String())
	}

	u1000 := filepath.Join(runRoot, "user", "1000")
	info, err := os.Stat(u1000)
	if err != nil {
		t.Fatalf("stat %s: %v", u1000, err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("%s perm = %04o, want 0700", u1000, got)
	}
	for _, skip := range []string{"0", "1", "65534"} {
		if _, err := os.Stat(filepath.Join(runRoot, "user", skip)); !os.IsNotExist(err) {
			t.Errorf("unexpected runtime dir created for uid %s", skip)
		}
	}
}

func TestApplyNofileLimitAndChildInheritance(t *testing.T) {
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &orig)
	})

	// Pick a target below orig.Max (or 65536 when running as root) so the test
	// works both as unprivileged user and as root while still changing Cur.
	target := defaultGuestNofile
	if os.Geteuid() != 0 && target > orig.Max {
		target = orig.Max
	}
	if target < 256 {
		t.Skipf("hard RLIMIT_NOFILE %d too small for test", orig.Max)
	}

	applied, err := applyNofileLimit(nil, target)
	if err != nil {
		t.Fatalf("applyNofileLimit(%d): %v", target, err)
	}
	if applied != target {
		t.Fatalf("applied = %d, want %d", applied, target)
	}

	// Verify a spawned child process inherits BOTH soft and hard limits
	// (guarding against Go 1.19+'s origRlimitNofile reset in forkAndExecInChild).
	out, err := exec.Command("/bin/sh", "-c", "printf '%s %s' \"$(ulimit -Sn)\" \"$(ulimit -Hn)\"").CombinedOutput()
	if err != nil {
		t.Fatalf("child ulimit check failed: %v (%s)", err, out)
	}
	parts := strings.Fields(string(out))
	if len(parts) != 2 {
		t.Fatalf("unexpected ulimit output %q", string(out))
	}
	soft, err1 := strconv.ParseUint(parts[0], 10, 64)
	hard, err2 := strconv.ParseUint(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("parse child ulimit %q: %v %v", string(out), err1, err2)
	}
	if soft != target || hard != target {
		t.Fatalf("child inherited RLIMIT_NOFILE soft=%d hard=%d, want %d:%d", soft, hard, target, target)
	}
}
