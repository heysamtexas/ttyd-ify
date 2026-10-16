package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testPassword = "correct horse battery"

// testGate is a gate whose failure delay is recorded rather than slept.
func testGate(t *testing.T) *gate {
	t.Helper()
	g, err := newGate(testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	g.sleep = func(time.Duration) {}
	return g
}

func bearer(pw string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+pw) }
}

func gatedServer(t *testing.T) (*server, *gate, http.Handler) {
	t.Helper()
	srv, _ := newTestServer(t)
	srv.gate = testGate(t)
	return srv, srv.gate, srv.routes()
}

func serve(h http.Handler, method, target string, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestValidatePassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"":                          false,
		"short":                     false,
		"elevenchars":               false,
		"twelve chars":              true,
		"correct horse battery":     true,
		" leading space ok?":        false,
		"trailing space ok? ":       false,
		"tab\tinside the password":  false,
		"newline\ninside it at all": false,
		"non-ascii pässwörd here":   false,
		"~!@#$%^&*()_+{}|:<>?`-=[]": true,
	} {
		if err := validatePassword(pw); (err == nil) != ok {
			t.Errorf("validatePassword(%q) = %v, want ok=%v", pw, err, ok)
		}
	}
}

// The password reaches wtd through its environment, and every terminal wtd spawns inherits that
// environment, so it has to be gone before anything is spawned.
func TestLoadPasswordRemovesItFromTheEnvironment(t *testing.T) {
	t.Setenv(passwordEnv, testPassword)
	pw, ok := loadPassword()
	if !ok || pw != testPassword {
		t.Fatalf("loadPassword() = %q, %v", pw, ok)
	}
	if _, still := os.LookupEnv(passwordEnv); still {
		t.Errorf("%s is still in the environment a terminal would inherit", passwordEnv)
	}

	t.Setenv(passwordEnv, "")
	if _, ok := loadPassword(); !ok {
		t.Error("set-but-empty read as unset; it must reach validatePassword and be refused")
	}
	os.Unsetenv(passwordEnv)
	if _, ok := loadPassword(); ok {
		t.Error("unset read as set")
	}
}

