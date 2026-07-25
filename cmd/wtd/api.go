package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
)

// The JSON API. Shapes, status codes and error codes come from api/openapi.yaml; this
// file is the implementation, and any divergence is a bug in one of the two.

// Capability strings, from the registry in api/openapi.yaml. This list is the only
// cross-repo skew defense that exists: the iOS client gates each screen on a flag here,
// so an older app against a newer server (or the reverse) degrades on purpose instead of
// failing in a way nobody can diagnose.
var features = []string{
	"token",
	"url-arg",
	"writable",
	"sessions-api",
	"projects-api",
	"picker-ui",
	"scrollback-replay",
	// The documents this spec references are served at /docs/, so a reference in the spec is
	// a URL a client can fetch rather than a filename in a repo it may not have.
	"docs-endpoint",
	// GET /api/v1/sessions/{name}. Separate from sessions-api because that one flag gates
	// three routes, so a client could not otherwise tell this one apart.
	"session-read",
}

// Error codes from the registry. Clients switch on these, never on the message.
const (
	codeBadRequest         = "bad_request"
	codeInvalidName        = "invalid_name"
	codeInvalidPath        = "invalid_path"
	codeUnknownProject     = "unknown_project"
	codeProjectPathMissing = "project_path_missing"
	codePathAndProject     = "path_and_project"
	codeAlreadyExists      = "already_exists"
	codeNotFound           = "not_found"
	codeOriginForbidden    = "origin_forbidden"
	codeUnsupportedMedia   = "unsupported_media_type"
	codePayloadTooLarge    = "payload_too_large"
	codeInternal           = "internal"
)

// maxBodyBytes caps request bodies. Small on purpose: the only bodies this API takes are
// a name and a path.
const maxBodyBytes = 16 << 10

func (s *server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/meta", s.handleMeta)
	mux.HandleFunc("GET /api/v1/sessions", s.handleSessionsList)
	mux.HandleFunc("GET /api/v1/sessions/{name}", s.handleSessionGet)
	mux.HandleFunc("POST /api/v1/sessions", s.guardMutating(s.handleSessionCreate))
	mux.HandleFunc("DELETE /api/v1/sessions/{name}", s.guardMutating(s.handleSessionDelete))
	mux.HandleFunc("GET /api/v1/projects", s.handleProjects)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
}

// guardMutating applies the CSRF policy to state-changing routes.
//
// There is no authentication, so the usual credential-based defenses do not exist: any
// page the user visits could otherwise POST to this server's tailnet address and create or
// destroy sessions. Two checks, both cheap:
//
//   - An Origin header, if present, must name this same host. Native clients send none
//     and are unaffected; wtd's own picker is same-origin.
//   - POST must be application/json, which a plain HTML form cannot send — that kills
//     form-based CSRF outright, since a form POST would trigger a preflight that fails.
//
// Residual exposure is stated honestly in the spec: DNS rebinding defeats an
// Origin-vs-Host comparison, because both names then belong to the attacker.
func (s *server) guardMutating(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			writeError(w, http.StatusForbidden, codeOriginForbidden,
				"cross-origin request rejected",
				"Origin "+origin+" does not match Host "+r.Host)
			return
		}
		if r.Method == http.MethodPost {
			ct := r.Header.Get("Content-Type")
			if mediaType(ct) != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, codeUnsupportedMedia,
					"Content-Type must be application/json", "got "+ct)
				return
			}
		}
		next(w, r)
	}
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // including Origin: null, which no legitimate client here sends
	}
	return strings.EqualFold(u.Host, host)
}

func mediaType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

// shortHostname is the hostname bin/wt's menu header prints, which uses `hostname -s`.
//
// os.Hostname returns whatever the kernel holds, FQDN included, and two surfaces naming the
// same box differently reads as a bug to whoever is looking at both. Kept as a function rather
// than inlined so the rule is testable on a machine whose own hostname is already short —
// which is most of them, and would otherwise make the assertion vacuous exactly where it runs.
func shortHostname(h string) string {
	short, _, _ := strings.Cut(h, ".")
	return short
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  version,
		"features": features,
		// The real ceiling, which the create-side pattern cannot express because it depends on
		// WT_DIR's depth. Without this a client learns its own input limit only by submitting a
		// too-long name and parsing an integer out of an English sentence.
		"maxSessionNameLength": min(maxSessionNameLen, max(sessionNameRoom(s.sessionDir()), 0)),
		"hostname":             shortHostname(hostname),
		"user":                 username,
		// Where to open the terminal WebSocket, not a filesystem path. It was documented as
		// the start command's absolute path for a long time, which was never what this
		// returned — see the Meta schema and TestMetaMatchesItsSchema.
		"terminalPath": "/ws",
		// terminalPath's counterpart for the JSON API. Without it the one field designed to
		// keep a client off hardcoded paths covered exactly one of the two surfaces that
		// would need to move together behind a path prefix, so a `base-path` deployment
		// would relocate /ws discoverably and /api/v1 silently.
		"apiPath": "/api/v1",
	})
}

