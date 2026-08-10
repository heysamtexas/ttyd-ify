// Command wtd serves the ttyd-ify web terminal: a ttyd-compatible WebSocket terminal,
// a JSON session API, and a browser session picker, on one port.
//
// It replaces ttyd, not dtach. Session persistence stays with dtach, because a dtach
// session's parent process is independent of this server — restarting wtd drops clients
// but leaves sessions running, which is the property the whole system depends on.
// All session logic stays in bin/wt, which wtd runs as its start command exactly as
// ttyd did, so there is only one implementation of it.
//
// Wire compatibility with ttyd 1.7.4 is deliberate and load-bearing: the iOS client in
// ~/src/ios-claude-terminal speaks ttyd's protocol directly, so matching it means that
// client works against wtd unchanged, and cutover and rollback are both free. See
// api/ws-protocol.md before changing anything on the wire.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// version is the advertised server version, reported by /api/v1/meta so a client can
// tell what it is talking to. Overridden at build time with
// -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	listen := flag.String("listen", "", "address to bind, host:port (required; a wildcard is refused)")
	startCommand := flag.String("start-command", "",
		"external program run for each terminal connection, ttyd-style; empty means wtd attaches to "+
			"dtach sessions itself")
	allowCrossOrigin := flag.Bool("allow-cross-origin", false,
		"accept WebSocket upgrades from any Origin (escape hatch; lets any web page the user visits open a shell)")
	replayBytes := flag.Int("replay-bytes", defaultReplayBytes,
		"bytes of recent output replayed to a client on attach, per session (0 disables replay)")
	stateDir := flag.String("state-dir", "",
		"directory where each session's replay buffer is saved across restarts (empty disables; "+
			"wt-serve passes systemd's $RUNTIME_DIRECTORY)")
	// Flags rather than environment variables, deliberately: WT_DIR set in /etc/ttyd-ify/config
	// reached nothing for as long as the key existed, because sourcing a config makes shell
	// variables and every consumer read the environment (#28). A value that travels as an argument
	// either arrives or is visibly absent in the process's own command line.
	sessionDir := flag.String("session-dir", "",
		"directory holding session sockets (default $WT_DIR, else ~/.dtach)")
	projectsFile := flag.String("projects-file", "",
		"project shortcut file (default $WT_PROJECTS, else ~/.config/wt/projects)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	addr, err := validateListen(*listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wtd: %v\n", err)
		os.Exit(1)
	}

	// Whichever program actually gets run, prove it exists now rather than on the first
	// connection, where the only symptom would be a terminal that opens and immediately closes.
	//
	// exec.LookPath, not os.Stat: Stat passes for a directory or a mode-0644 file, so it would not
	// deliver that promise. A partial install, a hand-copied file or a -start-command pointing
	// somewhere that was never installed all make a non-executable program plausible on a real box.
	//
	// With no start command the dependency is dtach, and checking it here is strictly better than
	// what came before: dtach used to be invoked from inside the picker, so a box without it failed
	// once per connection with the error buried in a shell.
	if *startCommand != "" {
		if _, err := exec.LookPath(*startCommand); err != nil {
			fmt.Fprintf(os.Stderr, "wtd: -start-command %q is not executable: %v\n", *startCommand, err)
			os.Exit(1)
		}
	} else if _, err := exec.LookPath("dtach"); err != nil {
		fmt.Fprintf(os.Stderr, "wtd: dtach is not on PATH: %v\n"+
			"wtd attaches to dtach sessions itself unless -start-command names another program.\n", err)
		os.Exit(1)
	}

	app := newServer(*startCommand)
	app.allowCrossOrigin = *allowCrossOrigin
	// Set before anything reads them: hubs builds commands through app, and warnSessionDirDepth
	// below must warn about the directory actually in use rather than the default.
	app.sessionDirFlag = *sessionDir
	app.projectsFileFlag = *projectsFile
	// Rebuilt rather than mutated: newServer installs a default-configured hubs so every
	// other entry point (tests included) has a working one, and this is the only place that
	// knows the operator's setting. defaultMaxWarmHubs is deliberately not a flag or a config
	// key — it is a backstop, not a tuning knob, and an unreachable knob is worse than a
	// constant with a comment.
	// The store only makes sense on the built-in dtach path with replay on: it restores a
	// ring only while the session's socket proves the session outlived the restart, and with
	// an external -start-command wtd has no socket to ask. Said out loud rather than silently
	// ignored, because an operator who set -state-dir configured a behavior.
	var store *ringStore
	switch {
	case *stateDir == "":
	case *replayBytes <= 0:
		log.Print("wtd: -state-dir is set but replay is disabled; nothing will be saved across restarts")
	case *startCommand != "":
		log.Print("wtd: -state-dir is ignored with -start-command; wtd cannot check session " +
			"liveness for sessions it does not manage")
	default:
		// One stat now beats total silence later: every load failure is an indistinguishable
		// "nothing saved", so a typo'd path — or systemd handing over a colon-separated list
		// because someone added a second RuntimeDirectory= — would otherwise surface only as
		// one error per session at shutdown, after the replay is already unrecoverable.
		if fi, err := os.Stat(*stateDir); err != nil {
			log.Printf("wtd: -state-dir %v; replay will not survive restarts", err)
		} else if !fi.IsDir() {
			log.Printf("wtd: -state-dir %q is not a directory; replay will not survive restarts", *stateDir)
		} else {
			store = &ringStore{dir: *stateDir, sessionDir: app.sessionDir()}
			store.sweep()
		}
	}
	app.hubs = newHubs(app.terminalCommand, *replayBytes, defaultMaxWarmHubs, store)
	if *replayBytes <= 0 {
		log.Print("wtd: replay is disabled (-replay-bytes 0); attaching to a session shows " +
			"a blank screen until it writes")
	}
	if *allowCrossOrigin {
		log.Print("wtd: WARNING -allow-cross-origin is set; any web page the user visits " +
			"can open a WebSocket to this port and get a shell")
	}
	warnSessionDirDepth(app.sessionDir())

	// Asked once, here, because the answer cannot change while we run and because an operator can
	// still act on it at this point — by the time the shutdown line prints, the sessions are
	// already dying. See survival.go for why no static guard can cover this.
	sessionFate, unit, killMode := checkSurvival(readSelfCgroup, systemctlKillMode)
	if w := survivalWarning(sessionFate, unit, killMode); w != "" {
		log.Print(w)
	}

	// Bind first, then assert on the socket we actually got.
	//
	// validateListen checks a *string*; for a hostname the kernel resolves it again at
	// bind time and could get a different answer, so the only address worth trusting is
	// the listener's own. This closes that window and makes the wildcard refusal a
	// property of the running socket rather than of a parsed argument.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wtd: listen %s: %v\n", addr, err)
		os.Exit(1)
	}
	if ap, perr := netip.ParseAddrPort(ln.Addr().String()); perr == nil && ap.Addr().IsUnspecified() {
		_ = ln.Close()
		fmt.Fprintf(os.Stderr, "wtd: %v\n",
			wildcardError(addr, fmt.Sprintf("it bound %s, which is every interface", ln.Addr())))
		os.Exit(1)
	}

	srv := &http.Server{
		Handler: app.routes(),
		// No read/write timeouts: a terminal connection is long-lived by definition,
		// and the iOS client pings every 20s, so any idle timeout must stay well above
		// that. Header timeout still guards against a stuck client mid-handshake.
		//
		// ReadTimeout in particular must stay unset, and not only for connection length:
		// it would cancel the request context under readHandshake, whose deadline has to
		// expire *before* the connection is torn down or the published 1008 close never
		// reaches the client. Hardening this struct means reading readHandshake first.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Logged only after the socket is up, and reporting the listener's address rather
	// than the requested one. CLAUDE.md teaches greping the journal for this line as
	// proof the service came up; printing it before bind made that a false positive on
	// every failed start, which under Restart= is every 3 seconds.
	terminals := "dtach (built in)"
	if *startCommand != "" {
		terminals = "start command: " + *startCommand
	}
	log.Printf("wtd %s: listening on %s (%s)", version, ln.Addr(), terminals)

	// Shut down on the signals systemd actually sends, so a restart closes sockets
	// cleanly instead of leaving clients to notice via ping timeout.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		errc <- srv.Serve(ln)
	}()

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "wtd: %v\n", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		// Shutdown stops accepting and waits for idle connections — but NOT for terminals.
		// net/http untracks a hijacked connection (server.go: StateHijacked → trackConn
		// false), so every WebSocket is invisible to it and the timeout below applies to
		// nothing. Kept because it still closes the listener cleanly and drains any
		// in-flight API request.
		//
		// Argless terminals are torn down by process exit: closing the pty masters hangs up
		// each child, which detaches the dtach client and leaves the session running. That
		// is the behavior we want on restart, so this is a documented gap rather than a
		// bug — see api/ws-protocol.md, which specifies a close-1001 per connection if we
		// ever want a polite goodbye instead of a dropped socket. Named connections do get
		// that goodbye, because closeAll below closes each hub's clients with a status.
		log.Print(shutdownNotice(sessionFate, unit, killMode))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("wtd: shutdown: %v", err)
		}
		// Saved before closeAll, which is the last moment the rings exist. This is the write
		// half of the ringStore contract: SIGTERM is what systemd sends on restart, so a
		// clean restart is exactly the case that gets its replay back. A crash saves nothing,
		// which costs what every restart used to cost.
		app.hubs.saveAll()
		// Held attachments are released explicitly rather than incidentally, so each hub's
		// clients get a close status and each socket's execute bit clears deterministically
		// instead of racing a pty-close SIGHUP against process exit.
		app.hubs.closeAll()
	}
}
