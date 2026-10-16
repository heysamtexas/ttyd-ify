package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uploadServer is a server whose inbox is a scratch directory that does not exist yet, so every
// test also covers its creation.
func uploadServer(t *testing.T) (*server, string) {
	t.Helper()
	srv, _ := newTestServer(t)
	dir := filepath.Join(t.TempDir(), "inbox")
	srv.uploadDirFlag = dir
	return srv, dir
}

func upload(h http.Handler, name, body string, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads?name="+url.QueryEscape(name),
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeUpload(t *testing.T, rec *httptest.ResponseRecorder) (name, path string, n int64) {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", rec.Code, rec.Body)
	}
	var got struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Name, got.Path, got.Bytes
}

// dirNames lists dir, so a test can assert nothing — no temporary file included — was left.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestUploadWritesThePrivateFile(t *testing.T) {
	srv, dir := uploadServer(t)
	name, path, n := decodeUpload(t, upload(srv.routes(), "screen shot.png", "pixels"))

	if name != "screen shot.png" || path != filepath.Join(dir, name) || n != 6 {
		t.Fatalf("got name=%q path=%q bytes=%d", name, path, n)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "pixels" {
		t.Fatalf("file holds %q, %v", got, err)
	}
	for p, want := range map[string]os.FileMode{path: 0o600, dir: 0o700} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != want {
			t.Errorf("%s is %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("inbox holds %v, want only the upload", names)
	}
}

// The whole promise of the inbox: a second file with the same name lands beside the first.
func TestUploadNeverOverwrites(t *testing.T) {
	srv, dir := uploadServer(t)
	h := srv.routes()
	for i, want := range []string{"a.txt", "a-1.txt", "a-2.txt"} {
		if name, _, _ := decodeUpload(t, upload(h, "a.txt", strings.Repeat("x", i+1))); name != want {
			t.Fatalf("upload %d saved as %q, want %q", i, name, want)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "x" {
		t.Errorf("the first a.txt now holds %q", got)
	}
}

func TestSuffixedName(t *testing.T) {
	for in, want := range map[string]string{
		"shot.png": "shot-3.png", "notes": "notes-3", ".env": ".env-3", "a.tar.gz": "a.tar-3.gz",
	} {
		if got := suffixedName(in, 3); got != want {
			t.Errorf("suffixedName(%q, 3) = %q, want %q", in, got, want)
		}
	}
}

func TestUploadRefusesNamesThatEscapeTheInbox(t *testing.T) {
	srv, dir := uploadServer(t)
	h := srv.routes()
	for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a\x00b", "a\nb", "\x1b[2J",
		strings.Repeat("n", 256), "\xff"} {
		rec := upload(h, name, "x")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), codeInvalidFilename) {
			t.Errorf("name %q: status %d %s, want 400 %s", name, rec.Code, rec.Body, codeInvalidFilename)
		}
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("refused uploads left %v behind", names)
	}
}

// The CSRF rules, applied to this route: a form can send multipart and text/plain, so both are
// refused, and so is any foreign Origin.
func TestUploadCSRF(t *testing.T) {
	srv, dir := uploadServer(t)
	h := srv.routes()
	for _, tc := range []struct {
		name       string
		mod        func(*http.Request)
		wantStatus int
	}{
		{"cross-site origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, http.StatusForbidden},
		{"multipart", func(r *http.Request) { r.Header.Set("Content-Type", "multipart/form-data; boundary=x") }, http.StatusUnsupportedMediaType},
		{"text/plain", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		{"json", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, http.StatusUnsupportedMediaType},
	} {
		if rec := upload(h, "f", "x", tc.mod); rec.Code != tc.wantStatus {
			t.Errorf("%s: status %d, want %d: %s", tc.name, rec.Code, tc.wantStatus, rec.Body)
		}
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("refused uploads left %v behind", names)
	}
	// Same-origin is the terminal page itself, and must work.
	decodeUpload(t, upload(h, "f", "x", func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }))
}

func TestUploadOverTheCapLeavesNothing(t *testing.T) {
	srv, dir := uploadServer(t)
	srv.uploadLimit = 4
	rec := upload(srv.routes(), "big", "12345")
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), codePayloadTooLarge) {
		t.Fatalf("status %d %s, want 413", rec.Code, rec.Body)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("an oversized upload left %v behind", names)
	}
}

func TestUploadIsBehindThePassword(t *testing.T) {
	srv, _, h := gatedServer(t)
	dir := filepath.Join(t.TempDir(), "inbox")
	srv.uploadDirFlag = dir
	if rec := upload(h, "f", "x"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no password: status %d, want 401", rec.Code)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("an unauthenticated upload left %v", names)
	}
	decodeUpload(t, upload(h, "f", "x", bearer(testPassword)))
}

func TestMetaReportsUploadDir(t *testing.T) {
	srv, dir := uploadServer(t)
	rec := serve(srv.routes(), http.MethodGet, "/api/v1/meta")
	var meta struct {
		UploadDir string `json:"uploadDir"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil || meta.UploadDir != dir {
		t.Fatalf("uploadDir = %q (%v), want %q", meta.UploadDir, err, dir)
	}
}
