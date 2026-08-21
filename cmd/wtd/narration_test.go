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
	writeNarration(t, dir, "demo", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"hi"}`)

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
	writeNarration(t, dir, "ops", `{"session":"something-else","event":"attention","at":"2026-08-21T16:40:12Z",`+
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
// handleSessionGet and deleteSession follow.
//
// The traversal cases below do NOT pin the list match, and it took a review to notice: remove the
// list match and readNarration still rejects "../ops", so they stay green either way. They are here
// because traversal must fail, not as evidence of how. The case that actually pins it is
// TestNarrationEndpointNeedsALiveSession.
func TestNarrationEndpointMatchesTheSessionListFirst(t *testing.T) {
	srv, dir := newTestServer(t)
	state := t.TempDir()
	srv.narrationDir = filepath.Join(state, narrationSubdir)
	mkSocket(t, dir, "ops"+socketSuffix, 0o600)

	good := `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"The ops session needs you."}`
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
	writeNarration(t, srv.narrationDir, "ops", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"done"}`)

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
	writeNarration(t, dir, "keepme", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"x"}`)
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

// The case that pins the list match, and the only one that does: a summary whose session is gone.
// The name is perfectly well-formed, the file is right there, and it must still be a 404 -- so this
// fails the moment the list match is dropped as redundant, which the traversal cases would not.
//
// It is also the real-world shape, not a contrivance. A session's shell exits and its summary sits
// in tmpfs until the next startup sweep; serving it would answer for a session that no longer
// exists, and after a name is reused, for a session that never said it.
func TestNarrationEndpointNeedsALiveSession(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.narrationDir = filepath.Join(t.TempDir(), narrationSubdir)
	writeNarration(t, srv.narrationDir, "ghost", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"done"}`)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ghost/narration", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: a summary with no session behind it was served, so the "+
			"handler is reading the filesystem rather than matching the session list", rec.Code)
	}
}

// The startup sweep. RuntimeDirectoryPreserve=restart means a restart does NOT clear these files,
// so without this a summary describes a turn from before the restart and the browser client speaks
// it -- turning voice on deliberately asks for the current summary, which is what makes a stale one
// dangerous rather than merely untidy.
func TestSweepNarrationRemovesWhatNoSessionOwns(t *testing.T) {
	dir := t.TempDir()
	sessions := t.TempDir()
	narr := filepath.Join(dir, narrationSubdir)

	// A live session: bound and still listening, which is the only proof of life probeSocket takes.
	l, err := net.Listen("unix", filepath.Join(sessions, "live"+socketSuffix))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	writeNarration(t, narr, "live", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"kept"}`)
	writeNarration(t, narr, "dead", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"gone"}`)
	// A hook that died mid-write. bin/wt-narrate's mktemp template produces this shape.
	partial := filepath.Join(narr, "live"+narrationTmpInfix+"AbC123")
	if err := os.WriteFile(partial, []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not ours. The sweep must leave it: deleting blind in a shared directory is how bugs eat data.
	stranger := filepath.Join(narr, "notes.txt")
	if err := os.WriteFile(stranger, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	sweepNarration(narr, sessions)

	for path, want := range map[string]bool{
		filepath.Join(narr, "live"+narrationSuffix): true,
		filepath.Join(narr, "dead"+narrationSuffix): false,
		partial:  false,
		stranger: true,
	} {
		_, err := os.Stat(path)
		if exists := err == nil; exists != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(path), exists, want)
		}
	}
}

// Every field the schema marks required carries client behaviour, so serving a file missing one is
// serving a client something it cannot act on. `event` was declared required and unenforced.
func TestReadNarrationRequiresAnEvent(t *testing.T) {
	dir := t.TempDir()
	writeNarration(t, dir, "ops", `{"at":"2026-08-21T16:40:12Z","headline":"something happened"}`)
	if _, err := readNarration(dir, "ops"); err == nil {
		t.Error("a summary with no event was accepted; the schema declares it required and a " +
			"client switches on it")
	}
}

