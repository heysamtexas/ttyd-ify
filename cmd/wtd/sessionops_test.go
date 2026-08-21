package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// probeSocket has three answers and only one of them authorizes unlinking. The distinction is
// the whole of #5: before it, every failure to connect meant "stale", so a socket that could not
// be *named* was indistinguishable from one with nothing behind it.
func TestProbeSocketDistinguishesRefusedFromUnknowable(t *testing.T) {
	dir := t.TempDir()

	listening := filepath.Join(dir, "listening.sock")
	l, err := net.Listen("unix", listening)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	// A real stale socket: bound, then closed without unlinking, so the file outlives its
	// listener exactly as it does when a dtach master dies. This is the shape reapStale exists
	// for, and it must keep being reaped.
	stale := filepath.Join(dir, "stale.sock")
	sl, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := sl.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("test setup: the stale socket file did not survive its listener: %v", err)
	}

	tooLong := filepath.Join(longSessionDir(t, "x"), "x"+socketSuffix)

	cases := []struct {
		name string
		path string
		want socketState
	}{
		{"a listening master", listening, socketListening},
		{"a stale socket, nothing bound", stale, socketRefused},
		{"no file at all", filepath.Join(dir, "absent.sock"), socketRefused},
		{"a path connect(2) cannot name", tooLong, socketUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeSocket(tc.path); got != tc.want {
				t.Errorf("probeSocket(%d-byte path) = %v, want %v", len(tc.path), got, tc.want)
			}
		})
	}
}

