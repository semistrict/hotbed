package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/semistrict/hotbed/internal/update"
)

func TestWorkerInitParsesFlags(t *testing.T) {
	t.Parallel()
	var c cli
	parser, err := kong.New(&c, kong.Name("hotbed"))
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	kctx, err := parser.Parse([]string{
		"--json", "worker", "init", "https://hotbed.example.com",
		"--dir", "/var/lib/hotbed",
		"--user", "alice",
		"--hostname", "linux-worker",
		"--slice", "work.slice",
		"--unit-property", "DeviceAllow=/dev/kvm",
		"--no-start",
	})
	if err != nil {
		t.Fatalf("kong.Parse: %v", err)
	}
	if kctx.Command() != "worker init <controller>" || !c.JSON {
		t.Fatalf("got command=%q json=%v, want 'worker init <controller>' json=true", kctx.Command(), c.JSON)
	}
	initCmd := c.Worker.Init
	if initCmd.Controller != "https://hotbed.example.com" ||
		initCmd.Dir != "/var/lib/hotbed" ||
		initCmd.User != "alice" ||
		initCmd.Hostname != "linux-worker" ||
		initCmd.Slice != "work.slice" ||
		!slices.Equal(initCmd.UnitProperties, []string{"DeviceAllow=/dev/kvm"}) ||
		!initCmd.NoStart {
		t.Fatalf("parsed workerInitCmd = %+v", initCmd)
	}
}

// The daemon exits for a new build only when its binary's content changed,
// not when it is told to stop (launchctl bootout) or when another process
// atomically rewrites the binary with the same build.
func TestReplacedAtTellsReplacementFromStopping(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "hotbed")
	if err := os.WriteFile(bin, []byte("build 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	was, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	wasSum, err := update.FileSHA256(bin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if replacedAt(ctx, bin, was, wasSum, time.Millisecond) {
		t.Fatal("stopped, it says its binary was replaced")
	}

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- replacedAt(ctx, bin, was, wasSum, 10*time.Millisecond) }()

	// Atomically rewriting the same bytes (new inode + mtime) must not trigger a restart.
	same := bin + ".same"
	if err := os.WriteFile(same, []byte("build 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(same, bin); err != nil {
		t.Fatal(err)
	}
	select {
	case replaced := <-done:
		t.Fatalf("same-content rewrite triggered replacedAt=%v", replaced)
	case <-time.After(80 * time.Millisecond):
	}

	next := bin + ".new"
	if err := os.WriteFile(next, []byte("build 2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, bin); err != nil {
		t.Fatal(err)
	}
	select {
	case replaced := <-done:
		if !replaced {
			t.Fatal("its binary replaced, it says it was not")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("its binary replaced, it did not notice")
	}
}
