package main

import (
	"context"
	"encoding/json"
	"fmt"
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
