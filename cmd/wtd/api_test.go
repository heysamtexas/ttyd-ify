package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// newTestServer points the server at a scratch session dir. Nothing here may touch
// ~/.dtach: on a developer box that holds real sessions, possibly including the one
// running the test.
func newTestServer(t *testing.T) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WT_DIR", dir)
	return newServer(""), dir
}

// The CSRF policy is the only thing standing between an unauthenticated mutating API and
// any web page the user happens to visit, so each rule gets a test.
func TestMutatingRoutesRejectCrossOrigin(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.routes()

	cases := []struct {
		name, method, path, origin, contentType string
		wantStatus                              int
		wantCode                                string
	}{
		{
			name: "cross-site origin", method: http.MethodPost, path: "/api/v1/sessions",
			origin: "http://evil.example", contentType: "application/json",
			wantStatus: http.StatusForbidden, wantCode: codeOriginForbidden,
		},
		{
			// "null" is what a sandboxed iframe or a redirect chain sends. It names no
			// host, so it can never match, and treating it as absent would be a hole.
			name: "origin null", method: http.MethodPost, path: "/api/v1/sessions",
			origin: "null", contentType: "application/json",
			wantStatus: http.StatusForbidden, wantCode: codeOriginForbidden,
		},
		{
			name: "cross-site origin on delete", method: http.MethodDelete, path: "/api/v1/sessions/x",
			origin:     "http://evil.example",
			wantStatus: http.StatusForbidden, wantCode: codeOriginForbidden,
		},
		{
			// A plain HTML form can only send these content types, so requiring JSON
			// kills form-based CSRF outright.
			name: "form-encoded post", method: http.MethodPost, path: "/api/v1/sessions",
			contentType: "application/x-www-form-urlencoded",
			wantStatus:  http.StatusUnsupportedMediaType, wantCode: codeUnsupportedMedia,
		},
		{
			name: "multipart post", method: http.MethodPost, path: "/api/v1/sessions",
			contentType: "multipart/form-data",
			wantStatus:  http.StatusUnsupportedMediaType, wantCode: codeUnsupportedMedia,
		},
		{
			name: "text/plain post", method: http.MethodPost, path: "/api/v1/sessions",
			contentType: "text/plain",
			wantStatus:  http.StatusUnsupportedMediaType, wantCode: codeUnsupportedMedia,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"name":"x"}`))
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := errorCode(t, rec.Body.Bytes()); got != tc.wantCode {
				t.Errorf("error code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// A same-origin request must pass the policy — the browser picker is served from this very
// origin, so a policy that rejected it would break the UI it exists to protect.
func TestSameOriginPassesPolicy(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"name":"../bad"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	// It should get past the CSRF gate and fail on name validation instead.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("same-origin request was rejected by the CSRF gate: %s", rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != codeInvalidName {
		t.Errorf("error code = %q, want %q (should have reached name validation)", got, codeInvalidName)
	}
}

func TestDeleteTraversalIsJustNotFound(t *testing.T) {
	srv, dir := newTestServer(t)

	// A real file outside the session dir that a traversal would target.
	victim := filepath.Join(filepath.Dir(dir), "victim.txt")
	if err := os.WriteFile(victim, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"../victim.txt",
		"..%2fvictim.txt",
		"....//victim.txt",
		"/etc/passwd",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+name, nil)
		srv.routes().ServeHTTP(rec, req)

		// 404 (no such session) or 405/301 from the router are all acceptable; what is
		// not acceptable is a 204 or anything touching the victim.
		if rec.Code == http.StatusNoContent {
			t.Errorf("DELETE %q reported success", name)
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("DELETE %q removed a file outside the session dir: %v", name, err)
		}
	}
}

// Create-side name rules. Each exists because of a specific downstream failure, so each
// is worth a case: the bash menu globs *.sock (a leading dot hides the session) and
// The deep-link path drops names containing ".." (making them unreachable).
func TestValidateSessionName(t *testing.T) {
	valid := []string{"dev", "my-app", "a", "under_score", "dot.ted", strings.Repeat("x", 64)}
	for _, name := range valid {
		if err := validateSessionName(name); err != nil {
			t.Errorf("validateSessionName(%q) = %v, want accepted", name, err)
		}
	}

	invalid := []string{
		"",                        // empty
		strings.Repeat("x", 65),   // too long
		".hidden",                 // invisible to the menu's glob
		"..",                      // unreachable by deep link
		"a..b",                    // ditto
		"../x",                    // traversal shape
		"has space",               // menu accepts, create does not
		"slash/name",              // would build a path
		"semi;colon",              // shell-ish
		"new\nline",               // control byte
		"üñî",                     // non-ASCII
		string([]byte{'a', 0x00}), // NUL
	}
	for _, name := range invalid {
		if err := validateSessionName(name); err == nil {
			t.Errorf("validateSessionName(%q) = nil, want rejected", name)
		}
	}
}

// Listing must be permissive where creating is strict, or a session made from the terminal
// menu would be invisible to the app and the two pickers would disagree.
func TestListingIsLooserThanCreating(t *testing.T) {
	dir := t.TempDir()
	mkSocket(t, dir, "has space.sock", 0o600)

	if err := validateSessionName("has space"); err == nil {
		t.Fatal("precondition: create should reject this name")
	}
	sessions, err := listSessions(dir, nil)
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name != "has space" {
		t.Errorf("listSessions = %+v, want the space-containing name listed anyway", sessions)
	}
}

func TestMetaAdvertisesFeatures(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var meta struct {
		Version      string   `json:"version"`
		Features     []string `json:"features"`
		TerminalPath string   `json:"terminalPath"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// features[] is the only cross-repo skew defense there is; an empty list would make
	// every client's feature detection silently fall back forever.
	if len(meta.Features) == 0 {
		t.Error("features is empty")
	}
	// Two of the names are load-bearing for a shipped phone, so they are asserted by name rather
	// than by the list merely being non-empty. `sessions-api` is a hard requirement — an app that
	// does not see it shows "Not a wtd Server" and offers Retry, with no degraded mode to fall back
	// to — and `scrollback-replay` gates the client's redraw kick, so withdrawing it silently
	// re-enables a workaround against a server that no longer needs it. Renaming either one is a
	// two-repo change; see .claude/rules/ios-client.md.
	for _, want := range []string{"sessions-api", "scrollback-replay"} {
		if !slices.Contains(meta.Features, want) {
			t.Errorf("features %v is missing %q, which a shipped iOS build gates on", meta.Features, want)
		}
	}
	if meta.TerminalPath != "/ws" {
		t.Errorf("terminalPath = %q, want /ws", meta.TerminalPath)
	}
}

// The keys handleMeta returns must be exactly the keys the served schema declares.
//
// This is the check that was missing, and its absence is why `terminalPath` sat in the
// published spec described as "absolute path of the start command (WT_PICKER)" while the
// handler returned `/ws` — a client-facing field, wrong on the wire, with CI green. Nothing
// read a description, and nothing compared the schema's field list to the handler's output.
//
// Descriptions are prose and cannot be asserted. The key set can, and it catches the whole
// class: a field added to the handler without a schema entry, a field promised by the schema
// and never sent, and a rename on either side.
func TestMetaMatchesItsSchema(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas struct {
				Meta struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"Meta"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}
	declared := spec.Components.Schemas.Meta.Properties
	if len(declared) == 0 {
		t.Fatal("the embedded spec declares no Meta properties; this test would assert nothing")
	}

	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode the response: %v", err)
	}

	for _, name := range spec.Components.Schemas.Meta.Required {
		if _, ok := got[name]; !ok {
			t.Errorf("the spec requires Meta.%s but the response omits it", name)
		}
	}
	for name := range got {
		if _, ok := declared[name]; !ok {
			t.Errorf("the response carries %q, which the Meta schema does not declare — a "+
				"client generated from the spec would not know about it", name)
		}
	}

	// Live assertion, but it can only fire on a box whose kernel hostname is qualified — most
	// are not, so TestShortHostname carries the deterministic half.
	if h, ok := got["hostname"].(string); ok && strings.Contains(h, ".") {
		t.Errorf("hostname = %q contains a dot; the schema promises the short form", h)
	}
}

