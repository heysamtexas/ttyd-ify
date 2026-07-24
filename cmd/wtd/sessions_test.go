package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Every test here uses t.TempDir(). Nothing may point at ~/.dtach: that holds real
// sessions on a developer box, including possibly the one running the test.

func TestListSessionsEmptyAndMissingDir(t *testing.T) {
	got, err := listSessions(t.TempDir())
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty dir returned %d sessions, want 0", len(got))
	}

	// A missing directory is the normal state on a fresh install — bin/wt creates it on
	// first run — so it must read as "no sessions", not as an error.
	got, err = listSessions(filepath.Join(t.TempDir(), "not-created-yet"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing dir returned %d sessions, want 0", len(got))
	}
}

// The attached/idle indicator relies entirely on dtach setting the socket's execute bit.
// This pins the mechanism using real unix sockets, independent of dtach itself.
func TestListSessionsAttachedFromExecBit(t *testing.T) {
	dir := t.TempDir()
	idle := mkSocket(t, dir, "idle.sock", 0o600)
	busy := mkSocket(t, dir, "busy.sock", 0o700) // exec bit set == attached
	_ = idle
	_ = busy

	sessions, err := listSessions(dir)
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(sessions), sessions)
	}

	// Sorted by name, so busy precedes idle.
	if sessions[0].Name != "busy" || !sessions[0].Attached {
		t.Errorf("busy: got %+v, want name=busy attached=true", sessions[0])
	}
	if sessions[1].Name != "idle" || sessions[1].Attached {
		t.Errorf("idle: got %+v, want name=idle attached=false", sessions[1])
	}
}

func TestListSessionsIgnoresNonSockets(t *testing.T) {
	dir := t.TempDir()
	mkSocket(t, dir, "real.sock", 0o600)

	// A plain file named like a socket must not be reported: a stale leftover would
	// otherwise appear as a session that cannot be attached.
	if err := os.WriteFile(filepath.Join(dir, "stale.sock"), []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	// And a socket without the suffix is not ours.
	mkSocket(t, dir, "other", 0o600)

	sessions, err := listSessions(dir)
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name != "real" {
		t.Errorf("got %+v, want exactly the 'real' session", sessions)
	}
}

// Names bin/wt's menu accepts but the create endpoint would reject must still be listed,
// or a session made from the terminal menu would be invisible to the app.
func TestListSessionsReportsNamesCreateWouldReject(t *testing.T) {
	dir := t.TempDir()
	mkSocket(t, dir, "has space.sock", 0o600)
	mkSocket(t, dir, "üñî.sock", 0o600)

	sessions, err := listSessions(dir)
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("got %d sessions, want both unusual names listed: %+v", len(sessions), sessions)
	}
}

// Integration check against real dtach: confirms the pid/cwd enrichment finds the master
// (not the attached client) and reads the session shell's working directory.
func TestListSessionsEnrichesFromRealDtach(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}

	dir := t.TempDir()
	cwd := t.TempDir()
	sock := filepath.Join(dir, "probe.sock")

	// -n creates the session detached, which is exactly how the API will create them.
	cmd := exec.Command("dtach", "-n", sock, "-z", "-r", "winch",
		"bash", "-c", "cd "+cwd+"; exec sleep 300")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dtach -n: %v (%s)", err, out)
	}
	t.Cleanup(func() {
		sessions, _ := listSessions(dir)
		for _, s := range sessions {
			if s.PID > 0 {
				if p, err := os.FindProcess(s.PID); err == nil {
					_ = p.Kill()
				}
			}
		}
	})

	// dtach -n returns before the socket is necessarily observable.
	var sessions []Session
	for i := 0; i < 40; i++ {
		var err error
		sessions, err = listSessions(dir)
		if err == nil && len(sessions) == 1 && sessions[0].PID > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1: %+v", len(sessions), sessions)
	}
	s := sessions[0]
	if s.Name != "probe" {
		t.Errorf("name = %q, want probe", s.Name)
	}
	if s.PID == 0 {
		t.Error("pid = 0, want the dtach master's pid")
	}
	if s.CWD != cwd {
		t.Errorf("cwd = %q, want %q (the session shell's cwd, not the master's)", s.CWD, cwd)
	}
	// Created detached, so nothing is attached and the exec bit should be clear.
	if s.Attached {
		t.Error("attached = true for a session created with dtach -n, want false")
	}
	if s.CreatedAt.IsZero() {
		t.Error("createdAt is zero, want the socket mtime")
	}
}

func mkSocket(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}
