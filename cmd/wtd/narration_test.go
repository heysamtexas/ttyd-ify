package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeNarration puts a summary where the server will look for it, the way bin/wt-narrate does.
func writeNarration(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A summary with no state directory configured is not an error condition to report; it is the
// feature being off. Distinguished as ErrNotExist so the handler answers 404 rather than 500,
// because "this deployment does not narrate" is a normal answer to a polling client.
func TestNarrationIsOffWithoutAStateDir(t *testing.T) {
	if got := narrationDir(""); got != "" {
		t.Errorf("narrationDir(%q) = %q, want empty", "", got)
	}
	if _, err := readNarration("", "demo"); !os.IsNotExist(err) {
		t.Errorf("readNarration with no dir returned %v, want a not-exist error so the handler "+
			"answers 404 rather than reporting a server fault", err)
	}
}

// The narration directory is a subdirectory rather than the state directory itself, because the
// ring store owns *.ring in that root and the two must not have to know about each other.
func TestNarrationLivesBesideTheRingStoreNotInIt(t *testing.T) {
	dir := narrationDir("/run/wt")
	if dir == "/run/wt" {
		t.Fatal("narration writes into the ring store's own directory; a name collision there is " +
			"a replay bug reported as a narration bug")
	}
	if want := filepath.Join("/run/wt", narrationSubdir); dir != want {
		t.Errorf("narrationDir = %q, want %q", dir, want)
	}
}

// This is the only route in the server that turns a session name into a filename. The name arrives
// from a client over the network, so the traversal rejection CLAUDE.md requires is checked here
// directly and not only through the handler, which additionally matches against the session list.
func TestReadNarrationRefusesAnUnusableName(t *testing.T) {
	dir := t.TempDir()
	writeNarration(t, dir, "demo", `{"at":"2026-08-21T16:40:12Z","headline":"hi"}`)

	for _, name := range []string{"", "../demo", "a/b", "..", "x/../demo"} {
		if _, err := readNarration(dir, name); err == nil {
			t.Errorf("readNarration(%q) was accepted; a session name is untrusted network input "+
				"and this call builds a path from it", name)
		}
	}
}

// Decoded and checked rather than relayed. A file that is half-written, truncated mid-flush, or
// missing a field that carries behaviour must fail here, where it can be reported, instead of
// reaching a client that then either repeats a summary forever or speaks an empty sentence.
func TestReadNarrationRejectsAFileItCannotUse(t *testing.T) {
	dir := t.TempDir()

	cases := []struct{ name, body, why string }{
		{"broken", `{"at":`, "truncated JSON is a half-written file, not a summary"},
		{"undated", `{"headline":"something happened"}`,
			"without a timestamp a client cannot tell a new summary from one it already spoke"},
		{"zerodated", `{"at":"0001-01-01T00:00:00Z","headline":"x"}`,
			"the zero time is not a usable deduplication key"},
		{"silent", `{"at":"2026-08-21T16:40:12Z","headline":"   "}`,
			"a blank headline means there is nothing to say, which is a 404 not a summary"},
	}
	for _, c := range cases {
		writeNarration(t, dir, c.name, c.body)
		if _, err := readNarration(dir, c.name); err == nil {
			t.Errorf("readNarration accepted %s: %s", c.name, c.why)
		}
	}

	// Bounded read: a file that is not a summary must not be loaded as if it were one.
	writeNarration(t, dir, "huge", `{"at":"2026-08-21T16:40:12Z","headline":"`+
		strings.Repeat("a", maxNarrationBytes+1)+`"}`)
	if _, err := readNarration(dir, "huge"); err == nil {
		t.Error("readNarration accepted a file past maxNarrationBytes")
	}
}

// The filename wins over the contents, because the filename is what the handler matched against the
// real session list. A summary that names a different session would otherwise let a client attribute
// one session's report to another.
func TestReadNarrationTrustsTheFilenameOverTheBody(t *testing.T) {
	dir := t.TempDir()
	writeNarration(t, dir, "ops", `{"session":"something-else","at":"2026-08-21T16:40:12Z",`+
		`"headline":"The ops session needs you.","needsYou":true}`)

	n, err := readNarration(dir, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if n.Session != "ops" {
		t.Errorf("session = %q, want %q; the body must not be able to rename itself", n.Session, "ops")
	}
	if !n.NeedsYou {
		t.Error("needsYou was dropped; it is the only field that authorizes speaking unprompted")
	}
}

// 404 covers three situations and none of them is a fault: no session, no narration for a real
// session, and narration not configured. A polling client treats all three as silence, so none may
// arrive as a 500.
func TestNarrationEndpointAnswersSilenceWithNotFound(t *testing.T) {
	srv, dir := newTestServer(t)
	srv.narrationDir = filepath.Join(t.TempDir(), narrationSubdir)
	mkSocket(t, dir, "quiet"+socketSuffix, 0o600)

	for _, path := range []string{
		"/api/v1/sessions/nosuch/narration", // no such session
		"/api/v1/sessions/quiet/narration",  // real session, nothing said yet
	} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: a client polls this and must read silence as "+
				"silence, not as a server fault", path, rec.Code)
		}
	}

	// Narration switched off entirely is the same answer.
	srv.narrationDir = ""
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/quiet/narration", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("with narration off, GET = %d, want 404", rec.Code)
	}
}

