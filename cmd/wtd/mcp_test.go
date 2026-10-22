package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// mcpPost sends one JSON-RPC body to /mcp through the full mux, gate included when one is set.
func mcpPost(t *testing.T, h http.Handler, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, mcpPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func decodeRPC(t *testing.T, rec *httptest.ResponseRecorder) rpcReply {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var r rpcReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("not JSON-RPC: %v; body %s", err, rec.Body)
	}
	return r
}

// callTool runs tools/call and returns the result's text and isError.
func callTool(t *testing.T, h http.Handler, name string, args map[string]any) (string, bool) {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	r := decodeRPC(t, mcpPost(t, h, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":`+string(params)+`}`))
	if r.Error != nil {
		t.Fatalf("tools/call %s: protocol error %+v", name, r.Error)
	}
	var res struct {
		Content []struct {
			Type, Text string
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("tools/call %s: result is not one text block: %s", name, r.Result)
	}
	return res.Content[0].Text, res.IsError
}

// fakeSession makes dir/<name>.sock a live listening socket, which is all the existence check
// and the listing need to see a session. No dtach involved.
func fakeSession(t *testing.T, dir, name string) {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, name+socketSuffix))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

func TestMCPInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.routes()
	for _, tc := range []struct{ asked, want string }{
		{"2025-06-18", "2025-06-18"},
		{"2025-03-26", "2025-03-26"},
		{"1999-01-01", mcpProtocolVersions[0]},
	} {
		r := decodeRPC(t, mcpPost(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+tc.asked+`","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
		var res struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct{ Name string } `json:"serverInfo"`
		}
		if err := json.Unmarshal(r.Result, &res); err != nil {
			t.Fatal(err)
		}
		if res.ProtocolVersion != tc.want {
			t.Errorf("asked %s, got %s, want %s", tc.asked, res.ProtocolVersion, tc.want)
		}
		if res.Capabilities.Tools == nil || res.ServerInfo.Name != "wtd" {
			t.Errorf("initialize result lacks tools capability or serverInfo: %s", r.Result)
		}
		if string(r.ID) != "1" {
			t.Errorf("id = %s, want 1 echoed", r.ID)
		}
	}
}

func TestMCPEnvelope(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.routes()

	t.Run("notification is accepted with no body", func(t *testing.T) {
		rec := mcpPost(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
			t.Errorf("status %d body %q, want 202 and empty", rec.Code, rec.Body)
		}
	})
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"unknown method", `{"jsonrpc":"2.0","id":2,"method":"prompts/list"}`, rpcMethodNotFound},
		{"bad json", `{"jsonrpc":`, rpcParseError},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, rpcInvalidRequest},
		{"not 2.0", `{"jsonrpc":"1.0","id":3,"method":"ping"}`, rpcInvalidRequest},
		{"unknown tool", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"rm_rf"}}`, rpcInvalidParams},
		{"bad arguments", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"read_session","arguments":{"lines":"many"}}}`, rpcInvalidParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := decodeRPC(t, mcpPost(t, h, tc.body))
			if r.Error == nil || r.Error.Code != tc.code {
				t.Errorf("error = %+v, want code %d", r.Error, tc.code)
			}
		})
	}
	t.Run("ping", func(t *testing.T) {
		if r := decodeRPC(t, mcpPost(t, h, `{"jsonrpc":"2.0","id":"p","method":"ping"}`)); r.Error != nil || string(r.ID) != `"p"` {
			t.Errorf("ping = %+v", r)
		}
	})
}

// The tool set is pinned, and so is which tools say they change nothing: a client decides whether
// to ask its human before a call from readOnlyHint, so a write tool marked read-only would be
// called unprompted. Adding a tool must be a deliberate edit here.
func TestMCPToolSet(t *testing.T) {
	srv, _ := newTestServer(t)
	r := decodeRPC(t, mcpPost(t, srv.routes(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	var res struct {
		Tools []struct {
			Name        string
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	readOnly := map[string]bool{"list_sessions": true, "read_session": true, "session_prompts": true,
		"host_status": true, "list_projects": true, "open_terminal": true,
		"send_input": false, "create_session": false, "kill_session": false,
		"show_terminal": true, "terminal_output": true, "terminal_input": false}
	if len(res.Tools) != len(readOnly) {
		t.Errorf("got %d tools, want %d", len(res.Tools), len(readOnly))
	}
	handlers := srv.mcpHandlers()
	for _, tool := range res.Tools {
		want, known := readOnly[tool.Name]
		if !known {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		_, plain := handlers[tool.Name]
		_, structured := srv.mcpViewHandlers()[tool.Name]
		if plain == structured {
			t.Errorf("tool %q has %v handlers, want exactly one", tool.Name, map[bool]string{true: "two", false: "no"}[plain])
		}
		if got := tool.Annotations["readOnlyHint"] == true; got != want {
			t.Errorf("tool %q readOnlyHint = %v, want %v", tool.Name, got, want)
		}
		if !want && tool.Name != "create_session" && tool.Annotations["destructiveHint"] != true {
			t.Errorf("tool %q changes sessions and must be marked destructive", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("tool %q input schema is not an object schema", tool.Name)
		}
	}
}

func TestMCPSessionTools(t *testing.T) {
	srv, dir := newTestServer(t)
	h := srv.routes()
	fakeSession(t, dir, "demo")

	text, isErr := callTool(t, h, "list_sessions", nil)
	if isErr || !strings.Contains(text, `"demo"`) {
		t.Errorf("list_sessions = %q (isError %v), want it to list demo", text, isErr)
	}

	text, isErr = callTool(t, h, "read_session", map[string]any{"name": "demo"})
	// A listening socket with no dtach behind it has no shell whose tty can be asked for a size, so
	// the read must refuse to open it rather than attach at a guessed size.
	if isErr || !strings.Contains(text, "cannot be read") {
		t.Errorf("read_session with no size to learn = %q (isError %v), want the refusal", text, isErr)
	}
	if _, _, ok := srv.hubs.screen("demo"); ok {
		t.Error("read_session opened a hub for a session whose size it could not learn")
	}

	for _, name := range []string{"absent", "../etc/passwd", ""} {
		if text, isErr := callTool(t, h, "read_session", map[string]any{"name": name}); !isErr {
			t.Errorf("read_session(%q) = %q, want an error result", name, text)
		}
		if text, isErr := callTool(t, h, "session_prompts", map[string]any{"name": name}); !isErr {
			t.Errorf("session_prompts(%q) = %q, want an error result", name, text)
		}
	}

	text, isErr = callTool(t, h, "session_prompts", map[string]any{"name": "demo"})
	if isErr || !strings.Contains(text, `"prompts": []`) {
		t.Errorf("session_prompts = %q, want an empty list", text)
	}

	if text, isErr := callTool(t, h, "host_status", nil); isErr || !strings.Contains(text, "cpuCount") {
		t.Errorf("host_status = %q", text)
	}
	if text, isErr := callTool(t, h, "list_projects", nil); isErr || !strings.HasPrefix(text, "[") {
		t.Errorf("list_projects = %q", text)
	}
}

func TestMCPOpenTerminalLinksFromTheAddressUsed(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.routes()
	// httptest's requests carry Host example.com.
	text, isErr := callTool(t, h, "open_terminal", map[string]any{"name": "my work"})
	if isErr || text != "http://example.com/?arg=my+work" {
		t.Errorf("open_terminal = %q, want http://example.com/?arg=my+work", text)
	}
	if text, isErr := callTool(t, h, "open_terminal", map[string]any{"name": "a/b"}); !isErr {
		t.Errorf("open_terminal(a/b) = %q, want an error result", text)
	}
}

// The tool that makes this endpoint worth having: an agent reads what a client's session shows.
func TestMCPReadSessionRendersTheHeldScreen(t *testing.T) {
	stub := writeStub(t, "printf 'draft\\r\\n\\033[2K\\033[1A\\033[2K\\r\\033[1;32mREADY:%s\\033[0m\\r\\n' \"$1\"\nsleep 600\n")
	dir := t.TempDir()
	t.Setenv("WT_DIR", dir)
	app, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)
	fakeSession(t, dir, "demo")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := attach(ctx, t, base, "demo", 80, 25)
	readUntil(ctx, t, conn, "READY:", 10*time.Second)
	_ = conn.CloseNow() // the hub, and its ring, outlive the client

	text, isErr := callTool(t, app.routes(), "read_session", map[string]any{"name": "demo"})
	if isErr || text != "READY:demo" {
		t.Errorf("read_session = %q, want the rendered screen %q", text, "READY:demo")
	}
}

func TestMCPRouteIsGuarded(t *testing.T) {
	t.Run("cross-site origin", func(t *testing.T) {
		srv, _ := newTestServer(t)
		rec := mcpPost(t, srv.routes(), `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
			func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("GET offers no stream", func(t *testing.T) {
		srv, _ := newTestServer(t)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, mcpPath, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Errorf("GET %s = %d Allow %q, want 405 Allow POST", mcpPath, rec.Code, rec.Header().Get("Allow"))
		}
	})
	t.Run("password gate applies", func(t *testing.T) {
		_, _, h := gatedServer(t)
		ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
		if rec := mcpPost(t, h, ping); rec.Code != http.StatusUnauthorized {
			t.Errorf("no credentials: status = %d, want 401", rec.Code)
		}
		if rec := mcpPost(t, h, ping, bearer(testPassword)); rec.Code != http.StatusOK {
			t.Errorf("bearer: status = %d, want 200", rec.Code)
		}
	})
}

// The write tools against real dtach: create a session, type into it, read the result, end it.
//
// sizeLine matches the output of `echo SIZE=$(stty size)`, not the echo of the command itself.
var sizeLine = regexp.MustCompile(`(?m)^SIZE=\d`)

// The size assertion is the reason the MCP tools may open a hub at all. The session's terminal is
// set to an odd size behind the server's back, then send_input opens the session for the first
// time; if that attached at any size but the current one, `stty size` would report the server's.
// And the echo's output is visible at all only because send_input opened the session *before*
// typing — dtach keeps no buffer, so output from a session nothing holds is gone.
func TestMCPWriteToolsDriveARealSession(t *testing.T) {
	if _, err := exec.LookPath("dtach"); err != nil {
		t.Skip("dtach not installed")
	}
	app, dir, _, _ := builtinServer(t)
	h := app.routes()
	const name = "mcpw"

	if text, isErr := callTool(t, h, "create_session", map[string]any{"name": name, "path": dir}); isErr {
		t.Fatalf("create_session: %s", text)
	}
	t.Cleanup(func() { _ = deleteSession(dir, name) })
	if text, isErr := callTool(t, h, "create_session", map[string]any{"name": name}); !isErr {
		t.Errorf("creating %q twice = %q, want an error", name, text)
	}

	// Behind the server's back: a push straight to dtach, with no hub involved.
	if err := pushInput(filepath.Join(dir, name+socketSuffix), "stty cols 111 rows 37\r"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if cols, rows, ok := app.sessionSize(name); ok && cols == 111 && rows == 37 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never reported the size it set")
		}
	}
	if _, _, held := app.hubs.screen(name); held {
		t.Fatal("a hub already holds the session; the first-open path would go untested")
	}

	args := map[string]any{"name": name, "text": "echo SIZE=$(stty size)", "keys": []string{"enter"}}
	if out, isErr := callTool(t, h, "send_input", args); isErr {
		t.Fatalf("send_input: %s", out)
	}
	var text string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		text, _ = callTool(t, h, "read_session", map[string]any{"name": name, "lines": 200})
		if sizeLine.MatchString(text) {
			break
		}
	}
	if !strings.Contains(text, "SIZE=37 111") {
		t.Fatalf("want the output SIZE=37 111 (the size the session already had); read_session showed:\n%s", text)
	}

	if text, isErr := callTool(t, h, "send_input", map[string]any{"name": name, "text": "x", "keys": []string{"f13"}}); !isErr {
		t.Errorf("unknown key = %q, want an error", text)
	}
	if text, isErr := callTool(t, h, "send_input", map[string]any{"name": name}); !isErr {
		t.Errorf("empty send_input = %q, want an error", text)
	}

	if text, isErr := callTool(t, h, "kill_session", map[string]any{"name": name}); isErr {
		t.Fatalf("kill_session: %s", text)
	}
	if text, isErr := callTool(t, h, "kill_session", map[string]any{"name": name}); !isErr {
		t.Errorf("killing it again = %q, want an error", text)
	}
}

