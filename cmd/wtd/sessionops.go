package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Session mutation and stale-socket handling. The rules here are specified in
// api/session-lifecycle.md; the comments explain why each one exists, because every
// single one of them prevents a specific observed failure.

const (
	// Session names, create-side. Deliberately stricter than bin/wt's menu — listing
	// stays permissive so the two pickers never disagree about what exists.
	maxSessionNameLen = 64

	// The longest socket path that can be named in a connect(2). sockaddr_un.sun_path is 108
	// bytes on Linux including its NUL terminator; the boundary is exact and was measured —
	// 107 binds, 108 fails with "AF_UNIX path too long".
	//
	// dtach binds a path longer than this anyway (it chdirs and binds a short relative name),
	// so a session behind one is alive and running while being unreachable by name. That
	// asymmetry — creatable but not connectable — is why probeSocket has three answers.
	maxSocketPathLen = 107

	// How long to wait for a socket to appear after dtach -n returns. The DELETE side has no
	// constant here on purpose: it uses `escalation` in ws.go, one table for both places that end
	// a process group.
	createSettleTimeout = 3 * time.Second
)

var errNotFound = errors.New("no such session")

// validateSessionName implements the create-side name rules.
//
// Hand-rolled rather than a regexp because the spec's pattern uses lookahead
// ((?!\.)(?!.*\.\.)) and Go's RE2 has no lookahead — transcribing it as a regexp would
// silently not enforce the two rules that matter most.
func validateSessionName(name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case len(name) > maxSessionNameLen:
		return fmt.Errorf("name is longer than %d characters", maxSessionNameLen)
	case strings.HasPrefix(name, "."):
		// bin/wt's menu globs "$DIR"/*.sock, and a leading dot hides the socket from
		// that glob — the session would exist but be invisible in the terminal picker.
		return errors.New("name may not start with a dot (it would be invisible to the terminal menu)")
	case strings.Contains(name, ".."):
		// bin/wt's direct-attach path drops any argument containing "..", so a session
		// named this way could never be reached by deep link.
		return errors.New(`name may not contain ".." (it would be unreachable by deep link)`)
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-') {
			return fmt.Errorf("name may only contain letters, digits, dot, underscore and hyphen (found %q)", r)
		}
	}
	return nil
}

// socketState is what probing a session socket can tell us. There are three answers, not two,
// and the third is the load-bearing one: "I could not find out" must never collapse into
// "nothing is listening", because only the second one authorizes unlinking.
type socketState int

const (
	// socketUnknown is the zero value deliberately: anything that forgets to set a state fails
	// towards leaving the socket alone, which is the only direction that cannot destroy a
	// running session.
	socketUnknown socketState = iota
	socketListening
	socketRefused
)

// String keeps a mismatch legible: the difference between these states is the difference between
// keeping and destroying a session, and "got 2, want 0" is no way to read that.
func (s socketState) String() string {
	switch s {
	case socketListening:
		return "listening"
	case socketRefused:
		return "refused"
	default:
		return "unknown"
	}
}

// probeSocket asks whether a dtach master is listening on path, and admits when it cannot tell.
//
// A socket file passing S_ISSOCK is NOT enough: a stale socket passes too, which is
// exactly why phantom sessions appear in bin/wt's menu. Connecting is the only reliable
// test — stale yields ECONNREFUSED.
//
// But *only* a refused connection proves nothing is listening. Every other failure is a failure
// to find out, and the two are not interchangeable: acting on "cannot tell" unlinks the socket
// of a running session and leaves its master and shell with no socket — the unrecoverable
// phantom deleteSession's header describes. That is not hypothetical (#5): a path over
// maxSocketPathLen cannot be named in a sockaddr_un, dtach binds one anyway, and the live
// session behind it was destroyed by nothing more than a listing.
//
// Nothing is ever written to the socket. A stray byte would be dtach client-protocol
// noise, and its effect on a live session is unverified; this is not the place to find out.
func probeSocket(path string) socketState {
	// Checked before dialing rather than read back out of the error, so the boundary is stated
	// in one place and testable without a syscall.
	if !nameable(path) {
		return socketUnknown
	}

	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return socketListening
	}
	// ECONNREFUSED is the stale answer: the file is there, nothing is bound to it. ENOENT is the
	// same conclusion by a different route — there is nothing left to unlink either way.
	// Anything else (a permission error, a full listen backlog, a timeout on a loaded box) means
	// a master may well be sitting there, so it is not permission to delete anything.
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
		return socketRefused
	}
	return socketUnknown
}

