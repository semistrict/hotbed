// Package settle turns what a program wrote to a terminal into the lines a
// reader would see: carriage-return redraws (progress bars, spinners) and
// cursor-up redraws (multi-line progress) collapse to their final state,
// erases erase, and colors and other escape sequences go. A plain log, one
// that never redraws, passes through line for line.
//
// It is a small terminal emulator over a live window of the newest rows: a
// row that scrolls out of the window can no longer be redrawn, so it is
// settled and emitted. Lines have no width limit: nothing wraps.
package settle

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Window is how many of the newest rows a program can still redraw by
// moving the cursor up: more than any multi-line progress display.
const Window = 64

// Screen is a terminal emulator that emits each row once it settles.
type Screen struct {
	rows [][]rune
	row  int // the cursor's row, an index into rows
	col  int
	emit func(string)

	// An escape sequence in progress across writes.
	state   escState
	params  []byte
	partial []byte // the start of a UTF-8 rune split across writes
}

type escState int

const (
	ground escState = iota
	escape          // after ESC
	csi             // after ESC [
	osc             // after ESC ]: to BEL or ESC \
	oscEsc          // ESC inside an OSC, perhaps its ST
	charset         // after ESC ( and the like: one more byte
)

// New is a screen that calls emit with each settled line, in order.
func New(emit func(line string)) *Screen {
	return &Screen{rows: [][]rune{nil}, emit: emit}
}

// Write feeds what the program wrote.
func (s *Screen) Write(p []byte) {
	if len(s.partial) > 0 {
		p = append(s.partial, p...)
		s.partial = nil
	}
	for i := 0; i < len(p); {
		b := p[i]
		switch s.state {
		case escape:
			s.escapeByte(b)
			i++
			continue
		case csi:
			s.csiByte(b)
			i++
			continue
		case osc:
			switch b {
			case 0x07:
				s.state = ground
			case 0x1b:
				s.state = oscEsc
			}
			i++
			continue
		case oscEsc:
			// ESC \ ends it; any other ESC starts a new sequence.
			if b == '\\' {
				s.state = ground
			} else {
				s.state = escape
				s.escapeByte(b)
			}
			i++
			continue
		case charset:
			s.state = ground
			i++
			continue
		}
		switch {
		case b == 0x1b:
			s.state = escape
			i++
		case b == '\n':
			// A newline starts the next line at its start, as a terminal
			// turns it into \r\n (onlcr) and as a log written to a file means.
			s.lineFeed()
			s.col = 0
			i++
		case b == '\r':
			s.col = 0
			i++
		case b == '\b':
			if s.col > 0 {
				s.col--
			}
			i++
		case b == '\t':
			s.col = (s.col/8 + 1) * 8
			i++
		case b < 0x20 || b == 0x7f:
			i++ // other controls (BEL, SI, SO, NUL) print nothing
		case b < utf8.RuneSelf:
			s.put(rune(b))
			i++
		default:
			if !utf8.FullRune(p[i:]) {
				s.partial = append([]byte(nil), p[i:]...)
				return
			}
			r, n := utf8.DecodeRune(p[i:])
			s.put(r)
			i += n
		}
	}
}

func (s *Screen) escapeByte(b byte) {
	switch b {
	case '[':
		s.state, s.params = csi, s.params[:0]
	case ']':
		s.state = osc
	case '(', ')', '*', '+', '#', '%':
		s.state = charset
	case 'M': // reverse index: up a row
		s.state = ground
		s.up(1)
	case 'E': // next line
		s.state = ground
		s.lineFeed()
		s.col = 0
	case 'D': // index: down a row
		s.state = ground
		s.lineFeed()
	default:
		s.state = ground // ESC 7, ESC 8, ESC =, and the rest change nothing seen here
	}
}

