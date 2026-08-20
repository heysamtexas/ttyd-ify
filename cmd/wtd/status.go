package main

// Server-side observation of the in-band agent status convention, api/ws-protocol.md section 6a:
//
//	ESC ] 1337 ; WTState=<state> BEL
//
// Why the server reads these bytes at all, when section 6a used to say it must not: the session
// picker and the JSON API never attach to a session, so they never see this stream, and a client
// listing eight sessions could not tell which one was blocked on a human (#102). The hub already
// relays every one of these bytes, so observing one OSC adds nothing reachable and nothing
// writable -- which is the whole reason this is a sniffer here and not a POST endpoint. A status
// endpoint would have been a second unauthenticated writable surface on a port whose only
// protection is the interface it binds to.
//
// This is NOT escScan (ring.go) and deliberately does not extend it. That one is a *framing*
// scanner answering "could a replay start here", it reads no payload on purpose, and the ring's
// trim boundaries depend on it; growing a payload buffer into it would put replay correctness
// behind a feature that only tints a badge.
//
// It is also not a terminal emulator. It recognizes exactly one OSC key and ignores everything
// else, byte for byte.

// The states section 6a defines. A value outside this set is ignored rather than treated as
// `clear`, so an emitter that learns a new state does not blank an older client's indicator --
// the same rule section 6a puts on clients, applied here for the same reason.
//
// `clear` is carried through as itself rather than mapped to "" because "" means something
// different and more important: nobody has ever observed this session. See Session.AgentStatus.
var statusStates = map[string]struct{}{
	"running":   {},
	"waiting":   {},
	"attention": {},
	"clear":     {},
}

// statusPrecedence orders the states when one session name has more than one hub behind it. Two
// hubs can share a name because a hub's key is the full argv while its name is argv[0], so
// `?arg=foo` and `?arg=foo&arg=x` are two hubs on one socket (see hubStat.pgids for the same
// wrinkle). Without a fixed order the reported status would depend on Go's map iteration and
// flicker between polls. Most-actionable wins, because the list exists to answer "which of these
// needs me".
var statusPrecedence = map[string]int{
	"attention": 4,
	"waiting":   3,
	"running":   2,
	"clear":     1,
}

// moreUrgent reports whether a should be preferred over b.
func moreUrgent(a, b string) bool { return statusPrecedence[a] > statusPrecedence[b] }

type statusState uint8

const (
	statusGround statusState = iota
	statusAfterESC
	statusInOSC
	statusInOSCESC // inside the OSC string, saw ESC: candidate ST
)

// maxStatusPayload bounds the accumulated OSC payload.
//
// This is a hard requirement, not a nicety: OSC 1337 is iTerm2's key=value space, and this
// convention borrows it deliberately so an emitter stays harmless in other terminals. The same
// space carries iTerm2's inline images and file transfers, whose payloads are base64 and run to
// megabytes. Buffering one would let any session make the server allocate without bound. A
// payload longer than this is abandoned, not truncated: a `WTState` value is around 22 bytes, so
// anything past this is definitionally not ours, and abandoning is safe because base64 contains
// no ESC to resynchronize on wrongly.
const maxStatusPayload = 128

const (
	statusBEL = 0x07
	statusESC = 0x1b
)

// statusScan finds WTState reports in a byte stream.
//
// Stateful across calls on purpose: pump reads the pty in fixed chunks, so a sequence is split
// across two reads whenever it straddles a boundary. A scanner reset per chunk would miss exactly
// the reports that arrive while output is busy, which is when status matters most.
//
// Owned by the single pump goroutine and never locked. It must stay that way: it holds no
// reference to the hub and hands its result back by return value.
type statusScan struct {
	state statusState
	buf   []byte
}

// feed consumes a chunk and reports the last recognized state in it, if any.
//
// Last rather than first: a chunk can carry several reports, and the newest is the true one.
func (s *statusScan) feed(p []byte) (string, bool) {
	var found string
	var ok bool
	for _, b := range p {
		if st, hit := s.step(b); hit {
			found, ok = st, true
		}
	}
	return found, ok
}

// step consumes one byte, reporting a state when a complete WTState report just terminated.
func (s *statusScan) step(b byte) (string, bool) {
	switch s.state {
	case statusGround:
		if b == statusESC {
			s.state = statusAfterESC
		}
		return "", false

	case statusAfterESC:
		switch b {
		case ']':
			s.state, s.buf = statusInOSC, s.buf[:0]
		case statusESC:
			// Stay armed: ESC ESC ] is still an OSC introducer for the second ESC.
		default:
			s.state = statusGround
		}
		return "", false

	case statusInOSC:
		switch {
		case b == statusBEL:
			return s.finish()
		case b == statusESC:
			s.state = statusInOSCESC
			return "", false
		case len(s.buf) >= maxStatusPayload:
			// Not ours, and too big to keep. See maxStatusPayload.
			s.state = statusGround
			return "", false
		default:
			s.buf = append(s.buf, b)
			return "", false
		}

	case statusInOSCESC:
		if b == '\\' { // ST
			return s.finish()
		}
		// An ESC inside the string that is not ST. Abandon rather than guess: missing one
		// report costs a stale badge, while resynchronizing wrongly could read a state out
		// of unrelated bytes.
		s.state = statusGround
		return "", false
	}
	return "", false
}

// finish parses a terminated OSC payload and returns to ground.
func (s *statusScan) finish() (string, bool) {
	s.state = statusGround
	payload := string(s.buf)
	s.buf = s.buf[:0]

	// Section 6a: a client MUST check the key and pass any payload whose key is not WTState
	// through to its other handlers untouched. Here "untouched" is free -- this scanner only
	// observes, and the bytes were already broadcast.
	const prefix = "1337;WTState="
	if len(payload) <= len(prefix) || payload[:len(prefix)] != prefix {
		return "", false
	}
	value := trimStatusValue(payload[len(prefix):])
	if _, known := statusStates[value]; !known {
		return "", false
	}
	return value, true
}

// trimStatusValue strips surrounding ASCII whitespace, matching the browser handler's .trim() so
// an emitter that adds a stray space behaves the same in both renderers.
func trimStatusValue(v string) string {
	start, end := 0, len(v)
	for start < end && isStatusSpace(v[start]) {
		start++
	}
	for end > start && isStatusSpace(v[end-1]) {
		end--
	}
	return v[start:end]
}

func isStatusSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\v' || b == '\f'
}
