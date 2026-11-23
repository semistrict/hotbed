package cmdlog

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLinesCutsLongLinesAndTrimsCR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	long := strings.Repeat("y", MaxLine+10)
	if err := os.WriteFile(path, []byte("a\r\n"+long+"\nb\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := NewLines(path)
	var got []string
	add := func(line []byte) { got = append(got, string(line)) }
	if err := l.Read(add, 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", long[:MaxLine], "b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines %q..., want %q...", short(got), short(want))
	}
	l.Flush(add)
	if last := got[len(got)-1]; last != "c" {
		t.Errorf("flushed %q, want c", last)
	}
	if off := l.Offset(); off != int64(len("a\r\n"+long+"\nb\nc")) {
		t.Errorf("offset %d", off)
	}
}

func TestLinesOfALogNotYetWritten(t *testing.T) {
	l := NewLines(filepath.Join(t.TempDir(), "none.log"))
	if err := l.Read(func([]byte) { t.Error("a line from no log") }, 0); err != nil {
		t.Fatal(err)
	}
}

// Reading a big log allocates a buffer and a line, not the log, however
// long it is.
func TestLinesReadsABigLogInBoundedMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	block := []byte(strings.Repeat("   Compiling crate v1.0.0\n", (1<<20)/26))
	const blocks = 256 // ~256 MiB
	for range blocks {
		if _, err := f.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	l := NewLines(path)
	n := 0
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := l.Read(func([]byte) { n++ }, 0); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if want := blocks * (len(block) / 26); n != want {
		t.Errorf("read %d lines, want %d", n, want)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("reading a %d MiB log allocated %d KiB", blocks, grew>>10)
	}
}

func short(lines []string) []string {
	out := make([]string, len(lines))
	for i, s := range lines {
		if len(s) > 10 {
			s = s[:10] + "…(" + string(rune('0'+len(s)%10)) + ")"
		}
		out[i] = s
	}
	return out
}
