package cmdlog

import (
	"errors"
	"io"
	"os"
)

// ReadChunk reads the log at path, open as f, from off into buf, for a
// reader that follows it as it grows: it returns what to show and the
// offset to read from next. Dropped bytes read as their Marker, on a line
// of its own. data is empty at the log's end. It is safe while the log is
// compacted: it checks what is dropped after it reads, and never returns
// bytes dropped meanwhile.
func ReadChunk(f *os.File, path string, off int64, buf []byte) (data []byte, next int64, err error) {
	d, err := ReadDropped(path)
	if err != nil {
		return nil, off, err
	}
	if d.has(off) {
		return marker(f, d, off)
	}
	if off < d.From {
		buf = buf[:min(int64(len(buf)), d.From-off)]
	}
	n, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, off, err
	}
	if n == 0 {
		return nil, off, nil
	}
	// The bytes read may have been dropped while they were read: Compact
	// records what it drops before it frees it.
	if after, err := ReadDropped(path); err != nil {
		return nil, off, err
	} else if after != d && off+int64(n) > after.From && off < after.To {
		if off >= after.From {
			return marker(f, after, off)
		}
		n = int(after.From - off)
	}
	return buf[:n], off + int64(n), nil
}

// marker is what a follower at off, inside d, reads in place of d's bytes
// it has yet to read: their Marker, on a line of its own, and where to go
// on from.
func marker(f *os.File, d Dropped, off int64) ([]byte, int64, error) {
	m := Marker(Dropped{From: off, To: d.To}) + "\n"
	nl, err := endsLine(f, off)
	if err != nil {
		return nil, d.From, err
	}
	if !nl {
		m = "\n" + m
	}
	return []byte(m), d.To, nil
}
