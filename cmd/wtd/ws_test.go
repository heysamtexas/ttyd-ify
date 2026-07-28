package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The handshake's three published failure modes. Each number and close code below is served
// to clients in /openapi.json and /docs/ws-protocol.md, so a client budgets against them —
// and all three were wrong at once (#18): the deadline was 30s not 10s, there was no size
// ceiling at all, and every failure closed 1008 including the ones specified as 1002.
//
// The mechanism and the value are tested separately on purpose. These tests shorten the
// deadline through server.handshakeWait, because waiting out the real one would add ten
// seconds of nothing to every run; TestHandshakeLimitsMatchTheirSpec is what holds the
// production values to the document.

// handshakeTestServer builds a server with a shortened handshake deadline. The rejection
// tests pass a start command that does not exist, which is itself an assertion: if one of
// them ever spawns anything, the journal says so loudly.
func handshakeTestServer(t *testing.T, wait time.Duration, startCommand string) string {
	t.Helper()
	app := newServer(startCommand)
	app.handshakeWait = wait
	t.Cleanup(app.hubs.closeAll)

	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	return srv.URL
}

const noStartCommand = "/nonexistent/start-command"

// expectClose reads until the connection closes and asserts the code, and the reason when one
// is given — api/ws-protocol.md publishes the reason strings, so they are part of the contract
// and not just debugging text. Any frame that arrives first is a failure worth reporting rather
// than skipping: it means the server spawned something for a handshake it should have rejected.
func expectClose(ctx context.Context, t *testing.T, conn *websocket.Conn, want websocket.StatusCode, wantReason string) {
	t.Helper()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if got := websocket.CloseStatus(err); got != want {
				t.Fatalf("close status = %v, want %v (error was %v)", got, want, err)
			}
			if wantReason != "" && !strings.Contains(err.Error(), wantReason) {
				t.Errorf("close reason does not contain %q; got %v", wantReason, err)
			}
			return
		}
		t.Fatalf("server sent a frame (%q) instead of closing %v", truncateBytes(data, 60), want)
	}
}

func TestHandshakeTimeoutClosesPolicyViolation(t *testing.T) {
	base := handshakeTestServer(t, 250*time.Millisecond, noStartCommand)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := dialTTY(ctx, base+"/ws")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck // best-effort teardown

	// Say nothing at all, which is the shape this rule exists for: a peer holding a slot
	// without ever using it.
	start := time.Now()
	// The reason string is published in api/ws-protocol.md's §5 table, so a client author can
	// tell a deliberate timeout from an unrelated 1008.
	expectClose(ctx, t, conn, websocket.StatusPolicyViolation, "handshake timeout")

	// Guards against the deadline being ignored and the close coming from something else
	// entirely — the read above would pass just as happily on an unrelated 1008.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("closed after %v, far past the configured 250ms: the handshake deadline is "+
			"not what closed this connection", elapsed)
	}
}

