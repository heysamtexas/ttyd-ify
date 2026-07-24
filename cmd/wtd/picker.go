package main

import (
	"embed"
	"net/http"
)

// The browser picker. Embedded so wtd stays a single self-contained binary — the same
// reason there are no external assets inside the page itself.
//
//go:embed web/index.html
var webFS embed.FS

// handleRoot serves the picker.
//
// Route shape matches ttyd's, where `/` is the browser entry point, and matters for a
// reason beyond tidiness: ttyd's page forwards location.search to the socket, so
// `/?arg=demo` is how CLAUDE.md tells you to exercise the deep-link path without a phone.
//
// KNOWN GAP: with ?arg= present this should serve a terminal page rather than the picker
// (api/openapi.yaml documents that split). Doing so needs a terminal emulator embedded in
// the binary, which is Phase 5 work and a real decision — vendoring a minified xterm.js
// bundle into a repo whose stated value is "readable in a browser at 3am" is not something
// to do incidentally. Until then ttyd on WT_PORT still serves the browser terminal, which
// is why the migration runs both.
func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// http.ServeMux gives "/" every unmatched path, so anything else is a 404 rather
	// than silently rendering the picker at made-up URLs.
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, codeNotFound, "no such route", r.URL.Path)
		return
	}

	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "picker not embedded", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is embedded in the binary, so its content changes only when the binary
	// does; no-store keeps a stale picker from surviving an upgrade.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}
