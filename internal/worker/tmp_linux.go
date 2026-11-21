package worker

import "golang.org/x/sys/unix"

// detach unmounts target lazily: a dead FUSE daemon's mount cannot be
// unmounted any other way.
func detach(target string) error { return unix.Unmount(target, unix.MNT_DETACH) }
