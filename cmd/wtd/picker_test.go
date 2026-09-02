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
	// Losing this one is invisible too: without the addon, URLs render as plain text and
	// nothing errors (#90).
	if !strings.Contains(terminal.Body.String(), "vendor/addon-web-links.js") {
		t.Error("terminal page does not load the web-links addon; URLs in output are dead text")
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

// URLs in output open on Cmd/Ctrl+click, and only on Cmd/Ctrl+click (#90).
//
// Every pinned shape below is load-bearing, because the input is attacker-influenceable —
// anything running in a session writes terminal output:
//
//   - the modifier-and-left-button gate keeps a click meant for text selection, or for a
//     TUI holding mouse tracking, from opening a tab;
//   - the scheme gate keeps output from opening file: or javascript: targets;
//   - noreferrer (which implies noopener) opens a new tab without handing this server's
//     address to a site chosen by output — a same-tab navigation would also silently drop
//     the page's socket;
//   - the hover readout is the anti-spoof half: an OSC 8 label can read as one URL and
//     open another, and the readout is the only place the real target is visible. It
//     replaces the confirm() dialog xterm pops for OSC 8 links when no linkHandler is set.
//
// Substring assertions on a static asset, like the banner test above: weak, but each fails
// if the corresponding gate is removed, which is the shape the regression would have.
func TestTerminalLinksRequireModifierAndNoopener(t *testing.T) {
	src, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(src)
	for _, want := range []string{
		"event.button !== 0",                        // left clicks only
		"!event.metaKey && !event.ctrlKey",          // the modifier gate
		`proto !== "http:" && proto !== "https:"`,   // the scheme gate
		`window.open(uri, "_blank", "noreferrer")`,  // new tab, no opener handle, no Referer
		"new WebLinksAddon.WebLinksAddon(openLink,", // bare URLs in text
		"activate: openLink",                        // OSC 8 hyperlinks, same gesture
		"hover: showLinkTarget",                     // the target readout
	} {
		if !strings.Contains(page, want) {
			t.Errorf("web/terminal.html has no %q — a link-opening gate or the target readout is gone (#90)", want)
		}
	}

	// The gesture is documented where a confused reader will look for it.
	help, err := webFS.ReadFile("web/help.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(help), "hold Cmd or Ctrl") {
		t.Error("web/help.html does not document the Cmd/Ctrl+click gesture (#90)")
	}
}

// Vendored assets are served from an explicit allowlist rather than a FileServer, so that
// LICENSE files, PROVENANCE.md, SHA256SUMS and directory listings stay unexposed.
func TestVendorAllowlist(t *testing.T) {
	srv, _ := newTestServer(t)

	served := []string{"xterm.js", "xterm.css", "addon-fit.js", "addon-webgl.js", "addon-web-links.js"}
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
		"LICENSE.addon-web-links",
		"PROVENANCE.md",
		"SHA256SUMS",
		"",
		"xterm.js.map",
		"addon-web-links.js.map",
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

// The tab tells you which agent needs you, or the feature does not exist (#96).
//
// The signal arrives in band — `ESC ] 1337 ; WTState=<state> BEL`, written by whatever runs in
// the session to its own tty — precisely so the server carries it without knowing it exists.
// That is the design's strength and its whole test surface: there is no Go code to exercise,
// because wtd is not supposed to have any. What can rot is this page, so pin the shapes whose
// removal would break the feature quietly rather than loudly:
//
//   - the OSC registration, without which every status byte is parsed by xterm and discarded;
//   - the key gate, because OSC 1337 is iTerm2's shared key=value space and a payload that is
//     not ours must fall through rather than be read as a state;
//   - both icon assignments. The swap in setStatus is the feature; the unconditional one at the
//     end is the *idle* icon, and it has to be outside setStatus because setStatus early-returns
//     when the state has not changed and every tab starts out idle. That bug shipped in review
//     and rendered no favicon at all on a fresh tab;
//   - the disconnect gate, without which a status still sitting in xterm's parse queue repaints
//     a tab whose socket has already closed, and nothing can take it down again;
//   - the bell guard, asserted as the whole expression on purpose. Pinning `!document.hidden`
//     and `sawStatusOSC` separately proved worthless: both also occur elsewhere in the file, so
//     deleting the entire guard left this test green. An ungated bell is worse than no bell —
//     readline rings one on every ambiguous tab completion, so the tab you are typing in goes
//     red — which makes this the one assertion the test most needed and least had.
//
// Comment lines are stripped first. The block above these handlers explains the protocol at
// length and names every one of these strings, so an assertion against the raw page would keep
// passing after the code it guards was deleted. That is not hypothetical either: it is why the
// bell guard is now pinned as one expression rather than as the names it mentions.
func TestTerminalPageRendersAgentStatus(t *testing.T) {
	src, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		t.Fatal(err)
	}
	page := stripJSLineComments(string(src))
	for want, why := range map[string]string{
		"registerOscHandler(OSC_STATUS":                      "the status OSC is not registered, so every WTState is parsed and dropped",
		`data.slice(0, eq) !== "WTState"`:                    "the key gate is gone; iTerm2's own OSC 1337 payloads would be read as states",
		"favicon.href = STATUS_ICONS[next]":                  "the favicon no longer changes, which is the whole feature",
		"favicon.href = STATUS_ICONS[status]":                "the idle icon is not assigned at load; a fresh tab shows the browser's blank document glyph, and nothing routes /favicon.ico",
		"if (next && !socketLive) return":                    "a status queued in xterm's parser can repaint a tab whose socket already closed, and nothing will clear it",
		"sawStatusOSC || statusFromBell || !document.hidden": "the bell guard is gone; readline's completion bell now reds the tab you are typing in, and an unrelated bell can overwrite a status the agent reported",
		"term.onBell(":                     "the bell fallback for agents without hooks is gone",
		"term.onTitleChange(":              "the shell's own window title is discarded again",
		"cleanTitle(":                      "titles from the session are no longer scrubbed, so a bidi override can reorder the tab strip",
		"isStatusGlyph(":                   "an agent's own animated spinner is back in the tab title, duplicating what the icon says",
		"if (title === baseTitle) return;": "an animating title rewrites document.title on every frame; Claude Code was measured setting one 308 times in 12 seconds",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("web/terminal.html has no %q — %s (#96)", want, why)
		}
	}

	// A tab that changes colour on its own is exactly the kind of surprise this page exists for.
	help, err := webFS.ReadFile("web/help.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(help), "WTState") {
		t.Error("web/help.html does not document the tab status signal, so nobody can emit one (#96)")
	}
}

