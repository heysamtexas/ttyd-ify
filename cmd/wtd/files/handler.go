// Package files is the read-only file and markdown browser wtd serves at /files/ (#144).
//
// Ported from mdbrowse, which ran as its own service on its own port. It is opt-in: wtd
// mounts it only when a root is configured, and it then refuses to start without a valid
// password file. Read-only by construction — GET and HEAD only, and nothing here writes,
// deletes or executes.
//
// Moving it onto wtd's port put it in the same origin as an unauthenticated shell, which
// mdbrowse never was. Any script that runs in a page served from here can open /ws with a
// matching Origin and type into a terminal. So the one rule this package adds over mdbrowse:
// nothing it serves may run script except wtd's own, which is what pageCSP and rawCSP are for.
package files

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed page.html login.html
var templates embed.FS

//go:embed assets/mermaid.min.js
var assets embed.FS

// mermaidAsset is the one file under assets/ that is served. An explicit name rather than a
// file server over the directory, so the license file and any directory listing stay
// unexposed — the same rule wtd's handleVendor follows.
const mermaidAsset = "/assets/mermaid.min.js"

// Extensions forced to text/plain so the browser shows them inline instead of
// offering a download. Anything absent falls through to http.ServeFile's own
// extension/sniff detection (which already handles images, html, pdf, etc.).
var textExts = map[string]bool{
	".bash": true, ".cfg": true, ".conf": true, ".env": true, ".gitignore": true,
	".go": true, ".hujson": true, ".ini": true, ".jsonl": true, ".log": true,
	".mk": true, ".mod": true, ".py": true, ".rb": true, ".rs": true,
	".service": true, ".sh": true, ".sql": true, ".sum": true, ".tf": true,
	".tfvars": true, ".toml": true, ".ts": true, ".tsx": true, ".txt": true,
	".yaml": true, ".yml": true, ".zsh": true,
}

// rawCSP goes on every file served as itself. `sandbox` with no allow-scripts runs no script
// at all and gives the document an opaque origin, so an .html or .svg in the tree renders as
// a static page that cannot reach the rest of this server. Without it, a cloned repo's
// demo page would be a shell for whoever opened it.
const rawCSP = "sandbox"

// pageCSP goes on every page rendered from the templates. Raw HTML in markdown passes through
// (see md), so only scripts carrying this response's nonce run — mermaid, emitted by the
// template. A <script>, an inline handler or a javascript: URL in a README is refused, and so
// is <script src> pointing at a .js file elsewhere in the tree, which is same-origin and would
// otherwise be trusted. frame-src stops a README from embedding the terminal.
func pageCSP(nonce string) string {
	return "script-src 'nonce-" + nonce + "'; object-src 'none'; base-uri 'none'; frame-src 'none'; form-action 'none'"
}

type browser struct {
	root string
	base string
	tmpl *template.Template
}

type crumb struct {
	Name string
	Href string
}

type entry struct {
	Name  string
	Href  string
	IsDir bool
}

type pageData struct {
	Title      string
	Crumbs     []crumb
	IsDir      bool
	Entries    []entry
	Body       template.HTML
	RawHref    string // markdown pages: link to ?raw=1
	ToggleHref string // dir pages: show/hide dot-entries
	ToggleName string
	Mermaid    string // the mermaid script's URL, when the page has a diagram
	Nonce      string
}

// New returns the browser for root, gated by the bcrypt hash in passwdFile, for mounting at
// base ("/files") behind http.StripPrefix(base, ...). It fails closed: a root that is not a
// directory or a password file newAuth rejects is an error, never an ungated handler.
func New(root, passwdFile, base string) (http.Handler, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("files root: %w", err)
	}
	if info, err := os.Stat(absRoot); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("files root %s is not a directory", absRoot)
	}
	tmpl, err := template.ParseFS(templates, "*.html")
	if err != nil {
		return nil, err
	}
	a, err := newAuth(passwdFile, base, tmpl)
	if err != nil {
		return nil, err
	}
	b := &browser{root: absRoot, base: base, tmpl: tmpl}

	// Everything is behind require except /login itself — the mermaid asset included.
	inner := http.NewServeMux()
	inner.HandleFunc("GET "+mermaidAsset, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		http.ServeFileFS(w, r, assets, strings.TrimPrefix(mermaidAsset, "/"))
	})
	inner.HandleFunc("/", b.browse)

	mux := http.NewServeMux()
	mux.HandleFunc("/login", a.login)
	mux.Handle("/", a.require(inner))
	return mux, nil
}

