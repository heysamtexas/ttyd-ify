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

// expectClose reads until the connection closes and asserts the code. Any frame that
// arrives first is a failure worth reporting rather than skipping: it means the server
// spawned something for a handshake it should have rejected.
func expectClose(ctx context.Context, t *testing.T, conn *websocket.Conn, want websocket.StatusCode) {
	t.Helper()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if got := websocket.CloseStatus(err); got != want {
				t.Fatalf("close status = %v, want %v (error was %v)", got, want, err)
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
	expectClose(ctx, t, conn, websocket.StatusPolicyViolation)

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
			expectClose(ctx, t, conn, websocket.StatusProtocolError)
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
		expectClose(ctx, t, conn, websocket.StatusMessageTooBig)
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

	for _, want := range []string{
		// The handshake sentence, from the consts that implement it.
		fmt.Sprintf("Not-JSON closes **1002**; no handshake within **%d s** closes **1008**; over **%d KiB** closes **1009**.",
			int(handshakeTimeout.Seconds()), maxHandshakeBytes>>10),
		// The post-handshake ceiling, which was the one row #18 found already correct.
		fmt.Sprintf("Any message after the handshake over **%d MiB** closes **1009**", maxFrameBytes>>20),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the served /ws description does not contain:\n  %s\n"+
				"Either the consts in ws.go moved and api/openapi.yaml was not updated, or the\n"+
				"sentence was reworded — check the numbers agree, then update this test.", want)
		}
	}
}