// stripJSLineComments drops whole-line // comments. Deliberately not an inline stripper: that
// would need to know about string literals to avoid cutting a URL in half, and every assertion
// this guards against lives on its own line anyway.
func stripJSLineComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// No control characters in anything served to a browser (#96 aftermath).
//
// This shipped to the live box and black-screened the terminal. The status block's title
// sanitiser was written with backslash-u escapes, and the escapes went into the file as the
// characters they denote rather than as escape text, so the source carried a literal NUL. The
// HTML parser replaces a raw NUL in a script with U+FFFD, which inverted a range in the regex
// character class — start above end — and the page died on "Invalid regular expression" before
// term.open() ever ran.
//
// Everything else passed the whole way down: the Go tests, "node --check" (a literal control
// character inside a character class is valid JavaScript), and a byte-for-byte check that the
// server was serving the file. Only a browser objected.
//
// The corruption is close to invisible, which is what makes it worth a test rather than care:
// grep silently reclassifies a file containing a NUL as binary and prints nothing at all, which
// reads exactly like "no match" and was misread as one at the time. The sanitiser is now a
// numeric code-point filter with no escapes in it, and this keeps the whole class out.
//
// Tab and newline are the only control characters with any business in these files.
func TestServedAssetsHaveNoControlCharacters(t *testing.T) {
	assets, err := webFS.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range assets {
		if entry.IsDir() {
			continue // vendor/ is third-party, shipped exactly as fetched
		}
		name := "web/" + entry.Name()
		src, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range src {
			if (b < 0x20 && b != '\n' && b != '\t') || b == 0x7f {
				t.Errorf("%s: control character %#x at byte %d — a raw NUL becomes U+FFFD in the "+
					"HTML parser and can silently change the meaning of the code around it (#96)",
					name, b, i)
				break // one report per file is enough to locate it
			}
		}
	}
}

