package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Building the dtach invocation, for both the create side and the attach side.
//
// Two things share this file because they must not drift: `POST /api/v1/sessions` creates a session
// detached (`dtach -n`) and a deep-linked `/ws?arg=` connection creates-or-attaches to one
// (`dtach -A`). Every other byte of the two command lines is identical, and has to be —
// api/session-lifecycle.md section 6 requires an API-created session to be indistinguishable from
// one made any other way, because a difference is a fork of the runtime contract. That requirement
// used to be prose plus a hope; dtachArgs makes the two argv lines literally the same code, and
// TestDtachArgsCreateAndAttachAgree is the assertion.
//
// The name and workdir rules here are the *attach* rules, and they are deliberately not the create
// rules. See validateAttachName and attachWorkdir: both are looser than their API counterparts, on
// purpose, and tightening either one to match would break clients that work today.

// dtachArgs builds the argv for a dtach session, shared by create and attach.
//
// mode is "-n" (create detached, never attach) or "-A" (attach, creating if absent). The flags are
// not incidental: -z passes Ctrl-Z through to the application instead of suspending dtach, and
// -r winch redraws on attach because dtach keeps no screen buffer of its own — a client attaching
// to an idle shell would otherwise see nothing at all until something wrote.
//
// The workdir is single-quoted the way bash's ${var@Q} would do it. Callers must reject control
// bytes first (hasControlBytes) rather than expect escaping: api/session-lifecycle.md section 6
// declines to reproduce @Q's $'...' mode, because "our Go quoter perfectly reproduces bash's @Q in
// all cases" is the kind of claim that ends up false. Refusing the input keeps shellQuote trivially
// auditable.
func dtachArgs(mode, socket, workdir string) []string {
	return []string{
		mode, socket,
		"-z", "-r", "winch",
		"bash", "-c", "cd " + shellQuote(workdir) + "; exec bash",
	}
}

// validateAttachName implements the deep-link name rules — everything a `?arg=` value must satisfy
// before it can become a socket path.
//
// Deliberately NOT validateSessionName, and this is the trap in this file. The create side refuses
// spaces, non-ASCII, names over 64 characters and a leading dot; the attach side must accept all of
// those, because the spec promises a client can deep-link a session it did not create through the
// API (api/openapi.yaml, the arg parameter). A session named "my project" is legal, listable, and
// reachable — tightening this to match POST would strand it.
//
// So only two classes are refused, and each for a mechanical reason rather than taste:
//
//   - "/" and ".." would escape WT_DIR. The name arrives from a client over the network and is
//     joined into a filesystem path, so this is the enforcement point, not a nicety.
//   - A socket path over maxSocketPathLen cannot be named in a connect(2) at all. dtach binds one
//     anyway, which is worse than an error: the session exists, nothing can ever attach to it, and
//     no later probe can distinguish it from a stale socket (#5). Refusing up front is the only
//     answer that leaves the directory in a state the server can reason about.
//
// The caller must not treat an error here as fatal to the connection. api/openapi.yaml publishes
// that no value of arg closes the connection; an unusable name degrades to a plain shell.
func validateAttachName(dir, name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case strings.ContainsRune(name, '/'):
		return errors.New(`name may not contain "/"`)
	case strings.Contains(name, ".."):
		return errors.New(`name may not contain ".."`)
	}
	// Shares the arithmetic with the create side and the startup warning, so the server cannot
	// enforce a ceiling it never mentioned or warn about one it does not enforce.
	return validateSocketPath(dir, name)
}

// attachWorkdir resolves the directory a deep-linked session starts in, falling back silently.
//
// Deliberately NOT resolveWorkdir, the second trap in this file. That function refuses an unknown
// project or a project path that has gone missing, and it is right to: an API caller that asked for
// a specific directory and silently got $HOME has no way to notice. This path has no caller to
// report to — it is a human opening a terminal — so the useful behaviour is the one the picker had:
// a name that matches no shortcut, or a shortcut pointing somewhere that no longer exists, starts
// in $HOME rather than refusing to open a terminal at all.
//
// A path with control bytes in it also falls back, because it cannot be quoted safely into the
// `bash -c` line dtachArgs builds. That case is a malformed projects file rather than anything a
// client controls: the file is root-owned on a real install.
func attachWorkdir(projects map[string]string, name, home string) string {
	dir, ok := projects[name]
	if !ok || dir == "" || hasControlBytes(dir) {
		return home
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return home
	}
	return dir
}

// sessionEnv is the environment a session's shell is born with.
//
// Shared by both creation paths on purpose. api/session-lifecycle.md section 6 requires an
// API-created session to be indistinguishable from one made by deep link, and until now that
// parity was two identical lines in two files with a comment on each asking them to stay in step.
// dtachArgs already made the argv one implementation; this makes the environment one too.
//
// The dtach master captures this for the whole life of the session, so anything missing here
// cannot be repaired by attaching later -- which is why TERM is set explicitly rather than
// inherited (wtd runs as a systemd unit with no usable TERM, and a session born without it is
// colorless until deleted) and why WT=1 is here (a login shell reads it to tell it is already
// inside a web session and skip auto-launching tmux, per docs/bashrc-snippet.sh; without it a
// user with that snippet gets a recursive multiplexer inside every session).
//
// WT_SESSION names the session, which nothing else in the environment does (#111). It is what
// lets a program running inside a session report on *itself* -- an agent hook has no other way to
// discover which session it is in, since it is spawned with no controlling terminal and the pty
// carries no name. An empty name omits the variable rather than setting it empty, so "unset means
// this is not a session" is a usable test.
//
// Two things a consumer has to know about the value:
//
//   - **It is safe to use as a filename, and only because both callers validate first.**
//     attachCommand runs validateAttachName and createSession runs validateSessionName before
//     reaching here, so no name containing "/" or ".." can arrive. A reader that turns it back
//     into a path should still re-apply that check rather than trust the variable, the way
//     ringstore does -- an environment variable is not a chain of custody.
//   - **It may contain spaces and non-ASCII.** The attach side is deliberately more permissive
//     than the create side (see the listing rule in .claude/rules/go-server.md), so a session
//     made from a deep link can be named things POST would refuse. Shell consumers must quote it.
func sessionEnv(name string) []string {
	env := append(os.Environ(), "WT=1", "TERM="+defaultTerm)
	if name != "" {
		env = append(env, "WT_SESSION="+name)
	}
	return env
}