// reapStale must remove a genuinely stale socket and leave everything else alone. The negative
// case matters as much as the positive one: this runs on the read path, so a listing that reaps
// too eagerly is silent data loss, and one that reaps nothing lets phantoms accumulate in
// a deep link — the disagreement between what lists and what attaches that reaping exists to settle.
func TestReapStaleRemovesOnlyProvablyStaleSockets(t *testing.T) {
	dir := t.TempDir()

	stale := filepath.Join(dir, "stale.sock")
	sl, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := sl.Close(); err != nil {
		t.Fatal(err)
	}

	live := filepath.Join(dir, "live.sock")
	l, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	// Not a socket, and not named .sock: neither should be touched by a function that unlinks
	// files on the read path.
	regular := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(regular, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	reaped := reapStale(dir)
	if len(reaped) != 1 || reaped[0] != "stale" {
		t.Errorf("reaped = %v, want exactly [stale]", reaped)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale socket survived (stat err = %v); phantoms would accumulate", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("the live socket was unlinked: %v", err)
	}
	if _, err := os.Stat(regular); err != nil {
		t.Errorf("a regular file was unlinked: %v", err)
	}
}

// The startup warning and the create-side refusal must agree about how much room a directory
// leaves, or the server warns about a limit it does not enforce — or enforces one it never
// mentioned. They share sessionNameRoom so that they cannot drift; this pins the arithmetic
// against the real limit rather than against itself.
func TestSessionNameRoomMatchesWhatValidateAccepts(t *testing.T) {
	for _, dir := range []string{"/tmp/x", "/home/someone/.dtach", "/tmp/" + strings.Repeat("d", 80)} {
		room := sessionNameRoom(dir)
		if room < 1 {
			if err := validateSocketPath(dir, "a"); err == nil {
				t.Errorf("%q: room = %d but a one-character name was accepted", dir, room)
			}
			continue
		}

		atLimit := strings.Repeat("n", room)
		if err := validateSocketPath(dir, atLimit); err != nil {
			t.Errorf("%q: a %d-character name (room = %d) was refused: %v", dir, len(atLimit), room, err)
		}
		if got := len(filepath.Join(dir, atLimit+socketSuffix)); got != maxSocketPathLen {
			t.Errorf("%q: a name filling the room yields a %d-byte path, want exactly %d",
				dir, got, maxSocketPathLen)
		}
		if err := validateSocketPath(dir, atLimit+"n"); err == nil {
			t.Errorf("%q: a name one character over the room was accepted", dir)
		}
	}
}

// DELETE must escalate to SIGKILL, and must take the session's background jobs with it.
//
// Both were specified in api/session-lifecycle.md §7 step 4 and neither was implemented: the ladder
// stopped at SIGTERM, and only the shell's pid was signalled. A shell trapping HUP and TERM turned
// DELETE into a 500 with the session still running — a refusal that reads as failure but is really
// an abandonment — and a background job outlived the session it belonged to as an orphan.
//
// leak_test.go proves the same escalation is needed on the terminal path, so "the shell always dies
// on SIGTERM" is not a property this repo assumes anywhere else either.
func TestDeleteEscalatesToSIGKILLAndTakesBackgroundJobs(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}

	dir := t.TempDir()
	const name = "stubborn"
	sock := filepath.Join(dir, name+socketSuffix)

	// Traps both signals it is sent before SIGKILL, and spawns a background job that only a group
	// signal can reach. The marker carries the job's pid so its death is observable.
	marker := filepath.Join(dir, "jobpid")
	script := "trap '' HUP TERM; sleep 600 & echo $! > " + marker + "; wait"
	out, err := exec.Command("dtach", "-n", sock, "-z", "-r", "winch", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("dtach -n: %v (%s)", err, out)
	}

	var shell int
	for i := 0; i < 60; i++ {
		if sessions, err := listSessions(dir, nil); err == nil && len(sessions) == 1 && sessions[0].PID > 0 {
			shell = sessions[0].PID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if shell == 0 {
		t.Fatal("no pid resolved for the stubborn session")
	}

	var job int
	waitFor(t, 10*time.Second, func() bool {
		b, err := os.ReadFile(marker)
		if err != nil {
			return false
		}
		job, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return job > 0
	})
	if job == 0 {
		t.Fatal("the background job never reported its pid")
	}
	t.Cleanup(func() {
		for _, pid := range []int{job, shell} {
			if processAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	if err := deleteSession(dir, name); err != nil {
		t.Fatalf("deleteSession on a shell that traps HUP and TERM: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after delete (stat err = %v)", err)
	}
	waitFor(t, 5*time.Second, func() bool { return !processAlive(shell) })
	if processAlive(shell) {
		t.Errorf("the shell (pid %d) trapped HUP and TERM and nothing escalated to SIGKILL", shell)
	}
	waitFor(t, 5*time.Second, func() bool { return !processAlive(job) })
	if processAlive(job) {
		t.Errorf("the background job (pid %d) outlived its session: the signal went to the pid, "+
			"not the process group", job)
	}
}

// A *stale* socket that cannot be probed must still be reaped. This is the other side of #5, and
// the first fix for it created this state: probeSocket answers socketUnknown for an over-long path
// without looking at the filesystem, so a live long-path session and a dead one give the identical
// answer — and skipping both left the dead one immortal. Unreapable here, undeletable through
// deleteSession, and listed as a session forever that nothing can attach to.
//
// Not a corner case on such a box: api/session-lifecycle.md calls reboot the common case for
// staleness, because socket files survive it and no master does. First boot would strand every one.
//
// The socket is made stale the way the lifecycle doc says produces exactly this — SIGKILL the
// master, which cannot clean up after itself.
func TestAStaleUnprobeableSocketIsStillReaped(t *testing.T) {
	t.Run("reapStale removes it", func(t *testing.T) {
		dir, name, sock := staleUnprobeableSession(t)

		// Still unprobeable — that has not changed, and is exactly why /proc has to be the evidence.
		if got := probeSocket(sock); got != socketUnknown {
			t.Errorf("probeSocket = %v, want socketUnknown for a %d-byte path", got, len(sock))
		}

		if reaped := reapStale(dir); len(reaped) != 1 || reaped[0] != name {
			t.Errorf("reaped = %v, want [%s]: a stale socket nothing holds must not be immortal "+
				"just because it cannot be dialled", reaped, name)
		}
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Errorf("the stale socket survived the reap (stat err = %v)", err)
		}
		if sessions, err := listSessions(dir, nil); err != nil || len(sessions) != 0 {
			t.Errorf("after reaping, %d sessions still listed (err %v), want 0", len(sessions), err)
		}
	})

	// The same state reached through the API. This returned 500 with the socket untouched, because
	// deleteSession fell past the unlink branch and then found no pid to signal — so the one
	// endpoint that could have cleaned up the mess refused to.
	t.Run("deleteSession removes it", func(t *testing.T) {
		dir, name, sock := staleUnprobeableSession(t)

		if err := deleteSession(dir, name); err != nil {
			t.Fatalf("deleteSession on a stale unprobeable socket: %v", err)
		}
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Errorf("the socket survived delete (stat err = %v)", err)
		}
		if err := deleteSession(dir, name); err != errNotFound {
			t.Errorf("second delete returned %v, want errNotFound", err)
		}
	})
}

// staleUnprobeableSession builds a real dtach session whose socket path is over the AF_UNIX limit,
// then makes it stale the way api/session-lifecycle.md says produces exactly this state: SIGKILL
// the master, which cannot clean up after itself. Returns the directory, name and socket path.
func staleUnprobeableSession(t *testing.T) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}

	const name = "stale-longpath"
	dir := longSessionDir(t, name)
	sock := filepath.Join(dir, name+socketSuffix)

	out, err := exec.Command("dtach", "-n", sock, "-z", "-r", "winch",
		"bash", "-c", "exec sleep 300").CombinedOutput()
	if err != nil {
		t.Skipf("dtach declined a %d-byte socket path: %v (%s)", len(sock), err, out)
	}

	var shell int
	for i := 0; i < 60; i++ {
		if sessions, err := listSessions(dir, nil); err == nil && len(sessions) == 1 && sessions[0].PID > 0 {
			shell = sessions[0].PID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if shell == 0 {
		t.Fatal("no pid resolved for the long-path session")
	}
	// The master is the shell's parent. Session.pid is deliberately the shell (#2), so the master
	// is not carried anywhere in the program and has to be read back out of /proc here.
	master := parentOf(t, shell)
	t.Cleanup(func() {
		for _, pid := range []int{shell, master} {
			if pid > 0 && processAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	if err := syscall.Kill(master, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the master %d: %v", master, err)
	}
	// The socket outlives a SIGKILLed master; wait until nothing references it any more.
	waitFor(t, 10*time.Second, func() bool { return !referencedIn(dir)(sock) })
	if referencedIn(dir)(sock) {
		t.Fatal("a dtach process still references the socket; it is not stale yet")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("the master unlinked its socket despite SIGKILL, so there is no stale socket: %v", err)
	}
	return dir, name, sock
}

// parentOf reads pid's parent from /proc. Field 4 of stat is ppid; the comm field before it can
// contain spaces and parentheses, so the scan starts after the last ')'.
func parentOf(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/stat: %v", pid, err)
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		t.Fatalf("unparseable /proc/%d/stat: %q", pid, s)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		t.Fatalf("unparseable /proc/%d/stat: %q", pid, s)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parse ppid from %q: %v", fields[1], err)
	}
	return ppid
}

// longSessionDir builds a session directory deep enough that <dir>/<name>.sock exceeds what
// connect(2) can name. The padding is explicit because t.TempDir() is nowhere near the limit on
// its own (~30 bytes) — inferring it from TMPDIR would let this quietly stop testing anything on
// a machine with a short one.
func longSessionDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	for len(filepath.Join(dir, name+socketSuffix)) <= maxSocketPathLen {
		dir = filepath.Join(dir, strings.Repeat("d", 16))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// DELETE against a real session created exactly the way POST /api/v1/sessions creates them:
// `dtach -n`, never attached.
//
// This is the path the Session.pid fix could have broken in silence. deleteSession used to take
// Session.pid — the dtach master — and walk down to the shell itself; now Session.pid *is* the
// shell and it signals that directly. One level off in either direction and the failure is
// invisible to every other test in this package: signal the master while its child lives and you
// get a stale socket plus an orphaned shell, signal a nonexistent child and delete becomes a
// no-op that reports success. Only a real process tree shows the difference, so the socket
// disappearing is not enough — the shell has to be confirmed dead too.
//
// WT_DIR is a t.TempDir. Never ~/.dtach: it holds real sessions on a developer box, possibly the
// one this test is running inside.
func TestDeleteSessionKillsAnUnattachedSession(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}

	dir := t.TempDir()
	workdir := t.TempDir()
	const name = "delete-probe"

	if err := createSession(dir, name, workdir); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	socket := filepath.Join(dir, name+socketSuffix)

	// createSession returns as soon as the socket is observable, which is before the session's
	// shell has run its `cd` — so poll for the enrichment rather than reading it once. Same
	// transient the API exposes to any caller that lists immediately after creating.
	var s Session
	for i := 0; i < 60; i++ {
		sessions, err := listSessions(dir, nil)
		if err == nil && len(sessions) == 1 {
			s = sessions[0]
			if s.PID > 0 && s.CWD == workdir {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.PID == 0 {
		t.Fatalf("no pid resolved for session %q; cannot tell a working delete from a no-op", name)
	}
	// Only reached if an assertion below fails before the delete: nothing else would ever
	// clean this session up, because its master is not in this test's process group.
	t.Cleanup(func() {
		if processAlive(s.PID) {
			_ = syscall.Kill(s.PID, syscall.SIGKILL)
		}
	})

	assertPIDAndCWDAgree(t, s)
	if s.Attached {
		t.Error("attached = true for a session created with dtach -n and never attached")
	}

	if err := deleteSession(dir, name); err != nil {
		t.Fatalf("deleteSession: %v", err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("socket %s still present after delete (stat err = %v)", socket, err)
	}
	// A vanished socket with a live shell is the phantom deleteSession's header exists to
	// prevent, and it is the exact shape a wrong pid level produces.
	waitFor(t, 5*time.Second, func() bool { return !processAlive(s.PID) })
	if processAlive(s.PID) {
		t.Errorf("the session's shell (pid %d) is still running after a successful delete", s.PID)
	}

	if err := deleteSession(dir, name); err != errNotFound {
		t.Errorf("deleting the same session twice returned %v, want errNotFound", err)
	}
}

// The environment of an API-created session, against real dtach — the regression test for #20.
//
// createSession builds its environment from os.Environ(), and wtd runs as a systemd unit, which
// supplies no usable TERM. So TERM has to be set explicitly the way the two /ws paths set it
// (ws.go, hub.go). Left to inheritance it is simply absent, and because the dtach master holds
// this environment for the whole life of the session, every later attach renders without color —
// unrepairable short of deleting the session, since an attaching client's TERM belongs to the
// client and never reaches the shell the master already started.
//
// t.Setenv("TERM", "") is the load-bearing line here, not scaffolding. `go test` inherits the
// developer's TERM, so on any box with a real terminal this test passes against the *unfixed*
// code: createSession would pick xterm-256color out of os.Environ() by accident and the assertion
// would prove nothing. Clearing it first is what reproduces the systemd condition. Empty rather
// than absent because t.Setenv cannot unset — and an empty TERM is equally colorless, so it is a
// faithful stand-in.
//
// WT_DIR is a t.TempDir. Never ~/.dtach: it holds real sessions on a developer box, possibly the
// one this test is running inside.
func TestCreateSessionEnvironmentMatchesThePickerPath(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}
	t.Setenv("TERM", "")

	dir := t.TempDir()
	workdir := t.TempDir()
	const name = "env-probe"

	if err := createSession(dir, name, workdir); err != nil {
		t.Fatalf("createSession: %v", err)
	}

	// Same transient the delete test documents: createSession returns as soon as the socket is
	// observable, which is before the session's shell is resolvable. Poll rather than read once.
	var s Session
	for i := 0; i < 60; i++ {
		sessions, err := listSessions(dir, nil)
		if err == nil && len(sessions) == 1 && sessions[0].PID > 0 {
			s = sessions[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.PID == 0 {
		t.Fatal("no pid resolved for the created session; there is nothing to read an environment from")
	}
	// Nothing else would ever reap this: the master is not in the test's process group.
	t.Cleanup(func() {
		if processAlive(s.PID) {
			_ = syscall.Kill(s.PID, syscall.SIGKILL)
		}
	})

	// Session.pid is the session's *shell*, not the dtach master, which is the process whose
	// environment the user actually gets.
	env := readEnviron(t, s.PID)
	for _, want := range []struct{ key, val string }{
		{"TERM", defaultTerm}, {"WT", "1"}, {"WT_SESSION", name},
	} {
		if got := env[want.key]; got != want.val {
			t.Errorf("session shell (pid %d) has %s=%q, want %q — a session created here must be "+
				"indistinguishable from a picker-made one (session-lifecycle.md §6)",
				s.PID, want.key, got, want.val)
		}
	}
}

// readEnviron reads a process's environment with getenv(3) semantics — the *first* occurrence of a
// key wins — so a caller asserts the value the process actually resolves rather than merely that
// the string was passed to it. The distinction is not academic here: createSession appends TERM to
// a copy of its own environment, so a duplicate key is possible in principle, and glibc would
// answer such a pair with the earlier one while a naive set-membership check called it a pass.
//
// Linux-only, which this project is (CLAUDE.md: "Linux with systemd, full stop"), and the target
// process belongs to the test's own user, so /proc is readable.
func readEnviron(t *testing.T, pid int) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		t.Fatalf("read environ of pid %d: %v", pid, err)
	}
	env := make(map[string]string)
	for _, entry := range strings.Split(string(raw), "\x00") {
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, seen := env[key]; !seen {
			env[key] = val
		}
	}
	return env
}

// The regression test for #5, against real dtach: a running session whose socket path is over the
// AF_UNIX limit must survive being listed.
//
// This was observed, not theorised. reapStale runs on the read path, so a plain
// GET /api/v1/sessions destroyed such a session — dtach binds the long path, connect(2) cannot
// name it, and "connect failed" was read as "nothing is listening". What was left is the
// unrecoverable phantom of session-lifecycle.md §7: master and shell still running, no socket,
// invisible to both pickers.
//
// DELETE is asserted too, because it is the recovery path. Signalling the shell needs no
// connection to the socket, so an unprobeable session must still be deletable — otherwise the fix
// would trade data loss for a session nothing can ever clean up.
func TestALiveSessionSurvivesAPathItCannotBeProbedOn(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}

	const name = "longpath"
	dir := longSessionDir(t, name)
	sock := filepath.Join(dir, name+socketSuffix)
	if len(sock) <= maxSocketPathLen {
		t.Fatalf("test setup: socket path is only %d bytes, need over %d", len(sock), maxSocketPathLen)
	}

	// dtach creates it despite the length — it chdirs and binds a short relative name. That
	// asymmetry, creatable but not connectable, is the entire bug.
	out, err := exec.Command("dtach", "-n", sock, "-z", "-r", "winch",
		"bash", "-c", "exec sleep 300").CombinedOutput()
	if err != nil {
		t.Skipf("dtach declined a %d-byte socket path, so this bug is unreachable here: %v (%s)",
			len(sock), err, out)
	}
	waitFor(t, 5*time.Second, func() bool { _, err := os.Stat(sock); return err == nil })
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("dtach did not create %s: %v", sock, err)
	}

	// /proc still resolves it: scanDtach matches command-line arguments and never connects, so
	// the pid is available even when the socket is not reachable.
	var s Session
	for i := 0; i < 60; i++ {
		sessions, err := listSessions(dir, nil)
		if err == nil && len(sessions) == 1 && sessions[0].PID > 0 {
			s = sessions[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.PID == 0 {
		t.Fatal("no pid resolved: cannot tell a surviving session from a destroyed one")
	}
	t.Cleanup(func() {
		if processAlive(s.PID) {
			_ = syscall.Kill(s.PID, syscall.SIGKILL)
		}
	})

	if got := probeSocket(sock); got != socketUnknown {
		t.Errorf("probeSocket = %v, want socketUnknown for a %d-byte path", got, len(sock))
	}

	if reaped := reapStale(dir); len(reaped) > 0 {
		t.Errorf("reapStale removed %v — the socket of a live session it could not probe", reaped)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the live session's socket was unlinked: %v", err)
	}
	if !processAlive(s.PID) {
		t.Fatal("the session's shell died during the listing")
	}
	// Listed after the reap as well: the session is still a session, not just a surviving file.
	if sessions, err := listSessions(dir, nil); err != nil || len(sessions) != 1 {
		t.Errorf("after reaping, listSessions returned %d sessions (err %v), want 1", len(sessions), err)
	}

	if err := deleteSession(dir, name); err != nil {
		t.Fatalf("deleteSession on an unprobeable session: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return !processAlive(s.PID) })
	if processAlive(s.PID) {
		t.Errorf("the session's shell (pid %d) survived delete", s.PID)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("the socket outlived the session it belonged to (stat err = %v)", err)
	}
}
