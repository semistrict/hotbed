package vm

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/guestd"
)

type fakeLauncher struct {
	mu      sync.Mutex
	running map[string]bool
	exited  map[string]int
	onStart func(ProcessSpec) error
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{
		running: map[string]bool{},
		exited:  map[string]int{},
	}
}

func (f *fakeLauncher) Start(_ context.Context, s ProcessSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onStart != nil {
		if err := f.onStart(s); err != nil {
			return err
		}
	}
	f.running[s.Name] = true
	delete(f.exited, s.Name)
	return nil
}

func (f *fakeLauncher) Stop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, name)
	f.exited[name] = 0
	return nil
}

func (f *fakeLauncher) Status(_ context.Context, name string) (ProcessStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[name] {
		return ProcessStatus{Loaded: true, Running: true}, nil
	}
	if code, ok := f.exited[name]; ok {
		return ProcessStatus{Loaded: true, Exited: true, ExitCode: code}, nil
	}
	return ProcessStatus{}, nil
}

func TestBuildFirecrackerConfig(t *testing.T) {
	mgr, err := NewManager(Config{
		RunDir:             t.TempDir(),
		KernelPath:         "/var/lib/cladefs/vmlinux",
		DefaultVCPUs:       4,
		DefaultMemMiB:      2048,
		DefaultDiskBytes:   16 << 30,
		DAXWindowBytes:     1 << 30,
		DAXPrivateMaxBytes: 256 << 20,
		DisableNetwork:     true,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	netAlloc := NetAlloc{
		TAPName:   "hbt-1234567890",
		GuestIP:   "10.199.0.2",
		GatewayIP: "10.199.0.1",
		Netmask:   "255.255.0.0",
		GuestMAC:  "02:68:62:aa:bb:cc",
	}
	fc := mgr.BuildFirecrackerConfig(LaunchSpec{
		ID:            "ws-1",
		CellHostPath:  "/mnt/cells/ws-1",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	}, 3, "/run/hotbed/vms/ws-1/vsock.sock", netAlloc)

	if !strings.Contains(fc.BootSource.BootArgs, "init=/sbin/hotbed-guestd") {
		t.Fatalf("boot_args missing init: %q", fc.BootSource.BootArgs)
	}
	if !strings.Contains(fc.BootSource.BootArgs, "ip=10.199.0.2::10.199.0.1:255.255.0.0::eth0:off") {
		t.Fatalf("boot_args missing ip=: %q", fc.BootSource.BootArgs)
	}
	if len(fc.Cladefs) != 1 || fc.Cladefs[0].DAXPrivateMaxBytes != 256<<20 {
		t.Fatalf("unexpected cladefs config: %+v", fc.Cladefs)
	}
	if len(fc.NetInterfaces) != 1 || fc.NetInterfaces[0].HostDevName != "hbt-1234567890" {
		t.Fatalf("unexpected net config: %+v", fc.NetInterfaces)
	}
	if fc.Balloon == nil || !fc.Balloon.DeflateOnOOM {
		t.Fatalf("expected balloon with deflate_on_oom")
	}
	b, err := json.Marshal(fc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"drives":[]`) {
		t.Fatalf("Firecracker JSON must include empty drives array: %s", string(b))
	}
}

func shortSockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "hb-vm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestManagerStartReconnectAndStop(t *testing.T) {
	ctx := context.Background()
	runDir := shortSockDir(t)
	workDir := t.TempDir()
	stateDir := t.TempDir()

	// Start a real guestd control server on a Unix socket.
	sockDir := shortSockDir(t)
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := guestd.NewServer(guestd.ServerConfig{WorkDir: workDir, StateDir: stateDir})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ServeControl(ctx, ln) }()
	defer ln.Close()

	// Fake host git-auth target socket.
	gaTarget := filepath.Join(sockDir, "git-cred.sock")
	gaLn, err := net.Listen("unix", gaTarget)
	if err != nil {
		t.Fatalf("listen git-auth target: %v", err)
	}
	defer gaLn.Close()
	go func() {
		for {
			c, err := gaLn.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("GIT_AUTH_BRIDGE_OK"))
			_ = c.Close()
		}
	}()

	fl := newFakeLauncher()
	dialerFactory := func(_ string) guestd.DialFunc {
		return guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock})
	}

	mgr, err := NewManager(Config{
		RunDir:          runDir,
		GitAuthHostSock: gaTarget,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 2 * time.Second,
	}, WithLauncher(fl), WithDialerFactory(dialerFactory))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	inst, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-abc",
		CellHostPath:  "/mnt/cells/ws-abc",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.GuestCID != 3 {
		t.Fatalf("GuestCID = %d, want 3", inst.GuestCID)
	}

	// Verify git-auth vsock bridge socket (<vsock>_5002) forwards to gaTarget.
	bridgeSock := fmt.Sprintf("%s_%d", inst.VsockUDS, guestd.GitAuthPort)
	conn, err := net.Dial("unix", bridgeSock)
	if err != nil {
		t.Fatalf("dial git-auth bridge %s: %v", bridgeSock, err)
	}
	got, _ := io.ReadAll(conn)
	_ = conn.Close()
	if string(got) != "GIT_AUTH_BRIDGE_OK" {
		t.Fatalf("git-auth bridge got %q", string(got))
	}

	// Simulate hotbed-worker restart: new Manager reconnects to surviving VM!
	mgr2, err := NewManager(Config{
		RunDir:          runDir,
		GitAuthHostSock: gaTarget,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 2 * time.Second,
	}, WithLauncher(fl), WithDialerFactory(dialerFactory))
	if err != nil {
		t.Fatalf("NewManager 2: %v", err)
	}
	n, err := mgr2.Reconnect(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Reconnect = (%d, %v), want (1, nil)", n, err)
	}
	client, err := mgr2.ClientFor("ws-abc")
	if err != nil {
		t.Fatalf("ClientFor after reconnect: %v", err)
	}
	if _, err := client.Health(ctx); err != nil {
		t.Fatalf("Health after reconnect: %v", err)
	}

	// Stop cleans up instance and directory.
	if err := mgr2.Stop(ctx, "ws-abc", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(inst.Dir); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed after Stop", inst.Dir)
	}
}

func TestManagerStartFailsFastOnExit158(t *testing.T) {
	fl := newFakeLauncher()
	fl.onStart = func(s ProcessSpec) error {
		_ = os.WriteFile(s.ConsoleLog, []byte("fatal: cladefsd disconnected\n"), 0o644)
		fl.exited[s.Name] = ExitCodeCladefsdLost
		return nil
	}
	// Override Start so running=false immediately
	fl2 := &exitingLauncher{exitCode: ExitCodeCladefsdLost}
	mgr, err := NewManager(Config{
		RunDir:         t.TempDir(),
		DisableNetwork: true,
		BootTimeout:    5 * time.Second,
	}, WithLauncher(fl2))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = mgr.Start(context.Background(), LaunchSpec{
		ID:            "ws-fail",
		CellHostPath:  "/mnt/cells/ws-fail",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	})
	if err == nil || !strings.Contains(err.Error(), "158") {
		t.Fatalf("expected fast exit 158 error, got: %v", err)
	}
}

type exitingLauncher struct{ exitCode int }

func (e *exitingLauncher) Start(_ context.Context, s ProcessSpec) error {
	return os.WriteFile(s.ConsoleLog, []byte("boom cladefsd lost\n"), 0o644)
}
func (e *exitingLauncher) Stop(context.Context, string) error { return nil }
func (e *exitingLauncher) Status(context.Context, string) (ProcessStatus, error) {
	return ProcessStatus{Loaded: true, Exited: true, ExitCode: e.exitCode}, nil
}

func TestInjectGuestdTar(t *testing.T) {
	var baseBuf bytes.Buffer
	tw := tar.NewWriter(&baseBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "etc/hostname", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
	_, _ = tw.Write([]byte("deb\n"))
	_ = tw.WriteHeader(&tar.Header{Name: "etc/resolv.conf", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("old"))
	_ = tw.Close()

	guestdBin := filepath.Join(t.TempDir(), "hotbed-guestd")
	if err := os.WriteFile(guestdBin, []byte("ELF-FAKE-GUESTD"), 0o755); err != nil {
		t.Fatal(err)
	}
	outTar := filepath.Join(t.TempDir(), "out.tar")
	if err := InjectGuestdTar(&baseBuf, guestdBin, []string{"1.1.1.1"}, outTar); err != nil {
		t.Fatalf("InjectGuestdTar: %v", err)
	}

	f, err := os.Open(outTar)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	seen := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		seen[hdr.Name] = string(b)
	}
	if seen["sbin/hotbed-guestd"] != "ELF-FAKE-GUESTD" {
		t.Fatalf("guestd entry = %q", seen["sbin/hotbed-guestd"])
	}
	if !strings.Contains(seen["etc/resolv.conf"], "nameserver 1.1.1.1") {
		t.Fatalf("resolv.conf = %q", seen["etc/resolv.conf"])
	}
	if _, ok := seen["work/"]; !ok {
		t.Fatalf("missing work/ directory in tar")
	}
}

func TestInjectGuestdTarUsrmergeSymlink(t *testing.T) {
	var baseBuf bytes.Buffer
	tw := tar.NewWriter(&baseBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "usr/sbin/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "sbin", Typeflag: tar.TypeSymlink, Linkname: "usr/sbin", Mode: 0o777})
	_ = tw.Close()

	guestdBin := filepath.Join(t.TempDir(), "hotbed-guestd")
	if err := os.WriteFile(guestdBin, []byte("ELF-USRMERGE"), 0o755); err != nil {
		t.Fatal(err)
	}
	outTar := filepath.Join(t.TempDir(), "out.tar")
	if err := InjectGuestdTar(&baseBuf, guestdBin, []string{"1.1.1.1"}, outTar); err != nil {
		t.Fatalf("InjectGuestdTar: %v", err)
	}
	f, err := os.Open(outTar)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	seen := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		seen[hdr.Name] = string(b)
	}
	if seen["usr/sbin/hotbed-guestd"] != "ELF-USRMERGE" {
		t.Fatalf("expected usr/sbin/hotbed-guestd in usrmerge tar, got keys: %v", seen)
	}
	if _, bad := seen["sbin/hotbed-guestd"]; bad {
		t.Fatalf("must not emit sbin/hotbed-guestd under symlinked sbin")
	}
}