// nameable reports whether path can be expressed in a sockaddr_un at all. An unnameable path is
// not merely hard to probe — connect(2) cannot be *asked* about it, which is why provablyDead
// treats it differently from a dial that failed.
func nameable(path string) bool { return len(path) <= maxSocketPathLen }

// provablyDead reports whether path may be unlinked: nothing is behind it, and that is a
// conclusion rather than an assumption.
//
// Two ways to reach the same standard. A refused connection is the direct proof. For a path
// connect(2) cannot express, the probe is *impossible* rather than negative, so /proc answers the
// real question instead — does any live dtach process hold this socket — without needing to name
// it. That is stronger evidence than the probe it stands in for, not weaker.
//
// A dial that merely failed on a nameable path is a different thing and stays unknown: a
// permission error or a full listen backlog means a master may well be sitting there. So the /proc
// fallback is deliberately *not* extended to it. Widening it would mean that on a box where /proc
// is unreadable (hidepid), every socket reads as unheld and the reaper eats the lot.
//
// Without the second clause a stale unnameable socket would be immortal: unreapable here,
// undeletable through deleteSession, listed as a session forever, and a phantom in bin/wt's menu
// after any reboot — which api/session-lifecycle.md calls the common case for staleness, since
// socket files survive a boot and no master does.
func provablyDead(path string, referenced func(string) bool) bool {
	switch probeSocket(path) {
	case socketRefused:
		return true
	case socketUnknown:
		return !nameable(path) && !referenced(path)
	}
	return false
}

// referencedIn returns a predicate answering "does any live dtach process name this socket",
// walking /proc at most once and only when first asked. Most deployments never hold an unprobeable
// socket, so the walk usually never happens.
//
// The returned closure memoizes and is NOT safe for concurrent use. Each caller builds its own,
// which also keeps the answer consistent for the whole of one reap pass.
func referencedIn(dir string) func(string) bool {
	var held map[string]bool
	return func(path string) bool {
		if held == nil {
			held = map[string]bool{}
			shells, clients := scanDtach(dir)
			for p := range shells {
				held[p] = true
			}
			for p, pids := range clients {
				if len(pids) > 0 {
					held[p] = true
				}
			}
		}
		return held[path]
	}
}

// unlinkStale removes path if it is provably not in use, reporting whether it did.
//
// The double check is a race guard, not belt-and-braces. Between deciding a socket is dead and
// unlinking it, a concurrent `dtach -A` can self-heal that same path by binding a fresh socket — a
// phone reconnecting on a `sessionArg` deep link does exactly this, unprompted. Unlinking then
// destroys a live session's socket and produces the unlink-first phantom: master and shell running
// with nothing to attach to, and nothing recovers it but a manual kill. So identity is compared
// across the window with os.SameFile, and the liveness question is asked again on the far side.
//
// Both unlink sites in this file go through here. api/session-lifecycle.md §7 step 2 requires the
// guard on the DELETE path too, and having one implementation is the only way that stays true.
func unlinkStale(path string, referenced func(string) bool) (bool, error) {
	before, err := os.Stat(path)
	if err != nil || before.Mode()&os.ModeSocket == 0 {
		return false, nil
	}
	if !provablyDead(path, referenced) {
		return false, nil
	}

	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		return false, nil // replaced under us; leave it and let the next read decide
	}
	if !provablyDead(path, referenced) {
		return false, nil
	}

	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

// sessionNameRoom is how many characters of session name fit in dir before the socket path
// exceeds what connect(2) can name.
//
// The one place this arithmetic lives. Both the create-side refusal and the startup warning read
// it, so the server cannot warn about a limit it does not enforce — or, worse, enforce one it
// never mentioned. filepath.Join in both directions means an odd dir (a trailing slash, say)
// cannot make the two disagree either.
func sessionNameRoom(dir string) int {
	return maxSocketPathLen - len(filepath.Join(dir, socketSuffix))
}

// validateSocketPath rejects a name whose socket could never be reached even once created.
//
// This cannot live in validateSessionName: it is as much a function of WT_DIR's depth as of the
// name. Refusing up front beats the alternative, which is not a failure but something worse —
// dtach creates the session happily, and every later probe is then unable to tell it from a
// stale socket (#5).
func validateSocketPath(dir, name string) error {
	room := sessionNameRoom(dir)
	if len(name) <= room {
		return nil
	}
	// One message for both cases. room can be 0 or negative under a deep enough dir, and "at most
	// 0 characters fit" is the true and useful thing to say there.
	return fmt.Errorf("its socket path would be %d bytes, over the %d-byte limit for a unix "+
		"socket: at most %d characters of name fit under %q",
		len(filepath.Join(dir, name+socketSuffix)), maxSocketPathLen, max(room, 0), dir)
}

