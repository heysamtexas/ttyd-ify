package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The built-in terminal path: wtd attaching to dtach itself, with no external start command.
//
// This file exists because of a coverage trap. Every other protocol test in this package passes a
// stub start command, so they all keep passing whether or not the built-in path works — they cover
// the ttyd-compatible override, which is not what a real box runs. Without the tests below, the hot
// path would have no coverage at all while the suite stayed green (#47 is the same failure mode one
// layer down).

func builtinServer(t *testing.T) (app *server, dir, home, base string) {
	t.Helper()
	dir = t.TempDir()
	home = t.TempDir()
	t.Setenv("WT_DIR", dir)
	// A developer box symlinks ~/.config/wt/projects at the live /etc/ttyd-ify/projects, so an
	// unset value here would read real shortcuts and make this test depend on the box.
	t.Setenv("WT_PROJECTS", filepath.Join(dir, "no-projects"))
	t.Setenv("HOME", home)
	// HISTFILE, or this fixture flakes on its own cleanup (#131).
	//
	// The shells these tests spawn are interactive, so bash writes its history file when the
	// connection closes -- into the very t.TempDir() that HOME points at, which the test is
	// concurrently removing. Whichever wins is timing, and the loser is `TempDir RemoveAll
	// cleanup: directory not empty` against code that is working perfectly. It reproduced at
	// roughly two failures in thirty runs and reddened a real CI job.
	//
	// Set here rather than in fallbackShell, which is production: a shell in a real person's home
	// directory *should* write history. It is only a problem because HOME is a directory with a
	// destructor. /dev/null rather than unsetting, because t.Setenv cannot unset, and bash
	// falls back to ~/.bash_history when HISTFILE is empty.
	t.Setenv("HISTFILE", "/dev/null")

	// The empty start command is the whole point: it selects the built-in path.
	app, base = hubTestServer(t, "", defaultReplayBytes, defaultMaxWarmHubs)
	return app, dir, home, base
}

// hubChildPGID reports the command name and process group of the process a hub holds.
//
// Read from /proc rather than inferred, because two things in the server depend on the answer and
// neither would fail loudly if it changed: hub.terminate signals the process *group* (-pgid), and
// survival.go reasons about the dtach master staying in wtd's cgroup because `dtach -A` forks it as
// a child of the client. Both were true when bin/wt exec'd dtach, so the pid wtd held *became* the
// dtach client. They stay true only because wtd now spawns dtach directly.
func hubChildPGID(t *testing.T, pid int) (comm string, pgid int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatalf("read /proc/%d/stat: %v", pid, err)
	}
	// comm is parenthesised and may itself contain spaces, so split on the LAST ")".
	line := string(raw)
	close := strings.LastIndex(line, ")")
	open := strings.Index(line, "(")
	if open < 0 || close < open {
		t.Fatalf("unparseable /proc/%d/stat: %q", pid, line)
	}
	comm = line[open+1 : close]
	// After "<pid> (comm) " the fields are state, ppid, pgrp, ...
	fields := strings.Fields(line[close+1:])
	if len(fields) < 3 {
		t.Fatalf("unparseable /proc/%d/stat tail: %q", pid, line)
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		t.Fatalf("pgrp in /proc/%d/stat: %v", pid, err)
	}
	return comm, pgid
}

