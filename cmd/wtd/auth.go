package main

// The server password (#146): one optional secret in front of every route, terminals included.
//
// Off unless WT_PASSWORD is set, and then fail-closed: a set-but-unusable password refuses to
// start rather than serve an open shell to an operator who asked for a closed one. It is
// checked here, once, before the listener exists; nothing downstream knows auth exists.
//
// Two ways in, because two kinds of client exist and neither can use the other's:
//   - A browser cannot attach a header to a WebSocket upgrade, so it logs in once at /login and
//     carries an HMAC-signed cookie. Cookies DO ride the upgrade, which is why this shape works
//     on Safari and iOS where basic auth does not.
//   - The native app and curl send `Authorization: Bearer <password>` (or Basic, any user) on
//     every request, the upgrade included, and are never redirected.
//
// The wire contract is shared with heysamtexas/ios-claude-terminal#15 — in particular the
// WWW-Authenticate header is how that client tells this wall from a reverse proxy's on a
// rejected upgrade, where it sees headers but no body. Change it in both repos or neither.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// passwordEnv is how the launcher hands the password over: the environment, not a flag,
	// because argv is world-readable in /proc and the journal records it. loadPassword removes
	// it again before any terminal is spawned, so no shell inherits it.
	passwordEnv = "WT_PASSWORD"
	// minPasswordLen is the floor below which the server refuses to start. The failure delay
	// below is the only brute-force defence, so the password's length is doing real work.
	minPasswordLen = 12

	sessionCookieName = "wtd_session"
	sessionTTL        = 30 * 24 * time.Hour
	// sessionKeyFile lives in -state-dir (/run/wt: tmpfs, 0700, kept across restarts), so a
	// restart or upgrade does not log every browser out, and a reboot does.
	sessionKeyFile = "session.key"

	// failDelay is paid by every wrong password. failSlots bounds how many password checks one
	// client address may have in flight, so a guesser gets at most failSlots guesses a second
	// however many connections it opens. A check past the cap gets 429 without being compared,
	// so the 429 says nothing about the guess. Per address, so a guesser elsewhere on the network
	// cannot lock the owner's own app out.
	failDelay = time.Second
	failSlots = 4

	codeAuthRequired = "auth_required"
	codeBadPassword  = "bad_password"
	codeRateLimited  = "rate_limited"
)

// authChallenge is sent on every password 401. Bearer and never Basic, so a browser fetch whose
// cookie has expired gets a 401 rather than a native credentials dialog.
const authChallenge = `Bearer realm="wtd"`

//go:embed web/login.html
var loginHTML string

var loginTmpl = template.Must(template.New("login").Parse(loginHTML))

type gate struct {
	// sum is sha256 of the password. Compared hash-to-hash so the comparison is constant-time
	// whatever length the guess is.
	sum [32]byte
	// key signs session cookies. HMAC(random key, password): the random half means a stolen
	// cookie is not an offline oracle for the password, and the password half means changing it
	// invalidates every outstanding session.
	key []byte

	mu       sync.Mutex
	inFlight map[string]int // password checks in flight, by client address
	// sleep is time.Sleep outside tests, which would otherwise pay failDelay per refusal.
	sleep func(time.Duration)
	now   func() time.Time
}

// loadPassword reads WT_PASSWORD and removes it from the environment. ok=false means unset, which
// is the default and means no gate; set-but-empty is a value, and validatePassword refuses it.
func loadPassword() (password string, ok bool) {
	password, ok = os.LookupEnv(passwordEnv)
	_ = os.Unsetenv(passwordEnv)
	return password, ok
}

// validatePassword is the start-time refusal. Printable ASCII only, and no surrounding spaces,
// because HTTP strips whitespace around header values: a password that cannot survive an
// Authorization header would lock the native client out while the browser worked.
func validatePassword(p string) error {
	if len(p) < minPasswordLen {
		return fmt.Errorf("%s must be at least %d characters (it is %d)", passwordEnv, minPasswordLen, len(p))
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] > 0x7e {
			return fmt.Errorf("%s must be printable ASCII (byte %d is not)", passwordEnv, i)
		}
	}
	if p[0] == ' ' || p[len(p)-1] == ' ' {
		return fmt.Errorf("%s must not start or end with a space", passwordEnv)
	}
	return nil
}

// newGate builds the gate for password, which must already have passed validatePassword.
// stateDir may be empty, in which case the signing key lives only as long as the process.
func newGate(password, stateDir string) (*gate, error) {
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	secret, err := sessionSecret(stateDir)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("wtd-session-v1|" + password))
	return &gate{
		sum:      sha256.Sum256([]byte(password)),
		key:      mac.Sum(nil),
		inFlight: map[string]int{},
		sleep:    time.Sleep,
		now:      time.Now,
	}, nil
}

// sessionSecret returns 32 random bytes, reused from stateDir when a previous run left them there.
// A missing or malformed file is replaced; only a directory that cannot be written is an error,
// and even then only a soft one — the key just will not outlive this process.
func sessionSecret(stateDir string) ([]byte, error) {
	var path string
	if stateDir != "" {
		path = filepath.Join(stateDir, sessionKeyFile)
		if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if path != "" {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			logf("wtd: cannot save the session key (%v); browsers will log in again after a restart", err)
		}
	}
	return b, nil
}

func (g *gate) check(password string) bool {
	sum := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(sum[:], g.sum[:]) == 1
}

