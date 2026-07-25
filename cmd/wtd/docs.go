package main

import (
	"embed"
	"net/http"
)

// The specification documents, served so that a reference to one is something a client can
// actually fetch.
//
// openapi.json is the contract, but it cannot express a WebSocket protocol — OpenAPI has no
// notion of one — so the terminal's wire detail lives in ws-protocol.md, and the reasoning
// behind the session model lives in session-lifecycle.md. Before these were served, the spec
// cited them by filename at a reader who had only the spec: a footnote pointing at a document
// they could not obtain, which is worse than no footnote, because it tells them they are
// missing something required without telling them what.
//
// Copied into this package by `make spec` for the same reason openapi.json is generated into
// it: go:embed cannot reach outside its own directory, and api/ is the source of truth.
// `make spec-check` fails if the copies drift.
//
//go:embed docs
var docsFS embed.FS

// docAssets is the allowlist: filename -> content type. Adding a document means adding it
// here, matching vendorAssets — the friction is intended, because everything in this map is
// published to anyone who can reach the port.
var docAssets = map[string]string{
	"ws-protocol.md":       "text/markdown; charset=utf-8",
	"session-lifecycle.md": "text/markdown; charset=utf-8",
	"compatibility.md":     "text/markdown; charset=utf-8",
}

// handleDocs serves one specification document. Modelled on handleVendor: allowlist lookup,
// then the shared asset writer.
func (s *server) handleDocs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")

	contentType, ok := docAssets[name]
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no such document", name)
		return
	}
	s.serveDoc(w, "docs/"+name, contentType)
}

// serveDoc mirrors serveAsset but reads from docsFS. Kept separate rather than parameterising
// serveAsset by filesystem: the two have different cache policies for different reasons, and a
// single function taking an fs.FS plus a cache mode would be longer than both.
func (s *server) serveDoc(w http.ResponseWriter, path, contentType string) {
	body, err := docsFS.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "document not embedded", path)
		return
	}

	w.Header().Set("Content-Type", contentType)
	// Same reasoning as the vendored assets: these change only when the binary does, so
	// revalidation is the right trade rather than a long cache that survives an upgrade.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
