package files

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"go.abhg.dev/goldmark/mermaid"
)

// md renders GitHub-flavored markdown. Raw HTML passes through (WithUnsafe): READMEs
// routinely use <img>/<details>. What raw HTML must NOT be able to do is run script, because
// this page now shares an origin with an unauthenticated shell — see pageCSP in handler.go,
// which is the control that makes WithUnsafe acceptable here.
var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		highlighting.NewHighlighting(highlighting.WithStyle("github")),
		&mermaid.Extender{
			// Explicit: AutoRenderMode silently switches to server-side
			// rendering if an mmdc binary ever appears on PATH.
			RenderMode: mermaid.RenderModeClient,
			// The page template emits the mermaid <script> tags itself, carrying the CSP
			// nonce. Tags emitted here would carry none and be blocked.
			NoScript: true,
		},
	),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

func renderMarkdown(src []byte) (template.HTML, error) {
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}