// The distinction 1002 draws is "you do not speak this protocol", against 1008's "you do,
// but you went quiet". Both told a client not to retry even before the fix, so the reason to
// get it right is the implementer reading a close code — which is exactly the audience the
// served close-code table is written for.
func TestUnparseableHandshakeClosesProtocolError(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"not json at all", "this is not json"},
		{"truncated json", `{"columns":80,`},
		{"a json array", `[80,25]`},
		{"a bare string", `"hello"`},
		// Valid JSON, and the only value that is "not an object" without also being
		// unparseable: it unmarshals into a struct without error and leaves every field
		// zero, so before this it was accepted and silently defaulted to 80x25.
		{"json null", `null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := handshakeTestServer(t, 10*time.Second, noStartCommand)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			conn, _, err := dialTTY(ctx, base+"/ws")
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow() //nolint:errcheck // best-effort teardown

			if err := conn.Write(ctx, websocket.MessageText, []byte(tc.payload)); err != nil {
				t.Fatalf("write: %v", err)
			}
			expectClose(ctx, t, conn, websocket.StatusProtocolError, "")
		})
	}
}

// The ceiling is enforced by the library's own read limit rather than by code here, which is
// the whole reason the fix is a one-line reorder — so this test is really asserting that the
// limit is applied to the *first* message and not only to later ones.
func TestOversizedHandshakeClosesMessageTooBig(t *testing.T) {
	// Padding inside AuthToken keeps the payload a structurally valid handshake, so a
	// failure here cannot be confused with the unparseable case above.
	padded := func(total int) []byte {
		envelope := len(fmt.Sprintf(`{"AuthToken":"","columns":%d,"rows":%d}`, 80, 25))
		return []byte(fmt.Sprintf(`{"AuthToken":"%s","columns":%d,"rows":%d}`,
			strings.Repeat("x", total-envelope), 80, 25))
	}

	// Asserted positively — the terminal starts and speaks — rather than by the close code
	// of a failed spawn: an off-by-one in the rejecting direction is the expensive mistake
	// here, so the test has to show a legitimate client getting through.
	t.Run("at the limit it is accepted", func(t *testing.T) {
		stub := writeStub(t, "printf 'READY\\n'\nsleep 600\n")
		base := handshakeTestServer(t, 10*time.Second, stub)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		conn, _, err := dialTTY(ctx, base+"/ws")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort teardown

		payload := padded(maxHandshakeBytes)
		if len(payload) != maxHandshakeBytes {
			t.Fatalf("built a %d-byte payload, wanted exactly %d", len(payload), maxHandshakeBytes)
		}
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatalf("write: %v", err)
		}

		if !readForMarker(ctx, conn, "READY", 10*time.Second) {
			t.Fatalf("a handshake of exactly %d bytes was refused; the ceiling is off by one "+
				"in the direction that rejects legitimate clients", maxHandshakeBytes)
		}
	})

	t.Run("one byte over it is refused", func(t *testing.T) {
		base := handshakeTestServer(t, 10*time.Second, noStartCommand)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		conn, _, err := dialTTY(ctx, base+"/ws")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort teardown

		if err := conn.Write(ctx, websocket.MessageText, padded(maxHandshakeBytes+1)); err != nil {
			// A write error is acceptable here: the server may close mid-write once the
			// limit trips. The close code is what matters, and Read reports it.
			t.Logf("write returned %v (the server closed during it)", err)
		}
		expectClose(ctx, t, conn, websocket.StatusMessageTooBig, "")
	})
}

// The consts and the served document must say the same thing, because #18 was three numbers
// that disagreed while CI stayed green. Modelled on TestHealthzMatchesItsSpec: read the
// embedded spec rather than restating its values here, so this fails when either side moves.
//
// Prose cannot be validated structurally, so this asserts the exact published sentences,
// rebuilt from the consts. A reword breaks it deliberately — that sentence is the contract,
// and whoever rewords it should confirm the numbers still match the code.
func TestHandshakeLimitsMatchTheirSpec(t *testing.T) {
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Description string `json:"description"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}

	// YAML folding decides where the served description wraps, so compare on collapsed
	// whitespace: a re-fold is not a contract change, but a changed number is.
	collapse := regexp.MustCompile(`\s+`)
	got := collapse.ReplaceAllString(spec.Paths["/ws"].Get.Description, " ")
	if got == "" {
		t.Fatal("the embedded spec has no /ws description; this test is checking nothing")
	}

	// Every number appears twice in this description — once in the handshake paragraph and
	// again in the close-code table — so pinning one and not the other leaves half the document
	// free to drift. That is not hypothetical: the table is where #18 found two of the three
	// wrong values.
	for _, want := range []string{
		// The handshake paragraph, from the consts that implement it.
		fmt.Sprintf("Not-JSON closes **1002**; no handshake within **%d s** closes **1008**; over **%d KiB** closes **1009**.",
			int(handshakeTimeout.Seconds()), maxHandshakeBytes>>10),
		// The post-handshake ceiling, which was the one row #18 found already correct.
		fmt.Sprintf("Any message after the handshake over **%d MiB** closes **1009**", maxFrameBytes>>20),
		// The same three facts again, as the close-code table states them.
		fmt.Sprintf("| `1008` | No handshake within %d s. |", int(handshakeTimeout.Seconds())),
		fmt.Sprintf("| `1009` | A message exceeded %d KiB (handshake) or %d MiB (after). |",
			maxHandshakeBytes>>10, maxFrameBytes>>20),
		"| `1002` | The first message was not a valid handshake. |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the served /ws description does not contain:\n  %s\n"+
				"Either the consts in ws.go moved and api/openapi.yaml was not updated, or the\n"+
				"sentence was reworded — check the numbers agree, then update this test.", want)
		}
	}

	// The const is what the spec publishes; this is what a real server actually waits. Pinning
	// only the const would let newServer's default drift away from the documented number while
	// every assertion above stayed green.
	if got := newServer(noStartCommand).handshakeWait; got != handshakeTimeout {
		t.Errorf("newServer sets handshakeWait to %v, but the spec publishes %v — the field the "+
			"server runs on must be the value the document promises", got, handshakeTimeout)
	}
}

