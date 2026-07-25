---
paths:
  - "bin/wt"
  - "bin/wt-serve"
  - "cmd/wtd/ws.go"
  - "cmd/wtd/hub.go"
  - "cmd/wtd/web/**"
  - "api/**"
---

# The primary client is a native iOS app in another repo

**`~/src/ios-claude-terminal`** — "WebClaude", a native iOS/iPadOS ttyd client. It does **not**
wrap ttyd's web UI: it speaks ttyd's WebSocket protocol directly (`URLSessionWebSocketTask`,
subprotocol `tty`) and renders with SwiftTerm. Read `WebClaude/Networking/TtydProtocol.swift` and
`Models/ServerProfile.swift` before changing anything on the wire — that's the whole client
contract in ~100 lines.

It's built with XcodeGen + Xcode and dev-signed, **not** shipped through the App Store, so it can
be rebuilt freely. The risk isn't review latency — it's that the two repos have no shared CI, no
type checking across the boundary, and beta users' installed builds don't update when this repo
does. A server change lands silently and breaks a phone.

## What the client depends on

| Thing | Where | Consequence if changed |
|---|---|---|
| `ttyd -W` (writable) | `args=(…)` in `bin/wt-serve` | Input is **silently dropped** — terminal looks fine, keystrokes do nothing |
| `ttyd -a` (`--url-arg`) | same line | `?arg=` is ignored, so deep-link profiles fall back to the menu |
| `/ws` endpoint, `/token` GET | ttyd itself | App GETs `/token`, ignores failure, sends `AuthToken: ""` — fine with no `-c` |
| Port **7681** | `WT_PORT` default | It's `ServerProfile.port`'s default; editable per profile, so a change means "every beta user edits their profile", not a hard break |
| `wt <name>` attaches **or creates** | `dtach -A` in `bin/wt` | A saved deep-link must work before the session exists |
| Plain `ws://`, no TLS | — | The app opens ATS wholesale for tailnet `ws://`. Adding TLS/`wss` is a client-side change too, not a drop-in |

## Two client modes are both in use

They exercise different code paths here:

- **Menu mode** — profile with no `sessionArg`. Lands on the `wt` picker. The menu is rendered by
  SwiftTerm on a phone screen and read by a human — nothing parses it, so reformatting is safe, but
  keep it **narrow** (a portrait phone is ~40 cols) and keep the single-keystroke choices.
- **Deep-link mode** — profile with `sessionArg` set → `ws://host:7681/ws?arg=<name>` → arrives as
  `$1`. This is why the direct-attach branch matters: the app auto-reconnects after drops and
  backgrounding (~30s grace), and only a `sessionArg` profile rejoins its session unattended.
  Without one, every reconnect dumps the user back at the menu.

`wt` only reads `$1` — ttyd passes a single `?arg=`. A name containing `/` or `..` is dropped and
the picker renders instead of erroring; that's what a client sees on a malformed URL, so keep the
graceful fallback.

**Exercise the deep-link path, not just the menu** — the menu passing proves nothing about the
client's hot path. ttyd's own web page forwards `location.search` to the socket, so
`http://127.0.0.1:7682/?arg=demo` drives the same `$1` branch the app's `sessionArg` uses, no app
or simulator needed.

## Blank-on-attach: fixed on the `wtd` path only, and the iOS half is still shipped

dtach keeps no screen buffer, so a reattach shows blank until something writes. That used to be
worked around from both sides: `bin/wt` passes `dtach -r winch`, and the app *also* jiggles the
window size on connect (`rows-1`, then real, in `TtydConnection.scheduleRedrawKick`). `wtd` now
holds each deep-linked session's attachment and replays its recent output on attach, and does the
size jiggle itself once per session (`hub.kick`), so neither client needs to. Consequences:

- The browser page's kick is gone (`cmd/wtd/web/terminal.html`). **The iOS one is not** — removing
  it is a separate PR in that repo, tracked in `heysamtexas/ios-claude-terminal#1`, and it must not
  land before this server is deployed to the boxes those builds talk to. It needs feature detection
  on `scrollback-replay` first. Harmless meanwhile: an extra SIGWINCH.
- **ttyd still has the old behavior**, since replay lives in `wtd`. On `wt.service` the two-sided
  workaround is still the only thing there is; don't remove either half there.
- If blank-on-attach regresses, first work out which server the client hit.

The app pings every 20s, so don't introduce a server-side idle timeout below that.
`ServerProfile.pathPrefix` supports ttyd behind a reverse proxy — a deployment shape this repo
doesn't document but the client already handles.

**`WT_AUTH` does not break the native app.** Basic auth breaks Safari and every iOS *browser*
(WebKit omits credentials on the WebSocket upgrade), which is why the default is empty. But the
`BasicCredentials` seam in `TtydConnection.swift` already sets `Authorization` on both the `/token`
GET and the upgrade — plumbed, no UI yet. So for an app-only fleet, `WT_AUTH` is a real option;
enabling it is a two-repo change. Network-layer control stays the default recommendation; just
don't dismiss auth as impossible on iOS.
