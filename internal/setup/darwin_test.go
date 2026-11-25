//go:build darwin

package setup

import (
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
	"io/fs"
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

// TestLiveVZAndZFSSparsePunchHole boots a real Ubuntu 26.04 LTS VM under
// vfkit (Apple's Virtualization framework) with nested virtualization, checks
// the guest has /dev/kvm, installs zfsutils-linux, creates the sparse raw ZFS
// pool, writes 128 MiB of incompressible data, deletes it, flushes ZFS
// deferred frees so autotrim=on punches holes back out, and verifies zfs.img
// shrinks on APFS. Opt-in via HOTBED_TEST_VZ=1 (downloads ~600 MiB Ubuntu
// image into ~/.hotbed/cache; needs vfkit and an M3 or later Mac).
func TestLiveVZAndZFSSparsePunchHole(t *testing.T) {
	if os.Getenv("HOTBED_TEST_VZ") != "1" {
		t.Skip("set HOTBED_TEST_VZ=1 to run the live vfkit Ubuntu + nested KVM + ZFS hole-punching test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	arch := runtime.GOARCH
	vfkitBin, err := findVfkit()
	if err != nil {
		t.Fatalf("findVfkit: %v", err)
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
	rawImg, err := ensureRawImage(cachedImg, os.Stderr)
	if err != nil {
		t.Fatalf("ensureRawImage: %v", err)
	}
	linuxBin, err := resolveLinuxBinary(ctx, os.Getenv("HOTBED_TEST_BINARY"), "127.0.0.1:7120", "", cacheDir, arch, nil, os.Stderr)
	if err != nil {
		t.Fatalf("resolveLinuxBinary: %v", err)
	}

	// The VM's disks clone the cached raw image, which APFS does only on its
	// own volume: keep them beside the cache, not in t.TempDir().
	baseDir, err := os.MkdirTemp(defaultBase, "vz-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })
	p := NewVMPaths(baseDir)
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
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
		VfkitBinary:   vfkitBin,
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
	if err := ensureVMDisks(rawImg, cfg, p, os.Stderr); err != nil {
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
	t.Logf("initial zfs.img: allocated=%s virtual=%s", FormatBytes(initialStat.AllocatedBytes), FormatBytes(initialStat.VirtualBytes))
	if initialStat.AllocatedBytes > 2<<20 {
		t.Fatalf("initial zfs.img too large: %d bytes", initialStat.AllocatedBytes)
	}

	if err := ensureVMRunning(ctx, cfg, p, os.Stderr); err != nil {
		t.Fatal(err)
	}
	cl, err := waitForSSH(ctx, cfg, p, signer, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	// Nested virtualization: the guest's worker runs Firecracker on /dev/kvm.
	if out, err := sshRun(ctx, cl, "test -c /dev/kvm && echo kvm"); err != nil || strings.TrimSpace(out) != "kvm" {
		t.Fatalf("guest has no /dev/kvm (nested virtualization): %q, %v", out, err)
	}

	// Host usage samples the Virtualization framework process that runs this
	// VM, not vfkit.
	vfkitPID, state, err := vmProcessState(ctx, p)
	if err != nil || state != vmProcRunning {
		t.Fatalf("vmProcessState = (%d, %v, %v), want a running vfkit", vfkitPID, state, err)
	}
	vzPID, err := vzProcessFor(vfkitPID)
	if err != nil {
		t.Fatalf("vzProcessFor(%d): %v", vfkitPID, err)
	}
	if exe, err := processExecutable(vzPID); err != nil || filepath.Base(exe) != vzProcessName {
		t.Fatalf("vzProcessFor(%d) = %d, whose executable is %q (%v), want %s", vfkitPID, vzPID, exe, err, vzProcessName)
	}
	usage, err := (vfkitVM{cfg: cfg, p: p, sampler: newDarwinHostUsageSampler()}).SampleHostUsage(ctx)
	if err != nil || usage == nil {
		t.Fatalf("SampleHostUsage = (%v, %v), want a sample", usage, err)
	}

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
	t.Logf("after zpool create: zfs.img allocated=%s", FormatBytes(postCreateStat.AllocatedBytes))

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
	t.Logf("after 128 MiB write: zfs.img allocated=%s", FormatBytes(grownStat.AllocatedBytes))
	if grownStat.AllocatedBytes < 100<<20 {
		t.Fatalf("expected zfs.img to grow past 100 MiB after 128 MiB random write, got %s", FormatBytes(grownStat.AllocatedBytes))
	}

	// Destroy the dataset, wait for ZFS async_destroy, and advance 4 TXGs so
	// ZFS drains its ms_defer[TXG_DEFER_SIZE=2] deferred-free queues and
	// autotrim=on with zfs_trim_txg_batch=1 punches holes out of zfs.img on
	// APFS (without manual full-pool zpool trim initializing unused metaslabs).
	diag, err := sshRun(ctx, cl, `set -e
echo 1 | sudo tee /sys/module/zfs/parameters/zfs_trim_txg_batch >/dev/null
echo "1. after dd:      $(cat /sys/block/vdb/stat)"
sudo zfs destroy -r hotbed/testvol
sudo zpool wait -t free hotbed
for i in 1 2 3 4; do sudo zpool sync hotbed; done
sleep 3
echo "2. after autotrim: $(cat /sys/block/vdb/stat)"
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
	t.Logf("after destroy + autotrim sync: zfs.img allocated=%s (reclaimed %s)",
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
	t.Logf("after stopVM: zfs.img allocated=%s logical=%s",
		FormatBytes(afterStopStat.AllocatedBytes), FormatBytes(afterStopStat.LogicalBytes))
	if trimmedStat.AllocatedBytes > 32<<20 {
		t.Fatalf("expected zfs.img to punch back under 32 MiB after trim, still %s (after stop: %s)",
			FormatBytes(trimmedStat.AllocatedBytes), FormatBytes(afterStopStat.AllocatedBytes))
	}

	// A second boot keeps the EFI variable store the first one created, and
	// the guest comes back with its pool.
	if _, err := os.Stat(p.EFIVars); err != nil {
		t.Fatalf("first boot left no EFI variable store: %v", err)
	}
	if err := ensureVMRunning(ctx, cfg, p, os.Stderr); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	cl2, err := waitForSSH(ctx, cfg, p, signer, os.Stderr)
	if err != nil {
		t.Fatalf("second boot SSH: %v", err)
	}
	defer cl2.Close()
	if out, err := sshRun(ctx, cl2, "sudo zpool import -d /dev/disk/by-id hotbed 2>/dev/null; sudo zpool list -H -o name hotbed"); err != nil || strings.TrimSpace(out) != "hotbed" {
		t.Fatalf("second boot: pool hotbed = %q, %v", out, err)
	}
	if err := stopVM(ctx, cfg, p, os.Stderr); err != nil {
		t.Fatalf("second stopVM: %v", err)
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
		if !strings.Contains(apiErr.Suggestion, "CI to deploy") || !strings.Contains(apiErr.Suggestion, "--binary") {
			t.Errorf("suggestion = %q, want mention of CI deploy and --binary", apiErr.Suggestion)
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
		if !strings.Contains(apiErr.Suggestion, "CI to deploy") || !strings.Contains(apiErr.Suggestion, "--binary") {
			t.Errorf("suggestion = %q, want mention of CI deploy and --binary", apiErr.Suggestion)
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

	t.Run("authenticates to an HTTPS controller and only to it", func(t *testing.T) {
		const token = "tok-123"
		var elsewhereAuth atomic.Value
		elsewhereAuth.Store("unset")
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			elsewhereAuth.Store(r.Header.Get("Authorization"))
			_, _ = w.Write(arm64Payload)
		}))
		defer elsewhere.Close()
		var downloadURL string
		srv := connect.NewServer()
		hotbedv1connect.RegisterClusterServiceHandler(srv, &fakeBuildCluster{
			getBuild: func(_ context.Context, _ *pb.GetBuildRequest) (*pb.Build, error) {
				return &pb.Build{Number: 42, Sha256: arm64Sum, Path: "/builds/42/hotbed-linux-arm64", DownloadUrl: downloadURL}, nil
			},
		})
		mux := http.NewServeMux()
		connecthttp.Mount(mux, srv)
		mux.HandleFunc("GET /builds/42/hotbed-linux-arm64", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(arm64Payload)
		})
		authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
		})
		ts := httptest.NewServer(authed)
		defer ts.Close()

		// The controller's own /builds path gets the token.
		if _, err := resolveLinuxBinary(t.Context(), "", ts.URL, token, t.TempDir(), "arm64", ts.Client(), nil); err != nil {
			t.Fatalf("resolveLinuxBinary from the controller: %v", err)
		}
		// Without it the controller refuses.
		if _, err := resolveLinuxBinary(t.Context(), "", ts.URL, "", t.TempDir(), "arm64", ts.Client(), nil); err == nil {
			t.Fatal("resolveLinuxBinary without a token succeeded against a controller that requires one")
		}
		// A download URL elsewhere gets no token.
		downloadURL = elsewhere.URL + "/hotbed-linux-arm64"
		if _, err := resolveLinuxBinary(t.Context(), "", ts.URL, token, t.TempDir(), "arm64", ts.Client(), nil); err != nil {
			t.Fatalf("resolveLinuxBinary from elsewhere: %v", err)
		}
		if got := elsewhereAuth.Load(); got != "" {
			t.Fatalf("download elsewhere got Authorization %q, want none", got)
		}
	})

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
		got, err := resolveLinuxBinary(t.Context(), "", addr, "", cacheDir, "arm64", ts.Client(), nil)
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
		got2, err := resolveLinuxBinary(t.Context(), "", addr, "", cacheDir, "arm64", ts.Client(), nil)
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
		_, err := resolveLinuxBinary(t.Context(), "", ts.Listener.Addr().String(), "", cacheDir, "arm64", ts.Client(), nil)
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

		_, err := resolveLinuxBinary(t.Context(), "", ts.Listener.Addr().String(), "", t.TempDir(), "arm64", ts.Client(), nil)
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
		got, err := resolveLinuxBinary(t.Context(), "", ts.Listener.Addr().String(), "", t.TempDir(), "arm64", &shortTimeoutClient, nil)
		if err != nil {
			t.Fatalf("resolveLinuxBinary failed under short Client.Timeout: %v", err)
		}
		gotBytes, err := os.ReadFile(got)
		if err != nil || !bytes.Equal(gotBytes, arm64Payload) {
			t.Fatalf("downloaded binary mismatch (err=%v)", err)
		}
	})

	t.Run("prefers direct download_url over controller /builds/ proxy", func(t *testing.T) {
		var githubHits, ctrlHits atomic.Int32
		ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			githubHits.Add(1)
			_, _ = w.Write(arm64Payload)
		}))
		defer ghSrv.Close()

		srv := connect.NewServer()
		hotbedv1connect.RegisterClusterServiceHandler(srv, &fakeBuildCluster{
			getBuild: func(_ context.Context, _ *pb.GetBuildRequest) (*pb.Build, error) {
				return &pb.Build{
					Number:      43,
					Sha256:      arm64Sum,
					Path:        "/builds/43/hotbed-linux-arm64",
					DownloadUrl: ghSrv.URL + "/releases/download/build-43/hotbed-linux-arm64",
				}, nil
			},
		})
		mux := http.NewServeMux()
		connecthttp.Mount(mux, srv)
		mux.HandleFunc("GET /builds/43/hotbed-linux-arm64", func(w http.ResponseWriter, _ *http.Request) {
			ctrlHits.Add(1)
			http.Error(w, "should not hit controller proxy", http.StatusInternalServerError)
		})
		ts := httptest.NewServer(mux)
		defer ts.Close()

		got, err := resolveLinuxBinary(t.Context(), "", ts.Listener.Addr().String(), "", t.TempDir(), "arm64", ts.Client(), nil)
		if err != nil {
			t.Fatalf("resolveLinuxBinary with download_url: %v", err)
		}
		gotBytes, err := os.ReadFile(got)
		if err != nil || !bytes.Equal(gotBytes, arm64Payload) {
			t.Fatalf("downloaded binary mismatch (err=%v)", err)
		}
		if githubHits.Load() != 1 || ctrlHits.Load() != 0 {
			t.Fatalf("hits: github=%d ctrl=%d, want 1/0", githubHits.Load(), ctrlHits.Load())
		}
	})

	t.Run("explicit --binary override validates ELF without calling controller", func(t *testing.T) {
		dir := t.TempDir()
		good := filepath.Join(dir, "hotbed-linux-arm64")
		if err := os.WriteFile(good, arm64Payload, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := resolveLinuxBinary(t.Context(), good, "127.0.0.1:1", "", t.TempDir(), "arm64", nil, nil)
		if err != nil || got != good {
			t.Fatalf("resolveLinuxBinary(explicit) = (%q, %v), want (%q, nil)", got, err, good)
		}
		if _, err := resolveLinuxBinary(t.Context(), good, "127.0.0.1:1", "", t.TempDir(), "amd64", nil, nil); err == nil {
			t.Fatal("resolveLinuxBinary accepted arm64 --binary for amd64 guest")
		}
	})
}

func TestIsVMProcessCmd(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "hb")
	p := NewVMPaths(baseDir)
	cfg := VMConfig{
		Hostname:    "mac-worker",
		Arch:        "arm64",
		CPUs:        4,
		MemoryBytes: 4 << 30,
		VfkitBinary: "/opt/homebrew/bin/vfkit",
	}
	args, err := VfkitArgs(cfg, p, controlSocketPath(p), sshSocketPath(p), false)
	if err != nil {
		t.Fatalf("VfkitArgs: %v", err)
	}
	realCmd := cfg.VfkitBinary + " " + strings.Join(args, " ")

	// Symlinked base dir: VMPaths uses the symlink, while the running vfkit
	// was given the resolved target.
	symlinkRoot := t.TempDir()
	realBase := filepath.Join(symlinkRoot, "real-hb")
	linkBase := filepath.Join(symlinkRoot, "link-hb")
	if err := os.MkdirAll(filepath.Join(realBase, "vm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBase, linkBase); err != nil {
		t.Fatal(err)
	}
	linkP := NewVMPaths(linkBase)
	resolvedRealPID := evalExistingSymlinks(NewVMPaths(realBase).PIDFile)

	// Base dir containing spaces.
	spaceP := NewVMPaths(filepath.Join(t.TempDir(), "my worker dir"))
	otherP := NewVMPaths(filepath.Join(t.TempDir(), "other"))

	tests := []struct {
		name string
		cmd  string
		p    VMPaths
		want bool
	}{
		{name: "real vfkit args", cmd: realCmd, p: p, want: true},
		{name: "vfkit with space in binary path", cmd: "/Applications/My Tools/vfkit --cpus 2 --pidfile " + p.PIDFile, p: p, want: true},
		{name: "pidfile in middle of args", cmd: "/opt/homebrew/bin/vfkit --pidfile " + p.PIDFile + " --cpus 2", p: p, want: true},
		{name: "pidfile with equals syntax", cmd: "/opt/homebrew/bin/vfkit --cpus 2 --pidfile=" + p.PIDFile + " --memory 4096", p: p, want: true},
		{name: "quoted pidfile with spaces", cmd: `/opt/homebrew/bin/vfkit --cpus 2 --pidfile "` + spaceP.PIDFile + `" --memory 4096`, p: spaceP, want: true},
		{name: "unquoted pidfile with spaces", cmd: "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + spaceP.PIDFile + " --memory 4096", p: spaceP, want: true},
		{name: "symlink resolved pidfile matches", cmd: "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + resolvedRealPID, p: linkP, want: true},
		{name: "other VM referencing our disks", cmd: "/opt/homebrew/bin/vfkit --pidfile " + otherP.PIDFile + " --device virtio-blk,path=" + p.RootDisk, p: p, want: false},
		{name: "other VM mentioning our pidfile in a device", cmd: "/opt/homebrew/bin/vfkit --pidfile " + otherP.PIDFile + " --device virtio-blk,path=" + p.PIDFile, p: p, want: false},
		{name: "pidfile suffix trap", cmd: "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.PIDFile + ".bak", p: p, want: false},
		{name: "qemu with our pidfile", cmd: "/opt/homebrew/bin/qemu-system-aarch64 -machine virt -pidfile " + p.PIDFile, p: p, want: false},
		{name: "unrelated sleep process", cmd: "/bin/sleep 60", p: p, want: false},
		{name: "grep mentioning vfkit and pidfile", cmd: "/usr/bin/grep vfkit " + p.PIDFile, p: p, want: false},
		{name: "env wrapper running vfkit", cmd: "/usr/bin/env /opt/homebrew/bin/vfkit --pidfile " + p.PIDFile, p: p, want: false},
		{name: "shell wrapper mentioning vfkit", cmd: "/bin/sh -c vfkit --pidfile " + p.PIDFile, p: p, want: false},
		{name: "other VM vfkit process", cmd: "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + otherP.PIDFile, p: p, want: false},
		{name: "sibling VMDir prefix trap", cmd: "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.VMDir + "2/vfkit.pid", p: p, want: false},
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

func TestVMSocketPath(t *testing.T) {
	p := NewVMPaths(filepath.Join(t.TempDir(), "hb"))
	if got := controlSocketPath(p); got != p.ControlSock {
		t.Fatalf("controlSocketPath = %q, want %q", got, p.ControlSock)
	}

	// A long symlinked base dir under /tmp (macOS /tmp -> /private/tmp) forces
	// short hashed /tmp paths, the same through the symlink as through its
	// target, and distinct per socket.
	longSymTmp, err := os.MkdirTemp("/tmp", "hotbed-long-symlink-test-"+strings.Repeat("x", 60)+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(longSymTmp) })
	realBase := filepath.Join(longSymTmp, "real-hb")
	linkBase := filepath.Join(longSymTmp, "link-hb")
	if err := os.MkdirAll(filepath.Join(realBase, "vm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBase, linkBase); err != nil {
		t.Fatal(err)
	}
	realP, linkP := NewVMPaths(realBase), NewVMPaths(linkBase)
	if len(realP.ControlSock) < 100 {
		t.Fatalf("expected a socket path >= 100 bytes, got %d", len(realP.ControlSock))
	}
	ctl, ssh := controlSocketPath(linkP), sshSocketPath(linkP)
	if !strings.HasPrefix(ctl, "/tmp/hotbed-") || !strings.HasSuffix(ctl, "-vfkit.sock") || len(ctl) >= 100 {
		t.Fatalf("controlSocketPath = %q, want a short /tmp/hotbed-<hash>-vfkit.sock", ctl)
	}
	if !strings.HasSuffix(ssh, "-ssh.sock") || strings.TrimSuffix(ssh, "-ssh.sock") != strings.TrimSuffix(ctl, "-vfkit.sock") {
		t.Fatalf("sshSocketPath = %q, want the same hash as %q with -ssh.sock", ssh, ctl)
	}
	if got := controlSocketPath(realP); got != ctl {
		t.Fatalf("controlSocketPath through the symlink = %q, through its target %q; want them the same", ctl, got)
	}
}

func TestVfkitArgs(t *testing.T) {
	p := NewVMPaths("/Users/a/.hotbed")
	cfg := VMConfig{Arch: "arm64", CPUs: 6, MemoryBytes: 12 << 30, VfkitBinary: "/opt/homebrew/bin/vfkit"}
	args, err := VfkitArgs(cfg, p, "/Users/a/.hotbed/vm/vfkit.sock", "/Users/a/.hotbed/vm/ssh.sock", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--cpus", "6",
		"--memory", "12288",
		"--nested",
		"--pidfile", "/Users/a/.hotbed/vm/vfkit.pid",
		"--restful-uri", "unix:///Users/a/.hotbed/vm/vfkit.sock",
		"--bootloader", "efi,variable-store=/Users/a/.hotbed/vm/efi-vars",
		"--device", "virtio-blk,path=/Users/a/.hotbed/vm/root.img,deviceId=hotbed-root",
		"--device", "virtio-blk,path=/Users/a/.hotbed/vm/zfs.img,deviceId=hotbed-zfs",
		"--device", "virtio-blk,path=/Users/a/.hotbed/vm/swap.img,deviceId=hotbed-swap",
		"--device", "virtio-blk,path=/Users/a/.hotbed/vm/seed.iso,deviceId=hotbed-seed",
		"--device", "virtio-net,nat",
		"--device", "virtio-vsock,port=22,socketURL=/Users/a/.hotbed/vm/ssh.sock,connect",
		"--device", "virtio-serial,logFilePath=/Users/a/.hotbed/vm/console.log",
		"--device", "virtio-rng",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("VfkitArgs =\n%q\nwant\n%q", args, want)
	}
	// A first boot creates the EFI variable store; later boots keep it.
	first, err := VfkitArgs(cfg, p, "c", "s", true)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(first, "--bootloader"); first[i+1] != "efi,variable-store=/Users/a/.hotbed/vm/efi-vars,create" {
		t.Fatalf("first boot --bootloader %q, want the store created", first[i+1])
	}
	if zfsDiskByID != "/dev/disk/by-id/virtio-hotbed-zfs" || swapDiskByID != "/dev/disk/by-id/virtio-hotbed-swap" {
		t.Fatalf("guest disk paths %q, %q do not name the deviceIds VfkitArgs gives", zfsDiskByID, swapDiskByID)
	}
	cfg.Arch = "amd64"
	if _, err := VfkitArgs(cfg, p, "c", "s", false); err == nil || !strings.Contains(err.Error(), "only on Apple silicon") {
		t.Fatalf("VfkitArgs(amd64) = %v, want an Apple silicon error", err)
	}
}

func TestParsePSStatCommand(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantCmd   string
		wantState vmProcState
	}{
		{name: "normal process", in: "S+   /opt/homebrew/bin/vfkit --cpus 2\n", wantCmd: "/opt/homebrew/bin/vfkit --cpus 2", wantState: vmProcRunning},
		{name: "zombie Z", in: "Z    (vfkit)\n", wantState: vmProcMissing},
		{name: "zombie ZN", in: "ZN   /opt/homebrew/bin/vfkit --pidfile /tmp/vm/vfkit.pid\n", wantState: vmProcMissing},
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
	ctlSock := controlSocketPath(p)
	if err := os.WriteFile(ctlSock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(ctlSock) })

	start := time.Now()
	pid, state, err := vmProcessState(t.Context(), p)
	elapsed := time.Since(start)
	if err != nil || state != vmProcMissing || pid != 0 {
		t.Fatalf("vmProcessState for recycled PID = (%d, %v, %v), want (0, vmProcMissing, nil)", pid, state, err)
	}
	if elapsed > time.Second {
		t.Fatalf("vmProcessState took %v for stale recycled PID; expected fast rejection", elapsed)
	}

	// stopVM must NOT signal the unrelated process, and MUST clean stale pid/socket files.
	if err := stopVM(t.Context(), VMConfig{}, p, nil); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	if err := syscall.Kill(sleepCmd.Process.Pid, 0); err != nil {
		t.Fatalf("unrelated process %d was killed by stopVM: %v", sleepCmd.Process.Pid, err)
	}
	if _, err := os.Stat(p.PIDFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale vfkit.pid still exists after stopVM: err=%v", err)
	}
	if _, err := os.Stat(ctlSock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale vfkit.sock still exists after stopVM: err=%v", err)
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

	// Live PID matching this VM's vfkit command line -> running even when its API
	// socket is not yet up.
	orig := processCommandLine
	t.Cleanup(func() { processCommandLine = orig })
	myPID := os.Getpid()
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid == myPID {
			return "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcMissing, nil
	}
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(myPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pid, state, err := vmProcessState(t.Context(), p); err != nil || state != vmProcRunning || pid != myPID {
		t.Fatalf("matching vfkit pid: got (%d, %v, %v), want (%d, vmProcRunning, nil)", pid, state, err, myPID)
	}
}

func TestStopVMDoesNotKillRecycledPIDMidShutdown(t *testing.T) {
	// Spawn a real unrelated process. Pretend on the very first check
	// (vmProcessState) it was vfkit, and immediately afterward its PID was
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
	ctlSock := controlSocketPath(p)
	if err := os.WriteFile(ctlSock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(ctlSock) })

	orig := processCommandLine
	t.Cleanup(func() { processCommandLine = orig })
	var calls atomic.Int32
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid != targetPID {
			return "", vmProcMissing, nil
		}
		if calls.Add(1) == 1 {
			return "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "/bin/sleep 30", vmProcRunning, nil
	}

	start := time.Now()
	if err := stopVM(t.Context(), VMConfig{}, p, nil); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stopVM took %v; expected quick exit once PID no longer matched vfkit", elapsed)
	}
	if err := syscall.Kill(targetPID, 0); err != nil {
		t.Fatalf("recycled process %d was signaled by stopVM: %v", targetPID, err)
	}
	if _, err := os.Stat(p.PIDFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("vfkit.pid was not removed: err=%v", err)
	}
	if _, err := os.Stat(ctlSock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("vfkit.sock was not removed: err=%v", err)
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
	ctlSock := controlSocketPath(p)
	if err := os.WriteFile(ctlSock, []byte("live-vfkit"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(ctlSock) })
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

	// stopVM must propagate the error and preserve vfkit.pid and vfkit.sock.
	if err := stopVM(t.Context(), VMConfig{}, p, nil); !errors.Is(err, probeErr) {
		t.Fatalf("stopVM err = %v, want %v", err, probeErr)
	}
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("stopVM removed vfkit.pid on vmProcUnknown: %v", err)
	}
	if _, err := os.Stat(ctlSock); err != nil {
		t.Errorf("stopVM removed vfkit.sock on vmProcUnknown: %v", err)
	}

	// ensureVMRunning must also refuse to unlink files or double-boot on vmProcUnknown.
	if err := ensureVMRunning(t.Context(), VMConfig{}, p, nil); !errors.Is(err, probeErr) {
		t.Fatalf("ensureVMRunning err = %v, want %v", err, probeErr)
	}
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("ensureVMRunning removed vfkit.pid on vmProcUnknown: %v", err)
	}
	if _, err := os.Stat(ctlSock); err != nil {
		t.Errorf("ensureVMRunning removed vfkit.sock on vmProcUnknown: %v", err)
	}

	// cleanStaleVMFiles must NOT unlink files on vmProcUnknown.
	cleanStaleVMFiles(t.Context(), p)
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("cleanStaleVMFiles removed vfkit.pid on vmProcUnknown: %v", err)
	}
	if _, err := os.Stat(ctlSock); err != nil {
		t.Errorf("cleanStaleVMFiles removed vfkit.sock on vmProcUnknown: %v", err)
	}

	// Destroy must abort and leave VM disks, pidfile, and vfkit socket intact!
	if _, err := Destroy(t.Context(), DestroyOptions{Dir: baseDir}); !errors.Is(err, probeErr) {
		t.Fatalf("Destroy err = %v, want %v", err, probeErr)
	}
	for _, path := range []string{p.VMDir, p.RootDisk, p.ZFSDisk, p.PIDFile, ctlSock} {
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
			return "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcUnknown, probeErr
	}
	if err := stopVM(t.Context(), VMConfig{}, p, nil); !errors.Is(err, probeErr) {
		t.Fatalf("stopVM mid-shutdown err = %v, want %v", err, probeErr)
	}
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("stopVM removed vfkit.pid on mid-shutdown vmProcUnknown: %v", err)
	}
	if _, err := os.Stat(ctlSock); err != nil {
		t.Errorf("stopVM removed vfkit.sock on mid-shutdown vmProcUnknown: %v", err)
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
	if err := stopSupervisor(t.Context(), p); err != nil {
		t.Fatalf("stopSupervisor: %v", err)
	}
	if supervised(t.Context(), p) {
		t.Fatal("expected supervised() == false after stopSupervisor")
	}

	// 3. stopSupervisor tolerates a race where the job exits right before bootout.
	mu.Lock()
	loaded[target] = "worker supervise"
	failBootoutErr = errors.New("exit status 3")
	mu.Unlock()
	if err := stopSupervisor(t.Context(), p); err != nil {
		t.Fatalf("stopSupervisor with already-gone race: %v", err)
	}
	mu.Lock()
	failBootoutErr = nil
	mu.Unlock()

	// 4. disableSupervisor boots out and removes plist file.
	if err := startSupervisor(t.Context(), p); err != nil {
		t.Fatalf("startSupervisor (2): %v", err)
	}
	if err := disableSupervisor(t.Context(), p); err != nil {
		t.Fatalf("disableSupervisor: %v", err)
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("plist should be removed by disableSupervisor, stat err=%v", err)
	}

	// 5. Destroy boots out the LaunchAgent, removes plist, cleans stale pid/socket, and deletes VMDir.
	if err := startSupervisor(t.Context(), p); err != nil {
		t.Fatalf("startSupervisor (3): %v", err)
	}
	if err := os.WriteFile(p.PIDFile, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctlSock := controlSocketPath(p)
	if err := os.WriteFile(ctlSock, []byte("stale"), 0o644); err != nil {
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
	if _, err := os.Stat(ctlSock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Destroy left vfkit.sock behind: %v", err)
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
	ctlSock := controlSocketPath(p)
	t.Cleanup(func() { _ = os.Remove(ctlSock) })

	// When VM is not running (e.g., stale recycled PID after reboot), cleanStaleVMFiles removes both files.
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctlSock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanStaleVMFiles(t.Context(), p)
	if _, err := os.Stat(p.PIDFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected stale vfkit.pid removed, err=%v", err)
	}
	if _, err := os.Stat(ctlSock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected stale vfkit.sock removed, err=%v", err)
	}

	// When VM IS running, cleanStaleVMFiles leaves both files untouched.
	orig := processCommandLine
	t.Cleanup(func() { processCommandLine = orig })
	myPID := os.Getpid()
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid == myPID {
			return "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcMissing, nil
	}
	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(myPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctlSock, []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanStaleVMFiles(t.Context(), p)
	if _, err := os.Stat(p.PIDFile); err != nil {
		t.Errorf("live vfkit.pid was unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(ctlSock); err != nil {
		t.Errorf("live vfkit.sock was unexpectedly removed: %v", err)
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
		if err := stopSupervisor(t.Context(), p); !errors.Is(err, permErr) {
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
		err := stopSupervisor(t.Context(), p)
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
		err := stopSupervisor(ctx, p)
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
		err := stopSupervisor(t.Context(), p)
		if !errors.Is(err, pollErr) {
			t.Fatalf("stopSupervisor err = %v, want wrapped %v", err, pollErr)
		}
	})
}

// startTestSSHServer serves SSH as hostSigner, accepting clientPub, on a new
// Unix socket, as vfkit serves the guest's vsock SSH, and returns its path.
func startTestSSHServer(t *testing.T, hostSigner ssh.Signer, clientPub ssh.PublicKey) string {
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
	// Keep the socket path short for macOS's 104-byte Unix socket limit.
	dir, err := os.MkdirTemp("/tmp", "hb-ssh-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ssh.sock")
	ln, err := net.Listen("unix", sock)
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
	return sock
}

// withSSHSock is p with its guest SSH socket at sock.
func withSSHSock(p VMPaths, sock string) VMPaths {
	p.SSHSock = sock
	return p
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
		cl, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, goodPort), clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("dialSSH with matching seeded host key failed: %v", err)
		}
		cl.Close()

		// Mismatched server host key is rejected.
		wrongPort := startTestSSHServer(t, generateTestHostSigner(t), clientSigner.PublicKey())
		if badCl, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, wrongPort), clientSigner, 5*time.Second); err == nil {
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
		cl1, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, firstPort), clientSigner, 5*time.Second)
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
		cl2, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, firstPort), clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("second dialSSH with pinned key failed: %v", err)
		}
		cl2.Close()

		// Subsequent connection to a different host key is rejected.
		otherHostSigner := generateTestHostSigner(t)
		otherPort := startTestSSHServer(t, otherHostSigner, clientSigner.PublicKey())
		if badCl, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, otherPort), clientSigner, 5*time.Second); err == nil {
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

	// Simulate a pre-upgrade VM dir with loose 0755 dir and 0644 seed.iso / root.img.
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
		cl, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, port), clientSigner, 5*time.Second)
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

		knownHosts, err := ensureVMKnownHosts(t.Context(), withSSHSock(p, firstPort))
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
		wantEntry := vmHostKeyAlias + " " + wantPubLine + "\n"
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
		cl, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, firstPort), clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("dialSSH with key pinned by ensureVMKnownHosts failed: %v", err)
		}
		cl.Close()

		// Go dialSSH rejects a different host key.
		otherHost := generateTestHostSigner(t)
		otherPort := startTestSSHServer(t, otherHost, clientSigner.PublicKey())
		if badCl, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, otherPort), clientSigner, 5*time.Second); err == nil {
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

		cl, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, firstPort), clientSigner, 5*time.Second)
		if err != nil {
			t.Fatalf("initial dialSSH failed: %v", err)
		}
		cl.Close()

		// Even if a different server listens on another port, ensureVMKnownHosts
		// writes strictly the HostKeyPub pinned by dialSSH (never re-pinning).
		otherHost := generateTestHostSigner(t)
		otherPort := startTestSSHServer(t, otherHost, clientSigner.PublicKey())

		knownHosts, err := ensureVMKnownHosts(t.Context(), withSSHSock(p, otherPort))
		if err != nil {
			t.Fatalf("ensureVMKnownHosts: %v", err)
		}
		khBytes, err := os.ReadFile(knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		wantPubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(firstHost.PublicKey())))
		wantEntry := vmHostKeyAlias + " " + wantPubLine + "\n"
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
	cl1, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, port), clientSigner, 5*time.Second)
	if err != nil {
		t.Fatalf("first dialSSH to RSA-SHA2 server failed: %v", err)
	}
	cl1.Close()

	// Second connect uses the pinned RSA key and must negotiate rsa-sha2-512/rsa-sha2-256
	// (sending only ssh-rsa fails against an RSA-SHA2-only server).
	cl2, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, port), clientSigner, 5*time.Second)
	if err != nil {
		t.Fatalf("second dialSSH to pinned RSA-SHA2 server failed: %v", err)
	}
	cl2.Close()

	// A different RSA-SHA2 server key is still rejected.
	otherRSAHost := generateRSASHA2OnlyHostSigner(t)
	otherPort := startTestSSHServer(t, otherRSAHost, clientSigner.PublicKey())
	if badCl, err := dialSSH(t.Context(), VMConfig{User: "alice"}, withSSHSock(p, otherPort), clientSigner, 5*time.Second); err == nil {
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

// mockVfkit serves vfkit's REST API (GET and POST /vm/state) on a Unix
// socket, as vfkit 0.6.4 answers.
type mockVfkit struct {
	mu         sync.Mutex
	state      string
	stayPaused bool
	changes    []string
}

func startMockVfkit(t *testing.T, sockPath, state string) *mockVfkit {
	t.Helper()
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen mock vfkit: %v", err)
	}
	m := &mockVfkit{state: state}
	srv := &http.Server{Handler: http.HandlerFunc(m.serveHTTP)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(sockPath)
	})
	return m
}

func (m *mockVfkit) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/vm/state" {
		http.NotFound(w, r)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"state": m.state, "canStop": true, "canHardStop": true})
	case http.MethodPost:
		var req struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.changes = append(m.changes, req.State)
		if req.State == vfkitResume && !m.stayPaused {
			m.state = vfkitStateRunning
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (m *mockVfkit) setState(state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = state
}

func (m *mockVfkit) getChanges() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.changes)
}

// setupMockDarwinVM sets up a configured worker VM whose vfkit is this test
// process, serving a mock REST API that reports state.
func setupMockDarwinVM(t *testing.T, state string) (string, VMConfig, VMPaths, *mockVfkit) {
	t.Helper()
	// Keep the control socket short for macOS's 104-byte Unix socket limit.
	baseDir, err := os.MkdirTemp("/tmp", "hb-vm-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })
	p := NewVMPaths(baseDir)
	if err := os.MkdirAll(p.VMDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := VMConfig{
		Controller:    "ctrl.ts.net:7120",
		User:          "", // avoid real SSH dials in unit tests
		Hostname:      "mac-worker",
		Arch:          "arm64",
		UbuntuVersion: DefaultUbuntuVersion,
		CPUs:          4,
		MemoryBytes:   4 << 30,
		VfkitBinary:   "/opt/homebrew/bin/vfkit",
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
			return "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "", vmProcMissing, nil
	}
	runLaunchctl = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("Could not find service"), errors.New("exit status 113")
	}

	vf := startMockVfkit(t, controlSocketPath(p), state)
	return baseDir, cfg, p, vf
}

func TestVMDetailedStateReportsVfkitState(t *testing.T) {
	// Only a paused VM is paused; vfkit's other states leave the process
	// running and are reported as they are.
	for _, tc := range []struct {
		state string
		want  vmProcState
	}{
		{vfkitStateRunning, vmProcRunning},
		{vfkitStateStarting, vmProcRunning},
		{"VirtualMachineStateError", vmProcRunning},
		{vfkitStatePaused, vmProcPaused},
	} {
		t.Run(tc.state, func(t *testing.T) {
			_, _, p, _ := setupMockDarwinVM(t, tc.state)
			pid, state, vmState, err := vmDetailedState(t.Context(), p)
			if err != nil || state != tc.want || pid != os.Getpid() || vmState != tc.state {
				t.Fatalf("vmDetailedState = (%d, %v, %q, %v), want (%d, %v, %q, nil)", pid, state, vmState, err, os.Getpid(), tc.want, tc.state)
			}
		})
	}
}

func TestVMStatusAndProcessStatePaused(t *testing.T) {
	baseDir, cfg, p, vf := setupMockDarwinVM(t, vfkitStatePaused)

	pid, state, err := vmProcessState(t.Context(), p)
	if err != nil || state != vmProcPaused || pid != os.Getpid() {
		t.Fatalf("vmProcessState(paused) = (%d, %v, %v), want (%d, vmProcPaused, nil)", pid, state, err, os.Getpid())
	}
	if (vfkitVM{cfg: cfg, p: p}).Running(t.Context()) {
		t.Fatal("vfkitVM.Running() = true for paused VM, want false")
	}

	st, err := GetStatus(t.Context(), baseDir)
	if err != nil {
		t.Fatalf("GetStatus(paused): %v", err)
	}
	if st.Running || st.VM == nil || st.VM.Running || !st.VM.Paused || st.VM.PID != os.Getpid() || st.VM.State != vfkitStatePaused {
		t.Fatalf("Status = %+v, VM = %+v, want Running=false and VM Running=false Paused=true PID=%d State=%s", st, st.VM, os.Getpid(), vfkitStatePaused)
	}

	// Trim and SSH must report the paused state clearly rather than "not running".
	if _, err := Trim(t.Context(), baseDir); err == nil || err.Error() != "CONFLICT: worker VM is paused" {
		t.Fatalf("Trim(paused) err = %v, want the paused conflict", err)
	}
	if _, err := SSH(t.Context(), baseDir, nil); err == nil || err.Error() != "CONFLICT: worker VM is paused" {
		t.Fatalf("SSH(paused) err = %v, want the paused conflict", err)
	}

	vf.setState(vfkitStateRunning)
	if !(vfkitVM{cfg: cfg, p: p}).Running(t.Context()) {
		t.Fatal("vfkitVM.Running() = false for running VM, want true")
	}
	st, err = GetStatus(t.Context(), baseDir)
	if err != nil {
		t.Fatalf("GetStatus(running): %v", err)
	}
	if !st.VM.Running || st.VM.Paused || st.VM.State != vfkitStateRunning {
		t.Fatalf("Status.VM(running) = %+v, want Running=true Paused=false State=%s", st.VM, vfkitStateRunning)
	}
}

func TestStartResumesPausedVM(t *testing.T) {
	origResync := resyncGuestClock
	t.Cleanup(func() { resyncGuestClock = origResync })

	t.Run("Start resumes through vfkit and resyncs the guest clock", func(t *testing.T) {
		baseDir, _, p, vf := setupMockDarwinVM(t, vfkitStatePaused)
		var resyncCalls atomic.Int32
		resyncGuestClock = func(context.Context, VMConfig, VMPaths) error {
			resyncCalls.Add(1)
			return nil
		}
		st, err := Start(t.Context(), baseDir)
		if err != nil {
			t.Fatalf("Start(paused): %v", err)
		}
		if got := vf.getChanges(); !slices.Equal(got, []string{vfkitResume}) {
			t.Fatalf("vfkit state changes = %v, want [Resume]", got)
		}
		if resyncCalls.Load() != 1 {
			t.Fatalf("resyncCalls = %d, want 1", resyncCalls.Load())
		}
		if st.VM == nil || !st.VM.Running || st.VM.Paused {
			t.Fatalf("Status.VM after resume = %+v, want Running=true Paused=false", st.VM)
		}
		if _, state, err := vmProcessState(t.Context(), p); err != nil || state != vmProcRunning {
			t.Fatalf("vmProcessState after resume = (%v, %v), want (vmProcRunning, nil)", state, err)
		}
	})
}

func TestResumeThatDoesNotTakeFails(t *testing.T) {
	origResync, origTimeout, origInterval := resyncGuestClock, resumeVerifyTimeout, resumeVerifyInterval
	t.Cleanup(func() {
		resyncGuestClock, resumeVerifyTimeout, resumeVerifyInterval = origResync, origTimeout, origInterval
	})
	resumeVerifyTimeout, resumeVerifyInterval = 50*time.Millisecond, 5*time.Millisecond
	resyncGuestClock = func(context.Context, VMConfig, VMPaths) error {
		t.Fatal("resyncGuestClock ran for a VM that stayed paused")
		return nil
	}
	_, cfg, p, vf := setupMockDarwinVM(t, vfkitStatePaused)
	vf.mu.Lock()
	vf.stayPaused = true
	vf.mu.Unlock()
	err := ensureVMRunning(t.Context(), cfg, p, nil)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict || apiErr.Message != "worker VM is still VirtualMachineStatePaused after vfkit was asked to resume it" {
		t.Fatalf("ensureVMRunning(stays paused) = %v, want the still-paused conflict", err)
	}
}

// A worker VM set up for QEMU is refused everywhere, so nothing starts a
// second VM on its disks, deletes them under a running QEMU, or makes an empty
// ZFS disk beside its pool.
func TestQEMUVMRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	baseDir, _, p, _ := setupMockDarwinVM(t, vfkitStateRunning)
	if err := os.WriteFile(filepath.Join(p.VMDir, "zfs.qcow2"), []byte("pool"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.VMDir, "qemu.pid"), []byte("123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const want = "the worker VM in "
	check := func(name string, err error) {
		t.Helper()
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict ||
			apiErr.Message != want+p.VMDir+" was set up for QEMU (qemu.pid, zfs.qcow2); hotbed now runs it with vfkit on raw disks" ||
			!strings.Contains(apiErr.Suggestion, "qemu-img convert -p -O raw root.qcow2 root.img") {
			t.Fatalf("%s = %v, want the QEMU conflict", name, err)
		}
	}
	_, err := GetStatus(t.Context(), baseDir)
	check("GetStatus", err)
	_, err = Start(t.Context(), baseDir)
	check("Start", err)
	_, err = Destroy(t.Context(), DestroyOptions{Dir: baseDir})
	check("Destroy", err)
	_, err = Setup(t.Context(), Options{Controller: "ctrl.ts.net:7120", Dir: baseDir}, nil, nil, nil)
	check("Setup", err)
	if _, err := os.Stat(filepath.Join(p.VMDir, "zfs.qcow2")); err != nil {
		t.Fatalf("the QEMU disk is gone: %v", err)
	}
	if _, err := os.Stat(p.ZFSDisk); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a raw ZFS disk was made beside the QEMU one: %v", err)
	}
}

func TestLoadVMWithoutVfkitBinary(t *testing.T) {
	baseDir, cfg, p, _ := setupMockDarwinVM(t, vfkitStateRunning)
	cfg.VfkitBinary = ""
	if err := writeJSONAtomic(p.Config, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := GetStatus(t.Context(), baseDir)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeNotInitialized {
		t.Fatalf("GetStatus = %v, want NOT_INITIALIZED", err)
	}
}

func TestStopVMPausedPullsThePlug(t *testing.T) {
	// A paused guest cannot power itself off: stopVM hard-stops it. The mock
	// vfkit (this process) never exits, so stopVM goes on to signals; the
	// process check says it is gone once the hard stop arrives.
	_, cfg, p, vf := setupMockDarwinVM(t, vfkitStatePaused)
	myPID := os.Getpid()
	processCommandLine = func(_ context.Context, pid int) (string, vmProcState, error) {
		if pid == myPID && !slices.Contains(vf.getChanges(), vfkitHardStop) {
			return "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.PIDFile, vmProcRunning, nil
		}
		return "/bin/other", vmProcRunning, nil
	}
	if err := stopVM(t.Context(), cfg, p, nil); err != nil {
		t.Fatalf("stopVM(paused): %v", err)
	}
	if got := vf.getChanges(); !slices.Equal(got, []string{vfkitHardStop}) {
		t.Fatalf("vfkit state changes = %v, want [HardStop]", got)
	}
	if _, err := os.Stat(p.PIDFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stopVM left %s: %v", p.PIDFile, err)
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
	sockDir, err := os.MkdirTemp("/tmp", "hb-ssh-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	p = withSSHSock(p, filepath.Join(sockDir, "ssh.sock"))
	ln, err := net.Listen("unix", p.SSHSock)
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

	cfg := VMConfig{User: "alice"}
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
// recording a size the VM, started with the old one, does not have.
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
			return "/opt/homebrew/bin/vfkit --cpus 2 --pidfile " + p.PIDFile, vmProcRunning, nil
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
