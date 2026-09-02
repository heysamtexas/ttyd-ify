package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// server holds what every handler needs. Kept deliberately small: session *state* is not
// cached anywhere, because the filesystem is already the source of truth (dtach sockets
// in WT_DIR) and a cache would be one more thing to go stale against the bash picker.
//
// hubs is not a cache of that state. It is live machinery — held pty attachments and their
// replay buffers — and it is still dtach that owns whether a session exists. The one place
// the two meet is `attached`, which hubs now answers because a held attachment pins the
// socket bit that used to answer it. See listSessions.
type server struct {
	startCommand string
	// allowCrossOrigin disables WebSocket Origin checking. Off by default; see handleWS.
	allowCrossOrigin bool
	// hubs holds one shared attachment per deep-linked session. See hub.go.
	hubs *hubs
	// handshakeWait is how long a connection may go without sending its handshake. A field
	// only so tests can shorten it: waiting out the real value would add ten seconds of
	// nothing to every run. TestHandshakeLimitsMatchTheirSpec asserts this field arrives here
	// as handshakeTimeout and that handshakeTimeout is what the served spec publishes — both
	// halves, because pinning only the const would let this default drift away from it.
	// readHandshake also floors a non-positive value, so a server literal cannot ship a
	// deadline of zero.
	handshakeWait time.Duration

	// sessionDirFlag and projectsFileFlag are the operator's settings arriving as arguments
	// rather than through the environment, which is what closes #28. WT_DIR in
	// /etc/ttyd-ify/config used to reach nothing at all: the launcher sources the config, which
	// creates shell variables, and every consumer read the *environment* — so the key looked
	// live, was documented, and was silently inert. A flag cannot fail that way, because a value
	// that does not arrive is a value the launcher did not pass.
	//
	// Empty means "not set", and the environment is then consulted as before. See sessionDir.
	sessionDirFlag   string
	projectsFileFlag string

	// stateDir is -state-dir, the directory systemd hands over as $RUNTIME_DIRECTORY. Held
	// separately from the ring store that also uses it, because the two have different
	// preconditions: a saved ring is only meaningful with replay on and no external start
	// command, while a prompt file is written by something else entirely and is readable
	// regardless. Empty means the operator configured none, and every consumer treats that as
	// "this feature is off" rather than as an error.
	stateDir string
}

func newServer(startCommand string) *server {
	s := &server{
		startCommand:  startCommand,
		handshakeWait: handshakeTimeout,
	}
	// The hub builds its commands through the server, so a named connection resolves WT_DIR and
	// the projects file the same way the private path and the JSON API do. Passing the method
	// value rather than the string is what keeps that one implementation — see terminalCommand.
	s.hubs = newHubs(s.terminalCommand, defaultReplayBytes, defaultMaxWarmHubs, nil)
	return s
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("GET /help", s.handleHelp)
	mux.HandleFunc("GET /help.css", s.handleHelpCSS)
	mux.HandleFunc("GET /vendor/{file}", s.handleVendor)
	mux.HandleFunc("GET /docs/{file}", s.handleDocs)
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
	// Set here as well as in writeJSON because this handler writes its body directly, to keep
	// it byte-identical to ttyd's. See writeJSON for why the header is sent at all.
	//
	// The ttyd parity this endpoint maintains is over the *body*: real ttyd sends no
	// Cache-Control, so the responses now differ by this header. Nothing checks headers here --
	// the conformance suite captures and diffs the body alone -- and a cache directive is not
	// something a ttyd client can be broken by.
	w.Header().Set("Cache-Control", "no-store")
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
	//
	// no-store because every response here is live state read from the filesystem or /proc at
	// request time, and a cached one is indistinguishable from a wrong one. The spec has
	// declared this header on the API since before any of it was polled by a browser and
	// nothing ever sent it (#116) -- it was a lie a native client's own request policy hid.
	// Setting it in the one function every JSON response goes through is why the fix is a line
	// rather than a per-handler habit that the next handler forgets.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
