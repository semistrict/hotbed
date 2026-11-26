package cmdlog

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
)

// LineEntry is one line returned by TailEntries along with the unix-ms
// timestamp at which it was received (0 when unknown or synthetic marker).
type LineEntry struct {
	Text       string
	ReceivedAt int64
}

// Tail returns the last n lines of the log at path, without newlines (nor a
// carriage return before one) and as valid UTF-8 (bytes that are not, such
// as a binary's, are U+FFFD), reading backward from its end, so its cost
// is what it returns, not the log's size. Dropped bytes read as one line,
// their Marker. It returns at most maxBytes of output: when the last n
// lines are longer, it returns the lines that fit, and, when not even the
// last line fits, that line's last maxBytes. cut says it returned less
// than n lines while the log has more. A log not yet written has no lines.
func Tail(path string, n, maxBytes int) (lines []string, cut bool, err error) {
	entries, cut, err := TailEntries(path, n, maxBytes)
	if err != nil {
		return nil, false, err
	}
	lines = make([]string, len(entries))
	for i, e := range entries {
		lines[i] = e.Text
	}
	return lines, cut, nil
}

// TailEntries is Tail with each line's received timestamp (unix ms) from
// <path>.ts.
func TailEntries(path string, n, maxBytes int) (entries []LineEntry, cut bool, err error) {
	// Bytes dropped while it reads read as zeros: it reads again.
	for {
		d, err := ReadDropped(path)
		if err != nil {
			return nil, false, err
		}
		entries, cut, err := tailEntries(path, d, n, maxBytes)
		if err != nil {
			return nil, false, err
		}
		if after, err := ReadDropped(path); err != nil {
			return nil, false, err
		} else if after == d {
			return entries, cut, nil
		}
	}
}

type tailSeg struct {
	startOff int64 // logical byte offset in <path>; -1 for synthetic marker/separator
	data     []byte
}

// tailEntries is TailEntries of a log whose dropped bytes are d.
func tailEntries(path string, d Dropped, n, maxBytes int) (entries []LineEntry, cut bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []LineEntry{}, false, nil
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
		return []LineEntry{}, false, nil
	}
	// The last byte, if a newline, ends the last line rather than starting
	// an empty one after it.
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, end-1); err != nil {
		return nil, false, err
	}
	hadTrailingNL := last[0] == '\n'
	if hadTrailingNL {
		end--
	}
	// Read backward until the tail holds n line breaks (the start of the
	// n-th line from the end), reaches the start, or is maxBytes long,
	// stepping over the dropped bytes, which read as their marker's line.
	var segs []tailSeg // accumulated in reverse order (newest first)
	totalLen := 0
	nlCount := 0
	off := end
	const chunk = 64 << 10
	for off > 0 && nlCount < n && totalLen <= maxBytes {
		if d.Bytes() > 0 && off == d.To {
			m := []byte(Marker(d) + "\n")
			segs = append(segs, tailSeg{startOff: -1, data: m})
			totalLen += len(m)
			nlCount++
			off = d.From
			// The head's last line ends before the marker.
			if nl, err := endsLine(f, off); err != nil {
				return nil, false, err
			} else if !nl {
				segs = append(segs, tailSeg{startOff: -1, data: []byte{'\n'}})
				totalLen++
				nlCount++
			}
			continue
		}
		floor := int64(0)
		if off > d.To {
			floor = d.To
		}
		size := min(int64(chunk), off-floor, int64(maxBytes+1-totalLen))
		buf := make([]byte, size)
		if _, err := f.ReadAt(buf, off-size); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		off -= size
		segs = append(segs, tailSeg{startOff: off, data: buf})
		totalLen += len(buf)
		nlCount += bytes.Count(buf, []byte{'\n'})
	}
	// Assemble in chronological order (oldest first) and build a parallel
	// byte-to-logical-offset lookup via the segment boundaries.
	tail := make([]byte, 0, totalLen)
	ordered := make([]tailSeg, 0, len(segs))
	for i := len(segs) - 1; i >= 0; i-- {
		ordered = append(ordered, segs[i])
		tail = append(tail, segs[i].data...)
	}

	tsIdx, _ := ReadTimestamps(path)
	// logicalOff maps an index into tail to its logical offset in <path> (or -1).
	logicalOff := func(idx int) int64 {
		if idx < 0 {
			return -1
		}
		pos := 0
		for _, seg := range ordered {
			if idx < pos+len(seg.data) {
				if seg.startOff < 0 {
					return -1
				}
				return seg.startOff + int64(idx-pos)
			}
			pos += len(seg.data)
		}
		// One past the end: if the log had a trailing newline, that newline sits at `end`.
		if idx == len(tail) && hadTrailingNL {
			return end
		}
		return -1
	}

	// Split tail into line spans [start, end) within tail.
	type span struct{ start, end int }
	var spans []span
	lineStart := 0
	for i := 0; i < len(tail); i++ {
		if tail[i] == '\n' {
			spans = append(spans, span{start: lineStart, end: i})
			lineStart = i + 1
		}
	}
	spans = append(spans, span{start: lineStart, end: len(tail)})

	switch {
	case len(spans) > n:
		// It reached the n-th line's start.
		spans = spans[len(spans)-n:]
	case len(tail) > maxBytes:
		// It stopped at maxBytes, inside the first part: that goes, unless
		// it is all there is, when its end stays.
		cut = true
		if len(spans) > 1 {
			spans = spans[1:]
		} else {
			spans[0].start = spans[0].end - maxBytes
		}
	}

	entries = make([]LineEntry, len(spans))
	for i, sp := range spans {
		p := string(tail[sp.start:sp.end])
		text := strings.ToValidUTF8(strings.TrimSuffix(p, "\r"), "\uFFFD")
		var ts int64
		if !tsIdx.Empty() {
			// Prefer the logical offset of the terminating newline (when it
			// came from the log itself), else the last byte of the line.
			offForTs := logicalOff(sp.end)
			if offForTs < 0 && sp.end > sp.start {
				offForTs = logicalOff(sp.end - 1)
			}
			if offForTs >= 0 {
				ts = tsIdx.At(offForTs)
			}
		}
		entries[i] = LineEntry{Text: text, ReceivedAt: ts}
	}
	return entries, cut, nil
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
