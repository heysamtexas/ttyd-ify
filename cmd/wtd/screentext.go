package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// screenText renders a session's raw output as the plain text an agent can read, for the MCP
// read_session tool.
//
// This is the one place wtd interprets terminal output, and it is not a contradiction of ring.go's
// "the client owns the emulator". That rule is about OUTPUT frames, which wtd must forward
// untouched; this is a read-only *view* for a client that has no emulator at all — a language
// model — and it never feeds back into the stream. Raw bytes are useless to that client: an agent's
// TUI redraws its input box in place with cursor addressing, so stripping the escapes leaves every
// frame it ever drew, concatenated.
//
// So it keeps a line grid and honours the sequences that decide *where text lands* — cursor moves,
// erases, the alternate screen — and drops everything else (colour, modes, titles). It is
// deliberately approximate: no wide-character widths, no scroll regions, no wrapping at the right
// margin. Good enough to read a screen, not to reproduce one; a client that needs the latter has
// /ws and a real emulator.
func screenText(b []byte, rows int) string {
	if rows <= 0 {
		rows = 24
	}
	t := &textGrid{rows: rows, lines: [][]rune{nil}}
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == escByte:
			i = t.escape(b, i)
			continue
		case c == '\n', c == '\v', c == '\f':
			t.down(1, true)
		case c == '\r':
			t.col = 0
		case c == '\b':
			if t.col > 0 {
				t.col--
			}
		case c == '\t':
			t.col = (t.col/8 + 1) * 8
		case c < 0x20 || c == 0x7f:
			// Other C0 controls (BEL, SI/SO, NUL) position nothing.
		default:
			r, n := utf8.DecodeRune(b[i:])
			t.put(r)
			i += n
			continue
		}
		i++
	}
	return t.String()
}

// textGrid is screenText's state. lines is every line seen, scrollback included; top is the index
// of the first line of the current screen, which is what an absolute cursor address counts from.
type textGrid struct {
	lines    [][]rune
	row, col int
	top      int
	rows     int

	savedRow, savedCol int
	// altFrom is len(lines) when the alternate screen was entered. Leaving it restores the main
	// screen, so everything drawn from there on is discarded.
	altFrom        int
	altRow, altCol int
	inAlt          bool
}

func (t *textGrid) ensureRow() {
	for len(t.lines) <= t.row {
		t.lines = append(t.lines, nil)
	}
}

// down moves the cursor n lines, scrolling the screen when it passes the bottom. scroll is false
// for a CSI cursor-down, which stops at the bottom margin instead.
func (t *textGrid) down(n int, scroll bool) {
	t.row += n
	if bottom := t.top + t.rows - 1; t.row > bottom {
		if scroll {
			t.top = t.row - t.rows + 1
		} else {
			t.row = bottom
		}
	}
	t.ensureRow()
}

func (t *textGrid) put(r rune) {
	t.ensureRow()
	line := t.lines[t.row]
	for len(line) < t.col {
		line = append(line, ' ')
	}
	if t.col < len(line) {
		line[t.col] = r
	} else {
		line = append(line, r)
	}
	t.lines[t.row] = line
	t.col++
}

// escape consumes the sequence starting at b[i] (an ESC) and returns the index after it.
func (t *textGrid) escape(b []byte, i int) int {
	if i+1 >= len(b) {
		return len(b)
	}
	switch b[i+1] {
	case '[':
		return t.csi(b, i+2)
	case ']', 'P', 'X', '^', '_':
		// A string: OSC (titles, hyperlinks, the agent-status report) or DCS/SOS/PM/APC. Ends at
		// BEL or ST (ESC \). None of it is screen text.
		for j := i + 2; j < len(b); j++ {
			if b[j] == 0x07 {
				return j + 1
			}
			if b[j] == escByte && j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
		}
		return len(b)
	case '7':
		t.savedRow, t.savedCol = t.row-t.top, t.col
	case '8':
		t.row, t.col = t.top+t.savedRow, t.savedCol
		t.ensureRow()
	case 'M': // reverse index
		if t.row > t.top {
			t.row--
		}
	case 'c': // full reset
		t.top = len(t.lines)
		t.row, t.col = t.top, 0
		t.ensureRow()
	default:
		// ESC, any intermediates (charset designations like ESC ( B), then one final byte.
		j := i + 1
		for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2f {
			j++
		}
		return min(j+1, len(b))
	}
	return i + 2
}

