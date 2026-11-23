package cmdlog

import (
	"os"

	"golang.org/x/sys/unix"
)

// punchHole frees n bytes of f from off, which then read as zeros; f's size
// stays.
func punchHole(f *os.File, off, n int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n)
}
