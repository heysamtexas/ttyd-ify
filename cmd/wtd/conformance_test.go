package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Wire-protocol conformance against real ttyd.
//
// This is the assertion the whole wtd plan rests on: the iOS client in
// ~/src/ios-claude-terminal speaks ttyd's protocol directly, so if wtd matches ttyd
// observably, that client works unchanged and cutover/rollback are free.
//
// Both servers are given test/stub-start-command.sh rather than bin/wt, so these tests
// exercise frames only — no dtach, no sockets, nothing that could touch ~/.dtach or the
// live service.
//
// One caveat since replay shipped: TestConformanceURLArg dials with ?arg=, which on wtd
// creates a *held* hub that outlives the connection — so against a long-lived server it
// leaves one start-command process running until the warm cap evicts it or the server stops.
// Harmless for the scratch instances CI and the recipe above use, but it is no longer true
// that these tests leave nothing behind.
//
// Run with:
//
//	CONFORMANCE_TTYD=http://127.0.0.1:7686 go test ./cmd/wtd -run Conformance -v
//	CONFORMANCE_TTYD=... CONFORMANCE_WTD=... go test ./cmd/wtd -run Conformance -v
//
// With only CONFORMANCE_TTYD set it characterizes ttyd and logs the baseline. With both
// set it compares them. Skips entirely when neither is set, so plain `go test` needs no
// running servers.

// Opcodes come from ws.go rather than being re-declared here. A second copy in the one
// file whose entire job is catching wire drift would mean a fix in one place silently not
// reaching the other — exactly the failure this harness exists to prevent.
const (
	opClientInput  = opInput
	opClientResize = opResize

	opServerOutput = opOutput
	opServerTitle  = opTitle
	opServerPrefs  = opPrefs
)

type observed struct {
	tokenBody   string
	subprotocol string
	// opcodes seen from the server, in first-seen order
	opcodes []byte
	// first payload seen per opcode — this is how the title and preferences frame
	// contents get pinned down, since neither is documented anywhere
	payloads map[byte]string
	// concatenated payloads of output frames
	output string
}

func (o observed) sawOpcode(b byte) bool {
	for _, c := range o.opcodes {
		if c == b {
			return true
		}
	}
	return false
}

func TestConformance(t *testing.T) {
	ttydURL := os.Getenv("CONFORMANCE_TTYD")
	wtdURL := os.Getenv("CONFORMANCE_WTD")

	if ttydURL == "" && wtdURL == "" {
		t.Skip("set CONFORMANCE_TTYD and/or CONFORMANCE_WTD to run wire conformance")
	}

	var ttydObs, wtdObs observed

	if ttydURL != "" {
		t.Run("characterize/ttyd", func(t *testing.T) {
			ttydObs = characterize(t, ttydURL)
			logObserved(t, "ttyd", ttydObs)
		})
	}
	if wtdURL != "" {
		t.Run("characterize/wtd", func(t *testing.T) {
			wtdObs = characterize(t, wtdURL)
			logObserved(t, "wtd", wtdObs)
		})
	}

	if ttydURL == "" || wtdURL == "" {
		t.Log("only one server given — characterization only, no comparison")
		return
	}

	t.Run("compare/token-byte-exact", func(t *testing.T) {
		if ttydObs.tokenBody != wtdObs.tokenBody {
			t.Errorf("/token differs:\n  ttyd = %q\n  wtd  = %q", ttydObs.tokenBody, wtdObs.tokenBody)
		}
	})

	t.Run("compare/subprotocol", func(t *testing.T) {
		if ttydObs.subprotocol != wtdObs.subprotocol {
			t.Errorf("negotiated subprotocol differs: ttyd = %q, wtd = %q",
				ttydObs.subprotocol, wtdObs.subprotocol)
		}
	})

	// These were previously only logged, which meant three claims in the code were
	// eyeballed rather than tested. A wrong TERM is the nastiest of them: colors and key
	// handling break on the phone with no error emitted anywhere.
	t.Run("compare/child-environment-and-pty-size", func(t *testing.T) {
		for _, want := range []string{
			"TERM:[xterm-256color]", // ttyd's -T default; wtd hardcodes the same
			"INITSIZE:25x80",        // handshake dimensions must reach the pty before the child's first write
		} {
			if !strings.Contains(ttydObs.output, want) {
				t.Errorf("ttyd output lacks %q — the baseline moved, re-derive it: %q", want, ttydObs.output)
			}
			if !strings.Contains(wtdObs.output, want) {
				t.Errorf("wtd output lacks %q (ttyd produces it)\n  wtd: %q", want, wtdObs.output)
			}
		}
	})

	t.Run("compare/title-frame", func(t *testing.T) {
		// Both servers are given a byte-identical start-command path by the harness, so
		// the payloads must match exactly. If they ever differ for a legitimate reason,
		// assert the shape rather than deleting the check.
		if ttydObs.payload(opServerTitle) != wtdObs.payload(opServerTitle) {
			t.Errorf("title frame differs:\n  ttyd = %q\n  wtd  = %q",
				ttydObs.payload(opServerTitle), wtdObs.payload(opServerTitle))
		}
	})

	t.Run("compare/preferences-frame", func(t *testing.T) {
		// ttyd emits literally "{ }" with no -t options. encoding/json would emit "{}",
		// so this catches a well-meaning "cleanup" of prefsBody.
		if ttydObs.payload(opServerPrefs) != wtdObs.payload(opServerPrefs) {
			t.Errorf("preferences frame differs:\n  ttyd = %q\n  wtd  = %q",
				ttydObs.payload(opServerPrefs), wtdObs.payload(opServerPrefs))
		}
	})

	t.Run("compare/server-opcode-order", func(t *testing.T) {
		// Order IS asserted, and ws.go claims it matches — those two statements used to
		// disagree, which is worse than either choice.
		//
		// It is deterministic in both servers by construction: ttyd emits title and
		// preferences on connect before the child produces anything, and wtd writes both
		// before starting its pty pump. Measured "120" on ttyd 1.7.4. If this ever goes
		// flaky, the fix is to assert that 1 and 2 precede the first 0, not to delete the
		// check — a client cannot receive output before the frames that describe the
		// terminal it is rendering into.
		if got, want := string(wtdObs.opcodes), string(ttydObs.opcodes); got != want {
			t.Errorf("server frame order differs: wtd sent %q, ttyd sent %q", got, want)
		}
	})

	t.Run("compare/server-opcodes", func(t *testing.T) {
		for _, op := range ttydObs.opcodes {
			if !wtdObs.sawOpcode(op) {
				t.Errorf("ttyd sent opcode %q but wtd never did", op)
			}
		}
		for _, op := range wtdObs.opcodes {
			if !ttydObs.sawOpcode(op) {
				t.Errorf("wtd sent opcode %q but ttyd never did — an extension the iOS client will treat as unknown", op)
			}
		}
	})
}

