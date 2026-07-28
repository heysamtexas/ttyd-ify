package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// The ttyd wire protocol. Every frame after the handshake carries a single ASCII opcode
// byte followed by its payload. Client and server reuse the same byte values with
// different meanings, hence the two blocks.
//
// Verified against ttyd 1.7.4 (its own web client) and the iOS client's
// TtydProtocol.swift. See api/ws-protocol.md.
const (
	// client → server
	opInput  = '0'
	opResize = '1'
	opPause  = '2'
	opResume = '3'

	// server → client
	opOutput = '0'
	opTitle  = '1'
	opPrefs  = '2'
)

// prefsBody is ttyd 1.7.4's preferences frame payload with no -t options configured.
// Measured from the live server, spaces included — encoding/json would emit "{}".
var prefsBody = []byte("{ }")

// Terminal defaults, matching ttyd's own (-T defaults to xterm-256color, and the
// handshake's dimensions are applied to the pty before the child is started).
const (
	defaultTerm = "xterm-256color"
	defaultCols = 80
	defaultRows = 25

	// A single frame larger than this is refused. ttyd/libwebsockets imposes its own
	// limits; this is generous enough for a large paste but stops a client from making
	// the server allocate without bound.
	maxFrameBytes = 1 << 20

	// The handshake gets a tighter ceiling than everything after it, applied before the
	// first read and raised to maxFrameBytes once it parses. The payload is two integers
	// and a token string, so this is generous by three orders of magnitude; allowing a
	// 1 MiB first message meant an unauthenticated peer could make the server buffer that
	// much before it had said anything at all. coder/websocket closes 1009 on its own when
	// the limit trips, which is the code api/openapi.yaml publishes for this.
	maxHandshakeBytes = 8 << 10

	// How long a connection may hold a slot without speaking. Both real clients send the
	// handshake from their open handler, so anything slower is a stuck or hostile peer —
	// and until it arrives the connection is a socket the server cannot use for anything.
	// Published in api/openapi.yaml, which is why it is 10s and not a rounder number.
	handshakeTimeout = 10 * time.Second

	// How much longer than handshakeTimeout the handshake read may block. See readHandshake:
	// this bounds how long the handler lingers after the deadline, nothing more.
	handshakeCloseGrace = 2 * time.Second

	// The widest dimension a pty accepts: winsize fields are unsigned 16-bit. Published in
	// api/openapi.yaml as the handshake and RESIZE bound, where both said 1..9999 until #37 —
	// a number that matched neither the kernel nor this code.
	maxDimension = 0xffff

	// Chunk size for pty → client. Larger reads mean fewer frames; this is well under
	// maxFrameBytes so a slow client never sees an oversized frame from us.
	ptyReadChunk = 16 * 1024

	// Liveness probing. The interval is far longer than ttyd's 5s default because the
	// cost of a slow reap here is one lingering session rather than anything unsafe, and
	// the iOS client already pings every 20s from its side. pingTimeout must stay under
	// the interval so probes cannot pile up.
	pingInterval = 30 * time.Second
	pingTimeout  = 20 * time.Second

	// How many consecutive unanswered pings end the connection. pingInterval * this is the
	// reap deadline api/openapi.yaml publishes (90s), and it must stay above the 60s floor
	// the same document guarantees. Giving up on the first miss made the real deadline 50s,
	// which broke both numbers at once (#40): a phone changing networks loses a pong without
	// being dead, and three intervals is the tolerance the published figure implies.
	maxMissedPongs = 3

	// Cap on per-connection warnings about malformed frames. An unauthenticated peer can
	// otherwise drive unbounded journald writes by spamming bad opcodes.
	maxFrameWarnings = 5
)

// offeredSubprotocols reports whether the client offered any subprotocol, and whether `tty` was
// among them. Both answers are needed: offering none is fine, and offering others without `tty` is
// not, so a single boolean cannot express the rule.
//
// Sec-WebSocket-Protocol may be repeated or comma-separated, and RFC 6455 treats those as
// equivalent, so both are flattened here rather than trusting clients to pick one form.
func offeredSubprotocols(r *http.Request) (offered, hasTTY bool) {
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, value := range strings.Split(header, ",") {
			switch strings.TrimSpace(value) {
			case "":
				// An empty or whitespace-only header is not an offer. ttyd accepts it as
				// "offered nothing", measured, so it must not fall into the refusal above.
			case "tty":
				offered, hasTTY = true, true
			default:
				offered = true
			}
		}
	}
	return offered, hasTTY
}

