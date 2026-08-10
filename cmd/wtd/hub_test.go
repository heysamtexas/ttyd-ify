package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Hub behavior: replay on attach, one session shared between clients, and a held attachment
// that outlives its clients without leaking processes.
//
// Every test here drives its own stub start command out of t.TempDir() or the shared
// test/stub-start-command.sh. None of them involve dtach or ~/.dtach — that directory holds
// real sessions on a developer box, possibly the one the test is running in.

func hubTestServer(t *testing.T, startCommand string, replayBytes, maxWarm int) (*server, string) {
	t.Helper()
	return hubTestServerWithStore(t, startCommand, replayBytes, maxWarm, nil)
}

// hubTestServerWithStore is the full-parameter form; store non-nil turns on ring persistence
// (#92), which only its own tests below use.
func hubTestServerWithStore(t *testing.T, startCommand string, replayBytes, maxWarm int, store *ringStore) (*server, string) {
	t.Helper()
	app := newServer(startCommand)
	// app.terminalCommand, not a literal exec of startCommand: these tests must go through the
	// same builder production does, or they would keep passing while the real path broke. With a
	// non-empty startCommand that builder runs the stub with the connection's argv, which is
	// exactly what every test here expects.
	app.hubs = newHubs(app.terminalCommand, replayBytes, maxWarm, store)
	// Hubs deliberately outlive the connections that created them, so nothing else would
	// ever release these processes.
	t.Cleanup(app.hubs.closeAll)

	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	return app, srv.URL
}