// reapStale unlinks sockets that are provably not listening, returning the names removed.
//
// Without this, a stale socket is a session that lists but cannot be attached. bin/wt is
// deliberately not taught to reap (two independent reapers double the race surface), so
// wtd converges both pickers by cleaning up on the read path.
//
// "Provably" is the whole contract, because this runs on the *read* path: merely listing
// sessions deletes files, with no confirmation and nothing reported to the caller. The standard
// and the race guard both live in unlinkStale; this function only decides what to offer it.
func reapStale(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	// One predicate for the whole pass: /proc is walked at most once, and every socket in this
	// listing is judged against the same snapshot of what is running.
	referenced := referencedIn(dir)

	var reaped []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), socketSuffix) {
			continue
		}
		if ok, _ := unlinkStale(filepath.Join(dir, entry.Name()), referenced); ok {
			reaped = append(reaped, strings.TrimSuffix(entry.Name(), socketSuffix))
		}
	}
	return reaped
}

// createSession creates a detached dtach session, matching bin/wt's invocation exactly.
//
// Parity is not cosmetic: a session created here must be indistinguishable from one made
// through the terminal menu, or the two paths drift and users hit differences nobody can
// explain. In particular WT=1 must be present, or a user with docs/bashrc-snippet.sh
// installed gets a recursive tmux launch inside API-created sessions only; and TERM must be
// set here rather than inherited, or the session is colorless for its entire life.
func createSession(dir, name, workdir string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	// The HTTP handler checks this too and is the enforcement point that produces a 400; this copy
	// is for callers that are not the handler, and mirrors the name check above.
	if err := validateSocketPath(dir, name); err != nil {
		return fmt.Errorf("session %q cannot be created: %w", name, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	socket := filepath.Join(dir, name+socketSuffix)

	// A stale socket at this name gets reaped first, giving the API path the same
	// self-healing that the deep-link path gets for free from `dtach -A`. Only a provable
	// refusal earns that: an unprobeable socket is reported as an existing session rather than
	// silently replaced, because replacing it would orphan whatever is behind it.
	if _, err := os.Stat(socket); err == nil {
		if probeSocket(socket) != socketRefused {
			return fmt.Errorf("session %q already exists", name)
		}
		_ = os.Remove(socket)
	}

	// Same flags and same shell command shape as bin/wt:26 and bin/wt:47. The path is
	// single-quoted the way bash's ${var@Q} would do it; control bytes are rejected by
	// the caller rather than escaped, because they cannot be quoted safely here.
	cmd := exec.Command("dtach", "-n", socket, "-z", "-r", "winch",
		"bash", "-c", "cd "+shellQuote(workdir)+"; exec bash")
	// TERM is the same constant the two /ws paths set (ws.go, hub.go). It cannot be left to
	// inheritance the way the rest of the environment is: wtd runs as a systemd unit, which
	// supplies no usable TERM, and the dtach master captures this environment for the whole life
	// of the session. Attaching later cannot repair it — the attaching client's TERM belongs to
	// the client, not to the shell the master already started — so a session born without it
	// stays colorless until it is deleted and recreated.
	cmd.Env = append(os.Environ(), "WT=1", "TERM="+defaultTerm)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dtach -n: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// dtach -n returns before the socket is necessarily observable, and a caller that
	// immediately lists would not see what it just made.
	deadline := time.Now().Add(createSettleTimeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("session %q did not appear within %s", name, createSettleTimeout)
}

// deleteSession terminates a session, killing from the leaves.
//
// The ordering is inviolable, and both alternatives were observed to produce phantoms:
//
//   - Unlink first: master and shell keep running with no socket — a live session
//     invisible to both pickers and impossible to attach or delete ever again.
//   - Kill the master first: stale socket AND an orphaned, still-running shell.
//
// So: signal the session's shell and let dtach clean up after itself. The master unlinks
// its own socket when its child dies.
func deleteSession(dir, name string) error {
	// The request string never builds a path. Enumerate and byte-match instead, which
	// removes traversal as a category rather than filtering for it. Attachment state is
	// irrelevant here — deleting an attached session is legal — so no hub stats are needed.
	sessions, err := listSessions(dir, nil)
	if err != nil {
		return err
	}
	var found *Session
	for i := range sessions {
		if sessions[i].Name == name {
			found = &sessions[i]
			break
		}
	}
	if found == nil {
		return errNotFound
	}

	socket := filepath.Join(dir, found.Name+socketSuffix)

	if _, err := os.Stat(socket); os.IsNotExist(err) {
		// Vanished between the listing and now. Gone is what was asked for.
		return nil
	}

	// A stale entry has no master to do the cleanup, so unlinking is all that is left and is safe
	// precisely because nothing is behind it. Same standard and same race guard as the reaper —
	// api/session-lifecycle.md §7 step 2 requires the guard here too, and this is the call that
	// makes that true.
	removed, err := unlinkStale(socket, referencedIn(dir))
	if err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	if removed {
		return nil
	}

	// Anything still here is live, or unprobeable with something behind it. Both are handled by
	// signalling the shell, which needs no connection to the socket — the master unlinks its own
	// socket on the way out. So DELETE keeps working on a session whose path is too long to probe,
	// and is the recovery path for #5 rather than another casualty of it.

	if found.PID == 0 {
		// A live socket whose master could not be read. Signalling nothing is better than
		// guessing, and unlinking would produce the worst phantom of the two. The probe state is
		// in the message because it is the only thing that separates the causes, and an operator
		// reading a 500 has nothing else to go on.
		return fmt.Errorf("session %q could not be resolved to a shell and its socket is not "+
			"provably stale (probe: %s); refusing to unlink a socket that may have a live "+
			"session behind it", name, probeSocket(socket))
	}

	// found.PID is the session's shell, resolved by scanDtach through the master-vs-client rule
	// documented there. Assert that here anyway, at the point of the signal: "never signal the
	// master while its child lives" is the rule this function exists to keep, and a master is a
	// dtach while a shell is not. One /proc read, and it catches both a regression that puts the
	// master back in Session.pid and a cmdline collision in scanDtach's map.
	if c := comm(found.PID); c == "dtach" {
		return fmt.Errorf("session %q resolved to pid %d, which is a dtach process (comm %q) "+
			"rather than a shell; refusing to signal the master", name, found.PID, c)
	}

	// SIGHUP, then SIGTERM, then SIGKILL — the same ladder and graces a terminal's teardown uses,
	// which is also what api/session-lifecycle.md §7 step 4 specifies. Each rung waits for the
	// master to unlink its own socket, which is how it reports that its child is gone.
	//
	// SIGKILL matters: the shell is user-controlled and leak_test proves a start command can trap
	// signals, so without it a shell trapping HUP and TERM turns DELETE into a 500 that leaves the
	// session running — a refusal that looks like a failure but is really an abandonment.
	for _, step := range escalation {
		signalSession(found.PID, step.sig)
		if socketGone(socket, step.grace) {
			return nil
		}
	}

	// Past SIGKILL a process only survives in uninterruptible sleep. The socket is left alone: it
	// still has a master, and the next listing reaps it once that master exits.
	return fmt.Errorf("session %q survived SIGHUP, SIGTERM and SIGKILL; %s is still there",
		name, socket)
}

// signalSession delivers sig to the session's process group when its shell leads one, and to the
// shell alone otherwise.
//
// The group is the point. A shell's background jobs live in it, and signalling only the pid leaves
// them running as orphans after the session they belonged to is gone — which is the behavior
// api/session-lifecycle.md §7 asks for ("background jobs die too") and what ws.go's teardown
// already does for a terminal's own children.
//
// Getpgid decides rather than assuming, because kill(-pid) against a pid that leads no group is
// ESRCH — a silent no-op, which is strictly worse than signalling the one process we do know about.
func signalSession(pid int, sig syscall.Signal) {
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pgid, sig) // ESRCH here just means it already exited
		return
	}
	_ = syscall.Kill(pid, sig)
}

// socketGone waits up to grace for the socket to disappear, reporting whether it did. A dtach
// master unlinks its own socket when its child dies, so the file vanishing is the session's death
// certificate — more reliable than watching the pid, which says nothing about whether dtach has
// finished cleaning up.
func socketGone(socket string, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); os.IsNotExist(err) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// shellQuote single-quotes s for bash, the same way ${var@Q} does. Callers must reject
// control bytes first; this handles embedded quotes only.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hasControlBytes reports whether s contains bytes that cannot be safely placed in the
// `bash -c` command line built above.
func hasControlBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}