// handshake is the first message a client sends, as JSON. The iOS client sends it in a
// TEXT frame and ttyd's own web client sends it in a BINARY frame, so both are accepted —
// rejecting either would break one of the two known clients.
type handshake struct {
	AuthToken string `json:"AuthToken"`
	Columns   int    `json:"columns"`
	Rows      int    `json:"rows"`
}

func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Origin checking is ON by default, a deliberate divergence from ttyd: ttyd only
	// checks origin with -O, so by default any web page a user visits can open a socket
	// to the tailnet address and get a shell. Measured behavior of this default:
	//
	//   no Origin header      -> accepted (the native-client shape)
	//   Origin matches Host   -> accepted (the browser pages wtd serves)
	//   cross-site Origin     -> 403, with the reason logged
	//
	// so no real client loses anything. -allow-cross-origin exists because that claim
	// rests on the iOS client sending no Origin on its upgrade, which is not verified
	// against a real device yet: if it turns out to send one, this is a flag flip during
	// migration rather than a rebuild. See api/compatibility.md.
	// A client that offers subprotocols but not `tty` is speaking something else, so it is
	// refused before the upgrade rather than handed a shell stream (#36). Rejecting here is the
	// one deliberate divergence from ttyd on this path: measured, ttyd 1.7.4 drops the TCP
	// connection with no HTTP response at all, which reaches a client as a bare reset. A 400 says
	// the same thing and can be read. Nothing is lost, because a client offering only other
	// subprotocols is not a ttyd client.
	if offered, hasTTY := offeredSubprotocols(r); offered && !hasTTY {
		log.Printf("wtd: ws from %s offered subprotocols without tty (%q); refusing",
			r.RemoteAddr, r.Header.Get("Sec-WebSocket-Protocol"))
		http.Error(w, "this endpoint speaks the tty subprotocol", http.StatusBadRequest)
		return
	}

	opts := &websocket.AcceptOptions{Subprotocols: []string{"tty"}}
	if s.allowCrossOrigin {
		opts.OriginPatterns = []string{"*"}
	}

	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		// Accept has already written a response. Log it: the most likely cause is an
		// Origin rejection, and a silent 403 here would look like a broken client
		// rather than a policy decision.
		log.Printf("wtd: ws accept from %s: %v", r.RemoteAddr, err)
		if origin := r.Header.Get("Origin"); origin != "" {
			log.Printf("wtd: (Origin %q vs Host %q — if this is a native client, "+
				"start wtd with -allow-cross-origin and file it as a bug)", origin, r.Host)
		}
		return
	}
	defer conn.CloseNow() //nolint:errcheck // best-effort teardown

	// No check that `tty` was negotiated. Offering nothing is legitimate and proceeds without a
	// selected subprotocol — which is what ttyd 1.7.4 does (measured: 101, no echo, connection
	// held open) and what api/ws-protocol.md §4 has always said SHOULD happen. Closing those
	// connections was a wire-compatibility bug, and it also gave 1008 a second writer that no
	// close-code table admitted, so a client following the document was closed and then told by
	// the same document that it had timed out (#36).
	//
	// After the pre-upgrade refusal above, the negotiated value can only be "tty" or empty.

	conn.SetReadLimit(maxHandshakeBytes)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hs, err := readHandshake(ctx, conn, s.handshakeWait)
	if err != nil {
		log.Printf("wtd: handshake from %s: %v", r.RemoteAddr, err)
		// Each of the three close codes the spec publishes for this path has exactly one
		// writer: 1002 here, 1008 from readHandshake's deadline timer, 1009 from the library
		// when the read limit trips.
		if errors.Is(err, errHandshakeUnparseable) {
			conn.Close(websocket.StatusProtocolError, "handshake was not a valid handshake") //nolint:errcheck // best-effort; the peer may already be gone
		}
		return
	}
	// The handshake parsed, so the peer is real and a paste-sized frame is legitimate.
	conn.SetReadLimit(maxFrameBytes)

	// ttyd appends every ?arg= value to the start command's argv, in order. bin/wt only
	// reads $1, but matching ttyd keeps other start commands working. These values are
	// never passed through a shell — exec takes them as argv — so quoting is not a
	// concern here; bin/wt does its own validation of the name it receives.
	//
	// filterArgs runs before the named/private decision below, not after: the values it
	// drops are the ones that cannot be an argv element at all, and hub selection keys on
	// them.
	args := filterArgs(r.URL.Query()["arg"], r.RemoteAddr)

	// Named connections join a shared hub for that session, which is what lets them be sent
	// recent output on attach instead of a blank screen. An argless connection lands on
	// bin/wt's interactive picker: wtd cannot know which session that ends up in, so there is
	// no key to buffer under, and sharing one picker between two clients would interleave
	// their keystrokes. Those keep a private pty, exactly as before. An empty ?arg= counts as
	// argless, because bin/wt treats an empty $1 as "no arg" and renders the menu.
	run := s.runTerminal
	if len(args) > 0 && args[0] != "" {
		run = s.runHubTerminal
	}

	err = run(ctx, conn, hs, args)
	if errors.Is(err, errSpawnFailed) {
		// Without this the handler just returns and the deferred CloseNow drops the TCP
		// connection, so a client gets a codeless disconnect indistinguishable from a network
		// drop — and retries into the same failure. api/openapi.yaml publishes 1011 for
		// exactly this (#34).
		s.reportSpawnFailure(ctx, conn, args, err)
	}
	if err != nil && !isDisconnect(err) {
		log.Printf("wtd: terminal for %s: %v", r.RemoteAddr, err)
	}
}

