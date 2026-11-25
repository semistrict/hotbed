//go:build linux

package vm_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/vm"
)

// TestLiveFirecrackerCladefsEndToEnd exercises the full Step 3 stack on a real
// Linux host with cladefsd + Firecracker + KVM + nftables when
// HOTBED_LIVE_VM_TEST=1 is set (skipped in normal CI/unit runs).
func TestLiveFirecrackerCladefsEndToEnd(t *testing.T) {
	if os.Getenv("HOTBED_LIVE_VM_TEST") != "1" {
		t.Skip("set HOTBED_LIVE_VM_TEST=1 on a cladefsd + KVM host to run live Firecracker test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cellRoot := os.Getenv("HOTBED_LIVE_CELL_ROOT")
	if cellRoot == "" {
		cellRoot = "/mnt/fxdax-scratch/cladefsd/cells/hotbed-live-test"
	}
	runDir := filepath.Join("/run/hotbed-live-vm-test")
	_ = os.RemoveAll(runDir)
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	store, err := cladefs.Open(ctx, cladefs.Config{
		ControlSock: cladefs.DefaultControlSock,
		VMSock:      cladefs.DefaultVMSock,
		CellRoot:    cellRoot,
		DefaultCap:  4 << 30,
	})
	if err != nil {
		t.Fatalf("cladefs.Open: %v", err)
	}
	defer store.Close()

	// Clean up any leftovers from an interrupted previous run.
	_ = store.DeleteCell(ctx, "ws-live-1")
	_ = store.DeleteCell(ctx, "base-live")
	t.Cleanup(func() {
		cctx, cc := context.WithTimeout(context.Background(), 15*time.Second)
		defer cc()
		_ = store.DeleteCell(cctx, "ws-live-1")
		_ = store.DeleteCell(cctx, "base-live")
	})

	// 1. Build clean rootfs tar directly from OCI registry (no Docker, no baked-in hotbed-guestd).
	tarPath := filepath.Join(cellRoot, "debian-guestd-live.tar")
	t.Cleanup(func() { _ = os.Remove(tarPath) })
	if err := vm.BuildGuestTar(ctx, vm.GuestImageOptions{
		ImageRef:   "public.ecr.aws/debian/debian:trixie-slim",
		Arch:       "amd64",
		DNSServers: []string{"1.1.1.1", "8.8.8.8"},
		OutTar:     tarPath,
	}); err != nil {
		t.Fatalf("BuildGuestTar: %v", err)
	}

	// 2. Import into cladefsd, snapshot version, sync to R2, and O(1) fork workspace cell.
	baseRef, err := store.ImportImage(ctx, "base-live", tarPath, 4<<30)
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}
	t.Logf("imported base cell=%d/%d ver=%d", baseRef.Tenant, baseRef.Cell, baseRef.Version)

	wsRef, err := store.ForkVersion(ctx, baseRef, "ws-live-1", false)
	if err != nil {
		t.Fatalf("ForkVersion: %v", err)
	}
	t.Logf("forked workspace cell=%d/%d at %s", wsRef.Tenant, wsRef.Cell, wsRef.HostPath)

	// Fake host git-auth Unix socket to verify guest->host vsock 5002 bridging.
	gaSockDir, err := os.MkdirTemp("/tmp", "hb-live-ga-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(gaSockDir) })
	gaSock := filepath.Join(gaSockDir, "git-cred.sock")
	gaLn, err := net.Listen("unix", gaSock)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(gaSock, 0o666)
	t.Cleanup(func() { _ = gaLn.Close() })
	go func() {
		for {
			c, err := gaLn.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("LIVE_GITAUTH_VSOCK_OK\n"))
			_ = c.Close()
		}
	}()

	// 3. Boot Firecracker in a transient hotbed.slice systemd unit with TAP/NAT networking.
	vmCfg := vm.Config{
		GuestdBin:          os.Getenv("HOTBED_LIVE_GUESTD_BIN"),
		RunDir:             runDir,
		Slice:              vm.DefaultSlice,
		VMUser:             vm.DefaultVMUser,
		DefaultVCPUs:       2,
		DefaultMemMiB:      512,
		DefaultDiskBytes:   4 << 30,
		DAXWindowBytes:     1 << 30,
		DAXPrivateMaxBytes: 256 << 20,
		GitAuthHostSock:    gaSock,
		BootTimeout:        25 * time.Second,
		ShutdownTimeout:    8 * time.Second,
	}
	mgr, err := vm.NewManager(vmCfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() {
		sctx, sc := context.WithTimeout(context.Background(), 10*time.Second)
		defer sc()
		_ = mgr.Stop(sctx, "ws-live-1", true)
	})

	inst, err := mgr.Start(ctx, vm.LaunchSpec{
		ID:            "ws-live-1",
		CellHostPath:  wsRef.HostPath,
		CladefsVMSock: store.VMSock(),
	})
	if err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	t.Logf("VM booted: unit=%s cid=%d tap=%s ip=%s", inst.UnitName, inst.GuestCID, inst.Net.TAPName, inst.Net.GuestIP)

	// 4. Verify health, tools-disk pivot, cgroup placement, network interface, and freeze/thaw via GuestRuntime.
	rt := runtime.NewGuestRuntime(&liveResolver{mgr: mgr})
	if err := rt.StartCommand(ctx, "ws-live-1", runtime.CommandSpec{
		ID:   "live-check",
		Argv: []string{"/bin/sh", "-c", "cat /proc/self/cgroup && test -x /run/hotbed-tools/hotbed-guestd && test ! -e /sbin/hotbed-guestd && grep -F '" + inst.Net.GuestIP + "' /proc/net/fib_trie && echo LIVE_STEP3_OK"},
		Dir:  "/work",
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	var out bytes.Buffer
	if _, err := rt.StreamLogs(ctx, "ws-live-1", "live-check", 0, true, &out); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	t.Logf("command output:\n%s", out.String())
	if !strings.Contains(out.String(), "0::/hotbed/live-check") {
		t.Fatalf("expected cgroup /hotbed/live-check, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), inst.Net.GuestIP) {
		t.Fatalf("expected guest IP %s on eth0, got:\n%s", inst.Net.GuestIP, out.String())
	}
	if !strings.Contains(out.String(), "LIVE_STEP3_OK") {
		t.Fatalf("missing LIVE_STEP3_OK")
	}

	// Start a long-lived sleeper, freeze/thaw it inside guest cgroup v2, then simulate worker restart!
	if err := rt.StartCommand(ctx, "ws-live-1", runtime.CommandSpec{
		ID:   "live-sleeper",
		Argv: []string{"/bin/sh", "-c", "echo SURVIVED_START; sleep 60"},
		Dir:  "/work",
	}); err != nil {
		t.Fatalf("StartCommand sleeper: %v", err)
	}
	if err := rt.FreezeCommand(ctx, "ws-live-1", "live-sleeper"); err != nil {
		t.Fatalf("FreezeCommand: %v", err)
	}
	if err := rt.ThawCommand(ctx, "ws-live-1", "live-sleeper"); err != nil {
		t.Fatalf("ThawCommand: %v", err)
	}

	// 5. Simulate hotbed-worker restart: fresh Manager reconnects to surviving Firecracker systemd unit!
	mgr2, err := vm.NewManager(vmCfg)
	if err != nil {
		t.Fatalf("NewManager 2: %v", err)
	}
	n, err := mgr2.Reconnect(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Reconnect = (%d, %v), want (1, nil)", n, err)
	}
	rt2 := runtime.NewGuestRuntime(&liveResolver{mgr: mgr2})
	stMap, err := rt2.StatusMany(ctx, "ws-live-1", []string{"live-sleeper"})
	if err != nil || !stMap["live-sleeper"].Running {
		t.Fatalf("sleeper did not survive worker reconnect: %+v, err=%v", stMap, err)
	}
	if err := rt2.StopCommand(ctx, "ws-live-1", "live-sleeper", 3*time.Second); err != nil {
		t.Fatalf("StopCommand after reconnect: %v", err)
	}

	// 6. Clean VM shutdown + cell delete.
	if err := mgr2.Stop(ctx, "ws-live-1", true); err != nil {
		t.Fatalf("mgr2.Stop: %v", err)
	}
	if err := store.DeleteCell(ctx, "ws-live-1"); err != nil {
		t.Fatalf("DeleteCell ws-live-1: %v", err)
	}
	if err := store.DeleteCell(ctx, "base-live"); err != nil {
		t.Fatalf("DeleteCell base-live: %v", err)
	}
}

type liveResolver struct{ mgr *vm.Manager }

func (r *liveResolver) EnsureVM(_ context.Context, wsID string) (*guestd.Client, error) {
	return r.mgr.ClientFor(wsID)
}
func (r *liveResolver) ClientFor(wsID string) (*guestd.Client, error) {
	return r.mgr.ClientFor(wsID)
}
func (r *liveResolver) StopVM(ctx context.Context, wsID string) error {
	return r.mgr.StopVM(ctx, wsID)
}
