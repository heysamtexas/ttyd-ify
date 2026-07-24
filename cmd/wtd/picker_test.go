package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// / must split on ?arg= the way api/openapi.yaml documents, because CLAUDE.md's
// deep-link test recipe (`/?arg=demo`) is the only way to drive the iOS client's hot path
// without a phone. If both URLs returned the same page, that recipe would silently stop
// testing anything.
func TestRootSplitsOnURLArg(t *testing.T) {
	srv, _ := newTestServer(t)

	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	picker := get("/")
	terminal := get("/?arg=demo")

	for name, rec := range map[string]*httptest.ResponseRecorder{"picker": picker, "terminal": terminal} {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q, want text/html", name, ct)
		}
	}

	if picker.Body.String() == terminal.Body.String() {
		t.Error("/ and /?arg= served the same page; the terminal/picker split is gone")
	}
	if !strings.Contains(terminal.Body.String(), "/vendor/xterm.js") {
		t.Error("terminal page does not load the vendored xterm bundle")
	}
	if !strings.Contains(picker.Body.String(), "/api/v1/sessions") {
		t.Error("picker does not call the sessions API")
	}
}

// The pages must not reference anything off-origin. This is not style: the terminal is
// normally reached over a tailnet with no route to the public internet, so a CDN reference
// would fail exactly where the tool is most needed.
func TestPagesHaveNoExternalReferences(t *testing.T) {
	for _, page := range []string{"web/index.html", "web/terminal.html"} {
		body, err := webFS.ReadFile(page)
		if err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		for _, marker := range []string{"http://", "https://", "//cdn", "unpkg", "jsdelivr"} {
			if strings.Contains(stripHTMLComments(string(body)), marker) {
				t.Errorf("%s references %q — assets must be same-origin", page, marker)
			}
		}
	}
}

// Vendored assets are served from an explicit allowlist rather than a FileServer, so that
// LICENSE files, PROVENANCE.md, SHA256SUMS and directory listings stay unexposed.
func TestVendorAllowlist(t *testing.T) {
	srv, _ := newTestServer(t)

	served := []string{"xterm.js", "xterm.css", "addon-fit.js"}
	for _, name := range served {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vendor/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("/vendor/%s: status = %d, want 200", name, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("/vendor/%s: empty body", name)
		}
	}

	blocked := []string{
		"LICENSE.xterm",
		"LICENSE.addon-fit",
		"PROVENANCE.md",
		"SHA256SUMS",
		"",
		"xterm.js.map",
	}
	for _, name := range blocked {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vendor/"+name, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("/vendor/%s is served but is not on the allowlist", name)
		}
	}
}

// Traversal attempts must never yield file contents. Go's mux normalizes and redirects
// rather than serving, and the embedded FS cannot reach outside the binary at all — this
// pins both so a future switch to a FileServer has to break a test first.
func TestVendorTraversal(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, target := range []string{
		"/vendor/..%2fopenapi.json",
		"/vendor/%2e%2e/openapi.json",
		"/vendor/....//openapi.json",
	} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s returned 200: %s", target, truncate(rec.Body.String(), 80))
		}
	}
}

func stripHTMLComments(s string) string {
	for {
		start := strings.Index(s, "<!--")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "-->")
		if end < 0 {
			return s[:start]
		}
		s = s[:start] + s[start+end+3:]
	}
}