// The tighter handshake ceiling is raised back to maxFrameBytes once the handshake parses, and
// nothing else in the suite sends a post-handshake message big enough to notice if that line
// disappears. It is a new failure mode: before the ceiling existed the limit was 1 MiB from the
// upgrade onward, so deleting the raise now kills a live session on a large paste and
// contradicts the 1 MiB the spec publishes.
func TestPostHandshakeFramesAreNotHeldToTheHandshakeCeiling(t *testing.T) {
	// Echo back what arrives so the assertion is that the frame was *processed*, not merely
	// that the connection stayed up.
	stub := writeStub(t, "printf 'READY\\n'\ncat\n")
	base := handshakeTestServer(t, 10*time.Second, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := dialTTY(ctx, base+"/ws")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck // best-effort teardown

	if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if !readForMarker(ctx, conn, "READY", 10*time.Second) {
		t.Fatal("the stub never started")
	}

	// Comfortably over maxHandshakeBytes and comfortably under maxFrameBytes: the band that
	// only works because the limit was raised.
	paste := append([]byte{opInput}, []byte(strings.Repeat("p", 64<<10)+"\n")...)
	if err := conn.Write(ctx, websocket.MessageBinary, paste); err != nil {
		t.Fatalf("a %d-byte INPUT frame was rejected: %v — the handshake ceiling was never "+
			"raised back to maxFrameBytes, so a large paste now kills the session", len(paste), err)
	}
	if !readForMarker(ctx, conn, strings.Repeat("p", 1024), 10*time.Second) {
		t.Fatalf("the %d-byte paste never came back through the terminal", len(paste))
	}
}

// An established session must survive its own handshake deadline passing. Nothing else in the
// suite outlives its handshake wait, so without this the deadline could start killing live
// connections and every other test would stay green.
//
// Scope, measured rather than assumed: `timer.Stop()` and readHandshake's `read` flag are each
// *individually* sufficient here, so this fails only when both are removed. The flag is not
// redundant — it covers the case Stop cannot, where the timer has already fired by the time the
// read returns, and that window is sub-millisecond (probed at 600 connections aimed at the
// deadline, zero hits). No test can reach it; the flag is there by construction and this test
// pins the mechanism it can actually observe.
func TestHandshakeDeadlineIsDisarmedOnceTheHandshakeArrives(t *testing.T) {
	// The session speaks again a full second after the 250ms deadline. Liveness is asserted by
	// that second line arriving, not by a ping: coder/websocket's Ping needs a concurrent
	// Reader to see the pong ("Ping must be called concurrently with Reader", conn.go), and a
	// test that just sleeps has none — it would report a dead connection either way.
	stub := writeStub(t, "printf 'READY\\n'\nsleep 1\nprintf 'STILL-ALIVE\\n'\nsleep 600\n")
	base := handshakeTestServer(t, 250*time.Millisecond, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := dialTTY(ctx, base+"/ws")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck // best-effort teardown

	if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if !readForMarker(ctx, conn, "READY", 10*time.Second) {
		t.Fatal("the stub never started")
	}
	if !readForMarker(ctx, conn, "STILL-ALIVE", 10*time.Second) {
		t.Fatal("the connection died after its handshake deadline passed: the timer fired " +
			"against an established session, so a client whose handshake landed near the " +
			"deadline gets 1008 with a session already running behind it")
	}
}

// A spawn failure used to drop the TCP connection with no close frame, so a client saw a bare
// transport error and retried into the identical failure (#34). api/openapi.yaml publishes 1011
// for it, plus a one-line reason as an OUTPUT frame so the cause is visible in the terminal
// rather than only in a code.
//
// Both paths, because they fail in different places: the private path fails at
// pty.StartWithSize inside runTerminal, while the hub path fails inside hubs.join before any
// subscriber exists — which is why the close is sent by the connection handler rather than
// through the hub's own broadcast machinery, and why testing only one would prove little about
// the other.
func TestSpawnFailureClosesInternalErrorWithAReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		arg  string
	}{
		{"argless, private pty", ""},
		{"named, via a hub", "some-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := handshakeTestServer(t, 10*time.Second, noStartCommand)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			u := base + "/ws"
			if tc.arg != "" {
				u += "?arg=" + tc.arg
			}
			conn, _, err := dialTTY(ctx, u)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow() //nolint:errcheck // best-effort teardown

			if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
				t.Fatalf("handshake: %v", err)
			}

			// Collect the frames, in order, until the close. Order is the assertion as much as
			// the content: the error must not arrive before the title, or a client that treats
			// frame 1 as its window title shows the message there and never prints it.
			var opcodes []byte
			var output strings.Builder
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
						t.Fatalf("close status = %v, want %v (a codeless drop is the bug this "+
							"guards; error was %v)", got, websocket.StatusInternalError, err)
					}
					break
				}
				if len(data) == 0 {
					continue
				}
				opcodes = append(opcodes, data[0])
				if data[0] == opOutput {
					output.Write(data[1:])
				}
			}

			if want := []byte{opTitle, opPrefs, opOutput}; string(opcodes) != string(want) {
				t.Errorf("frames arrived as %q, want %q — the documented order is title, "+
					"preferences, then output, on this path too", opcodes, want)
			}
			// The reason has to name the failure, not just be non-empty: "could not start a
			// terminal" with no cause would leave the operator no better off than the code did.
			for _, want := range []string{"could not start a terminal", noStartCommand} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("the terminal error %q does not mention %q", output.String(), want)
				}
			}
		})
	}
}

