package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// Browser assets, embedded so wtd stays one self-contained binary. Vendored third-party
// files and their provenance are in web/vendor/.
//
//go:embed web
var webFS embed.FS

// handleRoot serves the browser entry point, splitting on ?arg= exactly as the spec
// documents:
//
//	/           -> the session picker
//	/?arg=name  -> a terminal attached to that session
//
// The split matters beyond tidiness. ttyd's page forwards location.search to the socket, so
// `/?arg=demo` is how CLAUDE.md tells you to exercise the deep-link path without a phone —
// the same path a saved iOS profile uses. Keeping that recipe working is the point.
func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// ServeMux routes every unmatched path to "/", so anything else is a 404 rather than
	// the picker silently rendering at invented URLs.
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, codeNotFound, "no such route", r.URL.Path)
		return
	}

	page := "web/index.html"
	if r.URL.Query().Has("arg") {
		page = "web/terminal.html"
	}
	s.serveAsset(w, r, page, "text/html; charset=utf-8")
}

// handleHelp serves the human-facing help page. The terminal page's "?" overlay fetches
// this same document rather than carrying its own copy, so the *copy* cannot drift —
// adding a finding to web/help.html reaches both. The presentation is what can drift,
// which is why the FAQ styling is a shared asset (help.css) and not two hand-copies.
func (s *server) handleHelp(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, r, "web/help.html", "text/html; charset=utf-8")
}

func (s *server) handleHelpCSS(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, r, "web/help.css", "text/css; charset=utf-8")
}

// handleVendor serves the vendored xterm assets.
//
// Deliberately not http.FileServer over the whole embedded tree: that would also expose
// LICENSE files, PROVENANCE.md and SHA256SUMS, and would serve directory listings. An
// explicit allowlist keeps the served surface to exactly what the terminal page loads.
func (s *server) handleVendor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")

	contentType, ok := vendorAssets[name]
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no such asset", name)
		return
	}
	s.serveAsset(w, r, "web/vendor/"+name, contentType)
}

// vendorAssets is the allowlist: filename -> content type. Adding an asset means adding it
// here, which is the intended friction.
var vendorAssets = map[string]string{
	"xterm.js":       "text/javascript; charset=utf-8",
	"xterm.css":      "text/css; charset=utf-8",
	"addon-fit.js":   "text/javascript; charset=utf-8",
	"addon-webgl.js": "text/javascript; charset=utf-8",
}

// assetETags maps each embedded asset to a strong validator, hashed once on first use.
//
// The validator is the half of "revalidate cheaply" that was missing: Cache-Control:
// no-cache asks the browser to check before reusing its copy, but a check needs something
// to check *against*, and with neither ETag nor Last-Modified the only thing a browser can
// do is fetch the whole file again. So every page load re-downloaded all of /vendor/ (#62),
// which stopped being free once the WebGL renderer took that from 496 KB to ~744 KB.
//
// It has to be a content hash: embed.FS reports a zero modtime for everything, so
// Last-Modified would be a constant and http.ServeContent has nothing to work with either.
//
// A walk that fails yields no tags, which serves every asset exactly as before.
var assetETags = sync.OnceValue(func() map[string]string {
	tags := map[string]string{}
	_ = fs.WalkDir(webFS, "web", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, readErr := webFS.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(body)
		tags[path] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return tags
})

// etagMatches reports whether an If-None-Match header covers tag. Browsers echo back the
// one tag they were given, but a proxy in front of this server may send a list or "*".
func etagMatches(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
			return true
		}
	}
	return false
}

func (s *server) serveAsset(w http.ResponseWriter, r *http.Request, path, contentType string) {
	body, err := webFS.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "asset not embedded", path)
		return
	}

	w.Header().Set("Content-Type", contentType)
	if strings.HasPrefix(path, "web/vendor/") {
		// Vendored assets change only when the binary does, and their names are version-
		// less, so a long cache would survive an upgrade. Revalidation is cheap on a LAN,
		// and cheaper still when the ETag lets it end in a 304.
		w.Header().Set("Cache-Control", "no-cache")
		if tag := assetETags()[path]; tag != "" {
			w.Header().Set("ETag", tag)
			if etagMatches(r.Header.Get("If-None-Match"), tag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	} else {
		// The pages are no-store, so a validator would have nothing to validate.
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