// An external start command must still be run verbatim with the connection's argv. This is ttyd's
// `-a` contract, the conformance job depends on it, and it is the rollback if the built-in path
// misbehaves on a real box.
func TestTerminalCommandRunsAnExternalStartCommand(t *testing.T) {
	s := newServer("/opt/custom/start-command")

	tc, err := s.terminalCommand([]string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	if got := tc.cmd.Args; len(got) != 2 || got[0] != "/opt/custom/start-command" || got[1] != "demo" {
		t.Errorf("argv = %v, want [/opt/custom/start-command demo]", got)
	}
	if tc.label != "/opt/custom/start-command" {
		t.Errorf("label = %q, want the start command", tc.label)
	}
	if tc.notice != "" {
		t.Errorf("notice = %q, want none", tc.notice)
	}
}

func TestTerminalCommandArglessGetsAShell(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newServer("")

	// Both shapes of "no session": no args at all, and an empty ?arg=. api/ws-protocol.md treats
	// them identically, so the builder must too.
	for _, args := range [][]string{nil, {""}} {
		tc, err := s.terminalCommand(args)
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if !strings.HasSuffix(tc.cmd.Path, "bash") {
			t.Errorf("args %v: command is %q, want a bash", args, tc.cmd.Path)
		}
		if tc.cmd.Dir != home {
			t.Errorf("args %v: Dir = %q, want %q — wtd's own cwd is / under systemd", args, tc.cmd.Dir, home)
		}
		if !hasEnv(tc.cmd.Env, "WT=1") {
			t.Errorf("args %v: WT=1 missing", args)
		}
	}
}

func TestTerminalCommandNamedAttachesWithDtach(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WT_DIR", dir)
	t.Setenv("WT_PROJECTS", filepath.Join(dir, "no-projects"))
	t.Setenv("HOME", t.TempDir())
	s := newServer("")

	tc, err := s.terminalCommand([]string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(tc.cmd.Path, "dtach") {
		t.Errorf("command is %q, want dtach", tc.cmd.Path)
	}
	sock := filepath.Join(dir, "demo"+socketSuffix)
	if !containsArg(tc.cmd.Args, sock) || !containsArg(tc.cmd.Args, "-A") {
		t.Errorf("argv %v does not attach to %q with -A", tc.cmd.Args, sock)
	}
	if tc.label != "demo" {
		t.Errorf("label = %q, want the session name", tc.label)
	}
	if tc.notice != "" {
		t.Errorf("notice = %q, want none for a usable name", tc.notice)
	}
}

// An unusable name must not be an error. api/openapi.yaml publishes that no value of `arg` closes
// the connection, so the builder owes the caller a working command plus an explanation.
func TestTerminalCommandUnusableNameFallsBackWithANotice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WT_DIR", dir)
	t.Setenv("WT_PROJECTS", filepath.Join(dir, "no-projects"))
	t.Setenv("HOME", t.TempDir())
	s := newServer("")

	for _, name := range []string{"a/b", "../escape", strings.Repeat("n", 200)} {
		tc, err := s.terminalCommand([]string{name})
		if err != nil {
			t.Errorf("terminalCommand(%q) returned an error (%v); an unusable name must degrade, "+
				"not fail — 1011 would make a client with a typo'd profile backoff-loop", name, err)
			continue
		}
		if !strings.HasSuffix(tc.cmd.Path, "bash") {
			t.Errorf("terminalCommand(%q): command is %q, want a fallback bash", name, tc.cmd.Path)
		}
		if tc.notice == "" {
			t.Errorf("terminalCommand(%q): no notice; the user would silently get a shell where "+
				"they expected their session", name)
		}
		if tc.label != name {
			t.Errorf("terminalCommand(%q): label = %q, want the name so the connection stays "+
				"identifiable", name, tc.label)
		}
	}
}

// End-to-end, no bash in the middle: a named connection creates and attaches to a real dtach
// session, and a second client on the same name is replayed what the first one did.
//
// This is the claim the whole fold rests on, and only real dtach can check it.
func TestBuiltinNamedConnectionAttachesRealDtach(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}
	app, dir, _, base := builtinServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const name = "builtin-demo"
	first := attach(ctx, t, base, name, 80, 25)

	// A real shell, reached without a picker: ask it to identify itself.
	if err := writeFrame(ctx, first, opInput, []byte("printf 'PID%s\\n' \":$$\"\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	out := readUntil(ctx, t, first, "PID:", 20*time.Second)
	shellPID, err := findPID(out)
	if err != nil {
		t.Fatalf("%v (output was %q)", err, out)
	}
	t.Cleanup(func() { _ = deleteSession(dir, name) })

	// dtach owns the session, so its socket is on disk under WT_DIR.
	sock := filepath.Join(dir, name+socketSuffix)
	waitFor(t, 15*time.Second, func() bool { _, err := os.Stat(sock); return err == nil })
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("no dtach socket at %s: %v — the built-in path did not create a session", sock, err)
	}

	// WT=1 must reach the session's shell. Without it a user with docs/bashrc-snippet.sh installed
	// gets a recursive multiplexer inside every deep-linked session, and the picker used to be
	// what set it.
	// Marker split across the format string, same as the PID probe above: the pty echoes the typed
	// line, so a literal "WT[" in the command would match the echo and this would assert on its own
	// input. It read as a pass in isolation and a failure in the full suite purely on timing.
	if err := writeFrame(ctx, first, opInput, []byte("printf 'WT%s\\n' \"[$WT]\"\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if got := readUntil(ctx, t, first, "WT[", 20*time.Second); !strings.Contains(got, "WT[1]") {
		t.Errorf("WT is not 1 in the session's shell; got %q", got)
	}

	// WT_SESSION must reach it too, and carry this session's name (#111). This is the only
	// assertion that the deep-link path delivers it to a real shell -- the unit test covers what
	// is handed to exec, which is not the same claim. Same split-marker trick and for the same
	// reason: the pty echoes the line, so an unsplit literal would match its own input.
	if err := writeFrame(ctx, first, opInput, []byte("printf 'SESS%s\n' \"[$WT_SESSION]\"\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if got := readUntil(ctx, t, first, "SESS[", 20*time.Second); !strings.Contains(got, "SESS["+name+"]") {
		t.Errorf("WT_SESSION is not %q in the session's shell; got %q — a hook inside this "+
			"session cannot name the session it is in", name, got)
	}

	// Replay: a second client on the same name joins the same hub and is sent what already
	// happened, which is the feature the server exists for.
	second := attach(ctx, t, base, name, 80, 25)
	if got := readUntil(ctx, t, second, "PID:", 20*time.Second); !strings.Contains(got, "PID:") {
		t.Errorf("a second client on %q saw no replay of the first client's output; got %q", name, got)
	}

	if got := sessionByName(t, base, name).PID; got != shellPID {
		t.Errorf("the API reports pid %d for %q, want the shell's own %d", got, name, shellPID)
	}

	// The process shape the rest of the server assumes. Removing bin/wt could only have broken this
	// by making wtd hold a shell that forks dtach instead of being dtach, and nothing else in the
	// suite would have noticed: teardown signals -pgid, so a wrapper process would leave the dtach
	// client in a different group and detach would stop working, quietly.
	app.hubs.mu.Lock()
	var hubPID int
	for _, h := range app.hubs.m {
		if h.name == name {
			hubPID = h.cmd.Process.Pid
		}
	}
	app.hubs.mu.Unlock()
	if hubPID == 0 {
		t.Fatalf("no hub held for %q", name)
	}
	comm, pgid := hubChildPGID(t, hubPID)
	if comm != "dtach" {
		t.Errorf("the hub holds %q (pid %d), want dtach directly — a wrapper process breaks the "+
			"-pgid teardown and the cgroup reasoning in survival.go", comm, hubPID)
	}
	if pgid != hubPID {
		t.Errorf("hub child pid %d has pgid %d; they must be equal or terminate() signals the "+
			"wrong group (hub.go relies on pty.Start making the child a session leader)", hubPID, pgid)
	}

	_ = first.CloseNow()
	_ = second.CloseNow()
}

// An unusable name must produce a working terminal, never a close. api/openapi.yaml publishes "No
// value of arg closes the connection", and the shipped iOS client backs off and retries on 1011 —
// so closing here would make one typo in a saved profile loop instead of showing its user anything.
func TestBuiltinUnusableNameGivesAShellAndStaysOpen(t *testing.T) {
	_, _, _, base := builtinServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Over the socket-path ceiling: long enough to be unusable, short enough that filterArgs keeps
	// it (its limit is 4096 bytes).
	bad := strings.Repeat("n", 200)
	conn := attach(ctx, t, base, bad, 80, 25)
	t.Cleanup(func() { _ = conn.CloseNow() })

	if got := readUntil(ctx, t, conn, "wtd:", 20*time.Second); !strings.Contains(got, "wtd:") {
		t.Errorf("no notice explaining why the session could not be used; got %q", got)
	}

	// And it is a live shell, not a corpse: the connection must still serve a terminal.
	if err := writeFrame(ctx, conn, opInput, []byte("printf 'ALIVE%s\\n' \":$$\"\n")); err != nil {
		t.Fatalf("the connection was closed rather than degraded: %v", err)
	}
	if got := readUntil(ctx, t, conn, "ALIVE:", 20*time.Second); !strings.Contains(got, "ALIVE:") {
		t.Errorf("the fallback shell does not respond to input; got %q", got)
	}
}

// An argless connection gets a private shell rather than a picker, and it must be a working one.
func TestBuiltinArglessGetsAWorkingShell(t *testing.T) {
	_, _, home, base := builtinServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn := attach(ctx, t, base, "", 80, 25)
	t.Cleanup(func() { _ = conn.CloseNow() })

	// The marker is split across printf's format string on purpose, the same way findPID's callers
	// do it: the pty echoes the typed line back, so a literal "CWD:" in the command would be
	// matched in the echo and the assertion would read the input instead of the output.
	if err := writeFrame(ctx, conn, opInput, []byte("printf 'CWD%s\\n' \":$PWD\"\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	got := readUntil(ctx, t, conn, "CWD:", 20*time.Second)
	if !strings.Contains(got, "CWD:"+home) {
		t.Errorf("argless shell started in the wrong directory; want %q, got %q", home, got)
	}
}

func hasEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}
