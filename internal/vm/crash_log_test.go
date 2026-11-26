package vm

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/guestd"
)

func TestProcessStatusDiedAbnormally(t *testing.T) {
	cases := []struct {
		name string
		st   ProcessStatus
		want bool
	}{
		{"running", ProcessStatus{Loaded: true, Running: true, Result: "success"}, false},
		{"clean exit 0", ProcessStatus{Loaded: true, Exited: true, ExitCode: 0, Result: "success"}, false},
		{"not loaded", ProcessStatus{}, false},
		{"sigkill", ProcessStatus{Loaded: true, Exited: true, Signal: 9, Result: "signal"}, true},
		{"oom-kill", ProcessStatus{Loaded: true, Exited: true, Signal: 9, Result: "oom-kill", OOMKilled: true}, true},
		{"exit 158", ProcessStatus{Loaded: true, Exited: true, ExitCode: 158, Result: "exit-code"}, true},
		{"core-dump", ProcessStatus{Loaded: true, Exited: true, Signal: 11, Result: "core-dump"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.st.DiedAbnormally(); got != tc.want {
				t.Fatalf("DiedAbnormally(%+v) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}

func TestManagerStopPreservesLogsOnSignalDeath(t *testing.T) {
	ctx := context.Background()
	runDir := shortSockDir(t)
	sockDir := shortSockDir(t)
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := guestd.NewServer(guestd.ServerConfig{WorkDir: t.TempDir(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ServeControl(ctx, ln) }()

	fl := newFakeLauncher()
	fl.onStart = func(s ProcessSpec) error {
		_ = os.WriteFile(s.ConsoleLog, []byte("guest kernel boot\n"), 0o644)
		for i := 0; i < len(s.Args)-1; i++ {
			if s.Args[i] == "--log-path" {
				_ = os.WriteFile(s.Args[i+1], []byte("WARN cladefs_memory: host socket stalled\n"), 0o644)
			}
		}
		return nil
	}
	dialerFactory := func(_ string) guestd.DialFunc {
		return guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock})
	}
	mgr, err := NewManager(Config{
		RunDir:          runDir,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 500 * time.Millisecond,
	}, WithLauncher(fl), WithDialerFactory(dialerFactory), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	inst, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-sigkill",
		CellHostPath:  "/mnt/cells/ws-sigkill",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.FirecrackerLog == "" {
		t.Fatal("expected FirecrackerLog path set on Instance")
	}

	// Simulate cladefsd SIGKILLing Firecracker: vsock closes and systemd reports signal=9.
	_ = ln.Close()
	fl.mu.Lock()
	delete(fl.running, inst.UnitName)
	fl.customStatus[inst.UnitName] = ProcessStatus{
		Loaded: true,
		Exited: true,
		Signal: int(syscall.SIGKILL),
		Result: "signal",
	}
	fl.mu.Unlock()

	if err := mgr.Stop(ctx, "ws-sigkill", true); err == nil {
		t.Fatal("expected Stop to return guest shutdown/sync error after SIGKILL")
	}
	if _, err := os.Stat(inst.Dir); !os.IsNotExist(err) {
		t.Fatalf("expected active vmDir %s removed after Stop", inst.Dir)
	}

	crashRoot := filepath.Join(runDir, crashedDirName)
	entries, err := os.ReadDir(crashRoot)
	if err != nil {
		t.Fatalf("read %s: %v", crashRoot, err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "-ws-sigkill") {
		t.Fatalf("unexpected crash dirs: %+v", entries)
	}
	crashDir := filepath.Join(crashRoot, entries[0].Name())

	fcBytes, err := os.ReadFile(filepath.Join(crashDir, "firecracker.log"))
	if err != nil || !strings.Contains(string(fcBytes), "host socket stalled") {
		t.Fatalf("preserved firecracker.log = (%q, %v)", string(fcBytes), err)
	}
	consoleBytes, err := os.ReadFile(filepath.Join(crashDir, "console.log"))
	if err != nil || !strings.Contains(string(consoleBytes), "guest kernel boot") {
		t.Fatalf("preserved console.log = (%q, %v)", string(consoleBytes), err)
	}
	if _, err := os.Stat(filepath.Join(crashDir, "vm.json")); err != nil {
		t.Fatalf("expected preserved vm.json: %v", err)
	}
	exitBytes, err := os.ReadFile(filepath.Join(crashDir, "exit.json"))
	if err != nil {
		t.Fatalf("read exit.json: %v", err)
	}
	var rec crashedVMRecord
	if err := json.Unmarshal(exitBytes, &rec); err != nil {
		t.Fatalf("unmarshal exit.json: %v", err)
	}
	if rec.Status.Signal != int(syscall.SIGKILL) || rec.Status.Result != "signal" || rec.SyncErr == "" {
		t.Fatalf("unexpected exit.json record: %+v", rec)
	}

	// Neither Reconnect nor ReconcileOrphans may delete _crashed.
	if _, err := mgr.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	reaped, _ := mgr.ReconcileOrphans(ctx, nil)
	if len(reaped) != 0 {
		t.Fatalf("ReconcileOrphans must ignore %s, reaped=%v", crashedDirName, reaped)
	}
	if _, err := os.Stat(crashDir); err != nil {
		t.Fatalf("expected %s preserved across Reconnect/ReconcileOrphans: %v", crashDir, err)
	}
}

func TestManagerStopCleanExitDoesNotPreserveCrashLogs(t *testing.T) {
	ctx := context.Background()
	runDir := shortSockDir(t)
	sockDir := shortSockDir(t)
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv, err := guestd.NewServer(guestd.ServerConfig{WorkDir: t.TempDir(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ServeControl(ctx, ln) }()

	fl := newFakeLauncher()
	mgr, err := NewManager(Config{
		RunDir:          runDir,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: time.Second,
	}, WithLauncher(fl), WithDialerFactory(func(_ string) guestd.DialFunc {
		return guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock})
	}), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-clean",
		CellHostPath:  "/mnt/cells/ws-clean",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := mgr.Stop(ctx, "ws-clean", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, crashedDirName)); !os.IsNotExist(err) {
		t.Fatalf("clean stop should not create %s", crashedDirName)
	}
}

func TestManagerReconnectPreservesDeadSignaledVM(t *testing.T) {
	ctx := context.Background()
	runDir := shortSockDir(t)
	vmDir := filepath.Join(runDir, "ws-dead")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(vmDir, vmMeta{
		ID:             "ws-dead",
		UnitName:       "hotbed-vm-ws-dead",
		VsockUDS:       filepath.Join(vmDir, "vsock.sock"),
		ConsoleLog:     filepath.Join(vmDir, "console.log"),
		FirecrackerLog: filepath.Join(vmDir, "firecracker.log"),
		GuestCID:       5,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "firecracker.log"), []byte("ERROR died while worker restarting\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fl := newFakeLauncher()
	fl.customStatus["hotbed-vm-ws-dead"] = ProcessStatus{
		Loaded: true,
		Exited: true,
		Signal: int(syscall.SIGKILL),
		Result: "signal",
	}
	mgr, err := NewManager(Config{
		RunDir:         runDir,
		DisableNetwork: true,
	}, WithLauncher(fl), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	n, err := mgr.Reconnect(ctx)
	if err != nil || n != 0 {
		t.Fatalf("Reconnect = (%d, %v), want (0, nil)", n, err)
	}
	entries, err := os.ReadDir(filepath.Join(runDir, crashedDirName))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 preserved crash dir, got (%v, %v)", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(runDir, crashedDirName, entries[0].Name(), "firecracker.log"))
	if err != nil || !strings.Contains(string(b), "died while worker restarting") {
		t.Fatalf("preserved firecracker.log = (%q, %v)", string(b), err)
	}
}

func TestPruneCrashedVMDirsCapsRetention(t *testing.T) {
	crashRoot := t.TempDir()
	for i := 0; i < 6; i++ {
		name := time.Date(2026, 11, 26, 15, 0, i, 0, time.UTC).Format("20060102T150405.000Z") + "-ws"
		if err := os.MkdirAll(filepath.Join(crashRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pruneCrashedDirs(crashRoot, 3)
	entries, err := os.ReadDir(crashRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	// Oldest 3 (00, 01, 02) pruned; newest 3 (03, 04, 05) kept.
	if !strings.Contains(entries[0].Name(), "150003") || !strings.Contains(entries[2].Name(), "150005") {
		t.Fatalf("unexpected remaining entries: %s .. %s", entries[0].Name(), entries[2].Name())
	}
}

func TestDirectLauncherCapturesSignal(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep binary not found")
	}
	dl := NewDirectLauncher()
	ctx := context.Background()
	if err := dl.Start(ctx, ProcessSpec{
		Name:   "hotbed-vm-sigtest",
		Binary: sleepBin,
		Args:   []string{"60"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := dl.Kill(ctx, "hotbed-vm-sigtest"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := dl.Status(ctx, "hotbed-vm-sigtest")
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Exited {
			if st.Signal != int(syscall.SIGKILL) || st.Result != "signal" || !st.DiedAbnormally() {
				t.Fatalf("Status after SIGKILL = %+v, want signal=9 result=signal", st)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for killed process to exit")
}
