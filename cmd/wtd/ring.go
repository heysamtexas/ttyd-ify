package main

// The replay buffer: a bounded record of a session's recent output, sent to a client the
// moment it attaches so it sees context instead of a blank screen.
//
// Raw bytes, not a rendered screen. The client owns the emulator (xterm.js in a browser,
// SwiftTerm on iOS) and this server deliberately owns none, so what gets replayed is
// exactly what the session produced, in order — see api/ws-protocol.md, which forbids
// transforming OUTPUT.
//
// The one thing raw bytes get wrong is the *head*. Cutting a byte stream at an arbitrary
// offset can cut in the middle of an escape sequence, and a client fed a truncated CSI
// swallows the characters that follow while trying to complete it — visible garbage at the
// top of every replay, growing with however many bytes the emulator eats. So the ring
// tracks where sequences start and end and trims forward to the nearest boundary.
//
// Not safe for concurrent use. The hub owning a ring holds its mutex across every call,
// which is also what makes the replay/live seam atomic — see hub.subscribe.
type ring struct {
	max int // bytes of history to keep; 0 disables replay entirely

	buf   []byte  // buf[0] is at stream offset base
	base  int64   // stream offset of the oldest retained byte
	total int64   // bytes ever written
	marks []int64 // ascending stream offsets known to be outside any escape sequence

	scan escScan
}

const (
	// Boundaries are recorded every markStride bytes rather than at every safe byte: 256
	// marks for a 256 KiB buffer instead of one per byte. The cost is keeping up to
	// markStride bytes more history than asked for, since a trim rounds forward.
	markStride = 1024

	// Trimming copies the retained bytes down, so it runs once per max/ringSlackDiv bytes
	// written rather than on every write. The cost is peak memory of max + that slack per
	// session, which is what the "memory is bounded" claim has to account for.
	ringSlackDiv = 4
)

func newRing(max int) *ring {
	if max < 0 {
		max = 0
	}
	return &ring{max: max}
}

// write appends output to the ring, dropping the oldest bytes once it is over budget.
func (r *ring) write(p []byte) {
	if r.max <= 0 || len(p) == 0 {
		return
	}

	// Scanned before the append so a mark is an absolute stream offset, which survives the
	// buffer being copied down by trim. A mark sits *after* the byte just scanned: that is
	// the offset a replay may start at.
	for i, b := range p {
		if !r.scan.step(b) {
			continue
		}
		off := r.total + int64(i) + 1
		if off-r.lastMark() >= markStride {
			r.marks = append(r.marks, off)
		}
	}

	r.buf = append(r.buf, p...)
	r.total += int64(len(p))
	r.trim()
}

func (r *ring) lastMark() int64 {
	if n := len(r.marks); n > 0 {
		return r.marks[n-1]
	}
	return r.base
}

func (r *ring) trim() {
	if len(r.buf) <= r.max+r.max/ringSlackDiv {
		return
	}

	cut := r.total - int64(r.max) // offset of the oldest byte still worth keeping
	safe := cut
	for _, m := range r.marks {
		if m >= cut {
			safe = m
			break
		}
	}
	// safe == cut means no boundary was recorded in the whole retained window, i.e. the
	// scanner has been inside one sequence for longer than the buffer. escScan's own
	// resynchronization makes that all but unreachable; if it happens, an arbitrary cut and
	// a bounded buffer beat a perfect cut and an unbounded one.

	drop := int(safe - r.base)
	if drop <= 0 {
		return
	}
	if drop >= len(r.buf) {
		r.buf = r.buf[:0]
	} else {
		r.buf = r.buf[:copy(r.buf, r.buf[drop:])]
	}
	r.base = safe

	i := 0
	for i < len(r.marks) && r.marks[i] < r.base {
		i++
	}
	r.marks = append(r.marks[:0], r.marks[i:]...)
}

// snapshot copies out the retained history. A copy, not the buffer itself: the caller
// writes it to a socket after releasing the hub's mutex, by which time the pty pump may
// have appended more.
func (r *ring) snapshot() []byte {
	if len(r.buf) == 0 {
		return nil
	}
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}

// escScan tracks whether the stream is currently inside an escape sequence.
//
// A *framing* scanner, not a terminal emulator: it answers only "could a replay start
// here", which is the one question the ring needs. A full VT parser server-side is
// explicitly not wanted — the client renders, that is the whole design.
type escScan struct {
	state escState
	run   int // bytes spent in the current non-ground state
}

type escState uint8

const (
	escGround escState = iota
	escAfterESC
	escIntermediate
	escCSI
	escString    // OSC, DCS, PM, APC — terminated by BEL or ST
	escStringESC // inside a string, saw ESC: candidate ST
)

const (
	escByte = 0x1b

	// Longest sequence the scanner will wait out before deciding it has lost sync. An
	// unterminated OSC — a program that omits ST, or binary output that happens to open
	// one — would otherwise leave the scanner inside a sequence forever, the ring would
	// record no further boundaries, and every trim would fall back to an arbitrary cut.
	// Resynchronizing risks one garbled replay head, which is the pre-existing behavior;
	// never resynchronizing makes it permanent.
	maxSeqRun = 4096
)

// step consumes one byte and reports whether the stream is at a sequence boundary
// afterwards — i.e. whether the *next* byte could begin a replay.
func (s *escScan) step(b byte) bool {
	if s.state == escGround {
		if b == escByte {
			s.state, s.run = escAfterESC, 1
			return false
		}
		return true
	}

	s.run++
	if s.run > maxSeqRun {
		s.ground()
		return true
	}

	switch s.state {
	case escAfterESC:
		switch {
		case b == '[':
			s.state = escCSI
		case b == ']' || b == 'P' || b == '^' || b == '_':
			s.state = escString
		case b == '(' || b == ')' || b == '*' || b == '+' || b == '#' || b == '%' || b == ' ':
			// Charset designators and friends: exactly one more byte follows.
			s.state = escIntermediate
		default:
			// A complete two-byte escape: ESC c, ESC 7, ESC =, ESC M …
			s.ground()
			return true
		}

	case escIntermediate:
		s.ground()
		return true

	case escCSI:
		// Parameter and intermediate bytes are 0x20–0x3f; 0x40–0x7e ends the sequence.
		// A C0 control byte in between is executed immediately by a real terminal and
		// leaves the sequence open, so staying in escCSI matches what the client does.
		if b >= 0x40 && b <= 0x7e {
			s.ground()
			return true
		}

	case escString:
		switch b {
		case 0x07: // BEL terminates OSC
			s.ground()
			return true
		case escByte:
			s.state = escStringESC
		}

	case escStringESC:
		switch b {
		case '\\': // ST
			s.ground()
			return true
		case escByte:
			// Another ESC: still a candidate terminator.
		default:
			s.state = escString
		}
	}
	return false
}

func (s *escScan) ground() { s.state, s.run = escGround, 0 }
