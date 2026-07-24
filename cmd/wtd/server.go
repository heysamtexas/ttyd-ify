package main

import (
	"encoding/json"
	"net/http"
)

// server holds what every handler needs. Kept deliberately small: session state is not
// cached anywhere, because the filesystem is already the source of truth (dtach sockets
// in WT_DIR) and a cache would be one more thing to go stale against the bash picker.
type server struct {
	startCommand string
	// allowCrossOrigin disables WebSocket Origin checking. Off by default; see handleWS.
	allowCrossOrigin bool
}

func newServer(startCommand string) *server {
	return &server{startCommand: startCommand}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /token", s.handleToken)
	mux.HandleFunc("GET /ws", s.handleWS)
	s.apiRoutes(mux)
	return mux
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// tokenBody is ttyd 1.7.4's /token response, byte for byte: a space after the colon and
// no trailing newline. Semantically any equivalent JSON would do, but matching exactly
// costs nothing and lets the conformance test assert byte equality against real ttyd
// rather than "parses to the same thing", which is a weaker check that hides drift.
var tokenBody = []byte(`{"token": ""}`)

// handleToken exists purely for ttyd compatibility. ttyd serves a token only when
// started with -c (basic auth); with no auth configured it returns an empty one. Both
// known clients GET this on every connect — the iOS client ignores failures entirely and
// sends AuthToken:"" regardless — so the safe behavior is to always answer, always empty.
//
// Returning 404 here would be harmless for the iOS client but would break ttyd's own
// web client, which is also a supported browser path.
func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(tokenBody)
}

// writeError emits the error envelope from api/openapi.yaml: {"error":{code,message,detail}}.
// Clients switch on code, never on message, so codes come from the registry constants and
// must not be invented at the call site.
func writeError(w http.ResponseWriter, status int, code, message, detail string) {
	body := map[string]string{"code": code, "message": message}
	if detail != "" {
		body["detail"] = detail
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// No Access-Control-Allow-Origin, ever. There is no authentication, so a permissive
	// CORS header would let any page the user visits enumerate and mutate their sessions
	// at the tailnet address. The browser picker is served from this same origin and so
	// needs no CORS at all.
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
