package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDtachArgsCreateAndAttachAgree is the point of attach.go.
//
// api/session-lifecycle.md section 6 requires a session created through POST /api/v1/sessions to be
// indistinguishable from one created any other way — same flags, same shell command, same working
// directory handling — because a difference between the two is a fork of the runtime contract that
// nobody can explain later. That used to be two command lines in two languages held together by a
// comment. This asserts it: the only byte that may differ between the create and attach invocations
// is the mode flag.
func TestDtachArgsCreateAndAttachAgree(t *testing.T) {
	const socket = "/tmp/x/demo.sock"
	const workdir = "/home/someone/proj"

	create := dtachArgs("-n", socket, workdir)
	attach := dtachArgs("-A", socket, workdir)

	if len(create) != len(attach) {
		t.Fatalf("different argv lengths: -n %v, -A %v", create, attach)
	}
	if create[0] != "-n" || attach[0] != "-A" {
		t.Fatalf("mode is not the first element: -n %q, -A %q", create[0], attach[0])
	}
	for i := 1; i < len(create); i++ {
		if create[i] != attach[i] {
			t.Errorf("argv[%d] differs: create %q, attach %q", i, create[i], attach[i])
		}
	}
}

// The flags are load-bearing rather than habitual, so pin them by value: -z passes Ctrl-Z through
// to the application, and -r winch is what redraws a client attaching to an idle shell, since dtach
// keeps no screen buffer. Dropping either is a silent degradation on a phone, not a build error.
func TestDtachArgsShape(t *testing.T) {
	got := dtachArgs("-A", "/tmp/d/n.sock", "/home/u")
	want := []string{"-A", "/tmp/d/n.sock", "-z", "-r", "winch", "bash", "-c", "cd '/home/u'; exec bash"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A workdir with a quote in it must not break out of the single-quoted string, because the result is
// a shell command line. bash's ${var@Q} produces the same '\” form.
func TestDtachArgsQuotesTheWorkdir(t *testing.T) {
	got := dtachArgs("-A", "/tmp/s.sock", `/home/it's`)
	cmd := got[len(got)-1]
	if want := `cd '/home/it'\''s'; exec bash`; cmd != want {
		t.Fatalf("got %q, want %q", cmd, want)
	}
	// The apostrophe must never appear unescaped between the quotes that wrap the path.
	if strings.Contains(cmd, `'/home/it's'`) {
		t.Errorf("workdir escaped its quoting: %q", cmd)
	}
}

// validateAttachName is deliberately looser than validateSessionName. The cases that matter are the
// ones POST refuses and the deep link must not: a client can deep-link a session it did not create
// through the API, and api/openapi.yaml promises that. Tightening this to match the create side
// would strand every session made with a space or a leading dot in its name.
func TestValidateAttachNameIsLooserThanCreate(t *testing.T) {
	// Deliberately not t.TempDir(): its path is ~59 bytes, which leaves only 42 characters of
	// room under the 107-byte socket ceiling and would fail the 65-character case for a reason
	// that has nothing to do with name rules. validateAttachName only measures the path, so a
	// short directory that does not exist is the honest fixture here. The ceiling itself is
	// covered by TestValidateAttachNameEnforcesTheSocketPathLimit.
	const dir = "/tmp/d"

	// Accepted here, refused by validateSessionName. Each is a name a real session can have.
	for _, name := range []string{"my project", "café", ".hidden", strings.Repeat("n", 65)} {
		if err := validateAttachName(dir, name); err != nil {
			t.Errorf("validateAttachName(%q) = %v, want nil (the deep link must accept it)", name, err)
		}
		if err := validateSessionName(name); err == nil {
			t.Errorf("validateSessionName(%q) = nil; this test is asserting a difference that no "+
				"longer exists, so one of the two rule sets moved", name)
		}
	}
}

func TestValidateAttachNameRejections(t *testing.T) {
	dir := t.TempDir()

	// The wants below follow the order the checks run in: "/" is tested before "..", so a value
	// containing both is reported as a slash. Either refusal would be correct — what matters is
	// that neither class gets through — so these pin the message a client actually sees rather
	// than asserting a precedence anyone should depend on.
	for _, tc := range []struct {
		name, want string
	}{
		{"", "empty"},
		{"a/b", `"/"`},
		{"/abs", `"/"`},
		{"../etc/passwd", `"/"`},
		{"..", `".."`},
		{"a..b", `".."`},
		{"ok/../bad", `"/"`},
	} {
		err := validateAttachName(dir, tc.name)
		if err == nil {
			t.Errorf("validateAttachName(%q) = nil, want an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("validateAttachName(%q) = %q, want it to mention %s", tc.name, err, tc.want)
		}
	}
}

// The socket-path ceiling must be enforced on the attach path too, not just on POST. This is the
// hole #16 describes, from the other side: dtach binds an over-long path happily, so the session
// exists, nothing can ever attach to it, and no later probe can tell it from a stale socket.
func TestValidateAttachNameEnforcesTheSocketPathLimit(t *testing.T) {
	dir := t.TempDir()
	room := sessionNameRoom(dir)
	if room < 1 {
		t.Skipf("t.TempDir() %q leaves no room for a name; cannot test the boundary here", dir)
	}

	if err := validateAttachName(dir, strings.Repeat("a", room)); err != nil {
		t.Errorf("a name of exactly %d characters fits but was refused: %v", room, err)
	}
	if err := validateAttachName(dir, strings.Repeat("a", room+1)); err == nil {
		t.Errorf("a name of %d characters is one over the limit but was accepted", room+1)
	}
}

// attachWorkdir keeps the picker's silent fallback rather than resolveWorkdir's refusals. The
// difference is intentional and specified: an API caller that silently got $HOME instead of the
// directory it asked for cannot notice, but a human opening a terminal would rather land in $HOME
// than not get a terminal.
func TestAttachWorkdirFallsBackSilently(t *testing.T) {
	home := t.TempDir()
	real := t.TempDir()

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	projects := map[string]string{
		"good":    real,
		"missing": filepath.Join(t.TempDir(), "does-not-exist"),
		"a-file":  file,
		"empty":   "",
		"control": "/tmp/bad\nname",
	}

	for _, tc := range []struct {
		name, want, why string
	}{
		{"good", real, "a shortcut pointing at a real directory is used"},
		{"missing", home, "a shortcut pointing nowhere falls back rather than refusing"},
		{"a-file", home, "a shortcut pointing at a file is not a working directory"},
		{"empty", home, "an empty shortcut value falls back"},
		{"control", home, "a path with control bytes cannot be quoted into bash -c, so it falls back"},
		{"no-such-shortcut", home, "a name that is not a shortcut at all starts in $HOME"},
	} {
		if got := attachWorkdir(projects, tc.name, home); got != tc.want {
			t.Errorf("attachWorkdir(%q) = %q, want %q — %s", tc.name, got, tc.want, tc.why)
		}
	}
}

// WT=1 and the session directory are the two things that used to arrive for free from the picker and
// have to be taken over deliberately. Missing WT=1 gives a user with docs/bashrc-snippet.sh a
// recursive tmux inside every deep-linked session; a missing directory fails the very first deep
// link on a fresh install, inside dtach, where the error is least legible.
func TestAttachCommandSetsTheEnvironmentAndCreatesTheDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-yet")
	home := t.TempDir()

	cmd, err := attachCommand(dir, "demo", home)
	if err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("session dir %q was not created: %v", dir, err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("session dir mode is %o, want 700", perm)
	}

	var sawWT, sawTerm bool
	for _, kv := range cmd.Env {
		switch kv {
		case "WT=1":
			sawWT = true
		case "TERM=" + defaultTerm:
			sawTerm = true
		}
	}
	if !sawWT {
		t.Error("WT=1 is not in the environment; a login shell with docs/bashrc-snippet.sh " +
			"installed would recurse into its multiplexer inside the session")
	}
	if !sawTerm {
		t.Errorf("TERM=%s is not in the environment; the dtach master captures this for the "+
			"life of the session and attaching later cannot repair it", defaultTerm)
	}

	// The socket must sit directly under dir, named for the session.
	want := filepath.Join(dir, "demo"+socketSuffix)
	if !containsArg(cmd.Args, want) {
		t.Errorf("argv %v does not name the socket %q", cmd.Args, want)
	}
}

func TestAttachCommandRefusesAnUnusableName(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()

	for _, name := range []string{"", "a/b", "../x", strings.Repeat("a", 200)} {
		if _, err := attachCommand(dir, name, home); err == nil {
			t.Errorf("attachCommand(%q) = nil error, want a refusal", name)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
