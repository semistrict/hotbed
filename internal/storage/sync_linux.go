//go:build linux

package storage

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// flush writes the filesystem's dirty pages under path to disk. A snapshot
// holds only what the filesystem has been given: pages a process wrote
// through mmap (as linkers do) and that the kernel has not written back yet
// would be missing from it, as zeros, on zfs.
func flush(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Syncfs(int(f.Fd())); err != nil {
		return fmt.Errorf("syncfs %s: %w", path, err)
	}
	return nil
}
