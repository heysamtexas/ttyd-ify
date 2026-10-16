package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

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
	if meta := getMeta(t, h); meta["filesPath"] != nil {
		t.Errorf("meta advertises filesPath = %v with no root configured", meta["filesPath"])
	}
}

func TestFilesIsMountedWhenConfigured(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd, hash, 0o600); err != nil {
		t.Fatal(err)
	}
	if srv.files, err = files.New(dir, passwd, filesBase); err != nil {
		t.Fatal(err)
	}
	h := srv.routes()

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/files", http.StatusMovedPermanently},
		{http.MethodGet, "/files/", http.StatusUnauthorized}, // gated, not open
		{http.MethodGet, "/files/login", http.StatusOK},      // the one ungated page
		{http.MethodPost, "/files/login", http.StatusUnauthorized},
		{http.MethodGet, "/", http.StatusOK}, // the picker is still the picker
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
	if got := getMeta(t, h)["filesPath"]; got != "/files/" {
		t.Errorf("meta filesPath = %v, want /files/", got)
	}
}

func getMeta(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return m
}