// apiPath must name the prefix the API is actually served under.
//
// It exists so a client builds URLs from the server's own answer instead of hardcoding, which
// only helps if the answer is true — an advertised prefix nothing responds under is worse than
// no field at all, because a client that hardcoded /api/v1 works and one that believed the
// field 404s on every call. So this uses the *returned* value as the prefix rather than
// comparing two constants, which would agree with each other and prove nothing.
//
// terminalPath deliberately has no equivalent here: TestMeta already pins its exact value, and
// the spec-path walk in TestOpenAPIEndpoint already proves /ws is routed. Repeating it would
// only re-test those two.
func TestMetaAPIPathIsWhereTheAPIListens(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))
	var meta struct {
		APIPath string `json:"apiPath"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if meta.APIPath == "" {
		t.Fatal("apiPath is empty; a client has nothing to build a URL from")
	}

	// The GET routes below apiPath: the ones a client reaches first, and the only ones that
	// answer 200 without creating anything.
	for _, route := range []string{"/meta", "/sessions", "/projects"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, meta.APIPath+route, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s%s = %d, want 200 — apiPath does not name where the API is served",
				meta.APIPath, route, rec.Code)
		}
	}
}

// The schema promises Meta.hostname is the short form the terminal picker shows. On a box whose
// own hostname has no dot the live check above proves nothing, so the rule is pinned here.
func TestShortHostname(t *testing.T) {
	cases := map[string]string{
		"box.example.com": "box",
		"box.local":       "box",
		"box":             "box",
		"":                "",
	}
	for in, want := range cases {
		if got := shortHostname(in); got != want {
			t.Errorf("shortHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

// The documents the spec references must actually be served, because the spec now cites them by
// URL. Before they were served it cited them by filename at a reader who had only the spec —
// a footnote telling them they were missing something required, without telling them what.
// The published `name` pattern must compile under RE2 and must agree with validateSessionName.
//
// Two separate failures, one test. The pattern previously used ECMA-262 lookaheads —
// `(?!\.)(?!.*\.\.)` — which are legal in OpenAPI and which **RE2 rejects**, so a Go or Rust
// client generated from the spec could not compile it. The irony is on the record:
// validateSessionName is hand-rolled with a comment saying Go's RE2 has no lookahead, so we knew
// and published it anyway. regexp.Compile below is the whole guard against that returning.
//
// Agreement is the second half. A pattern that compiles but accepts names the server refuses is
// worse than none: a client pre-validates, submits, and gets a 400 it was told could not happen.
func TestPublishedNamePatternMatchesTheValidator(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas struct {
				SessionCreateRequest struct {
					Properties struct {
						Name struct {
							Pattern   string `json:"pattern"`
							MinLength int    `json:"minLength"`
							MaxLength int    `json:"maxLength"`
						} `json:"name"`
					} `json:"properties"`
				} `json:"SessionCreateRequest"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}
	p := spec.Components.Schemas.SessionCreateRequest.Properties.Name
	if p.Pattern == "" {
		t.Fatal("no name pattern in the served spec; this test would assert nothing")
	}
	if p.MaxLength != maxSessionNameLen {
		t.Errorf("spec maxLength = %d, server maxSessionNameLen = %d", p.MaxLength, maxSessionNameLen)
	}

	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		t.Fatalf("the published pattern does not compile under RE2: %v\n"+
			"pattern: %s\nA generated Go or Rust client would fail here.", err, p.Pattern)
	}

	// Every case the two could disagree on: dots, the character class, the boundaries.
	for _, name := range []string{
		"a", "abc", "a-b", "a_b", "a.b", "a.b.c", "web", "riggs", "9", "-", "_",
		"a.", "a.b.", ".a", "..", ".", "", "a..b", "a...b", "a/b", "a b", "a$b",
		"héllo", "a\tb", "..a", "a..", strings.Repeat("n", maxSessionNameLen),
		strings.Repeat("n", maxSessionNameLen+1),
	} {
		serverOK := validateSessionName(name) == nil
		specOK := re.MatchString(name) &&
			len(name) >= p.MinLength && len(name) <= p.MaxLength
		if serverOK != specOK {
			t.Errorf("name %q: server accepts=%v, published schema accepts=%v — a client "+
				"validating against the spec would disagree with the server", name, serverOK, specOK)
		}
	}
}

