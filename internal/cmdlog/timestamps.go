package cmdlog

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// A log's received timestamps live in a sibling append-only index (<path>.ts)
// made of fixed 16-byte little-endian records: (endOffset int64, timestampMs int64).
// Every byte in [prevEndOffset, endOffset) was received at timestampMs (unix ms).
// Because offsets are logical byte offsets in <path>, hole-punching compaction
// (Compact) never invalidates timestamps before or after the dropped range.

const tsRecordSize = 16

// TimestampEntry records that bytes up to EndOffset (exclusive) were received at TimestampMs.
type TimestampEntry struct {
	EndOffset   int64
	TimestampMs int64
}

func tsPath(path string) string { return path + ".ts" }

func encodeTimestampEntry(buf *[tsRecordSize]byte, endOffset, timestampMs int64) {
	binary.LittleEndian.PutUint64(buf[0:8], uint64(endOffset))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(timestampMs))
}

func decodeTimestampEntry(b []byte) TimestampEntry {
	return TimestampEntry{
		EndOffset:   int64(binary.LittleEndian.Uint64(b[0:8])),
		TimestampMs: int64(binary.LittleEndian.Uint64(b[8:16])),
	}
}

// RecordTimestamp appends a single (endOffset, timestampMs) entry to <path>.ts.
func RecordTimestamp(path string, endOffset, timestampMs int64) error {
	if path == "" || endOffset <= 0 || timestampMs <= 0 {
		return nil
	}
	f, err := os.OpenFile(tsPath(path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	var rec [tsRecordSize]byte
	encodeTimestampEntry(&rec, endOffset, timestampMs)
	_, werr := f.Write(rec[:])
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// RecordIfGrown checks <path>'s current logical size and, if it grew past the
// last offset recorded in <path>.ts, appends (size, timestampMs). This is used
// for runtimes (such as systemd StandardOutput=append:) that write directly to
// <path> without going through TimestampWriter.
func RecordIfGrown(path string, timestampMs int64) (int64, error) {
	if path == "" || timestampMs <= 0 {
		return 0, nil
	}
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if size <= 0 {
		return 0, nil
	}
	last, err := lastRecordedOffset(path)
	if err != nil {
		return 0, err
	}
	if size <= last {
		return last, nil
	}
	if err := RecordTimestamp(path, size, timestampMs); err != nil {
		return last, err
	}
	return size, nil
}

func lastRecordedOffset(path string) (int64, error) {
	f, err := os.Open(tsPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	full := (st.Size() / tsRecordSize) * tsRecordSize
	if full < tsRecordSize {
		return 0, nil
	}
	var rec [tsRecordSize]byte
	if _, err := f.ReadAt(rec[:], full-tsRecordSize); err != nil {
		return 0, err
	}
	return decodeTimestampEntry(rec[:]).EndOffset, nil
}

// TimestampWriter wraps a log file and records the receive timestamp of each
// Write into <path>.ts. It is safe for concurrent Write calls (e.g. stdout and
// stderr sharing one writer). Close closes only the sidecar timestamp file, not
// the underlying log file.
type TimestampWriter struct {
	mu     sync.Mutex
	path   string
	dst    *os.File
	tsFile *os.File
	offset int64
	nowFn  func() int64
}

// NewTimestampWriter wraps dst (already opened for append/write at path) so
// every non-empty Write appends a receive timestamp record to <path>.ts.
func NewTimestampWriter(path string, dst *os.File) *TimestampWriter {
	var off int64
	if dst != nil {
		if st, err := dst.Stat(); err == nil {
			off = st.Size()
		}
	}
	return &TimestampWriter{
		path:   path,
		dst:    dst,
		offset: off,
		nowFn:  func() int64 { return time.Now().UnixMilli() },
	}
}

// Write writes p to the underlying log file and appends a timestamp entry for
// the written byte range.
func (w *TimestampWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.dst.Write(p)
	if n > 0 {
		w.offset += int64(n)
		w.recordLocked(w.offset, w.nowFn())
	}
	return n, err
}

func (w *TimestampWriter) recordLocked(endOffset, timestampMs int64) {
	if w.path == "" || timestampMs <= 0 {
		return
	}
	if w.tsFile == nil {
		f, err := os.OpenFile(tsPath(w.path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		w.tsFile = f
	}
	var rec [tsRecordSize]byte
	encodeTimestampEntry(&rec, endOffset, timestampMs)
	_, _ = w.tsFile.Write(rec[:])
}

// Close closes the sidecar timestamp file. The caller remains responsible for
// closing the wrapped log file.
func (w *TimestampWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tsFile == nil {
		return nil
	}
	err := w.tsFile.Close()
	w.tsFile = nil
	return err
}

// TimestampIndex maps logical byte offsets in a log to the unix-millisecond
// timestamp at which those bytes were received.
type TimestampIndex struct {
	entries []TimestampEntry
}

// ReadTimestamps loads <path>.ts. A missing sidecar returns an empty index and
// a nil error (for legacy logs created before per-line timestamps).
func ReadTimestamps(path string) (*TimestampIndex, error) {
	if path == "" {
		return &TimestampIndex{}, nil
	}
	b, err := os.ReadFile(tsPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return &TimestampIndex{}, nil
	}
	if err != nil {
		return nil, err
	}
	count := len(b) / tsRecordSize
	entries := make([]TimestampEntry, 0, count)
	for i := 0; i < count; i++ {
		e := decodeTimestampEntry(b[i*tsRecordSize : (i+1)*tsRecordSize])
		if len(entries) > 0 && e.EndOffset <= entries[len(entries)-1].EndOffset {
			// Skip non-advancing duplicates if two recorders raced.
			continue
		}
		entries = append(entries, e)
	}
	return &TimestampIndex{entries: entries}, nil
}

// Empty reports whether the index has no timestamp entries.
func (idx *TimestampIndex) Empty() bool { return idx == nil || len(idx.entries) == 0 }

// At returns the receive timestamp (unix ms) for logical byte offset off, or 0
// if no timestamp entry covers off.
func (idx *TimestampIndex) At(off int64) int64 {
	if idx == nil || len(idx.entries) == 0 || off < 0 {
		return 0
	}
	i := sort.Search(len(idx.entries), func(i int) bool {
		return idx.entries[i].EndOffset > off
	})
	if i >= len(idx.entries) {
		return 0
	}
	return idx.entries[i].TimestampMs
}

// SplitChunks splits data (starting at logical byte offset off) along every
// timestamp boundary in idx and invokes fn for each contiguous sub-slice with
// its receive timestamp (0 for bytes not covered by idx).
func (idx *TimestampIndex) SplitChunks(off int64, data []byte, fn func(chunkOff int64, chunk []byte, ts int64) error) error {
	if len(data) == 0 {
		return nil
	}
	if idx == nil || len(idx.entries) == 0 {
		return fn(off, data, 0)
	}
	i := sort.Search(len(idx.entries), func(i int) bool {
		return idx.entries[i].EndOffset > off
	})
	pos := off
	rem := data
	for len(rem) > 0 {
		if i >= len(idx.entries) {
			return fn(pos, rem, 0)
		}
		e := idx.entries[i]
		take := min(int64(len(rem)), e.EndOffset-pos)
		if take <= 0 {
			i++
			continue
		}
		if err := fn(pos, rem[:take], e.TimestampMs); err != nil {
			return err
		}
		pos += take
		rem = rem[take:]
		if pos >= e.EndOffset {
			i++
		}
	}
	return nil
}

var _ io.WriteCloser = (*TimestampWriter)(nil)
