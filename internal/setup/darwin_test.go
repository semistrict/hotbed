//go:build darwin

package setup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"golang.org/x/crypto/ssh"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// TestLiveQEMUAndZFSSparsePunchHole boots a real Ubuntu 26.04 LTS VM under
// QEMU HVF, installs zfsutils-linux, creates the sparse qcow2 ZFS pool,
// writes 128 MiB of incompressible data, deletes it, flushes ZFS deferred
// frees so autotrim=on punches holes back out, and verifies zfs.qcow2 shrinks
// on APFS. Opt-in via HOTBED_TEST_QEMU=1 (downloads ~600 MiB Ubuntu image into
// ~/.hotbed/cache).
func TestLiveQEMUAndZFSSparsePunchHole(t *testing.T) {
	if os.Getenv("HOTBED_TEST_QEMU") != "1" {
		t.Skip("set HOTBED_TEST_QEMU=1 to run live QEMU Ubuntu + ZFS hole-punching test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	arch := runtime.GOARCH
	qemuBin, qemuImg, err := findQEMUBinaries(arch)
	if err != nil {
		t.Fatalf("findQEMUBinaries: %v", err)
	}
	fwCode, fwVars, err := findUEFIFirmware(arch, qemuBin)
	if err != nil {
		t.Fatalf("findUEFIFirmware: %v", err)
	}

	// Reuse ~/.hotbed/cache so we don't re-download Ubuntu across runs,
	// while keeping all VM state isolated in t.TempDir().
	defaultBase, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(defaultBase, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}

	imageURL, imageFilename, sumsURL := UbuntuCloudImageURL(DefaultUbuntuVersion, arch)
	cachedImg, err := ensureCloudImage(ctx, cacheDir, imageURL, imageFilename, sumsURL, os.Stderr)
	if err != nil {
		t.Fatalf("ensureCloudImage: %v", err)
	}
	linuxBin, err := resolveLinuxBinary(ctx, os.Getenv("HOTBED_TEST_BINARY"), "127.0.0.1:7120", cacheDir, arch, nil, os.Stderr)
	if err != nil {
		t.Fatalf("resolveLinuxBinary: %v", err)
	}

	baseDir := t.TempDir()
	p := NewVMPaths(baseDir)
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sshPort, err := pickSSHPort()
	if err != nil {
		t.Fatal(err)
	}
	cfg := VMConfig{
		Controller:    "127.0.0.1:7120",
		User:          "hotbedtest",
		Hostname:      "hotbed-test-vm",
		Arch:          arch,
		UbuntuVersion: DefaultUbuntuVersion,
		ImageURL:      imageURL,
		CPUs:          4,
		MemoryBytes:   4 << 30,
		RootSizeBytes: 16 << 30,
		ZFSSizeBytes:  64 << 30,
		SwapSizeBytes: 2 << 30,
		SSHPort:       sshPort,
		QEMUBinary:    qemuBin,
		FirmwareCode:  fwCode,
		StateDir:      DefaultVMStateDir,
	}

	signer, pubKey, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, _, err := ensureVMHostKeypair(p.HostKeyPriv, p.HostKeyPub)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSeedISO(ctx, p.SeedISO, cfg.Hostname, cfg.User, []string{pubKey}, hostKey); err != nil {
		t.Fatal(err)
	}
	if err := ensureVMDisks(ctx, qemuImg, cachedImg, fwVars, cfg, p, os.Stderr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer stopCancel()
		_ = stopVM(stopCtx, cfg, p, os.Stderr)
	})

	initialStat, err := FileDiskStat("zfs", p.ZFSDisk, cfg.ZFSSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("initial zfs.qcow2: allocated=%s virtual=%s", FormatBytes(initialStat.AllocatedBytes), FormatBytes(initialStat.VirtualBytes))
	if initialStat.AllocatedBytes > 2<<20 {
		t.Fatalf("initial zfs.qcow2 too large: %d bytes", initialStat.AllocatedBytes)
	}

	if err := ensureVMRunning(ctx, cfg, p, os.Stderr); err != nil {
		t.Fatal(err)
	}
	cl, err := waitForSSH(ctx, cfg, p, signer, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	// Install zfsutils-linux and create the autotrim ZFS pool + discard swap.
	setupScript := `set -euo pipefail
sudo cloud-init status --wait >/dev/null 2>&1 || true
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq zfsutils-linux
sudo modprobe zfs
sudo zpool create -f -o ashift=12 -o autotrim=on -O compression=zstd -O atime=off -O xattr=sa -m /var/lib/hotbed hotbed ` + zfsDiskByID + `
sudo mkswap -L hotbed-swap ` + swapDiskByID + ` >/dev/null
sudo swapon --discard ` + swapDiskByID + `
`
	if _, err := sshRun(ctx, cl, setupScript); err != nil {
		t.Fatalf("guest ZFS/swap setup failed: %v", err)
	}
	postCreateStat, err := FileDiskStat("zfs", p.ZFSDisk, cfg.ZFSSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after zpool create: zfs.qcow2 allocated=%s", FormatBytes(postCreateStat.AllocatedBytes))

	// Verify Linux hotbed binary runs cleanly inside the VM.
	if err := sshUploadFile(ctx, cl, linuxBin, vmBinPath, "0755"); err != nil {
		t.Fatalf("upload hotbed binary: %v", err)
	}
	verOut, err := sshRun(ctx, cl, vmBinPath+" --json version")
	if err != nil {
		t.Fatalf("guest hotbed version failed: %v (%s)", err, verOut)
	}
	var verEnv struct {
		OK   bool `json:"ok"`
		Data struct {
			OS string `json:"os"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(verOut), &verEnv); err != nil || !verEnv.OK || verEnv.Data.OS != "linux" {
		t.Fatalf("guest hotbed version = %q, err = %v", verOut, err)
	}

	// Write 128 MiB of incompressible random data into a ZFS dataset and sync.
	writeScript := `set -euo pipefail
sudo zfs create hotbed/testvol
sudo dd if=/dev/urandom of=/var/lib/hotbed/testvol/blob.bin bs=1M count=128 status=none
sync
`
	if _, err := sshRun(ctx, cl, writeScript); err != nil {
		t.Fatalf("write test data: %v", err)
	}
	grownStat, err := FileDiskStat("zfs", p.ZFSDisk, cfg.ZFSSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after 128 MiB write: zfs.qcow2 allocated=%s", FormatBytes(grownStat.AllocatedBytes))
	if grownStat.AllocatedBytes < 100<<20 {
		t.Fatalf("expected zfs.qcow2 to grow past 100 MiB after 128 MiB random write, got %s", FormatBytes(grownStat.AllocatedBytes))
	}

	// Destroy the dataset, wait for ZFS async_destroy, and advance 4 TXGs so
	// ZFS drains its ms_defer[TXG_DEFER_SIZE=2] deferred-free queues and
	// autotrim=on with zfs_trim_txg_batch=1 punches holes out of zfs.qcow2 on
	// APFS (without manual full-pool zpool trim initializing unused metaslabs).
	diag, err := sshRun(ctx, cl, `set -e
echo 1 | sudo tee /sys/module/zfs/parameters/zfs_trim_txg_batch >/dev/null
echo "1. after dd:      $(cat /sys/block/sdb/stat)"
sudo zfs destroy -r hotbed/testvol
sudo zpool wait -t free hotbed
for i in 1 2 3 4; do sudo zpool sync hotbed; done
sleep 3
echo "2. after autotrim: $(cat /sys/block/sdb/stat)"
sudo zpool list -v hotbed
`)
	if err != nil {
		t.Fatalf("destroy + trim: %v", err)
	}
	t.Logf("guest diagnostics:\n%s", diag)
	trimmedStat, err := FileDiskStat("zfs", p.ZFSDisk, cfg.ZFSSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	reclaimed := grownStat.AllocatedBytes - trimmedStat.AllocatedBytes
	t.Logf("after destroy + autotrim sync: zfs.qcow2 allocated=%s (reclaimed %s)",
		FormatBytes(trimmedStat.AllocatedBytes), FormatBytes(reclaimed))

	// Close SSH and shut down cleanly to see if any remaining discards flush on close.
	cl.Close()
	if err := stopVM(ctx, cfg, p, os.Stderr); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	afterStopStat, err := FileDiskStat("zfs", p.ZFSDisk, cfg.ZFSSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	infoOut, _ := exec.Command(qemuImg, "info", p.ZFSDisk).CombinedOutput()
	t.Logf("after stopVM: zfs.qcow2 allocated=%s logical=%s\n%s",
		FormatBytes(afterStopStat.AllocatedBytes), FormatBytes(afterStopStat.LogicalBytes), string(infoOut))
	if trimmedStat.AllocatedBytes > 32<<20 {
		t.Fatalf("expected zfs.qcow2 to punch back under 32 MiB after trim, still %s (after stop: %s)",
			FormatBytes(trimmedStat.AllocatedBytes), FormatBytes(afterStopStat.AllocatedBytes))
	}
}

func TestInitVMWorker(t *testing.T) {
	baseCfg := VMConfig{
		Controller:     "ctrl.ts.net:7120",
		User:           "alice",
		Hostname:       "mac-worker",
		Arch:           "arm64",
		StateDir:       DefaultVMStateDir,
		Slice:          "user-hotbed.slice",
		UnitProperties: []string{"AmbientCapabilities=CAP_SYS_ADMIN", "DeviceAllow=/dev/kvm"},
	}
	const helpWithZFSDevice = "Usage: hotbed worker init [<controller>]\nFlags:\n  --zfs-device=STRING  Linux: block device path under /dev/\n"

	t.Run("delegates provisioning to hotbed --json worker init --zfs-device", func(t *testing.T) {
		var cmds []string
		run := func(_ context.Context, cmd string) (string, error) {
			cmds = append(cmds, cmd)
			if strings.Contains(cmd, "worker init --help") {
				return helpWithZFSDevice, nil
			}
			return `{"ok":true,"command":"worker init","data":{"platform":"linux","dir":"/var/lib/hotbed","stateDir":"/var/lib/hotbed","controller":"ctrl.ts.net:7120","user":"alice","node":"mac-worker","clusterId":"cl-1","storage":"zfs-device","running":true,"service":"hotbed-worker.service"}}`, nil
		}

		res, err := initVMWorkerWith(t.Context(), run, baseCfg, false, nil)
		if err != nil {
			t.Fatalf("initVMWorkerWith: %v", err)
		}
		if len(cmds) != 2 {
			t.Fatalf("expected 2 commands (help probe + worker init), got %d: %q", len(cmds), cmds)
		}
		if !strings.Contains(cmds[0], "worker init --help") {
			t.Errorf("first command %q was not --help probe", cmds[0])
		}
		initCmd := cmds[1]
		for _, want := range []string{
			vmBinPath,
			"--json",
			"worker init",
			baseCfg.Controller,
			"--dir " + baseCfg.StateDir,
			"--user " + baseCfg.User,
			"--zfs-device " + zfsDiskByID,
			"--fstrim",
			"--hostname " + baseCfg.Hostname,
			"--slice " + baseCfg.Slice,
			"--unit-property",
			"AmbientCapabilities=CAP_SYS_ADMIN",
			"DeviceAllow=/dev/kvm",
		} {
			if !strings.Contains(initCmd, want) {
				t.Errorf("worker init command %q missing %q", initCmd, want)
			}
		}
		if strings.Contains(initCmd, "--no-start") {
			t.Errorf("worker init command unexpectedly included --no-start: %q", initCmd)
		}
		if res.ClusterID != "cl-1" || res.Node != "mac-worker" || res.Controller != baseCfg.Controller || res.User != "alice" || res.Service != "hotbed-worker.service" {
			t.Fatalf("unexpected Result: %+v", res)
		}
	})

	t.Run("version skew when guest binary lacks --zfs-device fails with actionable suggestion", func(t *testing.T) {
		var cmds []string
		run := func(_ context.Context, cmd string) (string, error) {
			cmds = append(cmds, cmd)
			if strings.Contains(cmd, "worker init --help") {
				return "Usage: hotbed worker init [<controller>]\nFlags:\n  --dir=PATH\n", nil
			}
			t.Fatalf("should not invoke worker init after version-skew probe failed: %q", cmd)
			return "", nil
		}
		_, err := initVMWorkerWith(t.Context(), run, baseCfg, false, nil)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
			t.Fatalf("expected INVALID_ARGS *api.Error, got %v", err)
		}
		if !strings.Contains(apiErr.Message, "--zfs-device") || !strings.Contains(apiErr.Message, "version skew") {
			t.Errorf("message = %q, want mention of --zfs-device and version skew", apiErr.Message)
		}
		if !strings.Contains(apiErr.Suggestion, "hotbed admin upgrade") || !strings.Contains(apiErr.Suggestion, "--binary") {
			t.Errorf("suggestion = %q, want mention of hotbed admin upgrade and --binary", apiErr.Suggestion)
		}
		if len(cmds) != 1 {
			t.Fatalf("expected only help probe to run, got %q", cmds)
		}
	})

	t.Run("help probe failure reports actionable error", func(t *testing.T) {
		run := func(_ context.Context, _ string) (string, error) {
			return "", errors.New("exit status 1: unknown command")
		}
		_, err := initVMWorkerWith(t.Context(), run, baseCfg, false, nil)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
			t.Fatalf("expected INVALID_ARGS *api.Error, got %v", err)
		}
		if !strings.Contains(apiErr.Suggestion, "hotbed admin upgrade") || !strings.Contains(apiErr.Suggestion, "--binary") {
			t.Errorf("suggestion = %q, want mention of hotbed admin upgrade and --binary", apiErr.Suggestion)
		}
	})

	t.Run("passes --no-start when requested", func(t *testing.T) {
		var gotCmd string
		run := func(_ context.Context, cmd string) (string, error) {
			if strings.Contains(cmd, "worker init --help") {
				return helpWithZFSDevice, nil
			}
			gotCmd = cmd
			return `{"ok":true,"data":{"clusterId":"cl-1","node":"mac-worker","running":false,"service":"hotbed-worker.service"}}`, nil
		}
		res, err := initVMWorkerWith(t.Context(), run, baseCfg, true, nil)
		if err != nil {
			t.Fatalf("initVMWorkerWith(noStart=true): %v", err)
		}
		if !strings.Contains(gotCmd, "--no-start") {
			t.Errorf("worker init command %q missing --no-start", gotCmd)
		}
		if res.Running {
			t.Errorf("expected Running=false, got true")
		}
	})

	t.Run("non-zero exit decodes JSON error envelope", func(t *testing.T) {
		run := func(_ context.Context, cmd string) (string, error) {
			if strings.Contains(cmd, "worker init --help") {
				return helpWithZFSDevice, nil
			}
			return `{"ok":false,"command":"worker init","error":{"code":"NOT_AUTHORIZED","message":"token rejected","suggestion":"check tailnet ACL"}}`, errors.New("Process exited with status 1")
		}
		_, err := initVMWorkerWith(t.Context(), run, baseCfg, false, nil)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected *api.Error, got %T (%v)", err, err)
		}
		if apiErr.Code != api.CodeNotAuthorized || apiErr.Message != "token rejected" || apiErr.Suggestion != "check tailnet ACL" {
			t.Fatalf("unexpected api.Error: %+v", apiErr)
		}
	})

	t.Run("zero exit with ok=false decodes JSON error envelope", func(t *testing.T) {
		run := func(_ context.Context, cmd string) (string, error) {
			if strings.Contains(cmd, "worker init --help") {
				return helpWithZFSDevice, nil
			}
			return `{"ok":false,"error":{"code":"ALREADY_INITIALIZED","message":"already joined to another controller"}}`, nil
		}
		_, err := initVMWorkerWith(t.Context(), run, baseCfg, false, nil)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeAlreadyInitialized {
			t.Fatalf("expected ALREADY_INITIALIZED *api.Error, got %v", err)
		}
	})

	t.Run("zero exit with ok=false and no error fails explicitly", func(t *testing.T) {
		run := func(_ context.Context, cmd string) (string, error) {
			if strings.Contains(cmd, "worker init --help") {
				return helpWithZFSDevice, nil
			}
			return `{"ok":false}`, nil
		}
		if _, err := initVMWorkerWith(t.Context(), run, baseCfg, false, nil); err == nil {
			t.Fatal("expected error when ok=false without error payload")
		}
	})

	t.Run("plain text output fails parse clearly", func(t *testing.T) {
		run := func(_ context.Context, cmd string) (string, error) {
			if strings.Contains(cmd, "worker init --help") {
				return helpWithZFSDevice, nil
			}
			return "initialized worker mac-worker\n", nil
		}
		_, err := initVMWorkerWith(t.Context(), run, baseCfg, false, nil)
		if err == nil || !strings.Contains(err.Error(), "parse hotbed worker init output") {
			t.Fatalf("expected parse error for plain text output, got %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		out  string
	}{
		{name: "ok=true with missing data", out: `{"ok":true}`},
		{name: "ok=true with null data", out: `{"ok":true,"data":null}`},
		{name: "ok=true with empty data", out: `{"ok":true,"data":{}}`},
		{name: "ok=true missing node", out: `{"ok":true,"data":{"clusterId":"cl-1"}}`},
		{name: "ok=true missing clusterId", out: `{"ok":true,"data":{"node":"mac-worker"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, cmd string) (string, error) {
				if strings.Contains(cmd, "worker init --help") {
					return helpWithZFSDevice, nil
				}
				return tc.out, nil
			}
			res, err := initVMWorkerWith(t.Context(), run, baseCfg, false, nil)
			if err == nil {
				t.Fatalf("expected error for %s, got result %+v", tc.name, res)
			}
		})
	}
}

func makeTestELF(class elf.Class, typ elf.Type, machine elf.Machine) []byte {
	var buf bytes.Buffer
	if class == elf.ELFCLASS32 {
		hdr := elf.Header32{
			Ident: [elf.EI_NIDENT]byte{
				0x7f, 'E', 'L', 'F',
				byte(elf.ELFCLASS32),
				byte(elf.ELFDATA2LSB),
				byte(elf.EV_CURRENT),
			},
			Type:    uint16(typ),
			Machine: uint16(machine),
			Version: uint32(elf.EV_CURRENT),
			Ehsize:  52,
		}
		_ = binary.Write(&buf, binary.LittleEndian, hdr)
		return buf.Bytes()
	}
	hdr := elf.Header64{
		Ident: [elf.EI_NIDENT]byte{
			0x7f, 'E', 'L', 'F',
			byte(class),
			byte(elf.ELFDATA2LSB),
			byte(elf.EV_CURRENT),
		},
		Type:    uint16(typ),
		Machine: uint16(machine),
		Version: uint32(elf.EV_CURRENT),
		Ehsize:  64,
	}
	_ = binary.Write(&buf, binary.LittleEndian, hdr)
	return buf.Bytes()
}

func TestVerifyLinuxELF(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	arm64Exec := write("arm64-exec", makeTestELF(elf.ELFCLASS64, elf.ET_EXEC, elf.EM_AARCH64))
	amd64PIE := write("amd64-pie", makeTestELF(elf.ELFCLASS64, elf.ET_DYN, elf.EM_X86_64))
	elf32 := write("elf32", makeTestELF(elf.ELFCLASS32, elf.ET_EXEC, elf.EM_386))
	relObj := write("arm64-rel", makeTestELF(elf.ELFCLASS64, elf.ET_REL, elf.EM_AARCH64))
	notELF := write("script", []byte("#!/bin/sh\necho hi\n"))

	if err := verifyLinuxELF(arm64Exec, "arm64"); err != nil {
		t.Errorf("valid arm64 ET_EXEC rejected: %v", err)
	}
	if err := verifyLinuxELF(amd64PIE, "amd64"); err != nil {
		t.Errorf("valid amd64 ET_DYN rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		path string
		arch string
	}{
		{name: "amd64 binary for arm64 guest", path: amd64PIE, arch: "arm64"},
		{name: "arm64 binary for amd64 guest", path: arm64Exec, arch: "amd64"},
		{name: "32-bit ELF rejected", path: elf32, arch: "amd64"},
		{name: "ET_REL object rejected", path: relObj, arch: "arm64"},
		{name: "non-ELF rejected", path: notELF, arch: "arm64"},
		{name: "unsupported arch rejected", path: arm64Exec, arch: "riscv64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyLinuxELF(tc.path, tc.arch); err == nil {
				t.Fatalf("verifyLinuxELF(%q, %q) succeeded, want error", tc.path, tc.arch)
			}
		})
	}
}

type fakeBuildCluster struct {
	hotbedv1connect.UnimplementedClusterServiceHandler
	getBuild func(context.Context, *pb.GetBuildRequest) (*pb.Build, error)
}

func (f *fakeBuildCluster) GetBuild(ctx context.Context, req *pb.GetBuildRequest) (*pb.Build, error) {
	return f.getBuild(ctx, req)
}

func TestResolveLinuxBinary(t *testing.T) {
	arm64Payload := makeTestELF(elf.ELFCLASS64, elf.ET_EXEC, elf.EM_AARCH64)
	arm64SumBytes := sha256.Sum256(arm64Payload)
	arm64Sum := hex.EncodeToString(arm64SumBytes[:])

	t.Run("downloads and caches verified binary from controller", func(t *testing.T) {
		var downloads atomic.Int32
		srv := connect.NewServer()
		hotbedv1connect.RegisterClusterServiceHandler(srv, &fakeBuildCluster{
			getBuild: func(_ context.Context, req *pb.GetBuildRequest) (*pb.Build, error) {
				if req.Os != "linux" || req.Arch != "arm64" {
					return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no build for %s/%s", req.Os, req.Arch))
				}
				return &pb.Build{
					Number: 42,
					Sha256: arm64Sum,
					Path:   "/builds/42/hotbed-linux-arm64",
				}, nil
			},
		})
		mux := http.NewServeMux()
		connecthttp.Mount(mux, srv)
		mux.HandleFunc("GET /builds/42/hotbed-linux-arm64", func(w http.ResponseWriter, _ *http.Request) {
			downloads.Add(1)
			_, _ = w.Write(arm64Payload)
		})
		ts := httptest.NewServer(mux)
		defer ts.Close()

		cacheDir := t.TempDir()
		addr := ts.Listener.Addr().String()
		got, err := resolveLinuxBinary(t.Context(), "", addr, cacheDir, "arm64", ts.Client(), nil)
		if err != nil {
			t.Fatalf("resolveLinuxBinary: %v", err)
		}
		if got != filepath.Join(cacheDir, "hotbed-linux-arm64") {
			t.Fatalf("path = %q, want cached path", got)
		}
		if downloads.Load() != 1 {
			t.Fatalf("downloads = %d, want 1", downloads.Load())
		}
		gotBytes, err := os.ReadFile(got)
		if err != nil || !bytes.Equal(gotBytes, arm64Payload) {
			t.Fatalf("cached binary mismatch (err=%v)", err)
		}

		// Second call with matching controller SHA-256 reuses cache without re-downloading.
		got2, err := resolveLinuxBinary(t.Context(), "", addr, cacheDir, "arm64", ts.Client(), nil)
		if err != nil {
			t.Fatalf("second resolveLinuxBinary: %v", err)
		}
		if got2 != got || downloads.Load() != 1 {
			t.Fatalf("second call got=%q downloads=%d, want cached reuse with downloads=1", got2, downloads.Load())
		}
	})

	t.Run("rejects sha256 mismatch from controller", func(t *testing.T) {
		srv := connect.NewServer()
		hotbedv1connect.RegisterClusterServiceHandler(srv, &fakeBuildCluster{
			getBuild: func(_ context.Context, _ *pb.GetBuildRequest) (*pb.Build, error) {
				return &pb.Build{
					Number: 42,
					Sha256: strings.Repeat("0", 64),
					Path:   "/builds/42/hotbed-linux-arm64",
				}, nil
			},
		})
		mux := http.NewServeMux()
		connecthttp.Mount(mux, srv)
		mux.HandleFunc("GET /builds/42/hotbed-linux-arm64", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(arm64Payload)
		})
		ts := httptest.NewServer(mux)
		defer ts.Close()

		cacheDir := t.TempDir()
		_, err := resolveLinuxBinary(t.Context(), "", ts.Listener.Addr().String(), cacheDir, "arm64", ts.Client(), nil)
		if err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("resolveLinuxBinary err = %v, want sha256 mismatch", err)
		}
		if _, statErr := os.Stat(filepath.Join(cacheDir, "hotbed-linux-arm64")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("corrupted binary was left in cache: err=%v", statErr)
		}
	})

	t.Run("controller without build suggests --binary", func(t *testing.T) {
		srv := connect.NewServer()
		hotbedv1connect.RegisterClusterServiceHandler(srv, &fakeBuildCluster{
			getBuild: func(_ context.Context, _ *pb.GetBuildRequest) (*pb.Build, error) {
				return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "dev controller has no build"))
			},
		})
		mux := http.NewServeMux()
		connecthttp.Mount(mux, srv)
		ts := httptest.NewServer(mux)
		defer ts.Close()

		_, err := resolveLinuxBinary(t.Context(), "", ts.Listener.Addr().String(), t.TempDir(), "arm64", ts.Client(), nil)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeNotFound || !strings.Contains(apiErr.Suggestion, "--binary") || !strings.Contains(apiErr.Message, "no published Linux") {
			t.Fatalf("err = %v, want NOT_FOUND *api.Error explaining no published Linux build and suggesting --binary", err)
		}
	})

	t.Run("slow binary download ignores short caller Client.Timeout", func(t *testing.T) {
		srv := connect.NewServer()
		hotbedv1connect.RegisterClusterServiceHandler(srv, &fakeBuildCluster{
			getBuild: func(_ context.Context, _ *pb.GetBuildRequest) (*pb.Build, error) {
				return &pb.Build{
					Number: 42,
					Sha256: arm64Sum,
					Path:   "/builds/42/hotbed-linux-arm64",
				}, nil
			},
		})
		mux := http.NewServeMux()
		connecthttp.Mount(mux, srv)
		mux.HandleFunc("GET /builds/42/hotbed-linux-arm64", func(w http.ResponseWriter, _ *http.Request) {
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write(arm64Payload[:len(arm64Payload)/2])
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write(arm64Payload[len(arm64Payload)/2:])
		})
		ts := httptest.NewServer(mux)
		defer ts.Close()

		shortTimeoutClient := *ts.Client()
		shortTimeoutClient.Timeout = 50 * time.Millisecond
		got, err := resolveLinuxBinary(t.Context(), "", ts.Listener.Addr().String(), t.TempDir(), "arm64", &shortTimeoutClient, nil)
		if err != nil {
			t.Fatalf("resolveLinuxBinary failed under short Client.Timeout: %v", err)
		}
		gotBytes, err := os.ReadFile(got)
		if err != nil || !bytes.Equal(gotBytes, arm64Payload) {
			t.Fatalf("downloaded binary mismatch (err=%v)", err)
		}
	})

	t.Run("explicit --binary override validates ELF without calling controller", func(t *testing.T) {
		dir := t.TempDir()
		good := filepath.Join(dir, "hotbed-linux-arm64")
		if err := os.WriteFile(good, arm64Payload, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := resolveLinuxBinary(t.Context(), good, "127.0.0.1:1", t.TempDir(), "arm64", nil, nil)
		if err != nil || got != good {
			t.Fatalf("resolveLinuxBinary(explicit) = (%q, %v), want (%q, nil)", got, err, good)
		}
		if _, err := resolveLinuxBinary(t.Context(), good, "127.0.0.1:1", t.TempDir(), "amd64", nil, nil); err == nil {
			t.Fatal("resolveLinuxBinary accepted arm64 --binary for amd64 guest")
		}
	})
}