// GET /api/v1/sessions/{name} must return exactly what the list returns for that name, because
// the 201 Location header points at it and a client refreshing one session should not have to
// list every session and filter.
func TestSessionReadMatchesTheListing(t *testing.T) {
	// newTestServer owns WT_DIR, so take the directory from it rather than setting one.
	srv, dir := newTestServer(t)
	mkSocket(t, dir, "alpha.sock", 0o600)
	mkSocket(t, dir, "with space.sock", 0o600)
	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	var listed []Session
	if err := json.Unmarshal(get("/api/v1/sessions").Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(listed))
	}

	for _, want := range listed {
		rec := get("/api/v1/sessions/" + url.PathEscape(want.Name))
		if rec.Code != http.StatusOK {
			t.Errorf("GET one %q: status = %d, want 200", want.Name, rec.Code)
			continue
		}
		var got Session
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Errorf("decode %q: %v", want.Name, err)
			continue
		}
		if got.Name != want.Name || got.Attached != want.Attached {
			t.Errorf("GET one %q returned %+v, want %+v", want.Name, got, want)
		}
	}

	// A name the create rules would reject is still readable — same asymmetry the list has.
	if rec := get("/api/v1/sessions/" + url.PathEscape("with space")); rec.Code != http.StatusOK {
		t.Errorf("a listed name POST would reject is not readable: status = %d", rec.Code)
	}
	if rec := get("/api/v1/sessions/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown name: status = %d, want 404", rec.Code)
	}
}