func (s *Screen) csiByte(b byte) {
	if b >= 0x20 && b <= 0x3f { // parameters and intermediates
		s.params = append(s.params, b)
		return
	}
	s.state = ground
	if len(s.params) > 0 && (s.params[0] == '?' || s.params[0] == '>' || s.params[0] == '=') {
		return // private modes: cursor visibility, alternate screens and the like
	}
	n := func(i, def int) int {
		ps := strings.Split(string(s.params), ";")
		if i >= len(ps) || ps[i] == "" {
			return def
		}
		v, err := strconv.Atoi(ps[i])
		if err != nil || v < 0 {
			return def
		}
		if v == 0 && def > 0 {
			return def
		}
		return v
	}
	switch b {
	case 'A': // cursor up
		s.up(n(0, 1))
	case 'B', 'e': // cursor down
		s.down(n(0, 1))
	case 'C', 'a': // cursor forward
		s.col += n(0, 1)
	case 'D': // cursor back
		s.col = max(0, s.col-n(0, 1))
	case 'E': // next line
		s.down(n(0, 1))
		s.col = 0
	case 'F': // previous line
		s.up(n(0, 1))
		s.col = 0
	case 'G', '`': // column
		s.col = n(0, 1) - 1
	case 'H', 'f': // position, within the live window
		s.row = min(n(0, 1)-1, Window-1)
		for s.row >= len(s.rows) {
			s.rows = append(s.rows, nil)
		}
		s.col = n(1, 1) - 1
	case 'K': // erase in line
		s.eraseLine(n(0, 0))
	case 'J': // erase in display
		s.eraseDisplay(n(0, 0))
	case 'P': // delete characters
		r := s.rows[s.row]
		if s.col < len(r) {
			k := min(n(0, 1), len(r)-s.col)
			s.rows[s.row] = append(r[:s.col], r[s.col+k:]...)
		}
	case 'X': // erase characters
		r := s.rows[s.row]
		for i := s.col; i < min(s.col+n(0, 1), len(r)); i++ {
			r[i] = ' '
		}
	}
	// m (colors), h and l (modes), r (scroll region), s and u (save and
	// restore), and the rest change nothing seen here.
}

func (s *Screen) put(r rune) {
	row := s.rows[s.row]
	for len(row) < s.col {
		row = append(row, ' ')
	}
	if s.col < len(row) {
		row[s.col] = r
	} else {
		row = append(row, r)
	}
	s.rows[s.row] = row
	s.col++
}

// lineFeed moves the cursor down a row, adding one at the bottom, where
// the oldest row scrolls out of the window and settles.
func (s *Screen) lineFeed() {
	s.row++
	if s.row == len(s.rows) {
		s.rows = append(s.rows, nil)
		if len(s.rows) > Window {
			s.emit(line(s.rows[0]))
			s.rows = s.rows[1:]
			s.row--
		}
	}
}

func (s *Screen) up(k int)   { s.row = max(0, s.row-k) }
func (s *Screen) down(k int) { s.row = min(len(s.rows)-1, s.row+k) }

func (s *Screen) eraseLine(mode int) {
	r := s.rows[s.row]
	switch mode {
	case 0:
		if s.col < len(r) {
			s.rows[s.row] = r[:s.col]
		}
	case 1:
		for i := 0; i <= s.col && i < len(r); i++ {
			r[i] = ' '
		}
	default:
		s.rows[s.row] = nil
	}
}

func (s *Screen) eraseDisplay(mode int) {
	switch mode {
	case 0:
		s.eraseLine(0)
		for i := s.row + 1; i < len(s.rows); i++ {
			s.rows[i] = nil
		}
	case 1:
		for i := 0; i < s.row; i++ {
			s.rows[i] = nil
		}
		s.eraseLine(1)
	default:
		// A cleared screen keeps what scrolled before it: drop the window's
		// rows, as a reader of the log would not see them again.
		for i := range s.rows {
			s.rows[i] = nil
		}
	}
}

// Row is the cursor's row in the live window, 0 for its oldest: its line in
// the output is the lines emitted so far plus Row()+1.
func (s *Screen) Row() int { return s.row }

// Rows is the live window as it stands, oldest first, not yet settled:
// what a reader sees now at the end of the output. It ends at the
// cursor's row or the last row written, whichever is lower, so an empty
// row the program has only moved into does not show.
func (s *Screen) Rows() []string {
	last := len(s.rows) - 1
	for last > s.row && len(s.rows[last]) == 0 {
		last--
	}
	if last == s.row && len(s.rows[last]) == 0 {
		last--
	}
	out := make([]string, 0, last+1)
	for _, r := range s.rows[:last+1] {
		out = append(out, line(r))
	}
	return out
}

// Flush emits the live window's rows (Rows), as the output ended: nothing
// can redraw them now.
func (s *Screen) Flush() {
	for _, l := range s.Rows() {
		s.emit(l)
	}
	s.rows, s.row, s.col = [][]rune{nil}, 0, 0
}

func line(r []rune) string { return strings.TrimRight(string(r), " ") }

// Lines settles all of out at once.
func Lines(out []byte) []string {
	var lines []string
	s := New(func(l string) { lines = append(lines, l) })
	s.Write(out)
	s.Flush()
	return lines
}
