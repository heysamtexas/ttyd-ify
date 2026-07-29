package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Every test here uses t.TempDir(). Nothing may point at ~/.dtach: that holds real
// sessions on a developer box, including possibly the one running the test.

func TestListSessionsEmptyAndMissingDir(t *testing.T) {
	got, err := listSessions(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty dir returned %d sessions, want 0", len(got))
	}

	// A missing directory is the normal state on a fresh install — wtd creates it on
	// first run — so it must read as "no sessions", not as an error.
	got, err = listSessions(filepath.Join(t.TempDir(), "not-created-yet"), nil)
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing dir returned %d sessions, want 0", len(got))
	}
}

// With no hub stats (nil), `attached` falls back to dtach's execute bit — signal 3 in
// attachedTo, and still ground truth for any session no hub holds. This pins that fallback
// using real unix sockets, independent of dtach itself.
func TestListSessionsAttachedFromExecBit(t *testing.T) {
	dir := t.TempDir()
	idle := mkSocket(t, dir, "idle.sock", 0o600)
	busy := mkSocket(t, dir, "busy.sock", 0o700) // exec bit set == attached
	_ = idle
	_ = busy

	sessions, err := listSessions(dir, nil)
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

	sessions, err := listSessions(dir, nil)
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name != "real" {
		t.Errorf("got %+v, want exactly the 'real' session", sessions)
	}
}

// Names the deep-link path accepts but the create endpoint would reject must still be listed,
// or a session made from the terminal menu would be invisible to the app.
func TestListSessionsReportsNamesCreateWouldReject(t *testing.T) {
	dir := t.TempDir()
	mkSocket(t, dir, "has space.sock", 0o600)
	mkSocket(t, dir, "üñî.sock", 0o600)

	sessions, err := listSessions(dir, nil)
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("got %d sessions, want both unusual names listed: %+v", len(sessions), sessions)
	}
}

// Integration check against real dtach: confirms pid and cwd both describe the session's
// *shell* — not the dtach master supervising it, and not an attached client.
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
	// Killing the shell — which is what pid names — lets the master notice its child die and
	// unlink the socket itself. Killing the master instead, which is what pid used to name,
	// leaves the shell orphaned and running for the rest of its 300 seconds.
	t.Cleanup(func() {
		sessions, _ := listSessions(dir, nil)
		for _, s := range sessions {
			if s.PID > 0 {
				if p, err := os.FindProcess(s.PID); err == nil {
					_ = p.Kill()
				}
			}
		}
	})

	// dtach -n returns before the socket is observable, AND before the session's shell has
	// run its `cd`. Until then /proc/<child>/cwd still reports the directory the shell
	// inherited, so waiting only for a pid makes this flaky — and reveals a real transient
	// the API shares: cwd is read live, so a session queried in the instant after creation
	// can legitimately report the wrong directory. It self-corrects, which is why this is a
	// polling test rather than a bug report.
	var sessions []Session
	for i := 0; i < 60; i++ {
		var err error
		sessions, err = listSessions(dir, nil)
		if err == nil && len(sessions) == 1 && sessions[0].PID > 0 && sessions[0].CWD == cwd {
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
		t.Error("pid = 0, want the session shell's pid")
	}
	if s.CWD != cwd {
		t.Errorf("cwd = %q, want %q (the session shell's cwd, not the master's)", s.CWD, cwd)
	}
	assertPIDAndCWDAgree(t, s)
	// Created detached, so nothing is attached and the exec bit should be clear.
	if s.Attached {
		t.Error("attached = true for a session created with dtach -n, want false")
	}
	if s.CreatedAt.IsZero() {
		t.Error("createdAt is zero, want the socket mtime")
	}
}

