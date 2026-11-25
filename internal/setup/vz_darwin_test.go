//go:build darwin

package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPickVZProcess(t *testing.T) {
	const vzExe = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"
	vfkitStart := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)
	ours := procInfo{pid: 50143, exe: vzExe, start: vfkitStart.Add(2 * time.Second)}
	// Another app's VM, started long before ours.
	earlier := procInfo{pid: 400, exe: vzExe, start: vfkitStart.Add(-time.Hour)}
	// Another app's VM, started long after ours.
	later := procInfo{pid: 600, exe: vzExe, start: vfkitStart.Add(time.Minute)}
	// A Virtualization framework process that runs no VM.
	other := procInfo{pid: 700, exe: "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.Installation.xpc/Contents/MacOS/com.apple.Virtualization.Installation", start: ours.start}

	got, err := pickVZProcess(vfkitStart, []procInfo{earlier, other, ours, later})
	if err != nil || got != ours.pid {
		t.Fatalf("pickVZProcess = (%d, %v), want (%d, nil)", got, err, ours.pid)
	}

	if _, err := pickVZProcess(vfkitStart, []procInfo{earlier, later}); err == nil ||
		err.Error() != "no com.apple.Virtualization.VirtualMachine process started within 10s of vfkit (started 2026-11-25T15:00:00Z)" {
		t.Fatalf("pickVZProcess(none) err = %v", err)
	}

	twin := ours
	twin.pid = 50200
	if _, err := pickVZProcess(vfkitStart, []procInfo{ours, twin}); err == nil ||
		err.Error() != "2 com.apple.Virtualization.VirtualMachine processes started within 10s of vfkit (pids [50143 50200]); cannot tell which runs the worker VM" {
		t.Fatalf("pickVZProcess(two) err = %v", err)
	}
}

// allocatedBytes is how much of path's file the filesystem holds.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

func TestConvertToRawAndEnsureVMDisks(t *testing.T) {
	dir := t.TempDir()
	// A 64 MiB raw "cloud image": 1 MiB of data at its start and at 32 MiB,
	// holes elsewhere.
	src := filepath.Join(dir, "cloud.img")
	data := bytes.Repeat([]byte("hotbed!\n"), 1<<20/8)
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 32 << 20} {
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := ensureRawImage(src, nil)
	if err != nil {
		t.Fatalf("ensureRawImage: %v", err)
	}
	if want := filepath.Join(dir, "cloud.raw"); raw != want {
		t.Fatalf("ensureRawImage = %q, want %q", raw, want)
	}
	got, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("raw image differs from its source")
	}
	if a := allocatedBytes(t, raw); a > 4<<20 {
		t.Fatalf("raw image holds %d bytes, want its 2 MiB of data and holes elsewhere", a)
	}

	// The raw image is converted once: a second call reuses it.
	if err := os.WriteFile(src, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again, err := ensureRawImage(src, nil); err != nil || again != raw {
		t.Fatalf("second ensureRawImage = (%q, %v), want (%q, nil)", again, err, raw)
	}

	p := NewVMPaths(filepath.Join(dir, "hb"))
	cfg := VMConfig{RootSizeBytes: 256 << 20, ZFSSizeBytes: 1 << 30, SwapSizeBytes: 128 << 20}
	if err := ensureVMDisks(raw, cfg, p, nil); err != nil {
		t.Fatalf("ensureVMDisks: %v", err)
	}
	for _, d := range []struct {
		path string
		size int64
	}{{p.RootDisk, cfg.RootSizeBytes}, {p.ZFSDisk, cfg.ZFSSizeBytes}, {p.SwapDisk, cfg.SwapSizeBytes}} {
		fi, err := os.Stat(d.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != d.size || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: size %d mode %04o, want %d and 0600", d.path, fi.Size(), fi.Mode().Perm(), d.size)
		}
	}
	root, err := os.ReadFile(p.RootDisk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(root[:len(want)], want) || !bytes.Equal(root[len(want):], make([]byte, len(root)-len(want))) {
		t.Fatal("root disk is not the cloud image followed by zeros")
	}
	if a := allocatedBytes(t, p.ZFSDisk); a != 0 {
		t.Errorf("new ZFS disk holds %d bytes, want 0", a)
	}

	// Existing disks are left as they are.
	if err := os.WriteFile(p.ZFSDisk, []byte("pool"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureVMDisks(raw, cfg, p, nil); err != nil {
		t.Fatalf("second ensureVMDisks: %v", err)
	}
	if b, _ := os.ReadFile(p.ZFSDisk); string(b) != "pool" {
		t.Fatalf("ensureVMDisks replaced an existing ZFS disk: %q", b)
	}

	// A root disk smaller than the image is refused.
	small := NewVMPaths(filepath.Join(dir, "small"))
	cfg.RootSizeBytes = 16 << 20
	if err := ensureVMDisks(raw, cfg, small, nil); err == nil {
		t.Fatal("ensureVMDisks made a 16 MiB root disk from a 64 MiB image")
	}
}