// TestConformanceHandshakeFrameType covers a real trap: ttyd's own web client sends the
// JSON handshake as a BINARY frame (textEncoder.encode), while the iOS client sends it as
// a TEXT frame (.string). Both work against ttyd, so wtd must accept either or it breaks
// one of the two clients.
func TestConformanceHandshakeFrameType(t *testing.T) {
	for _, target := range conformanceTargets(t) {
		for _, typ := range []struct {
			name string
			mt   websocket.MessageType
		}{
			{"text", websocket.MessageText},
			{"binary", websocket.MessageBinary},
		} {
			t.Run(target.name+"/"+typ.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()

				conn, _, err := dialTTY(ctx, target.url)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				defer conn.Close(websocket.StatusNormalClosure, "")

				if err := conn.Write(ctx, typ.mt, handshakeJSON(80, 25)); err != nil {
					t.Fatalf("write handshake as %s frame: %v", typ.name, err)
				}

				// The proof the handshake was accepted is that the start command runs
				// and its output comes back.
				got := readUntil(ctx, t, conn, "ARGV:", 10*time.Second)
				if !strings.Contains(got, "ARGV:") {
					t.Errorf("handshake as %s frame produced no start-command output; got %q", typ.name, got)
				}
			})
		}
	}
}

// TestConformanceURLArg checks that ?arg=<v> reaches the start command as $1 — the
// mechanism behind every saved deep-link profile in the iOS app.
func TestConformanceURLArg(t *testing.T) {
	for _, target := range conformanceTargets(t) {
		t.Run(target.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			conn, _, err := dialTTY(ctx, target.url+"/ws?arg=demo-session")
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "")

			if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
				t.Fatalf("handshake: %v", err)
			}

			got := readUntil(ctx, t, conn, "ARGV:", 10*time.Second)
			if !strings.Contains(got, "ARGV:[demo-session]") {
				t.Errorf("?arg=demo-session did not arrive as $1\n  got: %q", got)
			}
		})
	}
}

