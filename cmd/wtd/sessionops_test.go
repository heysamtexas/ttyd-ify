package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

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