// reportSpawnFailure tells the client why it has no terminal, in the terminal, then closes 1011.
//
// The title and preferences frames go first even though nothing will follow them, because the
// frame-order rule in api/ws-protocol.md is unconditional and a client that takes the first frame
// as its window title would otherwise put the error message there and never display it — which
// defeats the entire point of sending it.
//
// Every write is best-effort: this path is reached when the server is already failing, and the
// peer may be gone. A failure here must not mask the spawn error in the log.
func (s *server) reportSpawnFailure(ctx context.Context, conn *websocket.Conn, args []string, cause error) {
	// Matches the title a working connection would have had — the session name for a named
	// connection, the start command for an argless one — so a client's nav bar reads the same
	// either way.
	name := s.startCommand
	if len(args) > 0 && args[0] != "" {
		name = args[0]
	}
	hostname, _ := os.Hostname()
	_ = writeOp(ctx, conn, opTitle, []byte(fmt.Sprintf("%s (%s)", name, hostname)))
	_ = writeOp(ctx, conn, opPrefs, prefsBody)

	// CRLF, not LF: this lands in a terminal emulator with no shell to translate for it, so a
	// bare newline leaves the cursor in the middle of the line.
	_ = writeOp(ctx, conn, opOutput, []byte("\r\nwtd: "+oneLine(cause.Error())+"\r\n"))

	conn.Close(websocket.StatusInternalError, closeReason(errSpawnFailed.Error())) //nolint:errcheck // best-effort; the peer may already be gone
}

