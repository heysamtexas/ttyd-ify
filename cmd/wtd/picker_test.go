package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
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
	if !strings.Contains(terminal.Body.String(), "vendor/xterm.js") {
		t.Error("terminal page does not load the vendored xterm bundle")
	}
	// Losing the WebGL renderer is invisible — xterm silently falls back to building a DOM
	// node per cell, and the only symptom is that scrolling gets slow again (#61).
	if !strings.Contains(terminal.Body.String(), "vendor/addon-webgl.js") {
		t.Error("terminal page does not load the WebGL renderer; scrolling falls back to the DOM renderer")
	}
	if !strings.Contains(picker.Body.String(), "api/v1/sessions") {
		t.Error("picker does not call the sessions API")
	}
}

// The pages must not reference anything off-origin. This is not style: the terminal is
// normally reached over a tailnet with no route to the public internet, so a CDN reference
// would fail exactly where the tool is most needed.
func TestPagesHaveNoExternalReferences(t *testing.T) {
	for _, page := range []string{"web/index.html", "web/terminal.html", "web/help.html", "web/help.css"} {
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

// /help is one document reachable three ways: directly, from the picker's header link, and
// through the terminal page's "?" overlay, which fetches it and injects <main>. Those are
// string references nothing type-checks, so pin them here — a rename that missed one would
// leave a dead link, or an overlay that can never load its content.
func TestHelpPageAndItsEntryPoints(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/help", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /help: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /help: Content-Type = %q, want text/html", ct)
	}
	body := stripHTMLComments(rec.Body.String())

	// The overlay's extraction contract: querySelector("main") with real content in it.
	// An empty <main> would pass a bare substring check while the overlay, whose
	// replaceChildren removes the loading placeholder, rendered an empty panel.
	start, end := strings.Index(body, "<main"), strings.Index(body, "</main>")
	if start < 0 || end < start {
		t.Fatal("GET /help: no <main> element; the terminal overlay injects main's children")
	}
	content := body[start:end]
	if n := strings.Count(content, "<details"); n < 3 {
		t.Errorf("GET /help: %d findings in <main>, want at least 3 — is the copy gutted?", n)
	}
	// The finding that motivated the page (#55), asserted outside HTML comments.
	if !strings.Contains(content, "newline") {
		t.Error("GET /help: the multi-line-input finding is gone — is the copy stale?")
	}
	// Script-free is what keeps injecting this document into the live terminal page a
	// non-decision. Injection via replaceChildren does not execute scripts today, but
	// nothing should get the chance to start relying on that.
	if strings.Contains(body, "<script") {
		t.Error("GET /help carries a <script>; the page is injected into the terminal page and must stay script-free")
	}

	// The shared FAQ stylesheet is routed outside the spec (like /vendor/*), so the
	// documented-routes drift check never sees it.
	css := httptest.NewRecorder()
	srv.routes().ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/help.css", nil))
	if css.Code != http.StatusOK || !strings.Contains(css.Body.String(), ".faq") {
		t.Errorf("GET /help.css: status = %d — the shared FAQ stylesheet is gone", css.Code)
	}

	entryPoints := map[string][]string{
		"web/index.html":    {`href="help"`},
		"web/terminal.html": {`fetch("help")`, `href="help.css"`},
		"web/help.html":     {`href="help.css"`, `<main class="faq">`},
	}
	for page, wants := range entryPoints {
		src, err := webFS.ReadFile(page)
		if err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(src), want) {
				t.Errorf("%s does not contain %s — a help entry point or its styling is gone", page, want)
			}
		}
	}
}

// The disconnect banner must branch on the close code (#56).
//
// api/ws-protocol.md section 13 defines 1000 as "the terminal's process exited … treat as
// final", and the page appended "the session is still running" to every close regardless. That
// is wrong in the state a user is most likely to be in — just after typing `exit` — and it made
// the reconnect button lie, because `dtach -A` attaches *or creates*: pressing it started a new
// empty shell under the same name, which reads as a rejoin. The iOS client shipped the same bug
// from the other direction and "silently recreated the session the user had just exited".
//
// A substring assertion on a static asset is weak, and it is what there is: these pages have no
// JS harness in this repo. It earns its place by failing if someone collapses `onclose` back
// into one unconditional message, which is the shape the bug had.
func TestTerminalBannerBranchesOnCloseCode(t *testing.T) {
	src, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(src)
	for _, want := range []string{
		"ev.code === 1000",             // the branch itself
		"session ended",                // what 1000 must say instead
		"the session is still running", // still correct for 1001/1006/1013
		"new session",                  // the button's honest label after 1000
	} {
		if !strings.Contains(page, want) {
			t.Errorf("web/terminal.html has no %q — the close-code branch is gone, so the banner claims a dead session is running (#56)", want)
		}
	}
}

