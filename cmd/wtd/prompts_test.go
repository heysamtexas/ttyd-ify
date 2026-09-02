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
)

// promptFixture gives a server with a state directory and a live session socket, plus a writer
// for the file the hook would have produced. Nothing here points at ~/.dtach.
func promptFixture(t *testing.T) (*server, string, func(name, body string)) {
	t.Helper()
	srv, sessionDir := newTestServer(t)
	state := t.TempDir()
	srv.stateDir = state
	if err := os.MkdirAll(filepath.Join(state, promptsSubdir), 0o700); err != nil {
		t.Fatalf("mkdir prompts: %v", err)
	}
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(state, promptsSubdir, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return srv, sessionDir, write
}

// liveSession creates a real listening socket, which is what makes a session exist as far as this
// route is concerned.
func liveSession(t *testing.T, dir, name string) {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, name+socketSuffix))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

func getPrompts(t *testing.T, srv *server, name string) (int, promptFile) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+name+"/prompts", nil))
	var out promptFile
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, rec.Body)
		}
	}
	return rec.Code, out
}

func TestSessionPromptsServesWhatTheHookWrote(t *testing.T) {
	srv, sessionDir, write := promptFixture(t)
	liveSession(t, sessionDir, "ops")
	write("ops", `{"session":"ops","prompts":[
		{"at":"2026-09-02T16:31:02Z","text":"add a health panel","truncated":false},
		{"at":"2026-09-02T16:34:30Z","text":"now write the tests","truncated":true}]}`)

	code, got := getPrompts(t, srv, "ops")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Session != "ops" {
		t.Errorf("session = %q, want ops", got.Session)
	}
	if len(got.Prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(got.Prompts))
	}
	// Oldest first: they are shown in the order they were said, and reversing them would put a
	// client's "what did I just say" at the wrong end of the list.
	if got.Prompts[0].Text != "add a health panel" {
		t.Errorf("prompts[0] = %q, want the oldest", got.Prompts[0].Text)
	}
	if !got.Prompts[1].Truncated {
		t.Error("truncated did not survive the round trip; a client would present a cut-off " +
			"prompt as the whole of it")
	}
}

// A session with no prompt file is 200 and an empty array, never 404 and never null.
//
// This is the state of every session on a deployment where nobody installed the hook, so it has
// to be an ordinary answer rather than an error. Empty *array* specifically: null would mean
// "unknown", and this route genuinely cannot distinguish "no hook" from "nothing said yet" —
// both are none, and the spec says so.
func TestSessionPromptsWithNoFileIsAnEmptyArray(t *testing.T) {
	srv, sessionDir, _ := promptFixture(t)
	liveSession(t, sessionDir, "quiet")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/quiet/prompts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(raw["prompts"]); got != "[]" {
		t.Errorf("prompts = %s, want []", got)
	}
}

// No state directory at all is the same answer, for the same reason.
func TestSessionPromptsWithNoStateDirIsAnEmptyArray(t *testing.T) {
	srv, sessionDir := newTestServer(t)
	liveSession(t, sessionDir, "ops")
	code, got := getPrompts(t, srv, "ops")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Prompts) != 0 {
		t.Errorf("got %d prompts with no state dir, want none", len(got.Prompts))
	}
}

func TestSessionPromptsUnknownSessionIs404(t *testing.T) {
	srv, _, write := promptFixture(t)
	// A prompt file with no session behind it must not conjure one into existence: the session
	// list is the source of truth for what exists, and this route only decorates it.
	write("ghost", `{"session":"ghost","prompts":[{"at":"2026-09-02T16:31:02Z","text":"x","truncated":false}]}`)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ghost/prompts", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec.Body.Bytes()); code != codeNotFound {
		t.Errorf("error code = %q, want %q", code, codeNotFound)
	}
}

