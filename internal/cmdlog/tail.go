package cmdlog

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
)

// Tail returns the last n lines of the log at path, without newlines (nor a
// carriage return before one) and as valid UTF-8 (bytes that are not, such
// as a binary's, are U+FFFD), reading backward from its end, so its cost
// is what it returns, not the log's size. Dropped bytes read as one line,
// their Marker. It returns at most maxBytes of output: when the last n
// lines are longer, it returns the lines that fit, and, when not even the
// last line fits, that line's last maxBytes. cut says it returned less
// than n lines while the log has more. A log not yet written has no lines.
func Tail(path string, n, maxBytes int) (lines []string, cut bool, err error) {
	// Bytes dropped while it reads read as zeros: it reads again.
	for {
		d, err := ReadDropped(path)
		if err != nil {
			return nil, false, err
		}
		lines, cut, err := tail(path, d, n, maxBytes)
		if err != nil {
			return nil, false, err
		}
		if after, err := ReadDropped(path); err != nil {
			return nil, false, err
		} else if after == d {
			return lines, cut, nil
		}
	}
}

// tail is Tail of a log whose dropped bytes are d.
func tail(path string, d Dropped, n, maxBytes int) (lines []string, cut bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	end := st.Size()
	if n <= 0 || end == 0 {
		return []string{}, false, nil
	}
	// The last byte, if a newline, ends the last line rather than starting
	// an empty one after it.
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, end-1); err != nil {
		return nil, false, err
	}
	if last[0] == '\n' {
		end--
	}
	// Read backward until the tail holds n line breaks (the start of the
	// n-th line from the end), reaches the start, or is maxBytes long,
	// stepping over the dropped bytes, which read as their marker's line.
	var tail []byte
	off := end
	const chunk = 64 << 10
	for off > 0 && bytes.Count(tail, []byte{'\n'}) < n && len(tail) <= maxBytes {
		if d.Bytes() > 0 && off == d.To {
			tail = append([]byte(Marker(d)+"\n"), tail...)
			off = d.From
			// The head's last line ends before the marker.
			if nl, err := endsLine(f, off); err != nil {
				return nil, false, err
			} else if !nl {
				tail = append([]byte{'\n'}, tail...)
			}
			continue
		}
		floor := int64(0)
		if off > d.To {
			floor = d.To
		}
		size := min(int64(chunk), off-floor, int64(maxBytes+1-len(tail)))
		buf := make([]byte, size)
		if _, err := f.ReadAt(buf, off-size); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		off -= size
		tail = append(buf, tail...)
	}
	parts := strings.Split(string(tail), "\n")
	switch {
	case len(parts) > n:
		// It reached the n-th line's start.
		parts = parts[len(parts)-n:]
	case len(tail) > maxBytes:
		// It stopped at maxBytes, inside the first part: that goes, unless
		// it is all there is, when its end stays.
		cut = true
		if len(parts) > 1 {
			parts = parts[1:]
		} else {
			parts[0] = parts[0][len(parts[0])-maxBytes:]
		}
	}
	for i, p := range parts {
		parts[i] = strings.ToValidUTF8(strings.TrimSuffix(p, "\r"), "\uFFFD")
	}
	return parts, cut, nil
}

// endsLine says whether the log's bytes before off end a line: off is its
// start, or follows a newline.
func endsLine(f *os.File, off int64) (bool, error) {
	if off == 0 {
		return true, nil
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off-1); err != nil {
		return false, err
	}
	return b[0] == '\n', nil
}