// oneLine flattens an error for a terminal and a close reason, both of which are single-line
// contexts. Bounded because the text can contain a start-command path of any length.
func oneLine(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(s)
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// The two handshake failures whose close code is sent from here. An over-limit message is
// the third and needs neither: the websocket library closes 1009 itself.
var (
	// errHandshakeUnparseable is closed 1002 by the caller.
	errHandshakeUnparseable = errors.New("not a handshake")
	// errSpawnFailed marks "there is no terminal and there never was one", from either the
	// private or the hub path, so handleWS can close 1011 as api/openapi.yaml publishes rather
	// than dropping the connection with no code at all. See reportSpawnFailure.
	errSpawnFailed = errors.New("could not start a terminal")
	// errHandshakeTimeout was already closed 1008 by the timer below. It exists so the log
	// line says the server timed the peer out, rather than reporting the context error that
	// happens to be how the read found out.
	errHandshakeTimeout = errors.New("handshake timeout")
)

// readHandshake reads the first message and parses it, accepting either frame type.
func readHandshake(ctx context.Context, conn *websocket.Conn, wait time.Duration) (handshake, error) {
	// Zero would make AfterFunc fire immediately and close every connection before it could
	// speak. Every construction site goes through newServer today, so this guards against a
	// future one that builds a server literal and forgets the field.
	if wait <= 0 {
		wait = handshakeTimeout
	}

	// The deadline sends its own close frame rather than letting the read's context expire.
	// A context expiring during a read closes the connection abruptly — the library cannot
	// leave a half-read frame behind — and an abrupt close carries no code, so the specified
	// 1008 would never arrive. Measured: without the timer the client sees EOF and
	// CloseStatus reports -1.
	//
	// The flag covers what Stop cannot. Stop does not un-fire a timer that has already fired,
	// so a handshake arriving inside the fire→schedule window would otherwise be read
	// successfully, spawn a pty, and *then* be closed 1008 — leaving a created session running
	// behind a client told "client bug, do not retry". The window is sub-millisecond (probed at
	// 600 connections aimed at the deadline, zero hits), so no test reaches it: this is closed
	// by construction, not because it has been observed. Whichever side takes the mutex first
	// wins, and both outcomes are coherent — either the peer timed out and nothing spawns, or
	// it did not and the deadline is spent.
	var (
		mu             sync.Mutex
		timedOut, read bool
	)
	timer := time.AfterFunc(wait, func() {
		mu.Lock()
		defer mu.Unlock()
		if read {
			return
		}
		timedOut = true
		conn.Close(websocket.StatusPolicyViolation, "handshake timeout") //nolint:errcheck // nothing to do if the peer is already gone
	})
	defer timer.Stop()

	// The read outlives the deadline by handshakeCloseGrace so the timer is what ends a silent
	// connection. That is not about the close frame reaching the peer — it does either way,
	// since the frame is already in the socket buffer and the library's own teardown is a
	// graceful FIN — but about how long the handler goroutine lingers: bounded here at
	// wait+grace, against the 5s the library would otherwise spend waiting for a close
	// handshake it cannot complete. The value is not delicate; 250ms behaves identically.
	ctx, cancel := context.WithTimeout(ctx, wait+handshakeCloseGrace)
	defer cancel()

	_, data, err := conn.Read(ctx)

	mu.Lock()
	if timedOut {
		mu.Unlock()
		return handshake{}, errHandshakeTimeout
	}
	read = true
	mu.Unlock()

	if err != nil {
		return handshake{}, fmt.Errorf("read: %w", err)
	}

	// Decoded through a pointer so that a payload of JSON `null` is distinguishable: it is
	// valid JSON and unmarshals into a struct without error, leaving every field zero, so
	// decoding straight into a handshake would accept it and silently default to 80x25. The
	// spec's rule is "not parseable JSON, *or not an object*", and null is the only value
	// that is the second without being the first.
	var hs *handshake
	if err := json.Unmarshal(data, &hs); err != nil {
		return handshake{}, fmt.Errorf("%w: parse %q: %w", errHandshakeUnparseable, truncateBytes(data, 120), err)
	}
	if hs == nil {
		return handshake{}, fmt.Errorf("%w: payload was JSON null, not an object", errHandshakeUnparseable)
	}

	// A client that omits or zeroes the dimensions would otherwise get a 0x0 pty, where
	// full-screen programs render nothing at all.
	if hs.Columns <= 0 || hs.Columns > maxDimension {
		hs.Columns = defaultCols
	}
	if hs.Rows <= 0 || hs.Rows > maxDimension {
		hs.Rows = defaultRows
	}
	return *hs, nil
}

// The transport-safety floor on ?arg= values: what cannot be carried in argv at all,
// independent of what any particular start command would make of it. api/ws-protocol.md §8
// specifies these and requires dropping rather than closing.
//
// The third rule is not a number: a value must not contain a NUL. Linux argv elements are
// NUL-terminated, so exec fails with "invalid argument" and takes the connection down with
// it — and hubKey joins on NUL, so a value carrying one could forge another session's key.
// See the note on hubKey; this filter is what makes its separator unambiguous.
const (
	// ARG_MAX puts the kernel's own ceiling well over a megabyte, so this is policy. A name
	// this long cannot address a session anyway (the socket path ceiling is 107 bytes); the
	// cap is here to bound what a URL can make the server hand to a child process.
	maxArgBytes = 4096

	// bin/wt reads $1 alone and ttyd's own clients send one arg, so this is slack for an
	// unknown start command rather than a limit anything real approaches.
	maxArgs = 16
)

// filterArgs drops the ?arg= values that cannot be passed to a start command, and returns
// the rest in order.
//
// Dropping, never closing: a value this server cannot use is the same situation as a name
// bin/wt itself rejects, and that has always rendered the picker. The difference is what is
// left — bin/wt's rejection keeps the connection *named*, while a value dropped here is
// gone, so a connection whose only arg was dropped becomes argless and gets a private
// picker. api/openapi.yaml's arg description spells out both cases.
//
// The log line fires once per connection and only when something was dropped: a client cannot
// read a log, so this must not become a per-value warning an unauthenticated peer can drive.
func filterArgs(args []string, remoteAddr string) []string {
	kept := make([]string, 0, min(len(args), maxArgs))
	var dropped, ignored int

	for i, a := range args {
		if len(kept) >= maxArgs {
			// Unexamined from here, not len(args)-maxArgs, which would double-count values
			// already counted in `dropped`.
			ignored = len(args) - i
			break
		}
		// strings.Contains rather than IndexByte(a, 0) >= 0: the same check written with an
		// index is one typo (`> 0`) away from accepting a *leading* NUL, which is the case
		// that reaches exec and closes the connection.
		if strings.Contains(a, "\x00") || len(a) > maxArgBytes {
			dropped++
			continue
		}
		kept = append(kept, a)
	}

	// Reports what happened, not what it leads to: with values left over this is still a named
	// connection, so "lands on the picker" would be wrong in the case an operator is most
	// likely reading about.
	if dropped > 0 || ignored > 0 {
		log.Printf("wtd: arg from %s: dropped %d unusable value(s), ignored %d past the first %d; "+
			"%d passed to the start command (the connection continues either way)",
			remoteAddr, dropped, ignored, maxArgs, len(kept))
	}
	return kept
}

func (s *server) runTerminal(parent context.Context, conn *websocket.Conn, hs handshake, args []string) error {
	// This context is cancelled in the teardown below *before* anything waits on the
	// child. Cancelling a context passed to coder/websocket's Read/Write closes the
	// connection, and that is the only thing that releases pumpClient from conn.Read —
	// see the teardown comment. Relying on the caller's defer instead would order the
	// release after the wait, which is exactly how this leaked processes.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Deliberately exec.Command, not exec.CommandContext. CommandContext looks like a
	// kill-on-cancel safety net here and is not one: ctx derives from the request, which
	// for a hijacked connection is only cancelled once the handler returns, and the
	// handler cannot return until the wait below has. Lifecycle is explicit instead.
	cmd := exec.Command(s.startCommand, args...)
	cmd.Env = append(os.Environ(), "TERM="+defaultTerm)

	// Start on a pty sized from the handshake, so the child sees the right dimensions on
	// its very first write rather than after a resize round trip.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(hs.Columns),
		Rows: uint16(hs.Rows),
	})
	if err != nil {
		return fmt.Errorf("%w: start %s: %w", errSpawnFailed, s.startCommand, err)
	}
	// One Wait for the lifetime of the process, started immediately so terminate() can
	// wait on it with a bound. cmd.Wait must be called exactly once.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	defer func() {
		// Order matters, and each step is load-bearing:
		//  1. cancel  — releases pumpClient from conn.Read (closing the pty does NOT;
		//               that only unblocks the pty-side pump).
		//  2. close   — hangs up the pty, which is what makes a well-behaved child exit.
		//  3. terminate — signals the process *group* with escalation and a bounded wait.
		// Getting 1 and 3 the wrong way round leaks a goroutine per stuck child.
		cancel()
		_ = ptmx.Close()
		terminate(cmd, waited)
	}()

	// Frame order matches ttyd exactly: title, then preferences, then output — measured
	// from ttyd 1.7.4 as "120", and asserted by TestConformance/compare/server-opcode-order.
	// Both are written before the pty pump starts, so a client never sees output before the
	// frames describing the terminal it renders into.
	hostname, _ := os.Hostname()
	title := fmt.Sprintf("%s (%s)", s.startCommand, hostname)
	if err := writeOp(ctx, conn, opTitle, []byte(title)); err != nil {
		return err
	}
	if err := writeOp(ctx, conn, opPrefs, prefsBody); err != nil {
		return err
	}

	// pty → client
	errc := make(chan error, 2)
	go func() {
		// One buffer for the life of the connection, with byte 0 reserved for the opcode:
		// reading into buf[1:] means each chunk is framed in place instead of allocating
		// and copying len+1 bytes per 16 KiB of output.
		buf := make([]byte, ptyReadChunk+1)
		buf[0] = opOutput
		for {
			n, err := ptmx.Read(buf[1:])
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n+1]); werr != nil {
					errc <- werr
					return
				}
			}
			if err != nil {
				// EIO is how a pty reports "the child closed the other end" — a normal
				// exit, not a failure.
				if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) {
					errc <- nil
					return
				}
				errc <- fmt.Errorf("pty read: %w", err)
				return
			}
		}
	}()

	// client → pty
	go func() {
		errc <- s.pumpClient(ctx, conn, privatePty{ptmx})
	}()

	go keepAlive(ctx, conn, cancel, pingInterval, pingTimeout, maxMissedPongs)

	err = <-errc
	// Closing the pty releases the pty-side pump only. The client-side pump is blocked
	// in conn.Read on a socket and is freed by exactly one thing: cancelling the context
	// it was given, which closes the connection. That happens in the deferred teardown.
	_ = ptmx.Close()
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
	}
	return err
}

