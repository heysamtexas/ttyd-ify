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

// WT_SESSION names the session, on both creation paths, and only where there is one (#111).
//
// A program inside a session has no other way to learn which session it is in: an agent hook is
// spawned with no controlling terminal and the pty carries no name. The three cases below are the
// whole contract, and the third is the one that would be easy to get wrong -- an argless
// connection and an unusable name both land on the fallback shell, where naming the session the
// client asked for would name something that does not exist.
func TestSessionEnvNamesTheSession(t *testing.T) {
	get := func(env []string, key string) (string, bool) {
		for _, kv := range env {
			if name, value, ok := strings.Cut(kv, "="); ok && name == key {
				return value, true
			}
		}
		return "", false
	}

	// Names POST would refuse but a deep link accepts, because the attach side is deliberately
	// the more permissive of the two. A consumer that builds a shell command from this has to
	// quote it, which is why the odd ones are pinned here rather than left to chance.
	for _, name := range []string{"ops", "my session", "café"} {
		t.Run("attach/"+name, func(t *testing.T) {
			dir := t.TempDir()
			cmd, err := attachCommand(dir, name, "")
			if err != nil {
				t.Fatalf("attachCommand(%q): %v", name, err)
			}
			got, ok := get(cmd.Env, "WT_SESSION")
			if !ok {
				t.Fatal("WT_SESSION is absent; a hook inside this session cannot name it")
			}
			if got != name {
				t.Errorf("WT_SESSION = %q, want %q", got, name)
			}
			// The two that used to be maintained separately here must survive the shared builder.
			if v, ok := get(cmd.Env, "WT"); !ok || v != "1" {
				t.Errorf("WT = %q (present=%v), want 1", v, ok)
			}
			if v, ok := get(cmd.Env, "TERM"); !ok || v != defaultTerm {
				t.Errorf("TERM = %q (present=%v), want %s", v, ok, defaultTerm)
			}
		})
	}

	t.Run("fallback shell has none", func(t *testing.T) {
		cmd := fallbackShell(t.TempDir())
		if v, ok := get(cmd.Env, "WT_SESSION"); ok {
			t.Errorf("WT_SESSION = %q on a shell with no session behind it, want absent", v)
		}
		// Absent, not empty: "unset means this is not a session" has to be a usable test, and an
		// empty value would make it a two-part one.
		for _, kv := range cmd.Env {
			if kv == "WT_SESSION=" {
				t.Error("WT_SESSION is set to the empty string; it must be omitted entirely")
			}
		}
		if v, ok := get(cmd.Env, "WT"); !ok || v != "1" {
			t.Errorf("WT = %q (present=%v), want 1 even without a session", v, ok)
		}
	})

	t.Run("empty name omits it", func(t *testing.T) {
		if _, ok := get(sessionEnv(""), "WT_SESSION"); ok {
			t.Error("sessionEnv(\"\") set WT_SESSION")
		}
	})
}