func writeStub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stub.sh")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func attach(ctx context.Context, t *testing.T, base, arg string, cols, rows int) *websocket.Conn {
	t.Helper()
	u := strings.TrimRight(base, "/") + "/ws"
	if arg != "" {
		u += "?arg=" + url.QueryEscape(arg)
	}
	conn, _, err := dialTTY(ctx, u)
	if err != nil {
		t.Fatalf("dial %s: %v", u, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(cols, rows)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return conn
}

// The headline acceptance criterion: attaching to a session that already produced output
// shows that output immediately, with no keypress and no resize.
//
// The stub reports its own pid, which is what separates a real replay from the hub having
// died and respawned — a respawn would print the same greeting and pass a weaker test.
func TestReplayShowsPriorOutputOnAttach(t *testing.T) {
	stub := writeStub(t, "printf 'HELLO:%s\\n' \"$1\"\nprintf 'PID:%s\\n' \"$$\"\nsleep 600\n")
	_, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first := attach(ctx, t, base, "demo", 80, 25)
	out := readUntil(ctx, t, first, "PID:", 10*time.Second)
	if !strings.Contains(out, "HELLO:demo") {
		t.Fatalf("first client never saw the greeting; got %q", out)
	}
	pid, err := parsePID(out)
	if err != nil {
		t.Fatalf("%v (output was %q)", err, out)
	}
	// Abrupt loss, exactly what a phone dropping off a network does.
	_ = first.CloseNow()

	second := attach(ctx, t, base, "demo", 80, 25)
	replayed := readUntil(ctx, t, second, "PID:", 10*time.Second)
	if !strings.Contains(replayed, "HELLO:demo") {
		t.Fatalf("second client attached to a blank screen; got %q — replay did not happen", replayed)
	}
	replayedPID, err := parsePID(replayed)
	if err != nil {
		t.Fatalf("%v (output was %q)", err, replayed)
	}
	if replayedPID != pid {
		t.Fatalf("second client saw pid %d, first saw %d: the hub was respawned rather than held, "+
			"so this output is fresh rather than replayed", replayedPID, pid)
	}
	_ = second.CloseNow()
}

// The named path's title frame names the session, where the argless path names the start
// command. That difference is wire-visible and reaches a shipped phone — the iOS client puts
// this string straight in its nav bar — and the conformance harness cannot catch it, because
// it only ever dials without ?arg=. See api/compatibility.md.
func TestNamedConnectionTitlesTheSession(t *testing.T) {
	stub := writeStub(t, "printf 'READY\\n'\nsleep 600\n")
	_, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := attach(ctx, t, base, "my-session", 80, 25)
	defer conn.CloseNow() //nolint:errcheck // best-effort

	deadline, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	for {
		_, data, err := conn.Read(deadline)
		if err != nil {
			t.Fatalf("no title frame arrived: %v", err)
		}
		if len(data) == 0 || data[0] != opTitle {
			continue
		}
		title := string(data[1:])
		if !strings.Contains(title, "my-session") {
			t.Fatalf("title %q does not name the session; a deep-linked client would show "+
				"the start command's path in its nav bar instead", title)
		}
		return
	}
}

// The seam between replayed and live output must neither duplicate nor drop a byte.
//
// The session emits a padded counting sequence, so contiguity is provable from the output
// alone: a gap or a repeat breaks the run of consecutive numbers wherever it happens. The
// replay budget is small on purpose, so the ring really does trim and the second client's
// replay is a tail rather than the whole history.
//
// What this catches is a *systematic* seam bug — snapshotting at the wrong point, sending the
// overlap twice, dropping the tail. It does not prove the mutex: the window it would have to
// hit is a few instructions wide, and this stub writes every 10 ms. Treat the atomicity of
// subscribe/broadcast as established by reading them, with `go test -race` as the check that
// it stays that way.
func TestReplaySeamIsContiguous(t *testing.T) {
	stub := writeStub(t, "for i in $(seq 1 400); do\n"+
		"  printf '%d-PADPADPADPADPADPADPADPADPAD\\n' \"$i\"\n"+
		"  sleep 0.01\n"+
		"done\nprintf 'END\\n'\nsleep 600\n")
	_, base := hubTestServer(t, stub, 4096, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first := attach(ctx, t, base, "seam", 80, 25)
	defer first.CloseNow() //nolint:errcheck // best-effort
	// Well past the 4 KiB budget, so the ring has already trimmed by the time the second
	// client attaches mid-stream.
	if out := readUntil(ctx, t, first, "\n200-PAD", 30*time.Second); !strings.Contains(out, "200-PAD") {
		t.Fatalf("session did not reach 200 lines; got %d bytes", len(out))
	}

	second := attach(ctx, t, base, "seam", 80, 25)
	defer second.CloseNow() //nolint:errcheck // best-effort
	stream := readUntil(ctx, t, second, "END", 30*time.Second)
	if !strings.Contains(stream, "END") {
		t.Fatalf("second client never reached the end of the sequence; got %d bytes", len(stream))
	}

	// Anything before the first newline may be half a line: the ring guarantees escape
	// sequence framing, not line framing.
	if i := strings.IndexByte(stream, '\n'); i >= 0 {
		stream = stream[i+1:]
	}

	nums := regexp.MustCompile(`(?m)^(\d+)-PAD`).FindAllStringSubmatch(stream, -1)
	if len(nums) < 50 {
		t.Fatalf("only %d numbered lines arrived; too few to say anything about the seam", len(nums))
	}
	prev := -1
	for _, m := range nums {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("unparsable line %q", m[0])
		}
		if prev >= 0 && n != prev+1 {
			if n == prev {
				t.Fatalf("line %d arrived twice: the replay and the live stream overlap", n)
			}
			t.Fatalf("jumped from %d to %d: %d byte(s) of the seam were dropped", prev, n, n-prev-1)
		}
		prev = n
	}
	if prev != 400 {
		t.Fatalf("stream ended at %d, not 400: output was lost at the end", prev)
	}
	t.Logf("verified %d contiguous lines across the replay/live seam (ending at %d)", len(nums), prev)
}

// Two clients on one deep link share a single session: output reaches both, and input from
// either is seen by both. This is screen -x semantics, and what dtach itself does with two
// attached clients.
func TestHubSharesOneSessionBetweenClients(t *testing.T) {
	stub, err := filepath.Abs("../../test/stub-start-command.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stub); err != nil {
		t.Skipf("shared protocol stub not available: %v", err)
	}
	_, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first := attach(ctx, t, base, "shared", 80, 25)
	defer first.CloseNow() //nolint:errcheck // best-effort
	readUntil(ctx, t, first, "ARGV:[shared]", 10*time.Second)

	second := attach(ctx, t, base, "shared", 80, 25)
	defer second.CloseNow() //nolint:errcheck // best-effort
	// The second client's replay proves it joined rather than started something new: the
	// argv line was printed before it existed.
	if out := readUntil(ctx, t, second, "ARGV:[shared]", 10*time.Second); !strings.Contains(out, "ARGV:[shared]") {
		t.Fatalf("second client did not join the existing session; got %q", out)
	}

	// One client types; both must see it.
	if err := writeFrame(ctx, first, opInput, []byte("ping\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	for name, conn := range map[string]*websocket.Conn{"typist": first, "observer": second} {
		if out := readUntil(ctx, t, conn, "ECHO:ping", 10*time.Second); !strings.Contains(out, "ECHO:ping") {
			t.Fatalf("%s did not see the echo; got %q", name, out)
		}
	}
}

// A hub must outlive its clients — that is what makes the *next* attach show context — and
// must still be reaped when the server stops, even if its child ignores SIGHUP.
//
// This is the deliberate inverse of TestTerminalReapsChildThatIgnoresSIGHUP, which asserts
// that an argless connection's private pty dies with the connection. Both behaviors are
// intended; confusing them is how blank-on-attach or a process leak comes back.
func TestHubHoldsSessionAcrossDisconnectThenReapsOnShutdown(t *testing.T) {
	stub := writeStub(t, "trap '' HUP\nprintf 'PID:%s\\n' \"$$\"\nsleep 600\n")
	app, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn := attach(ctx, t, base, "held", 80, 25)
	out := readUntil(ctx, t, conn, "PID:", 10*time.Second)
	pid, err := parsePID(out)
	if err != nil {
		t.Fatalf("%v (output was %q)", err, out)
	}
	_ = conn.CloseNow()

	// Long enough that a private-pty teardown would have finished its whole escalation.
	time.Sleep(3 * time.Second)
	if !processAlive(pid) {
		t.Fatalf("child %d was reaped when its client left: the hub did not hold the "+
			"attachment, so the next attach will show a blank screen", pid)
	}

	app.hubs.closeAll()
	if processAlive(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		t.Fatalf("child %d survived closeAll: the signal ladder did not run at hub teardown", pid)
	}
}

// A client that stops draining must be dropped, and — the part that matters — the session
// must keep running for everyone else.
//
// This is the only path that can stall a session for every attached client: if the pty pump
// ever blocked on a slow subscriber, backpressure would reach the shell itself. It is also
// the path with the most moving parts (a byte budget, a close status, and a cancel hook that
// exists solely to release a writer stuck in conn.Write), so it gets a test that exercises
// all three rather than trusting the reasoning.
func TestHubDropsAStalledClientAndKeepsTheSessionRunning(t *testing.T) {
	// Several times maxSubBacklogBytes on demand, plus a cheap liveness probe.
	//
	// One fast pipeline, not 600 slow ones. The previous version looped `dd bs=1000 count=10`
	// 600 times for ~5.7 MiB against a 4 MiB budget — and since the socket absorbs a chunk
	// before the writer blocks, it never reliably queued 4 MiB inside the window. It passed
	// anyway, on the 512-slot frame channel that used to sit in front of the budget and fill
	// at a few hundred KiB. Removing that channel (#66) is what exposed this: the test named
	// the byte budget in every comment while measuring something else.
	stub := writeStub(t, "printf 'READY\\n'\n"+
		"while IFS= read -r line; do\n"+
		"  case \"$line\" in\n"+
		"    flood) dd if=/dev/zero bs=1M count=16 2>/dev/null | tr '\\0' 'x' ;;\n"+
		"    ping)  printf 'PONG\\n' ;;\n"+
		"  esac\n"+
		"done\n")
	app, base := hubTestServer(t, stub, 4096, defaultMaxWarmHubs)

	// Captured before anything connects: the drop line is the operator's only notification, and
	// asserting it here is what proves the peer address survived the trip from r.RemoteAddr (#67).
	logged := captureLog(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// The victim: reads its greeting, then never reads again. Not reading is the whole
	// fixture — its TCP receive buffer fills, the hub's writer for it blocks in conn.Write,
	// and frames pile up against the byte budget.
	stalled := attach(ctx, t, base, "flood", 80, 25)
	defer stalled.CloseNow() //nolint:errcheck // best-effort
	readUntil(ctx, t, stalled, "READY", 15*time.Second)

	// The bystander, on the same session.
	healthy := attach(ctx, t, base, "flood", 80, 25)
	defer healthy.CloseNow() //nolint:errcheck // best-effort
	readUntil(ctx, t, healthy, "READY", 15*time.Second)

	waitFor(t, 10*time.Second, func() bool { return app.hubs.stats()["flood"].clients == 2 })

	if err := writeFrame(ctx, healthy, opInput, []byte("flood\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// One reader for the rest of the test. The bystander has to keep draining or it would be
	// dropped too, and its Read context cannot be cancelled to stop it — cancelling a Read
	// closes the connection in coder/websocket, which is what broke the first version of this
	// test. Read and Write from separate goroutines is supported.
	pong := make(chan bool, 1)
	go func() { pong <- readForMarker(ctx, healthy, "PONG", 150*time.Second) }()

	// The drop is observed server-side rather than by reading the victim's socket: reading it
	// would drain the backlog and it would never be behind at all — the other trap this test
	// fell into first time round.
	//
	// The deadline is the discriminator, and it has to stay well under pingInterval (30 s).
	// A stalled client is *also* reaped by the liveness ping eventually, so a generous wait
	// here passes whether or not the byte budget exists at all — measured: 35 s with the
	// budget removed, ~3 s with it. Anything inside this window can only be the budget.
	//
	// "Can only be the budget" is now true in a way it was not: the budget used to share this
	// job with a 512-slot frame channel, so a pass proved only that *one* of them fired, and
	// on real traffic it was always the channel (#66). The channel is gone, so this window
	// admits exactly one explanation.
	const budgetWindow = 15 * time.Second
	if budgetWindow >= pingInterval {
		t.Fatalf("budgetWindow %v must stay under pingInterval %v or this test proves nothing",
			budgetWindow, pingInterval)
	}
	waitFor(t, budgetWindow, func() bool { return app.hubs.stats()["flood"].clients == 1 })
	if got := app.hubs.stats()["flood"].clients; got != 1 {
		t.Fatalf("hub still has %d clients after %v: the stalled one was not dropped by the "+
			"byte budget (the liveness ping would eventually get it, which is not what this "+
			"test is about)", got, budgetWindow)
	}

	// The drop line has to answer "which client, and how stuck was it" (#67). Asserted from the
	// real thing rather than from a unit test, because the peer address is the half that only
	// exists if it was plumbed all the way from r.RemoteAddr through join into subscribe — a
	// hand-built subscriber would pass while the server supplied nothing.
	//
	// This hub has *two* clients, which is the situation the line could not describe: it named
	// neither, so an operator watching one client reconnect-loop while another was fine had no
	// way to tell them apart.
	line := ""
	for _, l := range strings.Split(logged(), "\n") {
		if strings.Contains(l, "dropping client") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no drop line was logged, so the operator was not told; log was:\n%s", logged())
	}
	t.Logf("drop line: %s", line)
	if !strings.Contains(line, `hub "flood"`) {
		t.Errorf("drop line does not name the session: %s", line)
	}
	// A real ip:port, so two connections from one device are still distinguishable. The port is
	// the part that makes it an identifier rather than a label.
	peer := regexp.MustCompile(`dropping client (\S+):(\d+):`).FindStringSubmatch(line)
	if peer == nil {
		t.Errorf("drop line does not identify the client as ip:port, which is the point of #67: %s", line)
	}
	// And how long it had been behind — the figure that separates stalled from slow. It must be
	// a real measurement, not the zero value: this client never read a byte after its greeting.
	behind := regexp.MustCompile(`: ([0-9.]+m?s) behind`).FindStringSubmatch(line)
	if behind == nil {
		t.Fatalf("drop line does not say how long the client was behind: %s", line)
	}
	d, err := time.ParseDuration(behind[1])
	if err != nil {
		t.Fatalf("drop line's duration %q does not parse: %v", behind[1], err)
	}
	if d <= 0 {
		t.Errorf("drop line reports %v behind; a client that stopped reading was behind for a "+
			"measurable interval, and 0 means the stamp is not being taken: %s", d, line)
	}

	// The point of dropping it: the session itself is unharmed. A pty pump that had blocked
	// on the stalled client would have stalled the shell for the survivor too. The stub only
	// reaches this input after its flood loop finishes, so a PONG proves the whole chain.
	if err := writeFrame(ctx, healthy, opInput, []byte("ping\n")); err != nil {
		t.Fatalf("write to the surviving client: %v", err)
	}
	if !<-pong {
		t.Fatal("the session stopped responding after a stalled client was dropped: the pty " +
			"pump was blocked on it, which is the failure this budget exists to prevent")
	}
}

// captureLog redirects the standard logger for one test and returns a reader for what was
// written. Mutex-protected rather than a bare bytes.Buffer: wtd logs from the pty pump, the ws
// handlers and the reaper, so -race would flag sharing one with them. Safe because nothing in
// this package calls t.Parallel.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(lockedWriter{mu: &mu, buf: &buf})
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// The duration reported at a drop is "how long behind *now*", not "ever behind", and the
// difference is what makes it a discriminator (#67). A long-lived connection that falls behind
// briefly a thousand times must not accumulate; only an interval it never recovered from counts.
func TestSubscriberMeasuresHowLongItHasBeenContinuouslyBehind(t *testing.T) {
	sub := newSubscriber("test-behind", nil)
	frame := framedOutput([]byte("x"))

	// The empty -> non-empty edge starts the interval, so this call is itself the stamp.
	if _, behind, ok := sub.offer(frame); !ok || behind > 10*time.Millisecond {
		t.Fatalf("first offer: behind = %v, ok = %v; the frame that puts a client behind starts "+
			"the interval, so it reports ~0", behind, ok)
	}

	const gap = 50 * time.Millisecond
	time.Sleep(gap)
	_, behind, ok := sub.offer(frame)
	if !ok {
		t.Fatal("second offer refused; two 1-byte frames cannot exceed the budget")
	}
	// Generous margin below `gap`: clock granularity and scheduling can only make the observed
	// interval longer than the sleep, never much shorter.
	if behind < gap-10*time.Millisecond {
		t.Errorf("behind = %v after sleeping %v; the interval is not being measured from the edge",
			behind, gap)
	}

	// Draining to empty ends the interval. The next frame starts a new one, and must not report
	// the time since the *original* edge — that is the accumulation this guards.
	for {
		if _, more := sub.pop(); !more {
			break
		}
	}
	if _, behind, ok := sub.offer(frame); !ok || behind > 10*time.Millisecond {
		t.Errorf("after draining to empty, behind = %v (ok = %v); a client that caught up must "+
			"start a fresh interval rather than carry the old one forward", behind, ok)
	}
}

// readForMarker drains output frames looking for marker, keeping only a small sliding window.
// readUntil accumulates everything and rescans it per frame, which is fine for a few hundred
// bytes and quadratic over the megabytes this test moves.
func readForMarker(ctx context.Context, conn *websocket.Conn, marker string, timeout time.Duration) bool {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	window := make([]byte, 0, 128<<10)
	for {
		_, data, err := conn.Read(deadline)
		if err != nil {
			return false
		}
		if len(data) == 0 || data[0] != opOutput {
			continue
		}
		window = append(window, data[1:]...)
		if strings.Contains(string(window), marker) {
			return true
		}
		if len(window) > 64<<10 {
			window = append(window[:0], window[len(window)-1024:]...)
		}
	}
}

// Warm hubs are capped, so an unauthenticated peer cannot leave one held attachment per
// invented session name behind it.
func TestHubEvictsWarmHubsPastTheCap(t *testing.T) {
	stub := writeStub(t, "printf 'PID:%s\\n' \"$$\"\nsleep 600\n")
	app, base := hubTestServer(t, stub, defaultReplayBytes, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Four sessions against a cap of 2. Eviction runs when a hub is *created*, and it
	// compares against the hubs that are already idle — so with the cap at 2 the third
	// creation is still within it and the fourth is the one that evicts.
	pids := map[string]int{}
	for _, name := range []string{"first", "second", "third", "fourth"} {
		conn := attach(ctx, t, base, name, 80, 25)
		out := readUntil(ctx, t, conn, "PID:", 10*time.Second)
		pid, err := parsePID(out)
		if err != nil {
			t.Fatalf("%s: %v (output was %q)", name, err, out)
		}
		pids[name] = pid
		_ = conn.CloseNow()
		// Each hub must be warm (no clients) before the next join, or there is nothing to
		// evict and the cap is never exercised.
		waitFor(t, 5*time.Second, func() bool { return app.hubs.stats()[name].clients == 0 })
	}

	// Eviction happens in the background, since teardown blocks on the signal ladder.
	waitFor(t, 20*time.Second, func() bool { return !processAlive(pids["first"]) })
	if processAlive(pids["first"]) {
		t.Fatalf("the oldest warm hub (pid %d) was not evicted past a cap of 2", pids["first"])
	}
	for _, name := range []string{"second", "third", "fourth"} {
		if !processAlive(pids[name]) {
			t.Fatalf("%s hub (pid %d) was evicted, but it is inside the cap", name, pids[name])
		}
	}
}

// Per-session memory is bounded, measured across 20 sessions nobody is watching — the case
// the ticket asks about, since a warm hub keeps buffering output with no client attached.
func TestIdleHubsBoundMemory(t *testing.T) {
	const (
		replay   = 8 * 1024
		sessions = 20
	)
	// 200 KiB per session, 25x the budget, so the bound is doing real work.
	stub := writeStub(t, "dd if=/dev/zero bs=1000 count=200 2>/dev/null | tr '\\0' 'x'\nprintf '\\nDONE\\n'\nsleep 600\n")
	app, base := hubTestServer(t, stub, replay, sessions+1)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for i := 0; i < sessions; i++ {
		name := fmt.Sprintf("bulk-%02d", i)
		conn := attach(ctx, t, base, name, 80, 25)
		// Held until the session has finished writing, so what is measured is a settled
		// buffer rather than a race with the pty pump.
		if out := readUntil(ctx, t, conn, "DONE", 30*time.Second); !strings.Contains(out, "DONE") {
			t.Fatalf("%s never finished writing; got %d bytes", name, len(out))
		}
		_ = conn.CloseNow()
	}

	// The last client's departure is processed asynchronously, so wait for every hub to be
	// genuinely unwatched before measuring what unwatched hubs cost.
	waitFor(t, 10*time.Second, func() bool {
		_, warm := replayHeld(app.hubs)
		return warm == sessions
	})

	perSession := replay + replay/ringSlackDiv
	total, held, largest := replayHeldMax(app.hubs)
	if held != sessions {
		t.Fatalf("%d warm hubs, want %d", held, sessions)
	}
	// Per session, not on average: an aggregate check passes with one hub 20x over budget.
	if largest > perSession {
		t.Fatalf("one session's replay buffer holds %d bytes, past the %d-byte per-session bound",
			largest, perSession)
	}
	if total > sessions*perSession {
		t.Fatalf("replay buffers hold %d bytes across %d idle sessions, past the %d-byte bound",
			total, held, sessions*perSession)
	}
	t.Logf("%d idle sessions hold %d KiB of replay buffer (%d KiB each at most, budget %d KiB)",
		held, total/1024, perSession/1024, replay/1024)
}

func TestAttachedDerivation(t *testing.T) {
	idle := statMode(t, 0o600)
	busy := statMode(t, 0o700)

	ownPgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}

	// wantCount is read only when wantAttached; 0 there means "attached but uncountable",
	// which the wire renders as null. See attachedTo.
	cases := []struct {
		name         string
		info         os.FileInfo
		clients      []int
		stats        map[string]hubStat
		wantAttached bool
		wantCount    int
	}{
		{
			// The execute bit cannot be counted, so this is the uncountable case.
			name: "no hub, execute bit set",
			info: busy, wantAttached: true, wantCount: 0,
		},
		{
			name: "no hub, execute bit clear",
			info: idle, wantAttached: false, wantCount: 0,
		},
		{
			// The case the whole rework exists for: the bit is pinned on by wtd's own held
			// attachment, and nobody is watching.
			name: "hub held, no clients, only our own dtach client",
			info: busy, clients: []int{os.Getpid()},
			stats:        ownHub(ownPgid, 0),
			wantAttached: false, wantCount: 0,
		},
		{
			name:         "hub held with a client",
			info:         idle,
			stats:        ownHub(ownPgid, 1),
			wantAttached: true, wantCount: 1,
		},
		{
			name:         "hub held with three clients",
			info:         idle,
			stats:        ownHub(ownPgid, 3),
			wantAttached: true, wantCount: 3,
		},
		{
			// Someone attached over SSH or from the bash picker while wtd holds the session
			// warm. Invisible to both the client count and the pinned bit.
			name: "hub held, external dtach client attached",
			info: busy, clients: []int{os.Getpid()},
			stats:        ownHub(ownPgid+1_000_000, 0),
			wantAttached: true, wantCount: 1,
		},
		{
			// Both kinds of viewer at once, which is what the summing exists for: short-
			// circuiting on the subscriber count would report 2 viewers as 2 only by luck,
			// and the external one not at all.
			name: "hub held with a client AND an external dtach client",
			info: busy, clients: []int{os.Getpid()},
			stats:        ownHub(ownPgid+1_000_000, 2),
			wantAttached: true, wantCount: 3,
		},
		{
			name:         "hub held, no clients, no dtach processes at all",
			info:         busy,
			stats:        ownHub(ownPgid, 0),
			wantAttached: false, wantCount: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attached, count := attachedTo("s", tc.info, tc.clients, tc.stats)
			if attached != tc.wantAttached || count != tc.wantCount {
				t.Fatalf("attachedTo = (%v, %d), want (%v, %d)",
					attached, count, tc.wantAttached, tc.wantCount)
			}
		})
	}
}

// ownHub is the stats map for one held attachment on session "s".
func ownHub(pgid, clients int) map[string]hubStat {
	return map[string]hubStat{"s": {clients: clients, pgids: map[int]struct{}{pgid: {}}}}
}

func statMode(t *testing.T, mode os.FileMode) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sock")
	if err := os.WriteFile(path, nil, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// replayHeld reports the bytes every hub is currently retaining, and how many of them have
// no client attached. Reaching into the internals is the point: the bound being asserted is
// on the buffers themselves, not on anything observable from the wire.
func replayHeld(m *hubs) (total, warm int) {
	total, warm, _ = replayHeldMax(m)
	return total, warm
}

// replayHeldMax also reports the largest single ring, so a per-session bound can be asserted
// as a per-session bound rather than as an average that one fat hub could hide inside.
func replayHeldMax(m *hubs) (total, warm, largest int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.m {
		h.mu.Lock()
		n := len(h.ring.buf)
		total += n
		if n > largest {
			largest = n
		}
		if len(h.subs) == 0 {
			warm++
		}
		h.mu.Unlock()
	}
	return total, warm, largest
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- the ?arg= transport-safety floor (#17) ------------------------------------------

// attachArgs is attach() for more than one ?arg=, which is what the floor's count limit and
// the hubKey collision below both need. url.Values escapes a NUL as %00, which is exactly how
// a client would send one.
func attachArgs(ctx context.Context, t *testing.T, base string, args []string, cols, rows int) *websocket.Conn {
	t.Helper()
	u := strings.TrimRight(base, "/") + "/ws"
	if len(args) > 0 {
		q := url.Values{}
		for _, a := range args {
			q.Add("arg", a)
		}
		u += "?" + q.Encode()
	}
	conn, _, err := dialTTY(ctx, u)
	if err != nil {
		t.Fatalf("dial %s: %v", truncate(u, 120), err)
	}
	if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(cols, rows)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return conn
}

// api/ws-protocol.md §8 requires dropping values that cannot be argv elements and continuing.
// The regression to guard is the *close*: before this, a NUL reached exec, failed there, and
// took the connection down — the one outcome that rule forbids. So every case here asserts the
// connection survives and the start command ran, and only then checks what it received.
//
// Boundaries in pairs rather than one absurd value: an off-by-one that rejects a legitimate
// 4096-byte arg is as much a bug as one that accepts an unusable value.
func TestArgFloorDropsWhatCannotBeArgvAndKeepsTheConnection(t *testing.T) {
	atLimit := strings.Repeat("x", maxArgBytes)
	overLimit := strings.Repeat("x", maxArgBytes+1)

	countedArgs := func(n int) []string {
		args := make([]string, n)
		for i := range args {
			args[i] = fmt.Sprintf("s%d", i)
		}
		return args
	}

	// wantArgv, not a count. A count passes if the filter kept the wrong values — "keep" plus
	// a NUL value still yields one arg either way, and the wrong one makes it args[0] and so
	// the hub key. Asserting the values also makes this the only check that order survives,
	// which filterArgs promises and nothing else verifies.
	cases := []struct {
		name     string
		args     []string
		wantArgv string
	}{
		{"a NUL byte is dropped", []string{"a\x00b"}, ""},
		// A leading NUL is the case an index-based check gets wrong: strings.IndexByte returns
		// 0 here, so a `> 0` typo accepts it and it reaches exec.
		{"a leading NUL is dropped", []string{"\x00abc"}, ""},
		{"a trailing NUL is dropped", []string{"abc\x00"}, ""},
		{"a lone NUL is dropped", []string{"\x00"}, ""},
		{"a NUL among usable values drops only itself", []string{"keep", "a\x00b"}, "keep"},
		{"a NUL first still keeps the usable value", []string{"a\x00b", "keep"}, "keep"},
		{"at the byte limit it is kept", []string{atLimit}, atLimit},
		{"one byte over the limit is dropped", []string{overLimit}, ""},
		{"order is preserved", []string{"one", "two", "three"}, "one two three"},
		{"at the count limit all are kept", countedArgs(maxArgs),
			strings.Join(countedArgs(maxArgs), " ")},
		{"past the count limit the rest are ignored", countedArgs(maxArgs + 1),
			strings.Join(countedArgs(maxArgs), " ")},
		// A dropped value must not consume one of the 16 slots, or the count limit would
		// silently tighten for anyone who also sent something unusable.
		{"a drop before the cap does not consume its slot",
			append([]string{"a\x00b"}, countedArgs(maxArgs+1)...),
			strings.Join(countedArgs(maxArgs), " ")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A sentinel around "$*" so an empty argv is distinguishable from output that has
			// not arrived yet — the difference between "dropped, as specified" and "the
			// connection died", which is the regression this whole test exists for.
			stub := writeStub(t, "printf 'ARGV[%s]\\n' \"$*\"\nsleep 600\n")
			_, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			conn := attachArgs(ctx, t, base, tc.args, 80, 25)
			defer conn.CloseNow() //nolint:errcheck // best-effort teardown

			// Read to the *closing* bracket. Stopping at "ARGV[" returns as soon as the
			// prefix arrives, which for a 4096-byte value is thousands of bytes early.
			out := readUntil(ctx, t, conn, "]", 10*time.Second)

			// The pty translates the trailing newline to CRLF; neither carries meaning here.
			flat := strings.NewReplacer("\r", "", "\n", "").Replace(out)
			want := "ARGV[" + tc.wantArgv + "]"
			if !strings.Contains(flat, want) {
				t.Fatalf("start command received %q, want %q — the floor kept or dropped the "+
					"wrong values (a connection that closed instead shows as no output at all)",
					truncate(flat, 200), truncate(want, 200))
			}
		})
	}
}

// Two clients deep-linking the same *dropped* value each get their own picker, where two
// clients on a name validateAttachName merely rejects share one. api/openapi.yaml publishes both halves;
// this is the half that only became true with the floor, since a dropped arg makes the
// connection argless and an argless connection cannot be shared.
func TestTwoDroppedArgConnectionsDoNotShareAPicker(t *testing.T) {
	stub := writeStub(t, "printf 'PID:%s\\n' \"$$\"\nsleep 600\n")
	_, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pids := make([]int, 2)
	for i := range pids {
		conn := attachArgs(ctx, t, base, []string{"a\x00b"}, 80, 25)
		defer conn.CloseNow() //nolint:errcheck // best-effort teardown

		pid, err := parsePID(readUntil(ctx, t, conn, "PID:", 10*time.Second))
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		pids[i] = pid
	}

	if pids[0] == pids[1] {
		t.Fatalf("both connections landed on pid %d: a dropped arg left the connection named, "+
			"so two clients share one picker and interleave keystrokes", pids[0])
	}
}

// The collision this closes: hubKey joins args on NUL, which was documented as safe because
// "NUL cannot appear in an argv element". URL decoding put one there long before exec would
// have objected, so ?arg=a%00b keyed the same hub as ?arg=a&arg=b — one pty and one replay
// ring shared between two unrelated connections, reachable from a URL on an unauthenticated
// port.
//
// Distinct pids are the assertion, the same way TestReplayShowsPriorOutputOnAttach uses them:
// a shared hub means the second client is looking at the first one's process, and nothing
// weaker distinguishes that from a coincidence.
func TestArgWithNULDoesNotJoinAnotherHub(t *testing.T) {
	stub := writeStub(t, "printf 'PID:%s\\n' \"$$\"\nsleep 600\n")
	_, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two separate args, so the hub key is "a\x00b" — the key the NUL-carrying single arg
	// used to forge.
	two := attachArgs(ctx, t, base, []string{"a", "b"}, 80, 25)
	defer two.CloseNow() //nolint:errcheck // best-effort teardown
	twoPID, err := parsePID(readUntil(ctx, t, two, "PID:", 10*time.Second))
	if err != nil {
		t.Fatalf("two-arg connection: %v", err)
	}

	nul := attachArgs(ctx, t, base, []string{"a\x00b"}, 80, 25)
	defer nul.CloseNow() //nolint:errcheck // best-effort teardown
	nulPID, err := parsePID(readUntil(ctx, t, nul, "PID:", 10*time.Second))
	if err != nil {
		t.Fatalf("NUL-carrying connection: %v", err)
	}

	if nulPID == twoPID {
		t.Fatalf("?arg=a%%00b joined ?arg=a&arg=b's hub (both pid %d): one pty, one replay ring "+
			"and interleaved input shared between two unrelated connections", nulPID)
	}
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// One client's oversized frame must not end a session another client is using. That asymmetry is
// the whole reason a named connection exists, and it is what api/openapi.yaml and §14 now
// publish — they used to say 1009 "terminates the session's processes", which is true of an
// argless connection only.
//
// Worth guarding rather than assuming: on an unauthenticated port, a 1 MiB paste that could kill
// somebody else's live shell would be a denial of service reachable by anyone who can open a
// socket.
func TestOversizedFrameDropsOnlyItsSenderOnASharedSession(t *testing.T) {
	stub, err := filepath.Abs("../../test/stub-start-command.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stub); err != nil {
		t.Skipf("shared protocol stub not available: %v", err)
	}
	_, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	survivor := attach(ctx, t, base, "shared", 80, 25)
	defer survivor.CloseNow() //nolint:errcheck // best-effort teardown
	readUntil(ctx, t, survivor, "ARGV:[shared]", 10*time.Second)

	offender := attach(ctx, t, base, "shared", 80, 25)
	defer offender.CloseNow() //nolint:errcheck // best-effort teardown
	if out := readUntil(ctx, t, offender, "ARGV:[shared]", 10*time.Second); !strings.Contains(out, "ARGV:[shared]") {
		t.Fatalf("the second client did not join the existing session; got %q", out)
	}

	// One byte past the ceiling, so this is the documented 1009 path and not a coincidence.
	oversized := append([]byte{opInput}, make([]byte, maxFrameBytes+1)...)
	if err := offender.Write(ctx, websocket.MessageBinary, oversized); err != nil {
		t.Logf("write returned %v (the server closed during it, which is expected here)", err)
	}

	// The offender is dropped, and specifically with the published code.
	for {
		_, _, err := offender.Read(ctx)
		if err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
				t.Fatalf("the oversized sender was closed %v, want %v", got, websocket.StatusMessageTooBig)
			}
			break
		}
	}

	// The survivor keeps a working terminal — asserted by round-tripping input, not merely by
	// the socket still being open, since a killed process group would leave the connection up
	// with nothing behind it.
	if err := writeFrame(ctx, survivor, opInput, []byte("still-here\n")); err != nil {
		t.Fatalf("the surviving client could not write: %v — the session went down with the "+
			"other client's oversized frame", err)
	}
	if out := readUntil(ctx, t, survivor, "ECHO:still-here", 10*time.Second); !strings.Contains(out, "ECHO:still-here") {
		t.Fatalf("the surviving client got no echo back; got %q — one client's oversized paste "+
			"ended a session another client was using", out)
	}
}

// framedOutput wraps a payload the way hub.pump does: the OUTPUT opcode, then the bytes.
func framedOutput(payload []byte) []byte {
	frame := make([]byte, len(payload)+1)
	frame[0] = opOutput
	copy(frame[1:], payload)
	return frame
}

// The byte budget must be what decides, at every frame size — the property #66 is about.
//
// Swept rather than spot-checked, because a single size is what hid the bug: the suite only
// ever drove 16 KiB frames, where slots and bytes run out together, while a phone producing
// ~250-byte reads was dropped at 4% of the budget. Every size here would have failed against
// the 512-slot channel, and 1 byte is the honest worst case since read(2) on a pty may
// return exactly that.
func TestOfferRefusesOnlyAtTheByteBudget(t *testing.T) {
	for _, payload := range []int{1, 250, 351, 4096, 8192, ptyReadChunk - 1} {
		t.Run(fmt.Sprintf("payload=%d", payload), func(t *testing.T) {
			sub := newSubscriber("test-budget", nil)
			frame := framedOutput(bytes.Repeat([]byte("x"), payload))

			var accepted int
			for {
				backlog, _, ok := sub.offer(frame)
				if !ok {
					// Refusal has to land within one frame of the budget. Anything lower
					// means some other limit decided, which is exactly #66.
					if min := int64(maxSubBacklogBytes) - int64(len(frame)); backlog < min {
						t.Fatalf("refused at %d bytes, under the %d it should reach: something "+
							"other than the byte budget is the limit (#66)", backlog, min)
					}
					break
				}
				accepted++
				if accepted > 2*maxSubBacklogBytes {
					t.Fatalf("still accepting after %d frames: the budget never fired, so a "+
						"stalled client can hold output without bound", accepted)
				}
			}
			if sub.backlog() > maxSubBacklogBytes {
				t.Errorf("backlog %d exceeds the %d-byte budget", sub.backlog(), maxSubBacklogBytes)
			}
			// Merging is what buys all of the above, so assert it happened rather than
			// assuming it: unmerged, the entry count would equal the accept count.
			if payload < ptyReadChunk-1 && sub.pending() >= accepted {
				t.Errorf("queue holds %d entries for %d accepted frames; nothing merged",
					sub.pending(), accepted)
			}
		})
	}
}

// Merging must not corrupt the stream. Two traps: byte 0 of every frame is the OUTPUT opcode,
// so concatenating frames naively strands an opcode byte mid-output; and api/ws-protocol.md
// section 6 allows coalescing but forbids reordering.
func TestOfferMergesWithoutCorruptingTheStream(t *testing.T) {
	sub := newSubscriber("test-merge", nil)

	// Mixed sizes so both merge branches and the refuse-to-merge branch all run, including
	// a full-size pump frame, which is ptyReadChunk+1 bytes on the wire.
	var want []byte
	for i := 0; i < 3000; i++ {
		payload := []byte(fmt.Sprintf("<%d>", i))
		if i%500 == 499 {
			payload = bytes.Repeat([]byte("F"), ptyReadChunk)
		}
		want = append(want, payload...)
		if _, _, ok := sub.offer(framedOutput(payload)); !ok {
			t.Fatalf("offer %d refused at %d bytes", i, sub.backlog())
		}
	}

	var got []byte
	for {
		frame, ok := sub.pop()
		if !ok {
			break
		}
		if frame[0] != opOutput {
			t.Fatalf("popped a frame whose first byte is %q, not the OUTPUT opcode", frame[0])
		}
		// The bound clients depend on is "no larger than the pump emits", and the pump emits
		// ptyReadChunk+1: a full read plus the opcode byte.
		if len(frame) > ptyReadChunk+1 {
			t.Errorf("popped a %d-byte frame, larger than the %d-byte maximum the pump itself "+
				"produces", len(frame), ptyReadChunk+1)
		}
		got = append(got, frame[1:]...)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("stream corrupted by merging: got %d bytes, want %d", len(got), len(want))
	}
	if sub.backlog() != 0 {
		t.Errorf("backlog is %d after draining; bytes and queue have drifted", sub.backlog())
	}
}

// The bug's actual shape, through hub.broadcast rather than straight at a subscriber: small
// frames fanned out to a client that is draining. Nothing else in the suite drives this — the
// integration test's healthy client drains 16 KiB frames, the one size at which bytes and
// slots ran out together, which is why the suite stayed green while a phone was dropped eight
// times in 23 seconds. broadcast needs no pty, only subs, ring and name.
func TestBroadcastKeepsADrainingClientOnSmallFrames(t *testing.T) {
	h := &hub{name: "flood", ring: newRing(4096), subs: map[*subscriber]struct{}{}}
	sub := newSubscriber("test-draining", nil)
	h.subs[sub] = struct{}{}

	drained := make(chan int64, 1)
	go func() {
		var total int64
		for range sub.wake {
			for {
				frame, ok := sub.pop()
				if !ok {
					break
				}
				total += int64(len(frame) - 1)
			}
			if total >= 8<<20 {
				break
			}
		}
		drained <- total
	}()

	// Four times the byte budget in ~250-byte frames: ~33k of them, against 512 slots.
	var sent int64
	payload := bytes.Repeat([]byte("y"), 250)
	for sent < 8<<20 {
		h.broadcast(framedOutput(payload))
		sent += int64(len(payload))
		if _, ok := h.subs[sub]; !ok {
			t.Fatalf("client dropped after %d bytes while draining: %d bytes queued", sent,
				sub.backlog())
		}
	}
	close(sub.wake)
	if got := <-drained; got == 0 {
		t.Fatal("the reader drained nothing; the wake protocol never delivered")
	}
}

// Ring persistence across restarts (#92). These tests drive two server generations sharing
// one ringStore, which is exactly what a systemd restart is: a new wtd process over the same
// /run/wt. The store's sessionDir is a test-owned temp dir — a live `net.Listen` socket in it
// is what stands in for a dtach session that survived the restart.

// The ticket's acceptance criterion end to end at the hub level: output seen before the
// "restart" is replayed after it, preceded by the gap notice, followed by live output — and
// the saved file is consumed by its restore.
func TestSaveAllThenRestoreReplaysAcrossServerGenerations(t *testing.T) {
	store, listen := storeFixture(t)
	listen("demo")

	past := writeStub(t, "printf 'PAST\\n'\nsleep 600\n")
	gen1, base1 := hubTestServerWithStore(t, past, defaultReplayBytes, defaultMaxWarmHubs, store)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first := attach(ctx, t, base1, "demo", 80, 25)
	readUntil(ctx, t, first, "PAST", 10*time.Second)
	_ = first.CloseNow()

	// The exact sequence main.go runs on SIGTERM, in the same order.
	gen1.hubs.saveAll()
	gen1.hubs.closeAll()

	fresh := writeStub(t, "printf 'FRESH\\n'\nsleep 600\n")
	_, base2 := hubTestServerWithStore(t, fresh, defaultReplayBytes, defaultMaxWarmHubs, store)

	second := attach(ctx, t, base2, "demo", 80, 25)
	out := readUntil(ctx, t, second, "FRESH", 10*time.Second)
	_ = second.CloseNow()

	noticeAt := strings.Index(out, "saved before a server restart")
	pastAt := strings.Index(out, "PAST")
	freshAt := strings.Index(out, "FRESH")
	if noticeAt < 0 || pastAt < 0 {
		t.Fatalf("post-restart attach lost the restore: notice at %d, replay at %d, in %q",
			noticeAt, pastAt, out)
	}
	if noticeAt > pastAt || pastAt > freshAt {
		t.Fatalf("want gap notice, then restored replay, then live output; got %q", out)
	}
	if _, err := os.Stat(ringPath(store, "demo")); !os.IsNotExist(err) {
		t.Fatal("the saved ring survived its restore; a tail must not reappear on every spawn")
	}
}

// A restored replay is stale by definition, so the first attach must still get the redraw
// kick a blank attach gets — without it a full-screen program would sit pinned on its
// pre-restart frame. The stub traps the SIGWINCH the kick's genuine size change delivers;
// the join-time resize is a no-op (same size the pty was created with) and sends none.
func TestRestoredAttachStillKicksARepaint(t *testing.T) {
	store, listen := storeFixture(t)
	listen("demo")
	if err := store.save("demo", []byte("PAST\r\n")); err != nil {
		t.Fatal(err)
	}

	stub := writeStub(t,
		"trap 'printf \"WINCH\\n\"' WINCH\nprintf 'READY\\n'\nwhile true; do sleep 600 & wait $!; done\n")
	_, base := hubTestServerWithStore(t, stub, defaultReplayBytes, defaultMaxWarmHubs, store)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := attach(ctx, t, base, "demo", 80, 25)
	out := readUntil(ctx, t, conn, "WINCH", 15*time.Second)
	_ = conn.CloseNow()
	if !strings.Contains(out, "PAST") {
		t.Fatalf("replay was empty, so this kick proves nothing about the restore path; got %q", out)
	}
}

// A session that died with the server must not have its saved output replayed into a fresh
// session that inherits its name — and the stale file must still be consumed on the attempt.
func TestDeadSessionsRingIsNotReplayedIntoItsNamesake(t *testing.T) {
	store, _ := storeFixture(t) // deliberately no socket for "demo"
	if err := store.save("demo", []byte("STALE\r\n")); err != nil {
		t.Fatal(err)
	}

	stub := writeStub(t, "printf 'FRESH\\n'\nsleep 600\n")
	_, base := hubTestServerWithStore(t, stub, defaultReplayBytes, defaultMaxWarmHubs, store)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := attach(ctx, t, base, "demo", 80, 25)
	out := readUntil(ctx, t, conn, "FRESH", 10*time.Second)
	_ = conn.CloseNow()

	if strings.Contains(out, "STALE") {
		t.Fatalf("a dead session's output was replayed into its namesake: %q", out)
	}
	if strings.Contains(out, "saved before a server restart") {
		t.Fatalf("gap notice sent with nothing restored: %q", out)
	}
	if _, err := os.Stat(ringPath(store, "demo")); !os.IsNotExist(err) {
		t.Fatal("the dead session's ring file was not consumed; it would wait forever for a namesake")
	}
}

// Eviction bounds memory and /run is memory, so an evicted hub's ring is dropped, not saved.
// saveAll never sees it — eviction removes the hub from the map synchronously — and nothing
// on the eviction path writes; this pins both halves.
func TestEvictionDropsRingsRatherThanSavingThem(t *testing.T) {
	store, _ := storeFixture(t)
	stub := writeStub(t, "printf 'UP:%s\\n' \"$1\"\nsleep 600\n")
	app, base := hubTestServerWithStore(t, stub, defaultReplayBytes, 0, store) // cap 0: no warm hubs

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first := attach(ctx, t, base, "evicted", 80, 25)
	readUntil(ctx, t, first, "UP:evicted", 10*time.Second)
	_ = first.CloseNow()
	// Warm before the next creation, or there is nothing to evict.
	waitFor(t, 5*time.Second, func() bool { return app.hubs.stats()["evicted"].clients == 0 })

	second := attach(ctx, t, base, "kept", 80, 25)
	readUntil(ctx, t, second, "UP:kept", 10*time.Second)
	defer second.CloseNow()

	app.hubs.saveAll()
	if _, err := os.Stat(ringPath(store, "evicted")); !os.IsNotExist(err) {
		t.Fatal("an evicted hub left saved state behind")
	}
	if _, err := os.Stat(ringPath(store, "kept")); err != nil {
		t.Fatalf("the live hub's ring was not saved: %v", err)
	}
}

// The notice must stop once the session's own output has trimmed every restored byte out of
// the ring — a session that scrolled past the restart hours ago must not keep warning every
// new client about a gap its replay no longer contains. This is the h.ring.base >= h.restored
// arm of gapNotice, which no connection-level test reaches deterministically.
func TestGapNoticeStopsOnceRestoredBytesTrimOut(t *testing.T) {
	h := &hub{ring: newRing(64)}
	h.ring.write([]byte(strings.Repeat("r", 32)))
	h.restored = h.ring.total
	if h.gapNotice() == "" {
		t.Fatal("no gap notice while every restored byte is still retained")
	}

	// One 64-byte write pushes the ring past its trim slack (max + max/4), cutting the head —
	// the restored bytes — out of the retained window. Loop with a bound in case the trim
	// arithmetic changes.
	for i := 0; i < 100 && h.ring.base < h.restored; i++ {
		h.ring.write([]byte(strings.Repeat("x", 64)))
	}
	if h.ring.base < h.restored {
		t.Fatal("test setup never trimmed past the restored bytes; raise the write count")
	}
	if got := h.gapNotice(); got != "" {
		t.Fatalf("gap notice still sent after the restore trimmed out of the ring: %q", got)
	}
}
