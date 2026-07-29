package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// Every real-dtach test in this package opens with a LookPath and t.Skip, which is right on a
// developer box without dtach and wrong on CI: `go test` prints nothing for a skipped test, so a
// runner missing dtach silently shrinks the suite to the stub tests and still reports success. That
// is how eight tests — including the only assertions that WT=1 reaches a session's shell, that
// replay works through a real `dtach -A`, and that the signal ladder and socket probe behave
// against a real socket — went unrun on CI (#47).
//
// So on CI the skip is an error. Nothing pins WT_DIR here: this never creates a session.
func TestCIHasDtach(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("not CI; a developer box without dtach may legitimately skip the real-dtach tests")
	}
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Fatalf("dtach is not on PATH, so the real-dtach tests would skip and this job would "+
			"still pass: %v. The `go` job in .github/workflows/ci.yml installs it.", err)
	}
}

// End-to-end against real dtach, through the production argv.
//
// Everything else in this package uses a stub start command so dtach is never involved. This
// test is the exception, because three claims cannot be checked any other way:
//
//  1. Replay works through `dtach -A`, not just through a stub that writes to a pty.
//  2. `attached` is right when the socket's execute bit is *pinned on* by wtd's own held
//     attachment — the failure mode the whole attachedTo rework exists for. No stub can pin
//     that bit, because only dtach sets it.
//  3. A session outlives its hub, which is the property that makes dtach worth keeping.
//
// The empty start command selects the built-in path, so this exercises the same argv a real box
// runs rather than a script standing in for one.
//
// WT_DIR is a t.TempDir throughout. It must never be ~/.dtach: that holds real sessions on a
// developer box, possibly the one this test is running inside.
func TestIntegrationRealDtach(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}

	dir := t.TempDir()
	t.Setenv("WT_DIR", dir)
	// Point shortcuts at nothing: on a developer box ~/.config/wt/projects is a symlink to
	// the live /etc/ttyd-ify/projects, and a test should not depend on its contents.
	t.Setenv("WT_PROJECTS", filepath.Join(dir, "no-projects"))

	const name = "wtd-itest"
	app, base := hubTestServer(t, "", defaultReplayBytes, defaultMaxWarmHubs)

	// The dtach master and the shell inside it are not in the hub's process group — that is
	// the whole persistence model — so nothing else will ever clean them up.
	t.Cleanup(func() {
		// Step 6 deletes this session itself, so errNotFound is the expected outcome here and
		// is not worth logging. This stays as the safety net for a failure before that point.
		if err := deleteSession(dir, name); err != nil && !errors.Is(err, errNotFound) {
			t.Logf("cleanup: deleting session %s: %v", name, err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- 1. Create the session and put something identifiable in it -----------------
	first := attach(ctx, t, base, name, 80, 25)
	// Written so the marker cannot appear in the pty's echo of the input itself — the format
	// string holds "PID%s", and only the shell's *output* ever contains "PID:". Otherwise the
	// test passes on a terminal that echoes keystrokes and runs nothing.
	if err := writeFrame(ctx, first, opInput, []byte("printf 'PID%s\\n' \":$$\"\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	out := readUntil(ctx, t, first, "PID:", 20*time.Second)
	shellPID, err := findPID(out)
	if err != nil {
		t.Fatalf("%v (output was %q)", err, out)
	}

	sock := filepath.Join(dir, name+".sock")
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("no dtach socket at %s: %v", sock, err)
	}

	// --- 2. attached is true while a client is watching ----------------------------
	waitFor(t, 10*time.Second, func() bool { return sessionAttached(t, base, name) })
	if !sessionAttached(t, base, name) {
		t.Fatal("attached is false while a client is connected")
	}

	// cwd must resolve against the session's *shell*, through a real dtach process tree.
	//
	// Note what this does and does not prove: it pins the value end to end, but it does NOT
	// catch the misclassification described in scanDtach, because with real pids the buggy
	// version usually lands on the right process by accident. The rule itself is pinned
	// deterministically by TestSessionShellSkipsANestedDtach.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionByName(t, base, name).CWD; got != home {
		t.Errorf("cwd = %q, want %q — pid/cwd resolved against the wrong dtach process", got, home)
	}

	// pid must be the shell that printed $$ above, which is the strongest form of this
	// assertion available: it comes from inside the session rather than from another /proc walk
	// that could share the original bug. It used to be the dtach master — one level up, and
	// usually an adjacent integer, so it read as plausible.
	if got := sessionByName(t, base, name).PID; got != shellPID {
		t.Errorf("pid = %d, want %d — the shell that printed $$; the master is one level up",
			got, shellPID)
	}

	// --- 3. Client leaves: the bit stays pinned, attached must not ------------------
	_ = first.CloseNow()
	waitFor(t, 10*time.Second, func() bool { return !sessionAttached(t, base, name) })

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat %s: %v", sock, err)
	}
	pinned := info.Mode().Perm()&0o100 != 0
	if !pinned {
		t.Log("note: the execute bit was NOT pinned on — dtach cleared it despite wtd holding " +
			"an attachment, so the old derivation would have worked here after all")
	}
	if sessionAttached(t, base, name) {
		t.Fatal("attached is still true with no client watching: it is being read from the " +
			"socket's execute bit, which wtd's own held attachment pins on")
	}

	// --- 3b. An external attach must register, through real /proc and real pgids -----
	//
	// Without this the suite never exercises signal 2 at all: with a hub held and no wtd
	// clients, attachedTo returns false whether or not the /proc scan works, so step 3 would
	// pass with the entire client-counting loop deleted. TestAttachedDerivation covers the
	// logic with fabricated pgids; this covers the layout.
	if _, err := exec.LookPath("dtach"); err == nil {
		ext := exec.Command("dtach", "-a", sock, "-z", "-r", "winch")
		extPTY, err := pty.Start(ext)
		if err != nil {
			t.Fatalf("attach an external dtach client: %v", err)
		}
		waitFor(t, 10*time.Second, func() bool { return sessionAttached(t, base, name) })
		if !sessionAttached(t, base, name) {
			t.Error("an external dtach client is attached but the session reads as detached: " +
				"the /proc client scan is not finding it, or it is being mistaken for wtd's " +
				"own held attachment")
		}

		_ = extPTY.Close()
		_ = ext.Process.Kill()
		_, _ = ext.Process.Wait()

		waitFor(t, 15*time.Second, func() bool { return !sessionAttached(t, base, name) })
		if sessionAttached(t, base, name) {
			t.Error("the session still reads as attached after the external client exited")
		}
	}

	// --- 4. Replay: a fresh client sees prior output with no keypress ---------------
	second := attach(ctx, t, base, name, 80, 25)
	replayed := readUntil(ctx, t, second, "PID:", 20*time.Second)
	if !strings.Contains(replayed, "PID:") {
		t.Fatalf("reattached to a blank screen through real dtach; got %q", replayed)
	}
	if got, err := findPID(replayed); err != nil || got != shellPID {
		t.Fatalf("replayed pid %d (err %v), want %d", got, err, shellPID)
	}
	_ = second.CloseNow()

	// --- 5. The session outlives its hub -------------------------------------------
	// This is what "restarting wt.service leaves sessions running" means in practice:
	// buffers are lost, the shell is not.
	app.hubs.closeAll()
	if !processAlive(shellPID) {
		t.Fatalf("the session's shell (pid %d) died with its hub: dtach persistence is broken", shellPID)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the session socket vanished when its hub closed: %v", err)
	}
	t.Logf("session %s survived hub teardown (shell pid %d still alive, socket intact)", name, shellPID)

	// --- 6. DELETE, against a menu-shaped session with a client attached ------------
	//
	// Two things only this test can cover. First, the session here was created by the real
	// bin/wt via `dtach -A` — the shape the menu and every deep link produce — where
	// TestDeleteSessionKillsAnUnattachedSession covers the API's own `dtach -n`. Second,
	// deleting an *attached* session is legal (api/session-lifecycle.md §7), and an external
	// dtach client is the only way to hold a real attachment now that the hubs are closed.
	ext := exec.Command("dtach", "-a", sock, "-z", "-r", "winch")
	extPTY, err := pty.Start(ext)
	if err != nil {
		t.Fatalf("attach an external dtach client: %v", err)
	}
	t.Cleanup(func() {
		_ = extPTY.Close()
		_ = ext.Process.Kill()
		_, _ = ext.Process.Wait()
	})
	waitFor(t, 10*time.Second, func() bool { return sessionAttached(t, base, name) })

	if err := deleteSession(dir, name); err != nil {
		t.Fatalf("deleteSession on an attached session: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket %s still present after delete (stat err = %v)", sock, err)
	}
	waitFor(t, 5*time.Second, func() bool { return !processAlive(shellPID) })
	if processAlive(shellPID) {
		t.Errorf("the session's shell (pid %d) survived delete: the signal went to the wrong "+
			"process, or to nothing at all", shellPID)
	}
	t.Logf("session %s deleted while attached (shell pid %d dead, socket unlinked)", name, shellPID)
}

// Deleting an attached session must close its clients cleanly rather than dropping them.
//
// api/session-lifecycle.md section 7 promises this, and it is the half no unit test can see: DELETE
// works by signalling a pid resolved from /proc, so "which pid gets signalled" and "what the client
// observes" are the same question.
//
// This replaced a test that asserted the bash menu redrew after its session was deleted. That
// promise belonged to a picker running inside the pty, which no longer exists — wtd holds the dtach
// client directly, so when the session dies the client exits, the pty reaches EOF, and the hub
// closes every subscriber with a normal 1000. The observable moved; the requirement that a client
// is never left guessing did not.
func TestDeleteAttachedSessionClosesItsClientsNormally(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}

	dir := t.TempDir()
	t.Setenv("WT_DIR", dir)
	t.Setenv("WT_PROJECTS", filepath.Join(dir, "no-projects"))

	const name = "deleted-under-a-client"
	_, base := hubTestServer(t, "", defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conn := attach(ctx, t, base, name, 80, 25)

	// The session's own shell reports its pid, so its death is observed directly rather than
	// inferred from the socket vanishing.
	if err := writeFrame(ctx, conn, opInput, []byte("printf 'PID%s\\n' \":$$\"\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	out := readUntil(ctx, t, conn, "PID:", 20*time.Second)
	shellPID, err := findPID(out)
	if err != nil {
		t.Fatalf("%v (output was %q)", err, out)
	}
	t.Cleanup(func() {
		if processAlive(shellPID) {
			_ = syscall.Kill(shellPID, syscall.SIGKILL)
		}
	})

	sock := filepath.Join(dir, name+socketSuffix)
	waitFor(t, 15*time.Second, func() bool { _, err := os.Stat(sock); return err == nil })

	if got := sessionByName(t, base, name).PID; got != shellPID {
		t.Errorf("pid = %d, want %d", got, shellPID)
	}

	if err := deleteSession(dir, name); err != nil {
		t.Fatalf("deleteSession on a session with a client attached: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return !processAlive(shellPID) })
	if processAlive(shellPID) {
		t.Errorf("the session's shell (pid %d) survived delete", shellPID)
	}

	// The payoff: the client is closed with 1000, not reset. api/ws-protocol.md section 13 tells
	// clients to treat 1000 as final and not to reconnect on their own, which is only safe advice
	// if the server really sends it here — the shipped iOS client does exactly that.
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, _, rerr := conn.Read(ctx)
		if rerr != nil {
			if got := websocket.CloseStatus(rerr); got != websocket.StatusNormalClosure {
				t.Errorf("close status = %v, want %v (1000); a client whose session was deleted "+
					"must not see a bare drop: %v", got, websocket.StatusNormalClosure, rerr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Error("the connection stayed open after its session was deleted")
			break
		}
	}
	_ = conn.CloseNow()
}

// findPID pulls "PID:<n>" out of real shell output, which — unlike a stub's — is full of
// escape sequences, bracketed-paste toggles and bare carriage returns, so the marker is not
// at the start of a line. leak_test's parsePID is line-anchored and deliberately stays that
// way; this is the loose variant for output a human would recognize as a terminal.
func findPID(out string) (int, error) {
	m := regexp.MustCompile(`PID:(\d+)`).FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no PID:<n> in output")
	}
	return strconv.Atoi(m[1])
}

func sessionAttached(t *testing.T, base, name string) bool {
	t.Helper()
	return sessionByName(t, base, name).Attached
}

func sessionByName(t *testing.T, base, name string) Session {
	t.Helper()
	resp, err := http.Get(strings.TrimRight(base, "/") + "/api/v1/sessions")
	if err != nil {
		t.Fatalf("GET /api/v1/sessions: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	var sessions []Session
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	for _, s := range sessions {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("session %q is not listed at all", name)
	return Session{}
}

// The deep-link path enforces the same socket-path ceiling POST enforces.
//
// `POST /api/v1/sessions` refuses a name whose socket path would exceed the 107 bytes connect(2)
// can name. `?arg=` used to hand the name straight to dtach, which *binds* an over-long path quite
// happily — so the outcome was not an error but the worse thing: a session that exists, that
// nothing can ever attach to, and that no later probe can distinguish from a stale socket. That
// ambiguity is exactly why reapStale refuses to unlink on "could not find out", so such a session
// is not even cleaned up.
//
// This used to run bin/wt as a subprocess with dtach stubbed on PATH, because the ceiling was
// implemented twice — once in bash for `?arg=`, once in Go for POST — and the test's job was to
// prove the two agreed at the boundary. There is one implementation now, validateSocketPath, shared
// by validateAttachName and createSession, which is what closed #16. So the test moved to where a
// client actually reaches it: a real /ws?arg= connection, at room and room+1.
//
// The looser name rules stay looser on this path (spaces, non-ASCII, over 64 characters). That
// asymmetry is deliberate — see TestValidateAttachNameIsLooserThanCreate — and is not this test's
// business.
func TestDeepLinkEnforcesTheSocketPathLimit(t *testing.T) {
	// Deliberately NOT t.TempDir(), which is the rule everywhere else in this package. This test's
	// whole subject is path length, so it cannot inherit a base of arbitrary depth: under a long
	// TMPDIR — an agent's session-scoped scratch directory routinely is one — t.TempDir() alone can
	// already leave zero or negative room, and `strings.Repeat` with a negative count panics, which
	// aborts the test binary and silences every other test in the package. A short base keeps the
	// arithmetic below ours. It is still a throwaway directory, never ~/.dtach.
	tmp, err := os.MkdirTemp("/tmp", "wt-socklimit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })

	// Deep enough that the socket-path limit binds well before the 64-character name rule, so the
	// boundary under test is unambiguously the path's and not the name's.
	dir := tmp
	for sessionNameRoom(dir) > 32 {
		dir = filepath.Join(dir, strings.Repeat("d", 16))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	room := sessionNameRoom(dir)
	if room < 1 {
		t.Fatalf("fixture leaves no room for a name under %s (room = %d)", dir, room)
	}
	fits, over := strings.Repeat("a", room), strings.Repeat("a", room+1)

	// Assert the boundary directly first, so the wire assertions below are compared against
	// something rather than against whatever this arithmetic happens to produce.
	if err := validateSocketPath(dir, fits); err != nil {
		t.Fatalf("a %d-character name should fit under a %d-byte dir: %v", room, len(dir), err)
	}
	if validateSocketPath(dir, over) == nil {
		t.Fatalf("a %d-character name should not fit under a %d-byte dir", room+1, len(dir))
	}

	t.Setenv("WT_DIR", dir)
	t.Setenv("WT_PROJECTS", filepath.Join(dir, "no-projects"))
	_, base := hubTestServer(t, "", defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Over the limit: a working terminal with an explanation, and no session created. Never a close
	// — api/openapi.yaml publishes that no value of arg closes the connection.
	t.Run("over the limit degrades to a shell", func(t *testing.T) {
		conn := attach(ctx, t, base, over, 80, 25)
		defer conn.CloseNow() //nolint:errcheck // best-effort in a test

		if out := readUntil(ctx, t, conn, "wtd:", 20*time.Second); !strings.Contains(out, "wtd:") {
			t.Errorf("no notice explaining the refusal; got %q", out)
		}
		sock := filepath.Join(dir, over+socketSuffix)
		if _, err := os.Stat(sock); err == nil {
			t.Errorf("a session was created for a %d-byte socket path, over the %d-byte limit",
				len(sock), maxSocketPathLen)
		}
		// And it is a live terminal, not a corpse.
		if err := writeFrame(ctx, conn, opInput, []byte("printf 'ALIVE%s\\n' \":$$\"\n")); err != nil {
			t.Fatalf("the connection was closed rather than degraded: %v", err)
		}
		if out := readUntil(ctx, t, conn, "ALIVE:", 20*time.Second); !strings.Contains(out, "ALIVE:") {
			t.Errorf("the fallback shell does not respond to input; got %q", out)
		}
	})

	// At the limit: still creates and attaches. Without this the guard could reject everything and
	// break every deep link — the client's hot path — while the assertion above still passed.
	t.Run("at the limit still attaches", func(t *testing.T) {
		if _, err := exec.LookPath("dtach"); err != nil {
			t.Skip("dtach not installed")
		}
		conn := attach(ctx, t, base, fits, 80, 25)
		defer conn.CloseNow() //nolint:errcheck // best-effort in a test
		t.Cleanup(func() { _ = deleteSession(dir, fits) })

		sock := filepath.Join(dir, fits+socketSuffix)
		waitFor(t, 15*time.Second, func() bool { _, err := os.Stat(sock); return err == nil })
		if _, err := os.Stat(sock); err != nil {
			t.Errorf("a %d-byte socket path is within the %d-byte limit but no session was "+
				"created: %v", len(sock), maxSocketPathLen, err)
		}
	})
}