// The wire shape of an unresolved pid/cwd. openapi.yaml lists both as required and nullable, so
// they must be present and null — never absent.
//
// Nothing asserted the serialized form before, which is exactly how this survived: every consumer
// today is hand-written and tolerant, where absent and null behave identically. A decoder generated
// from the schema is not, and that is the audience the schema exists for.
func TestUnresolvedPIDAndCWDMarshalAsNull(t *testing.T) {
	data, err := json.Marshal(Session{Name: "unresolved", CreatedAt: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	for _, key := range []string{"name", "attached", "attachedCount", "cwd", "pid", "createdAt"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("key %q is absent from %s; openapi.yaml lists it as required", key, data)
		}
	}
	for _, key := range []string{"cwd", "pid"} {
		if got := string(raw[key]); got != "null" {
			t.Errorf("%s = %s, want null for an unresolved value", key, got)
		}
	}

	// A resolved session still carries plain values, not strings-wrapping-numbers or similar.
	data, err = json.Marshal(Session{Name: "resolved", PID: 4242, CWD: "/tmp", Attached: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, `"pid":4242`) || !strings.Contains(got, `"cwd":"/tmp"`) {
		t.Errorf("resolved session marshalled as %s, want pid 4242 and cwd /tmp", got)
	}

	// Decoding must survive the null it now emits, or the API's own round trip breaks — the
	// integration test reads sessions back off the wire.
	var back []Session
	if err := json.Unmarshal([]byte(`[{"name":"x","attached":false,"cwd":null,"pid":null,`+
		`"createdAt":"2026-07-25T00:00:00Z"}]`), &back); err != nil {
		t.Fatalf("decode a null pid/cwd: %v", err)
	}
	if len(back) != 1 || back[0].PID != 0 || back[0].CWD != "" {
		t.Errorf("decoded %+v, want pid 0 and empty cwd", back)
	}
}

// The keys a Session marshals to must be exactly the keys the served schema declares.
//
// Meta has had this check since terminalPath was found published with a description of something
// it had never returned; Session did not, which is how `attachedCount` could have reached the wire
// undeclared, or been declared and never sent. Session is the schema a client models its list view
// on, so a mismatch costs more here than in Meta: a decoder generated from this spec rejects a key
// it was not told about, and a required key that never arrives is a nil dereference on a phone.
func TestSessionMatchesItsSchema(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas struct {
				Session struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"Session"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}
	declared := spec.Components.Schemas.Session.Properties
	if len(declared) == 0 {
		t.Fatal("the embedded spec declares no Session properties; this test would assert nothing")
	}

	// Fully populated, so nothing can be absent merely because it was left unset. MarshalJSON
	// emits every key in both states anyway — that is the contract this also pins.
	data, err := json.Marshal(Session{
		Name: "s", Attached: true, AttachedCount: 1,
		CWD: "/tmp", PID: 1, CreatedAt: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}

	for _, name := range spec.Components.Schemas.Session.Required {
		if _, ok := got[name]; !ok {
			t.Errorf("the spec requires Session.%s but the wire shape omits it", name)
		}
	}
	for name := range got {
		if _, ok := declared[name]; !ok {
			t.Errorf("a Session marshals %q, which the schema does not declare — a client "+
				"generated from the spec would not know about it", name)
		}
	}
}

// attachedCount has three wire states and only two internal fields, so the encoding is the part
// that can go wrong. Zero must mean "nobody" for a detached session and "could not count" for an
// attached one, and a client told 0 viewers when someone is in fact watching would draw exactly
// the wrong conclusion — this field exists so it can warn that someone else is there.
func TestAttachedCountMarshalsItsThreeStates(t *testing.T) {
	cases := []struct {
		name string
		in   Session
		want string
	}{
		{"detached is a known zero", Session{Name: "idle"}, "0"},
		{"counted viewers", Session{Name: "busy", Attached: true, AttachedCount: 2}, "2"},
		{
			// The execute-bit path: dtach says attached, the /proc walk found nobody to count.
			"attached but uncountable", Session{Name: "opaque", Attached: true}, "null",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("unmarshal %s: %v", data, err)
			}
			if got := string(raw["attachedCount"]); got != tc.want {
				t.Errorf("attachedCount = %s, want %s (from %s)", got, tc.want, data)
			}
		})
	}
}

// assertPIDAndCWDAgree checks the invariant behind openapi.yaml's Session.pid: the reported
// pid and the reported cwd describe the *same* process, and that process is the session's shell
// rather than the dtach master supervising it.
//
// Reading cwd back off the reported pid is what catches the two drifting apart, and they did
// drift — pid carried the master while cwd came from its child one level down. Because the two
// pids are usually adjacent integers, the wrong one looked entirely plausible for months, so an
// eyeball check on the JSON is worth nothing here.
func assertPIDAndCWDAgree(t *testing.T, s Session) {
	t.Helper()
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", s.PID))
	if err != nil {
		t.Errorf("readlink /proc/%d/cwd: %v — pid does not name a readable live process", s.PID, err)
		return
	}
	if link != s.CWD {
		t.Errorf("/proc/%d/cwd = %q but the API reports cwd %q: pid and cwd describe two "+
			"different processes", s.PID, link, s.CWD)
	}
	// The direct form of the same claim: a master is a dtach, a shell is whatever the session
	// runs. This one fails loudly even if the two processes happen to share a cwd.
	if c := comm(s.PID); c == "dtach" {
		t.Errorf("pid %d is a dtach process (comm %q): the master was reported instead of the "+
			"session's shell", s.PID, c)
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

// The master of a session is the dtach process whose child is the *shell*. A dtach client that
// created its session with -A also has a child — the master it forked — so "has a child" picks
// the wrong process. This test builds that exact shape with a stand-in named "dtach", because
// /proc/<pid>/comm reports the executable name and the classifier cannot tell the difference.
//
// It is a unit test rather than part of the real-dtach integration test on purpose: with real
// pids the buggy version usually gets the right answer by accident, since both candidates are
// written to the same map key and the later /proc entry wins — which is the real master
// whenever the two pids have the same number of digits. Luck is not a property worth shipping.
func TestSessionShellSkipsANestedDtach(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "dtach")
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	data, err := os.ReadFile(sleepBin)
	if err != nil {
		t.Skip("cannot read sleep binary")
	}
	if err := os.WriteFile(fake, data, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		child     string
		wantShell bool
	}{
		{"child is a dtach: this process is a client", fake, false},
		{"child is not a dtach: this process is the master", sleepBin, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// `& wait` so the shell stays alive as the parent; a bare command would be
			// exec'd by bash and leave no child at all.
			cmd := exec.Command("bash", "-c", tc.child+" 30 & wait")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_, _ = cmd.Process.Wait()
			})

			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) && len(childPIDs(cmd.Process.Pid)) == 0 {
				time.Sleep(20 * time.Millisecond)
			}
			kids := childPIDs(cmd.Process.Pid)
			if len(kids) == 0 {
				t.Fatal("the child process never appeared")
			}

			shell, ok := sessionShell(cmd.Process.Pid)
			if ok != tc.wantShell {
				t.Fatalf("sessionShell = (%d, %v), want ok=%v (children %v, comm %q)",
					shell, ok, tc.wantShell, kids, comm(kids[0]))
			}
			if ok && shell != kids[0] {
				t.Fatalf("sessionShell returned %d, want the child %d", shell, kids[0])
			}
		})
	}
}
