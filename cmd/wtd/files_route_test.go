package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/heysamtexas/ttyd-ify/cmd/wtd/files"
)

// /files/ is opt-in (#144): a server with no root configured must not serve it, and must not
// say it does. The package's own tests cover what happens once it is on.
func TestFilesIsOffByDefault(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /files/ with no root = %d, want 404", rec.Code)
	}
	if meta := getMeta(t, h, nil); meta["filesPath"] != nil {
		t.Errorf("meta advertises filesPath = %v with no root configured", meta["filesPath"])
	}
}

// /files/ sits behind the server password like every other route — it has no gate of its own
// any more (#146), so this is the test that it is gated at all.
func TestFilesIsMountedBehindThePassword(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.gate = testGate(t)
	var err error
	if srv.files, err = files.New(t.TempDir(), filesBase); err != nil {
		t.Fatal(err)
	}
	h := srv.routes()

	cases := []struct {
		path  string
		token string
		want  int
	}{
		{"/files/", "", http.StatusUnauthorized},
		{"/files/", testPassword, http.StatusOK},
		{"/files", testPassword, http.StatusMovedPermanently},
		{"/files/assets/mermaid.min.js", "", http.StatusUnauthorized},
		{"/", testPassword, http.StatusOK}, // the picker is still the picker
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("GET %s (token %v) = %d, want %d", c.path, c.token != "", rec.Code, c.want)
		}
	}
	if got := getMeta(t, h, bearer(testPassword))["filesPath"]; got != "/files/" {
		t.Errorf("meta filesPath = %v, want /files/", got)
	}
}

// Every page a signed-in browser can land on links to /files/ when it is on: the picker, the
// terminal and the standalone help page. Each reads filesPath from /api/v1/meta, so a page
// that stops reading it silently loses the link on a box that serves files.
func TestPagesLinkToFiles(t *testing.T) {
	for _, page := range []string{"web/index.html", "web/terminal.html", "web/help.html"} {
		src, err := webFS.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		body := stripHTMLComments(string(src))
		for _, want := range []string{`href="files/"`, "m.filesPath"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s has no %q — its link to the /files/ browser is gone", page, want)
			}
		}
	}
}

func getMeta(t *testing.T, h http.Handler, mod func(*http.Request)) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return m
}
