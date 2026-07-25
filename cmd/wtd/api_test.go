package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	return newServer("/usr/local/bin/wt"), dir
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
// bin/wt's deep-link path drops names containing ".." (making them unreachable).
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
