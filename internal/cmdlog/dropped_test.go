package cmdlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// numbered writes lines "line N ..." of 64 bytes each, mib MiB of them, and
// returns the log's path.
func numbered(t *testing.T, mib int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b strings.Builder
	for i := range mib << 20 / 64 {
		fmt.Fprintf(&b, "line %08d %s\n", i, strings.Repeat(".", 64-14-1))
	}
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
	return path
}

func numberedLine(i int) string { return fmt.Sprintf("line %08d %s", i, strings.Repeat(".", 49)) }

// onDisk is how many non-hole data bytes the file at path holds (via
// SEEK_DATA/SEEK_HOLE, which reflects hole punching immediately on ZFS too).
func onDisk(t *testing.T, path string) int64 {
	t.Helper()
	n, err := DataBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var limit = Limit{Head: 1 << 20, Tail: 2 << 20}

const perMiB = 1 << 20 / 64

// A log past its limit keeps its head and its tail, frees the rest on
// disk, and keeps its size, so offsets stay.
func TestCompactKeepsHeadAndTail(t *testing.T) {
	path := numbered(t, 10)
	before := onDisk(t, path)
	d, err := Compact(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Dropped{From: 1 << 20, To: 8 << 20}); d != want {
		t.Fatalf("dropped %+v, want %+v", d, want)
	}
	if st, _ := os.Stat(path); st.Size() != 10<<20 {
		t.Errorf("size %d after compacting, want it kept", st.Size())
	}
	if after := onDisk(t, path); after > before-6<<20 {
		t.Errorf("%d bytes on disk after dropping 7 MiB of %d", after, before)
	}
	if got, err := ReadDropped(path); err != nil || got != d {
		t.Errorf("recorded %+v %v, want %+v", got, err, d)
	}
	// Under the limit again by less than slack, it is left alone.
	if again, err := Compact(path, limit); err != nil || again != d {
		t.Errorf("compacting again: %+v %v, want %+v", again, err, d)
	}
}

func TestCompactRefusesNoTail(t *testing.T) {
	if _, err := Compact(numbered(t, 1), Limit{Head: 1 << 20}); err == nil {
		t.Error("a limit that keeps no tail compacted")
	}
}

// Lines reads the head, the marker, then the tail.
func TestLinesStepOverDroppedBytes(t *testing.T) {
	path := numbered(t, 10)
	d, err := Compact(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	l := NewLines(path)
	if err := l.Read(func(line []byte) { got = append(got, string(line)) }, 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != perMiB+1+2*perMiB {
		t.Fatalf("%d lines, want %d", len(got), perMiB+1+2*perMiB)
	}
	if got[perMiB-1] != numberedLine(perMiB-1) {
		t.Errorf("last head line %q", got[perMiB-1])
	}
	if got[perMiB] != Marker(d) || Marker(d) != "[hotbed: 7.0 MiB of output dropped here: the log keeps its first and last bytes]" {
		t.Errorf("line after the head %q, want the marker %q", got[perMiB], Marker(d))
	}
	if got[perMiB+1] != numberedLine(8*perMiB) {
		t.Errorf("first tail line %q", got[perMiB+1])
	}
}

// Read stops after max bytes and goes on from there.
func TestLinesReadsAtMostMax(t *testing.T) {
	path := numbered(t, 1)
	l := NewLines(path)
	n := 0
	if err := l.Read(func([]byte) { n++ }, 640); err != nil {
		t.Fatal(err)
	}
	if n != 10 || l.Offset() != 640 {
		t.Errorf("read %d lines to %d, want 10 to 640", n, l.Offset())
	}
	if err := l.Read(func([]byte) { n++ }, 0); err != nil {
		t.Fatal(err)
	}
	if n != perMiB {
		t.Errorf("read %d lines in all, want %d", n, perMiB)
	}
}

// Tail reaching back past dropped bytes shows the marker in their place.
func TestTailAcrossDroppedBytes(t *testing.T) {
	path := numbered(t, 10)
	d, err := Compact(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	lines, cut, err := Tail(path, 2*perMiB+2, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{numberedLine(perMiB - 1), Marker(d), numberedLine(8 * perMiB)}
	if cut || len(lines) != 2*perMiB+2 || strings.Join(lines[:3], "|") != strings.Join(want, "|") {
		t.Errorf("Tail = %d lines starting %q, cut=%t; want %d starting %q", len(lines), lines[:3], cut, 2*perMiB+2, want)
	}
}

// Following from the start reads the head, the marker and the tail, and
// resuming from any offset handed out goes on from there.
func TestReadChunkFollowsAcrossDroppedBytes(t *testing.T) {
	path := numbered(t, 10)
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Compact(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out strings.Builder
	buf := make([]byte, 300<<10)
	off := int64(0)
	for {
		data, next, err := ReadChunk(f, path, off, buf)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			break
		}
		out.Write(data)
		off = next
	}
	want := string(whole[:d.From]) + Marker(d) + "\n" + string(whole[d.To:])
	if out.String() != want {
		t.Errorf("followed %d bytes, want %d: head, marker, tail", out.Len(), len(want))
	}
	if off != 10<<20 {
		t.Errorf("last offset %d, want the log's end", off)
	}
	// From an offset inside the dropped bytes, a reader that fell behind
	// gets a marker for what it missed, then the tail.
	data, next, err := ReadChunk(f, path, 3<<20, buf)
	if want := "\n" + Marker(Dropped{From: 3 << 20, To: d.To}) + "\n"; err != nil || string(data) != want || next != d.To {
		t.Errorf("from inside the dropped bytes: %q, %d, %v", data, next, err)
	}
}

// Dropped bytes that start and end inside lines leave the pieces of those
// lines on lines of their own, around the marker, for every reader.
func TestDroppedBytesInsideLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	line := strings.Repeat("x", 99) + "\n" // 100 bytes: 1 MiB falls inside one
	if err := os.WriteFile(path, []byte(strings.Repeat(line, 100_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Compact(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	headPiece := strings.Repeat("x", int(d.From%100))
	tailPiece := strings.Repeat("x", 99-int(d.To%100))
	want := []string{headPiece, Marker(d), tailPiece, line[:99]}

	var lines []string
	l := NewLines(path)
	if err := l.Read(func(b []byte) { lines = append(lines, string(b)) }, 0); err != nil {
		t.Fatal(err)
	}
	i := int(d.From / 100)
	if got := lines[i : i+4]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Lines around the marker = %q, want %q", got, want)
	}

	tail, _, err := Tail(path, len(lines)-i, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := tail[:4]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Tail around the marker = %q, want %q", got, want)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, next, err := ReadChunk(f, path, d.From, make([]byte, 4096))
	if err != nil || string(data) != "\n"+Marker(d)+"\n" || next != d.To {
		t.Errorf("ReadChunk at the dropped bytes = %q %d %v, want the marker on its own line", data, next, err)
	}
}

// A punch that fails leaves the bytes readable: the record goes back to
// what it was, so readers do not skip bytes still there.
func TestCompactThatCannotFreeKeepsTheBytes(t *testing.T) {
	path := numbered(t, 10)
	failing := func(*os.File, int64, int64) error { return errors.ErrUnsupported }
	defer func(p func(*os.File, int64, int64) error) { punch = p }(punch)
	punch = failing
	if _, err := Compact(path, limit); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Compact with no hole punching: %v, want ErrUnsupported", err)
	}
	if d, err := ReadDropped(path); err != nil || d != (Dropped{}) {
		t.Errorf("recorded %+v %v after a first punch failed, want nothing", d, err)
	}
	// Once compacted, a later failure restores the earlier record.
	punch = punchHole
	first, err := Compact(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(make([]byte, 3<<20))
	f.Close()
	punch = failing
	if _, err := Compact(path, limit); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("second Compact: %v", err)
	}
	if d, _ := ReadDropped(path); d != first {
		t.Errorf("recorded %+v after a later punch failed, want %+v", d, first)
	}
}

func TestCheckPunch(t *testing.T) {
	if err := CheckPunch(t.TempDir()); err != nil {
		t.Errorf("CheckPunch on this test's filesystem: %v", err)
	}
	defer func(p func(*os.File, int64, int64) error) { punch = p }(punch)
	punch = func(*os.File, int64, int64) error { return errors.ErrUnsupported }
	if err := CheckPunch(t.TempDir()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("CheckPunch where punching fails: %v", err)
	}
}
