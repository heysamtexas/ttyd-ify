package main

// The MCP Apps terminal view (#167): a live session rendered inside an agent's chat, in hosts that
// support MCP Apps (Claude, Claude Desktop, VS Code).
//
// show_terminal is an ordinary tool whose result the host renders with the ui://wtd/terminal
// resource — an HTML page with xterm.js inlined, running in the host's sandboxed iframe. The page
// has no network access (it declares no CSP domains), so it never connects to wtd: it polls
// terminal_output and sends keystrokes with terminal_input, both as tool calls the host proxies over
// the connection it already has to /mcp. That is what makes it work without TLS, without an Origin
// exception for the sandbox, and without a cookie — the three things a direct /ws connection from
// the iframe would each need.
//
// The two polling tools are visibility ["app"]: a host that implements MCP Apps keeps them out of
// the model's tool list. A host that does not will show them to its model, which is why
// terminal_input is annotated as honestly as send_input is.

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

const (
	viewURI      = "ui://wtd/terminal"
	viewMIMEType = "text/html;profile=mcp-app"
)

//go:embed web/mcp-terminal.html
var viewTemplate string

// viewHTML is the resource body, built once: the template with xterm.js inlined.
var viewHTML = sync.OnceValues(func() (string, error) {
	css, err := webFS.ReadFile("web/vendor/xterm.css")
	if err != nil {
		return "", err
	}
	js, err := webFS.ReadFile("web/vendor/xterm.js")
	if err != nil {
		return "", err
	}
	// An inline script ends at the first "</script" in it, whatever the JavaScript meant.
	if strings.Contains(strings.ToLower(string(js)), "</script") {
		return "", errors.New("xterm.js contains </script and cannot be inlined")
	}
	r := strings.NewReplacer("{{XTERM_CSS}}", string(css), "{{XTERM_JS}}", string(js))
	return r.Replace(viewTemplate), nil
})

var viewResource = map[string]any{
	"uri":         viewURI,
	"name":        "wtd terminal",
	"description": "A live terminal session from this box, rendered in the chat.",
	"mimeType":    viewMIMEType,
}

func (s *server) mcpResourcesRead(uri string) (any, *rpcError) {
	if uri != viewURI {
		return nil, &rpcError{rpcResourceNotFound, "no such resource: " + uri}
	}
	html, err := viewHTML()
	if err != nil {
		logf("wtd: mcp: build terminal view: %v", err)
		return nil, &rpcError{rpcInternalError, "terminal view unavailable"}
	}
	return map[string]any{"contents": []map[string]any{{
		"uri": viewURI, "mimeType": viewMIMEType, "text": html,
		// No csp: the view needs no network, and the host's restrictive default is what it wants.
		"_meta": map[string]any{"ui": map[string]any{"prefersBorder": true}},
	}}}, nil
}

// viewTools are appended to mcpTools.
var viewTools = []mcpTool{
	{
		Name: "show_terminal", Title: "Show a session's terminal",
		Description: "Shows the session as a live terminal in the chat, where the user can watch it and " +
			"type into it, in hosts that can display one. Elsewhere it returns the screen as text, " +
			"like read_session.",
		InputSchema: nameSchema(nil), Annotations: readOnly,
		Meta: map[string]any{"ui": map[string]any{"resourceUri": viewURI}},
	},
	{
		Name: "terminal_output", Title: "Terminal view: output",
		Description: "Used by the terminal view to poll a session's output. Returns the bytes written " +
			"after stream offset since, base64-encoded, and the offset to ask from next.",
		InputSchema: nameSchema(map[string]any{"since": map[string]any{"type": "integer",
			"description": "The offset the previous call returned; -1 for everything retained."}}),
		Annotations: readOnly,
		Meta:        map[string]any{"ui": map[string]any{"resourceUri": viewURI, "visibility": []string{"app"}}},
	},
	{
		Name: "terminal_input", Title: "Terminal view: keystrokes",
		Description: "Used by the terminal view to send the user's keystrokes to a session, verbatim.",
		InputSchema: nameSchema(map[string]any{"data": map[string]any{"type": "string", "maxLength": maxInputBytes,
			"description": "Bytes to type, exactly as the terminal produced them."}}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false},
		Meta:        map[string]any{"ui": map[string]any{"resourceUri": viewURI, "visibility": []string{"app"}}},
	},
}