// runHubTerminal serves a named connection from a shared session hub: replay first, then
// live output, with input and resizes going back to the hub's single pty.
//
// The teardown difference from runTerminal is the entire point of the ticket. This function
// never touches the child process — it unsubscribes and returns, leaving the hub attached to
// the session with its buffer intact so the *next* client sees context instead of a blank
// screen. The hub is released when its session exits, when the warm cap evicts it, or when
// wtd stops.
func (s *server) runHubTerminal(parent context.Context, conn *websocket.Conn, hs handshake, args []string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// cancel is the drop hook: a client dropped for backlog is by definition stuck in
	// conn.Write, and only closing the connection releases it.
	h, sub, replay, err := s.hubs.join(args, hs.Columns, hs.Rows, cancel)
	if err != nil {
		return err
	}
	defer h.unsubscribe(sub)

	// The joining client's handshake size wins, per last-writer-wins. Both known clients
	// also send a RESIZE immediately after the handshake, so this mostly matters for the
	// window between the two.
	if err := h.resize(hs.Columns, hs.Rows); err != nil {
		log.Printf("wtd: resize on join of %q: %v", h.name, err)
	}

	// Frame order matches ttyd and the private path exactly: title, preferences, then
	// output. Replay is output — it goes after the frames describing the terminal it is
	// rendered into, never before.
	hostname, _ := os.Hostname()
	title := fmt.Sprintf("%s (%s)", h.name, hostname)
	if err := writeOp(ctx, conn, opTitle, []byte(title)); err != nil {
		return err
	}
	if err := writeOp(ctx, conn, opPrefs, prefsBody); err != nil {
		return err
	}

	// Chunked at the same size live output uses, so a client never sees a frame shape from
	// replay that it would not see from a busy session.
	for off := 0; off < len(replay); off += ptyReadChunk {
		end := off + ptyReadChunk
		if end > len(replay) {
			end = len(replay)
		}
		if err := writeOp(ctx, conn, opOutput, replay[off:end]); err != nil {
			return err
		}
	}

	errc := make(chan error, 2)

	// hub → client. Frames arrive pre-framed and are shared read-only with the other
	// subscribers, so this only writes them.
	go func() {
		for {
			select {
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			case <-sub.done:
				// The hub dropped us: its session ended, it was evicted, or this client
				// fell too far behind. The hub picks the status because only it knows
				// which — see api/ws-protocol.md section 13.
				//
				// Best-effort, deliberately: a backlog drop also cancels the context, so
				// both cases of this select are ready and Go picks one at random. Roughly
				// half of those clients get a bare cancellation instead of this reason,
				// which is fine — they are not reading anyway, which is why they were
				// dropped.
				code := sub.closeCode
				if code == 0 {
					code = websocket.StatusNormalClosure
				}
				conn.Close(code, closeReason(sub.reason))
				errc <- nil
				return
			case frame := <-sub.frames:
				err := conn.Write(ctx, websocket.MessageBinary, frame)
				// Accounted whether or not the write succeeded: on failure this
				// subscriber is finished anyway, and leaving the bytes outstanding would
				// make the backlog look permanently full.
				sub.queued.Add(-int64(len(frame)))
				if err != nil {
					errc <- err
					return
				}
			}
		}
	}()

	// client → hub
	go func() {
		errc <- s.pumpClient(ctx, conn, h)
	}()

	go keepAlive(ctx, conn, cancel, pingInterval, pingTimeout, maxMissedPongs)

	err = <-errc
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "") //nolint:errcheck // best-effort
	}
	return err
}

