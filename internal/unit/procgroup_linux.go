package unit

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitExited returns once child pid has exited, leaving it unreaped
// (WNOWAIT): its ID stays its own until it is reaped.
func waitExited(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
