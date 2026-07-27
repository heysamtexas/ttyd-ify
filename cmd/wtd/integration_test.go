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

	"github.com/creack/pty"
)

// End-to-end against real dtach and the real bin/wt.
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
// WT_DIR is a t.TempDir throughout. It must never be ~/.dtach: that holds real sessions on a
// developer box, possibly the one this test is running inside.
func TestIntegrationRealDtach(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}
	wt, err := filepath.Abs("../../bin/wt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Skipf("bin/wt not available: %v", err)
	}

	dir := t.TempDir()
	// Read by both wtd (sessionDir) and bin/wt, which takes it from the environment only.
	t.Setenv("WT_DIR", dir)
	// Point shortcuts at nothing: on a developer box ~/.config/wt/projects is a symlink to
	// the live /etc/ttyd-ify/projects, and a test should not depend on its contents.
	t.Setenv("WT_PROJECTS", filepath.Join(dir, "no-projects"))

	const name = "wtd-itest"
	app, base := hubTestServer(t, wt, defaultReplayBytes, defaultMaxWarmHubs)

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

// Deleting an attached session must leave bin/wt's menu loop alive.
//
// api/session-lifecycle.md §7 promises this, and it is why bin/wt deliberately omits `set -e`:
// the menu's `dtach -a` returning non-zero — which is exactly what a session dying underneath it
// looks like — has to redraw the menu rather than drop the whole connection. Since DELETE works
// by signalling a pid resolved from /proc, "which pid gets signalled" and "what survives it" are
// the same question, and this is the half no unit test can see.
//
// An argless connection is the menu path. wtd gives it a private pty running bin/wt, which does
// *not* exec dtach — only the `?arg=` branch does — so there is a shell left to return to. The
// session is created from the menu itself, so this is also the only place a menu-created session
// is deleted through the API's code path.
func TestDeleteAttachedSessionLeavesTheMenuAlive(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}
	wt, err := filepath.Abs("../../bin/wt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Skipf("bin/wt not available: %v", err)
	}

	dir := t.TempDir()
	t.Setenv("WT_DIR", dir)
	// A developer box symlinks ~/.config/wt/projects to the live /etc/ttyd-ify/projects, and
	// its contents would change the "name (shortcuts: ...)" prompt this test reads.
	t.Setenv("WT_PROJECTS", filepath.Join(dir, "no-projects"))

	const name = "menu-made"
	_, base := hubTestServer(t, wt, defaultReplayBytes, defaultMaxWarmHubs)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conn := attach(ctx, t, base, "", 80, 25)
	if out := readUntil(ctx, t, conn, "terminals @", 15*time.Second); !strings.Contains(out, "terminals @") {
		t.Fatalf("no menu on an argless connection; got %q", out)
	}

	// n) new, then the name: the menu's own `dtach -A`, run as a child rather than exec'd.
	if err := writeFrame(ctx, conn, opInput, []byte("n\n")); err != nil {
		t.Fatalf("write menu choice: %v", err)
	}
	if out := readUntil(ctx, t, conn, "name", 10*time.Second); !strings.Contains(out, "name") {
		t.Fatalf("menu did not prompt for a session name; got %q", out)
	}
	if err := writeFrame(ctx, conn, opInput, []byte(name+"\n")); err != nil {
		t.Fatalf("write session name: %v", err)
	}

	sock := filepath.Join(dir, name+socketSuffix)
	waitFor(t, 15*time.Second, func() bool { _, err := os.Stat(sock); return err == nil })
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the menu did not create %s: %v", sock, err)
	}

	// Same marker trick as TestIntegrationRealDtach: the session's own shell reports its pid,
	// so its death is observed directly rather than inferred from the socket vanishing.
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

	if got := sessionByName(t, base, name).PID; got != shellPID {
		t.Errorf("pid = %d, want %d — a menu-created session reports the wrong process too",
			got, shellPID)
	}

	if err := deleteSession(dir, name); err != nil {
		t.Fatalf("deleteSession on a session attached from the menu: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return !processAlive(shellPID) })
	if processAlive(shellPID) {
		t.Errorf("the session's shell (pid %d) survived delete", shellPID)
	}

	// The payoff: the same connection must show the menu again. If wt exited with its session
	// the client would just see the socket close.
	if back := readUntil(ctx, t, conn, "terminals @", 15*time.Second); !strings.Contains(back, "terminals @") {
		t.Errorf("the menu did not redraw after its attached session was deleted — bin/wt died "+
			"with the session instead of looping; got %q", back)
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
// `POST /api/v1/sessions` refuses a name whose socket path would exceed the 107 bytes
// connect(2) can name. `?arg=` used to hand `$1` straight to dtach, which *binds* an over-long
// path quite happily — so the outcome was not an error but the worse thing: a session that
// exists, that nothing can ever attach to, and that no later probe can distinguish from a
// stale socket. That ambiguity is exactly why reapStale refuses to unlink on "could not find
// out", so such a session is not even cleaned up. Reachable by a client following the spec,
// which now documents `?arg=` as a way to create sessions.
//
// The limit is implemented twice now, in two languages, so what this pins is that they agree
// at the boundary — room and room+1, not one absurd name. bin/wt still accepts plenty of names
// POST would refuse (spaces, non-ASCII, over 64 characters), because the terminal menu creates
// such names and both pickers must list them; that asymmetry is deliberate and is not this
// test's business.
//
// dtach is stubbed on PATH rather than real: the question is which branch bin/wt takes, and a
// real dtach would leave a live master and a shell behind to be reaped on every run.
func TestDeepLinkEnforcesTheSocketPathLimit(t *testing.T) {
	wt, err := filepath.Abs("../../bin/wt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Skipf("bin/wt not available: %v", err)
	}

	// Deliberately NOT t.TempDir(), which is the rule everywhere else in this package. This
	// test's whole subject is path length, so it cannot inherit a base of arbitrary depth:
	// under a long TMPDIR — an agent's session-scoped scratch directory routinely is one —
	// t.TempDir() alone can already leave zero or negative room, and `strings.Repeat` with a
	// negative count panics, which aborts the test binary and silences every other test in the
	// package. A short base keeps the arithmetic below ours. It is still a throwaway directory,
	// never ~/.dtach, which is the rule that actually matters.
	base, err := os.MkdirTemp("/tmp", "wt-socklimit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	// Deep enough that the socket-path limit binds well before the 64-character name rule, so
	// the boundary under test is unambiguously the path's and not the name's.
	dir := base
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

	// The Go side defines the boundary; assert it here so the shell side is being compared
	// against something rather than against whatever this arithmetic happens to produce.
	if err := validateSocketPath(dir, fits); err != nil {
		t.Fatalf("a %d-character name should fit under a %d-byte dir: %v", room, len(dir), err)
	}
	if validateSocketPath(dir, over) == nil {
		t.Fatalf("a %d-character name should not fit under a %d-byte dir", room+1, len(dir))
	}

	stub := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(stub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stub, "dtach"),
		[]byte("#!/usr/bin/env bash\necho \"DTACH-CALLED $*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	run := func(wtDir, arg string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", wt, arg)
		cmd.Env = append(os.Environ(),
			"PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"),
			"WT_DIR="+wtDir,
			// On a developer box ~/.config/wt/projects symlinks to the live
			// /etc/ttyd-ify/projects; a test must not depend on its contents.
			"WT_PROJECTS="+filepath.Join(dir, "no-projects"))
		// Stdin is left nil, which exec connects to /dev/null: the picker's `read` then gets
		// EOF and the menu loop returns, so the fall-through case terminates rather than
		// waiting for a choice that is never coming.
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bin/wt with a %d-character name: %v (%s)", len(arg), err, out)
		}
		return string(out)
	}

	// Both spellings of the same directory. wtd measures a filepath.Join-ed path, which cleans;
	// bin/wt measures "$DIR/$1.sock" literally, so before it stripped its own trailing slash the
	// two disagreed by one byte — and the observable was a name POST accepts whose deep link
	// silently lands on the picker. The boundary must not move with the spelling.
	for _, wtDir := range []string{dir, dir + "/"} {
		// Over the limit: no dtach, and the picker instead — the same graceful fallback a name
		// containing `/` gets.
		if out := run(wtDir, over); strings.Contains(out, "DTACH-CALLED") {
			t.Errorf("WT_DIR=%q: bin/wt ran dtach for a %d-byte socket path, over the %d-byte "+
				"limit: %q", wtDir, len(filepath.Join(dir, over+socketSuffix)), maxSocketPathLen, out)
		} else if !strings.Contains(out, "terminals @") {
			t.Errorf("WT_DIR=%q: bin/wt neither ran dtach nor rendered the picker: %q", wtDir, out)
		}

		// At the limit: still attaches. Without this the guard could reject everything and break
		// every deep link — the client's hot path — while the assertion above still passed.
		if out := run(wtDir, fits); !strings.Contains(out, "DTACH-CALLED") {
			t.Errorf("WT_DIR=%q: bin/wt refused a %d-byte socket path, which is within the "+
				"%d-byte limit: %q", wtDir, len(filepath.Join(dir, fits+socketSuffix)),
				maxSocketPathLen, out)
		}
	}
}
