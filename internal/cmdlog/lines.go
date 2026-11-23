// Package cmdlog reads a command's output log: the file systemd appends the
// command's stdout and stderr to. A log can grow without limit while it is
// read, so nothing here reads a whole log into memory: Lines follows it from
// where it left off.
package cmdlog

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// MaxLine is the longest line Lines hands out; the rest of a longer line is
// dropped.
const MaxLine = 64 << 10

// Lines reads a growing log line by line, each call from where the last one
// stopped, in memory bounded by MaxLine however long the log or its lines.
// It is not safe for concurrent use.
type Lines struct {
	path string
	off  int64  // bytes read from the log
	part []byte // the line being read, up to MaxLine
	buf  []byte
}

// NewLines reads the log at path from its start.
func NewLines(path string) *Lines { return &Lines{path: path} }

// Path is the log's path.
func (l *Lines) Path() string { return l.path }

// Offset is how far into the log Lines has read.
func (l *Lines) Offset() int64 { return l.off }

// Read calls fn with each line completed since the last call, without its
// newline (nor a carriage return before it), reading at most max bytes of
// the log (0 for all of it, which, for a log still written, may be never).
// A line longer than MaxLine is cut to it. Dropped bytes end the line
// before them and read as one line, their Marker. A log not yet written is
// empty. fn must not keep line. The log must not be compacted while Lines
// reads it.
func (l *Lines) Read(fn func(line []byte), max int64) error {
	d, err := ReadDropped(l.path)
	if err != nil {
		return err
	}
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if l.buf == nil {
		l.buf = make([]byte, 64<<10)
	}
	for read := int64(0); max == 0 || read < max; {
		if d.has(l.off) {
			if len(l.part) > 0 {
				l.emit(fn)
			}
			fn([]byte(Marker(d)))
			l.off = d.To
		}
		buf := l.buf
		if max > 0 {
			buf = buf[:min(int64(len(buf)), max-read)]
		}
		if l.off < d.From {
			buf = buf[:min(int64(len(buf)), d.From-l.off)]
		}
		n, err := f.ReadAt(buf, l.off)
		l.off += int64(n)
		read += int64(n)
		l.feed(buf[:n], fn)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Flush calls fn with the last line if the log ended without a newline.
// Call it once the log is complete.
func (l *Lines) Flush(fn func(line []byte)) {
	if len(l.part) > 0 {
		l.emit(fn)
	}
}

func (l *Lines) feed(data []byte, fn func([]byte)) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		chunk := data
		if i >= 0 {
			chunk = data[:i]
		}
		if room := MaxLine - len(l.part); len(chunk) > room {
			chunk = chunk[:room]
		}
		l.part = append(l.part, chunk...)
		if i < 0 {
			return
		}
		l.emit(fn)
		data = data[i+1:]
	}
}

func (l *Lines) emit(fn func([]byte)) {
	fn(bytes.TrimSuffix(l.part, []byte("\r")))
	l.part = l.part[:0]
}
