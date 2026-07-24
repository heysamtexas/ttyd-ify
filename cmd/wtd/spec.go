package main

import _ "embed"

// The OpenAPI spec is served so a client can discover this server's surface at runtime.
//
// api/openapi.yaml is the source of truth and api/openapi.json is generated from it by
// `make spec`, with CI asserting the two are in sync. Generating at build time rather than
// converting at runtime keeps wtd free of a YAML dependency — the whole binary is stdlib
// plus a pty and a websocket library, and a spec converter is not worth changing that.
//
//go:embed openapi.json
var openAPIJSON []byte
