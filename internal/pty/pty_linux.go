//go:build linux

package pty

import (
	"fmt"
	"os"
	"syscall"

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
		n       uint32
		ctrlErr error
	)
	if err := rc.Control(func(fd uintptr) {
		if e := unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); e != nil {
			ctrlErr = fmt.Errorf("unlock pty: %w", e)
			return
		}
		v, e := unix.IoctlGetUint32(int(fd), unix.TIOCGPTN)
		if e != nil {
			ctrlErr = fmt.Errorf("get pty number: %w", e)
			return
		}
		n = v
	}); err != nil {
		master.Close()
		return nil, nil, "", err
	}
	if ctrlErr != nil {
		master.Close()
		return nil, nil, "", ctrlErr
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", n)
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