// handleSessionGet reads one session, which is what the 201 Location header has always pointed
// at. Until now that path had only a DELETE, so a client following the header got a 405 — and
// refreshing a single session after a create meant listing every session and filtering.
//
// Byte-exact matching against the real names, never a path built from the request string: the
// same rule deleteSession follows, for the same reason. The list is the source of both.
func (s *server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	sessions, err := listSessions(s.sessionDir(), s.hubs.stats())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot read sessions", err.Error())
		return
	}
	for i := range sessions {
		if sessions[i].Name == name {
			writeJSON(w, http.StatusOK, sessions[i])
			return
		}
	}
	writeError(w, http.StatusNotFound, codeNotFound, "no such session", name)
}

func (s *server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	// Reap on the read path: a stale socket lists as a session that cannot be attached,
	// and bin/wt is deliberately not taught to clean up, so this is where the two
	// pickers get reconciled.
	if reaped := reapStale(s.sessionDir()); len(reaped) > 0 {
		logf("wtd: reaped stale sessions: %s", strings.Join(reaped, ", "))
	}

	sessions, err := listSessions(s.sessionDir(), s.hubs.stats())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot read sessions", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	// Validation order is specified: body size → JSON parse → name → path/project.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
			"request body too large", err.Error())
		return
	}

	// Unknown fields are ignored rather than rejected: a newer client against an older
	// server should degrade predictably, and capability discovery is /api/v1/meta's job.
	var req struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Project string `json:"project"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "body is not a JSON object", err.Error())
		return
	}

	if err := validateSessionName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidName, "invalid session name", err.Error())
		return
	}
	// A name can satisfy every rule above and still be unusable here, because the limit is on
	// the socket path and WT_DIR's depth is the rest of it. 400 rather than 500: the caller can
	// act on this, and the detail names the real constraint rather than the symptom.
	if err := validateSocketPath(s.sessionDir(), req.Name); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidName, "session name is too long for this server", err.Error())
		return
	}
	if req.Path != "" && req.Project != "" {
		writeError(w, http.StatusBadRequest, codePathAndProject,
			"path and project are mutually exclusive", "")
		return
	}

	workdir, errCode, errMsg := s.resolveWorkdir(req.Path, req.Project)
	if errCode != "" {
		writeError(w, http.StatusBadRequest, errCode, errMsg, "")
		return
	}

	if err := createSession(s.sessionDir(), req.Name, workdir); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, codeAlreadyExists, err.Error(), "")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot create session", err.Error())
		return
	}

	sessions, err := listSessions(s.sessionDir(), s.hubs.stats())
	if err == nil {
		for _, sess := range sessions {
			if sess.Name == req.Name {
				writeJSON(w, http.StatusCreated, sess)
				return
			}
		}
	}
	// Created but not yet observable; report what was asked for rather than failing.
	writeJSON(w, http.StatusCreated, Session{Name: req.Name, CWD: workdir})
}

// resolveWorkdir turns path/project into a directory, or returns an error code.
//
// Note the deliberate difference from bin/wt: the menu silently falls back to $HOME when a
// project path is missing (bin/wt:82). The API refuses instead — a caller that asked for a
// specific directory and silently got $HOME has no way to notice.
func (s *server) resolveWorkdir(path, project string) (dir, errCode, errMsg string) {
	home, _ := os.UserHomeDir()

	switch {
	case path != "":
		if !filepath.IsAbs(path) {
			return "", codeInvalidPath, "path must be absolute"
		}
		if hasControlBytes(path) {
			return "", codeInvalidPath, "path contains control bytes"
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return "", codeInvalidPath, "path does not exist or is not a directory"
		}
		return path, "", ""

	case project != "":
		projects := loadProjects(s.projectsFile())
		p, ok := projects[project]
		if !ok {
			return "", codeUnknownProject, "no project named " + project
		}
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() {
			return "", codeProjectPathMissing, "project " + project + " points at " + p + ", which does not exist"
		}
		return p, "", ""

	default:
		return home, "", ""
	}
}

func (s *server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusNotFound, codeNotFound, "no session named in the request path", "")
		return
	}

	switch err := deleteSession(s.sessionDir(), name); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errNotFound):
		// Also the answer for any traversal attempt: the name is matched against real
		// entries, so ../../etc/passwd simply is not a session.
		writeError(w, http.StatusNotFound, codeNotFound, "no such session", "")
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot delete session", err.Error())
	}
}

func (s *server) handleProjects(w http.ResponseWriter, r *http.Request) {
	projects := loadProjects(s.projectsFile())

	type project struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Exists bool   `json:"exists"`
	}
	out := make([]project, 0, len(projects))
	for name, path := range projects {
		info, err := os.Stat(path)
		out = append(out, project{Name: name, Path: path, Exists: err == nil && info.IsDir()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if openAPIJSON == nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "spec not embedded", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPIJSON)
}