// attachCommand builds the command that attaches a connection to a named session, creating it if
// it does not exist yet.
//
// MkdirAll is not incidental: the session directory used to be created by the picker on first run,
// and without it the very first deep link on a fresh install fails inside dtach instead of working.
// 0700 rather than a umask-dependent mode, matching createSession — these are sockets onto
// interactive shells.
//
// The environment the session is born with is sessionEnv's, shared with the API creation path.
func attachCommand(dir, name, workdir string) (*exec.Cmd, error) {
	if err := validateAttachName(dir, name); err != nil {
		return nil, fmt.Errorf("session %q cannot be attached: %w", name, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	socket := filepath.Join(dir, name+socketSuffix)

	cmd := exec.Command("dtach", dtachArgs("-A", socket, workdir)...)
	cmd.Env = sessionEnv(name)
	return cmd, nil
}

// fallbackShell is the command an argless connection gets, and the one an unusable session name
// degrades to.
//
// A plain interactive bash, which is what the picker's `c) cancel to shell` branch always did. It is
// not attached to any dtach session, so it dies with the connection — which is exactly what
// api/ws-protocol.md section 9 specifies for an argless connection: a private pty, no sharing, no
// replay.
//
// Dir is $HOME rather than inherited. wtd runs as a systemd unit whose working directory is /, and
// dropping a user into / is a worse first impression than their home directory for no gain. WT=1 for
// the same reason every other spawn here sets it: a login shell reads it to avoid launching a
// multiplexer inside a session that is already one.
func fallbackShell(home string) *exec.Cmd {
	cmd := exec.Command("bash")
	cmd.Dir = home
	// No WT_SESSION: there is no session behind this shell, and an unusable name degrades to
	// here too. Reporting the name the client asked for would name something that does not
	// exist, which is worse for a consumer than reporting nothing.
	cmd.Env = sessionEnv("")
	return cmd
}

// terminalCmd is what a connection runs, plus the two things the caller needs to describe it.
type terminalCmd struct {
	cmd *exec.Cmd
	// label titles the window for a connection that has no session name of its own, and names the
	// subject in a spawn-failure message.
	label string
	// notice, when set, is one line shown to the user before any output. It explains why they got
	// a plain shell instead of the session they asked for.
	//
	// This exists because api/openapi.yaml publishes that no value of `arg` closes the connection.
	// An unusable name must still produce a working terminal, and silently handing someone a shell
	// where they expected their session is the kind of thing that gets diagnosed as a server bug.
	// Closing instead would be worse than useless: the published retry advice for 1011 is backoff,
	// so a client with a typo'd saved profile would loop rather than show its user anything.
	notice string
}

// terminalCommand builds the command that serves one connection.
//
// Two shapes, selected by whether an external start command is configured:
//
//   - startCommand set: run it with the connection's argv, exactly as ttyd ran its `-a` program.
//     This is the ttyd-compatible path the conformance job exercises, and the rollback if the
//     built-in path ever misbehaves on a real box.
//   - startCommand empty: wtd builds its own. A named connection attaches to that session with
//     `dtach -A`; an argless one gets a plain shell.
//
// An unusable name is not an error. It returns a working fallback shell and a notice, because the
// wire contract says so — see terminalCmd.notice.
func (s *server) terminalCommand(args []string) (terminalCmd, error) {
	named := len(args) > 0 && args[0] != ""

	if s.startCommand != "" {
		cmd := exec.Command(s.startCommand, args...)
		cmd.Env = append(os.Environ(), "TERM="+defaultTerm)
		return terminalCmd{cmd: cmd, label: s.startCommand}, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		// Not fatal: / is a poor working directory but a working one, and refusing to open a
		// terminal because $HOME is unreadable would be a worse answer than opening it here.
		home = "/"
		logf("wtd: cannot determine home directory (%v); terminals will start in /", err)
	}

	if !named {
		return terminalCmd{cmd: fallbackShell(home), label: "bash"}, nil
	}

	name := args[0]
	dir := s.sessionDir()
	cmd, err := attachCommand(dir, name, attachWorkdir(loadProjects(s.projectsFile()), name, home))
	if err != nil {
		// The name cannot become a reachable socket. Degrade to a shell and say why, keeping the
		// connection named so it still shares and replays like any other named connection.
		return terminalCmd{
			cmd:    fallbackShell(home),
			label:  name,
			notice: "wtd: " + oneLine(err.Error()) + "; opening a plain shell instead",
		}, nil
	}
	return terminalCmd{cmd: cmd, label: name}, nil
}