func (g *gate) sign(msg string) string {
	mac := hmac.New(sha256.New, g.key)
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (g *gate) mintSession() string {
	exp := strconv.FormatInt(g.now().Add(sessionTTL).Unix(), 10)
	return exp + "." + g.sign(exp)
}

func (g *gate) validSession(v string) bool {
	exp, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	ts, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || g.now().Unix() > ts {
		return false
	}
	return hmac.Equal([]byte(g.sign(exp)), []byte(sig))
}

// presented returns the password carried in an Authorization header, and whether there was one.
// A header in a scheme this server does not take counts as none.
func presented(r *http.Request) (string, bool) {
	if _, pw, ok := r.BasicAuth(); ok {
		return pw, true
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if ok && strings.EqualFold(scheme, "Bearer") {
		return token, true
	}
	return "", false
}

// errRateLimited is what attempt returns when the client has failSlots checks in flight.
var errRateLimited = errors.New("too many password attempts in flight")

// attempt checks one presented password, paying failDelay if it is wrong.
//
// The slot is taken BEFORE the comparison, and a client with none free is refused without one.
// Comparing first would make the cap an oracle: with every slot held, a right guess would pass
// and a wrong one would get an instant 429, so a guesser could test passwords at full speed.
// A right password holds its slot for one hash, so a legitimate client never notices the cap.
//
// Every refusal here happens before the wrapped handler runs, and the iOS client depends on that:
// it retries a 429 once, POST and DELETE included, which is only safe because a 429 never acted.
// Do not move throttling into a handler.
func (g *gate) attempt(r *http.Request, password string) (bool, error) {
	addr := clientAddr(r)
	g.mu.Lock()
	if g.inFlight[addr] >= failSlots {
		g.mu.Unlock()
		return false, errRateLimited
	}
	g.inFlight[addr]++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if g.inFlight[addr]--; g.inFlight[addr] <= 0 {
			delete(g.inFlight, addr)
		}
		g.mu.Unlock()
	}()
	if g.check(password) {
		return true, nil
	}
	g.sleep(failDelay)
	return false, nil
}

// clientAddr is the peer's IP without the port, so a new connection is not a new budget. The
// socket's own address, never a forwarded header, which a client could set to anything; behind
// a reverse proxy every client would share one budget, and this repo documents no proxy.
func clientAddr(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// wrap puts the gate in front of next. /healthz stays open, so a liveness probe needs no secret
// and says nothing but "ok"; /login is the one page an anonymous browser can load.
func (g *gate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			next.ServeHTTP(w, r)
			return
		case loginPath:
			g.login(w, r)
			return
		}
		if c, err := r.Cookie(sessionCookieName); err == nil && g.validSession(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if pw, ok := presented(r); ok {
			passed, err := g.attempt(r, pw)
			if err != nil {
				writeRateLimited(w)
				return
			}
			if passed {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", authChallenge+`, error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, codeBadPassword, "wrong password", "")
			return
		}
		// A browser navigating to a page goes to the login form and comes back. Everything else
		// — fetches, the /ws upgrade, the native app, curl — gets a 401 it can act on, never a
		// redirect it would have to recognise as a login wall.
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, loginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", authChallenge)
		writeError(w, http.StatusUnauthorized, codeAuthRequired, "password required",
			"send Authorization: Bearer <password>, or log in at "+loginPath)
	})
}

func writeRateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusTooManyRequests, codeRateLimited, "too many failed password attempts; retry shortly", "")
}

const loginPath = "/login"

func (g *gate) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	next := safeNext(r.FormValue("next"))
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		loginPage(w, next, "", http.StatusOK)
	case http.MethodPost:
		passed, err := g.attempt(r, r.FormValue("password"))
		if err != nil {
			loginPage(w, next, "Too many attempts. Wait a moment and try again.", http.StatusTooManyRequests)
			return
		}
		if !passed {
			loginPage(w, next, "Wrong password.", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    g.mintSession(),
			Path:     "/",
			HttpOnly: true,
			// Lax, not Strict, so a link from Slack or a mail opens the page signed in instead
			// of bouncing through /login. Lax still withholds the cookie from cross-site
			// subresource requests and POSTs, and the two routes that act — the /ws upgrade and
			// the mutating API — keep their own Origin checks (handleWS, guardMutating), which
			// refuse a cross-site request whether or not it carries the cookie.
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL.Seconds()),
			// No Secure flag: this serves plain HTTP over an encrypted tailnet, and a Secure
			// cookie would never be sent back.
		})
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "")
	}
}

func loginPage(w http.ResponseWriter, next, errMsg string, code int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = loginTmpl.Execute(w, map[string]string{"Next": next, "Error": errMsg, "Action": loginPath})
}

// safeNext keeps the post-login redirect on this server: a rooted path with no scheme and no
// host. Browsers strip tabs and newlines from URLs and read a backslash as a slash, so "/\t/x",
// "/\x" and "//x" all name the host x. Rather than enumerate those, any control byte, space or
// backslash is refused outright, and what is left must parse as a bare path.
func safeNext(v string) string {
	if v == "" || v[0] != '/' || (len(v) > 1 && v[1] == '/') {
		return "/"
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] == 0x7f || v[i] == '\\' {
			return "/"
		}
	}
	if u, err := url.Parse(v); err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return v
}
