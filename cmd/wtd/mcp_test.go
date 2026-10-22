package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		{"unknown method", `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`, rpcMethodNotFound},
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

// Every tool is read-only and says so; adding a writing tool must be a deliberate edit here.
func TestMCPToolsAreTheReadOnlySet(t *testing.T) {
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
	want := map[string]bool{"list_sessions": true, "read_session": true, "session_prompts": true,
		"host_status": true, "list_projects": true, "open_terminal": true}
	if len(res.Tools) != len(want) {
		t.Errorf("got %d tools, want %d", len(res.Tools), len(want))
	}
	handlers := srv.mcpHandlers()
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		if _, ok := handlers[tool.Name]; !ok {
			t.Errorf("tool %q is listed but has no handler", tool.Name)
		}
		if tool.Annotations["readOnlyHint"] != true {
			t.Errorf("tool %q is not marked read-only", tool.Name)
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
	if isErr || !strings.Contains(text, "no output is buffered") {
		t.Errorf("read_session with no hub = %q (isError %v), want the not-buffered explanation", text, isErr)
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