// /healthz's declared content type and body must match what the handler sends.
//
// They did not: the spec said text/plain with a body of exactly `ok`, while the handler has
// always returned application/json `{"status":"ok"}`. Nothing read the description and nothing
// compared it to the response, so a wrong endpoint description survived — the same shape of bug
// as Meta.terminalPath. A probe written from the spec would have compared the wrong string.
func TestHealthzMatchesItsSpec(t *testing.T) {
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Properties map[string]struct {
								Const string `json:"const"`
							} `json:"properties"`
						} `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}
	declared := spec.Paths["/healthz"].Get.Responses["200"].Content
	if len(declared) != 1 {
		t.Fatalf("the spec declares %d content types for /healthz 200, want exactly 1", len(declared))
	}
	var declaredType string
	for k := range declared {
		declaredType = k
	}

	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := mediaType(rec.Header().Get("Content-Type")); ct != declaredType {
		t.Errorf("Content-Type = %q but the spec declares %q", ct, declaredType)
	}
	// And the body satisfies the declared shape, including the const.
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q does not parse as the declared object: %v", rec.Body.String(), err)
	}
	for field, prop := range declared[declaredType].Schema.Properties {
		if prop.Const != "" && body[field] != prop.Const {
			t.Errorf("body[%q] = %q, spec requires const %q", field, body[field], prop.Const)
		}
	}
}

func TestDocsAreServed(t *testing.T) {
	srv, _ := newTestServer(t)

	for name := range docAssets {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /docs/%s: status = %d, want 200", name, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
			t.Errorf("GET /docs/%s: Content-Type = %q, want text/markdown", name, ct)
		}
		// Non-trivially long: an empty or truncated copy would serve a 200 and teach a client
		// nothing, which is the failure this whole endpoint exists to prevent.
		if n := rec.Body.Len(); n < 1000 {
			t.Errorf("GET /docs/%s: %d bytes, want a real document — is the copy stale?", name, n)
		}
	}

	// The allowlist is the boundary; anything else is a 404, not a path traversal.
	for _, bad := range []string{"openapi.json", "../openapi.json", "README.md"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/"+bad, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET /docs/%s returned 200; only the allowlist may be served", bad)
		}
	}
}

// The wire contract is the one document a client cannot do without, so assert the served copy
// carries the facts a client would be stuck without rather than merely being non-empty.
func TestServedWireProtocolCarriesTheEssentials(t *testing.T) {
	body, err := docsFS.ReadFile("docs/ws-protocol.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AuthToken", "columns", "RESIZE_TERMINAL", "SET_WINDOW_TITLE"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the served wire protocol does not mention %q", want)
		}
	}
}

func TestOpenAPIServedAsJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var spec struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("embedded spec is not valid JSON: %v", err)
	}
	if spec.OpenAPI == "" {
		t.Error("openapi version field is empty")
	}

	// Every route the spec documents must actually be routed. This is the drift check
	// that matters: a spec promising an endpoint that 404s is worse than no spec.
	for path := range spec.Paths {
		if strings.Contains(path, "{") {
			continue // templated paths need a concrete value; covered elsewhere
		}
		probe := httptest.NewRecorder()
		srv.routes().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, path, nil))
		if probe.Code == http.StatusNotFound {
			t.Errorf("spec documents %s but the server 404s it", path)
		}
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Code
}

// The features registry in api/openapi.yaml and the features slice in api.go describe the same
// server, and nothing used to check that they agreed. `session-status` shipped advertised and
// undocumented for two releases as a result (#114).
//
// The registry is the anti-skew mechanism between this repo and the iOS client, so a flag the
// server sends and the spec does not define is precisely the failure it exists to prevent: a client
// author reads the spec, does not find the flag, and concludes the capability does not exist.
//
// Both directions, because they fail differently. A flag advertised and undocumented leaves a
// client unable to learn what it means. A flag documented and not advertised is a promise this
// server does not keep -- except for the reserved names, which are deliberately documented as
// future work and must stay documented without being sent.
func TestEveryAdvertisedFeatureIsDocumented(t *testing.T) {
	var spec struct {
		Info struct {
			Description string `json:"description"`
		} `json:"info"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}
	// Scoped to the registry section rather than the whole description: the document contains
	// other markdown tables, including the error-code registry, whose rows look identical.
	desc := spec.Info.Description
	start := strings.Index(desc, "## Features registry")
	if start < 0 {
		t.Fatal("the embedded spec has no features registry; this test would assert nothing")
	}
	section := desc[start:]
	if end := strings.Index(section[len("## Features registry"):], "\n    ## "); end >= 0 {
		section = section[:len("## Features registry")+end]
	}

	// The registry is a markdown table of `| `name` | meaning |` rows.
	rows := regexp.MustCompile("\\| `([a-z0-9-]+)` \\|").FindAllStringSubmatch(section, -1)
	documented := make(map[string]bool, len(rows))
	for _, m := range rows {
		documented[m[1]] = true
	}
	if len(documented) == 0 {
		t.Fatal("no feature rows parsed out of the registry; this test would assert nothing")
	}

	for _, f := range features {
		if !documented[f] {
			t.Errorf("features[] advertises %q and the registry table does not define it; a client "+
				"author reading the spec cannot learn what it means", f)
		}
	}

	// Reserved names are documented on purpose and not advertised: a server sending one is claiming
	// to implement a spec revision that does not exist yet.
	reserved := map[string]bool{"auth-basic": true, "base-path": true, "tls": true, "flow-control": true}
	for f := range documented {
		if !reserved[f] && !slices.Contains(features, f) {
			t.Errorf("the registry documents %q and this server does not advertise it; either it "+
				"is reserved and belongs on the reserved list, or the promise is not kept", f)
		}
	}
}

