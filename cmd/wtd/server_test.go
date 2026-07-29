package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// /token must match ttyd 1.7.4 byte for byte. Captured from the running instance:
//
//	$ curl -fsS http://<host>:7681/token | xxd
//	00000000: 7b22 746f 6b65 6e22 3a20 2222 7d  {"token": ""}
//
// Note the space after the colon and the absence of a trailing newline —
// encoding/json would produce neither. Both known clients parse JSON and would accept
// either form, so this is belt-and-braces against wire drift, not a functional need.
func TestTokenMatchesTtydByteForByte(t *testing.T) {
	const want = `{"token": ""}`

	rec := httptest.NewRecorder()
	newServer("").routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/token", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q (byte-exact match with ttyd)", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// No authentication exists, so a permissive CORS header would let any page the user
// visits enumerate and mutate their sessions at the tailnet address. The browser picker
// is same-origin and needs no CORS at all.
func TestNoCORSHeadersAnywhere(t *testing.T) {
	handler := newServer("").routes()

	for _, path := range []string{"/token", "/healthz"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "http://evil.example")
		handler.ServeHTTP(rec, req)

		for header := range rec.Header() {
			if len(header) >= 6 && (header[:6] == "Access" || header[:6] == "access") {
				t.Errorf("%s returned CORS header %q, want none", path, header)
			}
		}
	}
}
