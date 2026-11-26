// Package outq answers questions about a command's output: the lines that
// match a pattern, with context; a range; its head or tail; what came after
// a line already read. It reads the output as its terminal shows it
// (settled: internal/settle) or as written (raw), numbers its lines from 1,
// and bounds every answer, saying what it left out and how to get it.
package outq

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/settle"
)

// Bounds on an answer.
const (
	DefaultMax = 200
	MaxMax     = 5000
	MaxContext = 50
	// MaxLineRunes is the longest line an answer shows whole.
	MaxLineRunes = 2000
)

// Query is what is asked of an output.
type Query struct {
	// Grep selects the lines it matches, with Context lines around each.
	Grep    *regexp.Regexp
	Context int
	// Head and Tail take the first or last N lines (or, with Grep, matches).
	Head, Tail int
	// From and To bound the lines by number (0 for no bound); After takes
	// only lines numbered past it; SinceOffset (>= 0) only the lines
	// written from that byte of the output on.
	From, To, After int64
	SinceOffset     int64
	// Raw reads lines as written, not settled.
	Raw bool
	// Max is the most lines an answer holds (0 for DefaultMax).
	Max int
}

// Answer is what a Query found.
type Answer struct {
	Lines   []*pb.OutputLine
	Total   int64 // lines in the output
	Matches int64 // lines in range that Grep matched
	Omitted string
}

// Source reads an output from the start, chunk by chunk, each with the byte
// offset it starts at, until fn returns an error or the output ends.
type Source func(fn func(off int64, chunk []byte) error) error

// Check is q's own consistency: what it cannot ask together.
func (q *Query) Check() error {
	if q.Head > 0 && q.Tail > 0 {
		return fmt.Errorf("--head and --tail ask for both ends: take one")
	}
	if q.Head < 0 || q.Tail < 0 || q.Context < 0 || q.Max < 0 || q.From < 0 || q.To < 0 || q.After < 0 {
		return fmt.Errorf("counts and line numbers may not be negative")
	}
	if q.To > 0 && q.From > q.To {
		return fmt.Errorf("--from %d is past --to %d", q.From, q.To)
	}
	if q.Context > MaxContext {
		return fmt.Errorf("--context %d: at most %d", q.Context, MaxContext)
	}
	return nil
}

func (q *Query) max() int {
	switch {
	case q.Max == 0:
		return DefaultMax
	case q.Max > MaxMax:
		return MaxMax
	}
	return q.Max
}

// lines reads src's lines, numbered from 1, calling fn with each and the
// first line written at or after q.SinceOffset: 0 while the reader has not
// passed that offset (or q has none). It is known before any line from it
// on is read.
func (q *Query) lines(src Source, fn func(n int64, text string, sinceLine int64)) (total int64, err error) {
	var n, sinceLine int64
	emit := func(text string) {
		n++
		fn(n, text, sinceLine)
	}
	if q.Raw {
		var partial []byte
		err = src(func(off int64, chunk []byte) error {
			if q.SinceOffset >= 0 && sinceLine == 0 && off+int64(len(chunk)) > q.SinceOffset {
				// The line the offset falls in, begun before it or not.
				before := max(0, q.SinceOffset-off)
				sinceLine = n + 1 + int64(bytes.Count(chunk[:before], []byte("\n")))
			}
			for {
				i := bytes.IndexByte(chunk, '\n')
				if i < 0 {
					partial = append(partial, chunk...)
					return nil
				}
				partial = append(partial, chunk[:i]...)
				emit(toText(partial))
				partial = partial[:0]
				chunk = chunk[i+1:]
			}
		})
		if len(partial) > 0 {
			emit(toText(partial))
		}
		return n, err
	}
	sc := settle.New(emit)
	err = src(func(off int64, chunk []byte) error {
		if q.SinceOffset >= 0 && sinceLine == 0 && off+int64(len(chunk)) > q.SinceOffset {
			before := max(0, q.SinceOffset-off)
			sc.Write(chunk[:before])
			// The row the cursor is on as the offset passes, and every
			// row after it.
			sinceLine = n + int64(sc.Row()) + 1
			chunk = chunk[before:]
		}
		sc.Write(chunk)
		return nil
	})
	sc.Flush()
	return n, err
}

