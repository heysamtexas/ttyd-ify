package files

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// A file is shown as a page — highlighted source, a CSV table — rather than as itself when it is
// text, small enough, and something this file knows how to show. Everything else is served raw,
// exactly as before. The page is rendered server-side into escaped HTML, so showing a file this
// way runs none of it: the only scripts on the page are the template's own, under pageCSP.

// maxView is the largest file shown as a page; anything bigger is served raw. Highlighting is a
// regex lexer over the whole file, and a page of a megabyte of source is already unreadable.
const maxView = 1 << 20

// maxCSVRows caps a CSV table; the rest is a click away in raw.
const maxCSVRows = 5000

// rawOnly extensions keep their sandboxed render as themselves even though chroma has a lexer
// for them: someone opening an .html or .svg most likely wants to see it, not its source.
var rawOnly = map[string]bool{".html": true, ".htm": true, ".xhtml": true, ".svg": true}

var csvDelims = map[string]rune{".csv": ',', ".tsv": '\t'}

// The formatter emits classes, never inline styles, so the page's theme toggle can restyle
// tokens: inline colours would be one theme's and stay that way.
func codeFormatter() *chromahtml.Formatter {
	return chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithLineNumbers(true), chromahtml.TabWidth(4))
}

// chromaCSS is the token stylesheet for both themes: github-dark by default, github under
// data-theme="light". Each is scoped to its own theme so a token only one style colours cannot
// keep the other theme's colour.
func chromaCSS() (template.CSS, error) {
	var out strings.Builder
	for _, t := range []struct{ scope, style string }{
		{`:root:not([data-theme="light"])`, "github-dark"},
		{`:root[data-theme="light"]`, "github"},
	} {
		var buf bytes.Buffer
		if err := codeFormatter().WriteCSS(&buf, styles.Get(t.style)); err != nil {
			return "", err
		}
		// Every rule is "/* Token */ selector { ... }"; scope the selector. The block's own
		// background and colour are dropped: the page's palette (pre, --code-bg) sets those.
		for _, line := range strings.SplitAfter(buf.String(), "\n") {
			comment, rule, ok := strings.Cut(line, "*/ ")
			if !ok || strings.HasPrefix(rule, ".bg ") || strings.HasPrefix(rule, ".chroma {") {
				continue
			}
			out.WriteString(comment + "*/ " + t.scope + " " + rule)
		}
	}
	return template.CSS(out.String()), nil
}

// serveView renders fsPath as a page and reports whether it did; false means serve it raw.
func (b *browser) serveView(w http.ResponseWriter, r *http.Request, fsPath, rel string, info os.FileInfo) bool {
	name := info.Name()
	ext := strings.ToLower(filepath.Ext(name))
	if rawOnly[ext] || info.Size() > maxView {
		return false
	}
	comma := csvDelims[ext]
	lexer := lexers.Match(name)
	if lexer == nil && (textExts[ext] || comma != 0) {
		lexer = lexers.Fallback
	}
	if lexer == nil {
		return false
	}
	src, err := os.ReadFile(fsPath)
	if err != nil || bytes.IndexByte(src, 0) >= 0 || !utf8.Valid(src) {
		return false // binary, or gone: let the raw path answer
	}

	data := pageData{Title: path.Base(rel), Crumbs: b.crumbs(rel, ""), RawHref: "?raw=1", Wide: true}
	if comma != 0 {
		if body, truncated, err := csvTable(src, comma); err == nil {
			data.Body = body
			if truncated {
				data.Notice = fmt.Sprintf("Showing the first %d rows; raw has the rest.", maxCSVRows)
			}
			b.render(w, data)
			return true
		}
		// Not parseable as CSV: show it as text instead.
	}
	if ext == ".json" {
		data.PrettyHref, data.PrettyName = "?pretty=1", "pretty"
		if r.URL.Query().Get("pretty") == "1" {
			data.PrettyHref, data.PrettyName = "?", "as-is"
			var buf bytes.Buffer
			if err := json.Indent(&buf, src, "", "  "); err != nil {
				data.Notice = "Not valid JSON, so shown as-is: " + err.Error()
			} else {
				src = buf.Bytes()
			}
		}
	}
	body, err := highlight(lexer, string(src))
	if err != nil {
		return false
	}
	data.Body = body
	b.render(w, data)
	return true
}

func highlight(lexer chroma.Lexer, src string) (template.HTML, error) {
	it, err := chroma.Coalesce(lexer).Tokenise(nil, src)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	// With classes the style goes unused: the colours are chromaCSS.
	if err := codeFormatter().Format(&buf, styles.Fallback, it); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// csvTable renders src as an HTML table, the first row as its header. Every cell is escaped.
func csvTable(src []byte, comma rune) (body template.HTML, truncated bool, err error) {
	cr := csv.NewReader(bytes.NewReader(src))
	cr.Comma, cr.LazyQuotes, cr.FieldsPerRecord = comma, true, -1
	var buf strings.Builder
	buf.WriteString(`<table class="csv">` + "\n")
	for i := 0; ; i++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false, err
		}
		if i == maxCSVRows+1 {
			truncated = true
			break
		}
		cell := "td"
		if i == 0 {
			cell = "th"
		}
		buf.WriteString("<tr>")
		for _, f := range rec {
			buf.WriteString("<" + cell + ">" + template.HTMLEscapeString(f) + "</" + cell + ">")
		}
		buf.WriteString("</tr>\n")
	}
	buf.WriteString("</table>\n")
	return template.HTML(buf.String()), truncated, nil
}

// humanSize formats n the way `ls -lh` does: bytes below 1K, then one decimal below 10 and
// whole numbers above, base 1024, always rounded up.
func humanSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10)
	}
	const units = "KMGTPE"
	v, i := float64(n)/1024, 0
	for v >= 1024 && i+1 < len(units) {
		v /= 1024
		i++
	}
	if c := math.Ceil(v*10) / 10; c < 10 {
		return fmt.Sprintf("%.1f%c", c, units[i])
	}
	c := math.Ceil(v)
	if c >= 1024 && i+1 < len(units) {
		return fmt.Sprintf("1.0%c", units[i+1])
	}
	return fmt.Sprintf("%.0f%c", c, units[i])
}
