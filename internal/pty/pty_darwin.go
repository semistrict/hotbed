//go:build darwin

package pty

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (*os.File, *os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	rc, err := master.SyscallConn()
	if err != nil {
		master.Close()
		return nil, nil, "", fmt.Errorf("ptmx syscall conn: %w", err)
	}
	var (
		slavePath string
		ctrlErr   error
	)
	if err := rc.Control(func(fd uintptr) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, unix.TIOCPTYGRANT, 0); errno != 0 {
			ctrlErr = fmt.Errorf("grant pty: %w", errno)
			return
		}
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, unix.TIOCPTYUNLK, 0); errno != 0 {
			ctrlErr = fmt.Errorf("unlock pty: %w", errno)
			return
		}
		var nameBuf [128]byte
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&nameBuf[0]))); errno != 0 {
			ctrlErr = fmt.Errorf("get pty name: %w", errno)
			return
		}
		i := bytes.IndexByte(nameBuf[:], 0)
		if i < 0 {
			ctrlErr = fmt.Errorf("unterminated pty slave name")
			return
		}
		slavePath = string(nameBuf[:i])
	}); err != nil {
		master.Close()
		return nil, nil, "", err
	}
	if ctrlErr != nil {
		master.Close()
		return nil, nil, "", ctrlErr
	}
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, "", fmt.Errorf("open %s: %w", slavePath, err)
	}
	return master, slave, slavePath, nil
}

func resizePTY(master *os.File, cols, rows uint16) error {
	if cols == 0 && rows == 0 {
		return nil
	}
	rc, err := master.SyscallConn()
	if err != nil {
		return err
	}
	var ctrlErr error
	if err := rc.Control(func(fd uintptr) {
		ctrlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{
			Row: rows,
			Col: cols,
		})
	}); err != nil {
		return err
	}
	return ctrlErr
}