// The picker and a quiet terminal tab wear the same mark, and keep wearing it (#99).
//
// The picker is not a session, so it has no status to show — it takes the identity half of
// #96 only. That means the same icon is drawn by two pages that share no code, because
// serving a shared script would mean a new route and a new handler for twelve lines of
// canvas calls.
//
// The cost of that choice is drift, and it is the invisible kind: restyle the mark in one
// page and the two disagree with nothing to catch it, since neither page has a JS harness in
// this repo. So the block is delimited in both files and compared byte for byte. Editing
// either one alone fails here; editing both together passes, which is the intended workflow.
func TestIdleMarkMatchesAcrossPages(t *testing.T) {
	const (
		start = "  // --- idle mark"
		end   = "  // --- end idle mark ---"
	)
	extract := func(name string) string {
		t.Helper()
		src, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		page := string(src)
		i := strings.Index(page, start)
		j := strings.Index(page, end)
		if i < 0 || j < 0 {
			t.Fatalf("%s has no delimited idle-mark block — the picker and the terminal can now "+
				"draw different icons with nothing to notice (#99)", name)
		}
		return page[i : j+len(end)]
	}

	picker := extract("web/index.html")
	terminal := extract("web/terminal.html")
	if picker != terminal {
		t.Errorf("the idle mark differs between web/index.html and web/terminal.html, so the "+
			"picker tab and a quiet terminal tab no longer match (#99)\n\npicker:\n%s\n\nterminal:\n%s",
			picker, terminal)
	}

	// Drawing it is not enough; the picker has to actually install it as the tab icon.
	src, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := stripJSLineComments(string(src))
	for want, why := range map[string]string{
		"drawIdleMark(g)":            "the picker never draws the mark",
		`link.rel = "icon"`:          "the picker draws the mark and never installs it as the favicon",
		`c.toDataURL("image/png")`:   "the picker's icon is not turned into an image",
		"document.head.append(link)": "the icon link is never added to the document",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("web/index.html has no %q — %s (#99)", want, why)
		}
	}
}

// The picker's status colours must be the favicon's, or one state means two things depending on
// where you look at it (#108). The two pages cannot share code -- there is no build step and no JS
// harness in this repo -- so the colours are duplicated and this compares them.
//
// terminal.html owns them, in STATUS_COLORS. index.html restates them in CSS. Editing either alone
// fails here.
func TestPickerStatusColoursMatchTheFavicon(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		src, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(src)
	}
	terminal := read("web/terminal.html")
	picker := read("web/index.html")

	// Parsed out of terminal.html rather than written here twice, so this test cannot drift from
	// the source of truth in the same edit that breaks the pages.
	line := ""
	for _, l := range strings.Split(terminal, "\n") {
		if strings.Contains(l, "const STATUS_COLORS") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("web/terminal.html no longer declares STATUS_COLORS — this test can no longer " +
			"tell whether the picker agrees with the favicon (#108)")
	}

	for _, state := range []string{"running", "waiting", "attention"} {
		hex := statusHexFor(t, line, state)
		// The CSS rule for that state must carry the same hex. Checking the pair together, not
		// just that the hex appears somewhere, so swapping two colours still fails.
		rule := ".status." + state
		i := strings.Index(picker, rule)
		if i < 0 {
			t.Errorf("web/index.html has no %q rule, so the picker cannot render %s (#108)", rule, state)
			continue
		}
		end := strings.Index(picker[i:], "}")
		if end < 0 {
			t.Fatalf("%q rule in web/index.html is unterminated", rule)
		}
		if !strings.Contains(strings.ToLower(picker[i:i+end]), hex) {
			t.Errorf("web/index.html renders %s without %s, but the favicon uses it — the same "+
				"state now looks different in a tab and in the list (#108)\n  rule: %s",
				state, hex, picker[i:i+end])
		}
	}
}

// statusHexFor pulls one state's colour out of terminal.html's STATUS_COLORS line.
func statusHexFor(t *testing.T, line, state string) string {
	t.Helper()
	i := strings.Index(line, state+":")
	if i < 0 {
		t.Fatalf("STATUS_COLORS has no %s: %s", state, line)
	}
	rest := line[i:]
	j := strings.Index(rest, "\"")
	if j < 0 {
		t.Fatalf("STATUS_COLORS entry for %s has no quoted colour: %s", state, rest)
	}
	k := strings.Index(rest[j+1:], "\"")
	if k < 0 {
		t.Fatalf("STATUS_COLORS entry for %s has an unterminated colour: %s", state, rest)
	}
	return strings.ToLower(rest[j+1 : j+1+k])
}

