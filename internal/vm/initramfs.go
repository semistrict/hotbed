package vm

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxCachedInitramfses bounds the old guestd-*.cpio kept beside the newest:
// machines already running keep theirs open.
const maxCachedInitramfses = 4

// EnsureInitramfs builds (or reuses, by hotbed-guestd's SHA-256) an initramfs
// in dir holding /hotbed-guestd, /dev/console, and the mount points it needs:
// what a machined machine boots instead of Firecracker's tools disk (machined
// has no pmem). hotbed-guestd runs as its init (rdinit=/hotbed-guestd), mounts
// the workspace's cell, and switches root onto it, keeping the initramfs at
// /run/hotbed-tools (cmd/hotbed-guestd's pivotToWorkspaceRoot).
func EnsureInitramfs(dir, guestdBin string) (string, error) {
	if dir == "" || guestdBin == "" {
		return "", fmt.Errorf("vm initramfs: dir and guestdBin are required")
	}
	shaHex, _, err := hashFileSHA256(guestdBin)
	if err != nil {
		return "", fmt.Errorf("vm initramfs: hash %s: %w", guestdBin, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("vm initramfs: mkdir %s: %w", dir, err)
	}
	dst := filepath.Join(dir, "guestd-"+shaHex[:16]+".cpio")
	if info, err := os.Stat(dst); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return dst, nil
	}
	guestd, err := os.Open(guestdBin)
	if err != nil {
		return "", fmt.Errorf("vm initramfs: %w", err)
	}
	defer guestd.Close()
	info, err := guestd.Stat()
	if err != nil {
		return "", fmt.Errorf("vm initramfs: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".guestd-*.cpio")
	if err != nil {
		return "", fmt.Errorf("vm initramfs: %w", err)
	}
	defer os.Remove(tmp.Name())
	w := newcWriter{w: bufio.NewWriter(tmp)}
	for _, d := range []string{"dev", "newroot", "proc", "run", "sys"} {
		w.entry(d, modeDir|0o755, 0, 0, 0, nil)
	}
	w.entry("dev/console", modeChar|0o600, 5, 1, 0, nil)
	w.entry("hotbed-guestd", modeFile|0o755, 0, 0, info.Size(), guestd)
	w.entry("TRAILER!!!", 0, 0, 0, 0, nil)
	if w.err == nil {
		w.err = w.w.Flush()
	}
	if w.err != nil {
		tmp.Close()
		return "", fmt.Errorf("vm initramfs: write %s: %w", tmp.Name(), w.err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", fmt.Errorf("vm initramfs: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("vm initramfs: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", fmt.Errorf("vm initramfs: %w", err)
	}
	pruneInitramfses(dir, dst)
	return dst, nil
}

// pruneInitramfses removes all but the newest maxCachedInitramfses
// guestd-*.cpio in dir, never keep.
func pruneInitramfses(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type cached struct {
		path string
		mod  int64
	}
	var old []cached
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if p == keep || !strings.HasPrefix(e.Name(), "guestd-") || !strings.HasSuffix(e.Name(), ".cpio") {
			continue
		}
		if info, err := e.Info(); err == nil {
			old = append(old, cached{p, info.ModTime().UnixNano()})
		}
	}
	slices.SortFunc(old, func(a, b cached) int { return int(b.mod - a.mod) })
	for i, c := range old {
		if i >= maxCachedInitramfses-1 {
			_ = os.Remove(c.path)
		}
	}
}

// The file types of a cpio entry's mode.
const (
	modeDir  = 0o040000
	modeChar = 0o020000
	modeFile = 0o100000
)

// newcWriter writes the "new ASCII" cpio format, the kernel's initramfs's:
// a 110-byte header of hex fields, the name, the data, each padded to 4
// bytes. It keeps the first error.
type newcWriter struct {
	w   *bufio.Writer
	ino uint32
	err error
}

func (n *newcWriter) entry(name string, mode, rdevMajor, rdevMinor uint32, size int64, data io.Reader) {
	if n.err != nil {
		return
	}
	n.ino++
	nlink := uint32(1)
	if mode&modeDir != 0 {
		nlink = 2
	}
	// magic, ino, mode, uid, gid, nlink, mtime, filesize, devmajor,
	// devminor, rdevmajor, rdevminor, namesize, check.
	_, n.err = fmt.Fprintf(n.w, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		n.ino, mode, 0, 0, nlink, 0, size, 0, 0, rdevMajor, rdevMinor, len(name)+1, 0)
	if n.err != nil {
		return
	}
	if _, n.err = n.w.WriteString(name + "\x00"); n.err != nil {
		return
	}
	n.pad(110 + len(name) + 1)
	if data != nil {
		var copied int64
		copied, n.err = io.Copy(n.w, io.LimitReader(data, size))
		if n.err == nil && copied != size {
			n.err = fmt.Errorf("%s: %d bytes, want %d", name, copied, size)
		}
	}
	n.pad(int(size))
}

func (n *newcWriter) pad(written int) {
	if n.err == nil && written%4 != 0 {
		_, n.err = n.w.Write(make([]byte, 4-written%4))
	}
}
