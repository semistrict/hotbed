package unit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Wait returns once every process of the unit has exited, or ctx is done.
// The kernel rewrites a cgroup's cgroup.events when it empties, so Wait
// watches that file with inotify instead of asking systemd over and over. A
// unit whose cgroup is already gone has exited.
func Wait(ctx context.Context, slice, name string) error {
	events := filepath.Join("/sys/fs/cgroup", SliceDir(slice), name+".service", "cgroup.events")
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify: %w", err)
	}
	// A non-blocking descriptor joins Go's poller, so a deadline interrupts
	// a read in progress.
	f := os.NewFile(uintptr(fd), "inotify")
	defer f.Close()
	if _, err := unix.InotifyAddWatch(fd, events, unix.IN_MODIFY); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("watch %s: %w", events, err)
	}
	stop := context.AfterFunc(ctx, func() { f.SetReadDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, 4096)
	for {
		// Read after the watch is in place, so an exit between the two
		// is not missed.
		populated, err := populated(events)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !populated {
			return nil
		}
		if _, err := f.Read(buf); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read inotify: %w", err)
		}
	}
}

// populated reads whether a cgroup still has processes.
func populated(events string) (bool, error) {
	b, err := os.ReadFile(events)
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := bytes.CutPrefix(sc.Bytes(), []byte("populated ")); ok {
			return string(v) != "0", nil
		}
	}
	return false, fmt.Errorf("%s has no populated line: %q", events, b)
}