// terminalSink is where a client's frames go: a pty this connection owns alone, or a hub
// shared with every other client watching the same session. One implementation of the client
// frame parser serves both, so a protocol fix cannot reach one path and miss the other.
type terminalSink interface {
	input(p []byte) error
	resize(cols, rows int) error
}

// privatePty is the sink for an argless connection: its own pty, torn down with it.
type privatePty struct{ ptmx *os.File }

func (p privatePty) input(b []byte) error {
	if _, err := p.ptmx.Write(b); err != nil {
		return fmt.Errorf("pty write: %w", err)
	}
	return nil
}

func (p privatePty) resize(cols, rows int) error {
	return pty.Setsize(p.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// keepAlive probes the client and cancels the connection's context when it stops answering.
//
// Without this a client that vanished without closing its socket (phone in a lift, dead
// battery, NAT rebinding) holds the terminal until the kernel's TCP keepalive notices —
// tcp_keepalive_time, two hours by default. That pins a subscriber and the session's
// "attached" state, which is visible to the app via /api/v1/sessions. Real ttyd avoids this
// with -P (5s default).
//
// Ping/pong rather than an idle read deadline: a terminal can sit legitimately silent for
// hours, so absence of *user* traffic must not be treated as death.
//
// A single unanswered ping is not death either, which is why this counts to maxMissedPongs
// instead of giving up on the first one (#40). One lost pong is what a phone changing networks
// produces, and reaping on it drops a connection that would have recovered — while also making
// the real deadline 50s, under the 60s floor api/openapi.yaml publishes as a guarantee. The
// deadline is now pingInterval*maxMissedPongs, and it is the number the spec states.
// The three timing values are parameters rather than the consts directly so a test can drive the
// counting in under a second. Production passes the consts from both call sites; a separate test
// pins those against the deadline the spec publishes.
func keepAlive(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc,
	interval, timeout time.Duration, maxMissed int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	missed := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancelPing := context.WithTimeout(ctx, timeout)
			err := conn.Ping(pingCtx)
			cancelPing()

			if err == nil {
				missed = 0
				continue
			}
			// Distinguish "this probe went unanswered" from "the connection is gone". A
			// failed Ping is only evidence of silence when the socket is still usable;
			// once it is not, waiting out the remaining budget delays the teardown for no
			// benefit, because no later ping can succeed either.
			if ctx.Err() != nil || isDisconnect(err) {
				cancel()
				return
			}

			missed++
			if missed >= maxMissed {
				// Cancelling closes the connection, which unblocks both pumps and runs
				// the teardown for whichever path this is.
				log.Printf("wtd: no pong for %v (%d probes); closing", interval*time.Duration(maxMissed), missed)
				cancel()
				return
			}
		}
	}
}