// Every Cache-Control the spec declares must be the one the server actually sends.
//
// It was not: the spec has declared `no-store` on the API and on /token since long before any
// of it was polled by a browser, and nothing ever sent it (#116). The lie survived because
// nothing compared the two -- the same shape of bug as Meta.terminalPath and /healthz's content
// type, and the reason this test is driven from the document rather than from a list of paths
// written by hand. A new endpoint that declares a cache policy is covered the moment it is
// documented.
//
// Note the assertion is on the header alone, not on a 200. A templated path is probed with a
// name that does not exist, and the 404 travels through writeError -> writeJSON, so the header
// is expected there too: the policy belongs to the writer, not to the happy path. That is what
// makes it a line of code instead of a per-handler habit the next handler forgets.
func TestDeclaredCacheControlIsSent(t *testing.T) {
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Headers map[string]struct {
						Schema struct {
							Const string `json:"const"`
						} `json:"schema"`
					} `json:"headers"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}

	// Concrete values for the templated paths, so they are checked rather than skipped.
	concrete := map[string]string{
		"/api/v1/sessions/{name}": "/api/v1/sessions/nonexistent",
		"/docs/{file}":            "/docs/ws-protocol.md",
	}

	srv, _ := newTestServer(t)
	checked := 0
	for path, item := range spec.Paths {
		want := item.Get.Responses["200"].Headers["Cache-Control"].Schema.Const
		if want == "" {
			continue
		}
		target := path
		if c, ok := concrete[path]; ok {
			target = c
		}
		if strings.Contains(target, "{") {
			t.Errorf("%s declares Cache-Control but has no concrete probe; add one", path)
			continue
		}
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control = %q but the spec declares %q", target, got, want)
		}
		checked++
	}
	// Without this the test passes vacuously if the spec stops declaring the header at all,
	// which is one of the two ways #116 could have been "fixed".
	if checked < 4 {
		t.Errorf("only %d paths declare a Cache-Control const; the spec used to declare 5", checked)
	}
}
