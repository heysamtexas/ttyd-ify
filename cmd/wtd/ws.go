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

	// Chunk size for pty → client. Larger reads mean fewer frames; this is well under
	// maxFrameBytes so a slow client never sees an oversized frame from us.
	ptyReadChunk = 16 * 1024

	// Liveness probing. The interval is far longer than ttyd's 5s default because the
	// cost of a slow reap here is one lingering session rather than anything unsafe, and
	// the iOS client already pings every 20s from its side. pingTimeout must stay under
	// the interval so probes cannot pile up.
	pingInterval = 30 * time.Second
	pingTimeout  = 20 * time.Second

	// Cap on per-connection warnings about malformed frames. An unauthenticated peer can
	// otherwise drive unbounded journald writes by spamming bad opcodes.
	maxFrameWarnings = 5
)

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

	if sub := conn.Subprotocol(); sub != "tty" {
		conn.Close(websocket.StatusPolicyViolation, "expected the tty subprotocol")
		return
	}

	conn.SetReadLimit(maxFrameBytes)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hs, err := readHandshake(ctx, conn)
	if err != nil {
		log.Printf("wtd: handshake from %s: %v", r.RemoteAddr, err)
		conn.Close(websocket.StatusPolicyViolation, "bad handshake")
		return
	}

	// ttyd appends every ?arg= value to the start command's argv, in order. bin/wt only
	// reads $1, but matching ttyd keeps other start commands working. These values are
	// never passed through a shell — exec takes them as argv — so quoting is not a
	// concern here; bin/wt does its own validation of the name it receives.
	args := r.URL.Query()["arg"]

	if err := s.runTerminal(ctx, conn, hs, args); err != nil && !isDisconnect(err) {
		log.Printf("wtd: terminal for %s: %v", r.RemoteAddr, err)
	}
}

// readHandshake reads the first message and parses it, accepting either frame type.
func readHandshake(ctx context.Context, conn *websocket.Conn) (handshake, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		return handshake{}, fmt.Errorf("read: %w", err)
	}

	var hs handshake
	if err := json.Unmarshal(data, &hs); err != nil {
		return handshake{}, fmt.Errorf("parse %q: %w", truncateBytes(data, 120), err)
	}

	// A client that omits or zeroes the dimensions would otherwise get a 0x0 pty, where
	// full-screen programs render nothing at all.
	if hs.Columns <= 0 || hs.Columns > 0xffff {
		hs.Columns = defaultCols
	}
	if hs.Rows <= 0 || hs.Rows > 0xffff {
		hs.Rows = defaultRows
	}
	return hs, nil
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
		return fmt.Errorf("start %s: %w", s.startCommand, err)
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

	// Frame order matches ttyd exactly: title, then preferences, then output. Measured
	// from the live server; a client that assumed a different order would already be
	// broken against real ttyd, but there is no reason to differ.
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
		buf := make([]byte, ptyReadChunk)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := writeOp(ctx, conn, opOutput, buf[:n]); werr != nil {
					errc <- werr
					return
				}
			}
			if err != nil {
				// EIO is how a pty reports "the child closed the other end", i.e. a
				// normal exit, not a failure.
				if errors.Is(err, io.EOF) || isEIO(err) {
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
		errc <- s.pumpClient(ctx, conn, ptmx)
	}()

	// Liveness. Without this a client that vanished without closing its socket (phone in
	// a lift, dead battery, NAT rebinding) holds the terminal until the kernel's TCP
	// keepalive notices — tcp_keepalive_time, two hours by default. That pins a wt
	// process, a dtach client, and the session's "attached" state, which is now visible
	// to the app via /api/v1/sessions. Real ttyd avoids this with -P (5s default).
	//
	// Ping/pong rather than an idle read deadline: a terminal can sit legitimately silent
	// for hours, so absence of *user* traffic must not be treated as death.
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, cancelPing := context.WithTimeout(ctx, pingTimeout)
				err := conn.Ping(pingCtx)
				cancelPing()
				if err != nil {
					// Cancelling closes the connection, which unblocks both pumps and
					// runs the teardown that ends the child.
					cancel()
					return
				}
			}
		}
	}()

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

// escalation is the signal ladder used to end a terminal's process group. SIGHUP first
// because that is ttyd's default close signal and, for the real start command, it ends the
// dtach *client* — detaching and leaving the session running, which is the property the
// whole system depends on. A start command is user-editable bash, though, so a child that
// ignores SIGHUP must not be able to pin a process, an fd and a goroutine forever.
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

func (s *server) pumpClient(ctx context.Context, conn *websocket.Conn, ptmx *os.File) error {
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
			if _, err := ptmx.Write(data[1:]); err != nil {
				return fmt.Errorf("pty write: %w", err)
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
			if size.Columns <= 0 || size.Rows <= 0 || size.Columns > 0xffff || size.Rows > 0xffff {
				continue
			}
			if err := pty.Setsize(ptmx, &pty.Winsize{
				Cols: uint16(size.Columns),
				Rows: uint16(size.Rows),
			}); err != nil {
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

func isEIO(err error) bool {
	return errors.Is(err, syscall.EIO)
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
