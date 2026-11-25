package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// toolsDirName holds cached read-only ext4 host-tools disk images under
	// Manager Config.RunDir. Reconnect skips this directory.
	toolsDirName = "_tools"

	// toolsDiskHeadroomBytes reserves space for ext4 metadata (superblock,
	// inode tables, block bitmaps, directory blocks) around hotbed-guestd.
	toolsDiskHeadroomBytes = 8 << 20

	// toolsDiskAlignBytes aligns the tools disk image size to 2 MiB so
	// Firecracker can map it as a read-only virtio-pmem KVM memslot on both
	// 4 KiB (x86_64) and 16 KiB (arm64 Asahi) hosts.
	toolsDiskAlignBytes = 2 << 20

	// maxCachedToolsDisks bounds old guestd-*.ext4 images kept on disk.
	// Linux keeps unlinked files open for already-running Firecracker VMs.
	maxCachedToolsDisks = 4
)

// EnsureToolsDisk builds (or reuses by content SHA-256) a 2 MiB-aligned
// read-only ext4 disk image containing /hotbed-guestd, /dev/, and /newroot/.
// Firecracker attaches this image as a read-only virtio-pmem root device
// (/dev/pmem0) so every VM runs the host's current hotbed-guestd directly from
// the shared host page cache without baking it into cladefs OS or project base
// images (and avoiding arm64 Firecracker PCI virtio-blk boot hangs).
func EnsureToolsDisk(cacheDir, guestdBin string) (string, error) {
	if cacheDir == "" {
		return "", fmt.Errorf("vm tools disk: cacheDir is required")
	}
	if guestdBin == "" {
		return "", fmt.Errorf("vm tools disk: guestdBin is required")
	}
	shaHex, size, err := hashFileSHA256(guestdBin)
	if err != nil {
		return "", fmt.Errorf("vm tools disk: hash %s: %w", guestdBin, err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("vm tools disk: mkdir %s: %w", cacheDir, err)
	}
	_ = os.Chmod(cacheDir, 0o755)

	dstPath := filepath.Join(cacheDir, "guestd-"+shaHex[:16]+".ext4")
	if info, err := os.Stat(dstPath); err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size()%toolsDiskAlignBytes == 0 {
		return dstPath, nil
	}

	mke2fsBin, err := findMke2fs()
	if err != nil {
		return "", fmt.Errorf("vm tools disk: %w", err)
	}

	stageDir, err := os.MkdirTemp(cacheDir, ".stage-*")
	if err != nil {
		return "", fmt.Errorf("vm tools disk: create staging dir: %w", err)
	}
	defer os.RemoveAll(stageDir)

	for _, sub := range []string{"dev", "newroot"} {
		if err := os.Mkdir(filepath.Join(stageDir, sub), 0o755); err != nil {
			return "", fmt.Errorf("vm tools disk: mkdir %s: %w", sub, err)
		}
	}
	if err := copyFileMode(guestdBin, filepath.Join(stageDir, "hotbed-guestd"), 0o755); err != nil {
		return "", fmt.Errorf("vm tools disk: stage %s: %w", guestdBin, err)
	}

	tmpFile, err := os.CreateTemp(cacheDir, ".guestd-*.ext4.tmp")
	if err != nil {
		return "", fmt.Errorf("vm tools disk: create temp image: %w", err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	const blockSize = int64(4096)
	rawBytes := size + toolsDiskHeadroomBytes
	totalBytes := ((rawBytes + toolsDiskAlignBytes - 1) / toolsDiskAlignBytes) * toolsDiskAlignBytes
	blocks := totalBytes / blockSize
	cmd := exec.Command(mke2fsBin,
		"-t", "ext4",
		"-d", stageDir,
		"-b", "4096",
		"-m", "0",
		"-O", "^has_journal,^resize_inode",
		"-L", "hotbed-tools",
		"-q",
		"-F",
		tmpPath,
		fmt.Sprintf("%d", blocks),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("vm tools disk: mke2fs %s: %w: %s", dstPath, err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return "", fmt.Errorf("vm tools disk: chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return "", fmt.Errorf("vm tools disk: rename %s -> %s: %w", tmpPath, dstPath, err)
	}
	ok = true
	pruneOldToolsDisks(cacheDir, filepath.Base(dstPath), maxCachedToolsDisks)
	return dstPath, nil
}

func findMke2fs() (string, error) {
	if p, err := exec.LookPath("mke2fs"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/sbin/mke2fs", "/usr/sbin/mke2fs"} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("mke2fs not found on PATH or in /sbin:/usr/sbin (install e2fsprogs)")
}

func hashFileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func pruneOldToolsDisks(cacheDir, keepName string, maxKeep int) {
	ents, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	type diskEntry struct {
		name    string
		modUnix int64
	}
	var disks []diskEntry
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasPrefix(name, "guestd-") || !strings.HasSuffix(name, ".ext4") {
			continue
		}
		if name == keepName {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		disks = append(disks, diskEntry{name: name, modUnix: info.ModTime().UnixNano()})
	}
	if len(disks)+1 <= maxKeep {
		return
	}
	slices.SortFunc(disks, func(a, b diskEntry) int {
		if a.modUnix > b.modUnix {
			return -1
		}
		if a.modUnix < b.modUnix {
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	for _, d := range disks[maxKeep-1:] {
		_ = os.Remove(filepath.Join(cacheDir, d.name))
	}
}
