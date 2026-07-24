package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A start command that ignores SIGHUP must still be reaped when its client vanishes.
//
// This is a regression test for a real leak: the original teardown sent SIGHUP to the pid
// (not the process group) and then called cmd.Wait() with no bound, so a child that
// ignored SIGHUP pinned a process, two fds and two goroutines forever — permanently, not
// slowly. The start command is user-editable bash, so "our start command happens to die on
// hangup" is not a safety property.
//
// It also covers the ordering trap: the client-side pump is only released by cancelling
// the context, so if that cancel is ordered after the wait, the wait can never complete.
func TestTerminalReapsChildThatIgnoresSIGHUP(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "ignores-hup.sh")
	script := "#!/usr/bin/env bash\ntrap '' HUP\necho \"PID:$$\"\nsleep 600\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(newServer(stub).routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := dialTTY(ctx, srv.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// The child reports its own pid so the test can watch for the reap directly rather
	// than inferring it from the server's fd count.
	out := readUntil(ctx, t, conn, "PID:", 10*time.Second)
	pid, err := parsePID(out)
	if err != nil {
		t.Fatalf("%v (output was %q)", err, out)
	}
	if !processAlive(pid) {
		t.Fatalf("child %d not running at the start of the test", pid)
	}

	// Abrupt loss: no close frame, exactly what a phone dropping off a network does.
	_ = conn.CloseNow()

	// The escalation ladder is SIGHUP (2s) then SIGTERM (3s) then SIGKILL, so the child
	// must be gone comfortably inside this window. bash terminates on SIGTERM here since
	// the stub only traps HUP.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return // reaped
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Leave nothing behind for the next test run if the assertion fails.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("child %d still alive 15s after the client vanished: it ignored SIGHUP and "+
		"nothing escalated", pid)
}

// The escalation must reach grandchildren too, not just the immediate child. `sleep` here
// is a separate process in the same group; signalling only the pid would orphan it.
func TestTerminalReapsGrandchildren(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "spawns-child.sh")
	// The inner sleep prints its own pid, and the outer shell ignores HUP so the group
	// signal is the only thing that can clean up.
	script := "#!/usr/bin/env bash\ntrap '' HUP\nsleep 600 &\necho \"PID:$!\"\nwait\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(newServer(stub).routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := dialTTY(ctx, srv.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, handshakeJSON(80, 25)); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	out := readUntil(ctx, t, conn, "PID:", 10*time.Second)
	pid, err := parsePID(out)
	if err != nil {
		t.Fatalf("%v (output was %q)", err, out)
	}

	_ = conn.CloseNow()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild %d survived teardown: the signal went to the pid, not the group", pid)
}

func parsePID(out string) (int, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "\r"))
		if rest, ok := strings.CutPrefix(line, "PID:"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return 0, fmt.Errorf("parse pid from %q: %w", line, err)
			}
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no PID: line in start-command output")
}

// processAlive reports whether pid exists. Signal 0 checks for existence without
// delivering anything; ESRCH means gone, EPERM means alive but not ours.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
