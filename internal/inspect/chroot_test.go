package inspect

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLookToolInRoot(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite("usr/bin/strace", 0o755)
	mustWrite("usr/bin/perf_6.12", 0o755)
	if err := os.Symlink("/usr/bin/perf_6.12", filepath.Join(root, "usr/bin/perf")); err != nil {
		t.Fatal(err)
	}
	mustWrite("usr/local/bin/bpftrace-real", 0o755)
	if err := os.Symlink("bpftrace-real", filepath.Join(root, "usr/local/bin/bpftrace")); err != nil {
		t.Fatal(err)
	}
	mustWrite("usr/bin/not-exec", 0o644)
	// Absolute symlink pointing to a host binary that does NOT exist inside root
	// must NOT resolve against the host filesystem.
	if err := os.Symlink("/bin/sh", filepath.Join(root, "usr/bin/host-escape")); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"strace":   "/usr/bin/strace",
		"perf":     "/usr/bin/perf",
		"bpftrace": "/usr/local/bin/bpftrace",
	} {
		got, err := lookToolInRoot(root, name)
		if err != nil || got != want {
			t.Errorf("lookToolInRoot(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, missing := range []string{"not-exec", "gdb", "host-escape"} {
		if got, err := lookToolInRoot(root, missing); err == nil {
			t.Errorf("lookToolInRoot(%q) unexpectedly succeeded: %q", missing, got)
		}
	}
}

func TestChrootCmdWrapping(t *testing.T) {
	name, args := chrootCmd("", "", "/usr/bin/strace", []string{"-p", "123"})
	if name != "/usr/bin/strace" || !slices.Equal(args, []string{"-p", "123"}) {
		t.Fatalf("passthrough got %q %v", name, args)
	}

	name, args = chrootCmd("/var/lib/hotbed/vol/ws/w1", "/tmp/perf-1", "/usr/bin/perf", []string{"stat", "-p", "123"})
	if name != "unshare" {
		t.Fatalf("wrapped command = %q, want unshare", name)
	}
	for _, want := range []string{"--mount", "--propagation", "slave", "/var/lib/hotbed/vol/ws/w1", "/tmp/perf-1", "/usr/bin/perf", "stat", "123"} {
		if !slices.Contains(args, want) {
			t.Errorf("wrapped args missing %q: %v", want, args)
		}
	}
	for _, want := range []string{`HOME="/nonexistent"`, `XDG_CONFIG_HOME="/nonexistent"`} {
		if !strings.Contains(chrootExecScript, want) {
			t.Errorf("chrootExecScript missing %s", want)
		}
	}
}