// Over the wire, a name that could escape the prompts directory is refused.
//
// Worth being precise about what this proves and what it does not. These requests never reach the
// handler with a hostile name in the first place: ServeMux cleans the path and `{name}` matches a
// single segment, so `../escaped` and `a/b` are routed elsewhere or not at all. Removing the
// handler's own validation does not make this test fail. It is here as a statement about the
// *route* — no URL a client can compose reads a file outside the prompts directory — and
// TestReadPromptsRefusesUnusableNames covers the validation itself, which is what would matter to
// a future caller reaching readPrompts from somewhere other than this handler.
func TestSessionPromptsRefusesTraversal(t *testing.T) {
	srv, sessionDir, _ := promptFixture(t)
	liveSession(t, sessionDir, "ops")

	// Written outside the prompts directory, where a traversal would have to reach to find it.
	outside := filepath.Join(srv.stateDir, "escaped.json")
	if err := os.WriteFile(outside, []byte(`{"session":"x","prompts":[{"at":"2026-09-02T16:31:02Z","text":"secret","truncated":false}]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, name := range []string{"../escaped", "..%2Fescaped", "a/b", ".."} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/api/v1/sessions/"+name+"/prompts", nil))
			if rec.Code == http.StatusOK {
				t.Errorf("status = 200 for %q; a name that leaves the directory must not be served: %s",
					name, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Errorf("%q read a file outside the prompts directory", name)
			}
		})
	}
}

// An unparseable or foreign file reports no prompts rather than a 500. The most likely writer of
// a bad file is a half-finished write by something that is not the hook, and a client polling
// this cannot act on an error anyway.
func TestSessionPromptsIgnoresAnUnreadableFile(t *testing.T) {
	srv, sessionDir, write := promptFixture(t)
	liveSession(t, sessionDir, "broken")
	for _, body := range []string{`garbage{`, `[]`, `null`, `{"prompts":"not a list"}`} {
		write("broken", body)
		code, got := getPrompts(t, srv, "broken")
		if code != http.StatusOK {
			t.Errorf("body %q: status = %d, want 200", body, code)
		}
		if len(got.Prompts) != 0 {
			t.Errorf("body %q: got %d prompts, want none", body, len(got.Prompts))
		}
	}
}

// The read is bounded. A file larger than the cap must not be read into memory whole on a route
// built to be polled.
func TestSessionPromptsBoundsTheRead(t *testing.T) {
	srv, sessionDir, write := promptFixture(t)
	liveSession(t, sessionDir, "huge")
	// Valid JSON that is far past the bound: the decoder hits the limit mid-document and the
	// route reports none rather than allocating it.
	var b strings.Builder
	b.WriteString(`{"session":"huge","prompts":[`)
	for i := 0; i < 40000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"at":"2026-09-02T16:31:02Z","text":"padding padding padding","truncated":false}`)
	}
	b.WriteString(`]}`)
	if b.Len() <= maxPromptFileBytes {
		t.Fatalf("fixture is %d bytes, which does not exceed the %d-byte bound", b.Len(), maxPromptFileBytes)
	}
	write("huge", b.String())

	code, got := getPrompts(t, srv, "huge")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Prompts) != 0 {
		t.Errorf("got %d prompts from an oversize file, want none", len(got.Prompts))
	}
}

// Reading prompts must not unlink anything, the same rule GET /api/v1/host follows and for the
// same reason: this is built to be polled.
func TestSessionPromptsDoesNotReapStaleSockets(t *testing.T) {
	srv, sessionDir, _ := promptFixture(t)
	stale := filepath.Join(sessionDir, "abandoned"+socketSuffix)
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A stale socket still reads as an existing session here — the same answer the listing
	// would give before it reaps — so this is a 200 with no prompts, not a 404.
	code, got := getPrompts(t, srv, "abandoned")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Prompts) != 0 {
		t.Errorf("got %d prompts, want none", len(got.Prompts))
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the prompts route unlinked a stale socket: %v", err)
	}
}

// The handler's keys and the schema's must agree, both directions.
func TestSessionPromptsMatchesItsSchema(t *testing.T) {
	srv, sessionDir, write := promptFixture(t)
	liveSession(t, sessionDir, "ops")
	write("ops", `{"session":"ops","prompts":[{"at":"2026-09-02T16:31:02Z","text":"hello","truncated":true}]}`)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/ops/prompts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var doc any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}
	compareKeys := func(path, schemaName string, value any) {
		schema, _ := specDig(spec, "components", "schemas", schemaName).(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			t.Fatalf("the spec declares no properties for %s; this test would pass vacuously", schemaName)
		}
		obj, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s: handler sent %T, want an object", path, value)
		}
		for k := range props {
			if _, sent := obj[k]; !sent {
				t.Errorf("%s: the spec declares %q and the handler does not send it", path, k)
			}
		}
		for k := range obj {
			if _, declared := props[k]; !declared {
				t.Errorf("%s: the handler sends %q and the spec does not declare it", path, k)
			}
		}
	}
	compareKeys("SessionPrompts", "SessionPrompts", doc)
	rows, _ := specDig(doc, "prompts").([]any)
	if len(rows) != 1 {
		t.Fatalf("the fixture no longer produces exactly one prompt row")
	}
	compareKeys("Prompt", "Prompt", rows[0])
}

// readPrompts refuses a name it cannot safely turn into a filename, on its own.
//
// This is the layer the route test cannot exercise, because ServeMux never lets such a name reach
// the handler. It matters anyway: the check is what makes readPrompts safe to call from anywhere,
// and "the router happens to sanitise this for us" is a property of one caller rather than of this
// function. ringstore holds the same line for the same reason.
func TestReadPromptsRefusesUnusableNames(t *testing.T) {
	srv, _, _ := promptFixture(t)

	// Reachable only by escaping the prompts directory.
	outside := filepath.Join(srv.stateDir, "escaped.json")
	body := `{"session":"x","prompts":[{"at":"2026-09-02T16:31:02Z","text":"secret","truncated":false}]}`
	if err := os.WriteFile(outside, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, name := range []string{"../escaped", "..", "a/b", "", "/etc/passwd"} {
		if got := srv.readPrompts(name); got != nil {
			t.Errorf("readPrompts(%q) returned %d prompts; an unusable name must read nothing",
				name, len(got))
		}
	}

	// And the control: a usable name in the right directory does read, so the loop above is
	// rejecting names rather than failing to find anything at all.
	inside := filepath.Join(srv.stateDir, promptsSubdir, "escaped.json")
	if err := os.WriteFile(inside, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := srv.readPrompts("escaped"); len(got) != 1 {
		t.Errorf("readPrompts(\"escaped\") read %d prompts, want 1 — the rejections above prove "+
			"nothing if the reader cannot read a good file", len(got))
	}
}
