# Vendored: mermaid

`mermaid.min.js` is [`mermaid`](https://www.npmjs.com/package/mermaid) 11.12.0, `dist/mermaid.min.js`,
byte-identical to the published file. MIT licensed (`LICENSE.mermaid`). Committed rather than
loaded from a CDN for the reason `cmd/wtd/web/vendor/PROVENANCE.md` gives: the page has to work on a
tailnet with no route to the internet. Only `mermaid.min.js` is embedded and served.

```
07e37dfa97b337ccc85365d57eddf99b9706f09db3b59b260d0333b23b343c4b  mermaid.min.js
```

To update, fetch `https://cdn.jsdelivr.net/npm/mermaid@<version>/dist/mermaid.min.js`, replace
the file and the checksum above, and open a markdown page that has a diagram under the page's
CSP (`pageCSP` in `handler.go`). A mermaid release that starts needing `eval` or inline
scripts fails there, and shows the diagram's source instead of the diagram.
