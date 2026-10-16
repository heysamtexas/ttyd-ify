package files

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const testBase = "/files"

// fixture builds a tree under t.TempDir(), and returns the handler mounted the way wtd mounts
// it, so every URL a test sees is the URL a browser would. The password is wtd's job, in front
// of this handler, and is tested there.
func fixture(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "src")
	mustWrite(t, filepath.Join(root, "README.md"), "# Hello\n\n<script>alert(1)</script>\n")
	mustWrite(t, filepath.Join(root, "diagram.md"), "```mermaid\ngraph TD; A-->B\n```\n")
	mustWrite(t, filepath.Join(root, "a dir", "notes.txt"), "plain\n")
	mustWrite(t, filepath.Join(root, "demo.html"), "<script>fetch('/api/v1/sessions')</script>\n")
	mustWrite(t, filepath.Join(root, "site", "index.html"), "<h1>raw index</h1>\n")
	mustWrite(t, filepath.Join(root, "a:b.md"), "# colon\n")
	mustWrite(t, filepath.Join(root, "dangling.md"), "<img src=\"x\" alt='\n\n```mermaid\ngraph TD; A-->B\n```\n")
	mustWrite(t, filepath.Join(root, "code.md"), "```go\nfunc main() {}\n```\n")
	mustWrite(t, filepath.Join(dir, "secret"), "outside the root\n")
	h, err := New(root, testBase)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Method-less, unlike wtd's mount, so TestReadOnly reaches the browser's own 405.
	mux := http.NewServeMux()
	mux.Handle(testBase+"/", http.StripPrefix(testBase, h))
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

func get(h http.Handler, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
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
		rec := get(h, target, nil)
		if strings.Contains(rec.Body.String(), "outside the root") {
			t.Errorf("GET %s escaped the root: %d %q", target, rec.Code, rec.Body.String())
		}
	}
}