// A name that no session has is a 404 before any path is built from it -- the same rule
// handleSessionGet and deleteSession follow. Asserted with a traversal that would otherwise reach a
// real file, so the test fails if the list match is ever removed as redundant.
func TestNarrationEndpointMatchesTheSessionListFirst(t *testing.T) {
	srv, dir := newTestServer(t)
	state := t.TempDir()
	srv.narrationDir = filepath.Join(state, narrationSubdir)
	mkSocket(t, dir, "ops"+socketSuffix, 0o600)

	good := `{"at":"2026-08-21T16:40:12Z","headline":"The ops session needs you."}`
	writeNarration(t, srv.narrationDir, "ops", good)
	// A file one level up, reachable only by a handler that builds a path from the request.
	writeNarration(t, state, "ops", good)

	for _, raw := range []string{
		"/api/v1/sessions/" + "%2e%2e%2fops" + "/narration",
		"/api/v1/sessions/" + "%2e%2e" + "/narration",
	} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, raw, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s returned 200; the name was not matched against the session list "+
				"before a path was built from it", raw)
		}
	}
}

// The happy path, and the shape a client is entitled to. Checked field by field because every one of
// them carries behaviour on the client: `at` decides whether to speak at all, `headline` is what
// gets said, and `needsYou` decides whether to interrupt someone who is driving.
func TestNarrationEndpointServesTheSummary(t *testing.T) {
	srv, dir := newTestServer(t)
	srv.narrationDir = filepath.Join(t.TempDir(), narrationSubdir)
	mkSocket(t, dir, "ops"+socketSuffix, 0o600)
	writeNarration(t, srv.narrationDir, "ops", `{"event":"attention","at":"2026-08-21T16:40:12Z",`+
		`"headline":"The ops session needs you.",`+
		`"detail":"It wants to run terraform apply. Say yes or no.","needsYou":true}`)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ops/narration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got Narration
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := Narration{
		Session: "ops", Event: "attention",
		At:       time.Date(2026, 8, 21, 16, 40, 12, 0, time.UTC),
		Headline: "The ops session needs you.",
		Detail:   "It wants to run terraform apply. Say yes or no.",
		NeedsYou: true,
	}
	if !got.At.Equal(want.At) {
		t.Errorf("at = %v, want %v; this is the client's deduplication key", got.At, want.At)
	}
	got.At, want.At = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("narration = %+v, want %+v", got, want)
	}
}

// A file our own hook wrote badly is a server fault, not a client one, and must not be reported as
// "nothing to say" -- that would make a broken narrator indistinguishable from a quiet session and
// leave nothing in the logs to find.
func TestNarrationEndpointReportsABrokenFile(t *testing.T) {
	srv, dir := newTestServer(t)
	srv.narrationDir = filepath.Join(t.TempDir(), narrationSubdir)
	mkSocket(t, dir, "ops"+socketSuffix, 0o600)
	writeNarration(t, srv.narrationDir, "ops", `{"at": not json`)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ops/narration", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; a malformed summary is a bug in bin/wt-narrate and must "+
			"be distinguishable from a session with nothing to say", rec.Code)
	}
}

// A deleted session takes its summary with it. Names get reused, and a summary that outlived its
// session would be spoken once against a new session that never said it.
func TestDeletingASessionDropsItsNarration(t *testing.T) {
	srv, dir := newTestServer(t)
	srv.narrationDir = filepath.Join(t.TempDir(), narrationSubdir)
	writeNarration(t, srv.narrationDir, "ops", `{"at":"2026-08-21T16:40:12Z","headline":"done"}`)

	// A stale socket, not a listening one: deleteSession refuses to unlink a socket that might
	// still have a session behind it, so a listening socket with no shell is a 500 rather than a
	// delete. Bound then closed without unlinking is the shape a dead dtach master leaves.
	sl, err := net.Listen("unix", filepath.Join(dir, "ops"+socketSuffix))
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := sl.Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/ops", nil)
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(srv.narrationDir, "ops.json")); !os.IsNotExist(err) {
		t.Error("the summary survived its session; a session recreated under this name would " +
			"serve its predecessor's last words")
	}
}

// dropNarration unlinks, so the traversal rejection is not optional here either.
func TestDropNarrationRefusesAnUnusableName(t *testing.T) {
	dir := t.TempDir()
	writeNarration(t, dir, "keepme", `{"at":"2026-08-21T16:40:12Z","headline":"x"}`)
	outside := filepath.Join(dir, "outside.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"", "../outside", "a/b", ".."} {
		dropNarration(dir, name)
	}
	for _, must := range []string{filepath.Join(dir, "keepme.json"), outside} {
		if _, err := os.Stat(must); err != nil {
			t.Errorf("%s was removed by an unusable name: %v", must, err)
		}
	}
}

// The voice control must be invisible against a server that cannot narrate. This page is served by
// the binary it ships with, but a client can be pointed at any wtd, and a button that silently
// cannot work is worse than no button -- it reads as a broken feature rather than an absent one.
// Same rule the picker follows for session-status (#108).
func TestTerminalPageGatesVoiceOnTheFeature(t *testing.T) {
	src, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(src)

	for want, why := range map[string]string{
		`includes("session-narration")`: "the page does not check the feature flag, so it would " +
			"offer a voice control against a server with no narration endpoint",
		`hidden`: "the voice button is not hidden by default, so it is visible for the moment " +
			"before the capability check answers -- and forever if the check fails",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("web/terminal.html has no %q -- %s", want, why)
		}
	}

	// The dedupe is the difference between a summary and a loop. A client that speaks on every
	// poll says the same two sentences every three seconds at someone who is driving.
	if !strings.Contains(page, "spokenAt") {
		t.Error("web/terminal.html does not track what it has already spoken; polling would " +
			"re-speak the same summary on every tick")
	}
}
