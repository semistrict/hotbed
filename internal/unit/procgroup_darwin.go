package unit

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitExited returns once child pid has exited, leaving it unreaped: a
// kqueue's NOTE_EXIT fires at exit, before any wait reaps it, and its ID
// stays its own until then. One already exited is ESRCH to register.
func waitExited(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	change := []unix.Kevent_t{{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	out := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, change, out, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ESRCH):
			return nil
		case err != nil:
			return err
		case n == 0:
			continue
		}
		if out[0].Flags&unix.EV_ERROR != 0 {
			if e := unix.Errno(out[0].Data); e != unix.ESRCH {
				return e
			}
		}
		return nil
	}
}
