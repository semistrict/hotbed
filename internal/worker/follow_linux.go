//go:build linux

package worker

import (
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// logWatcher wakes FollowLog when bytes are written to a command's log file,
// avoiding tight polling while keeping the log file descriptor open.
type logWatcher struct {
	file   *os.File
	events chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newLogWatcher(path string) *logWatcher {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil
	}
	// A non-blocking descriptor joins Go's poller, so SetReadDeadline and
	// Close interrupt a Read in progress.
	f := os.NewFile(uintptr(fd), "inotify:"+path)
	if f == nil {
		_ = unix.Close(fd)
		return nil
	}
	if _, err := unix.InotifyAddWatch(fd, path, unix.IN_MODIFY|unix.IN_CLOSE_WRITE); err != nil {
		_ = f.Close()
		return nil
	}
	w := &logWatcher{
		file:   f,
		events: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go w.loop()
	return w
}

func (w *logWatcher) loop() {
	defer close(w.done)
	buf := make([]byte, 4096)
	for {
		n, err := w.file.Read(buf)
		if n > 0 {
			select {
			case w.events <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

func (w *logWatcher) Events() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.events
}

func (w *logWatcher) Close() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		_ = w.file.SetReadDeadline(time.Now())
		_ = w.file.Close()
		<-w.done
	})
}
