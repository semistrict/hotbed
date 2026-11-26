package settle

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"plain log passes line for line", "one\ntwo\n\nfour\n", []string{"one", "two", "", "four"}},
		{"no final newline", "one\ntwo", []string{"one", "two"}},
		{"empty", "", nil},
		{"carriage-return progress keeps its last state", "  10%\r  55%\r 100% done\n", []string{" 100% done"}},
		{"a shorter redraw over a longer one needs an erase", "downloading 9999 KB\r\x1b[Kdone\n", []string{"done"}},
		{"without the erase the tail of the old text stays, as on a terminal", "abcdef\rXY\n", []string{"XYcdef"}},
		{"colors go", "\x1b[1;31merror\x1b[0m: bad\n", []string{"error: bad"}},
		{"OSC titles and hyperlinks go", "\x1b]0;title\x07\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\\n", []string{"link"}},
		{"multi-line progress redrawn by cursor up", "a: 1%\nb: 1%\n\x1b[2Aa: 100%\n\x1b[Kb: 100%\n", []string{"a: 100%", "b: 100%"}},
		{"cursor up then erase below clears the redrawn rows", "x\nprogress 1\nprogress 2\n\x1b[2A\x1b[Jfinal\n", []string{"x", "final"}},
		{"backspace spinner", "working |\b/\b-\b\\\bdone\n", []string{"working done"}},
		{"tabs move to the next stop", "a\tb\n", []string{"a       b"}},
		{"private modes change nothing seen", "\x1b[?25lhidden cursor\x1b[?25h\n", []string{"hidden cursor"}},
		{"UTF-8 passes", "héllo → ✓\n", []string{"héllo → ✓"}},
		{"column move", "abc\x1b[1GX\n", []string{"Xbc"}},
		{"trailing spaces trim", "padded   \n", []string{"padded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Lines([]byte(tc.in)); !slices.Equal(got, tc.want) {
				t.Errorf("Lines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Writes split anywhere, even inside an escape sequence or a rune, settle
// the same as one write.
func TestSplitWrites(t *testing.T) {
	in := []byte("\x1b[1;32mok\x1b[0m héllo ✓\r\x1b[Kfinal ✓\n\x1b]0;t\x07next\n")
	want := Lines(in)
	for cut := 1; cut < len(in); cut++ {
		var got []string
		s := New(func(l string) { got = append(got, l) })
		s.Write(in[:cut])
		s.Write(in[cut:])
		s.Flush()
		if !slices.Equal(got, want) {
			t.Fatalf("split at %d: %q, want %q", cut, got, want)
		}
	}
}

// Rows past the window settle as they scroll out, in order, and the live
// window shows what a reader sees now.
func TestWindowSettles(t *testing.T) {
	var emitted []string
	s := New(func(l string) { emitted = append(emitted, l) })
	var b strings.Builder
	for i := range Window + 10 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	b.WriteString("progress 50%")
	s.Write([]byte(b.String()))
	if len(emitted) != 11 || emitted[0] != "line 0" || emitted[10] != "line 10" {
		t.Fatalf("emitted %d rows, first %q last %q; want the 11 that scrolled out", len(emitted), emitted[0], emitted[len(emitted)-1])
	}
	rows := s.Rows()
	if rows[len(rows)-1] != "progress 50%" || rows[0] != "line 11" {
		t.Fatalf("live rows %q … %q, want line 11 … progress 50%%", rows[0], rows[len(rows)-1])
	}
	// Moving up past the window cannot reach a settled row.
	s.Write([]byte("\x1b[999A\r\x1b[Kchanged\n"))
	s.Flush()
	if emitted[11] != "changed" {
		t.Fatalf("the window's top row after a long cursor up = %q, want it changed", emitted[11])
	}
}
