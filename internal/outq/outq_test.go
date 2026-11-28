package outq

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/settle"
)

// chunks is a Source reading out in pieces of n bytes, so lines and escape
// sequences split across reads.
func chunks(out string, n int) Source {
	return func(fn func(off int64, chunk []byte) error) error {
		for off := 0; off < len(out); off += n {
			end := min(off+n, len(out))
			if err := fn(int64(off), []byte(out[off:end])); err != nil {
				return err
			}
		}
		return nil
	}
}

// numbered is "line 1\n" … "line n\n".
func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// show is an answer as "number[:]text" lines, ":" marking matches, and its
// omitted note.
func show(a Answer) string {
	var b strings.Builder
	for _, l := range a.Lines {
		sep := " "
		if l.Match {
			sep = ":"
		}
		fmt.Fprintf(&b, "%d%s%s\n", l.Number, sep, l.Text)
	}
	if a.Omitted != "" {
		fmt.Fprintf(&b, "[%s]\n", a.Omitted)
	}
	return b.String()
}

func run(t *testing.T, out string, q Query) Answer {
	t.Helper()
	if q.SinceOffset == 0 {
		q.SinceOffset = -1
	}
	a, err := Run(chunks(out, 7), q)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDefaultIsTheTailThatFits(t *testing.T) {
	a := run(t, numbered(10), Query{Max: 3})
	want := "8 line 8\n9 line 9\n10 line 10\n[showing lines 8–10 of the 10 in range; 7 not shown (an answer holds at most 3 lines: --max raises it up to 5000): page with --from/--to or --after, or find what matters with --grep]\n"
	if got := show(a); got != want || a.Total != 10 {
		t.Errorf("answer =\n%s(total %d), want\n%s(total 10)", got, a.Total, want)
	}
}

func TestHeadTailRanges(t *testing.T) {
	out := numbered(10)
	for _, tc := range []struct {
		name string
		q    Query
		want string
	}{
		{"head", Query{Head: 2}, "1 line 1\n2 line 2\n[showing lines 1–2 of the 10 in range; 8 not shown: page with --from/--to or --after, or find what matters with --grep]\n"},
		{"tail", Query{Tail: 2}, "9 line 9\n10 line 10\n[showing lines 9–10 of the 10 in range; 8 not shown: page with --from/--to or --after, or find what matters with --grep]\n"},
		{"from to reads the range from its start", Query{From: 4, To: 5}, "4 line 4\n5 line 5\n"},
		{"after takes what came since", Query{After: 8}, "9 line 9\n10 line 10\n"},
		{"after past the end is nothing", Query{After: 10}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := show(run(t, out, tc.q)); got != tc.want {
				t.Errorf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestGrepWithContext(t *testing.T) {
	out := "compile a\ncompile b\nerror: one\nnote x\ncompile c\ncompile d\ncompile e\nerror: two\ndone\n"
	a := run(t, out, Query{Grep: regexp.MustCompile(`^error`), Context: 1})
	want := "2 compile b\n3:error: one\n4 note x\n7 compile e\n8:error: two\n9 done\n"
	if got := show(a); got != want || a.Matches != 2 {
		t.Errorf("got\n%s(matches %d) want\n%s(matches 2)", got, a.Matches, want)
	}
	// Overlapping context shows each line once.
	a = run(t, "a\nerr 1\nerr 2\nb\n", Query{Grep: regexp.MustCompile(`err`), Context: 2})
	if got, want := show(a), "1 a\n2:err 1\n3:err 2\n4 b\n"; got != want {
		t.Errorf("overlapping context: got\n%s want\n%s", got, want)
	}
}

func TestGrepHeadTailAndBounds(t *testing.T) {
	out := numbered(100)
	re := regexp.MustCompile(`line \d*7$`)
	a := run(t, out, Query{Grep: re, Tail: 2})
	if got, want := show(a), "87:line 87\n97:line 97\n[showing 2 of 10 matching lines; earlier ones: --head, or --to before the first shown]\n"; got != want {
		t.Errorf("tail of matches: got\n%s want\n%s", got, want)
	}
	a = run(t, out, Query{Grep: re, Max: 3})
	if got, want := show(a), "7:line 7\n17:line 17\n27:line 27\n[showing 3 of 10 matching lines; later ones: --tail, or --after the last shown]\n"; got != want {
		t.Errorf("matches past max: got\n%s want\n%s", got, want)
	}
	a = run(t, out, Query{Grep: regexp.MustCompile(`nothing`)})
	if a.Matches != 0 || len(a.Lines) != 0 || a.Omitted != "" || a.Total != 100 {
		t.Errorf("no match = %+v, want none, nothing omitted, total 100", a)
	}
}

// What a progress bar redraws reads as its last state, settled, and as
// written, raw.
func TestSettledAndRaw(t *testing.T) {
	out := "start\n 10%\r 50%\r100%\n\x1b[31mfail\x1b[0m\n"
	if got, want := show(run(t, out, Query{})), "1 start\n2 100%\n3 fail\n"; got != want {
		t.Errorf("settled: got\n%q want\n%q", got, want)
	}
	if got, want := show(run(t, out, Query{Raw: true})), "1 start\n2  10%\r 50%\r100%\n3 \x1b[31mfail\x1b[0m\n"; got != want {
		t.Errorf("raw: got\n%q want\n%q", got, want)
	}
}

func TestSinceOffset(t *testing.T) {
	out := "boot\nlistening\nGET /a\nGET /b\n"
	off := int64(strings.Index(out, "GET /a"))
	for _, raw := range []bool{false, true} {
		a := run(t, out, Query{SinceOffset: off, Raw: raw})
		if got, want := show(a), "3 GET /a\n4 GET /b\n"; got != want {
			t.Errorf("raw=%v: got\n%s want\n%s", raw, got, want)
		}
		// An offset the output has not reached yet: nothing since.
		if a := run(t, out, Query{SinceOffset: int64(len(out)) + 10, Raw: raw}); len(a.Lines) != 0 {
			t.Errorf("raw=%v: offset past the end shows %v", raw, a.Lines)
		}
	}
}

func TestLongLinesAreClipped(t *testing.T) {
	long := strings.Repeat("x", MaxLineRunes+5)
	a := run(t, long+"\n", Query{})
	if got := a.Lines[0].Text; got != strings.Repeat("x", MaxLineRunes)+" …[+5 characters]" {
		t.Errorf("clipped line ends %q", got[len(got)-30:])
	}
}

func TestCheck(t *testing.T) {
	for _, q := range []Query{{Head: 1, Tail: 1}, {From: 5, To: 2}, {Context: MaxContext + 1}, {Head: -1}} {
		if _, err := Run(chunks("", 1), q); err == nil {
			t.Errorf("Run(%+v) took it, want refused", q)
		}
	}
}

func TestWatcher(t *testing.T) {
	re := regexp.MustCompile(`listening on`)
	for _, raw := range []bool{false, true} {
		w := NewWatcher(re, 1, raw)
		w.Write([]byte("listening on :1 (old)\nbuilding\n"))
		if m := w.Match(); m != 0 {
			t.Errorf("raw=%v: matched line %d, at or before --after 1", raw, m)
		}
		// A line still being written counts as it stands.
		w.Write([]byte("listening on :3000"))
		if m := w.Match(); m != 3 {
			t.Errorf("raw=%v: match = %d, want line 3 as it is drawn", raw, m)
		}
		w.Write([]byte("\nnext\n"))
		if m := w.Match(); m != 3 {
			t.Errorf("raw=%v: match = %d after more lines, want still 3", raw, m)
		}
	}
}

// A row still being drawn is not final: an --after past it would miss how
// it ends.
func TestFinal(t *testing.T) {
	for _, tc := range []struct {
		out   string
		raw   bool
		total int64
		final int64
	}{
		{"a\nb\n", false, 2, 2},
		{"a\nbuilding |", false, 2, 1},
		{"a\nbuilding |\r", false, 2, 1},
		{"a\nb\n", true, 2, 2},
		{"a\nbuilding", true, 2, 1},
	} {
		a := run(t, tc.out, Query{Raw: tc.raw})
		if a.Total != tc.total || a.Final != tc.final {
			t.Errorf("%q raw=%v: total %d final %d, want %d %d", tc.out, tc.raw, a.Total, a.Final, tc.total, tc.final)
		}
	}
}

func TestLineAt(t *testing.T) {
	out := "one\ntwo\nthree\n"
	for _, raw := range []bool{false, true} {
		if line, total, err := LineAt(chunks(out, 3), int64(strings.Index(out, "two")), raw); err != nil || line != 2 || total != 3 {
			t.Errorf("raw=%v: line %d total %d err %v, want 2 3", raw, line, total, err)
		}
		if line, total, _ := LineAt(chunks(out, 3), int64(len(out)), raw); line != 0 || total != 3 {
			t.Errorf("raw=%v at the end: line %d total %d, want 0 (nothing since) 3", raw, line, total)
		}
	}
}

func TestRunTimedPopulatesReceivedAt(t *testing.T) {
	type timedChunk struct {
		data string
		ts   int64
	}
	chunks := []timedChunk{
		{"line one\n", 1_700_000_001_000},
		{"progress 10%", 1_700_000_002_000},
		{"\rprogress 100%\n", 1_700_000_005_000},
		{"error: boom\n", 1_700_000_009_000},
	}
	src := func(fn func(off int64, chunk []byte, ts int64) error) error {
		var off int64
		for _, c := range chunks {
			if err := fn(off, []byte(c.data), c.ts); err != nil {
				return err
			}
			off += int64(len(c.data))
		}
		return nil
	}

	// Settled mode collapses progress bar and keeps final update timestamp.
	ans, err := RunTimed(src, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Lines) != 3 {
		t.Fatalf("settled lines = %+v, want 3", ans.Lines)
	}
	wantTs := []int64{1_700_000_001_000, 1_700_000_005_000, 1_700_000_009_000}
	wantText := []string{"line one", "progress 100%", "error: boom"}
	for i := range wantTs {
		if ans.Lines[i].Text != wantText[i] || ans.Lines[i].ReceivedAt != wantTs[i] {
			t.Errorf("settled line[%d] = (%q, %d), want (%q, %d)", i, ans.Lines[i].Text, ans.Lines[i].ReceivedAt, wantText[i], wantTs[i])
		}
	}

	// Raw mode preserves received_at per raw line.
	rawAns, err := RunTimed(src, Query{Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rawAns.Lines) != 3 || rawAns.Lines[1].ReceivedAt != 1_700_000_005_000 {
		t.Fatalf("raw lines = %+v", rawAns.Lines)
	}

	// Grep mode preserves received_at on matched and context lines.
	grepAns, err := RunTimed(src, Query{Grep: regexp.MustCompile("boom"), Context: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(grepAns.Lines) != 2 || grepAns.Lines[0].ReceivedAt != 1_700_000_005_000 || grepAns.Lines[1].ReceivedAt != 1_700_000_009_000 {
		t.Fatalf("grep lines = %+v", grepAns.Lines)
	}
}

func TestTTYColsClipMatchAndRawGrepPast256KiB(t *testing.T) {
	// 1. Query{Cols: settle.ScreenCols} wraps nextest-style space-padded rows.
	pad := func(s string) string {
		return s + strings.Repeat(" ", settle.ScreenCols-len(s))
	}
	ttyLog := pad("Running 0/1") + "\x1b[1A\r\x1b[2KFAIL crate::it\r\nSummary: 1 failed\r\n"
	a := run(t, ttyLog, Query{Cols: settle.ScreenCols, Grep: regexp.MustCompile("FAIL")})
	if a.Matches != 1 || len(a.Lines) != 1 || a.Lines[0].Text != "FAIL crate::it" {
		t.Fatalf("TTY Cols grep = %+v", a)
	}

	// 2. clipMatch centers the window around a match past MaxLineRunes.
	longLine := strings.Repeat("a", MaxLineRunes+500) + "NEEDLE_HERE" + strings.Repeat("b", 500) + "\n"
	a = run(t, longLine, Query{Grep: regexp.MustCompile("NEEDLE_HERE")})
	if a.Matches != 1 || len(a.Lines) != 1 || !strings.Contains(a.Lines[0].Text, "NEEDLE_HERE") {
		t.Fatalf("clipMatch lost match past MaxLineRunes: %+v", a.Lines)
	}

	// 3. Raw --grep scans past maxRawLine (256 KiB) to find matches deep in a single raw line.
	hugeRaw := strings.Repeat("z", maxRawLine+40_000) + "DEEP_FAIL_TOKEN" + strings.Repeat("q", 100) + "\n"
	a = run(t, hugeRaw, Query{Raw: true, Grep: regexp.MustCompile("DEEP_FAIL_TOKEN")})
	if a.Matches != 1 || len(a.Lines) != 1 || !strings.Contains(a.Lines[0].Text, "DEEP_FAIL_TOKEN") {
		t.Fatalf("raw grep missed match past 256 KiB: matches=%d lines=%+v", a.Matches, a.Lines)
	}
}
