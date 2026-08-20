package main

import (
	"strings"
	"testing"
)

// feedAll runs a whole string through one scanner and reports the last state it found.
func feedAll(t *testing.T, s *statusScan, in string) (string, bool) {
	t.Helper()
	return s.feed([]byte(in))
}

func TestStatusScanRecognizesEveryDocumentedState(t *testing.T) {
	// api/ws-protocol.md section 6a's table, in full. `clear` is included deliberately: it is a
	// state on the wire here, not an absence, because absence means "never observed".
	for _, state := range []string{"running", "waiting", "attention", "clear"} {
		var s statusScan
		got, ok := feedAll(t, &s, "\x1b]1337;WTState="+state+"\x07")
		if !ok || got != state {
			t.Errorf("state %q: got (%q, %v), want (%q, true)", state, got, ok, state)
		}
	}
}

func TestStatusScanAcceptsStringTerminator(t *testing.T) {
	// BEL is what the convention documents and what the emitter writes, but ST (ESC \) is the
	// other legal OSC terminator and costs nothing to accept. A client that only handled BEL
	// would silently see nothing from an emitter using ST.
	var s statusScan
	got, ok := feedAll(t, &s, "\x1b]1337;WTState=waiting\x1b\\")
	if !ok || got != "waiting" {
		t.Fatalf("got (%q, %v), want (waiting, true)", got, ok)
	}
}

func TestStatusScanSurvivesAChunkBoundary(t *testing.T) {
	// The reason the scanner is stateful at all: pump reads the pty in fixed-size chunks, so a
	// sequence straddles a boundary whenever it happens to land there. Split at every offset,
	// because the interesting breaks are inside the introducer and inside the key.
	full := "\x1b]1337;WTState=attention\x07"
	for cut := 0; cut <= len(full); cut++ {
		var s statusScan
		if _, ok := s.feed([]byte(full[:cut])); ok && cut < len(full) {
			t.Fatalf("cut %d: reported a state before the sequence terminated", cut)
		}
		got, ok := s.feed([]byte(full[cut:]))
		if cut == len(full) {
			continue // whole sequence already consumed by the first feed
		}
		if !ok || got != "attention" {
			t.Errorf("cut %d: got (%q, %v), want (attention, true)", cut, got, ok)
		}
	}
}

func TestStatusScanIgnoresOtherOSCKeys(t *testing.T) {
	// OSC 1337 is iTerm2's key=value space, borrowed on purpose so an emitter stays harmless in
	// other terminals. Everything else in that space must pass through unremarked.
	for _, payload := range []string{
		"\x1b]1337;File=name=x.png;inline=1\x07",
		"\x1b]1337;CurrentDir=/home/sam\x07",
		"\x1b]1337;WTStateExtra=running\x07",
		"\x1b]0;a window title\x07",
		"\x1b]2;another title\x07",
		"\x1b]1337;WTState=\x07", // empty value is not a state
	} {
		var s statusScan
		if got, ok := feedAll(t, &s, payload); ok {
			t.Errorf("payload %q: reported %q, want nothing", payload, got)
		}
	}
}

func TestStatusScanIgnoresAnUnknownValue(t *testing.T) {
	// Section 6a: a client MUST ignore an unrecognized value rather than treating it as `clear`,
	// so an emitter that learns a new state does not blank an older renderer. The same rule
	// applies here, and the check that matters is that a prior state SURVIVES the unknown one.
	var s statusScan
	if _, ok := feedAll(t, &s, "\x1b]1337;WTState=running\x07"); !ok {
		t.Fatal("setup: running not recognized")
	}
	if got, ok := feedAll(t, &s, "\x1b]1337;WTState=pondering\x07"); ok {
		t.Errorf("unknown value reported %q, want nothing", got)
	}
}

func TestStatusScanAbandonsAnOversizePayload(t *testing.T) {
	// The bound is a memory guard, not a nicety: OSC 1337 also carries iTerm2's inline images,
	// whose base64 payloads run to megabytes. Buffering one would let any session make the
	// server allocate without limit.
	var s statusScan
	huge := "\x1b]1337;File=" + strings.Repeat("QUJD", 4096) + "\x07"
	if got, ok := feedAll(t, &s, huge); ok {
		t.Errorf("oversize payload reported %q, want nothing", got)
	}
	if len(s.buf) > maxStatusPayload {
		t.Errorf("buffer grew to %d bytes, want <= %d", len(s.buf), maxStatusPayload)
	}
	// And the scanner must still work afterwards: abandoning has to resynchronize, not wedge.
	got, ok := feedAll(t, &s, "\x1b]1337;WTState=running\x07")
	if !ok || got != "running" {
		t.Errorf("after oversize: got (%q, %v), want (running, true)", got, ok)
	}
}

func TestStatusScanReportsTheLastStateInAChunk(t *testing.T) {
	// A busy session can emit several reports in one read. The newest is the true one.
	var s statusScan
	in := "\x1b]1337;WTState=running\x07some output\x1b]1337;WTState=attention\x07"
	got, ok := feedAll(t, &s, in)
	if !ok || got != "attention" {
		t.Fatalf("got (%q, %v), want (attention, true)", got, ok)
	}
}

func TestStatusScanToleratesSurroundingWhitespace(t *testing.T) {
	// Matches the browser handler's .trim(), so an emitter with a stray newline behaves the
	// same in the tab icon and in the session list rather than working in one and not the other.
	var s statusScan
	got, ok := feedAll(t, &s, "\x1b]1337;WTState= running \x07")
	if !ok || got != "running" {
		t.Fatalf("got (%q, %v), want (running, true)", got, ok)
	}
}

func TestStatusScanIgnoresAnUnterminatedSequence(t *testing.T) {
	// A program that opens an OSC and never closes it must not leave a state latched, and must
	// not stop the next real report from being seen.
	var s statusScan
	if got, ok := feedAll(t, &s, "\x1b]1337;WTState=running"); ok {
		t.Errorf("unterminated sequence reported %q, want nothing", got)
	}
}

func TestMoreUrgentOrdersTheStates(t *testing.T) {
	// Two hubs can share a session name, so stats() has to pick one status deterministically or
	// the field flickers with Go's map order. Most actionable wins.
	cases := []struct {
		a, b string
		want bool
	}{
		{"attention", "waiting", true},
		{"waiting", "running", true},
		{"running", "clear", true},
		{"clear", "", true},
		{"waiting", "attention", false},
		{"", "clear", false},
		{"running", "running", false},
	}
	for _, c := range cases {
		if got := moreUrgent(c.a, c.b); got != c.want {
			t.Errorf("moreUrgent(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
