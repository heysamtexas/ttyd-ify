package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	app := newServer(startCommand)
	app.hubs = newHubs(startCommand, replayBytes, maxWarm)
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

// The seam between replayed and live output must neither duplicate nor drop a byte.
//
// The session emits a padded counting sequence, so contiguity is provable from the output
// alone: a gap or a repeat breaks the run of consecutive numbers wherever it happens. The
// replay budget is small on purpose, so the ring really does trim and the second client's
// replay is a tail rather than the whole history.
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

// Warm hubs are capped, so an unauthenticated peer cannot leave one held attachment per
// invented session name behind it.
func TestHubEvictsWarmHubsPastTheCap(t *testing.T) {
	stub := writeStub(t, "printf 'PID:%s\\n' \"$$\"\nsleep 600\n")
	app, base := hubTestServer(t, stub, defaultReplayBytes, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pids := map[string]int{}
	for _, name := range []string{"first", "second", "third"} {
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
	for _, name := range []string{"second", "third"} {
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
	total, held := replayHeld(app.hubs)
	if held != sessions {
		t.Fatalf("%d warm hubs, want %d", held, sessions)
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

	cases := []struct {
		name    string
		info    os.FileInfo
		clients []int
		stats   map[string]hubStat
		want    bool
	}{
		{
			name: "no hub, execute bit set",
			info: busy, want: true,
		},
		{
			name: "no hub, execute bit clear",
			info: idle, want: false,
		},
		{
			// The case the whole rework exists for: the bit is pinned on by wtd's own held
			// attachment, and nobody is watching.
			name: "hub held, no clients, only our own dtach client",
			info: busy, clients: []int{os.Getpid()},
			stats: map[string]hubStat{"s": {clients: 0, pgid: ownPgid}},
			want:  false,
		},
		{
			name:  "hub held with a client",
			info:  idle,
			stats: map[string]hubStat{"s": {clients: 1, pgid: ownPgid}},
			want:  true,
		},
		{
			// Someone attached over SSH or from the bash picker while wtd holds the session
			// warm. Invisible to both the client count and the pinned bit.
			name: "hub held, external dtach client attached",
			info: busy, clients: []int{os.Getpid()},
			stats: map[string]hubStat{"s": {clients: 0, pgid: ownPgid + 1_000_000}},
			want:  true,
		},
		{
			name:  "hub held, no clients, no dtach processes at all",
			info:  busy,
			stats: map[string]hubStat{"s": {clients: 0, pgid: ownPgid}},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachedTo("s", tc.info, tc.clients, tc.stats); got != tc.want {
				t.Fatalf("attachedTo = %v, want %v", got, tc.want)
			}
		})
	}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.m {
		h.mu.Lock()
		total += len(h.ring.buf)
		if len(h.subs) == 0 {
			warm++
		}
		h.mu.Unlock()
	}
	return total, warm
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