// toolOut is a result with structured content, which the view reads and the model does not.
type toolOut struct {
	text       string
	structured map[string]any
}

func (s *server) mcpViewHandlers() map[string]func(*http.Request, toolArgs) (toolOut, error) {
	return map[string]func(*http.Request, toolArgs) (toolOut, error){
		"show_terminal":   s.toolShowTerminal,
		"terminal_output": s.toolTerminalOutput,
		"terminal_input":  s.toolTerminalInput,
	}
}

// showTerminalLines is how much of the screen show_terminal's text carries. The text is what the
// model reads and what a host prints in the tool call's card, under the live view; a whole screen
// there is a second, worse copy of what the user is already looking at. A few lines give the model
// context, and a host without MCP Apps can still call read_session for more.
const showTerminalLines = 6

// toolShowTerminal hands the view the session name and a link, and the model a short summary.
func (s *server) toolShowTerminal(r *http.Request, a toolArgs) (toolOut, error) {
	a.Lines = showTerminalLines
	screen, err := s.toolReadSession(r, a)
	if err != nil {
		return toolOut{}, err
	}
	text := fmt.Sprintf("Showing session %q as a live terminal in the chat, where the user can watch "+
		"and type into it. Its last lines:\n\n%s", a.Name, screen)
	link, err := s.toolOpenTerminal(r, a)
	if err != nil {
		return toolOut{}, err
	}
	return toolOut{text: text, structured: map[string]any{"name": a.Name, "url": link}}, nil
}

func (s *server) toolTerminalOutput(_ *http.Request, a toolArgs) (toolOut, error) {
	if !s.sessionSocketExists(a.Name) {
		return toolOut{}, fmt.Errorf("%w: %q", errNoSuchSession, a.Name)
	}
	data, next, reset, cols, rows, ok := s.hubs.output(a.Name, a.Since)
	if !ok && s.openForReading(a.Name) {
		data, next, reset, cols, rows, ok = s.hubs.output(a.Name, a.Since)
	}
	if !ok {
		return toolOut{}, fmt.Errorf("session %q cannot be opened: its terminal size could not be determined", a.Name)
	}
	return toolOut{
		text: fmt.Sprintf("%d bytes from %q", len(data), a.Name),
		structured: map[string]any{
			"data": base64.StdEncoding.EncodeToString(data), "offset": next, "reset": reset,
			"cols": cols, "rows": rows,
		},
	}, nil
}

func (s *server) toolTerminalInput(_ *http.Request, a toolArgs) (toolOut, error) {
	if !s.sessionSocketExists(a.Name) {
		return toolOut{}, fmt.Errorf("%w: %q", errNoSuchSession, a.Name)
	}
	if a.Data == "" {
		return toolOut{}, errors.New("nothing to send")
	}
	if len(a.Data) > maxInputBytes {
		return toolOut{}, fmt.Errorf("data is %d bytes; at most %d at once", len(a.Data), maxInputBytes)
	}
	// Through the hub the view is already polling, which writes to its pty directly: one write,
	// no process per keystroke. A view always has a hub, but the warm cap can evict one between
	// polls; dtach -p is the fallback, and it attaches nothing either.
	held, err := s.hubs.inputTo(a.Name, []byte(a.Data))
	if !held {
		err = pushInput(filepath.Join(s.sessionDir(), a.Name+socketSuffix), a.Data)
	}
	if err != nil {
		return toolOut{}, err
	}
	return toolOut{text: "sent", structured: map[string]any{"sent": len(a.Data)}}, nil
}
