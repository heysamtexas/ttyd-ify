package files

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	testBase     = "/files"
	testPassword = "correct horse"
)

// fixture builds a tree and a 0640 password file under t.TempDir(), and returns the handler
// mounted the way wtd mounts it, so every URL a test sees is the URL a browser would.
func fixture(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "src")
	mustWrite(t, filepath.Join(root, "README.md"), "# Hello\n\n<script>alert(1)</script>\n")
	mustWrite(t, filepath.Join(root, "diagram.md"), "```mermaid\ngraph TD; A-->B\n```\n")
	mustWrite(t, filepath.Join(root, "a dir", "notes.txt"), "plain\n")
	mustWrite(t, filepath.Join(root, "demo.html"), "<script>fetch('/api/v1/sessions')</script>\n")
	mustWrite(t, filepath.Join(dir, "secret"), "outside the root\n")
	passwd := writeHash(t, dir, 0o640)
	h, err := New(root, passwd, testBase)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+testBase+"/", http.StripPrefix(testBase, h))
	mux.Handle("POST "+testBase+"/login", http.StripPrefix(testBase, h))
	return mux, root
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeHash(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "passwd")
	if err := os.WriteFile(p, append(hash, '\n'), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask must not decide what is being tested
		t.Fatal(err)
	}
	return p
}

func get(h http.Handler, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func authed(r *http.Request) { r.SetBasicAuth("", testPassword) }

func TestNewFailsClosed(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not a hash\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	worldDir := t.TempDir()
	cases := map[string]struct{ root, passwd string }{
		"no password file configured":  {root, ""},
		"password file missing":        {root, filepath.Join(dir, "nope")},
		"password file world-readable": {root, writeHash(t, worldDir, 0o644)},
		"password file not bcrypt":     {root, bad},
		"root missing":                 {filepath.Join(dir, "no-such-root"), writeHash(t, t.TempDir(), 0o640)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if h, err := New(c.root, c.passwd, testBase); err == nil || h != nil {
				t.Fatalf("New(%q, %q) = %v, %v; want a refusal and no handler", c.root, c.passwd, h, err)
			}
		})
	}
}

func TestGateRefusesWithoutCredentials(t *testing.T) {
	h, _ := fixture(t)
	for _, target := range []string{"/files/", "/files/README.md", "/files/assets/mermaid.min.js"} {
		rec := get(h, target, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials = %d, want 401", target, rec.Code)
		}
		if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("GET %s: no Basic challenge, so `curl -u` cannot work", target)
		}
	}
	rec := get(h, "/files/", func(r *http.Request) { r.SetBasicAuth("", "wrong password") })
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", rec.Code)
	}
}

func TestBrowserIsSentToLoginUnderTheMount(t *testing.T) {
	h, _ := fixture(t)
	rec := get(h, "/files/a%20dir/?all=1", func(r *http.Request) { r.Header.Set("Accept", "text/html") })
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != "/files/login" {
		t.Errorf("login redirect path = %q, want /files/login", loc.Path)
	}
	if got := loc.Query().Get("next"); got != "/files/a%20dir/?all=1" {
		t.Errorf("next = %q, want the original request under /files", got)
	}
}

func TestLoginRoundTrip(t *testing.T) {
	h, _ := fixture(t)

	page := get(h, "/files/login?next=/files/README.md", nil)
	if !strings.Contains(page.Body.String(), `action="/files/login"`) {
		t.Errorf("login form does not post to /files/login:\n%s", page.Body.String())
	}

	form := url.Values{"password": {testPassword}, "next": {"/files/README.md"}}
	req := httptest.NewRequest(http.MethodPost, "/files/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/files/README.md" {
		t.Fatalf("login = %d to %q, want 303 to /files/README.md", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Path != "/files/" {
		t.Errorf("cookie Path = %q, want /files/ — the terminal half has no business carrying it", c.Path)
	}
	if !c.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}

	ok := get(h, "/files/README.md", func(r *http.Request) { r.AddCookie(c) })
	if ok.Code != http.StatusOK {
		t.Errorf("GET with the session cookie = %d, want 200", ok.Code)
	}
	forged := *c
	exp, _, _ := strings.Cut(c.Value, ".")
	forged.Value = exp + ".not-the-signature"
	if rec := get(h, "/files/", func(r *http.Request) { r.AddCookie(&forged) }); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET with a forged cookie = %d, want 401", rec.Code)
	}
}

func TestLoginRefusesOffsiteNext(t *testing.T) {
	a := &auth{base: testBase}
	for _, next := range []string{"//evil.example/", "https://evil.example/", "/", "/api/v1/sessions",
		"/filesystem", `/files/\evil.example`, ""} {
		if got := a.safeNext(next); got != "/files/" {
			t.Errorf("safeNext(%q) = %q, want /files/", next, got)
		}
	}
	if got := a.safeNext("/files/a/b.md?raw=1"); got != "/files/a/b.md?raw=1" {
		t.Errorf("safeNext kept the wrong thing: %q", got)
	}
}

func TestWrongPasswordIsDelayed(t *testing.T) {
	h, _ := fixture(t)
	start := time.Now()
	form := url.Values{"password": {"nope nope"}}
	req := httptest.NewRequest(http.MethodPost, "/files/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", rec.Code)
	}
	if time.Since(start) < failDelay {
		t.Errorf("a wrong password answered in %v, under failDelay", time.Since(start))
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a wrong password set a cookie")
	}
}

