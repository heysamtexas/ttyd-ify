package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// storeFixture is a store over two temp dirs plus one live session socket named "demo",
// which is the shape every restore decision is made against: state on one side, the
// socket that proves the session outlived the server on the other.
func storeFixture(t *testing.T) (*ringStore, func(name string) net.Listener) {
	t.Helper()
	rs := &ringStore{dir: t.TempDir(), sessionDir: t.TempDir()}
	listen := func(name string) net.Listener {
		l, err := net.Listen("unix", filepath.Join(rs.sessionDir, name+socketSuffix))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	return rs, listen
}

// staleSocket leaves the file a kill -9'd dtach master leaves: bound, then closed without
// unlinking — S_ISSOCK passes, connect(2) refuses. The case the liveness gate exists for.
func staleSocket(t *testing.T, rs *ringStore, name string) {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(rs.sessionDir, name+socketSuffix))
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func ringPath(rs *ringStore, name string) string {
	return filepath.Join(rs.dir, name+ringSuffix)
}

func TestRingStoreRoundTripConsumesTheFile(t *testing.T) {
	rs, listen := storeFixture(t)
	listen("demo")

	want := []byte("shell output from before the restart")
	if err := rs.save("demo", want); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(ringPath(rs, "demo"))
	if err != nil {
		t.Fatalf("saved file: %v", err)
	}
	// 0600 is part of the contract, not tidiness: the file is raw terminal output.
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("saved file mode = %o, want 600", got)
	}

	if got := rs.load("demo"); !bytes.Equal(got, want) {
		t.Errorf("load = %q, want %q", got, want)
	}
	if _, err := os.Stat(ringPath(rs, "demo")); !os.IsNotExist(err) {
		t.Errorf("ring file survived its load; a restored tail must not reappear on every spawn")
	}
	if got := rs.load("demo"); got != nil {
		t.Errorf("second load = %q, want nil", got)
	}
}

// A session that died with the server must not have its output replayed into a fresh
// session that inherits its name — and the stale file must still be consumed, or it waits
// forever for that namesake. The stale-socket case is the load-bearing one: a kill -9'd
// master leaves a file that passes S_ISSOCK, and `dtach -A` recreates a session right over
// it, so a stat-based gate replays a dead session into the recreation. probeSocket does not.
func TestRingStoreRefusesADeadSessionsRing(t *testing.T) {
	rs, _ := storeFixture(t)
	staleSocket(t, rs, "killed")

	for _, name := range []string{"gone", "killed"} { // no socket at all; stale socket file
		if err := rs.save(name, []byte("output of a session that is dead now")); err != nil {
			t.Fatalf("save(%q): %v", name, err)
		}
		if got := rs.load(name); got != nil {
			t.Errorf("load(%q) for a dead session = %q, want nil", name, got)
		}
		if _, err := os.Stat(ringPath(rs, name)); !os.IsNotExist(err) {
			t.Errorf("dead session %q's ring file was not consumed", name)
		}
	}
}

// The session name is untrusted network input becoming a filename. Both directions must
// refuse the same names the socket path refuses, or the store is a second, weaker gate.
func TestRingStoreRefusesPathEscapes(t *testing.T) {
	rs, _ := storeFixture(t)

	for _, name := range []string{"", "a/b", "..", "a..b", "../../etc/cron.d/x"} {
		if err := rs.save(name, []byte("x")); err == nil {
			t.Errorf("save(%q) accepted a name validateAttachName rejects", name)
		}
		if got := rs.load(name); got != nil {
			t.Errorf("load(%q) = %q, want nil", name, got)
		}
	}
	entries, err := os.ReadDir(rs.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("refused saves still left %d file(s) in the state dir", len(entries))
	}
}

func TestRingStoreSaveOfNothingWritesNothing(t *testing.T) {
	rs, listen := storeFixture(t)
	listen("demo")

	if err := rs.save("demo", nil); err != nil {
		t.Fatalf("save(nil): %v", err)
	}
	if _, err := os.Stat(ringPath(rs, "demo")); !os.IsNotExist(err) {
		t.Errorf("an empty ring produced a file; absence already means \"nothing to replay\"")
	}
}

func TestRingStoreSweepKeepsOnlyLiveSessions(t *testing.T) {
	rs, listen := storeFixture(t)
	listen("alive")
	staleSocket(t, rs, "killed")

	if err := rs.save("alive", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := rs.save("dead", []byte("drop")); err != nil {
		t.Fatal(err)
	}
	if err := rs.save("killed", []byte("drop: socket file without a listener")); err != nil {
		t.Fatal(err)
	}
	// A .tmp is what a crash mid-save leaves; sweep runs at startup, when no save is in flight.
	torn := filepath.Join(rs.dir, "torn"+ringSuffix+".tmp")
	if err := os.WriteFile(torn, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not wtd's file: sweep must leave it alone rather than treat the directory as disposable.
	other := filepath.Join(rs.dir, "unrelated.txt")
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	rs.sweep()

	if _, err := os.Stat(ringPath(rs, "alive")); err != nil {
		t.Errorf("sweep removed the ring of a live session: %v", err)
	}
	if _, err := os.Stat(ringPath(rs, "dead")); !os.IsNotExist(err) {
		t.Errorf("sweep kept the ring of a dead session")
	}
	if _, err := os.Stat(ringPath(rs, "killed")); !os.IsNotExist(err) {
		t.Errorf("sweep kept the ring of a killed session whose stale socket passes a stat")
	}
	if _, err := os.Stat(torn); !os.IsNotExist(err) {
		t.Errorf("sweep kept a torn .tmp file")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("sweep removed a file that is not its own: %v", err)
	}
}