// closeReason fits a message into a WebSocket close frame, which allows 123 bytes of reason.
// The browser page shows it verbatim; nothing parses it.
func closeReason(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// escalation is the signal ladder for ending a process group: SIGHUP, then SIGTERM, then SIGKILL,
// with the graces api/session-lifecycle.md §7 specifies.
//
// SIGHUP first because that is ttyd's default close signal and, for the real start command, it ends
// the dtach *client* — detaching and leaving the session running, which is the property the whole
// system depends on. A start command is user-editable bash, though, so a child that ignores SIGHUP
// must not be able to pin a process, an fd and a goroutine forever.
//
// Both places that end something use this one table: terminate below, for a terminal's own process
// group, and deleteSession, for a session's shell. The ladders were specified identically and there
// is no reason for them to drift.
var escalation = []struct {
	sig   syscall.Signal
	grace time.Duration
}{
	{syscall.SIGHUP, 2 * time.Second},
	{syscall.SIGTERM, 3 * time.Second},
	{syscall.SIGKILL, 2 * time.Second},
}

// terminate ends cmd's process group, escalating until it reaps, and never blocks
// indefinitely.
//
// The *group*, not the pid: creack/pty starts the child in a new session, so its process
// group id equals its pid, and signalling -pid reaches the grandchildren too. Signalling
// only the pid leaves anything the start command spawned running.
func terminate(cmd *exec.Cmd, waited <-chan error) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid

	for _, step := range escalation {
		// ESRCH here just means it already exited; the select below confirms the reap.
		_ = syscall.Kill(-pgid, step.sig)
		select {
		case <-waited:
			return
		case <-time.After(step.grace):
		}
	}

	// Past SIGKILL a process only survives in uninterruptible sleep. Leaking one parked
	// goroutine and saying so is better than blocking this connection's teardown forever.
	log.Printf("wtd: pid %d survived SIGKILL; abandoning its reaper goroutine", pgid)
}

