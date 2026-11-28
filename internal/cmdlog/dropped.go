package cmdlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// A log past its limit keeps its first bytes and its last ones, and the
// bytes between are dropped: freed with a hole punched in the file, so
// offsets into the log stay where they were and readers resume where they
// left off. Its .dropped file says which bytes; readers skip them, and show
// a marker in their place.

// Dropped is the range of a log's bytes dropped, From up to To; empty when
// none are.
type Dropped struct{ From, To int64 }

// Bytes is how many bytes are dropped.
func (d Dropped) Bytes() int64 { return d.To - d.From }

func (d Dropped) has(off int64) bool { return off >= d.From && off < d.To }

// punch frees bytes of a log: punchHole, but for tests.
var punch = punchHole

func droppedPath(path string) string { return path + ".dropped" }

// ReadDropped reads which of the log at path's bytes are dropped.
func ReadDropped(path string) (Dropped, error) {
	b, err := os.ReadFile(droppedPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return Dropped{}, nil
	}
	if err != nil {
		return Dropped{}, err
	}
	var d Dropped
	if _, err := fmt.Sscanf(string(b), "%d %d\n", &d.From, &d.To); err != nil || d.From < 0 || d.To < d.From {
		return Dropped{}, fmt.Errorf("%s: not a dropped range: %q", droppedPath(path), b)
	}
	return d, nil
}

// Remove removes the log at path and its sidecar records (.dropped, .ts, .tty).
func Remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	for _, sidecar := range []string{droppedPath(path), tsPath(path), ttyPath(path)} {
		if serr := os.Remove(sidecar); serr != nil && !errors.Is(serr, os.ErrNotExist) {
			err = errors.Join(err, serr)
		}
	}
	return err
}

// KeptBytes is how many bytes of the log at path are kept: its logical size
// less any range Compact dropped and punched out. Unlike Stat_t.Blocks, this
// does not depend on filesystem compression or ZFS TXG flush timing.
func KeptBytes(path string, size int64) int64 {
	d, err := ReadDropped(path)
	if err != nil || d.Bytes() <= 0 {
		return size
	}
	return max(0, size-d.Bytes())
}

// Limit is what a log keeps once past it: its first Head bytes (to the next
// slack boundary) and about its last Tail.
type Limit struct{ Head, Tail int64 }

// slack is how far past its limit a log grows before it is compacted again,
// and the unit it is compacted in, so a busy log is not compacted on every
// write.
const slack = 1 << 20

// Compact drops a log's bytes past l, keeping its first l.Head bytes and its
// last l.Tail (to the next slack boundary). It records the bytes it drops
// before it frees them, so a reader that checks the record after it reads
// knows whether what it read was there. It returns the log's dropped range.
// Only one Compact may run on a log at once.
func Compact(path string, l Limit) (Dropped, error) {
	if err := l.Check(); err != nil {
		return Dropped{}, err
	}
	d, err := ReadDropped(path)
	if err != nil {
		return d, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return d, err
	}
	to := (st.Size() - l.Tail) / slack * slack
	if d == (Dropped{}) {
		// From a slack boundary, as every later end is: filesystems free
		// only whole blocks.
		head := (l.Head + slack - 1) / slack * slack
		d.From, d.To = head, head
	}
	if to-d.To < slack {
		return d, nil
	}
	next := Dropped{From: d.From, To: to}
	if err := writeDropped(path, next); err != nil {
		return d, err
	}
	if err := punch(f, d.To, to-d.To); err != nil {
		// The bytes are still there: readers are to see them.
		err = fmt.Errorf("free %s's dropped bytes: %w", path, err)
		if d.Bytes() == 0 {
			return d, errors.Join(err, removeDropped(path))
		}
		return d, errors.Join(err, writeDropped(path, d))
	}
	return next, nil
}

// Check says whether l can limit a log: it keeps a tail of at least slack,
// which a log's last bytes, still being written, always are.
func (l Limit) Check() error {
	if l.Head < 0 || l.Tail < slack {
		return fmt.Errorf("log limit %+v: keep a head of at least 0 bytes and a tail of at least %d", l, slack)
	}
	return nil
}

// CheckPunch says whether logs in dir can drop bytes: whether its
// filesystem frees a range of a file, as Compact needs.
func CheckPunch(dir string) error {
	f, err := os.CreateTemp(dir, ".punch-check.*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Truncate(2 * slack); err != nil {
		return err
	}
	if err := punch(f, 0, slack); err != nil {
		return fmt.Errorf("%s cannot free part of a file (hole punching), which keeping command logs to their limit needs: %w", dir, err)
	}
	return nil
}

func removeDropped(path string) error {
	if err := os.Remove(droppedPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writeDropped records d for the log at path, atomically and durably
// before it returns: a crash leaves the old record or this one, and never
// a hole without the record that explains it.
func writeDropped(path string, d Dropped) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".dropped.*")
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tmp, "%d %d\n", d.From, d.To); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), droppedPath(path)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Marker is what readers show in place of dropped bytes.
func Marker(d Dropped) string {
	return fmt.Sprintf("[hotbed: %s of output dropped here: the log keeps its first and last bytes]", size(d.Bytes()))
}

func size(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", b)
}
