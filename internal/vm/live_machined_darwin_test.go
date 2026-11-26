//go:build darwin

package vm_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	machinedv1 "github.com/semistrict/cladefs/go/machined/v1"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/vm"
)

// TestLiveMachinedEndToEnd boots a workspace VM on a real machined (Apple's
// Hypervisor.framework) the way a macOS worker does: hotbed-guestd from an
// initramfs, switching root onto the workspace's cell of machined's embedded
// cladefs daemon, reached over machined's vsock socket; then takes the VM back
// in a second Manager, as a restarted worker would, and stops it. Set
// HOTBED_LIVE_MACHINED to a machined binary (machined-vm beside it, both
// signed with the hypervisor entitlement) and HOTBED_LIVE_KERNEL_PATH to
// cladefs's arm64 guest Image.
func TestLiveMachinedEndToEnd(t *testing.T) {
	machined := os.Getenv("HOTBED_LIVE_MACHINED")
	kernelSrc := os.Getenv("HOTBED_LIVE_KERNEL_PATH")
	if machined == "" || kernelSrc == "" {
		t.Skip("set HOTBED_LIVE_MACHINED and HOTBED_LIVE_KERNEL_PATH to run a live machined VM")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	// Short: machined's vsock sockets go under its state directory.
	root, err := os.MkdirTemp("/tmp", "hbm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	images, state, cells := filepath.Join(root, "images"), filepath.Join(root, "state"), filepath.Join(root, "cells")
	for _, d := range []string{images, cells} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kernel := filepath.Join(images, "Image")
	if err := os.Link(kernelSrc, kernel); err != nil {
		t.Fatalf("link the kernel into machined's images: %v", err)
	}
	guestdBin := filepath.Join(root, "hotbed-guestd")
	build := exec.CommandContext(ctx, "go", "build", "-o", guestdBin, "github.com/semistrict/hotbed/cmd/hotbed-guestd")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hotbed-guestd: %v\n%s", err, out)
	}

	socket := filepath.Join(root, "machined.sock")
	daemon := exec.Command(machined, "--socket", socket, "--images", images, "--state", state)
	var daemonLog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &daemonLog, &daemonLog
	if err := daemon.Start(); err != nil {
		t.Fatalf("start machined: %v", err)
	}
	t.Cleanup(func() {
		_ = daemon.Process.Signal(os.Interrupt)
		_ = daemon.Wait()
		if t.Failed() {
			t.Logf("machined's log:\n%s", daemonLog.String())
		}
	})
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("machined never served %s:\n%s", socket, daemonLog.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

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