// Liveness (#40). keepAlive used to give up on the *first* unanswered ping, which made the real
// reap deadline 50 s — under the 60 s floor api/openapi.yaml publishes as a guarantee, and short
// enough that one lost pong from a phone changing networks cost it the connection.
//
// Driven at 150 ms rather than 30 s, because the point is the counting. The values the deadline
// is actually built from are pinned separately, below.
func TestKeepAliveToleratesMissedPongsBeforeReaping(t *testing.T) {
	const (
		interval   = 150 * time.Millisecond
		timeout    = 100 * time.Millisecond
		maxMissed  = 3
		wantReap   = interval * maxMissed
		serverGone = 5 * time.Second
	)

	reaped := make(chan time.Duration, 1)
	start := make(chan time.Time, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"tty"}})
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort teardown

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		began := time.Now()
		start <- began
		// Synchronously, so the handler is still alive when cancel fires and the connection
		// has not been torn down under it.
		keepAlive(ctx, conn, cancel, interval, timeout, maxMissed)
		reaped <- time.Since(began)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := dialTTY(ctx, srv.URL+"/ws")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck // best-effort teardown

	// Never reading is what suppresses the pongs: coder/websocket auto-pongs from inside Read,
	// so a client that does not read looks exactly like one whose transport has stopped
	// answering — the case this deadline exists for.
	<-start

	select {
	case elapsed := <-reaped:
		// The floor is the assertion. One interval means the old give-up-on-first-miss
		// behaviour is back, and no amount of tolerance in the upper bound catches that.
		if elapsed < 2*interval {
			t.Fatalf("reaped after %v, less than two probe intervals: a single missed pong is "+
				"ending connections again, which puts the real deadline under the 60 s floor "+
				"the spec publishes", elapsed)
		}
		// Generous upper bound — this is a timing test on shared CI hardware, and being late
		// is not the bug being guarded.
		if elapsed > wantReap+2*time.Second {
			t.Errorf("reaped after %v, far past the expected %v: the counter may not be "+
				"resetting or the interval is not being honoured", elapsed, wantReap)
		}
		t.Logf("reaped after %v (expected ~%v for %d missed probes)", elapsed, wantReap, maxMissed)
	case <-time.After(serverGone):
		t.Fatal("keepAlive never reaped a client that answered no pongs at all")
	}
}