func toText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// clip cuts a line longer than MaxLineRunes, saying how much it cut.
func clip(s string) string {
	if len(s) <= MaxLineRunes || utf8.RuneCountInString(s) <= MaxLineRunes {
		return s
	}
	r := []rune(s)
	return string(r[:MaxLineRunes]) + fmt.Sprintf(" …[+%d characters]", len(r)-MaxLineRunes)
}

// inRange says whether line n is in the range q asks for, given the first
// line since its offset.
func (q *Query) inRange(n, sinceLine int64) bool {
	if q.From > 0 && n < q.From || q.To > 0 && n > q.To || n <= q.After {
		return false
	}
	if q.SinceOffset >= 0 && (sinceLine == 0 || n < sinceLine) {
		return false
	}
	return true
}

// Run answers q of src.
func Run(src Source, q Query) (Answer, error) {
	if err := q.Check(); err != nil {
		return Answer{}, err
	}
	if q.Grep != nil {
		return q.grep(src)
	}
	return q.plain(src)
}

// plain answers a query with no pattern: its range's head, its tail, or by
// default the last lines that fit.
func (q *Query) plain(src Source) (Answer, error) {
	max := q.max()
	want, fromEnd := q.Head, false
	switch {
	case q.Head > 0:
	case q.Tail > 0:
		want, fromEnd = q.Tail, true
	case q.From > 0 || q.To > 0 || q.After > 0 || q.SinceOffset >= 0:
		want = max // a range reads from its start
	default:
		want, fromEnd = max, true
	}
	take := min(want, max)
	var picked []*pb.OutputLine
	var inRange int64
	total, err := q.lines(src, func(n int64, text string, sinceLine int64) {
		if !q.inRange(n, sinceLine) {
			return
		}
		inRange++
		l := &pb.OutputLine{Number: n, Text: clip(text)}
		if fromEnd {
			picked = append(picked, l)
			if len(picked) > take {
				picked = picked[1:]
			}
		} else if len(picked) < take {
			picked = append(picked, l)
		}
	})
	if err != nil {
		return Answer{}, err
	}
	a := Answer{Lines: picked, Total: total}
	if left := inRange - int64(len(picked)); left > 0 {
		// The answer's bound, not what was asked, left lines out.
		capped := take == max && want >= max
		a.Omitted = plainOmitted(q, picked, inRange, left, capped, max)
	}
	return a, nil
}

func plainOmitted(q *Query, picked []*pb.OutputLine, inRange, left int64, capped bool, max int) string {
	var b strings.Builder
	if len(picked) == 0 {
		fmt.Fprintf(&b, "%d lines in range not shown", left)
	} else {
		first, last := picked[0].Number, picked[len(picked)-1].Number
		fmt.Fprintf(&b, "showing lines %d–%d of the %d in range; %d not shown", first, last, inRange, left)
	}
	if capped {
		fmt.Fprintf(&b, " (an answer holds at most %d lines: --max raises it up to %d)", max, MaxMax)
	}
	b.WriteString(": page with --from/--to or --after, or find what matters with --grep")
	return b.String()
}