// writeAudio puts a WAV beside a summary, the way bin/wt-narrate does after the JSON lands.
func writeAudio(t *testing.T, dir, name string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+narrationAudioSuffix), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// hasAudio is derived from the filesystem, never from the file. The hook writes the JSON first and
// the audio after -- deliberately, so a text-to-speech failure cannot cost the text -- which means
// a summary claiming its own audio would be claiming something it was written too early to know.
func TestHasAudioComesFromTheFilesystemNotTheFile(t *testing.T) {
	dir := t.TempDir()
	// A summary that claims audio it does not have. This is the ordering the hook produces.
	writeNarration(t, dir, "ops", `{"event":"waiting","at":"2026-08-21T16:40:12Z",`+
		`"headline":"done","hasAudio":true}`)

	n, err := readNarration(dir, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if n.HasAudio {
		t.Error("hasAudio was taken from the file; a client would fetch audio that does not exist")
	}

	writeAudio(t, dir, "ops", []byte("RIFFxxxxWAVE"))
	if n, err = readNarration(dir, "ops"); err != nil {
		t.Fatal(err)
	}
	if !n.HasAudio {
		t.Error("audio is on disk and hasAudio is false; the client will synthesise instead of " +
			"playing it, which is the thing that stops working when a phone locks")
	}

	// An empty file is a write that failed, not audio. Playing it is silence a listener cannot
	// distinguish from the feature being off.
	writeAudio(t, dir, "ops", nil)
	if n, err = readNarration(dir, "ops"); err != nil {
		t.Fatal(err)
	}
	if n.HasAudio {
		t.Error("a zero-length file counted as audio")
	}
}

// The audio route: present, absent, and the range support a media element needs to seek.
func TestNarrationAudioEndpoint(t *testing.T) {
	srv, dir := newTestServer(t)
	srv.narrationDir = filepath.Join(t.TempDir(), narrationSubdir)
	mkSocket(t, dir, "ops"+socketSuffix, 0o600)
	writeNarration(t, srv.narrationDir, "ops", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"done"}`)

	// No audio yet: a 404, and not an error. A deployment with no voice configured is the common case.
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ops/narration/audio", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("with no audio, status = %d, want 404", rec.Code)
	}

	body := []byte("RIFF....WAVEfmt short pretend audio payload")
	writeAudio(t, srv.narrationDir, "ops", body)

	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ops/narration/audio", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav; a media element decides how to decode by this", ct)
	}
	// The bytes behind this path change every turn while the path does not, so a cached response is
	// an earlier turn's summary played as if it were this one.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}

	// Range support is not decoration: it is how an <audio> element seeks.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ops/narration/audio", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Errorf("ranged status = %d, want 206", rec.Code)
	}
	if rec.Body.String() != "RIFF" {
		t.Errorf("ranged body = %q, want %q", rec.Body.String(), "RIFF")
	}

	// Same session-list gate as the JSON: this route also turns a name into a path.
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ghost/narration/audio", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown session = %d, want 404", rec.Code)
	}
}

// Audio is part of a summary's lifecycle, so everything that removes a summary removes its audio.
// A WAV left behind would be spoken against a session that never said it -- worse than a stale
// JSON, because it is the thing the listener actually hears.
func TestAudioIsRemovedWithItsSummary(t *testing.T) {
	dir := t.TempDir()
	sessions := t.TempDir()
	narr := filepath.Join(dir, narrationSubdir)

	writeNarration(t, narr, "dead", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"x"}`)
	writeAudio(t, narr, "dead", []byte("RIFF"))
	partial := filepath.Join(narr, "dead"+narrationAudioSuffix+tmpInfix+"AbC123")
	if err := os.WriteFile(partial, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	sweepNarration(narr, sessions)
	for _, p := range []string{
		filepath.Join(narr, "dead"+narrationSuffix),
		filepath.Join(narr, "dead"+narrationAudioSuffix),
		partial,
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep", filepath.Base(p))
		}
	}

	// And the DELETE path.
	writeNarration(t, narr, "gone", `{"event":"waiting","at":"2026-08-21T16:40:12Z","headline":"x"}`)
	writeAudio(t, narr, "gone", []byte("RIFF"))
	dropNarration(narr, "gone")
	if _, err := os.Stat(filepath.Join(narr, "gone"+narrationAudioSuffix)); !os.IsNotExist(err) {
		t.Error("dropNarration left the audio behind")
	}
}
