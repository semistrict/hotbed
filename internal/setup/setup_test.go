package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateZFSDevicePath(t *testing.T) {
	valid := []string{
		"/dev/sdb",
		"/dev/nvme0n1",
		"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_hotbed-zfs",
		"/dev/disk/by-id/virtio-hotbed-zfs",
	}
	for _, dev := range valid {
		if err := validateZFSDevicePath(dev); err != nil {
			t.Errorf("validateZFSDevicePath(%q) = %v, want nil", dev, err)
		}
	}

	invalid := []string{
		"",
		"dev/sdb",
		"/dev",
		"/dev/",
		"/tmp/zfs.img",
		"/etc/passwd",
		"/dev/../etc/passwd",
		"/dev/disk/../sdb",
		"/dev//sdb",
		"/dev/sdb/",
		"/dev/sdb extra",
		"/dev/sdb\n",
	}
	for _, dev := range invalid {
		if err := validateZFSDevicePath(dev); err == nil {
			t.Errorf("validateZFSDevicePath(%q) = nil, want error", dev)
		}
	}
}

func TestCopyFileAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello-hotbed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFileAtomic(src, dst, 0o600); err != nil {
		t.Fatalf("copyFileAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello-hotbed" {
		t.Fatalf("dst content = %q, want hello-hotbed", string(got))
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("dst perm = %04o, want 0600", fi.Mode().Perm())
	}
	if err := copyFileAtomic(dir, filepath.Join(dir, "from-dir"), 0o600); err == nil {
		t.Fatal("copyFileAtomic on directory succeeded, want error")
	}
}