// csi consumes a control sequence whose parameters start at b[i].
func (t *textGrid) csi(b []byte, i int) int {
	start := i
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x3f {
		i++
	}
	params := string(b[start:i])
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++ // intermediates
	}
	if i >= len(b) {
		return len(b)
	}
	final := b[i]
	private := strings.HasPrefix(params, "?")
	args := csiArgs(strings.TrimLeft(params, "?<=>"))
	arg := func(n, def int) int {
		if n < len(args) && args[n] > 0 {
			return args[n]
		}
		return def
	}

	switch {
	case private && (final == 'h' || final == 'l'):
		for _, a := range args {
			if a == 1049 || a == 1047 || a == 47 {
				t.altScreen(final == 'h')
			}
		}
	case private:
		// Other private modes (cursor visibility, mouse, bracketed paste) position nothing.
	case final == 'A':
		t.row = max(t.row-arg(0, 1), t.top)
	case final == 'B':
		t.down(arg(0, 1), false)
	case final == 'C':
		t.col += arg(0, 1)
	case final == 'D':
		t.col = max(t.col-arg(0, 1), 0)
	case final == 'E':
		t.down(arg(0, 1), false)
		t.col = 0
	case final == 'F':
		t.row = max(t.row-arg(0, 1), t.top)
		t.col = 0
	case final == 'G', final == '`':
		t.col = arg(0, 1) - 1
	case final == 'd':
		t.row = t.top + min(arg(0, 1), t.rows) - 1
		t.ensureRow()
	case final == 'H', final == 'f':
		t.row = t.top + min(arg(0, 1), t.rows) - 1
		t.col = arg(1, 1) - 1
		t.ensureRow()
	case final == 'K':
		t.ensureRow()
		line := t.lines[t.row]
		switch arg(0, 0) {
		case 0:
			if t.col < len(line) {
				t.lines[t.row] = line[:t.col]
			}
		case 1:
			for j := 0; j <= t.col && j < len(line); j++ {
				line[j] = ' '
			}
		case 2:
			t.lines[t.row] = nil
		}
	case final == 'J':
		t.ensureRow()
		switch arg(0, 0) {
		case 0:
			if t.col < len(t.lines[t.row]) {
				t.lines[t.row] = t.lines[t.row][:t.col]
			}
			t.lines = t.lines[:t.row+1]
		case 1:
			for j := t.top; j < t.row; j++ {
				t.lines[j] = nil
			}
		case 2, 3:
			// Clear the screen: scrollback above it stays, the screen starts empty.
			t.lines = t.lines[:t.top]
			t.ensureRow()
		}
	}
	return i + 1
}

func (t *textGrid) altScreen(enter bool) {
	if enter == t.inAlt {
		return
	}
	t.inAlt = enter
	if enter {
		t.altFrom, t.altRow, t.altCol = len(t.lines), t.row, t.col
		t.top = len(t.lines)
		t.row, t.col = t.top, 0
		t.ensureRow()
		return
	}
	t.lines = t.lines[:t.altFrom]
	t.row, t.col = t.altRow, t.altCol
	t.top = max(len(t.lines)-t.rows, 0)
	t.ensureRow()
}

func csiArgs(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]int, len(parts))
	for i, p := range parts {
		if sub, _, ok := strings.Cut(p, ":"); ok {
			p = sub
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// String joins the grid with trailing spaces and trailing blank lines removed.
func (t *textGrid) String() string {
	out := make([]string, len(t.lines))
	for i, l := range t.lines {
		out[i] = strings.TrimRight(string(l), " ")
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// lastLines returns at most n trailing lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
