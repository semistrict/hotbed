//go:build linux

package baseimage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// unmountSubmounts unmounts any leftover mounts strictly underneath rootDir
// (e.g. API VFS or bind mounts left by an interrupted chroot provisioning)
// deepest-first, falling back to MNT_DETACH when a mountpoint is EBUSY.
func unmountSubmounts(rootDir string) error {
	if rootDir == "" {
		return nil
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	mounts := parseSubmounts(string(data), rootDir)
	var errs []error
	for _, mp := range mounts {
		if err := unix.Unmount(mp, 0); err != nil {
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
				continue
			}
			if errors.Is(err, unix.EBUSY) {
				if derr := unix.Unmount(mp, unix.MNT_DETACH); derr != nil && !errors.Is(derr, unix.EINVAL) && !errors.Is(derr, unix.ENOENT) {
					errs = append(errs, fmt.Errorf("lazy unmount %s: %w", mp, derr))
				}
				continue
			}
			errs = append(errs, fmt.Errorf("unmount %s: %w", mp, err))
		}
	}
	return errors.Join(errs...)
}