// The help page describes the banner, so the two drift as a pair. Its first draft canonized the
// banner text as "literal", which is how a wrong banner becomes a wrong promise — the reason #56
// was filed at all. This asserts the direction that actually bites: help.html claiming the banner
// cannot tell an exited session apart, after it can.
func TestHelpPageDoesNotUnderstateTheBanner(t *testing.T) {
	src, err := webFS.ReadFile("web/help.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "cannot yet tell") {
		t.Error(`web/help.html still says the banner "cannot yet tell" an ended session apart, but it branches on close 1000 now (#56)`)
	}
}

// The page must handle xterm's *other* output event (#64).
//
// xterm splits mouse reports by encoding: CoreMouseService does `"DEFAULT" === activeEncoding ?
// triggerBinaryEvent(t) : triggerDataEvent(t, true)`, and DEFAULT is X10. The page registered
// onData only, so under X10 every click, drag and wheel report was silently discarded — the app
// had enabled mouse mode, the page consumed the event and sent nothing.
//
// Both halves are asserted, because fixing one without the other still sends the wrong thing:
// the handler has to exist, *and* it has to route through the binary path. onBinary delivers a
// "binary string" of one byte per character code, so encoding it as UTF-8 expands every X10
// coordinate above 127 into two bytes and the application reads a different column.
func TestTerminalPageHandlesBinaryMouseReports(t *testing.T) {
	src, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(src)
	if !strings.Contains(page, "term.onBinary(") {
		t.Error("web/terminal.html does not register term.onBinary — under X10 mouse encoding " +
			"every click, drag and wheel report is silently dropped (#64)")
	}
	// The routing, not just the registration: `sendReport(d, false)` here would compile, run, and
	// corrupt every report past column 95.
	if !strings.Contains(page, "term.onBinary((d) => sendReport(d, true))") {
		t.Error("web/terminal.html does not route onBinary through the binary send path; a UTF-8 " +
			"encode changes the coordinates in an X10 report (#64)")
	}
	// And the latin-1 encode those reports depend on must still exist.
	if !strings.Contains(page, "charCodeAt(i)") {
		t.Error("web/terminal.html no longer maps a binary payload byte-per-character-code; " +
			"TextEncoder would UTF-8 expand X10 coordinates (#64)")
	}
}

// Vendored assets are served from an explicit allowlist rather than a FileServer, so that
// LICENSE files, PROVENANCE.md, SHA256SUMS and directory listings stay unexposed.
func TestVendorAllowlist(t *testing.T) {
	srv, _ := newTestServer(t)

	served := []string{"xterm.js", "xterm.css", "addon-fit.js", "addon-webgl.js"}
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
		"LICENSE.addon-webgl",
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

// The allowlist is friction by design, which means it is also a way to ship a page whose
// <script> 404s. Nothing else would notice: xterm's renderer addon failing to load is a
// silent downgrade, not an error. So resolve every /vendor/ reference the pages actually
// make, rather than trusting the two lists to have been edited together.
func TestPagesReferenceOnlyAllowlistedVendorAssets(t *testing.T) {
	srv, _ := newTestServer(t)

	// The reference is relative now (#57), so this resolves it the way a browser would before
	// asking the server: the pages are only served at "/", so "vendor/x" is "/vendor/x".
	ref := regexp.MustCompile(`\bvendor/([\w.-]+)`)
	for _, page := range []string{"web/index.html", "web/terminal.html", "web/help.html"} {
		src, err := webFS.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		// Comments out first: terminal.html's header cites vendor/PROVENANCE.md, which is
		// deliberately *not* served (TestVendorAllowlist pins that), and a citation is not a
		// reference the page loads. The old regex required a leading slash and skipped it by
		// accident; relative references make that accident stop working.
		for _, m := range ref.FindAllStringSubmatch(stripHTMLComments(string(src)), -1) {
			resolved := "/vendor/" + m[1]
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, resolved, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s loads %q (resolving to %s) but the server answers %d — add it to vendorAssets",
					page, m[0], resolved, rec.Code)
			}
		}
	}
}

