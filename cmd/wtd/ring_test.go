package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// The ring must never hand back a head that starts inside an escape sequence, because a
// client fed a truncated CSI eats the characters that follow it. This is the property the
// mark bookkeeping exists for, so it is asserted directly rather than via the hub.
func TestRingTrimsToASequenceBoundary(t *testing.T) {
	const max = 8 * 1024
	r := newRing(max)

	// A stream where sequences are dense and of varying length, so an arbitrary cut would
	// land inside one with high probability: SGR colors, cursor moves, and OSC titles with
	// both terminators.
	var full bytes.Buffer
	for i := 0; i < 4000; i++ {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&full, "\x1b[%d;%dm", 30+i%8, 40+i%8)
		case 1:
			fmt.Fprintf(&full, "\x1b]0;title %d\x07", i)
		case 2:
			fmt.Fprintf(&full, "\x1b]2;longer title %d\x1b\\", i)
		default:
			fmt.Fprintf(&full, "line %d\r\n", i)
		}
	}

	// Written in odd-sized chunks so sequences straddle write boundaries too — a pty read
	// has no reason to end where a sequence does.
	stream := full.Bytes()
	for off := 0; off < len(stream); off += 37 {
		end := off + 37
		if end > len(stream) {
			end = len(stream)
		}
		r.write(stream[off:end])
	}

	snap := r.snapshot()
	if len(snap) == 0 {
		t.Fatal("snapshot is empty")
	}
	if !bytes.HasSuffix(stream, snap) {
		t.Fatal("snapshot is not a suffix of the stream: bytes were reordered or dropped mid-buffer")
	}

	cut := len(stream) - len(snap)
	if cut != 0 && !boundaries(stream)[cut] {
		t.Fatalf("replay would start at offset %d, which is inside an escape sequence", cut)
	}
}

// Peak memory per session is part of the acceptance criteria, so the bound is asserted with
// the slack the amortized trim needs, not hand-waved.
func TestRingBoundsMemory(t *testing.T) {
	const max = 4 * 1024
	r := newRing(max)

	chunk := bytes.Repeat([]byte("abcdefghij"), 100) // 1 KiB of plain, mark-friendly output
	for i := 0; i < 500; i++ {
		r.write(chunk)
		if got := len(r.buf); got > max+max/ringSlackDiv {
			t.Fatalf("buffer grew to %d bytes, past the %d-byte bound", got, max+max/ringSlackDiv)
		}
	}

	// And it really is the *newest* output that survives.
	tail := append([]byte("\r\n"), []byte("FINAL-MARKER")...)
	r.write(tail)
	if !bytes.HasSuffix(r.snapshot(), tail) {
		t.Fatal("newest bytes are not at the end of the snapshot")
	}
}

func TestRingUnderCapacityKeepsEverything(t *testing.T) {
	r := newRing(4096)
	r.write([]byte("hello "))
	r.write([]byte("world"))
	if got := string(r.snapshot()); got != "hello world" {
		t.Fatalf("snapshot = %q, want %q", got, "hello world")
	}
}

// WT_REPLAY_BYTES=0 is the migration escape hatch: hubs still multiplex, nothing is stored.
func TestRingZeroCapacityStoresNothing(t *testing.T) {
	r := newRing(0)
	r.write([]byte("hello"))
	if snap := r.snapshot(); snap != nil {
		t.Fatalf("snapshot = %q, want nil with replay disabled", snap)
	}
}

// snapshot must copy: the hub writes it to a socket after releasing its mutex, by which
// time the pty pump may have appended and trimmed underneath.
func TestRingSnapshotIsACopy(t *testing.T) {
	r := newRing(64)
	r.write([]byte("original"))
	snap := r.snapshot()
	r.write([]byte("more output"))
	if string(snap) != "original" {
		t.Fatalf("snapshot mutated to %q after a later write", snap)
	}
}

func TestEscScanFramesSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"sgr", "\x1b[31m"},
		{"cursor move", "\x1b[12;40H"},
		{"private mode", "\x1b[?25l"},
		{"osc bel", "\x1b]0;a title\x07"},
		{"osc st", "\x1b]0;a title\x1b\\"},
		{"dcs st", "\x1bPsomething\x1b\\"},
		{"two byte", "\x1bc"},
		{"charset", "\x1b(B"},
		{"osc with esc inside", "\x1b]0;a\x1bb\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s escScan
			for i := 0; i < len(tc.in)-1; i++ {
				if s.step(tc.in[i]) {
					t.Fatalf("byte %d (%q) reported a boundary inside the sequence", i, tc.in[i])
				}
			}
			if !s.step(tc.in[len(tc.in)-1]) {
				t.Fatal("final byte did not close the sequence")
			}
			if !s.step('x') {
				t.Fatal("a plain byte after the sequence is not a boundary")
			}
		})
	}
}

// An unterminated string sequence must not silence marks forever, or the ring loses every
// safe trim point and falls back to arbitrary cuts permanently.
func TestEscScanResynchronizes(t *testing.T) {
	var s escScan
	s.step(escByte)
	s.step(']') // OSC, never terminated

	saw := false
	for i := 0; i < maxSeqRun+16; i++ {
		if s.step('x') {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("no boundary reported within %d bytes of an unterminated OSC", maxSeqRun+16)
	}
}

func TestEscScanPlainTextIsAllBoundaries(t *testing.T) {
	var s escScan
	for i, b := range []byte("plain text\r\n\tand a tab") {
		if !s.step(b) {
			t.Fatalf("byte %d (%q) of plain text was not a boundary", i, b)
		}
	}
}

// boundaries returns the stream offsets a replay may safely start at, computed the same way
// the ring does but without its stride, so the test checks the *choice* of cut point rather
// than re-deriving it.
func boundaries(stream []byte) map[int]bool {
	out := map[int]bool{0: true}
	var s escScan
	for i, b := range stream {
		if s.step(b) {
			out[i+1] = true
		}
	}
	return out
}

// countingStream is the fixture for seam tests: a stream whose contents prove contiguity by
// themselves, so a gap or a repeat is visible without knowing where the cut fell.
func countingStream(from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}
