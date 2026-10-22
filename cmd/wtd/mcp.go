package main

// The MCP endpoint (#162): a Model Context Protocol server, so an agent can see the sessions on
// this box — list them, read what is on their screens, read what the human asked — without
// driving the WebSocket itself.
//
// Served by wtd on the same mux as everything else, which is the whole security argument: it sits
// behind WT_PASSWORD exactly like /ws does, so it reaches nobody the shell does not already reach.
// A separate MCP server would be a second listener with a second auth story.
//
// Hand-rolled rather than built on an SDK, for the reason the rest of this binary is stdlib plus
// two libraries: the official Go SDK needs a newer toolchain than go.mod pins, and the subset used
// here is small. It is the Streamable HTTP transport in its simplest legal form — stateless, no
// session IDs, every response a single JSON body, no SSE stream (GET answers 405, which the
// transport spec defines as "this server offers none").
//
// Read-only on purpose. Writing to a session is shell access, and the first cut does not offer it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const mcpPath = "/mcp"

// mcpProtocolVersions are the revisions this endpoint answers to, newest first. Every one of them
// is identical for the subset implemented here (initialize, ping, tools/list, tools/call).
var mcpProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// mcpInstructions is what initialize hands the agent about this server as a whole.
const mcpInstructions = "wtd serves browser and phone terminals for one Linux box. Each session is " +
	"a long-lived shell kept alive by dtach, often running a coding agent. Use list_sessions to see " +
	"them, read_session to read one's recent screen as text, session_prompts for what a human asked " +
	"the agent in it, and open_terminal to give the user a link to it. Nothing here writes to a session."

// JSON-RPC 2.0 error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// handleMCPGet answers the transport's optional server-to-client stream, which this server does
// not offer. 405 rather than 404 is what the transport spec asks for, and an explicit route is
// what keeps the request from falling through to the picker on GET /.
func (s *server) handleMCPGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodPost)
	writeError(w, http.StatusMethodNotAllowed, codeBadRequest,
		"this MCP endpoint offers no event stream; POST JSON-RPC requests to it", "")
}

// handleMCP serves one JSON-RPC message. It is wrapped in guardMutating, so a cross-site Origin
// is refused before this runs — which is also the transport spec's DNS-rebinding requirement.
func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{rpcParseError, "cannot read body"}})
		return
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "request body too large", "")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// A JSON array is a batch, which the protocol dropped in 2025-06-18 and this server
		// never supported. It reaches here as an unmarshal error into a struct.
		e := &rpcError{rpcParseError, "parse error"}
		if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			e = &rpcError{rpcInvalidRequest, "batches are not supported"}
		}
		writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: e})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if len(req.ID) == 0 {
			// A response or a malformed notification from the client: nothing to answer.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, rpcResponse{ID: req.ID, Error: &rpcError{rpcInvalidRequest, "not a JSON-RPC 2.0 request"}})
		return
	}
	if len(req.ID) == 0 {
		// A notification (notifications/initialized, notifications/cancelled). Nothing here has
		// state for one to change, so each is accepted and ignored.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := s.mcpDispatch(r, req)
	writeRPC(w, rpcResponse{ID: req.ID, Result: result, Error: rpcErr})
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) mcpDispatch(r *http.Request, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		// A version this server does not know gets its newest; the client decides whether
		// it can live with that.
		proto := mcpProtocolVersions[0]
		for _, v := range mcpProtocolVersions {
			if v == p.ProtocolVersion {
				proto = v
			}
		}
		return map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "wtd", "version": version},
			"instructions":    mcpInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{rpcInvalidParams, "tools/call needs a tool name"}
		}
		tool, ok := s.mcpHandlers()[p.Name]
		if !ok {
			return nil, &rpcError{rpcInvalidParams, "unknown tool: " + p.Name}
		}
		var args toolArgs
		if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
			if err := json.Unmarshal(p.Arguments, &args); err != nil {
				return nil, &rpcError{rpcInvalidParams, "arguments: " + err.Error()}
			}
		}
		text, err := tool(r, args)
		if err != nil {
			// A tool that ran and failed is a result the model can read, not a protocol error.
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	}
	return nil, &rpcError{rpcMethodNotFound, "method not found: " + req.Method}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// toolArgs is the union of every tool's arguments; each tool reads the ones it declares.
type toolArgs struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

func nameSchema(extra map[string]any) map[string]any {
	props := map[string]any{"name": map[string]any{"type": "string", "description": "The session name, exactly as list_sessions reports it."}}
	for k, v := range extra {
		props[k] = v
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{"name"}}
}

var noArgs = map[string]any{"type": "object", "properties": map[string]any{}}

// readOnly is every tool's annotation: none of them changes anything on the box.
var readOnly = map[string]any{"readOnlyHint": true, "openWorldHint": false}

const (
	defaultReadLines = 60
	maxReadLines     = 1000
)