func TestIsVMProcessCmd(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "hb")
	p := NewVMPaths(baseDir)
	cfg := VMConfig{
		Hostname:     "mac-worker",
		Arch:         "arm64",
		CPUs:         4,
		MemoryBytes:  4 << 30,
		SSHPort:      22222,
		QEMUBinary:   "/opt/homebrew/bin/qemu-system-aarch64",
		FirmwareCode: "/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
	}
	args, err := QEMUArgs(cfg, p, qmpSocketPath(p))
	if err != nil {
		t.Fatalf("QEMUArgs: %v", err)
	}
	realCmd := cfg.QEMUBinary + " " + strings.Join(args, " ")

	// Long base dir triggers /tmp/hotbed-qmp-<hash>.sock.
	longBase := filepath.Join(t.TempDir(), strings.Repeat("nested-dir-", 10))
	longP := NewVMPaths(longBase)
	longQMP := qmpSocketPath(longP)
	if !strings.HasPrefix(longQMP, "/tmp/hotbed-qmp-") {
		t.Fatalf("expected hashed /tmp QMP socket path, got %q", longQMP)
	}

	// Symlinked base dir: VMPaths uses the symlink, while QEMU args use the resolved target.
	symlinkRoot := t.TempDir()
	realBase := filepath.Join(symlinkRoot, "real-hb")
	linkBase := filepath.Join(symlinkRoot, "link-hb")
	if err := os.MkdirAll(filepath.Join(realBase, "vm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBase, linkBase); err != nil {
		t.Fatal(err)
	}
	realP := NewVMPaths(realBase)
	linkP := NewVMPaths(linkBase)
	resolvedRealPID := evalExistingSymlinks(realP.PIDFile)
	resolvedRealQMP := evalExistingSymlinks(qmpSocketPath(realP))

	// Long symlinked base dir under /tmp (macOS /tmp -> /private/tmp) forces the
	// hashed /tmp/hotbed-qmp-<hash>.sock path regardless of host TMPDIR length.
	longSymTmp, err := os.MkdirTemp("/tmp", "hotbed-long-symlink-test-"+strings.Repeat("x", 60)+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(longSymTmp) })
	longRealBase := filepath.Join(longSymTmp, "real-hb")
	longLinkBase := filepath.Join(longSymTmp, "link-hb")
	if err := os.MkdirAll(filepath.Join(longRealBase, "vm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(longRealBase, longLinkBase); err != nil {
		t.Fatal(err)
	}
	longRealP := NewVMPaths(longRealBase)
	longLinkP := NewVMPaths(longLinkBase)
	if len(longRealP.QMPSock) < 100 || len(longLinkP.QMPSock) < 100 {
		t.Fatalf("expected long symlink QMP socket paths >= 100 bytes, got %d and %d", len(longRealP.QMPSock), len(longLinkP.QMPSock))
	}
	resolvedLongRealQMP := evalExistingSymlinks(qmpSocketPath(longRealP))

	// Base dir containing spaces.
	spaceBase := filepath.Join(t.TempDir(), "my worker dir")
	spaceP := NewVMPaths(spaceBase)

	otherP := NewVMPaths(filepath.Join(t.TempDir(), "other"))

	tests := []struct {
		name string
		cmd  string
		p    VMPaths
		want bool
	}{
		{name: "real arm64 qemu args", cmd: realCmd, p: p, want: true},
		{name: "x86_64 qemu with space in binary path", cmd: "/Applications/My Tools/qemu-system-x86_64 -machine q35 -pidfile " + p.PIDFile, p: p, want: true},
		{name: "pidfile in middle of args", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -pidfile " + p.PIDFile + " -machine virt", p: p, want: true},
		{name: "pidfile with equals syntax", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt --pidfile=" + p.PIDFile + " -m 4096", p: p, want: true},
		{name: "quoted pidfile with spaces", cmd: `/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile "` + spaceP.PIDFile + `" -m 4096`, p: spaceP, want: true},
		{name: "unquoted pidfile with spaces", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + spaceP.PIDFile + " -m 4096", p: spaceP, want: true},
		{name: "long path matched via hashed qmp socket", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -qmp unix:" + longQMP + ",server=on,wait=off", p: longP, want: true},
		{name: "qmp unix socket at end of line", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -qmp unix:" + qmpSocketPath(p), p: p, want: true},
		{name: "qmp unix socket quoted", cmd: `/opt/homebrew/bin/qemu-system-aarch64 -qmp "unix:` + qmpSocketPath(p) + `,server=on,wait=off"`, p: p, want: true},
		{name: "chardev path matching qmp socket", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -chardev socket,id=qmp,path=" + qmpSocketPath(p) + ",server=on,wait=off", p: p, want: true},
		{name: "qmp unix path= syntax", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -qmp unix:path=" + qmpSocketPath(p) + ",server=on,wait=off", p: p, want: true},
		{name: "symlink resolved pidfile matches", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + resolvedRealPID, p: linkP, want: true},
		{name: "symlink resolved qmp socket matches", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -qmp unix:" + resolvedRealQMP + ",server=on,wait=off", p: linkP, want: true},
		{name: "long symlink under /tmp resolved qmp socket matches", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -qmp unix:" + resolvedLongRealQMP + ",server=on,wait=off", p: longLinkP, want: true},
		{name: "other VM referencing our firmware and disks under vmDir", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + otherP.PIDFile + " -drive if=pflash,format=raw,file=" + p.EFIVars + " -drive file=" + p.RootDisk + " -qmp unix:" + qmpSocketPath(otherP) + ",server=on,wait=off", p: p, want: false},
		{name: "other VM mentioning our pidfile under -drive", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + otherP.PIDFile + " -drive file=" + p.PIDFile, p: p, want: false},
		{name: "pidfile suffix trap", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.PIDFile + ".bak", p: p, want: false},
		{name: "qmp unix suffix trap", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -qmp unix:" + qmpSocketPath(p) + ".bak,server=on,wait=off", p: p, want: false},
		{name: "chardev path suffix trap", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -chardev socket,id=qmp,path=" + qmpSocketPath(p) + "2,server=on", p: p, want: false},
		{name: "chardev non-path key boundary trap", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -chardev socket,id=qmp,mypath=" + qmpSocketPath(p) + ",server=on", p: p, want: false},
		{name: "unrelated sleep process", cmd: "/bin/sleep 60", p: p, want: false},
		{name: "grep mentioning qemu-system and pidfile", cmd: "/usr/bin/grep qemu-system " + p.PIDFile, p: p, want: false},
		{name: "env wrapper running qemu-system", cmd: "/usr/bin/env /opt/homebrew/bin/qemu-system-aarch64 -pidfile " + p.PIDFile, p: p, want: false},
		{name: "shell wrapper mentioning qemu-system", cmd: "/bin/sh -c qemu-system-aarch64 -pidfile " + p.PIDFile, p: p, want: false},
		{name: "other VM qemu process", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + otherP.PIDFile, p: p, want: false},
		{name: "sibling VMDir prefix trap", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.VMDir + "2/qemu.pid", p: p, want: false},
		{name: "empty command line", cmd: "   ", p: p, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVMProcessCmd(tc.cmd, tc.p); got != tc.want {
				t.Errorf("isVMProcessCmd(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestParsePSStatCommand(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantCmd   string
		wantState vmProcState
	}{
		{name: "normal process", in: "S+   /opt/homebrew/bin/qemu-system-aarch64 -machine virt\n", wantCmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt", wantState: vmProcRunning},
		{name: "zombie Z", in: "Z    (qemu-system-aarch64)\n", wantState: vmProcMissing},
		{name: "zombie ZN", in: "ZN   /opt/homebrew/bin/qemu-system-aarch64 -pidfile /tmp/vm/qemu.pid\n", wantState: vmProcMissing},
		{name: "zombie stat only", in: "Z\n", wantState: vmProcMissing},
		{name: "empty", in: "   \n", wantState: vmProcMissing},
		{name: "stat only", in: "S\n", wantState: vmProcUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotCmd, gotState := parsePSStatCommand(tc.in)
			if gotState != tc.wantState || gotCmd != tc.wantCmd {
				t.Errorf("parsePSStatCommand(%q) = (%q, %v), want (%q, %v)", tc.in, gotCmd, gotState, tc.wantCmd, tc.wantState)
			}
		})
	}
}

func TestVMProcessStateRecycledPID(t *testing.T) {
	// Spawn a real unrelated child process to simulate a recycled PID after
	// host reboot or crash.
	sleepCmd := exec.Command("/bin/sleep", "30")
	if err := sleepCmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = sleepCmd.Process.Kill()
		_ = sleepCmd.Wait()
	})

	p := NewVMPaths(t.TempDir())
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(sleepCmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qmpSock := qmpSocketPath(p)
	if err := os.WriteFile(qmpSock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(qmpSock) })

	start := time.Now()
	pid, state, err := vmProcessState(t.Context(), p)
	elapsed := time.Since(start)
	if err != nil || state != vmProcMissing || pid != 0 {
		t.Fatalf("vmProcessState for recycled PID = (%d, %v, %v), want (0, vmProcMissing, nil)", pid, state, err)
	}
	if elapsed > time.Second {
		t.Fatalf("vmProcessState took %v for stale recycled PID; expected fast rejection", elapsed)
	}

	// stopVM must NOT signal the unrelated process, and MUST clean stale pid/qmp files.
	if err := stopVM(t.Context(), VMConfig{}, p, nil); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	if err := syscall.Kill(sleepCmd.Process.Pid, 0); err != nil {
		t.Fatalf("unrelated process %d was killed by stopVM: %v", sleepCmd.Process.Pid, err)
	}
	if _, err := os.Stat(p.PIDFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale qemu.pid still exists after stopVM: err=%v", err)
	}
	if _, err := os.Stat(qmpSock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale qmp.sock still exists after stopVM: err=%v", err)
	}
}

func TestVMProcessStateLiveAndMissing(t *testing.T) {
	p := NewVMPaths(t.TempDir())
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Missing PID file -> not running.
	if pid, state, err := vmProcessState(t.Context(), p); err != nil || state != vmProcMissing || pid != 0 {
		t.Fatalf("missing pidfile: got (%d, %v, %v), want (0, vmProcMissing, nil)", pid, state, err)
	}

	// Invalid PID file -> not running.
	if err := os.WriteFile(p.PIDFile, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pid, state, err := vmProcessState(t.Context(), p); err != nil || state != vmProcMissing || pid != 0 {
		t.Fatalf("invalid pidfile: got (%d, %v, %v), want (0, vmProcMissing, nil)", pid, state, err)
	}

	// Live PID matching this VM's QEMU command line -> running even when QMP
	// socket is not yet up.
	orig := processCommandLine
	t.Cleanup(func() { processCommandLine = orig })
	myPID := os.Getpid()
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid == myPID {
			return "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcMissing, nil
	}
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(myPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pid, state, err := vmProcessState(t.Context(), p); err != nil || state != vmProcRunning || pid != myPID {
		t.Fatalf("matching qemu pid: got (%d, %v, %v), want (%d, vmProcRunning, nil)", pid, state, err, myPID)
	}
}

func TestStopVMDoesNotKillRecycledPIDMidShutdown(t *testing.T) {
	// Spawn a real unrelated process. Pretend on the very first check
	// (vmProcessState) it was QEMU, and immediately afterward its PID was
	// recycled to /bin/sleep before stopVM could send SIGTERM/SIGKILL.
	sleepCmd := exec.Command("/bin/sleep", "30")
	if err := sleepCmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = sleepCmd.Process.Kill()
		_ = sleepCmd.Wait()
	})

	p := NewVMPaths(t.TempDir())
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetPID := sleepCmd.Process.Pid
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(targetPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qmpSock := qmpSocketPath(p)
	if err := os.WriteFile(qmpSock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(qmpSock) })

	orig := processCommandLine
	t.Cleanup(func() { processCommandLine = orig })
	var calls atomic.Int32
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid != targetPID {
			return "", vmProcMissing, nil
		}
		if calls.Add(1) == 1 {
			return "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "/bin/sleep 30", vmProcRunning, nil
	}

	start := time.Now()
	if err := stopVM(t.Context(), VMConfig{}, p, nil); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stopVM took %v; expected quick exit once PID no longer matched QEMU", elapsed)
	}
	if err := syscall.Kill(targetPID, 0); err != nil {
		t.Fatalf("recycled process %d was signaled by stopVM: %v", targetPID, err)
	}
	if _, err := os.Stat(p.PIDFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("qemu.pid was not removed: err=%v", err)
	}
	if _, err := os.Stat(qmpSock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("qmp.sock was not removed: err=%v", err)
	}
}

func TestDarwinProcessCommandLineErrors(t *testing.T) {
	origRunPS := runPS
	t.Cleanup(func() { runPS = origRunPS })

	// Obtain a definitely-dead PID by running and reaping a short-lived child.
	deadCmd := exec.Command("/usr/bin/true")
	if err := deadCmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	deadPID := deadCmd.Process.Pid
	livePID := os.Getpid()

	exit1Err := exec.Command("/usr/bin/false").Run()
	if exit1Err == nil {
		t.Fatal("expected /usr/bin/false to fail")
	}

	t.Run("canceled context returns vmProcUnknown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		runPS = func(_ context.Context, _ int) ([]byte, error) {
			t.Fatal("runPS should not be called when ctx is already canceled")
			return nil, nil
		}
		_, state, err := darwinProcessCommandLine(ctx, livePID)
		if state != vmProcUnknown || !errors.Is(err, context.Canceled) {
			t.Fatalf("got (%v, %v), want (vmProcUnknown, context.Canceled)", state, err)
		}
	})

	t.Run("ps exec failure returns vmProcUnknown", func(t *testing.T) {
		runPS = func(_ context.Context, _ int) ([]byte, error) {
			return nil, exec.ErrNotFound
		}
		_, state, err := darwinProcessCommandLine(t.Context(), livePID)
		if state != vmProcUnknown || !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("got (%v, %v), want (vmProcUnknown, exec.ErrNotFound)", state, err)
		}
	})

	t.Run("ps timeout returns vmProcUnknown", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		runPS = func(psCtx context.Context, _ int) ([]byte, error) {
			<-psCtx.Done()
			return nil, psCtx.Err()
		}
		_, state, err := darwinProcessCommandLine(ctx, livePID)
		if state != vmProcUnknown || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got (%v, %v), want (vmProcUnknown, context.DeadlineExceeded)", state, err)
		}
	})

	t.Run("ps exit 1 with empty output on live PID returns vmProcUnknown", func(t *testing.T) {
		runPS = func(_ context.Context, _ int) ([]byte, error) {
			return nil, exit1Err
		}
		_, state, err := darwinProcessCommandLine(t.Context(), livePID)
		if state != vmProcUnknown || err == nil {
			t.Fatalf("got (%v, %v), want (vmProcUnknown, non-nil error)", state, err)
		}
	})

	t.Run("ps exit 1 with empty output on missing PID returns vmProcMissing", func(t *testing.T) {
		runPS = func(_ context.Context, _ int) ([]byte, error) {
			return nil, exit1Err
		}
		_, state, err := darwinProcessCommandLine(t.Context(), deadPID)
		if state != vmProcMissing || err != nil {
			t.Fatalf("got (%v, %v), want (vmProcMissing, nil)", state, err)
		}
	})

	t.Run("ps stat-only output on live PID returns vmProcUnknown", func(t *testing.T) {
		runPS = func(_ context.Context, _ int) ([]byte, error) {
			return []byte("S\n"), nil
		}
		_, state, err := darwinProcessCommandLine(t.Context(), livePID)
		if state != vmProcUnknown || err == nil {
			t.Fatalf("got (%v, %v), want (vmProcUnknown, non-nil error)", state, err)
		}
	})
}

func TestStopVMAndDestroyAbortOnUnknownProcessState(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	origProc := processCommandLine
	origLaunchctl := runLaunchctl
	t.Cleanup(func() {
		processCommandLine = origProc
		runLaunchctl = origLaunchctl
	})
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
		return []byte("Could not find service"), errors.New("exit status 113")
	}

	baseDir := filepath.Join(t.TempDir(), "worker-base")
	p := NewVMPaths(baseDir)
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}
	myPID := os.Getpid()
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(myPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qmpSock := qmpSocketPath(p)
	if err := os.WriteFile(qmpSock, []byte("live-qmp"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(qmpSock) })
	if err := os.WriteFile(p.RootDisk, []byte("live-root-disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ZFSDisk, []byte("live-zfs-disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	probeErr := errors.New("ps inspection failed")
	processCommandLine = func(_ context.Context, _ int) (string, vmProcState, error) {
		return "", vmProcUnknown, probeErr
	}

	// stopVM must propagate the error and preserve qemu.pid and qmp.sock.
	if err := stopVM(t.Context(), VMConfig{}, p, nil); !errors.Is(err, probeErr) {
		t.Fatalf("stopVM err = %v, want %v", err, probeErr)
	}
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("stopVM removed qemu.pid on vmProcUnknown: %v", err)
	}
	if _, err := os.Stat(qmpSock); err != nil {
		t.Errorf("stopVM removed qmp.sock on vmProcUnknown: %v", err)
	}

	// ensureVMRunning must also refuse to unlink files or double-boot on vmProcUnknown.
	if err := ensureVMRunning(t.Context(), VMConfig{}, p, nil); !errors.Is(err, probeErr) {
		t.Fatalf("ensureVMRunning err = %v, want %v", err, probeErr)
	}
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("ensureVMRunning removed qemu.pid on vmProcUnknown: %v", err)
	}
	if _, err := os.Stat(qmpSock); err != nil {
		t.Errorf("ensureVMRunning removed qmp.sock on vmProcUnknown: %v", err)
	}

	// cleanStaleVMFiles must NOT unlink files on vmProcUnknown.
	cleanStaleVMFiles(t.Context(), p)
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("cleanStaleVMFiles removed qemu.pid on vmProcUnknown: %v", err)
	}
	if _, err := os.Stat(qmpSock); err != nil {
		t.Errorf("cleanStaleVMFiles removed qmp.sock on vmProcUnknown: %v", err)
	}

	// Destroy must abort and leave VM disks, pidfile, and qmp socket intact!
	if _, err := Destroy(t.Context(), DestroyOptions{Dir: baseDir}); !errors.Is(err, probeErr) {
		t.Fatalf("Destroy err = %v, want %v", err, probeErr)
	}
	for _, path := range []string{p.VMDir, p.RootDisk, p.ZFSDisk, p.PIDFile, qmpSock} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("Destroy deleted %s after stopVM inspection error: %v", path, err)
		}
	}

	// Mid-shutdown inspection error during stopVM polling must also abort without unlinking.
	var calls atomic.Int32
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid != myPID {
			return "", vmProcMissing, nil
		}
		if calls.Add(1) == 1 {
			return "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcUnknown, probeErr
	}
	if err := stopVM(t.Context(), VMConfig{}, p, nil); !errors.Is(err, probeErr) {
		t.Fatalf("stopVM mid-shutdown err = %v, want %v", err, probeErr)
	}
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("stopVM removed qemu.pid on mid-shutdown vmProcUnknown: %v", err)
	}
	if _, err := os.Stat(qmpSock); err != nil {
		t.Errorf("stopVM removed qmp.sock on mid-shutdown vmProcUnknown: %v", err)
	}

	// Destroy still succeeds cleanly once the VM is already stopped (no PID file).
	if err := os.Remove(p.PIDFile); err != nil {
		t.Fatal(err)
	}
	if _, err := Destroy(t.Context(), DestroyOptions{Dir: baseDir}); err != nil {
		t.Fatalf("Destroy with no PID file: %v", err)
	}
	if _, err := os.Stat(p.VMDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Destroy did not remove VMDir when already stopped: %v", err)
	}
}

func TestLaunchAgentPlist(t *testing.T) {
	got := LaunchAgentPlist("com.semistrict.hotbed.worker.test", "/usr/local/bin/hotbed", []string{"worker", "supervise", "--dir", "/tmp/a&b<c>"}, "/tmp/supervise.log")
	for _, want := range []string{
		"<key>Label</key>\n  <string>com.semistrict.hotbed.worker.test</string>",
		"<string>/usr/local/bin/hotbed</string>",
		"<string>worker</string>",
		"<string>supervise</string>",
		"<string>--dir</string>",
		"<string>/tmp/a&amp;b&lt;c&gt;</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>KeepAlive</key>\n  <true/>",
		"<key>AbandonProcessGroup</key>\n  <true/>",
		"<key>ExitTimeOut</key>\n  <integer>60</integer>",
		"<key>StandardOutPath</key>\n  <string>/tmp/supervise.log</string>",
		"<key>StandardErrorPath</key>\n  <string>/tmp/supervise.log</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LaunchAgentPlist missing %q\ngot:\n%s", want, got)
		}
	}
}