func (s *server) pumpClient(ctx context.Context, conn *websocket.Conn, sink terminalSink) error {
	warnings := 0
	warn := func(format string, args ...any) {
		if warnings < maxFrameWarnings {
			log.Printf(format, args...)
			warnings++
			if warnings == maxFrameWarnings {
				log.Print("wtd: further malformed-frame warnings suppressed for this connection")
			}
		}
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			continue // ttyd tolerates empty frames; so do we
		}

		switch data[0] {
		case opInput:
			if err := sink.input(data[1:]); err != nil {
				return err
			}
		case opResize:
			var size struct {
				Columns int `json:"columns"`
				Rows    int `json:"rows"`
			}
			if err := json.Unmarshal(data[1:], &size); err != nil {
				// Malformed resize is ignored rather than fatal: dropping the whole
				// terminal over a bad resize would be a far worse failure than a
				// stale window size.
				warn("wtd: ignoring malformed resize %q", truncateBytes(data[1:], 80))
				continue
			}
			if size.Columns <= 0 || size.Rows <= 0 || size.Columns > maxDimension || size.Rows > maxDimension {
				continue
			}
			if err := sink.resize(size.Columns, size.Rows); err != nil {
				log.Printf("wtd: resize: %v", err)
			}
		case opPause, opResume:
			// Flow control. Neither known client sends these, and wtd applies no
			// backpressure of its own, so accepting and ignoring them keeps any client
			// that does send them working. Documented in api/ws-protocol.md.
		default:
			// Unknown opcodes are ignored, matching the permissive posture a relay
			// needs: a newer client sending something we do not understand should
			// degrade, not lose its terminal.
			warn("wtd: ignoring unknown client opcode %q", data[0])
		}
	}
}

func writeOp(ctx context.Context, conn *websocket.Conn, opcode byte, payload []byte) error {
	frame := make([]byte, 0, len(payload)+1)
	frame = append(frame, opcode)
	frame = append(frame, payload...)
	return conn.Write(ctx, websocket.MessageBinary, frame)
}

// isDisconnect reports whether an error is just a client going away, which is the normal
// end of every terminal session and not worth logging.
func isDisconnect(err error) bool {
	if err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) ||
		// "use of closed network connection" — what the abrupt-drop path produces once
		// teardown has closed the socket underneath a pump.
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure,
		websocket.StatusGoingAway,
		websocket.StatusAbnormalClosure,
		// 1005: no status received. Very common from a phone that backgrounds or loses
		// signal. Without it, every normal backgrounding logs an error and real faults
		// stop standing out.
		websocket.StatusNoStatusRcvd:
		return true
	}
	return false
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