// No password configured is today's behaviour: every route as open as before.
func TestNoPasswordMeansNoGate(t *testing.T) {
	srv, _ := newTestServer(t)
	if srv.gate != nil {
		t.Fatal("a default server has a gate")
	}
	h := srv.routes()
	for _, path := range []string{"/api/v1/meta", "/token", "/"} {
		if rec := serve(h, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s without a password configured = %d, want 200", path, rec.Code)
		}
	}
	if rec := serve(h, http.MethodGet, "/login"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("GET /login with no password = %d to %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
}

// The wire contract with the iOS client (ios-claude-terminal#15). Status, code and challenge are
// all part of it: on a rejected upgrade the client sees headers but no body, so the
// WWW-Authenticate header is the only way it can tell this wall from a proxy's, and its error=
// parameter the only way it can tell "password required" from "wrong password".
func TestGateRefusals(t *testing.T) {
	_, g, h := gatedServer(t)
	var slept int
	g.sleep = func(time.Duration) { slept++ }

	for _, path := range []string{"/api/v1/meta", "/api/v1/sessions", "/token", "/openapi.json", "/", "/ws",
		"/vendor/xterm.js", "/docs/ws-protocol.md"} {
		rec := serve(h, http.MethodGet, path)
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec.Body.Bytes()) != codeAuthRequired {
			t.Errorf("GET %s without credentials = %d %s, want 401 %s", path, rec.Code, rec.Body, codeAuthRequired)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="wtd"` {
			t.Errorf("GET %s: WWW-Authenticate = %q", path, got)
		}
	}
	if slept != 0 {
		t.Errorf("a request with no credential paid the failure delay %d times; it guessed nothing", slept)
	}

	rec := serve(h, http.MethodGet, "/api/v1/meta", bearer("wrong password!"))
	if rec.Code != http.StatusUnauthorized || errorCode(t, rec.Body.Bytes()) != codeBadPassword {
		t.Errorf("wrong Bearer = %d %s, want 401 %s", rec.Code, rec.Body, codeBadPassword)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="wtd", error="invalid_token"` {
		t.Errorf("wrong Bearer: WWW-Authenticate = %q", got)
	}
	if slept != 1 {
		t.Errorf("a wrong password paid the failure delay %d times, want 1", slept)
	}

	// /healthz stays open: a liveness probe should not need the secret.
	if rec := serve(h, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

func TestGateAcceptsBearerAndBasic(t *testing.T) {
	_, _, h := gatedServer(t)
	for name, mod := range map[string]func(*http.Request){
		"Bearer":           bearer(testPassword),
		"lowercase bearer": func(r *http.Request) { r.Header.Set("Authorization", "bearer "+testPassword) },
		"Basic, any user":  func(r *http.Request) { r.SetBasicAuth("whoever", testPassword) },
		"Basic, no user":   func(r *http.Request) { r.SetBasicAuth("", testPassword) },
	} {
		if rec := serve(h, http.MethodGet, "/api/v1/meta", mod); rec.Code != http.StatusOK {
			t.Errorf("%s: GET /api/v1/meta = %d, want 200", name, rec.Code)
		}
	}
	if rec := serve(h, http.MethodGet, "/token", bearer(testPassword)); rec.Body.String() != `{"token": ""}` {
		t.Errorf("/token behind the gate = %q; its body is ttyd's and must not change", rec.Body.String())
	}
}

// A browser navigating to a page is sent to the login form and back. Nothing else is ever
// redirected: the native client does not follow redirects and would read one as a proxy wall.
func TestBrowserNavigationGoesToLogin(t *testing.T) {
	_, _, h := gatedServer(t)
	html := func(r *http.Request) { r.Header.Set("Accept", "text/html,application/xhtml+xml") }
	rec := serve(h, http.MethodGet, "/?arg=demo%20x", html)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("browser GET / = %d, want 303", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != "/login" || loc.Query().Get("next") != "/?arg=demo%20x" {
		t.Errorf("redirect = %q, want /login?next=<the original request>", rec.Header().Get("Location"))
	}
	if rec := serve(h, http.MethodPost, "/api/v1/sessions", html); rec.Code != http.StatusUnauthorized {
		t.Errorf("a POST was redirected (%d); only GET navigations go to the login page", rec.Code)
	}
}

func TestLoginRoundTrip(t *testing.T) {
	_, g, h := gatedServer(t)

	page := serve(h, http.MethodGet, "/login?next=/help")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="/login"`) ||
		!strings.Contains(page.Body.String(), `value="/help"`) {
		t.Fatalf("login page = %d:\n%s", page.Code, page.Body.String())
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("login page CSP = %q; it must not be frameable", csp)
	}

	cookie := login(t, h, testPassword, "/help", http.StatusSeeOther)
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Errorf("cookie = %+v, want HttpOnly, SameSite=Strict, Path=/", cookie)
	}
	withCookie := func(c *http.Cookie) func(*http.Request) { return func(r *http.Request) { r.AddCookie(c) } }
	for _, path := range []string{"/api/v1/meta", "/token", "/help"} {
		if rec := serve(h, http.MethodGet, path, withCookie(cookie)); rec.Code != http.StatusOK {
			t.Errorf("GET %s with the session cookie = %d, want 200", path, rec.Code)
		}
	}

	forged := *cookie
	exp, _, _ := strings.Cut(cookie.Value, ".")
	forged.Value = exp + ".not-the-signature"
	if rec := serve(h, http.MethodGet, "/api/v1/meta", withCookie(&forged)); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged cookie = %d, want 401", rec.Code)
	}

	g.now = func() time.Time { return time.Now().Add(sessionTTL + time.Hour) }
	if rec := serve(h, http.MethodGet, "/api/v1/meta", withCookie(cookie)); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired cookie = %d, want 401", rec.Code)
	}
}

func TestWrongLoginIsDelayedAndSetsNoCookie(t *testing.T) {
	_, g, h := gatedServer(t)
	var slept time.Duration
	g.sleep = func(d time.Duration) { slept += d }
	if c := login(t, h, "not the password", "/", http.StatusUnauthorized); c != nil {
		t.Error("a wrong password set a cookie")
	}
	if slept != failDelay {
		t.Errorf("wrong login slept %v, want %v", slept, failDelay)
	}
}

// Failures beyond failSlots in flight are refused at once with 429, so a guesser cannot buy
// parallelism by opening connections.
func TestConcurrentFailuresAreCapped(t *testing.T) {
	_, g, h := gatedServer(t)
	for i := 0; i < failSlots; i++ {
		g.slots <- struct{}{} // every slot taken, as if failSlots guesses were mid-delay
	}
	rec := serve(h, http.MethodGet, "/api/v1/meta", bearer("wrong password!"))
	if rec.Code != http.StatusTooManyRequests || errorCode(t, rec.Body.Bytes()) != codeRateLimited {
		t.Errorf("guess with every slot taken = %d %s, want 429 %s", rec.Code, rec.Body, codeRateLimited)
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("429 headers = %v; want Retry-After and no challenge", rec.Header())
	}
	// The right password never waits for a slot.
	if rec := serve(h, http.MethodGet, "/api/v1/meta", bearer(testPassword)); rec.Code != http.StatusOK {
		t.Errorf("right password with every slot taken = %d, want 200", rec.Code)
	}
	if c := login(t, h, "not the password", "/", http.StatusTooManyRequests); c != nil {
		t.Error("a refused login set a cookie")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/?arg=x":                "/?arg=x",
		"/files/a.md?raw=1":      "/files/a.md?raw=1",
		"//evil.example/":        "/",
		"https://evil.example/":  "/",
		`/\evil.example`:         "/",
		"/ok\r\nSet-Cookie: x=y": "/",
		"relative":               "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// The session key outlives a restart through -state-dir, and changing the password invalidates
// every cookie minted under the old one.
func TestSessionKeyPersistsAndPasswordChangeLogsOut(t *testing.T) {
	dir := t.TempDir()
	a, err := newGate(testPassword, dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, sessionKeyFile))
	if err != nil {
		t.Fatalf("no session key saved: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("session key mode = %04o, want 0600", info.Mode().Perm())
	}
	cookie := a.mintSession()

	b, err := newGate(testPassword, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !b.validSession(cookie) {
		t.Error("a restart with the same state dir and password logged the browser out")
	}
	c, err := newGate(testPassword+" changed", dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.validSession(cookie) {
		t.Error("a cookie survived a password change")
	}
}

// The upgrade is where basic-auth designs fail in a browser, so it is tested over a real
// socket: refused with a 401 and the challenge before upgrading, accepted with a Bearer header
// (the native client) and with the session cookie (a browser).
func TestWebSocketUpgradeIsGated(t *testing.T) {
	app := newServer(noStartCommand)
	app.gate = testGate(t)
	t.Cleanup(app.hubs.closeAll)
	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dial := func(h http.Header) (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"tty"}, HTTPHeader: h})
	}

	_, resp, err := dial(nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("upgrade without credentials: err=%v resp=%v, want a 401", err, resp)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="wtd"` {
		t.Errorf("rejected upgrade WWW-Authenticate = %q", got)
	}

	_, resp, err = dial(http.Header{"Authorization": {"Bearer wrong password!"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("upgrade with a wrong password: err=%v resp=%v", err, resp)
	}

	conn, _, err := dial(http.Header{"Authorization": {"Bearer " + testPassword}})
	if err != nil {
		t.Fatalf("upgrade with the Bearer password: %v", err)
	}
	conn.CloseNow()

	cookie := &http.Cookie{Name: sessionCookieName, Value: app.gate.mintSession()}
	conn, _, err = dial(http.Header{"Cookie": {cookie.String()}, "Origin": {srv.URL}})
	if err != nil {
		t.Fatalf("upgrade with the session cookie: %v", err)
	}
	conn.CloseNow()
}

func login(t *testing.T, h http.Handler, password, next string, want int) *http.Cookie {
	t.Helper()
	form := url.Values{"password": {password}, "next": {next}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("POST /login = %d, want %d:\n%s", rec.Code, want, rec.Body.String())
	}
	if want == http.StatusSeeOther && rec.Header().Get("Location") != next {
		t.Errorf("login redirected to %q, want %q", rec.Header().Get("Location"), next)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}