func TestLaunchAgentLifecycle(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	origLaunchctl := runLaunchctl
	origPoll := supervisorPollInterval
	t.Cleanup(func() {
		runLaunchctl = origLaunchctl
		supervisorPollInterval = origPoll
	})
	supervisorPollInterval = time.Millisecond

	var (
		mu             sync.Mutex
		calls          [][]string
		loaded         = map[string]string{}
		bootoutDelays  = map[string]int{} // remaining "print" calls still showing loaded after bootout
		failBootoutErr error
	)
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, slices.Clone(args))
		if len(args) == 0 {
			return nil, errors.New("no args")
		}
		switch args[0] {
		case "bootstrap":
			// args: bootstrap gui/<uid> <plistPath>
			if len(args) != 3 {
				return nil, errors.New("bad bootstrap args")
			}
			b, err := os.ReadFile(args[2])
			if err != nil {
				return nil, err
			}
			label := strings.TrimSuffix(filepath.Base(args[2]), ".plist")
			loaded[args[1]+"/"+label] = string(b)
			return nil, nil
		case "print":
			target := args[1]
			if rem := bootoutDelays[target]; rem > 0 {
				bootoutDelays[target] = rem - 1
				if rem-1 == 0 {
					delete(loaded, target)
				}
			}
			if body, ok := loaded[target]; ok {
				return []byte(body), nil
			}
			return []byte("Could not find service"), errors.New("exit status 113")
		case "bootout":
			target := args[1]
			if failBootoutErr != nil {
				delete(loaded, target)
				return []byte("No such process"), failBootoutErr
			}
			if bootoutDelays[target] == 0 {
				delete(loaded, target)
			}
			return nil, nil
		default:
			return nil, errors.New("unexpected launchctl subcommand: " + args[0])
		}
	}

	baseDir := filepath.Join(t.TempDir(), "worker-base")
	p := NewVMPaths(baseDir)
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := launchDomain() + "/" + launchAgentLabel(baseDir)
	plistPath := launchAgentPath(baseDir)

	// 1. startSupervisor writes plist and calls launchctl bootstrap.
	if err := startSupervisor(t.Context(), p); err != nil {
		t.Fatalf("startSupervisor: %v", err)
	}
	if !supervised(t.Context(), p) {
		t.Fatal("expected supervised() == true after startSupervisor")
	}
	plistBytes, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if !strings.Contains(string(plistBytes), "<string>supervise</string>") || !strings.Contains(string(plistBytes), baseDir) {
		t.Fatalf("unexpected plist contents:\n%s", string(plistBytes))
	}

	// 2. stopSupervisor calls launchctl bootout and waits until print says gone.
	mu.Lock()
	bootoutDelays[target] = 3
	mu.Unlock()
	if err := stopSupervisor(t.Context(), VMConfig{}, p); err != nil {
		t.Fatalf("stopSupervisor: %v", err)
	}
	if supervised(t.Context(), p) {
		t.Fatal("expected supervised() == false after stopSupervisor")
	}

	// 2b. Legacy LaunchAgent running qemu-system directly stops the VM (cleaning stale files) before bootout.
	if err := os.WriteFile(p.PIDFile, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyQMP := qmpSocketPath(p)
	if err := os.WriteFile(legacyQMP, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	loaded[target] = "program = /opt/homebrew/bin/qemu-system-aarch64"
	mu.Unlock()
	if err := stopSupervisor(t.Context(), VMConfig{}, p); err != nil {
		t.Fatalf("stopSupervisor (legacy qemu): %v", err)
	}
	if _, err := os.Stat(p.PIDFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("legacy stopSupervisor left stale qemu.pid: err=%v", err)
	}
	if _, err := os.Stat(legacyQMP); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("legacy stopSupervisor left stale qmp.sock: err=%v", err)
	}

	// 3. stopSupervisor tolerates a race where the job exits right before bootout.
	mu.Lock()
	loaded[target] = "worker supervise"
	failBootoutErr = errors.New("exit status 3")
	mu.Unlock()
	if err := stopSupervisor(t.Context(), VMConfig{}, p); err != nil {
		t.Fatalf("stopSupervisor with already-gone race: %v", err)
	}
	mu.Lock()
	failBootoutErr = nil
	mu.Unlock()

	// 4. disableSupervisor boots out and removes plist file.
	if err := startSupervisor(t.Context(), p); err != nil {
		t.Fatalf("startSupervisor (2): %v", err)
	}
	if err := disableSupervisor(t.Context(), VMConfig{}, p); err != nil {
		t.Fatalf("disableSupervisor: %v", err)
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("plist should be removed by disableSupervisor, stat err=%v", err)
	}

	// 5. Destroy boots out the LaunchAgent, removes plist, cleans stale pid/qmp, and deletes VMDir.
	if err := startSupervisor(t.Context(), p); err != nil {
		t.Fatalf("startSupervisor (3): %v", err)
	}
	if err := os.WriteFile(p.PIDFile, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qmpSock := qmpSocketPath(p)
	if err := os.WriteFile(qmpSock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Destroy(t.Context(), DestroyOptions{Dir: baseDir})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if supervised(t.Context(), p) {
		t.Fatal("Destroy left LaunchAgent loaded")
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Destroy left plist behind: %v", err)
	}
	if _, err := os.Stat(qmpSock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Destroy left qmp.sock behind: %v", err)
	}
	if !slices.Contains(res.Removed, plistPath) || !slices.Contains(res.Removed, p.VMDir) {
		t.Errorf("Destroy.Removed = %v, want %q and %q", res.Removed, plistPath, p.VMDir)
	}

	// Confirm bootstrap and bootout were actually invoked.
	mu.Lock()
	var sawBootstrap, sawBootout bool
	for _, c := range calls {
		if len(c) >= 1 && c[0] == "bootstrap" {
			sawBootstrap = true
		}
		if len(c) >= 2 && c[0] == "bootout" && c[1] == target {
			sawBootout = true
		}
	}
	mu.Unlock()
	if !sawBootstrap || !sawBootout {
		t.Fatalf("expected both bootstrap (%v) and bootout (%v) calls; got %v", sawBootstrap, sawBootout, calls)
	}
}

func TestCleanStaleVMFiles(t *testing.T) {
	p := NewVMPaths(t.TempDir())
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}
	qmpSock := qmpSocketPath(p)
	t.Cleanup(func() { _ = os.Remove(qmpSock) })

	// When VM is not running (e.g., stale recycled PID after reboot), cleanStaleVMFiles removes both files.
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qmpSock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanStaleVMFiles(t.Context(), p)
	if _, err := os.Stat(p.PIDFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected stale qemu.pid removed, err=%v", err)
	}
	if _, err := os.Stat(qmpSock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected stale qmp.sock removed, err=%v", err)
	}

	// When VM IS running, cleanStaleVMFiles leaves both files untouched.
	orig := processCommandLine
	t.Cleanup(func() { processCommandLine = orig })
	myPID := os.Getpid()
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid == myPID {
			return "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcMissing, nil
	}
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(myPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qmpSock, []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanStaleVMFiles(t.Context(), p)
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("live qemu.pid was unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(qmpSock); err != nil {
		t.Errorf("live qmp.sock was unexpectedly removed: %v", err)
	}
}

func TestSupervisorStateAndStopSupervisorErrors(t *testing.T) {
	origLaunchctl := runLaunchctl
	origPoll := supervisorPollInterval
	t.Cleanup(func() {
		runLaunchctl = origLaunchctl
		supervisorPollInterval = origPoll
	})
	supervisorPollInterval = time.Millisecond

	p := NewVMPaths(filepath.Join(t.TempDir(), "worker-base"))

	t.Run("supervisorState checks ctx.Err first", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		runLaunchctl = func(_ context.Context, _ ...string) ([]byte, error) {
			t.Fatal("runLaunchctl should not run on canceled context")
			return nil, nil
		}
		loaded, err := supervisorState(ctx, p)
		if loaded || !errors.Is(err, context.Canceled) {
			t.Fatalf("supervisorState = (%v, %v), want (false, context.Canceled)", loaded, err)
		}
	})

	t.Run("supervisorState recognizes explicit missing service responses", func(t *testing.T) {
		exit113Err := exec.Command("/bin/sh", "-c", "exit 113").Run()
		if exit113Err == nil {
			t.Fatal("expected exit 113 error")
		}
		for _, tc := range []struct {
			name string
			out  string
			err  error
		}{
			{name: "exit 113 empty output", out: "", err: exit113Err},
			{name: "Could not find service", out: `Could not find service "com.semistrict.hotbed.worker"`, err: errors.New("exit status 113")},
			{name: "No such process", out: "Boot-out failed: 3: No such process", err: errors.New("exit status 3")},
			{name: "Could not find domain", out: "Could not find domain for gui/501", err: errors.New("exit status 112")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				runLaunchctl = func(_ context.Context, _ ...string) ([]byte, error) {
					return []byte(tc.out), tc.err
				}
				loaded, err := supervisorState(t.Context(), p)
				if loaded || err != nil {
					t.Fatalf("supervisorState = (%v, %v), want (false, nil)", loaded, err)
				}
			})
		}
	})

	t.Run("supervisorState propagates unexpected launchctl error", func(t *testing.T) {
		transientErr := errors.New("exit status 1")
		runLaunchctl = func(_ context.Context, _ ...string) ([]byte, error) {
			return []byte("Operation not permitted"), transientErr
		}
		loaded, err := supervisorState(t.Context(), p)
		if loaded || !errors.Is(err, transientErr) {
			t.Fatalf("supervisorState = (%v, %v), want (false, wrapped %v)", loaded, err, transientErr)
		}
	})

	t.Run("stopSupervisor propagates initial print failure", func(t *testing.T) {
		permErr := errors.New("exit status 1")
		runLaunchctl = func(_ context.Context, _ ...string) ([]byte, error) {
			return []byte("Operation not permitted"), permErr
		}
		if err := stopSupervisor(t.Context(), VMConfig{}, p); !errors.Is(err, permErr) {
			t.Fatalf("stopSupervisor err = %v, want wrapped %v", err, permErr)
		}
	})

	t.Run("stopSupervisor does not mask failed bootout when print fails transiently", func(t *testing.T) {
		bootoutErr := errors.New("exit status 5")
		printErr := errors.New("exit status 1")
		printCalls := 0
		runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
			switch args[0] {
			case "print":
				printCalls++
				if printCalls == 1 {
					return []byte("worker supervise"), nil
				}
				return []byte("Input/output error"), printErr
			case "bootout":
				return []byte("Input/output error"), bootoutErr
			default:
				return nil, errors.New("unexpected subcommand")
			}
		}
		err := stopSupervisor(t.Context(), VMConfig{}, p)
		if !errors.Is(err, bootoutErr) || !errors.Is(err, printErr) {
			t.Fatalf("stopSupervisor err = %v, want error wrapping both %v and %v", err, bootoutErr, printErr)
		}
	})

	t.Run("stopSupervisor does not mask failed bootout on context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		bootoutErr := errors.New("signal: killed")
		runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
			switch args[0] {
			case "print":
				return []byte("worker supervise"), nil
			case "bootout":
				cancel()
				return nil, bootoutErr
			default:
				return nil, errors.New("unexpected subcommand")
			}
		}
		err := stopSupervisor(ctx, VMConfig{}, p)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stopSupervisor err = %v, want context.Canceled", err)
		}
	})

	t.Run("stopSupervisor propagates print error during post-bootout poll", func(t *testing.T) {
		pollErr := errors.New("exit status 1")
		printCalls := 0
		runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
			switch args[0] {
			case "print":
				printCalls++
				if printCalls == 1 {
					return []byte("worker supervise"), nil
				}
				return []byte("launchd busy"), pollErr
			case "bootout":
				return nil, nil
			default:
				return nil, errors.New("unexpected subcommand")
			}
		}
		err := stopSupervisor(t.Context(), VMConfig{}, p)
		if !errors.Is(err, pollErr) {
			t.Fatalf("stopSupervisor err = %v, want wrapped %v", err, pollErr)
		}
	})
}