// The jail is lexical; every spelling of "up" has to land inside the root or nowhere.
func TestPathJail(t *testing.T) {
	h, _ := fixture(t)
	for _, target := range []string{
		"/files/../secret",
		"/files/%2e%2e/secret",
		"/files/a%20dir/../../secret",
		"/files/a%20dir/%2e%2e/%2e%2e/secret",
		"/files/..%2fsecret",
	} {
		rec := get(h, target, authed)
		if strings.Contains(rec.Body.String(), "outside the root") {
			t.Errorf("GET %s escaped the root: %d %q", target, rec.Code, rec.Body.String())
		}
	}
}

func TestLinksAndRedirectsCarryTheMount(t *testing.T) {
	h, _ := fixture(t)

	rec := get(h, "/files/a%20dir?all=1", authed)
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/files/a%20dir/?all=1" {
		t.Errorf("dir without slash = %d to %q, want 301 to /files/a%%20dir/?all=1",
			rec.Code, rec.Header().Get("Location"))
	}

	body := get(h, "/files/a%20dir/notes.txt", authed)
	if body.Code != http.StatusOK || body.Body.String() != "plain\n" {
		t.Errorf("file = %d %q", body.Code, body.Body.String())
	}

	page := get(h, "/files/a%20dir/", authed).Body.String()
	for _, want := range []string{`href="/files/"`, `href="notes.txt"`} {
		if !strings.Contains(page, want) {
			t.Errorf("dir page lacks %s:\n%s", want, page)
		}
	}
	for _, href := range regexp.MustCompile(`href="(/[^"]*)"`).FindAllStringSubmatch(page, -1) {
		if !strings.HasPrefix(href[1], "/files/") {
			t.Errorf("absolute link %q escapes the mount point", href[1])
		}
	}
}

// Same origin as /ws: nothing from the tree may run script. These assert the headers that
// make it so, because a browser is the only thing that would otherwise notice.
func TestTreeContentCannotRunScript(t *testing.T) {
	h, _ := fixture(t)

	md := get(h, "/files/README.md", authed)
	csp := md.Header().Get("Content-Security-Policy")
	m := regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("markdown page CSP = %q, want a nonce-only script-src", csp)
	}
	if strings.Contains(csp, "'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("markdown page CSP = %q trusts more than its nonce", csp)
	}
	if !strings.Contains(md.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("fixture no longer exercises raw HTML; this test would assert nothing")
	}
	if strings.Contains(md.Body.String(), "nonce=") {
		t.Error("a page with no diagram carries a nonced script")
	}

	diagram := get(h, "/files/diagram.md", authed)
	nonce := regexp.MustCompile(`'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(
		diagram.Header().Get("Content-Security-Policy"))
	if nonce == nil {
		t.Fatal("diagram page has no nonce")
	}
	want := `<script nonce="` + nonce[1] + `" src="/files/assets/mermaid.min.js"></script>`
	if !strings.Contains(diagram.Body.String(), want) {
		t.Errorf("diagram page lacks %s:\n%s", want, diagram.Body.String())
	}
	if again := get(h, "/files/diagram.md", authed).Header().Get("Content-Security-Policy"); strings.Contains(again, nonce[1]) {
		t.Error("the nonce repeated across responses")
	}

	for _, target := range []string{"/files/demo.html", "/files/README.md?raw=1", "/files/a%20dir/notes.txt"} {
		if got := get(h, target, authed).Header().Get("Content-Security-Policy"); got != "sandbox" {
			t.Errorf("GET %s CSP = %q, want sandbox", target, got)
		}
	}
}

func TestMermaidAssetIsTheOnlyAsset(t *testing.T) {
	h, _ := fixture(t)
	rec := get(h, "/files/assets/mermaid.min.js", authed)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("mermaid = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Body.Len() < 1<<20 {
		t.Errorf("mermaid is %d bytes; the vendored file is ~2.7 MB", rec.Body.Len())
	}
	for _, target := range []string{"/files/assets/", "/files/assets/LICENSE.mermaid"} {
		if rec := get(h, target, authed); strings.Contains(rec.Body.String(), "MIT License") || rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "mermaid.min.js") {
			t.Errorf("GET %s exposes the embedded assets: %d", target, rec.Code)
		}
	}
}

func TestReadOnly(t *testing.T) {
	h, _ := fixture(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/files/README.md", nil)
		authed(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}
}
