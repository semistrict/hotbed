//go:build linux

package unit

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// prepareChrootTargets makes each bind's mountpoint in rootDir: a directory, or
// for a file a regular file (replacing a symlink or special file). Paths are
// resolved inside rootDir as a chrooted process resolves them, so symlinks in
// the rootfs never lead outside it.
func prepareChrootTargets(rootDir string, binds []Bind) error {
	root, err := os.Open(rootDir)
	if err != nil {
		return fmt.Errorf("open root %s: %w", rootDir, err)
	}
	defer root.Close()
	for _, b := range binds {
		if err := prepareTarget(root, b); err != nil {
			return err
		}
	}
	return nil
}

func prepareTarget(root *os.File, b Bind) error {
	target := path.Clean(b.Target)
	src, err := os.Stat(b.Source)
	if err != nil {
		return fmt.Errorf("bind %s: %w", b.Target, err)
	}
	if src.IsDir() {
		return mkdirAllIn(root, target)
	}
	dir, base := path.Split(target)
	if err := mkdirAllIn(root, dir); err != nil {
		return err
	}
	parent, err := openIn(root, dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	// Replace non-regular file mountpoints (such as a dangling /etc/resolv.conf
	// symlink or a FIFO) with an empty regular file.
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			return nil
		case unix.S_IFDIR:
			return fmt.Errorf("make mountpoint %s: a directory is where a file's goes", target)
		}
		if err := unix.Unlinkat(pfd, base, 0); err != nil {
			return fmt.Errorf("replace %s: %w", target, err)
		}
	}
	fd, err := unix.Openat(pfd, base, unix.O_CREAT|unix.O_EXCL|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o644)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			if err := unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT == unix.S_IFREG {
				return nil
			}
		}
		return fmt.Errorf("make mountpoint %s: %w", target, err)
	}
	return unix.Close(fd)
}

func openIn(root *os.File, target string) (*os.File, error) {
	rel := strings.TrimPrefix(path.Clean("/"+target), "/")
	if rel == "" {
		rel = "."
	}
	fd, err := unix.Openat2(int(root.Fd()), rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s in chroot: %w", target, err)
	}
	return os.NewFile(uintptr(fd), target), nil
}

func mkdirAllIn(root *os.File, target string) error {
	cur := "/"
	for part := range strings.SplitSeq(strings.Trim(path.Clean("/"+target), "/"), "/") {
		if part == "" {
			continue
		}
		next := path.Join(cur, part)
		f, err := openIn(root, next)
		if err == nil {
			f.Close()
			cur = next
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
		parent, err := openIn(root, cur)
		if err != nil {
			return err
		}
		err = unix.Mkdirat(int(parent.Fd()), part, 0o755)
		parent.Close()
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("make mountpoint %s: %w", next, err)
		}
		cur = next
	}
	return nil
}