func startTestSSHServer(t *testing.T, hostSigner ssh.Signer, clientPub ssh.PublicKey) int {
	t.Helper()
	srvCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	srvCfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ssh: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc, chans, reqs, err := ssh.NewServerConn(c, srvCfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.UnknownChannelType, "unused")
				}
			}(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func generateTestHostSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVMSSHHostKeyPinning(t *testing.T) {
	t.Run("seeded host key embedded in cloud-init user-data and accepted, mismatch rejected", func(t *testing.T) {
		p := NewVMPaths(t.TempDir())
		if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
			t.Fatal(err)
		}
		clientSigner, clientPubLine, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
		if err != nil {
			t.Fatalf("ensureSSHKeypair: %v", err)
		}
		hostKey, hostPub, err := ensureVMHostKeypair(p.HostKeyPriv, p.HostKeyPub)
		if err != nil {
			t.Fatalf("ensureVMHostKeypair: %v", err)
		}
		for _, path := range []string{p.HostKeyPriv, p.HostKeyPub} {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", path, err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s perm = %04o, want 0600", path, fi.Mode().Perm())
			}
		}

		// Verify cloud-init user-data embeds the generated host keypair.
		userData := CloudInitUserData("mac-worker", "alice", []string{clientPubLine}, hostKey)
		for _, want := range []string{
			"ssh_keys:\n",
			"  ed25519_private: |\n",
			"    -----BEGIN OPENSSH PRIVATE KEY-----\n",
			"  ed25519_public: " + hostKey.Public + "\n",
		} {
			if !strings.Contains(userData, want) {
				t.Fatalf("CloudInitUserData missing %q:\n%s", want, userData)
			}
		}

		// Re-calling ensureVMHostKeypair is idempotent and preserves the exact key.
		hostKey2, hostPub2, err := ensureVMHostKeypair(p.HostKeyPriv, p.HostKeyPub)
		if err != nil {
			t.Fatalf("second ensureVMHostKeypair: %v", err)
		}
		if hostKey2.Public != hostKey.Public || string(hostPub2.Marshal()) != string(hostPub.Marshal()) {
			t.Fatalf("ensureVMHostKeypair was not idempotent")
		}

		// Matching server host key is accepted.
		seededSigner, err := ssh.ParsePrivateKey([]byte(hostKey.PrivatePEM))
		if err != nil {
			t.Fatalf("parse seeded host key: %v", err)
		}
		goodPort := startTestSSHServer(t, seededSigner, clientSigner.PublicKey())
		cl, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: goodPort}, p, clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("dialSSH with matching seeded host key failed: %v", err)
		}
		cl.Close()

		// Mismatched server host key is rejected.
		wrongPort := startTestSSHServer(t, generateTestHostSigner(t), clientSigner.PublicKey())
		if badCl, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: wrongPort}, p, clientSigner, 5*time.Second); err == nil {
			badCl.Close()
			t.Fatalf("dialSSH accepted mismatched host key; want rejection")
		}
	})

	t.Run("TOFU pins host key on first connect and rejects different key on second connect", func(t *testing.T) {
		p := NewVMPaths(t.TempDir())
		if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
			t.Fatal(err)
		}
		clientSigner, _, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
		if err != nil {
			t.Fatalf("ensureSSHKeypair: %v", err)
		}
		if _, err := os.Stat(p.HostKeyPub); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expected no HostKeyPub before TOFU, got err=%v", err)
		}

		firstHostSigner := generateTestHostSigner(t)
		firstPort := startTestSSHServer(t, firstHostSigner, clientSigner.PublicKey())

		// First connection pins the key.
		cl1, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: firstPort}, p, clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("TOFU first dialSSH failed: %v", err)
		}
		cl1.Close()

		fi, err := os.Stat(p.HostKeyPub)
		if err != nil {
			t.Fatalf("HostKeyPub not persisted after TOFU: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("TOFU HostKeyPub perm = %04o, want 0600", fi.Mode().Perm())
		}
		pinned, err := loadSSHPublicKey(p.HostKeyPub)
		if err != nil {
			t.Fatalf("loadSSHPublicKey after TOFU: %v", err)
		}
		if string(pinned.Marshal()) != string(firstHostSigner.PublicKey().Marshal()) {
			t.Fatalf("pinned key does not match first server host key")
		}

		// Subsequent connection to the same host key succeeds.
		cl2, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: firstPort}, p, clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("second dialSSH with pinned key failed: %v", err)
		}
		cl2.Close()

		// Subsequent connection to a different host key is rejected.
		otherHostSigner := generateTestHostSigner(t)
		otherPort := startTestSSHServer(t, otherHostSigner, clientSigner.PublicKey())
		if badCl, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: otherPort}, p, clientSigner, 5*time.Second); err == nil {
			badCl.Close()
			t.Fatalf("dialSSH accepted changed host key after TOFU pin; want rejection")
		}
	})
}

