package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/alecthomas/kong"
)

func TestWorkerInitParsesZFSDeviceFlags(t *testing.T) {
	var c cli
	parser, err := kong.New(&c, kong.Name("hotbed"))
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	kctx, err := parser.Parse([]string{
		"--json", "worker", "init", "ctrl.ts.net:7120",
		"--dir", "/var/lib/hotbed",
		"--user", "alice",
		"--zfs-device", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_hotbed-zfs",
		"--hostname", "mac-worker",
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
	if initCmd.Controller != "ctrl.ts.net:7120" ||
		initCmd.Dir != "/var/lib/hotbed" ||
		initCmd.User != "alice" ||
		initCmd.ZFSDevice != "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_hotbed-zfs" ||
		initCmd.TrimFS ||
		initCmd.Hostname != "mac-worker" ||
		initCmd.Slice != "work.slice" ||
		!slices.Equal(initCmd.UnitProperties, []string{"DeviceAllow=/dev/kvm"}) ||
		!initCmd.NoStart {
		t.Fatalf("parsed workerInitCmd = %+v", initCmd)
	}

	var c2 cli
	parser2, err := kong.New(&c2, kong.Name("hotbed"))
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser2.Parse([]string{
		"--json", "worker", "init", "ctrl.ts.net:7120",
		"--zfs-device", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_hotbed-zfs",
		"--fstrim",
		"--zfs-arc-max", "3221225472",
	}); err != nil {
		t.Fatalf("kong.Parse(--fstrim --zfs-arc-max): %v", err)
	}
	if !c2.Worker.Init.TrimFS {
		t.Fatalf("expected TrimFS=true when --fstrim is passed")
	}
	if c2.Worker.Init.ZFSARCMax != "3221225472" {
		t.Fatalf("ZFSARCMax = %q, want %q", c2.Worker.Init.ZFSARCMax, "3221225472")
	}
}

// The supervisor exits for a new build only when its binary was replaced,
// not when it is told to stop (launchctl bootout).
func TestReplacedAtTellsReplacementFromStopping(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "hotbed")
	if err := os.WriteFile(bin, []byte("build 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	was, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if replacedAt(ctx, bin, was, time.Millisecond) {
		t.Fatal("stopped, it says its binary was replaced")
	}

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- replacedAt(ctx, bin, was, 10*time.Millisecond) }()
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
