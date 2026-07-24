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

	// How long to wait for a socket to appear after dtach -n returns.
	createSettleTimeout = 3 * time.Second

	// Grace given to a session's shell to exit before escalating during delete.
	deleteGrace = 3 * time.Second
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

// socketLive reports whether a dtach master is accepting connections on path.
//
// A socket file passing S_ISSOCK is NOT enough: a stale socket passes too, which is
// exactly why phantom sessions appear in bin/wt's menu. Connecting is the only reliable
// test — stale yields ECONNREFUSED.
//
// Nothing is ever written to the socket. A stray byte would be dtach client-protocol
// noise, and its effect on a live session is unverified; this is not the place to find out.
func socketLive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// reapStale unlinks sockets that have no listening master, returning the names removed.
//
// Without this, a stale socket is a session that lists but cannot be attached. bin/wt is
// deliberately not taught to reap (two independent reapers double the race surface), so
// wtd converges both pickers by cleaning up on the read path.
func reapStale(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var reaped []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), socketSuffix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())

		before, err := os.Stat(path)
		if err != nil || before.Mode()&os.ModeSocket == 0 {
			continue
		}
		if socketLive(path) {
			continue
		}

		// Race guard: between the probe and the unlink, a concurrent `dtach -A` can
		// have self-healed this same path by binding a fresh socket. Unlinking then
		// would destroy a live session's socket. Re-stat and compare identity; if
		// anything changed, leave it alone and let the next read handle it.
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(before, after) {
			continue
		}
		if socketLive(path) {
			continue
		}
		if err := os.Remove(path); err == nil {
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
// installed gets a recursive tmux launch inside API-created sessions only.
func createSession(dir, name, workdir string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	socket := filepath.Join(dir, name+socketSuffix)

	// A stale socket at this name gets reaped first, giving the API path the same
	// self-healing that the deep-link path gets for free from `dtach -A`.
	if _, err := os.Stat(socket); err == nil {
		if socketLive(socket) {
			return fmt.Errorf("session %q already exists", name)
		}
		_ = os.Remove(socket)
	}

	// Same flags and same shell command shape as bin/wt:26 and bin/wt:47. The path is
	// single-quoted the way bash's ${var@Q} would do it; control bytes are rejected by
	// the caller rather than escaped, because they cannot be quoted safely here.
	cmd := exec.Command("dtach", "-n", socket, "-z", "-r", "winch",
		"bash", "-c", "cd "+shellQuote(workdir)+"; exec bash")
	cmd.Env = append(os.Environ(), "WT=1")

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
	// removes traversal as a category rather than filtering for it.
	sessions, err := listSessions(dir)
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

	// A stale entry has no master to do the cleanup, so unlinking is all that is left
	// and is safe precisely because nothing is listening.
	if !socketLive(socket) {
		if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
		return nil
	}

	if found.PID == 0 {
		// Live socket but no master identified: signalling nothing is better than
		// guessing, and unlinking would produce the worst phantom of the two.
		return fmt.Errorf("session %q is live but its master could not be identified; refusing to unlink a live socket", name)
	}

	shell, ok := firstChild(found.PID)
	if !ok {
		return fmt.Errorf("session %q has no child shell to terminate", name)
	}

	// SIGHUP to the shell, mirroring what a real hangup does. The master notices its
	// child exit and unlinks the socket itself.
	_ = syscall.Kill(shell, syscall.SIGHUP)

	deadline := time.Now().Add(deleteGrace)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); os.IsNotExist(err) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A shell ignoring SIGHUP would otherwise make delete a no-op that reports success.
	_ = syscall.Kill(shell, syscall.SIGTERM)
	deadline = time.Now().Add(deleteGrace)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); os.IsNotExist(err) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("session %q did not exit after SIGHUP and SIGTERM", name)
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
