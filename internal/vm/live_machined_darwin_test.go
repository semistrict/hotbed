//go:build darwin

package vm_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	machinedv1 "github.com/semistrict/cladefs/go/machined/v1"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/machinedtest"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/vm"
)

// TestLiveMachinedEndToEnd boots a workspace VM on a real machined (Apple's
// Hypervisor.framework) the way a macOS worker does: hotbed-guestd from an
// initramfs, switching root onto the workspace's cell of machined's embedded
// cladefs daemon, reached over machined's vsock socket; then takes the VM back
// in a second Manager, as a restarted worker would, and stops it
// (machinedtest.Start says what it needs).
func TestLiveMachinedEndToEnd(t *testing.T) {
	m := machinedtest.Start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	socket, images, kernel, guestdBin, root := m.Socket, m.Images, m.Kernel, m.GuestdBin, m.Dir
	cells := filepath.Join(root, "cells")

	store, err := cladefs.Open(ctx, cladefs.Config{
		ControlSock: socket,
		VMSock:      socket,
		CellRoot:    cells,
		DefaultCap:  4 << 30,
	})
	if err != nil {
		t.Fatalf("cladefs.Open on machined: %v", err)
	}
	defer store.Close()

	tarPath := filepath.Join(cells, "debian.tar")
	if err := vm.BuildGuestTar(ctx, vm.GuestImageOptions{
		ImageRef:   "public.ecr.aws/debian/debian:trixie-slim",
		Arch:       "arm64",
		DNSServers: []string{vm.MachinedGateway},
		OutTar:     tarPath,
	}); err != nil {
		t.Fatalf("BuildGuestTar: %v", err)
	}
	baseRef, err := store.ImportImage(ctx, tarPath, 4<<30)
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}
	wsRef, err := store.ForkVersion(ctx, baseRef, false)
	if err != nil {
		t.Fatalf("ForkVersion: %v", err)
	}

	newManager := func() *vm.Manager {
		t.Helper()
		conn, err := vm.DialMachined(socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		mgr, err := vm.NewManager(vm.Config{
			KernelPath:      kernel,
			GuestdBin:       guestdBin,
			RunDir:          filepath.Join(root, "run"),
			DefaultVCPUs:    2,
			DefaultMemMiB:   1024,
			DisableDAX:      true,
			BootTimeout:     60 * time.Second,
			ShutdownTimeout: 8 * time.Second,
		}, vm.WithVMM(vm.NewMachined(machinedv1.NewMachinedClient(conn), images)))
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return mgr
	}
	mgr := newManager()
	inst, err := mgr.Start(ctx, vm.LaunchSpec{ID: "ws-live-1", CellHostPath: wsRef.CellID()})
	if err != nil {
		t.Fatalf("mgr.Start: %v\nconsole:\n%s", err, readFile(filepath.Join(root, "run", "ws-live-1", "console.log")))
	}
	t.Logf("VM booted in %s: unit=%s vsock=%s", inst.Boot, inst.UnitName, inst.VsockUDS)

	// hotbed-guestd runs off the initramfs on the workspace's root, in its
	// command's cgroup, and resolves names through machined.
	rt := runtime.NewGuestRuntime(&liveResolver{mgr: mgr})
	run := func(rt *runtime.GuestRuntime, id, script string) string {
		t.Helper()
		if err := rt.StartCommand(ctx, "ws-live-1", runtime.CommandSpec{ID: id, Argv: []string{"/bin/sh", "-c", script}, Dir: "/work"}); err != nil {
			t.Fatalf("StartCommand %s: %v", id, err)
		}
		var out bytes.Buffer
		if _, err := rt.StreamLogs(ctx, "ws-live-1", id, 0, true, &out); err != nil {
			t.Fatalf("StreamLogs %s: %v", id, err)
		}
		return out.String()
	}
	out := run(rt, "live-check", "cat /proc/self/cgroup; test -x /run/hotbed-tools/hotbed-guestd && test ! -e /sbin/hotbed-guestd && grep -q trixie /etc/os-release && getent hosts deb.debian.org >/dev/null && echo LIVE_MACHINED_OK")
	if !strings.Contains(out, "0::/hotbed/live-check") || !strings.Contains(out, "LIVE_MACHINED_OK") {
		t.Fatalf("command output:\n%s\nwant cgroup /hotbed/live-check and LIVE_MACHINED_OK", out)
	}

	// Nested virtualization (issue 33): the guest's kernel boots at EL2 and
	// has KVM, so Firecracker runs in a Mac's workspaces. Without it, dmesg
	// says "CPU: All CPU(s) started at EL1" and "HYP mode not available".
	out = run(rt, "live-kvm", "test -c /dev/kvm && echo KVM_OK || dmesg | grep -E 'CPU: All|kvm'")
	if !strings.Contains(out, "KVM_OK") {
		t.Fatalf("command output:\n%s\nwant KVM_OK: /dev/kvm in the guest", out)
	}

	// The guest's clock is the host's (the board's RTC), not 1970's: TLS and
	// signed package indexes need it.
	out = run(rt, "live-clock", "date +%s")
	guest, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("guest date: %q: %v", out, err)
	}
	if skew := time.Since(time.Unix(guest, 0)); skew < -time.Minute || skew > time.Minute {
		t.Fatalf("guest clock %s is %s off the host's", time.Unix(guest, 0).UTC(), skew)
	}

	if err := rt.StartCommand(ctx, "ws-live-1", runtime.CommandSpec{ID: "live-sleeper", Argv: []string{"sleep", "120"}, Dir: "/work"}); err != nil {
		t.Fatalf("StartCommand sleeper: %v", err)
	}

	// A restarted worker takes the VM back, its command still running.
	mgr2 := newManager()
	n, err := mgr2.Reconnect(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Reconnect = (%d, %v), want (1, nil)", n, err)
	}
	rt2 := runtime.NewGuestRuntime(&liveResolver{mgr: mgr2})
	st, err := rt2.StatusMany(ctx, "ws-live-1", []string{"live-sleeper"})
	if err != nil || !st["live-sleeper"].Running {
		t.Fatalf("sleeper after reconnect: %+v, err=%v", st, err)
	}

	if err := mgr2.Stop(ctx, "ws-live-1", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got, err := mgr2.Status(ctx, "ws-live-1"); err != nil || got.Loaded {
		t.Fatalf("Status after Stop = %+v, %v; want none", got, err)
	}
	// Its cell is closed: it deletes.
	if err := store.DeleteCell(ctx, wsRef.Tenant, wsRef.Cell); err != nil {
		t.Fatalf("DeleteCell: %v", err)
	}
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