var mcpTools = []mcpTool{
	{
		Name: "list_sessions", Title: "List sessions",
		Description: "Every terminal session on this box: name, working directory, whether a client " +
			"is attached, and the status the agent in it last reported, if any.",
		InputSchema: noArgs, Annotations: readOnly,
	},
	{
		Name: "read_session", Title: "Read a session's screen",
		Description: "The session's recent output rendered as plain text, newest line last. Available " +
			"only for a session a client has opened since the server last started; for any other, " +
			"this says so rather than guessing. The rendering is approximate: colours are dropped " +
			"and wide characters may shift columns.",
		InputSchema: nameSchema(map[string]any{"lines": map[string]any{
			"type": "integer", "minimum": 1, "maximum": maxReadLines,
			"description": fmt.Sprintf("How many trailing lines to return (default %d).", defaultReadLines),
		}}),
		Annotations: readOnly,
	},
	{
		Name: "session_prompts", Title: "What the human asked",
		Description: "The messages a human has sent to the coding agent in this session, oldest first. " +
			"Empty when nothing has been recorded, which includes a box with no prompt hook installed.",
		InputSchema: nameSchema(nil), Annotations: readOnly,
	},
	{
		Name: "host_status", Title: "Host health",
		Description: "Load, memory, memory pressure and disk for this box, and the memory each session's " +
			"process tree uses. A null field means the server could not find out, not zero.",
		InputSchema: noArgs, Annotations: readOnly,
	},
	{
		Name: "list_projects", Title: "List projects",
		Description: "The project shortcuts configured on this box: a name, the directory it points at, " +
			"and whether that directory exists.",
		InputSchema: noArgs, Annotations: readOnly,
	},
	{
		Name: "open_terminal", Title: "Link to a session",
		Description: "A URL that opens the session in a browser terminal. Give it to the user; opening it " +
			"creates the session if it does not exist yet.",
		InputSchema: nameSchema(nil), Annotations: readOnly,
	},
}

func (s *server) mcpHandlers() map[string]func(*http.Request, toolArgs) (string, error) {
	return map[string]func(*http.Request, toolArgs) (string, error){
		"list_sessions":   s.toolListSessions,
		"read_session":    s.toolReadSession,
		"session_prompts": s.toolSessionPrompts,
		"host_status":     s.toolHostStatus,
		"list_projects":   s.toolListProjects,
		"open_terminal":   s.toolOpenTerminal,
	}
}

func jsonText(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	return string(b), err
}

var errNoSuchSession = errors.New("no such session")

// toolListSessions is GET /api/v1/sessions, reap included, so the two never disagree about what
// exists.
func (s *server) toolListSessions(_ *http.Request, _ toolArgs) (string, error) {
	if reaped := reapStale(s.sessionDir()); len(reaped) > 0 {
		logf("wtd: reaped stale sessions: %s", strings.Join(reaped, ", "))
	}
	sessions, err := listSessions(s.sessionDir(), s.hubs.stats())
	if err != nil {
		return "", fmt.Errorf("cannot read sessions: %w", err)
	}
	return jsonText(sessions)
}

// toolReadSession renders the ring of the hub holding the session.
//
// It never creates a hub for a session that has none, though that would make every session
// readable. A hub is a real dtach attachment, spawned at a window size: dtach redraws for and
// resizes to its newest client, so attaching would reflow the screen of whoever is watching that
// session on their phone, and kick a full-screen program into repainting — all for a read. A
// session nobody has opened since the server started says so instead.
func (s *server) toolReadSession(_ *http.Request, a toolArgs) (string, error) {
	if !s.sessionSocketExists(a.Name) {
		return "", fmt.Errorf("%w: %q", errNoSuchSession, a.Name)
	}
	lines := a.Lines
	if lines <= 0 {
		lines = defaultReadLines
	}
	lines = min(lines, maxReadLines)

	raw, rows, ok := s.hubs.screen(a.Name)
	if !ok {
		return fmt.Sprintf("Session %q is running, but no output is buffered for it: this server keeps "+
			"a session's recent output only once a client has opened it since the server started. "+
			"open_terminal gives a link that does.", a.Name), nil
	}
	text := lastLines(screenText(raw, rows), lines)
	if text == "" {
		return fmt.Sprintf("Session %q has produced no output since it was opened.", a.Name), nil
	}
	return text, nil
}

func (s *server) toolSessionPrompts(_ *http.Request, a toolArgs) (string, error) {
	if !s.sessionSocketExists(a.Name) {
		return "", fmt.Errorf("%w: %q", errNoSuchSession, a.Name)
	}
	prompts := s.readPrompts(a.Name)
	if prompts == nil {
		prompts = []prompt{}
	}
	return jsonText(promptFile{Session: a.Name, Prompts: prompts})
}

func (s *server) toolHostStatus(_ *http.Request, _ toolArgs) (string, error) {
	sessions, err := listSessions(s.sessionDir(), s.hubs.stats())
	return jsonText(newHostProbe().report(sessions, err))
}

func (s *server) toolListProjects(_ *http.Request, _ toolArgs) (string, error) {
	return jsonText(s.projectList())
}

// toolOpenTerminal builds the deep link from the address the agent used to reach this server,
// which is the one address known to work from where it is running.
func (s *server) toolOpenTerminal(r *http.Request, a toolArgs) (string, error) {
	if err := validateAttachName(s.sessionDir(), a.Name); err != nil {
		return "", fmt.Errorf("cannot open %q: %w", a.Name, err)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: r.Host, Path: "/", RawQuery: "arg=" + url.QueryEscape(a.Name)}
	return u.String(), nil
}
