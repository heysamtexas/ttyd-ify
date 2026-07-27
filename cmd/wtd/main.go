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
	startCommand := flag.String("start-command", "/usr/local/bin/wt", "command run for each terminal connection")
	allowCrossOrigin := flag.Bool("allow-cross-origin", false,
		"accept WebSocket upgrades from any Origin (escape hatch; lets any web page the user visits open a shell)")
	replayBytes := flag.Int("replay-bytes", defaultReplayBytes,
		"bytes of recent output replayed to a client on attach, per session (0 disables replay)")
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

	// exec.LookPath, not os.Stat: Stat passes for a directory or a mode-0644 file, so it
	// would not deliver the "fail at startup, not on first connection" promise below.
	// install.sh skips existing binaries unless FORCE=1, which makes a stale or
	// wrongly-moded wt entirely plausible on a real box.
	if _, err := exec.LookPath(*startCommand); err != nil {
		// Fail loudly at startup rather than on the first connection, where the only
		// symptom would be a terminal that opens and immediately closes.
		fmt.Fprintf(os.Stderr, "wtd: -start-command %q is not executable: %v\n", *startCommand, err)
		os.Exit(1)
	}

	app := newServer(*startCommand)
	app.allowCrossOrigin = *allowCrossOrigin
	// Rebuilt rather than mutated: newServer installs a default-configured hubs so every
	// other entry point (tests included) has a working one, and this is the only place that
	// knows the operator's setting. defaultMaxWarmHubs is deliberately not a flag or a config
	// key — it is a backstop, not a tuning knob, and an unreachable knob is worse than a
	// constant with a comment.
	app.hubs = newHubs(*startCommand, *replayBytes, defaultMaxWarmHubs)
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
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Logged only after the socket is up, and reporting the listener's address rather
	// than the requested one. CLAUDE.md teaches greping the journal for this line as
	// proof the service came up; printing it before bind made that a false positive on
	// every failed start, which under Restart= is every 3 seconds.
	log.Printf("wtd %s: listening on %s (start command: %s)", version, ln.Addr(), *startCommand)

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
		// Held attachments are released explicitly rather than incidentally, so each hub's
		// clients get a close status and each socket's execute bit clears deterministically
		// instead of racing a pty-close SIGHUP against process exit.
		app.hubs.closeAll()
	}
}