// api/ws-protocol.md section 1: "pages MUST use relative URLs so they still work behind a
// stripping proxy." The pages never complied (#57) — every asset, /token, /help and /api/v1/*
// was root-absolute, and so was the socket URL, which was assembled from location.host and
// therefore discarded the path outright. Behind a prefix-stripping proxy (a deployment the iOS
// client supports via ServerProfile.pathPrefix) that meant the page loaded from the proxy and
// then reached past it for everything it needed.
//
// Asserted as the *absence of the shapes that can carry a URL*, rather than as a list of the
// references that exist today. A list would have passed unchanged while #55 added two more
// root-absolute references, which is exactly how this MUST drifted for as long as it did.
func TestPagesUseRelativeURLs(t *testing.T) {
	// Every way these pages name something to fetch. `url(` covers the stylesheet.
	forbidden := []string{`href="/`, `src="/`, `fetch("/`, `url(/`, `api("/`}
	for _, page := range []string{"web/index.html", "web/terminal.html", "web/help.html", "web/help.css"} {
		src, err := webFS.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		// Comments describe server routes, which really are at /token and /help.css — it is the
		// page's own references that have to be relative.
		body := stripHTMLComments(string(src))
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains a root-absolute reference %q — ws-protocol section 1 requires relative URLs (#57)", page, bad)
			}
		}
	}

	// The socket URL is the one a prefix scan cannot catch, because the old shape named no path
	// at all: proto + "//" + location.host + "/ws". Resolving against location.href instead is
	// what makes it inherit the prefix along with everything else.
	term, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(term)
	if strings.Contains(page, `"//" + location.host`) {
		t.Error(`web/terminal.html builds the socket URL from location.host, which drops any path prefix — resolve "ws" against location.href instead (#57)`)
	}
	if !strings.Contains(page, `new URL("ws" + location.search, location.href)`) {
		t.Error("web/terminal.html no longer resolves the socket URL relative to the page (#57)")
	}
}

// Cache-Control: no-cache asks for revalidation, which only saves anything if the response
// carries a validator to revalidate against. Without one the browser re-downloads the whole
// bundle on every page load (#62) — invisible on a LAN, a wait on a relayed tailnet link.
func TestVendorAssetsRevalidate(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vendor/xterm.js", nil))
	tag := rec.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag on a vendored asset: Cache-Control: no-cache cannot revalidate without one")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}

	req := httptest.NewRequest(http.MethodGet, "/vendor/xterm.js", nil)
	req.Header.Set("If-None-Match", tag)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match with the current tag: status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried a %d-byte body", rec.Body.Len())
	}

	// A stale tag must still get the file, or an upgraded binary would serve the old asset.
	req = httptest.NewRequest(http.MethodGet, "/vendor/xterm.js", nil)
	req.Header.Set("If-None-Match", `"stale"`)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("stale If-None-Match: status = %d, %d bytes; want 200 with a body", rec.Code, rec.Body.Len())
	}

	// Distinct assets must not share a validator, or one would be served as another.
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vendor/addon-fit.js", nil))
	if other := rec.Header().Get("ETag"); other == tag {
		t.Errorf("addon-fit.js and xterm.js share the ETag %s", tag)
	}
}

// The pages carry no-store, so they must not also claim a validator — that combination
// tells a cache two different things and it is the served page, not the vendored bundle,
// that has to reflect a restarted server immediately.
func TestPagesAreNotCached(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, target := range []string{"/", "/?arg=demo", "/help", "/help.css"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s: Cache-Control = %q, want no-store", target, cc)
		}
		if tag := rec.Header().Get("ETag"); tag != "" {
			t.Errorf("GET %s: no-store response also carries ETag %s", target, tag)
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