// grep answers a query with a pattern: the matches in range, with context;
// --head or --tail takes the first or last N, and by default the first
// that fit.
func (q *Query) grep(src Source) (Answer, error) {
	max := q.max()
	want, fromEnd := 0, false
	switch {
	case q.Head > 0:
		want = q.Head
	case q.Tail > 0:
		want, fromEnd = q.Tail, true
	}
	// First pass: which matches to show, and how many there are.
	var chosen []int64
	var matches int64
	total, err := q.lines(src, func(n int64, text string, sinceLine int64) {
		if !q.inRange(n, sinceLine) || !q.Grep.MatchString(text) {
			return
		}
		matches++
		switch {
		case fromEnd:
			chosen = append(chosen, n)
			if len(chosen) > want {
				chosen = chosen[1:]
			}
		case want > 0:
			if len(chosen) < want {
				chosen = append(chosen, n)
			}
		default:
			// The first matches whose lines, with context, fit.
			if len(chosen)*(2*q.Context+1) < max {
				chosen = append(chosen, n)
			}
		}
	})
	if err != nil {
		return Answer{}, err
	}
	a := Answer{Total: total, Matches: matches}
	if len(chosen) == 0 {
		return a, nil
	}
	// Second pass: the chosen matches' lines, with context, in order.
	isMatch := make(map[int64]bool, len(chosen))
	for _, m := range chosen {
		isMatch[m] = true
	}
	ci := 0 // the first chosen match not yet past
	cut := false
	_, err = q.lines(src, func(n int64, text string, sinceLine int64) {
		for ci < len(chosen) && chosen[ci]+int64(q.Context) < n {
			ci++
		}
		if ci == len(chosen) || n < chosen[ci]-int64(q.Context) {
			return
		}
		if !q.inRange(n, sinceLine) && !isMatch[n] {
			// Context stops at the range's edge.
			return
		}
		if len(a.Lines) == max {
			cut = true
			return
		}
		a.Lines = append(a.Lines, &pb.OutputLine{Number: n, Text: clip(text), Match: isMatch[n]})
	})
	if err != nil {
		return Answer{}, err
	}
	shown := int64(0)
	for _, l := range a.Lines {
		if l.Match {
			shown++
		}
	}
	if left := matches - shown; left > 0 || cut {
		var b strings.Builder
		fmt.Fprintf(&b, "showing %d of %d matching lines", shown, matches)
		if cut {
			fmt.Fprintf(&b, " (an answer holds at most %d lines: --max raises it up to %d)", max, MaxMax)
		}
		switch {
		case fromEnd:
			b.WriteString("; earlier ones: --head, or --to before the first shown")
		default:
			b.WriteString("; later ones: --tail, or --after the last shown")
		}
		a.Omitted = b.String()
	}
	return a, nil
}

// Watcher reads an output as it grows, to tell when a line after a given
// one matches a pattern: what waiting --until a line watches.
type Watcher struct {
	re      *regexp.Regexp
	after   int64
	raw     bool
	n       int64 // lines read whole (settled, or ended by a newline)
	sc      *settle.Screen
	partial []byte
	match   int64
}

// NewWatcher watches for a line past after that re matches.
func NewWatcher(re *regexp.Regexp, after int64, raw bool) *Watcher {
	w := &Watcher{re: re, after: after, raw: raw}
	w.sc = settle.New(func(text string) {
		w.n++
		w.see(w.n, text)
	})
	return w
}

func (w *Watcher) see(n int64, text string) {
	if w.match == 0 && n > w.after && w.re.MatchString(text) {
		w.match = n
	}
}

// Write feeds what the output gained.
func (w *Watcher) Write(chunk []byte) {
	if !w.raw {
		w.sc.Write(chunk)
		return
	}
	for {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			w.partial = append(w.partial, chunk...)
			return
		}
		w.partial = append(w.partial, chunk[:i]...)
		w.n++
		w.see(w.n, toText(w.partial))
		w.partial = w.partial[:0]
		chunk = chunk[i+1:]
	}
}

// Match is the first line past after that matched, counting the lines
// still being drawn (or, raw, the line not yet ended) as they stand now;
// 0 for none yet.
func (w *Watcher) Match() int64 {
	if w.match > 0 {
		return w.match
	}
	if w.raw {
		if len(w.partial) > 0 && w.n+1 > w.after && w.re.Match(w.partial) {
			return w.n + 1
		}
		return 0
	}
	for i, text := range w.sc.Rows() {
		if n := w.n + int64(i) + 1; n > w.after && w.re.MatchString(text) {
			return n
		}
	}
	return 0
}