// TestConformanceInputAndResize checks the two client→server frames that matter: input
// relay, and whether a resize frame actually reaches the pty (the stub reports
// `stty size`, so a wrong answer means the pty was never resized).
func TestConformanceInputAndResize(t *testing.T) {
	for _, target := range conformanceTargets(t) {
		t.Run(target.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			conn, _, err := dialTTY(ctx, target.url)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "")

			if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
				t.Fatalf("handshake: %v", err)
			}
			readUntil(ctx, t, conn, "ARGV:", 10*time.Second)

			if err := writeFrame(ctx, conn, opClientInput, []byte("hello\n")); err != nil {
				t.Fatalf("write input: %v", err)
			}
			if got := readUntil(ctx, t, conn, "ECHO:hello", 10*time.Second); !strings.Contains(got, "ECHO:hello") {
				t.Errorf("input was not relayed to the pty; got %q", got)
			}

			resize, _ := json.Marshal(map[string]int{"columns": 100, "rows": 30})
			if err := writeFrame(ctx, conn, opClientResize, resize); err != nil {
				t.Fatalf("write resize: %v", err)
			}
			// Give the winch a moment to land before asking the pty its size.
			time.Sleep(300 * time.Millisecond)
			if err := writeFrame(ctx, conn, opClientInput, []byte("size\n")); err != nil {
				t.Fatalf("write size probe: %v", err)
			}
			if got := readUntil(ctx, t, conn, "SIZE:", 10*time.Second); !strings.Contains(got, "SIZE:30x100") {
				t.Errorf("resize did not reach the pty, want SIZE:30x100\n  got: %q", got)
			}
		})
	}
}

// --- helpers ---------------------------------------------------------------

type target struct{ name, url string }

func conformanceTargets(t *testing.T) []target {
	t.Helper()
	var out []target
	if u := os.Getenv("CONFORMANCE_TTYD"); u != "" {
		out = append(out, target{"ttyd", u})
	}
	if u := os.Getenv("CONFORMANCE_WTD"); u != "" {
		out = append(out, target{"wtd", u})
	}
	if len(out) == 0 {
		t.Skip("set CONFORMANCE_TTYD and/or CONFORMANCE_WTD to run wire conformance")
	}
	return out
}

func handshakeJSON(cols, rows int) []byte {
	// Field names and shape are ttyd's, via TtydProtocol.swift's Handshake struct.
	return []byte(fmt.Sprintf(`{"AuthToken":"","columns":%d,"rows":%d}`, cols, rows))
}

func dialTTY(ctx context.Context, base string) (*websocket.Conn, *http.Response, error) {
	wsURL := base
	if !strings.Contains(wsURL, "/ws") {
		wsURL = strings.TrimRight(base, "/") + "/ws"
	}
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"tty"},
	})
}

func writeFrame(ctx context.Context, conn *websocket.Conn, opcode byte, payload []byte) error {
	return conn.Write(ctx, websocket.MessageBinary, append([]byte{opcode}, payload...))
}

// readUntil accumulates output-frame payloads until marker appears or it times out.
// Returns whatever it collected, so a caller can report what actually arrived.
func readUntil(ctx context.Context, t *testing.T, conn *websocket.Conn, marker string, timeout time.Duration) string {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var sb strings.Builder
	for {
		_, data, err := conn.Read(deadline)
		if err != nil {
			return sb.String()
		}
		if len(data) == 0 {
			continue
		}
		if data[0] == opServerOutput {
			sb.Write(data[1:])
			if strings.Contains(sb.String(), marker) {
				return sb.String()
			}
		}
	}
}

func characterize(t *testing.T, base string) observed {
	t.Helper()
	obs := observed{}

	resp, err := http.Get(strings.TrimRight(base, "/") + "/token")
	if err != nil {
		t.Fatalf("GET /token: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	obs.tokenBody = string(body)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, httpResp, err := dialTTY(ctx, base)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if httpResp != nil {
		obs.subprotocol = httpResp.Header.Get("Sec-WebSocket-Protocol")
	}

	if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// Collect frames for a bounded window rather than until a marker, so incidental
	// frames (title, preferences) are observed too.
	window, cancelWindow := context.WithTimeout(ctx, 4*time.Second)
	defer cancelWindow()
	var out strings.Builder
	seen := map[byte]bool{}
	obs.payloads = map[byte]string{}
	for {
		_, data, err := conn.Read(window)
		if err != nil {
			break
		}
		if len(data) == 0 {
			continue
		}
		if !seen[data[0]] {
			seen[data[0]] = true
			obs.opcodes = append(obs.opcodes, data[0])
			obs.payloads[data[0]] = string(data[1:])
		}
		if data[0] == opServerOutput {
			out.Write(data[1:])
		}
	}
	obs.output = out.String()
	return obs
}

func (o observed) payload(op byte) string { return o.payloads[op] }

func logObserved(t *testing.T, name string, o observed) {
	t.Helper()
	t.Logf("%s: /token = %q", name, o.tokenBody)
	t.Logf("%s: subprotocol = %q", name, o.subprotocol)
	t.Logf("%s: server opcodes seen = %q", name, string(o.opcodes))
	for _, op := range o.opcodes {
		if op != opServerOutput {
			t.Logf("%s:   opcode %q payload = %q", name, op, truncate(o.payload(op), 200))
		}
	}
	t.Logf("%s: output = %q", name, truncate(o.output, 300))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
