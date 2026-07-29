---
paths:
  - "cmd/wtd/attach.go"
  - "bin/wt-serve"
  - "cmd/wtd/ws.go"
  - "cmd/wtd/hub.go"
  - "cmd/wtd/web/**"
  - "api/**"
---

# The primary client is a native iOS app in another repo

**`~/src/ios-claude-terminal`** — "WebClaude", a native iOS/iPadOS **wtd** client. It speaks
ttyd's WebSocket protocol directly (`URLSessionWebSocketTask`, subprotocol `tty`) and renders with
SwiftTerm, but plain-ttyd support was removed: it is wtd-only, and SSH is its stated recovery path.
Read `WebClaude/Networking/WtdProtocol.swift` and `Models/ServerProfile.swift` before changing
anything on the wire.

**Verify claims about it in that repo, not from this file.** Everything below was true of
`ios-claude-terminal@231315d`. This file has been wrong before — it described the `Ttyd*` filenames,
a `/token` round trip and a `BasicCredentials` seam for a day after all three were deleted, and the
citations in `api/*.md` dangled with it (#33). A local checkout can also be behind; `git fetch`
first.

It's built with XcodeGen + Xcode and dev-signed, **not** shipped through the App Store, so it can
be rebuilt freely. The risk isn't review latency — it's that the two repos have no shared CI, no
type checking across the boundary, and beta users' installed builds don't update when this repo
does. A server change lands silently and breaks a phone.

## What the client depends on

| Thing | Where | Consequence if changed |
|---|---|---|
| Writable (ttyd's `-W`) | Hardcoded in `wtd`, no knob | Input is **silently dropped** — terminal looks fine, keystrokes do nothing. It was a ttyd flag; wtd removed the footgun |
| `?arg=` → session selection (ttyd's `-a`) | Hardcoded in `wtd`, no knob | `?arg=` is ignored, so deep-link profiles fall back to a bare shell with no session behind it |
| `/ws` endpoint | `cmd/wtd/ws.go` | The app **no longer GETs `/token`** — that round trip was deleted, saving one per connect. It still sends `AuthToken: ""` in the handshake. `/token` is kept for ttyd parity and for wtd's own terminal page, not for this client |
| `sessions-api` in `features[]` | `cmd/wtd/api.go` | **Hard requirement, not a fallback.** A server that answers without advertising it shows "Not a wtd Server" and offers Retry; there is no bare-terminal degraded mode any more. So this flag can never be withdrawn — doing so locks the app out rather than degrading it. `session-read` *is* optional: without it the app falls back to list-and-filter |
| Port **7681** | `WT_PORT` default | It's `ServerProfile.port`'s default. Retiring ttyd moved `wtd` onto this port for exactly that reason (#23), so no profile needed editing |
| `?arg=<name>` attaches **or creates** | `dtach -A` in `cmd/wtd/attach.go` | A saved deep-link must work before the session exists |
| Plain `ws://`, no TLS | — | The app opens ATS wholesale for tailnet `ws://`. Adding TLS/`wss` is a client-side change too, not a drop-in |

## Every connection this client makes is named

This file said "two client modes are both in use" for a while after it stopped being true, which is
the exact failure this header warns about — it shaped server decisions around a path the app cannot
reach. Verified in that repo: `sessionName` on `TerminalRoute` and `WtdConnection` is
**non-optional**, and the comment there calls that load-bearing — "the only un-named connection the
app ever made was the plain-ttyd break-glass route", removed with plain-ttyd support. `SessionListView`
says the same.

So there is one mode: **deep-link**. `ws://host:7681/ws?arg=<name>`. The app auto-reconnects after
drops and backgrounding (~30s grace) and rejoins its session unattended, which only works because
`?arg=` creates-or-attaches.

An **argless** connection is now a plain shell on the server rather than a picker, and no shipped
client opens one — reaching it takes a hand-typed URL. A name containing `/` or `..`, or one too long
for a socket path, opens that same shell with one `wtd:` line saying why, and stays *named* so it
still shares and replays. Never an error, never a close: keep that graceful fallback, because the
client backs off and retries on 1011 and a typo'd `sessionArg` would loop instead of showing its user
anything.

**Exercise the deep-link path** — it is the only path. wtd's terminal page forwards
`location.search` to the socket exactly as ttyd's did, so `http://<bound-address>:7681/?arg=demo`
drives the same code the app's `sessionArg` uses, no app or simulator needed.

## Blank-on-attach: fixed on the server, and the client now gates on the flag

dtach keeps no screen buffer, so a reattach shows blank until something writes. That used to be
worked around from both sides: `dtach -r winch` is passed on every attach, and the app *also* jiggled the
window size on connect. `wtd` now holds each deep-linked session's attachment, replays its recent
output on attach, and does the size jiggle itself once per session (`hub.kick`). Consequences:

- The browser page's kick is gone (`cmd/wtd/web/terminal.html`). **The iOS kick still exists but is
  gated** on the server advertising `scrollback-replay`, so it does not fire against this server and
  still does against an un-upgraded one (`scheduleRedrawKick`). `ios-claude-terminal#1` is closed;
  the comment there says the gate must stay and that the way to retire the kick is to ship the
  feature server-side, not to delete the client code.
- **That gate is load-bearing in both directions**, which is why `hub.kick` fires on an *empty
  replay snapshot* and not only on the first client of a hub: an operator running
  `WT_REPLAY_BYTES=0` still advertises `scrollback-replay`, and without that second trigger a
  client trusting the flag would attach to a blank screen forever. Do not "simplify" the kick to
  spawn-time only — see `api/ws-protocol.md` §7a.
- `wtd` still passes `dtach -r winch` (`dtachArgs`). Keep it: dtach keeps no screen buffer, so it is
  what redraws a client attaching to an idle session, which replay alone does not cover when the
  ring is empty.

The app pings every 20s, so don't introduce a server-side idle timeout below that.
`ServerProfile.pathPrefix` supports the server behind a reverse proxy — a deployment shape this repo
doesn't document but the client already handles.

**`WT_AUTH` does not break the native app** — it is not a browser, and can set `Authorization`
itself. Basic auth breaks Safari and every iOS *browser* (WebKit omits credentials on the WebSocket
upgrade), which is why the default was empty.

But the estimate in #27 grew: that client once carried a `BasicCredentials` seam plumbing the header
onto both the `/token` GET and the upgrade, and it was **deleted** when the client became wtd-only —
a server that refuses to start with `WT_AUTH` set made the seam unreachable, so it was carrying no
weight. The client half is therefore a re-add, not a UI on existing plumbing. Still a real future
option behind the reserved `auth-basic` flag, still a two-repo change, and `/token` would have to
start returning a real token. Meanwhile `wtd` implements none of it and **refuses to start if
`WT_AUTH` is set** rather than serve an unauthenticated shell to an operator who configured a
password. Network-layer control stays the recommendation; just don't dismiss auth as impossible on
iOS.