// The two rules that decide whether this page tells the truth (#108).
func TestPickerGatesStatusAndSeparatesUnknownFromClear(t *testing.T) {
	src, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(src)

	for want, why := range map[string]string{
		// Gated on the flag, not on the field: a session nobody opened is null on a server that
		// fully supports this, so the value alone cannot distinguish unsupported from unobserved.
		`includes("session-status")`: "the picker renders status without checking the feature flag, " +
			"so it would show every row as unknown against an older server",
		// An allowlist, so an unrecognized value renders nothing instead of being read as `clear`.
		"hasOwnProperty.call(STATUS_TITLES": "the picker does not check the value against a known " +
			"set, so a new state from a newer server could render as something else",
		// `clear` must have its own visible mark; null renders nothing at all.
		".status.clear": "the picker has no rule for `clear`, so a session reporting " +
			"nothing-to-report looks identical to one nobody has ever observed",
	} {
		if !strings.Contains(stripJSLineComments(page), want) &&
			!strings.Contains(page, want) {
			t.Errorf("web/index.html has no %q — %s (#108)", want, why)
		}
	}

	// And the unknown case must genuinely render nothing: there is no CSS class for it.
	if strings.Contains(page, ".status.unknown") || strings.Contains(page, ".status.null") {
		t.Error("web/index.html styles an `unknown`/`null` status class, but null must render " +
			"nothing — an absent badge is what distinguishes it from `clear` (#108)")
	}
}

// The host panel's load-bearing behaviours, asserted against the served page.
//
// There is no JS test harness in this repo and adding one would mean a node toolchain for four
// pages, so the frontend contract is pinned by substring assertions the way the agent-status
// block is. Each expression below is here because losing it silently breaks something a reader
// of the page would not notice:
//
//   - The panel must never resize the pty. It is an overlay for exactly this reason, and a
//     well-meant sendResize() in the toggle would make every peek repaint a full-screen program.
//   - Polling must stop when the panel is closed or the tab is hidden. A tab left open behind
//     another one otherwise walks every session's process tree forever, which is the cost #115
//     records against a route built to be polled.
//   - Closing must hand focus back to the terminal, and nothing here may be reachable by
//     keyboard: every keystroke on this page belongs to the shell.
//   - null must not render as zero. `pressure: null` is a kernel with no PSI and 0.00 is the
//     reading on a healthy box, so a panel that draws them alike reports calm for a machine it
//     has measured nothing about -- the same trap agentStatus's null-versus-clear split names.
func TestTerminalPanelPollsOnlyWhenVisible(t *testing.T) {
	src, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		t.Fatalf("read terminal.html: %v", err)
	}
	page := string(src)

	for _, want := range []string{
		// The endpoint, fetched relatively like everything else on this page.
		`fetch("api/v1/host")`,
		// Both halves of the poll gate.
		`function panelPolling(on)`,
		`if (!document.hidden) pollHost();`,
		`panelPolling(open);`,
		// Focus discipline.
		`if (!open) term.focus();`,
		`tabindex="-1"`,
		// The verdict is computed here, not on the server, and "unknown" is a real outcome.
		`function hostVerdict(h)`,
		`worst === null ? "unknown" : worst`,
		// null-not-zero, for the two fields where the distinction decides what a reader believes.
		`out.append(row("stall", "not measured"));`,
		`if (h.sessions === null)`,
		// Heaviest first: the list answers "what do I close".
		`sort((a, b) => b.rssBytes - a.rssBytes)`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("terminal.html no longer contains %q", want)
		}
	}

	// The panel is an overlay: the only sendResize calls are the socket handshake and the
	// debounced window resize. A third one means something is now resizing the pty, and the
	// most likely candidate is a panel that shrinks #term.
	if n := strings.Count(page, "sendResize("); n != 3 {
		t.Errorf("sendResize( appears %d times, want 3 (the definition, the connect path and the window listener); a new caller means the panel may be resizing the pty", n)
	}
}

// The panel's severity colours must be the ones the rest of the UI already uses for the same
// meanings, or two parts of the same page disagree about what amber means.
func TestTerminalPanelReusesTheStatusPalette(t *testing.T) {
	src, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		t.Fatalf("read terminal.html: %v", err)
	}
	page := string(src)
	// Amber and red are the agent-status colours from STATUS_COLORS; green is the picker's
	// --live. Grey is the picker's --idle, used here for "not measured" and for the relative
	// session bars, which carry size and not severity.
	for name, hex := range map[string]string{
		"tight":    "#fbbf24",
		"critical": "#ef4444",
		"ok":       "#4ade80",
		"none":     "#9a9a94",
	} {
		if !strings.Contains(page, hex) {
			t.Errorf("the panel's %s colour %s is not in terminal.html", name, hex)
		}
	}
	// And the two it shares with the favicon must still match that block, which is the source.
	for _, hex := range []string{"#fbbf24", "#ef4444"} {
		if strings.Count(page, hex) < 2 {
			t.Errorf("%s appears once; the panel and STATUS_COLORS should both use it", hex)
		}
	}
}
