//go:build linux || darwin

package cmdlog

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// DataBytes returns the number of non-hole bytes in the file at path using
// SEEK_DATA / SEEK_HOLE. Unlike Stat_t.Blocks, this reflects hole punching
// immediately on ZFS regardless of compression or TXG commit timing.
func DataBytes(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	var total, off int64
	for off < size {
		data, err := unix.Seek(int(f.Fd()), off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("SEEK_DATA %s at %d: %w", path, off, err)
		}
		hole, err := unix.Seek(int(f.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			return 0, fmt.Errorf("SEEK_HOLE %s at %d: %w", path, data, err)
		}
		if hole > size {
			hole = size
		}
		total += hole - data
		off = hole
	}
	return total, nil
}