func (b *browser) browse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Lexical jail: Clean of a rooted path cannot contain "..", so the join
	// below cannot escape root. Symlinks inside root are deliberately followed:
	// anyone past the gate can already read the tree as the service user.
	rel := path.Clean("/" + r.URL.Path)
	fsPath := filepath.Join(b.root, filepath.FromSlash(rel))
	info, err := os.Stat(fsPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			target := (&url.URL{Path: b.base + rel + "/"}).EscapedPath()
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		b.serveDir(w, r, fsPath, rel)
		return
	}
	switch ext := strings.ToLower(filepath.Ext(fsPath)); {
	case ext == ".md" || ext == ".markdown":
		if r.URL.Query().Get("raw") == "" {
			b.serveMarkdown(w, fsPath, rel)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case textExts[ext]:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Content-Security-Policy", rawCSP)
	http.ServeFile(w, r, fsPath)
}

func (b *browser) serveDir(w http.ResponseWriter, r *http.Request, fsPath, rel string) {
	showAll := r.URL.Query().Get("all") == "1"
	dirents, err := os.ReadDir(fsPath)
	if err != nil {
		http.Error(w, "cannot read directory", http.StatusInternalServerError)
		return
	}
	suffix := ""
	if showAll {
		suffix = "?all=1"
	}
	var entries []entry
	for _, d := range dirents {
		name := d.Name()
		if !showAll && strings.HasPrefix(name, ".") {
			continue
		}
		isDir := d.IsDir()
		if !isDir && d.Type()&os.ModeSymlink != 0 {
			// a symlink to a directory should browse like one
			if info, err := os.Stat(filepath.Join(fsPath, name)); err == nil && info.IsDir() {
				isDir = true
			}
		}
		href := url.PathEscape(name)
		if isDir {
			href += "/"
		}
		entries = append(entries, entry{Name: name, Href: href + suffix, IsDir: isDir})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	toggleHref, toggleName := "?all=1", "show hidden"
	if showAll {
		toggleHref, toggleName = "?", "hide hidden"
	}
	b.render(w, pageData{
		Title:      rel,
		Crumbs:     b.crumbs(rel, suffix),
		IsDir:      true,
		Entries:    entries,
		ToggleHref: toggleHref,
		ToggleName: toggleName,
	})
}

func (b *browser) serveMarkdown(w http.ResponseWriter, fsPath, rel string) {
	src, err := os.ReadFile(fsPath)
	if err != nil {
		http.Error(w, "cannot read file", http.StatusInternalServerError)
		return
	}
	body, err := renderMarkdown(src)
	if err != nil {
		http.Error(w, "markdown render failed", http.StatusInternalServerError)
		return
	}
	data := pageData{
		Title:   path.Base(rel),
		Crumbs:  b.crumbs(rel, ""),
		Body:    body,
		RawHref: "?raw=1",
	}
	// Only pages with a diagram pay for the 2.7 MB script.
	if strings.Contains(string(body), `<pre class="mermaid">`) {
		data.Mermaid = b.base + mermaidAsset
	}
	b.render(w, data)
}

// crumbs builds the breadcrumb trail for rel ("/" or "/a/b/c"), as absolute links under base.
func (b *browser) crumbs(rel, suffix string) []crumb {
	cs := []crumb{{Name: filepath.Base(b.root), Href: b.base + "/" + suffix}}
	if rel != "/" {
		href := b.base
		for _, p := range strings.Split(strings.Trim(rel, "/"), "/") {
			href += "/" + url.PathEscape(p)
			cs = append(cs, crumb{Name: p, Href: href + "/" + suffix})
		}
	}
	return cs
}

func (b *browser) render(w http.ResponseWriter, data pageData) {
	// last crumb is the current page; render it unlinked
	data.Crumbs[len(data.Crumbs)-1].Href = ""
	nonce, err := newNonce()
	if err != nil {
		http.Error(w, "cannot generate nonce", http.StatusInternalServerError)
		return
	}
	data.Nonce = nonce
	var buf bytes.Buffer
	if err := b.tmpl.ExecuteTemplate(&buf, "page", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", pageCSP(nonce))
	_, _ = buf.WriteTo(w)
}

func newNonce() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
