package main

import "testing"

func TestScreenText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		rows int
		want string
	}{
		{"plain lines", "a\r\nb\r\n", 24, "a\nb"},
		{"colour is dropped", "\x1b[1;31mred\x1b[0m ok", 24, "red ok"},
		{"titles and status reports are dropped", "\x1b]0;title\x07x\x1b]1337;wt=busy\x1b\\y", 24, "xy"},
		{"carriage return overwrites", "hello\rJ", 24, "Jello"},
		{"backspace", "abc\b\bX", 24, "aXc"},
		{"erase to end of line", "abcdef\r\x1b[3C\x1b[K", 24, "abc"},
		{"whole-line erase", "keep\r\ngone\x1b[2K\r\n", 24, "keep"},
		{
			// How an agent's TUI redraws its input box: erase the line, move up, repeat, then
			// draw the new frame. Stripping escapes would leave both frames; the screen has one.
			name: "in-place redraw keeps only the last frame",
			in:   "> draft one\r\nstatus\r\n" + "\x1b[2K\x1b[1A\x1b[2K\x1b[1A\x1b[2K\r" + "> final\r\nstatus\r\n",
			rows: 24, want: "> final\nstatus",
		},
		{"absolute address counts from the screen top", "1\r\n2\r\n3\r\n4\r\n\x1b[1;1HX", 3, "1\n2\nX\n4"},
		{"clear screen keeps scrollback above it", "old\r\n\x1b[2J\x1b[Hnew", 1, "old\nnew"},
		{
			name: "leaving the alternate screen restores the main one",
			in:   "shell$ \x1b[?1049h\x1b[Hfull screen app\x1b[?1049lls",
			rows: 24, want: "shell$ ls",
		},
		{"while in the alternate screen, it is what shows", "shell$ \x1b[?1049h\x1b[Hvim buffer", 24, "shell$\nvim buffer"},
		{"utf-8 survives", "héllo ✓", 24, "héllo ✓"},
		{"a truncated sequence at the end is not text", "ok\x1b[3", 24, "ok"},
		{"charset designation is not text", "\x1b(Bx", 24, "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := screenText([]byte(tc.in), tc.rows); got != tc.want {
				t.Errorf("screenText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc", 2); got != "b\nc" {
		t.Errorf("got %q", got)
	}
	if got := lastLines("a", 5); got != "a" {
		t.Errorf("got %q", got)
	}
}