func TestEnsurePrivateVMDirAndFilePermissions(t *testing.T) {
	p := NewVMPaths(t.TempDir())

	// Creating a new VMDir sets 0700.
	if err := ensurePrivateVMDir(p.VMDir); err != nil {
		t.Fatalf("ensurePrivateVMDir (new): %v", err)
	}
	fi, err := os.Stat(p.VMDir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("new VMDir perm = %04o, want 0700", fi.Mode().Perm())
	}

	// Simulate a pre-upgrade VM dir with loose 0755 dir and 0644 seed.iso / root.qcow2.
	if err := os.Chmod(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{p.SeedISO, p.RootDisk} {
		if err := os.WriteFile(path, []byte("secret-vm-data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := ensureVMPrivatePermissions(p); err != nil {
		t.Fatalf("ensureVMPrivatePermissions: %v", err)
	}
	if fi, err := os.Stat(p.VMDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("tightened VMDir perm = %04o (err=%v), want 0700", fi.Mode().Perm(), err)
	}
	for _, path := range []string{p.SeedISO, p.RootDisk} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("tightened %s perm = %04o, want 0600", path, fi.Mode().Perm())
		}
	}

	// Verify ensureSeedISO itself writes seed.iso with 0600 inside a 0700 VMDir.
	if _, err := os.Stat("/usr/bin/hdiutil"); err == nil {
		p2 := NewVMPaths(t.TempDir())
		if err := os.MkdirAll(p2.VMDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p2.VMDir, 0o755); err != nil {
			t.Fatal(err)
		}
		hostKey, _, err := ensureVMHostKeypair(p2.HostKeyPriv, p2.HostKeyPub)
		if err != nil {
			t.Fatal(err)
		}
		if err := ensureSeedISO(t.Context(), p2.SeedISO, "mac-worker", "alice", []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest"}, hostKey); err != nil {
			t.Fatalf("ensureSeedISO: %v", err)
		}
		if fi, err := os.Stat(p2.VMDir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("VMDir perm after ensureSeedISO = %04o (err=%v), want 0700", fi.Mode().Perm(), err)
		}
		if fi, err := os.Stat(p2.SeedISO); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("SeedISO perm after ensureSeedISO = %04o (err=%v), want 0600", fi.Mode().Perm(), err)
		}
	}
}