func TestLinksAndRedirectsCarryTheMount(t *testing.T) {
	h, _ := fixture(t)

	rec := get(h, "/files/a%20dir?all=1", nil)
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/files/a%20dir/?all=1" {
		t.Errorf("dir without slash = %d to %q, want 301 to /files/a%%20dir/?all=1",
			rec.Code, rec.Header().Get("Location"))
	}

	body := get(h, "/files/a%20dir/notes.txt", nil)
	if body.Code != http.StatusOK || body.Body.String() != "plain\n" {
		t.Errorf("file = %d %q", body.Code, body.Body.String())
	}

	page := get(h, "/files/a%20dir/", nil).Body.String()
	for _, want := range []string{`href="/files/"`, `href="./notes.txt"`} {
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

	md := get(h, "/files/README.md", nil)
	csp := md.Header().Get("Content-Security-Policy")
	m := regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("markdown page CSP = %q, want a nonce-only script-src", csp)
	}
	scriptSrc, _, _ := strings.Cut(strings.SplitN(csp, "script-src", 2)[1], ";")
	if strings.Contains(csp, "'self'") || strings.Contains(csp, "unsafe-eval") || strings.Contains(scriptSrc, "unsafe") {
		t.Errorf("markdown page CSP = %q trusts more than its nonce", csp)
	}
	if !strings.Contains(md.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("fixture no longer exercises raw HTML; this test would assert nothing")
	}
	if strings.Contains(md.Body.String(), "nonce=") {
		t.Error("a page with no diagram carries a nonced script")
	}

	diagram := get(h, "/files/diagram.md", nil)
	nonce := regexp.MustCompile(`'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(
		diagram.Header().Get("Content-Security-Policy"))
	if nonce == nil {
		t.Fatal("diagram page has no nonce")
	}
	want := `<script nonce="` + nonce[1] + `" src="/files/assets/mermaid.min.js"></script>`
	if !strings.Contains(diagram.Body.String(), want) {
		t.Errorf("diagram page lacks %s:\n%s", want, diagram.Body.String())
	}
	if again := get(h, "/files/diagram.md", nil).Header().Get("Content-Security-Policy"); strings.Contains(again, nonce[1]) {
		t.Error("the nonce repeated across responses")
	}

	for _, target := range []string{"/files/demo.html", "/files/README.md?raw=1", "/files/a%20dir/notes.txt"} {
		if got := get(h, target, nil).Header().Get("Content-Security-Policy"); got != rawCSP || !strings.HasPrefix(got, "sandbox;") {
			t.Errorf("GET %s CSP = %q, want %q", target, got, rawCSP)
		}
	}
}

func TestMermaidAssetIsTheOnlyAsset(t *testing.T) {
	h, _ := fixture(t)
	rec := get(h, "/files/assets/mermaid.min.js", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("mermaid = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Body.Len() < 1<<20 {
		t.Errorf("mermaid is %d bytes; the vendored file is ~2.7 MB", rec.Body.Len())
	}
	for _, target := range []string{"/files/assets/", "/files/assets/LICENSE.mermaid"} {
		if rec := get(h, target, nil); strings.Contains(rec.Body.String(), "MIT License") || rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "mermaid.min.js") {
			t.Errorf("GET %s exposes the embedded assets: %d", target, rec.Code)
		}
	}
}

func TestReadOnly(t *testing.T) {
	h, _ := fixture(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/files/README.md", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}
}

func TestNewRefusesAMissingRoot(t *testing.T) {
	if h, err := New(filepath.Join(t.TempDir(), "nope"), testBase); err == nil || h != nil {
		t.Fatalf("New(missing root) = %v, %v; want a refusal and no handler", h, err)
	}
}

// Every response from the browser carries nosniff, and every document refuses framing and
// defaults to loading nothing: these stop a file from the tree being promoted to a script, or
// the terminal's origin being framed by one.
func TestHardeningHeaders(t *testing.T) {
	h, _ := fixture(t)
	for _, target := range []string{"/files/", "/files/README.md", "/files/demo.html",
		"/files/a%20dir/notes.txt", "/files/assets/mermaid.min.js"} {
		rec := get(h, target, nil)
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s: X-Content-Type-Options = %q, want nosniff", target, got)
		}
		if target == "/files/assets/mermaid.min.js" {
			continue // a script, not a document; nothing to frame
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %s: CSP %q lacks %s", target, csp, want)
			}
		}
	}
}

// Untrusted markdown that leaves an attribute open must not be able to swallow the nonced
// mermaid tags (and so the nonce): they come before the body, and dangling markup can only
// capture what follows it.
func TestMermaidScriptsPrecedeTheMarkdown(t *testing.T) {
	h, _ := fixture(t)
	body := get(h, "/files/dangling.md", nil).Body.String()
	script := strings.Index(body, "<script nonce=")
	article := strings.Index(body, "<article>")
	if script < 0 || article < 0 {
		t.Fatalf("fixture no longer renders a diagram inside an article:\n%s", body)
	}
	if script > article {
		t.Error("the nonced script comes after the untrusted markdown, where dangling markup can capture it")
	}
}

// A file name with a colon must link as a path, not as a URL scheme ("a:b.md" is scheme "a").
func TestEntryLinksAreRelativePaths(t *testing.T) {
	h, _ := fixture(t)
	page := get(h, "/files/", nil).Body.String()
	if !strings.Contains(page, `href="./a:b.md"`) {
		t.Errorf("listing does not link a:b.md as ./a:b.md:\n%s", page)
	}
	for _, href := range regexp.MustCompile(`<li[^>]*><a href="([^"]*)"`).FindAllStringSubmatch(page, -1) {
		if !strings.HasPrefix(href[1], "./") {
			t.Errorf("entry link %q is not ./-relative", href[1])
		}
	}
	if rec := get(h, "/files/a:b.md", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "colon") {
		t.Errorf("GET /files/a:b.md = %d", rec.Code)
	}
}

// http.ServeFile redirects .../index.html to its directory, which this browser lists, so a raw
// index.html was unreachable. It must be served as itself, sandboxed like any raw file.
func TestRawIndexHTMLIsServed(t *testing.T) {
	h, _ := fixture(t)
	rec := get(h, "/files/site/index.html", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "raw index") {
		t.Fatalf("GET /files/site/index.html = %d %q, want the file itself", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != rawCSP {
		t.Errorf("raw index.html CSP = %q, want %q", got, rawCSP)
	}
}

// Highlighting was dropped: a code block renders as plain <pre><code>, with none of the inline
// styles a highlighter emits.
func TestCodeBlocksAreNotHighlighted(t *testing.T) {
	h, _ := fixture(t)
	body := get(h, "/files/code.md", nil).Body.String()
	if !strings.Contains(body, `<pre><code class="language-go">`) || strings.Contains(body, "style=") {
		t.Errorf("code block is not plain <pre><code>:\n%s", body)
	}
}
