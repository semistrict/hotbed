package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// The worker unit depends only on network-online.target (no tailscaled or ZFS mounts).
func TestSystemdWorkerUnitDependencies(t *testing.T) {
	unit := SystemdWorkerUnit("/usr/local/bin/hotbed", "/var/lib/hotbed/worker")
	for _, want := range []string{"After=network-online.target\n", "Wants=network-online.target\n"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "tailscaled") || strings.Contains(unit, "zfs") || strings.Contains(unit, "Requires=") {
		t.Fatalf("unit has unexpected dependency:\n%s", unit)
	}
}
