package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureToolsDiskBuildsCachesAndPrunes(t *testing.T) {
	if _, err := findMke2fs(); err != nil {
		t.Skipf("mke2fs unavailable on this host: %v", err)
	}
	cacheDir := filepath.Join(t.TempDir(), toolsDirName)
	guestdBin := filepath.Join(t.TempDir(), "hotbed-guestd")
	if err := os.WriteFile(guestdBin, []byte("ELF-FAKE-GUESTD-V1"), 0o755); err != nil {
		t.Fatal(err)
	}

	p1, err := EnsureToolsDisk(cacheDir, guestdBin)
	if err != nil {
		t.Fatalf("EnsureToolsDisk v1: %v", err)
	}
	info1, err := os.Stat(p1)
	if err != nil || info1.Size() == 0 {
		t.Fatalf("stat %s: info=%v err=%v", p1, info1, err)
	}
	if info1.Size()%toolsDiskAlignBytes != 0 {
		t.Fatalf("tools disk size %d must be aligned to %d bytes for virtio-pmem", info1.Size(), toolsDiskAlignBytes)
	}
	if info1.Mode().Perm()&0o444 != 0o444 {
		t.Fatalf("tools disk mode %o must be world-readable for VMUser", info1.Mode().Perm())
	}

	// Second call with identical binary must hit cache without rewriting the file.
	p1Again, err := EnsureToolsDisk(cacheDir, guestdBin)
	if err != nil {
		t.Fatalf("EnsureToolsDisk v1 cached: %v", err)
	}
	if p1Again != p1 {
		t.Fatalf("cached path = %q, want %q", p1Again, p1)
	}
	info1Again, err := os.Stat(p1Again)
	if err != nil {
		t.Fatal(err)
	}
	if !info1Again.ModTime().Equal(info1.ModTime()) {
		t.Fatalf("cached tools disk was unexpectedly rewritten")
	}

	// Updating hotbed-guestd produces a distinct content-addressed disk image
	// without disturbing the earlier image.
	var lastPath string
	for i := 2; i <= maxCachedToolsDisks+2; i++ {
		if err := os.WriteFile(guestdBin, []byte("ELF-FAKE-GUESTD-V"+strings.Repeat("x", i)), 0o755); err != nil {
			t.Fatal(err)
		}
		p, err := EnsureToolsDisk(cacheDir, guestdBin)
		if err != nil {
			t.Fatalf("EnsureToolsDisk v%d: %v", i, err)
		}
		if p == lastPath {
			t.Fatalf("expected new path for v%d, got %q", i, p)
		}
		lastPath = p
	}
	ents, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	ext4Count := 0
	for _, ent := range ents {
		if strings.HasSuffix(ent.Name(), ".ext4") {
			ext4Count++
		}
	}
	if ext4Count > maxCachedToolsDisks {
		t.Fatalf("expected at most %d cached tools disks after pruning, got %d", maxCachedToolsDisks, ext4Count)
	}
}