func TestPrepareVMHostKeyForSetup(t *testing.T) {
	t.Run("reinitializing existing VM disk without HostKeyPub does not fabricate mismatched key", func(t *testing.T) {
		p := NewVMPaths(t.TempDir())
		if err := ensurePrivateVMDir(p.VMDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.RootDisk, []byte("existing-disk"), 0o600); err != nil {
			t.Fatal(err)
		}

		hk, err := prepareVMHostKeyForSetup(p)
		if err != nil {
			t.Fatalf("prepareVMHostKeyForSetup: %v", err)
		}
		if hk.PrivatePEM != "" || hk.Public != "" {
			t.Fatalf("expected empty VMHostKey for existing disk without pin, got %+v", hk)
		}
		if _, err := os.Stat(p.HostKeyPriv); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("HostKeyPriv should not be generated for existing disk; err=%v", err)
		}
		if _, err := os.Stat(p.HostKeyPub); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("HostKeyPub should not be generated for existing disk; err=%v", err)
		}

		// Subsequent TOFU dialSSH pins the guest's actual host key.
		clientSigner, _, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
		if err != nil {
			t.Fatal(err)
		}
		guestSigner := generateTestHostSigner(t)
		port := startTestSSHServer(t, guestSigner, clientSigner.PublicKey())
		cl, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: port}, p, clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("dialSSH TOFU after existing-disk reinit failed: %v", err)
		}
		cl.Close()
		pinned, err := loadSSHPublicKey(p.HostKeyPub)
		if err != nil {
			t.Fatalf("loadSSHPublicKey: %v", err)
		}
		if string(pinned.Marshal()) != string(guestSigner.PublicKey().Marshal()) {
			t.Fatalf("pinned key mismatch after TOFU")
		}
	})

	t.Run("reinitializing existing VM disk with existing HostKeyPub preserves pin", func(t *testing.T) {
		p := NewVMPaths(t.TempDir())
		if err := ensurePrivateVMDir(p.VMDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.RootDisk, []byte("existing-disk"), 0o600); err != nil {
			t.Fatal(err)
		}
		guestSigner := generateTestHostSigner(t)
		if err := pinVMHostKeyAtomic(p.HostKeyPub, guestSigner.PublicKey()); err != nil {
			t.Fatal(err)
		}

		hk, err := prepareVMHostKeyForSetup(p)
		if err != nil {
			t.Fatalf("prepareVMHostKeyForSetup: %v", err)
		}
		wantPub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(guestSigner.PublicKey())))
		if hk.PrivatePEM != "" || hk.Public != wantPub {
			t.Fatalf("got VMHostKey %+v, want Public=%q and empty PrivatePEM", hk, wantPub)
		}
		if _, err := os.Stat(p.HostKeyPriv); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("HostKeyPriv should not be generated when preserving TOFU pin; err=%v", err)
		}
	})

	t.Run("fresh VM without RootDisk generates and pins host keypair", func(t *testing.T) {
		p := NewVMPaths(t.TempDir())
		if err := ensurePrivateVMDir(p.VMDir); err != nil {
			t.Fatal(err)
		}
		hk, err := prepareVMHostKeyForSetup(p)
		if err != nil {
			t.Fatalf("prepareVMHostKeyForSetup: %v", err)
		}
		if hk.PrivatePEM == "" || hk.Public == "" {
			t.Fatalf("expected generated VMHostKey for fresh VM, got %+v", hk)
		}
		for _, path := range []string{p.HostKeyPriv, p.HostKeyPub} {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", path, err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s perm = %04o, want 0600", path, fi.Mode().Perm())
			}
		}
	})
}

func TestUnifiedInteractiveSSHAndDialSSHPinning(t *testing.T) {
	t.Run("interactive SSH pin path first -> Go dialSSH enforces same pin and rejects changed key", func(t *testing.T) {
		p := NewVMPaths(t.TempDir())
		if err := ensurePrivateVMDir(p.VMDir); err != nil {
			t.Fatal(err)
		}
		clientSigner, _, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
		if err != nil {
			t.Fatal(err)
		}
		firstHost := generateTestHostSigner(t)
		firstPort := startTestSSHServer(t, firstHost, clientSigner.PublicKey())

		knownHosts, err := ensureVMKnownHosts(t.Context(), p, firstPort)
		if err != nil {
			t.Fatalf("ensureVMKnownHosts: %v", err)
		}
		fi, err := os.Stat(knownHosts)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("known_hosts perm = %04o (err=%v), want 0600", fi.Mode().Perm(), err)
		}
		khBytes, err := os.ReadFile(knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		wantPubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(firstHost.PublicKey())))
		wantEntry := "[127.0.0.1]:" + strconv.Itoa(firstPort) + " " + wantPubLine + "\n"
		if string(khBytes) != wantEntry {
			t.Fatalf("known_hosts = %q, want %q", string(khBytes), wantEntry)
		}
		pinned, err := loadSSHPublicKey(p.HostKeyPub)
		if err != nil {
			t.Fatalf("HostKeyPub not pinned by ensureVMKnownHosts: %v", err)
		}
		if string(pinned.Marshal()) != string(firstHost.PublicKey().Marshal()) {
			t.Fatalf("HostKeyPub does not match server key")
		}

		// Go dialSSH accepts the pinned key.
		cl, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: firstPort}, p, clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("dialSSH with key pinned by ensureVMKnownHosts failed: %v", err)
		}
		cl.Close()

		// Go dialSSH rejects a different host key.
		otherHost := generateTestHostSigner(t)
		otherPort := startTestSSHServer(t, otherHost, clientSigner.PublicKey())
		if badCl, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: otherPort}, p, clientSigner, 5*time.Second); err == nil {
			badCl.Close()
			t.Fatalf("dialSSH accepted changed host key after interactive-SSH pin")
		}
	})

	t.Run("Go dialSSH first -> interactive SSH known_hosts enforces the exact same pinned key", func(t *testing.T) {
		p := NewVMPaths(t.TempDir())
		if err := ensurePrivateVMDir(p.VMDir); err != nil {
			t.Fatal(err)
		}
		clientSigner, _, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
		if err != nil {
			t.Fatal(err)
		}
		firstHost := generateTestHostSigner(t)
		firstPort := startTestSSHServer(t, firstHost, clientSigner.PublicKey())

		cl, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: firstPort}, p, clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("initial dialSSH failed: %v", err)
		}
		cl.Close()

		// Even if a different server listens on another port, ensureVMKnownHosts
		// writes strictly the HostKeyPub pinned by dialSSH (never re-pinning).
		otherHost := generateTestHostSigner(t)
		otherPort := startTestSSHServer(t, otherHost, clientSigner.PublicKey())

		knownHosts, err := ensureVMKnownHosts(t.Context(), p, otherPort)
		if err != nil {
			t.Fatalf("ensureVMKnownHosts: %v", err)
		}
		khBytes, err := os.ReadFile(knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		wantPubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(firstHost.PublicKey())))
		wantEntry := "[127.0.0.1]:" + strconv.Itoa(otherPort) + " " + wantPubLine + "\n"
		if string(khBytes) != wantEntry {
			t.Fatalf("known_hosts = %q, want %q", string(khBytes), wantEntry)
		}
	})
}

func generateRSASHA2OnlyHostSigner(t *testing.T) ssh.Signer {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	base, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("new RSA signer: %v", err)
	}
	algSigner, ok := base.(ssh.AlgorithmSigner)
	if !ok {
		t.Fatalf("RSA signer %T does not implement ssh.AlgorithmSigner", base)
	}
	restricted, err := ssh.NewSignerWithAlgorithms(algSigner, []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256})
	if err != nil {
		t.Fatalf("NewSignerWithAlgorithms: %v", err)
	}
	return restricted
}