// The deadline is a product of two consts and a promise made in prose, so neither const can move
// without the published guarantee being rechecked. This is the half of #40 that stops it recurring:
// the numbers were not wrong by accident, they were wrong because nothing connected them.
func TestLivenessDeadlineHonoursItsPublishedFloor(t *testing.T) {
	const publishedFloor = 60 * time.Second

	if deadline := pingInterval * maxMissedPongs; deadline <= publishedFloor {
		t.Errorf("the reap deadline is %v, which is not above the %v floor api/openapi.yaml "+
			"guarantees (\"There is no idle timeout below 60 s\")", deadline, publishedFloor)
	}
	// A ping that outlives its own interval would let probes pile up, and the missed-pong count
	// would then measure something other than elapsed time.
	if pingTimeout >= pingInterval {
		t.Errorf("pingTimeout %v must stay under pingInterval %v or probes overlap and the "+
			"missed count no longer corresponds to %v of silence",
			pingTimeout, pingInterval, pingInterval*maxMissedPongs)
	}

	var spec struct {
		Paths map[string]struct {
			Get struct {
				Description string `json:"description"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}
	got := regexp.MustCompile(`\s+`).ReplaceAllString(spec.Paths["/ws"].Get.Description, " ")

	for _, want := range []string{
		fmt.Sprintf("It pings every %d s", int(pingInterval.Seconds())),
		fmt.Sprintf("so a live connection has %d s of tolerance", int((pingInterval * maxMissedPongs).Seconds())),
		fmt.Sprintf("There is no idle timeout below %d s", int(publishedFloor.Seconds())),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the served /ws description does not contain:\n  %s\n"+
				"The liveness consts and the document have drifted apart again.", want)
		}
	}
}

// ptr is for the one case that needs a header no API can express: a present but empty
// Sec-WebSocket-Protocol.
func ptr[T any](v T) *T { return &v }

// Subprotocol negotiation (#36). Both served rules were false at once, and the failure they
// produced actively misdirected the client author who hit it: a client offering nothing was closed
// 1008, and 1008's only documented meaning is "no handshake within 10 s" — which it had never been
// given the chance to send.
//
// The expected values here are ttyd 1.7.4's, measured rather than assumed (probed 2026-07-28, and
// recorded in api/compatibility.md). Wire compatibility is the reason the no-subprotocol case is a
// bug rather than a preference: a hand-rolled client that worked against ttyd did not work here.
func TestSubprotocolNegotiation(t *testing.T) {
	stub := writeStub(t, "printf 'READY\\n'\nsleep 600\n")
	base := handshakeTestServer(t, 10*time.Second, stub)

	// Dialing by hand rather than through dialTTY: the point is controlling the offer exactly.
	//
	// Offers go through DialOptions.Subprotocols rather than a raw header, because the library
	// verifies the echo against what it asked for and rejects a subprotocol it did not request —
	// setting the header directly makes even a correct server look like a protocol violation.
	// rawHeader exists only for the empty-header case, which no API can express.
	dial := func(t *testing.T, offer []string, rawHeader *string) (*http.Response, *websocket.Conn) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)

		opts := &websocket.DialOptions{Subprotocols: offer, HTTPHeader: http.Header{}}
		if rawHeader != nil {
			opts.HTTPHeader.Set("Sec-WebSocket-Protocol", *rawHeader)
		}
		conn, resp, err := websocket.Dial(ctx, strings.Replace(base, "http://", "ws://", 1)+"/ws", opts)
		if err != nil && resp == nil {
			t.Fatalf("dial: %v", err)
		}
		if err != nil {
			t.Logf("dial returned %v (status %s)", err, resp.Status)
		}
		return resp, conn
	}

	t.Run("tty is echoed", func(t *testing.T) {
		resp, conn := dial(t, []string{"tty"}, nil)
		if conn == nil {
			t.Fatalf("offering tty was refused with %s", resp.Status)
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort teardown
		if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "tty" {
			t.Fatalf("negotiated subprotocol = %q, want %q — browsers fail the connection when a "+
				"requested subprotocol is not echoed", got, "tty")
		}
	})

	// The regression that matters: this used to be closed 1008. Asserted by the terminal actually
	// starting, because a connection that upgrades and is then closed would pass a weaker check.
	t.Run("offering nothing is accepted and proceeds", func(t *testing.T) {
		resp, conn := dial(t, nil, nil)
		if conn == nil {
			t.Fatalf("offering no subprotocol was refused with %s — ttyd accepts it, so this "+
				"breaks a client that works against ttyd", resp.Status)
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort teardown

		if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "" {
			t.Errorf("echoed subprotocol %q to a client that offered none", got)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
			t.Fatalf("handshake on a subprotocol-less connection: %v", err)
		}
		if !readForMarker(ctx, conn, "READY", 10*time.Second) {
			t.Fatal("a connection that offered no subprotocol never got a terminal: it was " +
				"upgraded and then closed, which is the 1008 bug this replaced")
		}
	})

	// An empty header is "offered nothing", not "offered something unknown" — ttyd accepts it, and
	// the refusal below must not swallow it.
	t.Run("an empty header counts as offering nothing", func(t *testing.T) {
		resp, conn := dial(t, nil, ptr(""))
		if conn == nil {
			t.Fatalf("an empty Sec-WebSocket-Protocol was refused with %s", resp.Status)
		}
		_ = conn.CloseNow()
	})

	for _, tc := range []struct {
		name  string
		offer []string
	}{
		{"a single other subprotocol", []string{"chat"}},
		{"several, none of them tty", []string{"chat", "mqtt"}},
	} {
		t.Run(tc.name+" is refused with 400", func(t *testing.T) {
			resp, conn := dial(t, tc.offer, nil)
			if conn != nil {
				_ = conn.CloseNow()
				t.Fatalf("offering %q was upgraded; a client speaking something else must not be "+
					"handed a shell stream", tc.offer)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("offering %q got %s, want 400 before the upgrade — ttyd drops the TCP "+
					"connection here, which a client cannot tell from a network failure",
					tc.offer, resp.Status)
			}
		})
	}

	// tty among others is still tty: the rule is "offered tty", not "offered only tty".
	t.Run("tty alongside others is accepted", func(t *testing.T) {
		resp, conn := dial(t, []string{"chat", "tty"}, nil)
		if conn == nil {
			t.Fatalf("offering %q was refused with %s", "chat, tty", resp.Status)
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort teardown
		if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "tty" {
			t.Fatalf("negotiated %q, want tty", got)
		}
	})
}

// The handshake's dimension contract (#37). §5's row was wrong three ways at once: it promised
// defaults for a non-numeric dimension (the server closes 1002), said 80x24 (it is 80x25), and
// bounded the range at 1..9999 (it is 1..65535). Two of the three were published in
// api/openapi.yaml, where a client budgets against them — a client author reading "80x24" builds a
// 24-row fallback for a 25-row terminal.
func TestHandshakeDimensionContract(t *testing.T) {
	t.Run("the published defaults are the code's", func(t *testing.T) {
		var spec struct {
			Paths map[string]struct {
				Get struct {
					Description string `json:"description"`
				} `json:"get"`
			} `json:"paths"`
		}
		if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
			t.Fatalf("decode the embedded spec: %v", err)
		}
		got := regexp.MustCompile(`\s+`).ReplaceAllString(spec.Paths["/ws"].Get.Description, " ")

		for _, want := range []string{
			fmt.Sprintf("default to **%dx%d silently**", defaultCols, defaultRows),
			fmt.Sprintf("outside\n        **1..%d**", maxDimension),
		} {
			flat := regexp.MustCompile(`\s+`).ReplaceAllString(want, " ")
			if !strings.Contains(got, flat) {
				t.Errorf("the served /ws description does not contain %q — the dimension consts "+
					"and the document have drifted apart", flat)
			}
		}
	})

	// Out of range defaults; not a number closes. The boundary is asserted from both sides
	// because 65535 is a legitimate value a client may send and 65536 is not.
	for _, tc := range []struct {
		name    string
		payload string
		wantCol int // 0 means "expect a 1002 close"
		wantRow int
	}{
		{"real dimensions are used as sent", `{"AuthToken":"","columns":132,"rows":43}`, 132, 43},
		{"absent dimensions default", `{"AuthToken":""}`, defaultCols, defaultRows},
		{"zero defaults", `{"AuthToken":"","columns":0,"rows":0}`, defaultCols, defaultRows},
		{"negative defaults", `{"AuthToken":"","columns":-1,"rows":-1}`, defaultCols, defaultRows},
		{"at the top of the range it is used", `{"AuthToken":"","columns":65535,"rows":65535}`, maxDimension, maxDimension},
		{"one past the range defaults", `{"AuthToken":"","columns":65536,"rows":65536}`, defaultCols, defaultRows},
		{"a string dimension closes 1002", `{"AuthToken":"","columns":"80","rows":25}`, 0, 0},
		{"a fractional dimension closes 1002", `{"AuthToken":"","columns":80.5,"rows":25}`, 0, 0},
		{"a boolean dimension closes 1002", `{"AuthToken":"","columns":true,"rows":25}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The stub reports the pty's real size, so the assertion is what the child saw
			// rather than what the server parsed.
			stub := writeStub(t, "printf 'SIZE:%s\\n' \"$(stty size < /dev/tty | tr ' ' 'x')\"\nsleep 600\n")
			base := handshakeTestServer(t, 10*time.Second, stub)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			conn, _, err := dialTTY(ctx, base+"/ws")
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow() //nolint:errcheck // best-effort teardown

			if err := conn.Write(ctx, websocket.MessageText, []byte(tc.payload)); err != nil {
				t.Fatalf("write: %v", err)
			}

			if tc.wantCol == 0 {
				expectClose(ctx, t, conn, websocket.StatusProtocolError, "")
				return
			}
			// stty reports rows then columns.
			want := fmt.Sprintf("SIZE:%dx%d", tc.wantRow, tc.wantCol)
			out := readUntil(ctx, t, conn, "SIZE:", 10*time.Second)
			flat := strings.NewReplacer("\r", "", "\n", "").Replace(out)
			if !strings.Contains(flat, want) {
				t.Fatalf("the child's pty was %q, want %q", flat, want)
			}
		})
	}
}
