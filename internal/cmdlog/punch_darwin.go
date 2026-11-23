package cmdlog

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fpunchhole is fcntl(2)'s struct fpunchhole.
type fpunchhole struct {
	flags    uint32
	reserved uint32
	offset   int64
	length   int64
}

// punchHole frees n bytes of f from off, which then read as zeros; f's size
// stays. APFS frees whole blocks, so off and n are block multiples here.
func punchHole(f *os.File, off, n int64) error {
	arg := fpunchhole{offset: off, length: n}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), unix.F_PUNCHHOLE, uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return &os.PathError{Op: "fcntl F_PUNCHHOLE", Path: f.Name(), Err: errno}
	}
	return nil
}
