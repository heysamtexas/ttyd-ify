package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
		if err := deleteSession(dir, name); err != nil {
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
	// This is what "restarting wt-web.service leaves sessions running" means in practice:
	// buffers are lost, the shell is not.
	app.hubs.closeAll()
	if !processAlive(shellPID) {
		t.Fatalf("the session's shell (pid %d) died with its hub: dtach persistence is broken", shellPID)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the session socket vanished when its hub closed: %v", err)
	}
	t.Logf("session %s survived hub teardown (shell pid %d still alive, socket intact)", name, shellPID)
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