func TestPinnedRSAHostKeyNegotiatesRSASHA2(t *testing.T) {
	rsaHost := generateRSASHA2OnlyHostSigner(t)
	edHost := generateTestHostSigner(t)

	gotRSA := hostKeyAlgorithmsFor(rsaHost.PublicKey())
	wantRSA := []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	if !slices.Equal(gotRSA, wantRSA) {
		t.Fatalf("hostKeyAlgorithmsFor(rsa) = %v, want %v", gotRSA, wantRSA)
	}
	gotEd := hostKeyAlgorithmsFor(edHost.PublicKey())
	wantEd := []string{ssh.KeyAlgoED25519}
	if !slices.Equal(gotEd, wantEd) {
		t.Fatalf("hostKeyAlgorithmsFor(ed25519) = %v, want %v", gotEd, wantEd)
	}

	p := NewVMPaths(t.TempDir())
	if err := ensurePrivateVMDir(p.VMDir); err != nil {
		t.Fatal(err)
	}
	clientSigner, _, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
	if err != nil {
		t.Fatal(err)
	}

	port := startTestSSHServer(t, rsaHost, clientSigner.PublicKey())

	// First connect (TOFU) pins the RSA host key.
	cl1, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: port}, p, clientSigner, 5*time.Second)
	if err != nil {
		t.Fatalf("first dialSSH to RSA-SHA2 server failed: %v", err)
	}
	cl1.Close()

	// Second connect uses the pinned RSA key and must negotiate rsa-sha2-512/rsa-sha2-256
	// (sending only ssh-rsa fails against an RSA-SHA2-only server).
	cl2, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: port}, p, clientSigner, 5*time.Second)
	if err != nil {
		t.Fatalf("second dialSSH to pinned RSA-SHA2 server failed: %v", err)
	}
	cl2.Close()

	// A different RSA-SHA2 server key is still rejected.
	otherRSAHost := generateRSASHA2OnlyHostSigner(t)
	otherPort := startTestSSHServer(t, otherRSAHost, clientSigner.PublicKey())
	if badCl, err := dialSSH(t.Context(), VMConfig{User: "alice", SSHPort: otherPort}, p, clientSigner, 5*time.Second); err == nil {
		badCl.Close()
		t.Fatalf("dialSSH accepted different RSA-SHA2 host key; want rejection")
	}
}

func TestDarwinSetupRejectsZFSDevice(t *testing.T) {
	_, err := Setup(t.Context(), Options{
		Controller: "ctrl.ts.net:7120",
		ZFSDevice:  "/dev/sdb",
	}, nil, nil, nil)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
		t.Fatalf("Setup(--zfs-device on darwin) err = %v, want INVALID_ARGS *api.Error", err)
	}
	if !strings.Contains(apiErr.Message, "--zfs-device") {
		t.Errorf("err message = %q, want mention of --zfs-device", apiErr.Message)
	}
}

type mockQMPServer struct {
	sockPath string
	ln       net.Listener

	mu         sync.Mutex
	running    bool
	status     string
	blockJSON  string
	stayPaused bool
	contCalls  int
	quitCalls  int
	commands   []string
}

func startMockQMPServer(t *testing.T, sockPath string, running bool, status, blockJSON string) *mockQMPServer {
	t.Helper()
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen mock QMP %s: %v", sockPath, err)
	}
	s := &mockQMPServer{
		sockPath:  sockPath,
		ln:        ln,
		running:   running,
		status:    status,
		blockJSON: blockJSON,
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	})
	go s.serve()
	return s
}

func (s *mockQMPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = c.Write([]byte(`{"QMP":{"version":{"qemu":{"major":10,"minor":0,"micro":0}}}}` + "\n"))
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				var req struct {
					Execute string `json:"execute"`
				}
				if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
					return
				}
				s.mu.Lock()
				s.commands = append(s.commands, req.Execute)
				switch req.Execute {
				case "qmp_capabilities":
					s.mu.Unlock()
					_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
				case "query-status":
					running := s.running
					st := s.status
					s.mu.Unlock()
					if !running {
						_, _ = c.Write([]byte(`{"event":"STOP"}` + "\n"))
					}
					_, _ = fmt.Fprintf(c, `{"return":{"running":%t,"singlestep":false,"status":%q}}`+"\n", running, st)
				case "query-block":
					blk := s.blockJSON
					if blk == "" {
						blk = "[]"
					}
					s.mu.Unlock()
					var compact bytes.Buffer
					if err := json.Compact(&compact, []byte(blk)); err == nil {
						blk = compact.String()
					}
					_, _ = fmt.Fprintf(c, `{"return":%s}`+"\n", blk)
				case "cont":
					s.contCalls++
					if !s.stayPaused {
						s.running = true
						s.status = "running"
						s.blockJSON = `[{"device":"root","io-status":"ok"},{"device":"swap","io-status":"ok"}]`
					}
					resumed := !s.stayPaused
					s.mu.Unlock()
					if resumed {
						_, _ = c.Write([]byte(`{"event":"RESUME"}` + "\n"))
					}
					_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
				case "quit":
					s.quitCalls++
					s.mu.Unlock()
					_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
				default:
					s.mu.Unlock()
					_, _ = c.Write([]byte(`{"error":{"class":"CommandNotFound","desc":"unknown"}}` + "\n"))
				}
			}
		}(conn)
	}
}

func (s *mockQMPServer) setState(running bool, status, blockJSON string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = running
	s.status = status
	s.blockJSON = blockJSON
}

func (s *mockQMPServer) getContCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contCalls
}

func (s *mockQMPServer) getCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

