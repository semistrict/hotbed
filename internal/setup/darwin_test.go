//go:build darwin

package setup

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEnsureSSHKeypairAndSeedISO(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	pub := filepath.Join(dir, "id_ed25519.pub")

	s1, pub1, err := ensureSSHKeypair(priv, pub)
	if err != nil {
		t.Fatalf("ensureSSHKeypair: %v", err)
	}
	if !strings.HasPrefix(pub1, "ssh-ed25519 ") {
		t.Fatalf("unexpected pubkey: %q", pub1)
	}
	fi, err := os.Stat(priv)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o, want 0600", fi.Mode().Perm())
	}
	// Reloading returns the same key.
	s2, pub2, err := ensureSSHKeypair(priv, pub)
	if err != nil {
		t.Fatalf("reload keypair: %v", err)
	}
	if pub2 != pub1 || string(s2.PublicKey().Marshal()) != string(s1.PublicKey().Marshal()) {
		t.Fatalf("reloaded key did not match original")
	}

	iso := filepath.Join(dir, "seed.iso")
	if err := ensureSeedISO(t.Context(), iso, "test-worker", "testuser", []string{pub1}); err != nil {
		t.Fatalf("ensureSeedISO: %v", err)
	}
	isoInfo, err := os.Stat(iso)
	if err != nil || isoInfo.Size() == 0 {
		t.Fatalf("seed.iso missing or empty: %v", err)
	}
}

func TestVerifyLinuxELF(t *testing.T) {
	dir := t.TempDir()
	makeELF := func(name string, machine uint16) string {
		p := filepath.Join(dir, name)
		var hdr [20]byte
		copy(hdr[:4], []byte{0x7f, 'E', 'L', 'F'})
		binary.LittleEndian.PutUint16(hdr[18:20], machine)
		if err := os.WriteFile(p, hdr[:], 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	armBin := makeELF("arm64", 183)
	amdBin := makeELF("amd64", 62)
	nonELF := filepath.Join(dir, "macho")
	_ = os.WriteFile(nonELF, []byte("not-an-elf-binary-at-all"), 0o755)

	if err := verifyLinuxELF(armBin, "arm64"); err != nil {
		t.Errorf("valid arm64 ELF rejected: %v", err)
	}
	if err := verifyLinuxELF(amdBin, "amd64"); err != nil {
		t.Errorf("valid amd64 ELF rejected: %v", err)
	}
	if err := verifyLinuxELF(armBin, "amd64"); err == nil {
		t.Errorf("arm64 ELF accepted for amd64")
	}
	if err := verifyLinuxELF(nonELF, "arm64"); err == nil {
		t.Errorf("non-ELF file accepted")
	}
}

func TestQMPSocketPathLimit(t *testing.T) {
	short := NewVMPaths("/Users/ramon/.hotbed")
	if got := qmpSocketPath(short); got != short.QMPSock {
		t.Errorf("short path changed: got %q, want %q", got, short.QMPSock)
	}
	long := NewVMPaths("/var/folders/xx/" + strings.Repeat("a", 95) + "/.hotbed")
	got := qmpSocketPath(long)
	if len(got) >= 100 {
		t.Errorf("long path not shortened: %q (len %d)", got, len(got))
	}
	if !strings.HasPrefix(got, "/tmp/hotbed-qmp-") {
		t.Errorf("unexpected shortened path: %q", got)
	}
}

// TestLiveQEMUAndZFSSparsePunchHole boots a real Ubuntu 26.04 LTS VM under
// QEMU HVF, installs zfsutils-linux, creates the sparse qcow2 ZFS pool,
// writes 128 MiB of incompressible data, deletes it, runs zpool trim -w, and
// verifies that zfs.qcow2 grows on APFS and then punches holes back out.
// Opt-in via HOTBED_TEST_QEMU=1 (downloads ~600 MiB Ubuntu image into ~/.hotbed/cache).
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
	linuxBin, err := resolveLinuxBinary(ctx, "", cacheDir, arch, os.Stderr)
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
	if err := ensureSeedISO(ctx, p.SeedISO, cfg.Hostname, cfg.User, []string{pubKey}); err != nil {
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
	verOut, err := sshRun(ctx, cl, vmBinPath+" version")
	if err != nil || !strings.Contains(verOut, `"os":"linux"`) {
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

	// Destroy the dataset, wait for ZFS async_destroy, advance 3 TXGs so ZFS
	// drains its ms_defer[TXG_DEFER_SIZE=2] deferred-free queues into the
	// allocatable metaslab trees, then run zpool trim -w so SCSI UNMAP punches
	// holes out of zfs.qcow2 on APFS.
	// Test automatic hole punching via ZFS autotrim=on with zfs_trim_txg_batch=1
	// (without manual full-pool zpool trim initializing unused metaslabs).
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
	t.Logf("after destroy + zpool trim + zpool sync: zfs.qcow2 allocated=%s (reclaimed %s)",
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
