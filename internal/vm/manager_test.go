package vm

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/guestd"
)

type fakeLauncher struct {
	mu           sync.Mutex
	running      map[string]bool
	deactivating map[string]bool
	exited       map[string]int
	slices       map[string]string
	onStart      func(ProcessSpec) error
	onStop       func(ctx context.Context, name string) error
	onKill       func(ctx context.Context, name string) error
	stopCalls    []string
	killCalls    []string
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{
		running:      map[string]bool{},
		deactivating: map[string]bool{},
		exited:       map[string]int{},
		slices:       map[string]string{},
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
	delete(f.deactivating, s.Name)
	delete(f.exited, s.Name)
	f.slices[s.Name] = s.Slice
	return nil
}

func (f *fakeLauncher) Stop(ctx context.Context, name string) error {
	f.mu.Lock()
	f.stopCalls = append(f.stopCalls, name)
	onStop := f.onStop
	f.mu.Unlock()
	if onStop != nil {
		return onStop(ctx, name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, name)
	delete(f.deactivating, name)
	f.exited[name] = 0
	return nil
}

func (f *fakeLauncher) Status(_ context.Context, name string) (ProcessStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[name] {
		return ProcessStatus{Loaded: true, Running: true}, nil
	}
	if f.deactivating[name] {
		return ProcessStatus{Loaded: true, Deactivating: true}, nil
	}
	if code, ok := f.exited[name]; ok {
		return ProcessStatus{Loaded: true, Exited: true, ExitCode: code}, nil
	}
	return ProcessStatus{}, nil
}

func (f *fakeLauncher) List(_ context.Context, slice string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for name := range f.running {
		if slice != "" && f.slices[name] != "" && f.slices[name] != slice {
			continue
		}
		if strings.HasPrefix(name, "hotbed-vm-") && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for name := range f.deactivating {
		if slice != "" && f.slices[name] != "" && f.slices[name] != slice {
			continue
		}
		if strings.HasPrefix(name, "hotbed-vm-") && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

type fakeKillerLauncher struct {
	*fakeLauncher
}

func (f *fakeKillerLauncher) Kill(ctx context.Context, name string) error {
	f.mu.Lock()
	f.killCalls = append(f.killCalls, name)
	onKill := f.onKill
	f.mu.Unlock()
	if onKill != nil {
		return onKill(ctx, name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, name)
	delete(f.deactivating, name)
	f.exited[name] = 137
	return nil
}

func fakeToolsDiskOpt(t *testing.T) ManagerOption {
	t.Helper()
	return WithToolsDiskBuilder(func(cacheDir, _ string) (string, error) {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return "", err
		}
		p := filepath.Join(cacheDir, "guestd-test.ext4")
		if err := os.WriteFile(p, []byte("fake-ext4"), 0o644); err != nil {
			return "", err
		}
		return p, nil
	})
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
	if !strings.Contains(fc.BootSource.BootArgs, "psi=1") {
		t.Fatalf("boot_args missing psi=1: %q", fc.BootSource.BootArgs)
	}
	if !strings.Contains(fc.BootSource.BootArgs, "ip=10.199.0.2::10.199.0.1:255.255.0.0::eth0:off") {
		t.Fatalf("boot_args missing ip=: %q", fc.BootSource.BootArgs)
	}
	if strings.Contains(fc.BootSource.BootArgs, "HOTBED_NOFILE=") {
		t.Fatalf("boot_args should omit HOTBED_NOFILE when unset: %q", fc.BootSource.BootArgs)
	}
	nofileMgr, err := NewManager(Config{
		RunDir:         t.TempDir(),
		KernelPath:     "/var/lib/cladefs/vmlinux",
		Nofile:         131072,
		DisableNetwork: true,
	})
	if err != nil {
		t.Fatalf("NewManager(Nofile): %v", err)
	}
	nofileFC := nofileMgr.BuildFirecrackerConfig(LaunchSpec{
		ID:            "ws-nofile",
		CellHostPath:  "/mnt/cells/ws-nofile",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	}, 5, "/run/hotbed/vms/ws-nofile/vsock.sock", NetAlloc{})
	if !strings.Contains(nofileFC.BootSource.BootArgs, "HOTBED_NOFILE=131072") {
		t.Fatalf("boot_args missing HOTBED_NOFILE=131072: %q", nofileFC.BootSource.BootArgs)
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
	if !strings.Contains(string(b), `"cache_type":"Unsafe"`) {
		t.Fatalf("Firecracker JSON must default cache_type to Unsafe: %s", string(b))
	}

	noDAXMgr, err := NewManager(Config{
		RunDir:         t.TempDir(),
		KernelPath:     "/var/lib/cladefs/Image",
		DisableDAX:     true,
		DisableNetwork: true,
	})
	if err != nil {
		t.Fatalf("NewManager(DisableDAX): %v", err)
	}
	noDAXFC := noDAXMgr.BuildFirecrackerConfig(LaunchSpec{
		ID:            "ws-arm64",
		CellHostPath:  "/mnt/cells/ws-arm64",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	}, 4, "/run/hotbed/vms/ws-arm64/vsock.sock", NetAlloc{})
	noDAXBytes, err := json.Marshal(noDAXFC)
	if err != nil {
		t.Fatalf("marshal noDAX: %v", err)
	}
	if strings.Contains(string(noDAXBytes), "dax_window_size") || strings.Contains(string(noDAXBytes), "dax_private_max_bytes") {
		t.Fatalf("DisableDAX must omit dax_window_size and dax_private_max_bytes: %s", string(noDAXBytes))
	}

	// When ToolsDiskPath is set, Firecracker attaches it as a read-only root
	// virtio-pmem device and boots /hotbed-guestd with HOTBED_ROOT=cladefs:rootfs.
	toolsFC := mgr.BuildFirecrackerConfig(LaunchSpec{
		ID:            "ws-tools",
		CellHostPath:  "/mnt/cells/ws-tools",
		CladefsVMSock: "/run/cladefsd/vm.sock",
		ToolsDiskPath: "/var/lib/hotbed/vms/_tools/guestd-abc123.ext4",
	}, 6, "/run/hotbed/vms/ws-tools/vsock.sock", netAlloc)
	if !strings.Contains(toolsFC.BootSource.BootArgs, "init=/hotbed-guestd") || !strings.Contains(toolsFC.BootSource.BootArgs, "HOTBED_ROOT=cladefs:rootfs") {
		t.Fatalf("tools-disk boot_args missing init or HOTBED_ROOT: %q", toolsFC.BootSource.BootArgs)
	}
	if strings.Contains(toolsFC.BootSource.BootArgs, "root=rootfs") || strings.Contains(toolsFC.BootSource.BootArgs, "rootfstype=cladefs") {
		t.Fatalf("tools-disk boot_args must not override root=/dev/pmem0: %q", toolsFC.BootSource.BootArgs)
	}
	if len(toolsFC.Drives) != 0 {
		t.Fatalf("tools-disk config must leave drives empty, got: %+v", toolsFC.Drives)
	}
	if len(toolsFC.Pmem) != 1 || toolsFC.Pmem[0].ID != "hotbed-tools" || !toolsFC.Pmem[0].RootDevice || !toolsFC.Pmem[0].ReadOnly || toolsFC.Pmem[0].PathOnHost != "/var/lib/hotbed/vms/_tools/guestd-abc123.ext4" {
		t.Fatalf("unexpected tools disk pmem: %+v", toolsFC.Pmem)
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
	}, WithLauncher(fl), WithDialerFactory(dialerFactory), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	inst, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-abc",
		CellHostPath:  "/mnt/cells/ws-abc",
		CladefsVMSock: "/run/cladefsd/vm.sock",
		VCPUs:         3,
		MemMiB:        1536,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.GuestCID != 3 {
		t.Fatalf("GuestCID = %d, want 3", inst.GuestCID)
	}
	// It boots at the size asked, and says so.
	if inst.VCPUs != 3 || inst.MemMiB != 1536 {
		t.Fatalf("instance size = %d vCPUs, %d MiB, want 3, 1536", inst.VCPUs, inst.MemMiB)
	}
	var fcCfg FirecrackerConfig
	if b, err := os.ReadFile(filepath.Join(inst.Dir, "vm.json")); err != nil {
		t.Fatalf("read vm.json: %v", err)
	} else if err := json.Unmarshal(b, &fcCfg); err != nil {
		t.Fatalf("decode vm.json: %v", err)
	}
	if fcCfg.MachineConfig.VCPUCount != 3 || fcCfg.MachineConfig.MemSizeMiB != 1536 {
		t.Fatalf("vm.json machine config = %+v, want 3 vCPUs, 1536 MiB", fcCfg.MachineConfig)
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
	}, WithLauncher(fl), WithDialerFactory(dialerFactory), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager 2: %v", err)
	}
	n, err := mgr2.Reconnect(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Reconnect = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := os.Stat(filepath.Join(runDir, toolsDirName, "guestd-test.ext4")); err != nil {
		t.Fatalf("Reconnect must preserve %s cache dir: %v", toolsDirName, err)
	}
	client, err := mgr2.ClientFor("ws-abc")
	if err != nil {
		t.Fatalf("ClientFor after reconnect: %v", err)
	}
	if got := mgr2.Get("ws-abc"); got.VCPUs != 3 || got.MemMiB != 1536 {
		t.Fatalf("reconnected size = %d vCPUs, %d MiB, want 3, 1536", got.VCPUs, got.MemMiB)
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
	}, WithLauncher(fl2), fakeToolsDiskOpt(t))
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
func (e *exitingLauncher) List(context.Context, string) ([]string, error) { return nil, nil }

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

func TestInjectGuestdTarWithoutGuestdOmitsBinary(t *testing.T) {
	var baseBuf bytes.Buffer
	tw := tar.NewWriter(&baseBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "sbin/hotbed-guestd", Typeflag: tar.TypeReg, Mode: 0o755, Size: 5})
	_, _ = tw.Write([]byte("stale"))
	_ = tw.WriteHeader(&tar.Header{Name: "etc/hostname", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
	_, _ = tw.Write([]byte("deb\n"))
	_ = tw.Close()

	outTar := filepath.Join(t.TempDir(), "out.tar")
	if err := InjectGuestdTar(&baseBuf, "", []string{"1.1.1.1"}, outTar); err != nil {
		t.Fatalf("InjectGuestdTar(empty guestd): %v", err)
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
	if _, ok := seen["sbin/hotbed-guestd"]; ok {
		t.Fatalf("empty guestdBin must omit sbin/hotbed-guestd")
	}
	if !strings.Contains(seen["etc/resolv.conf"], "nameserver 1.1.1.1") {
		t.Fatalf("resolv.conf = %q", seen["etc/resolv.conf"])
	}
	if _, ok := seen["work/"]; !ok {
		t.Fatalf("missing work/ directory in tar")
	}
}

func TestParseSystemdProcessStatus(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want ProcessStatus
	}{
		{
			name: "oom-kill by systemd-oomd",
			raw:  "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=oom-kill\nExecMainCode=2\nExecMainStatus=9\n",
			want: ProcessStatus{Loaded: true, Exited: true, Signal: 9, Result: "oom-kill", OOMKilled: true},
		},
		{
			name: "normal exit 158",
			raw:  "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nExecMainCode=1\nExecMainStatus=158\n",
			want: ProcessStatus{Loaded: true, Exited: true, ExitCode: 158, Result: "exit-code"},
		},
		{
			name: "signal kill non-oom",
			raw:  "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=signal\nExecMainCode=2\nExecMainStatus=15\n",
			want: ProcessStatus{Loaded: true, Exited: true, Signal: 15, Result: "signal"},
		},
		{
			name: "running",
			raw:  "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nExecMainCode=0\nExecMainStatus=0\n",
			want: ProcessStatus{Loaded: true, Running: true, Result: "success"},
		},
		{
			name: "deactivating",
			raw:  "LoadState=loaded\nActiveState=deactivating\nSubState=stop-sigterm\nResult=success\nExecMainCode=0\nExecMainStatus=0\n",
			want: ProcessStatus{Loaded: true, Deactivating: true, Result: "success"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSystemdProcessStatus(tc.raw)
			if got != tc.want {
				t.Fatalf("parseSystemdProcessStatus = %+v, want %+v", got, tc.want)
			}
		})
	}
	if !(ProcessStatus{Deactivating: true}).Alive() {
		t.Fatal("Deactivating status must report Alive() == true")
	}
}

func TestParseVMListAndSliceUnits(t *testing.T) {
	listOut := strings.Join([]string{
		"hotbed-vm-2x6xdsk5.service loaded active running Firecracker",
		"hotbed-vm-base-cladefs-vm-b470a516.service loaded active running Firecracker",
		"hotbed-vm-failed.service loaded failed failed Firecracker",
		"other.service loaded active running Other",
	}, "\n")
	cands := parseVMListUnits(listOut)
	wantCands := []string{
		"hotbed-vm-2x6xdsk5.service",
		"hotbed-vm-base-cladefs-vm-b470a516.service",
		"hotbed-vm-failed.service",
	}
	if !slices.Equal(cands, wantCands) {
		t.Fatalf("parseVMListUnits = %q, want %q", cands, wantCands)
	}

	showOut := strings.Join([]string{
		"Id=hotbed-vm-2x6xdsk5.service",
		"Slice=user-1000-hotbed.slice",
		"LoadState=loaded",
		"ActiveState=active",
		"",
		"Id=hotbed-vm-base-cladefs-vm-b470a516.service",
		"Slice=user-1000-hotbed.slice",
		"LoadState=loaded",
		"ActiveState=deactivating",
		"",
		"Id=hotbed-vm-other-slice.service",
		"Slice=hotbed.slice",
		"LoadState=loaded",
		"ActiveState=active",
		"",
		"Id=hotbed-vm-inactive.service",
		"Slice=user-1000-hotbed.slice",
		"LoadState=loaded",
		"ActiveState=inactive",
	}, "\n")
	got := parseVMSliceUnits(showOut, "user-1000-hotbed.slice")
	want := []string{"hotbed-vm-2x6xdsk5", "hotbed-vm-base-cladefs-vm-b470a516"}
	if !slices.Equal(got, want) {
		t.Fatalf("parseVMSliceUnits = %q, want %q", got, want)
	}
}

func startTestGuestd(t *testing.T) func(string) guestd.DialFunc {
	t.Helper()
	sockDir := shortSockDir(t)
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv, err := guestd.NewServer(guestd.ServerConfig{WorkDir: t.TempDir(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ServeControl(ctx, ln) }()
	return func(_ string) guestd.DialFunc {
		return guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock})
	}
}

func TestManagerStopPreservesMetadataWhenUnitStaysAlive(t *testing.T) {
	ctx := context.Background()
	runDir := shortSockDir(t)
	fl := newFakeLauncher()
	fl.onStop = func(_ context.Context, _ string) error {
		// Simulate systemctl stop failing while Firecracker stays alive.
		return errors.New("systemctl stop timed out")
	}
	mgr, err := NewManager(Config{
		RunDir:          runDir,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 100 * time.Millisecond,
	}, WithLauncher(fl), WithDialerFactory(startTestGuestd(t)), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	inst, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-stubborn",
		CellHostPath:  "/mnt/cells/ws-stubborn",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := mgr.Stop(ctx, "ws-stubborn", true); err == nil {
		t.Fatal("expected Stop to fail while unit remains alive")
	}
	if _, err := os.Stat(filepath.Join(inst.Dir, "meta.json")); err != nil {
		t.Fatalf("expected meta.json preserved when Stop fails, got: %v", err)
	}
	if mgr.Get("ws-stubborn") == nil {
		t.Fatal("expected instance to remain tracked in Manager when Stop fails")
	}
	mgr.mu.Lock()
	cidOwner := mgr.usedCIDs[inst.GuestCID]
	mgr.mu.Unlock()
	if cidOwner != "ws-stubborn" {
		t.Fatalf("CID %d owner = %q, want ws-stubborn preserved while VM still alive", inst.GuestCID, cidOwner)
	}

	// Once Stop actually kills the unit, retry cleans up dir, instance, and CID.
	fl.mu.Lock()
	fl.onStop = nil
	fl.mu.Unlock()
	if err := mgr.Stop(ctx, "ws-stubborn", false); err != nil {
		t.Fatalf("retry Stop: %v", err)
	}
	if _, err := os.Stat(inst.Dir); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed after verified Stop", inst.Dir)
	}
	if mgr.Get("ws-stubborn") != nil {
		t.Fatal("expected instance removed after verified Stop")
	}
}

func TestManagerStopEscalatesToKillAndHonorsCancelledContext(t *testing.T) {
	ctx := context.Background()
	runDir := shortSockDir(t)
	fl := &fakeKillerLauncher{fakeLauncher: newFakeLauncher()}
	fl.onStop = func(stopCtx context.Context, name string) error {
		if stopCtx.Err() != nil {
			t.Errorf("Stop received cancelled context for %s: %v", name, stopCtx.Err())
			return stopCtx.Err()
		}
		// Leave unit alive so Manager must escalate to Killer.Kill.
		return errors.New("graceful stop hung")
	}
	mgr, err := NewManager(Config{
		RunDir:          runDir,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 100 * time.Millisecond,
	}, WithLauncher(fl), WithDialerFactory(startTestGuestd(t)), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	inst, err := mgr.Start(ctx, LaunchSpec{
		ID:            "base-cladefs-vm-deadbeef",
		CellHostPath:  "/mnt/cells/bases/cladefs/cladefs-vm-deadbeef",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Pass an ALREADY-CANCELLED caller context (the exact base-build abort bug):
	// Manager.Stop(..., syncFS=false) must still stop/kill Firecracker instead
	// of deleting meta.json while leaving the unit running.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := mgr.Stop(cancelledCtx, "base-cladefs-vm-deadbeef", false); err != nil {
		t.Fatalf("Stop(syncFS=false) with cancelled ctx should escalate to Kill and succeed: %v", err)
	}
	fl.mu.Lock()
	kills := slices.Clone(fl.killCalls)
	alive := fl.running[inst.UnitName]
	fl.mu.Unlock()
	if len(kills) != 1 || kills[0] != inst.UnitName {
		t.Fatalf("killCalls = %q, want [%s]", kills, inst.UnitName)
	}
	if alive {
		t.Fatalf("unit %s must not remain running", inst.UnitName)
	}
	if _, err := os.Stat(inst.Dir); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed after Kill confirmed dead", inst.Dir)
	}
}

func startSlowShutdownGuestd(t *testing.T, shutdownDelay time.Duration, shutdownErr bool) (func(string) guestd.DialFunc, *atomic.Bool) {
	t.Helper()
	sockDir := shortSockDir(t)
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var completed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/guestd.v1.GuestService/Health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/guestd.v1.GuestService/Shutdown", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(shutdownDelay):
		}
		if shutdownErr {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"internal","message":"sync failed"}`))
			return
		}
		completed.Store(true)
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return func(_ string) guestd.DialFunc {
		return guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock})
	}, &completed
}

func TestManagerStopSyncFSWaitsBeyondShutdownTimeoutAndFailsOnSyncError(t *testing.T) {
	ctx := context.Background()

	// 1. Sync takes 150ms (> 50ms ShutdownTimeout, < 2s SyncShutdownTimeout):
	// Stop(syncFS=true) must wait for full guest syscall.Sync() instead of
	// killing Firecracker mid-sync (board 11807).
	fl1 := newFakeLauncher()
	dialer1, completed1 := startSlowShutdownGuestd(t, 150*time.Millisecond, false)
	mgr1, err := NewManager(Config{
		RunDir:              shortSockDir(t),
		DisableNetwork:      true,
		BootTimeout:         5 * time.Second,
		ShutdownTimeout:     50 * time.Millisecond,
		SyncShutdownTimeout: 2 * time.Second,
	}, WithLauncher(fl1), WithDialerFactory(dialer1), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager 1: %v", err)
	}
	inst1, err := mgr1.Start(ctx, LaunchSpec{
		ID:            "prov-slow-sync",
		CellHostPath:  "/mnt/cells/prov-slow-sync",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	})
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	if err := mgr1.Stop(ctx, "prov-slow-sync", true); err != nil {
		t.Fatalf("Stop(syncFS=true) failed on slow sync within SyncShutdownTimeout: %v", err)
	}
	if !completed1.Load() {
		t.Fatal("guest Shutdown(SyncFs=true) did not complete before VM was stopped")
	}
	if _, err := os.Stat(inst1.Dir); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed after Stop", inst1.Dir)
	}

	// 2. Sync exceeds SyncShutdownTimeout: Stop(syncFS=true) must still stop
	// Firecracker and clean up metadata, AND return an error so callers never
	// snapshot a half-flushed cell to R2.
	fl2 := newFakeLauncher()
	dialer2, completed2 := startSlowShutdownGuestd(t, 400*time.Millisecond, false)
	mgr2, err := NewManager(Config{
		RunDir:              shortSockDir(t),
		DisableNetwork:      true,
		BootTimeout:         5 * time.Second,
		ShutdownTimeout:     50 * time.Millisecond,
		SyncShutdownTimeout: 80 * time.Millisecond,
	}, WithLauncher(fl2), WithDialerFactory(dialer2), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager 2: %v", err)
	}
	inst2, err := mgr2.Start(ctx, LaunchSpec{
		ID:            "prov-timeout-sync",
		CellHostPath:  "/mnt/cells/prov-timeout-sync",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	})
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if err := mgr2.Stop(ctx, "prov-timeout-sync", true); err == nil {
		t.Fatal("expected Stop(syncFS=true) to fail when guest sync timed out")
	}
	if completed2.Load() {
		t.Fatal("expected timed-out sync not to have completed")
	}
	if st, _ := fl2.Status(ctx, inst2.UnitName); st.Alive() {
		t.Fatalf("VM unit %s must still be stopped after sync timeout", inst2.UnitName)
	}
	if _, err := os.Stat(inst2.Dir); !os.IsNotExist(err) {
		t.Fatalf("expected %s cleaned up after sync timeout stop", inst2.Dir)
	}

	// 3. Cancelled context with syncFS=true: must still stop VM cleanly AND
	// return a non-nil sync-skipped error.
	fl3 := newFakeLauncher()
	mgr3, err := NewManager(Config{
		RunDir:          shortSockDir(t),
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 50 * time.Millisecond,
	}, WithLauncher(fl3), WithDialerFactory(startTestGuestd(t)), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager 3: %v", err)
	}
	inst3, err := mgr3.Start(ctx, LaunchSpec{
		ID:            "prov-cancel-sync",
		CellHostPath:  "/mnt/cells/prov-cancel-sync",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	})
	if err != nil {
		t.Fatalf("Start 3: %v", err)
	}
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := mgr3.Stop(cancelledCtx, "prov-cancel-sync", true); err == nil {
		t.Fatal("expected Stop(syncFS=true) with cancelled ctx to report sync skipped")
	}
	if st, _ := fl3.Status(ctx, inst3.UnitName); st.Alive() {
		t.Fatalf("VM unit %s must still be stopped on cancelled ctx", inst3.UnitName)
	}
}

func TestManagerReconnectReapsEphemeralBuildVMs(t *testing.T) {
	ctx := context.Background()
	runDir := shortSockDir(t)
	fl := newFakeLauncher()
	dialer := startTestGuestd(t)

	mgr1, err := NewManager(Config{
		RunDir:          runDir,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 200 * time.Millisecond,
	}, WithLauncher(fl), WithDialerFactory(dialer), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager 1: %v", err)
	}

	for _, id := range []string{"ws-keep", "base-proj-vm-11111111", "prov-baseimg-22222222"} {
		if _, err := mgr1.Start(ctx, LaunchSpec{
			ID:            id,
			CellHostPath:  "/mnt/cells/" + id,
			CladefsVMSock: "/run/cladefsd/vm.sock",
		}); err != nil {
			t.Fatalf("Start %s: %v", id, err)
		}
	}

	// Worker restarts: Reconnect must adopt ws-keep and reap base-* and prov-*.
	mgr2, err := NewManager(Config{
		RunDir:          runDir,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 200 * time.Millisecond,
	}, WithLauncher(fl), WithDialerFactory(dialer))
	if err != nil {
		t.Fatalf("NewManager 2: %v", err)
	}
	n, err := mgr2.Reconnect(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Reconnect = (%d, %v), want (1, nil)", n, err)
	}
	if mgr2.Get("ws-keep") == nil {
		t.Fatal("expected ws-keep reconnected")
	}
	for _, id := range []string{"base-proj-vm-11111111", "prov-baseimg-22222222"} {
		if mgr2.Get(id) != nil {
			t.Fatalf("ephemeral build VM %s must not be reconnected", id)
		}
		if st, _ := fl.Status(ctx, "hotbed-vm-"+id); st.Alive() {
			t.Fatalf("ephemeral build VM %s must be stopped during Reconnect", id)
		}
		if _, err := os.Stat(filepath.Join(runDir, id)); !os.IsNotExist(err) {
			t.Fatalf("expected dir for %s removed", id)
		}
	}
}

func TestManagerReconcileOrphansReapsDirlessZombiesAndPreservesOwnedAndStarting(t *testing.T) {
	ctx := context.Background()
	runDir := shortSockDir(t)
	fl := newFakeLauncher()
	mgr, err := NewManager(Config{
		RunDir:          runDir,
		Slice:           "user-1000-hotbed.slice",
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 200 * time.Millisecond,
	}, WithLauncher(fl), WithDialerFactory(startTestGuestd(t)), fakeToolsDiskOpt(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Start a legitimately owned workspace VM.
	if _, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-owned",
		CellHostPath:  "/mnt/cells/ws/ws-owned",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	}); err != nil {
		t.Fatalf("Start ws-owned: %v", err)
	}

	// Simulate two dir-less zombie units in systemd (exact ubu1 incident) plus
	// an untracked released workspace zombie and an in-flight starting VM.
	fl.mu.Lock()
	fl.running["hotbed-vm-base-cladefs-vm-b470a516"] = true
	fl.slices["hotbed-vm-base-cladefs-vm-b470a516"] = "user-1000-hotbed.slice"
	fl.running["hotbed-vm-base-cladefs-vm-cba75ac3"] = true
	fl.slices["hotbed-vm-base-cladefs-vm-cba75ac3"] = "user-1000-hotbed.slice"
	fl.running["hotbed-vm-ws-zombie"] = true
	fl.slices["hotbed-vm-ws-zombie"] = "user-1000-hotbed.slice"
	fl.running["hotbed-vm-ws-booting"] = true
	fl.slices["hotbed-vm-ws-booting"] = "user-1000-hotbed.slice"
	fl.mu.Unlock()

	mgr.mu.Lock()
	mgr.starting["ws-booting"] = struct{}{}
	mgr.mu.Unlock()

	reaped, _ := mgr.ReconcileOrphans(ctx, map[string]bool{"ws-owned": true})
	wantReaped := []string{"base-cladefs-vm-b470a516", "base-cladefs-vm-cba75ac3", "ws-zombie"}
	if !slices.Equal(reaped, wantReaped) {
		t.Fatalf("reaped = %q, want %q", reaped, wantReaped)
	}
	for _, id := range wantReaped {
		if st, _ := fl.Status(ctx, "hotbed-vm-"+id); st.Alive() {
			t.Fatalf("zombie %s still alive after ReconcileOrphans", id)
		}
	}
	for _, keep := range []string{"ws-owned", "ws-booting"} {
		if st, _ := fl.Status(ctx, "hotbed-vm-"+keep); !st.Alive() {
			t.Fatalf("owned/starting VM %s was erroneously killed", keep)
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, toolsDirName, "guestd-test.ext4")); err != nil {
		t.Fatalf("ReconcileOrphans must preserve %s cache dir: %v", toolsDirName, err)
	}
}