func TestMCPSendInputRefusesUnknownSessions(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, name := range []string{"absent", "../x", ""} {
		if text, isErr := callTool(t, srv.routes(), "send_input", map[string]any{"name": name, "text": "id"}); !isErr {
			t.Errorf("send_input(%q) = %q, want an error", name, text)
		}
	}
}

// The view is reachable the way the MCP Apps spec says a host finds it: a model-visible tool whose
// _meta names a ui:// resource, readable with the spec's MIME type, and two tools only the view may
// call. A host keys on these exact strings, so they are pinned.
func TestMCPTerminalViewIsDiscoverable(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.routes()

	r := decodeRPC(t, mcpPost(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	var list struct {
		Tools []struct {
			Name string
			Meta struct {
				UI struct {
					ResourceURI string   `json:"resourceUri"`
					Visibility  []string `json:"visibility"`
				} `json:"ui"`
			} `json:"_meta"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &list); err != nil {
		t.Fatal(err)
	}
	wantVis := map[string]string{"show_terminal": "", "terminal_output": "app", "terminal_input": "app"}
	for _, tool := range list.Tools {
		vis, isView := wantVis[tool.Name]
		if !isView {
			if tool.Meta.UI.ResourceURI != "" {
				t.Errorf("%s links a UI resource it should not", tool.Name)
			}
			continue
		}
		if tool.Meta.UI.ResourceURI != viewURI {
			t.Errorf("%s resourceUri = %q, want %q", tool.Name, tool.Meta.UI.ResourceURI, viewURI)
		}
		if got := strings.Join(tool.Meta.UI.Visibility, ","); got != vis {
			t.Errorf("%s visibility = %q, want %q", tool.Name, got, vis)
		}
	}

	r = decodeRPC(t, mcpPost(t, h, `{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"`+viewURI+`"}}`))
	var read struct {
		Contents []struct {
			URI, MimeType, Text string
		} `json:"contents"`
	}
	if err := json.Unmarshal(r.Result, &read); err != nil || len(read.Contents) != 1 {
		t.Fatalf("resources/read: %v %s", err, r.Result)
	}
	c := read.Contents[0]
	if c.MimeType != "text/html;profile=mcp-app" || c.URI != viewURI {
		t.Errorf("resource = %s %s", c.URI, c.MimeType)
	}
	if !strings.Contains(c.Text, "new Terminal(") || strings.Contains(c.Text, "{{XTERM") {
		t.Error("the view's HTML does not have xterm.js inlined")
	}
	// The host validates ui/initialize against the MCP Apps schema, which names the view appInfo;
	// MCP's own clientInfo there got the request rejected and the view never shown (#170).
	if !strings.Contains(c.Text, "appInfo:") || strings.Contains(c.Text, "clientInfo") {
		t.Error("the view's ui/initialize must send appInfo, not clientInfo")
	}
	// Exactly once: the resource is ~half a megabyte of xterm.js, and every host fetches it.
	js, _ := webFS.ReadFile("web/vendor/xterm.js")
	if n := strings.Count(c.Text, string(js[:256])); n != 1 {
		t.Errorf("xterm.js is inlined %d times, want 1", n)
	}
	if strings.Contains(c.Text, "<script src") || strings.Contains(c.Text, "<link ") {
		t.Error("the view loads an external file; the sandbox would block it")
	}

	r = decodeRPC(t, mcpPost(t, h, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"ui://wtd/other"}}`))
	if r.Error == nil || r.Error.Code != rpcResourceNotFound {
		t.Errorf("unknown resource: error = %+v", r.Error)
	}
}

// callView runs a structured tool and returns its structuredContent.
func callView(t *testing.T, h http.Handler, name string, args map[string]any) map[string]any {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	r := decodeRPC(t, mcpPost(t, h, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":`+string(params)+`}`))
	var res struct {
		IsError           bool `json:"isError"`
		Content           []struct{ Text string }
		StructuredContent map[string]any `json:"structuredContent"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || res.IsError {
		t.Fatalf("%s: %v %s", name, err, r.Result)
	}
	return res.StructuredContent
}

// The view's polling contract: the first call (since -1) resets and gets everything retained, a
// call with the returned offset gets exactly the bytes written since, and keystrokes sent with
// terminal_input reach the session.
func TestMCPTerminalViewStreamsOutputAndTakesInput(t *testing.T) {
	stub := writeStub(t, "printf 'BOOT\\n'\nstty -echo\nwhile IFS= read -r line; do printf 'GOT:%s\\n' \"$line\"; done\n")
	dir := t.TempDir()
	t.Setenv("WT_DIR", dir)
	app, base := hubTestServer(t, stub, defaultReplayBytes, defaultMaxWarmHubs)
	fakeSession(t, dir, "demo")
	h := app.routes()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := attach(ctx, t, base, "demo", 80, 25)
	readUntil(ctx, t, conn, "BOOT", 10*time.Second)
	_ = conn.CloseNow()

	decode := func(sc map[string]any) string {
		b, err := base64.StdEncoding.DecodeString(sc["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first := callView(t, h, "terminal_output", map[string]any{"name": "demo", "since": -1})
	if first["reset"] != true || !strings.Contains(decode(first), "BOOT") {
		t.Fatalf("first poll = %v, want a reset carrying BOOT", first)
	}
	offset := first["offset"]
	again := callView(t, h, "terminal_output", map[string]any{"name": "demo", "since": offset})
	if again["reset"] != false || decode(again) != "" {
		t.Fatalf("poll with no new output = %v, want nothing", again)
	}

	callView(t, h, "terminal_input", map[string]any{"name": "demo", "data": "hello\r"})
	var got string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		sc := callView(t, h, "terminal_output", map[string]any{"name": "demo", "since": offset})
		got += decode(sc)
		offset = sc["offset"]
		if strings.Contains(got, "GOT:hello") {
			break
		}
	}
	if !strings.Contains(got, "GOT:hello") || strings.Contains(got, "BOOT") {
		t.Fatalf("output after input = %q, want only the new GOT:hello", got)
	}
}
