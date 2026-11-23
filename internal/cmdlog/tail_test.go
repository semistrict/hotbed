package cmdlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLog(t *testing.T, s string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.log")
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTail(t *testing.T) {
	for _, tc := range []struct {
		name, log string
		n, max    int
		want      string
		cut       bool
	}{
		{"last lines", "a\nb\nc\nd\n", 2, 100, "c|d", false},
		{"fewer than n", "a\nb\n", 5, 100, "a|b", false},
		{"no trailing newline", "a\nb\nc", 2, 100, "b|c", false},
		{"crlf", "a\r\nb\r\n", 5, 100, "a|b", false},
		{"empty lines kept", "a\n\nb\n", 3, 100, "a||b", false},
		{"empty log", "", 5, 100, "", false},
		{"just a newline", "\n", 5, 100, "", false},
		// The last 3 lines take 12 bytes; 10 fit two of them.
		{"bytes cut lines", "aaa\nbbb\nccc\nddd\n", 3, 10, "ccc|ddd", true},
		// Exactly n lines in exactly maxBytes is not cut.
		{"exact fit", "x\naaa\nbbb\n", 2, 7, "aaa|bbb", false},
		{"invalid utf-8", "ok\n\xff\xfebin\n", 5, 100, "ok|\uFFFDbin", false},
		// Cut inside a rune, the line is still valid UTF-8.
		{"cut inside a rune", "\u00e9\u00e9\n", 1, 3, "\uFFFD\u00e9", true},
		{"one long line", strings.Repeat("y", 50) + "z\n", 3, 10, strings.Repeat("y", 9) + "z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, cut, err := Tail(writeLog(t, tc.log), tc.n, tc.max)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(lines, "|"); got != tc.want || cut != tc.cut {
				t.Errorf("Tail = %q cut=%t, want %q cut=%t", got, cut, tc.want, tc.cut)
			}
		})
	}
}

func TestTailOfALogNotYetWritten(t *testing.T) {
	lines, cut, err := Tail(filepath.Join(t.TempDir(), "none.log"), 10, 100)
	if err != nil || len(lines) != 0 || cut {
		t.Errorf("Tail = %q %t %v, want no lines", lines, cut, err)
	}
}

// The tail of a big log, past many read chunks, reads only its end.
func TestTailOfABigLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200_000 {
		fmt.Fprintf(f, "line %d of a log longer than any one read\n", i)
	}
	f.Close()
	lines, cut, err := Tail(path, 3000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if cut || len(lines) != 3000 || lines[0] != "line 197000 of a log longer than any one read" || lines[2999] != "line 199999 of a log longer than any one read" {
		t.Errorf("Tail = %d lines from %q to %q, cut=%t", len(lines), lines[0], lines[len(lines)-1], cut)
	}
}