func setupMockDarwinVM(t *testing.T, running bool, status, blockJSON string) (string, VMConfig, VMPaths, *mockQMPServer) {
	t.Helper()
	baseDir := t.TempDir()
	p := NewVMPaths(baseDir)
	if err := os.MkdirAll(p.VMDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := VMConfig{
		Controller:    "ctrl.ts.net:7120",
		User:          "alice",
		Hostname:      "mac-worker",
		Arch:          "arm64",
		UbuntuVersion: DefaultUbuntuVersion,
		CPUs:          4,
		MemoryBytes:   4 << 30,
		SSHPort:       0, // avoid real SSH dials in unit tests
		StateDir:      DefaultVMStateDir,
	}
	if err := writeJSONAtomic(p.Config, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	myPID := os.Getpid()
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(myPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origProc := processCommandLine
	origLaunchctl := runLaunchctl
	t.Cleanup(func() {
		processCommandLine = origProc
		runLaunchctl = origLaunchctl
	})
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid == myPID {
			return "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcMissing, nil
	}
	runLaunchctl = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("Could not find service"), errors.New("exit status 113")
	}

	qmp := startMockQMPServer(t, qmpSocketPath(p), running, status, blockJSON)
	return baseDir, cfg, p, qmp
}

func TestQMPInspectVM(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "hb-qmp-inspect-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	sock := filepath.Join(tmpDir, "q.sock")

	srv := startMockQMPServer(t, sock, true, "running", `[{"device":"swap","io-status":"ok"}]`)

	t.Run("running VM does not query block devices", func(t *testing.T) {
		info, err := qmpInspectVM(t.Context(), sock)
		if err != nil {
			t.Fatalf("qmpInspectVM: %v", err)
		}
		if !info.Known || !info.Running || info.QMPStatus != "running" || len(info.IOErrors) != 0 {
			t.Fatalf("qmpInspectVM(running) = %+v, want Known=true Running=true QMPStatus=running IOErrors=nil", info)
		}
		if slices.Contains(srv.getCommands(), "query-block") {
			t.Errorf("query-block should not be called when VM is running; commands=%v", srv.getCommands())
		}
	})

	t.Run("paused io-error collects non-ok block io-status", func(t *testing.T) {
		srv.setState(false, "io-error", `[
			{"device":"root","io-status":"ok"},
			{"device":"swap","io-status":"nospace"},
			{"device":"","io-status":"failed","inserted":{"file":"/Users/alice/.hotbed/vm/zfs.qcow2"}}
		]`)
		info, err := qmpInspectVM(t.Context(), sock)
		if err != nil {
			t.Fatalf("qmpInspectVM(io-error): %v", err)
		}
		if !info.Known || info.Running || info.QMPStatus != "io-error" {
			t.Fatalf("qmpInspectVM(io-error) = %+v, want Known=true Running=false QMPStatus=io-error", info)
		}
		wantErrs := []string{"swap: nospace", "zfs.qcow2: failed"}
		if !slices.Equal(info.IOErrors, wantErrs) {
			t.Fatalf("IOErrors = %v, want %v", info.IOErrors, wantErrs)
		}
	})

	t.Run("paused without io errors reports paused with empty IOErrors", func(t *testing.T) {
		srv.setState(false, "paused", `[{"device":"root","io-status":"ok"},{"device":"swap","io-status":"ok"}]`)
		info, err := qmpInspectVM(t.Context(), sock)
		if err != nil {
			t.Fatalf("qmpInspectVM(paused): %v", err)
		}
		if !info.Known || info.Running || info.QMPStatus != "paused" || len(info.IOErrors) != 0 {
			t.Fatalf("qmpInspectVM(paused) = %+v, want Known=true Running=false QMPStatus=paused IOErrors=nil", info)
		}
	})

	t.Run("non-paused non-running statuses do not query block devices", func(t *testing.T) {
		for _, status := range []string{"suspended", "debug", "guest-panicked"} {
			before := len(srv.getCommands())
			srv.setState(false, status, `[{"device":"swap","io-status":"nospace"}]`)
			info, err := qmpInspectVM(t.Context(), sock)
			if err != nil {
				t.Fatalf("qmpInspectVM(%s): %v", status, err)
			}
			if !info.Known || info.Running || info.QMPStatus != status || len(info.IOErrors) != 0 {
				t.Fatalf("qmpInspectVM(%s) = %+v, want Known=true Running=false QMPStatus=%s IOErrors=nil", status, info, status)
			}
			newCmds := srv.getCommands()[before:]
			if slices.Contains(newCmds, "query-block") {
				t.Errorf("query-block should not be called for status %q; commands=%v", status, newCmds)
			}
		}
	})
}

func TestVMProcessStateExcludesNonPausedQMPStatuses(t *testing.T) {
	for _, status := range []string{"suspended", "debug", "guest-panicked"} {
		t.Run(status, func(t *testing.T) {
			_, _, p, _ := setupMockDarwinVM(t, false, status, `[{"device":"root","io-status":"ok"}]`)
			pid, state, qmp, err := vmDetailedState(t.Context(), p)
			if err != nil || state != vmProcRunning || pid != os.Getpid() {
				t.Fatalf("vmDetailedState(%s) = (%d, %v, %+v, %v), want (%d, vmProcRunning, ..., nil)",
					status, pid, state, qmp, err, os.Getpid())
			}
			if qmp.QMPStatus != status {
				t.Fatalf("qmp.QMPStatus = %q, want %q", qmp.QMPStatus, status)
			}
		})
	}
}

func TestVMStatusAndProcessStatePausedOnIOError(t *testing.T) {
	nospaceBlocks := `[{"device":"root","io-status":"ok"},{"device":"swap","io-status":"nospace"}]`
	baseDir, cfg, p, qmp := setupMockDarwinVM(t, false, "io-error", nospaceBlocks)

	pid, state, err := vmProcessState(t.Context(), p)
	if err != nil || state != vmProcPaused || pid != os.Getpid() {
		t.Fatalf("vmProcessState(io-error) = (%d, %v, %v), want (%d, vmProcPaused, nil)", pid, state, err, os.Getpid())
	}
	if (qemuVM{cfg: cfg, p: p}).Running(t.Context()) {
		t.Fatal("qemuVM.Running() = true for paused VM, want false")
	}

	st, err := GetStatus(t.Context(), baseDir)
	if err != nil {
		t.Fatalf("GetStatus(io-error): %v", err)
	}
	if st.Running {
		t.Fatalf("Status.Running = true for paused VM, want false")
	}
	if st.VM == nil {
		t.Fatal("Status.VM is nil")
	}
	if st.VM.Running || !st.VM.Paused || st.VM.PID != os.Getpid() || st.VM.QMPStatus != "io-error" {
		t.Fatalf("Status.VM = %+v, want Running=false Paused=true PID=%d QMPStatus=io-error", st.VM, os.Getpid())
	}
	if want := []string{"swap: nospace"}; !slices.Equal(st.VM.IOErrors, want) {
		t.Fatalf("Status.VM.IOErrors = %v, want %v", st.VM.IOErrors, want)
	}

	// Trim and SSH must report the paused state clearly rather than "not running".
	if _, err := Trim(t.Context(), baseDir); err == nil || !strings.Contains(err.Error(), "paused") || !strings.Contains(err.Error(), "swap: nospace") {
		t.Fatalf("Trim(paused) err = %v, want paused error mentioning swap: nospace", err)
	}
	if _, err := SSH(t.Context(), baseDir, nil); err == nil || !strings.Contains(err.Error(), "paused") || !strings.Contains(err.Error(), "swap: nospace") {
		t.Fatalf("SSH(paused) err = %v, want paused error mentioning swap: nospace", err)
	}

	// Once QEMU resumes, vmProcessState and GetStatus report Running=true, Paused=false.
	qmp.setState(true, "running", `[{"device":"root","io-status":"ok"},{"device":"swap","io-status":"ok"}]`)
	pid, state, err = vmProcessState(t.Context(), p)
	if err != nil || state != vmProcRunning || pid != os.Getpid() {
		t.Fatalf("vmProcessState(running) = (%d, %v, %v), want (%d, vmProcRunning, nil)", pid, state, err, os.Getpid())
	}
	if !(qemuVM{cfg: cfg, p: p}).Running(t.Context()) {
		t.Fatal("qemuVM.Running() = false for running VM, want true")
	}
	st, err = GetStatus(t.Context(), baseDir)
	if err != nil {
		t.Fatalf("GetStatus(running): %v", err)
	}
	if !st.VM.Running || st.VM.Paused || st.VM.QMPStatus != "running" || len(st.VM.IOErrors) != 0 {
		t.Fatalf("Status.VM(running) = %+v, want Running=true Paused=false QMPStatus=running IOErrors=nil", st.VM)
	}
}

func TestStartAndResumePausedVM(t *testing.T) {
	origMinFree := minHostFreeBytes
	origHostFree := hostFreeBytes
	origResync := resyncGuestClock
	origVerifyTimeout := resumeVerifyTimeout
	origVerifyInterval := resumeVerifyInterval
	t.Cleanup(func() {
		minHostFreeBytes = origMinFree
		hostFreeBytes = origHostFree
		resyncGuestClock = origResync
		resumeVerifyTimeout = origVerifyTimeout
		resumeVerifyInterval = origVerifyInterval
	})
	minHostFreeBytes = 2 << 30
	resumeVerifyTimeout = 80 * time.Millisecond
	resumeVerifyInterval = 10 * time.Millisecond

	nospaceBlocks := `[{"device":"root","io-status":"ok"},{"device":"swap","io-status":"nospace"}]`

	t.Run("Start refuses loudly when host disk is below threshold", func(t *testing.T) {
		baseDir, _, _, qmp := setupMockDarwinVM(t, false, "io-error", nospaceBlocks)
		hostFreeBytes = func(string) (int64, error) { return 512 << 20, nil }
		resyncGuestClock = func(context.Context, VMConfig, VMPaths) error {
			t.Fatal("resyncGuestClock should not be called when resume is refused")
			return nil
		}

		_, err := Start(t.Context(), baseDir)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict {
			t.Fatalf("Start(low disk) err = %v, want CONFLICT *api.Error", err)
		}
		for _, want := range []string{"io-error", "swap: nospace", "512.0 MiB", "2.00 GiB"} {
			if !strings.Contains(apiErr.Message, want) {
				t.Errorf("Start error message %q missing %q", apiErr.Message, want)
			}
		}
		if !strings.Contains(apiErr.Suggestion, "free disk space") {
			t.Errorf("Start error suggestion %q missing disk space guidance", apiErr.Suggestion)
		}
		if qmp.getContCalls() != 0 {
			t.Fatalf("expected 0 QMP cont calls when disk is full, got %d", qmp.getContCalls())
		}
	})

	t.Run("Start resumes via QMP cont and resyncs clock when free space allows", func(t *testing.T) {
		baseDir, _, p, qmp := setupMockDarwinVM(t, false, "io-error", nospaceBlocks)
		hostFreeBytes = func(string) (int64, error) { return 5 << 30, nil }
		var resyncCalls atomic.Int32
		resyncGuestClock = func(context.Context, VMConfig, VMPaths) error {
			resyncCalls.Add(1)
			return nil
		}

		st, err := Start(t.Context(), baseDir)
		if err != nil {
			t.Fatalf("Start(free disk): %v", err)
		}
		if qmp.getContCalls() != 1 {
			t.Fatalf("contCalls = %d, want 1", qmp.getContCalls())
		}
		if resyncCalls.Load() != 1 {
			t.Fatalf("resyncCalls = %d, want 1", resyncCalls.Load())
		}
		if st.VM == nil || !st.VM.Running || st.VM.Paused {
			t.Fatalf("Status.VM after resume = %+v, want Running=true Paused=false", st.VM)
		}
		_, state, err := vmProcessState(t.Context(), p)
		if err != nil || state != vmProcRunning {
			t.Fatalf("vmProcessState after resume = (%v, %v), want (vmProcRunning, nil)", state, err)
		}
	})

	t.Run("ensureVMRunning fails loudly if VM remains paused after cont", func(t *testing.T) {
		_, cfg, p, qmp := setupMockDarwinVM(t, false, "io-error", nospaceBlocks)
		qmp.mu.Lock()
		qmp.stayPaused = true
		qmp.mu.Unlock()
		hostFreeBytes = func(string) (int64, error) { return 5 << 30, nil }
		resyncGuestClock = func(context.Context, VMConfig, VMPaths) error {
			t.Fatal("resyncGuestClock should not run when VM stayed paused")
			return nil
		}

		err := ensureVMRunning(t.Context(), cfg, p, nil)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict {
			t.Fatalf("ensureVMRunning(stayPaused) err = %v, want CONFLICT *api.Error", err)
		}
		if !strings.Contains(apiErr.Message, "remained paused") || !strings.Contains(apiErr.Message, "swap: nospace") {
			t.Errorf("err message = %q, want mention of remained paused and swap: nospace", apiErr.Message)
		}
		if qmp.getContCalls() != 1 {
			t.Fatalf("contCalls = %d, want 1", qmp.getContCalls())
		}
	})

	t.Run("ensureVMRunning resumes manually paused VM when disk space allows", func(t *testing.T) {
		_, cfg, p, qmp := setupMockDarwinVM(t, false, "paused", `[{"device":"root","io-status":"ok"}]`)
		hostFreeBytes = func(string) (int64, error) { return 5 << 30, nil }
		resyncGuestClock = func(context.Context, VMConfig, VMPaths) error { return nil }

		if err := ensureVMRunning(t.Context(), cfg, p, nil); err != nil {
			t.Fatalf("ensureVMRunning(paused): %v", err)
		}
		if qmp.getContCalls() != 1 {
			t.Fatalf("contCalls = %d, want 1", qmp.getContCalls())
		}
	})
}

func TestSupervisorPausedVMRecovery(t *testing.T) {
	origMinFree := minHostFreeBytes
	origHostFree := hostFreeBytes
	origResync := resyncGuestClock
	origCheckInterval := pausedVMCheckInterval
	t.Cleanup(func() {
		minHostFreeBytes = origMinFree
		hostFreeBytes = origHostFree
		resyncGuestClock = origResync
		pausedVMCheckInterval = origCheckInterval
	})
	minHostFreeBytes = 2 << 30
	pausedVMCheckInterval = 15 * time.Millisecond

	nospaceBlocks := `[{"device":"root","io-status":"ok"},{"device":"swap","io-status":"nospace"}]`
	baseDir, cfg, p, qmp := setupMockDarwinVM(t, false, "io-error", nospaceBlocks)

	var freeBytes atomic.Int64
	freeBytes.Store(256 << 20) // 256 MiB: below 2 GiB threshold
	hostFreeBytes = func(string) (int64, error) { return freeBytes.Load(), nil }

	var resyncCalls atomic.Int32
	resyncGuestClock = func(context.Context, VMConfig, VMPaths) error {
		resyncCalls.Add(1)
		return nil
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 1. Below threshold: supervisor logs host free space and io-status, does NOT send cont.
	resumed, err := checkAndRecoverPausedVM(t.Context(), cfg, p, log)
	if err != nil || resumed {
		t.Fatalf("checkAndRecoverPausedVM(low disk) = (%v, %v), want (false, nil)", resumed, err)
	}
	if qmp.getContCalls() != 0 {
		t.Fatalf("contCalls = %d while disk below threshold, want 0", qmp.getContCalls())
	}
	logOut := logBuf.String()
	for _, want := range []string{"qmpStatus=io-error", "swap: nospace", "256.0 MiB", "2.00 GiB"} {
		if !strings.Contains(logOut, want) {
			t.Errorf("supervisor log %q missing %q", logOut, want)
		}
	}

	// 2. Manual pause (status=paused, no IO errors): supervisor leaves it alone.
	qmp.setState(false, "paused", `[{"device":"root","io-status":"ok"}]`)
	freeBytes.Store(4 << 30)
	resumed, err = checkAndRecoverPausedVM(t.Context(), cfg, p, log)
	if err != nil || resumed || qmp.getContCalls() != 0 {
		t.Fatalf("checkAndRecoverPausedVM(manual pause) = (%v, %v, cont=%d), want (false, nil, 0)", resumed, err, qmp.getContCalls())
	}

	// 3. Put VM back into io-error and run darwinWatchPausedVM: it detects and resumes automatically.
	qmp.setState(false, "io-error", nospaceBlocks)
	freeBytes.Store(512 << 20)

	checked := make(chan struct{}, 16)
	hostFreeBytes = func(string) (int64, error) {
		v := freeBytes.Load()
		select {
		case checked <- struct{}{}:
		default:
		}
		return v, nil
	}
	resynced := make(chan struct{}, 4)
	resyncGuestClock = func(context.Context, VMConfig, VMPaths) error {
		resyncCalls.Add(1)
		select {
		case resynced <- struct{}{}:
		default:
		}
		return nil
	}

	watchCtx, watchCancel := context.WithCancel(t.Context())
	defer watchCancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		darwinWatchPausedVM(watchCtx, baseDir, log)
	}()

	// Wait deterministically for the initial check and at least one ticker check
	// while host free space is still below threshold.
	for range 2 {
		select {
		case <-checked:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for paused-VM watcher check ticks")
		}
	}
	if qmp.getContCalls() != 0 {
		t.Fatalf("watcher sent cont while host free space was below threshold")
	}

	// Host space recovers -> watcher resumes VM and resyncs guest clock.
	freeBytes.Store(4 << 30)
	select {
	case <-resynced:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for paused-VM watcher to resume and resync guest clock")
	}
	watchCancel()
	<-watchDone

	if qmp.getContCalls() != 1 {
		t.Fatalf("watcher contCalls = %d, want 1", qmp.getContCalls())
	}
	if resyncCalls.Load() != 1 {
		t.Fatalf("watcher resyncCalls = %d, want 1", resyncCalls.Load())
	}
	_, state, err := vmProcessState(t.Context(), p)
	if err != nil || state != vmProcRunning {
		t.Fatalf("vmProcessState after watcher recovery = (%v, %v), want (vmProcRunning, nil)", state, err)
	}
}

func TestDarwinResyncGuestClock(t *testing.T) {
	p := NewVMPaths(t.TempDir())
	if err := ensurePrivateVMDir(p.VMDir); err != nil {
		t.Fatal(err)
	}
	clientSigner, _, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner := generateTestHostSigner(t)
	if err := pinVMHostKeyAtomic(p.HostKeyPub, hostSigner.PublicKey()); err != nil {
		t.Fatal(err)
	}

	srvCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientSigner.PublicKey().Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized")
		},
	}
	srvCfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	gotCmd := make(chan string, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc, chans, reqs, err := ssh.NewServerConn(c, srvCfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
						continue
					}
					ch, chReqs, err := newCh.Accept()
					if err != nil {
						return
					}
					go func() {
						defer ch.Close()
						for req := range chReqs {
							if req.Type == "exec" {
								var payload struct{ Cmd string }
								_ = ssh.Unmarshal(req.Payload, &payload)
								select {
								case gotCmd <- payload.Cmd:
								default:
								}
								_ = req.Reply(true, nil)
								_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))
								return
							}
							_ = req.Reply(false, nil)
						}
					}()
				}
			}(conn)
		}
	}()

	cfg := VMConfig{
		User:    "alice",
		SSHPort: ln.Addr().(*net.TCPAddr).Port,
	}
	if err := darwinResyncGuestClock(t.Context(), cfg, p); err != nil {
		t.Fatalf("darwinResyncGuestClock: %v", err)
	}
	select {
	case cmd := <-gotCmd:
		if !strings.Contains(cmd, "date -u -s") || !strings.Contains(cmd, "chronyc -a makestep") {
			t.Fatalf("unexpected clock resync command: %q", cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSH clock resync command")
	}
}

// worker init with another size refuses while the VM runs, rather than
// recording a size QEMU, started with the old one, does not have.
func TestRefuseResizeWhileRunning(t *testing.T) {
	p := NewVMPaths(t.TempDir())
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		t.Fatal(err)
	}
	running := VMConfig{CPUs: 7, MemoryBytes: 24 << 30}
	resized := VMConfig{CPUs: 7, MemoryBytes: 12 << 30}

	if err := refuseResizeWhileRunning(t.Context(), running, resized, p); err != nil {
		t.Fatalf("stopped VM: %v, want nil", err)
	}

	orig := processCommandLine
	t.Cleanup(func() { processCommandLine = orig })
	myPID := os.Getpid()
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid == myPID {
			return "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcMissing, nil
	}
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(myPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := refuseResizeWhileRunning(t.Context(), running, running, p); err != nil {
		t.Fatalf("running VM, same size: %v, want nil", err)
	}
	err := refuseResizeWhileRunning(t.Context(), running, resized, p)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict {
		t.Fatalf("running VM, new size: err = %v, want CONFLICT *api.Error", err)
	}
	if want := "the worker VM runs with 7 vCPUs and 24.00 GiB RAM; 7 vCPUs and 12.00 GiB RAM need it started again"; apiErr.Message != want {
		t.Errorf("message = %q, want %q", apiErr.Message, want)
	}
}
