# wtd WebSocket protocol

> **Maintainer's copy, served for reference.** This document is written for someone working in
> the `ttyd-ify` repository. Bracketed citations — `[cmd/wtd/attach.go: func dtachArgs]`, `[iOS Models/ServerProfile.swift: port]`
> — name files in repositories you were not given, and tags like `[GT]` and `[LAB]` record how a
> claim was verified there. Neither is something you need in order to build a client: skip them,
> because every requirement is stated in the prose beside them. This document *is* normative for
> the `/ws` wire protocol, which OpenAPI cannot express; the contract for the HTTP surface is
> [`/openapi.json`](/openapi.json).

The ttyd wire protocol, written down properly for the first time. Until now it existed
only in ttyd's C source and, partially re-derived, in the iOS client's Swift. Both repos
have guessed at it independently; this document is the contract that ends the guessing.

`wtd` MUST implement this protocol exactly, because the client cannot be updated in
lockstep: the iOS app ("WebClaude", `~/src/ios-claude-terminal`) is dev-signed onto a phone
and does not rebuild when this repo changes — a server change lands silently and breaks it
with no error anywhere. Wire compatibility with ttyd 1.7.4 is therefore a hard requirement,
not a preference.

Sibling documents: [`/openapi.json`](/openapi.json) (HTTP surface),
[`compatibility.md`](compatibility.md) (ttyd flag/behavior matrix),
[`session-lifecycle.md`](session-lifecycle.md) (dtach session states).

## Sources and verification legend

Every claim in this document carries one of these tags. If a behavior could not be
traced to a source, it is marked **UNVERIFIED** — implementers must check it empirically
rather than trust it.

| Tag | Meaning |
|---|---|
| **[GT]** | Verified ground truth: extracted from ttyd 1.7.4's own served web client on the live instance, or confirmed by the iOS app working against that instance in production. |
| **[iOS path: anchor]** | The iOS client source, rooted at `~/src/ios-claude-terminal/WebClaude/`. Anchors, not line numbers, for the reason above and one more: CI cannot see that repository at all, so a citation into it can never be resolved here — only its *form* can be checked, and `test/spec-guards.py` rejects a line number. Every one of these was dangling once (#33): the client was renamed `Ttyd*` → `Wtd*` and the line numbers moved with it, invisibly. |
| This repo's own files | Written as the path, a colon, and an **anchor**: a literal fragment of that file — a function name, a variable, a `case` label. Never a line number. Line citations rot on any edit above them, and 29 of them had silently drifted 16-20 lines before `test/spec-guards.py` began checking that every anchor still resolves (#41). |
| **[LAB]** | Empirically verified on this box, 2026-07-24, dtach 0.9. |
| **UNVERIFIED** | Believed but not confirmed. Do not build on it without checking. |

Requirements language: MUST / MUST NOT / SHOULD / MAY per RFC 2119. Normative
requirements bind `wtd`. Statements about ttyd and the clients are descriptive.

## 1. Transport and endpoints

One HTTP listener, one port (default **7681** — it is also the default in the iOS
client's `ServerProfile.port` [iOS Models/ServerProfile.swift: port], so changing it means editing
every saved profile by hand). Plain HTTP and `ws://` — no TLS in v1; the iOS app opens
ATS for plaintext `ws://` on the tailnet, and adding `wss://` is a client-side change
too (see `CLAUDE.md`).

| Path | Purpose |
|---|---|
| `GET /ws` | The terminal WebSocket. Query string is meaningful (section 8). |
| `GET /token` | Auth-token compatibility endpoint; always `{"token": ""}` (section 4). |
| `GET /` | HTML: browser picker without `?arg=`, terminal page with it. See `openapi.yaml`. |

URL construction, as both known clients actually do it:

- ttyd's web client builds the WS URL as
  `proto + "//" + host + pathname(trailing slashes stripped) + "/ws" + location.search`
  — **the full query string is forwarded**, which is how `?arg=` reaches the server. [GT]
- The iOS client builds `ws://host:port[/prefix]/ws` and adds `?arg=<sessionArg>` when a
  profile has one [iOS Models/ServerProfile.swift: webSocketURL]. It builds no `/token` URL:
  the token round trip was removed from that client, so `/token` now has exactly one live
  consumer — `wtd`'s own browser terminal page (§4). [GT]
- `pathPrefix` in the iOS client exists for reverse-proxy deployments
  [iOS Models/ServerProfile.swift: pathPrefix]. `wtd` itself serves at `/` — the proxy must strip the
  prefix (see `compatibility.md`, base-path section). Consequence for `wtd`'s own HTML:
  pages MUST use relative URLs so they still work behind a stripping proxy.

`wtd` MUST bind exactly one resolved address and MUST refuse a wildcard
(`0.0.0.0`, `::`, empty) even when explicitly configured — see `compatibility.md` and
the security section below.

## 2. HTTP upgrade and subprotocol

A standard RFC 6455 upgrade on `GET /ws`. Requirements beyond the RFC:

| Rule | Requirement | Why |
|---|---|---|
| Subprotocol | If the client offers `tty` in `Sec-WebSocket-Protocol`, `wtd` MUST select it and echo `Sec-WebSocket-Protocol: tty` in the 101 response. | ttyd's web client does `new WebSocket(wsUrl, ["tty"])` [GT]; per RFC 6455 a browser **fails the connection** if a requested subprotocol is not echoed. |
| No subprotocol offered | `wtd` MUST accept the upgrade and proceed without selecting one. | This is what ttyd 1.7.4 does — 101, no echo, connection held open [LAB: probed 2026-07-28] — so closing such a connection is a wire-compatibility bug, not leniency. `wtd` did close it (1008) until #36. |
| Other subprotocols only (no `tty`) | `wtd` MUST reject the upgrade with **HTTP 400**, before switching protocols. | The client is speaking something else; handing it a shell stream is wrong. **Deliberate divergence from ttyd**, which drops the TCP connection with no HTTP response at all [LAB: probed 2026-07-28] — a bare reset a client cannot interpret. Nothing depends on matching it: a client offering only other subprotocols is not a ttyd client. |
| Origin | If an `Origin` header is present and its authority (host[:port]) differs from the request's `Host` authority, `wtd` MUST reject the upgrade with HTTP 403 before switching protocols. | See section 12 — this closes cross-site WebSocket shell hijacking. |
| Query string | Preserved and parsed for `arg` (section 8). Unknown query parameters MUST be ignored. | ttyd's client forwards all of `location.search` [GT]; future client-side params must not break old servers. |

`Sec-WebSocket-Protocol` may be repeated or comma-separated; RFC 6455 treats those as
equivalent and `wtd` flattens both. An empty or whitespace-only header counts as *offering
nothing*, not as offering an unknown protocol, so it is accepted — again matching ttyd
[LAB: probed 2026-07-28].

Client quirk worth knowing: the iOS client sets `Sec-WebSocket-Protocol: tty` as a raw
request header rather than through the WebSocket API's protocols parameter
[iOS Networking/WtdConnection.swift: Sec-WebSocket-Protocol]. That means iOS likely does *not* enforce the echo
the way browsers do — but `wtd` MUST echo anyway, because browsers do enforce it.

## 3. Compression and extensions

`wtd` SHOULD NOT negotiate `permessage-deflate` or other extensions in v1 (decline the
offer; negotiation is opt-in, so declining is always legal). Whether ttyd 1.7.4
negotiates it is **UNVERIFIED**; no known client requires it, and terminal output is
latency-sensitive small writes where compression buys little and buys bugs.

## 4. The token dance (`GET /token`)

ttyd without `-c` serves `{"token": ""}` at `/token` [GT], and the value goes in the
handshake's `AuthToken` field.

**No native client performs this round trip any more.** The iOS app used to GET `/token` on
every connect and ignore all failures; that code was deleted when it became wtd-only, saving a
round trip per connect. It still sends `AuthToken: ""`
[iOS Networking/WtdProtocol.swift: Handshake], because the field is part of the handshake, not
because it fetched anything.

`wtd` requirements:

- `GET /token` MUST return HTTP 200 with JSON body `{"token": ""}` — always, because
  `wtd` has no auth (see `compatibility.md`, `-c` row).
- `wtd` MUST NOT require the token dance: the handshake's `AuthToken` is accepted with
  any value, including absent, and is otherwise ignored.

Why keep the endpoint now that the app does not use it: `wtd`'s own terminal page still
fetches it (`cmd/wtd/web/terminal.html`), so it is not vestigial; `server_test.go` pins the body
byte-exact and the conformance suite diffs it against real ttyd, so it is load-bearing for
parity; and third-party
ttyd-compatible clients may be less forgiving.

## 5. Handshake

After the upgrade, the **first WebSocket message from the client** is a JSON object with
no opcode prefix:

```json
{"AuthToken": "", "columns": 80, "rows": 24}
```

Field names are exact: `AuthToken` (capital A, capital T), `columns`, `rows` — the iOS
client encodes precisely this struct [iOS Networking/WtdProtocol.swift: Handshake]; ttyd's web
client sends `JSON.stringify({AuthToken, columns, rows})` [GT].

> ### The frame-type trap — read this twice
>
> **ttyd's own web client sends the handshake as a BINARY frame**
> (`textEncoder.encode(...)`) [GT].
> **The iOS client sends it as a TEXT frame** (`.string(...)`)
> [iOS Networking/WtdConnection.swift: didOpenWithProtocol].
>
> Both work against ttyd 1.7.4 in production, so ttyd accepts either [GT]. A `wtd` that
> checks the WS message type here breaks exactly one of the two clients — and if it's
> the iOS one, the failure is invisible until a phone stops connecting. `wtd` MUST
> accept the handshake as **either** a text or a binary WS message and treat the payload
> identically (UTF-8 JSON bytes).

Handshake handling rules:

| Case | Behavior | Why |
|---|---|---|
| Valid JSON object | Extract fields, spawn the terminal (section 9). | |
| `AuthToken` absent or any string | Ignore the value. | No auth exists; iOS sends `""` even when `/token` failed. |
| `columns`/`rows` absent, or outside **1..65535** | Use defaults **80x25** for the missing/out-of-range dimension; log; continue. | Closing would strand a buggy client with a blank screen and no message anywhere. Lenient-and-log matches the repo's graceful-fallback philosophy (an unusable `?arg=` opens a shell rather than erroring). |
| `columns`/`rows` present but **not a number** (`"80"`, `80.5`, `true`) | Close **1002**. | Not leniency being abandoned — leniency is not available. The handshake is one `json.Unmarshal`, so a type error fails the whole payload, and recovering per field would mean decoding into `json.RawMessage` and coercing each one. Same ruling as JSON `null` in #18: a payload that is not the documented shape is not a handshake. This row previously promised defaults here and the server has always closed (#37). |
| Unknown extra keys | Ignore. | Future clients must be able to add fields without breaking old servers. |
| Payload is not parseable JSON, or not an object | Close **1002** (protocol error). Nothing is spawned. | The peer does not speak this protocol; do not attach a shell to it. |
| No message within **10 s** of the upgrade | Close **1008** with reason `handshake timeout`. | Both real clients send it immediately in their open handler [iOS Networking/WtdConnection.swift: didOpenWithProtocol] [GT]; anything slower is a stuck or hostile peer holding a socket. |
| Handshake message larger than **8 KiB** | Close **1009**. | The legitimate handshake is under 100 bytes. |

The PTY MUST be created with the handshake's `columns`x`rows` **before** the start
command runs, so the terminal renders at the client's real width from the first byte
(a portrait phone is ~40 columns — see `CLAUDE.md`, menu mode).

Immediately after a successful handshake the iOS client sends a duplicate resize frame
[iOS Networking/WtdConnection.swift: didOpenWithProtocol] — dims may have changed while connecting.
`wtd` MUST tolerate resize frames arriving before it has produced any output.

## 6. Framing after the handshake

Every subsequent WebSocket message, in both directions, is:

```
byte 0        : one ASCII opcode character
bytes 1..end  : payload (may be empty)
```

The opcode applies to the complete WebSocket **message**, not to transport fragments —
if the transport fragments a message, reassemble before parsing (standard WS library
behavior; stated here because the opcode-byte scheme makes it tempting to parse frames).

### Client → server

| Opcode | Byte | Payload | Semantics |
|---|---|---|---|
| INPUT | `'0'` (0x30) | raw bytes | Write verbatim to the PTY. [GT] [iOS Networking/WtdProtocol.swift: ClientOp] |
| RESIZE_TERMINAL | `'1'` (0x31) | JSON `{"columns": N, "rows": N}` | Apply to the PTY (TIOCSWINSZ); kernel delivers SIGWINCH. [GT] [iOS Networking/WtdProtocol.swift: ClientOp] |
| PAUSE | `'2'` (0x32) | none | Flow control; see section 11. [GT] |
| RESUME | `'3'` (0x33) | none | Flow control; see section 11. [GT] |

Message-type leniency: post-handshake client messages arrive as binary from both known
clients (iOS wraps everything in `Data` [iOS Networking/WtdProtocol.swift: ClientOp]; ttyd's
web client sends binary [GT]). `wtd` MUST accept binary and SHOULD also accept text
messages by treating the payload as UTF-8 bytes — mirroring the leniency the iOS client
itself applies on receive [iOS Networking/WtdConnection.swift: func receive].

| Malformed input | `wtd` behavior | Why |
|---|---|---|
| Zero-length message (no opcode byte) | Ignore. | iOS's own parser shrugs at empty frames [iOS Networking/WtdProtocol.swift: ServerMessage]; symmetric leniency. |
| Unknown opcode byte | Ignore, log once per connection per opcode. | The iOS client ignores unknown *server* opcodes [iOS Networking/WtdProtocol.swift: case unknown]; a server killing a live shell over one stray byte is strictly worse. Survive, don't drop the connection. |
| RESIZE payload not valid JSON, or `columns`/`rows` missing/non-integer/outside **1..65535** | Ignore the frame, log. | Same rationale. Input keeps flowing; a bad resize must not cost the user their session. Note the asymmetry with the handshake: a malformed RESIZE is ignored because there is already a working terminal to keep, where a malformed handshake has nothing to fall back on. |
| Any message larger than **1 MiB** | Close **1009**. | Memory safety on an unauthenticated port. 1 MiB comfortably covers the largest realistic paste. |

### Server → client

| Opcode | Byte | Payload | Semantics |
|---|---|---|---|
| OUTPUT | `'0'` (0x30) | raw PTY bytes | Client feeds these to its terminal emulator. [GT] [iOS Networking/WtdProtocol.swift: ServerMessage] |
| SET_WINDOW_TITLE | `'1'` (0x31) | UTF-8 string | Client displays it (iOS: `windowTitle` [iOS Networking/WtdConnection.swift: windowTitle]; web: `document.title`). [GT] |
| SET_PREFERENCES | `'2'` (0x32) | JSON | Client options for ttyd's own web UI. iOS ignores it [iOS Networking/WtdProtocol.swift: case preferences]. [GT] |

Server frame rules:

- All server frames MUST be sent as **binary** WS messages. OUTPUT payload is raw bytes
  with no UTF-8 guarantee — a multi-byte character can split across two frames — and
  RFC 6455 requires text frames to be valid UTF-8, so a text OUTPUT frame is a protocol
  violation that browsers will kill the connection over. Both clients set
  `binaryType = "arraybuffer"` / handle `Data` [GT] [iOS Networking/WtdConnection.swift: func receive].
- `wtd` MUST NOT transform, validate, or re-chunk OUTPUT on UTF-8 boundaries — pass raw
  bytes in order. The emulator (xterm.js, SwiftTerm) owns decoding.
- `wtd` MAY coalesce consecutive PTY reads into one OUTPUT frame but MUST NOT reorder
  bytes and MUST NOT hold output on a timer beyond single-digit milliseconds — this is
  an interactive terminal, and the blank-screen redraw kick (section 7) depends on
  prompt delivery.
- After a successful handshake `wtd` MUST send one SET_WINDOW_TITLE frame and SHOULD
  send one SET_PREFERENCES frame with payload `{}`. Title payload: `<arg> (<hostname>)` for a
  named connection, `<start-command> (<hostname>)` for an argless one
  [cmd/wtd/ws.go: runHubTerminal], and matches `compatibility.md` section 4.
  Nothing parses the title — iOS
  displays it verbatim, the web page puts it in `document.title` — so the format is
  free, but the frame's existence is expected. ttyd 1.7.4's exact title string and its
  title/preferences ordering are **UNVERIFIED**; no known client is order-sensitive, so
  `wtd` fixes the order as title-then-preferences.

## 7. Resize semantics — no coalescing, ever

`wtd` MUST apply every RESIZE frame to the PTY immediately, in arrival order, one
`TIOCSWINSZ` per frame, even when consecutive frames are redundant or net to zero.

This is load-bearing, not pedantry. A server that debounces or coalesces resizes would
collapse a `rows-1, rows` pair into a net-zero change, no SIGWINCH would fire, and
blank-on-attach would regress with no error anywhere on either side.

That jiggle used to be the client's job on both clients, and was the visible half of a
two-sided workaround for dtach keeping no screen buffer. It is now the **server's**, once
per session: `wtd` performs it when a hub first attaches (`hub.kick`, `rows-1` at t+0.4 s,
the real size 0.15 s after that — the deltas the iOS client proved), so the replay buffer
starts with a painted screen in it. `wtd` also passes `dtach ... -r winch`
[cmd/wtd/attach.go: func dtachArgs]; that fires on the hub's single attach and costs nothing.

Consequences for clients:

- A client **SHOULD NOT** send a redraw kick of its own. It is redundant now, and with
  multiple clients on one session it repaints everyone's view, not just the sender's.
- A client that still sends one is harmless and stays supported — which matters, because
  installed iOS builds do not update when this server does
  [iOS Networking/WtdConnection.swift: scheduleRedrawKick]. Removing the iOS half is a separate change in
  that repo and MUST NOT be done before this server side is deployed.

Debouncing interactive resizes is still the **client's** job and already happens there: the
iOS client debounces at 100 ms [iOS Networking/WtdConnection.swift: resizeDebounceTimer]. The server's job is
to be faithful.

With several clients on one session the window size is **last writer wins**, matching what
dtach itself does with two attached clients: there is one PTY and one size. A joining
client's handshake dimensions are applied on attach, so the newest arrival wins by default.

Values are validated per section 6 (integers 1..65535); anything else means the frame is
ignored. The server's own kick is guarded to stay >= 1 row.

## 7a. Replay on attach (`scrollback-replay`)

Numbered 7a rather than 8 on purpose: every cross-reference in this document and in the iOS
client is by section number, and renumbering nine sections to insert one is how a spec and an
implementation quietly stop matching.

On a **named** connection, after the title and preferences frames, `wtd` sends the session's
recent output as ordinary OUTPUT frames, then continues with live output. dtach keeps no
screen buffer of its own — a second client attaching to a live session was measured receiving
**64 bytes**, none of it what the first client had seen — so this buffer exists only because
`wtd` holds the attachment and remembers the tail.

Guarantees:

- Replayed bytes are exactly what the session produced, in order, with **no synthetic
  sequences** — no clear-screen, no reset, no markers. A client cannot tell replay from live
  output and MUST NOT try; there is no frame, opcode, or delimiter that separates them.
- The seam is exact: no byte is sent twice and none is skipped. The snapshot and the client's
  registration happen atomically with respect to the output pump.
- Replay is chunked like live output (16 KiB frames), so no client sees a frame shape from
  replay that a busy session would not also produce.
- The **head** of a replay may begin mid-line, but is trimmed forward to an escape-sequence
  boundary rather than cut at an arbitrary offset — a truncated CSI would otherwise make the
  emulator eat the bytes that follow it. Two documented escape hatches keep that from being
  absolute: an unterminated string sequence resynchronizes after 4096 bytes, and if no
  boundary was recorded in the whole retained window the cut falls back to an arbitrary
  offset. Both bound memory at the cost of one garbled head, which is the pre-replay
  behavior anyway.
- The buffer is in memory and per session. `WT_REPLAY_BYTES` (default 256 KiB, `0` disables
  replay entirely) is the history target; up to 1.25x that is actually retained, because
  trimming is amortized rather than run on every write. Restarting `wtd` loses every buffer
  and **no** sessions.

Two things trigger the one-time redraw, not one, and the second is what makes the feature
safe to detect on:

- The first client of a fresh hub, so a session that has produced nothing still paints.
- **Every attach whose replay snapshot comes back empty** — including every attach at all when
  an operator has set `WT_REPLAY_BYTES=0` [cmd/wtd/hub.go: len(replay) == 0].

That second trigger is why a client may skip its own size-jiggle on **any** server advertising
`scrollback-replay`, rather than only on one with replay actually enabled. Without it, a client
that trusted the feature flag on a server with replay switched off would attach to a blank
screen and wait forever. Reported in #19: the guarantee was real but only discoverable by
reading the Go source, and a client cannot read the Go source.

Client responsibilities:

- Feature-detect via `scrollback-replay` in `GET /api/v1/meta`. A server without it behaves
  as before, so a client MUST NOT depend on replay to render a usable screen.
- A client that reconnects into a terminal emulator it kept alive will see its last screen
  again, since replay overlaps what it already had. Whether to clear before reconnecting is
  the **client's** decision — the server does not transform OUTPUT (section 6) and will not
  inject a clear on its behalf.
- Replay covers what happened *before* attaching. Scrollback while connected remains the
  client's own buffer (xterm.js, SwiftTerm). They are different mechanisms and both are
  needed.
- **Replay is rendered at whatever width the session was using when the bytes were produced**,
  not at the attaching client's width. The snapshot is taken before the joining client's size
  is applied, so a 40-column phone joining a session a 120-column browser was driving receives
  a replay hard-wrapped for 120 columns. A full-screen program repaints itself once the
  SIGWINCH lands; for a plain shell the wrapped history simply stays wrapped. This is an
  accepted consequence of replaying raw bytes instead of a rendered screen, and it is the
  first thing a user with both a phone and a laptop will notice.

## 8. `?arg=` → argv

ttyd's `-a`/`--url-arg` appends each `arg` query value to the start command's argv;
`wtd` has no such flag — the behavior is **always on** (see `compatibility.md`). This is
the deep-link hot path: an iOS profile with `sessionArg` connects to
`ws://host:7681/ws?arg=<name>`, which must become `wt <name>` so the app's
auto-reconnect rejoins its session unattended (`CLAUDE.md`, deep-link mode).

Rules:

- Each `arg` query parameter's value, URL-decoded, is appended to the argv in order of
  appearance. Multiple `arg` values are ttyd's documented behavior (**UNVERIFIED** empirically
  against 1.7.4); only the first selects a session, so extras are inert today. They are still
  carried, because an external `-start-command` may read them. [GT for the single-arg path]
- An **empty** value (`?arg=`) MUST be treated as "no session", the same as no `arg` at all
  [cmd/wtd/attach.go: named :=]. It still produces an (empty) argv element for an external start
  command, preserving ttyd's shape.
- Query parameters other than `arg` MUST be ignored (the web client forwards all of
  `location.search` [GT]).
- Session-name policy lives in exactly one place — `validateAttachName`
  [cmd/wtd/attach.go: func validateAttachName], which refuses a name containing `/` or `..`, or
  one whose socket path would exceed the 107-byte ceiling, and degrades to a plain shell instead
  of erroring. It is deliberately **looser** than the create-side rules: a deep link must be able
  to reach a session `POST /api/v1/sessions` would not have created. One implementation, shared
  with `POST` for the socket-path ceiling — the divergence that used to exist between a bash
  check and a Go one is what #16 was about.
- A client on a bad URL gets a working terminal, never an error; preserve that.
- Transport-safety floor (things that cannot be carried as a process argument at all): a value
  containing a NUL byte, or a value longer than 4096 bytes. On violation `wtd` MUST drop the
  offending value(s) and continue — never close the connection. Implemented in `filterArgs`
  [cmd/wtd/ws.go], applied **before** hub selection (#17).
- A client MUST pass the session name through **untransformed** from whatever listed it. The API
  lists byte-exact and the deep-link path accepts names `POST /api/v1/sessions` refuses, so an
  interior space is legitimate; trimming one selects a different session. The iOS client shipped
  exactly that bug and fixed it client-side (their PR #6). Leading and trailing whitespace **can**
  occur: nothing between the URL and the socket name trims it, so `?arg=%20foo` creates and
  attaches to a session whose name begins with a space. This document previously claimed the
  opposite, on the strength of the bash menu's `read -r`; that only ever applied to names typed at
  the menu, never to a deep link, and the menu is gone.
- Separately, at most the first **16 usable** values are passed on and the rest are ignored.
  The count is taken *after* the floor removes unusable values, so one NUL does not cost a
  usable value its place — "more than 16 `arg` values" is not a positional rule, and stating
  it as one is wrong in the mixed case.
- The floor and `validateAttachName` degrade differently, and §9's two connection shapes are
  why: a name rejected by `validateAttachName` leaves the *arg present*, so the connection is
  still named and shared and every client on it sees one `wtd:` line explaining the fallback,
  while the floor **discards** the value, so a connection whose only `arg` was dropped is argless
  — private shell, no replay, no explanation, process dies with the connection. Both are
  graceful; only the floor loses the name. This is observable to a client only when a second one
  deep-links the same value.
- Dropping is also what makes NUL safe as `hubKey`'s separator [cmd/wtd/hub.go]. Before the
  floor existed, `?arg=a%00b` keyed the same hub as `?arg=a&arg=b` — one pty and one replay
  ring shared between two unrelated connections, reachable from a URL. A future change that
  weakens `filterArgs` MUST change `hubKey` in the same commit.

## 9. Connection lifecycle

There are **two connection shapes**, and which one you get is decided by `?arg=` alone.
Everything on the wire is identical between them; the difference is what owns the process.

| | Argless (`/ws`) — plain shell | Named (`/ws?arg=<name>`) — deep link |
|---|---|---|
| Process | One fresh `bash` on its own PTY, per connection | One `dtach -A` per session name, **held** across connections (a *hub*) |
| Replay on attach | None — there is no prior state to replay | Recent output, then live (section 7a) |
| Second client | Gets its own separate shell | Joins the same session: same PTY, same output, interleaved input |
| Client closes | Process group is signalled and reaped | Client unsubscribes; **the process stays** |
| Lifetime | The connection | Until the session exits, the warm cap evicts it, or `wtd` stops |

Why argless connections are not shared: there is no session name to key a buffer under, and
two clients sharing one anonymous shell would interleave their keystrokes to no purpose. An
**empty** `?arg=` counts as argless (section 8). An argless connection is not a way to browse
sessions — it has no session behind it and nothing to reattach to. `GET /api/v1/sessions`
lists; `?arg=<name>` opens.

Session persistence lives entirely in dtach. A hub holds an *attachment*, not a session:
restarting `wtd` drops every client and every replay buffer and leaves all sessions running,
which is the property the whole design rests on. This is why `wtd` runs `dtach -A` rather than
owning the pty itself — the dtach master daemonizes out of `wtd`'s process group, so it is
independent of the server's lifetime.

Spawn requirements (both shapes):

- New session and process group (`setsid`), PTY slave as controlling terminal, stdin/
  stdout/stderr on the PTY — the standard terminal spawn. The process group matters for
  cleanup: the dtach *client* `wtd` spawns is the process group leader, so its pgid equals its
  pid; the dtach *masters* are outside it (they daemonize into their own sessions —
  structurally why sessions survive disconnects, confirmed by masters outliving their launcher
  [LAB]). Both facts are asserted from `/proc`
  [cmd/wtd/builtin_test.go: func hubChildPGID], because teardown signals `-pgid` and a wrapper
  process between `wtd` and dtach would break detach silently.
- Environment: the service user's environment, plus `TERM=xterm-256color` (both clients
  are xterm-family emulators; ttyd's default TERM is **UNVERIFIED** but this value is
  what the fleet already runs), plus `WT=1` so a login shell can tell it is already inside a
  web session and skip launching a multiplexer [cmd/wtd/attach.go: func fallbackShell]. `WT_DIR`
  and `WT_PROJECTS` are no longer passed to a child at all: `wtd` reads them itself, from
  `-session-dir`/`-projects-file` or the environment [cmd/wtd/config.go: func (s *server) sessionDir].
  They travel as flags because the environment hop was a silent-failure class — a config key
  that reached nothing, with no error anywhere, twice (`WT_PROJECTS`, then `WT_DIR` in #28).
- Initial window size: from the handshake, before exec (section 5).

### State machine

```
 upgrade ok            valid handshake                child exit / PTY EOF
────────────▶ AWAITING_HANDSHAKE ────────────▶ RUNNING ────────────────────▶ CLOSING ──▶ CLOSED
                    │                             │                            ▲
                    │ bad handshake / timeout     │ client close / TCP error   │
                    └────────────▶ CLOSED         └────────────────────────────┘
                                                     (kill process group)
```

| State | Event | Action | Next |
|---|---|---|---|
| — | `GET /ws`, upgrade valid (section 2) | Send 101, echo `tty` if offered | AWAITING_HANDSHAKE |
| AWAITING_HANDSHAKE | Valid handshake (text **or** binary) | Create PTY at columns x rows; spawn start command + args from `?arg=`; send title frame; send preferences frame | RUNNING |
| AWAITING_HANDSHAKE | Unparseable handshake | Close 1002 | CLOSED |
| AWAITING_HANDSHAKE | 10 s timeout | Close 1008 | CLOSED |
| AWAITING_HANDSHAKE | Message > 8 KiB | Close 1009 | CLOSED |
| RUNNING | INPUT | Write payload to PTY | RUNNING |
| RUNNING | RESIZE (valid) | TIOCSWINSZ now, in order (section 7) | RUNNING |
| RUNNING | RESIZE (malformed) / unknown opcode / empty message | Ignore, log | RUNNING |
| RUNNING | PAUSE / RESUME | Accept; v1 no-op (section 11) | RUNNING |
| RUNNING | Message > 1 MiB | Close 1009. **Argless:** kill the process group. **Named:** drop that client only — the session and every other client are unaffected | CLOSING |
| RUNNING | PTY readable | Send OUTPUT frame(s) | RUNNING |
| RUNNING | Child exits or PTY EOF | Flush remaining output, then Close 1000 | CLOSING |
| RUNNING | PTY write fails (child gone) | Treat as child exit | CLOSING |
| RUNNING | Client Close frame / TCP error / liveness timeout (section 10) | **Argless:** reply/close; SIGHUP the process group, escalate SIGTERM after 2 s, SIGKILL after 5 s; reap. **Named:** unsubscribe only — the hub keeps its attachment and its buffer | CLOSED |
| RUNNING | Client stops draining output past the backlog budget (4 MiB) | Named only: drop that client and close its connection; the session and every other client are unaffected | CLOSED |
| RUNNING | Spawn already failed (race) — see failure table | | |
| any | `wtd` shutdown (SIGTERM from systemd) | Close every **named** connection 1001; argless connections drop with no close frame (see §14); SIGHUP each process group, hubs included; wait bounded (5 s); exit | CLOSED |

For a named connection, "child exits or PTY EOF" is the hub's held start command ending —
the session was killed, its shell exited, or a client sent Ctrl-`\` and detached it. That
closes **every** client on that session, which is a deliberate consequence of sharing one
attachment: with a single client, indistinguishable from before; with two, detaching is a
shared-fate action.

Why SIGHUP first: it is what a real terminal hangup delivers. bash exits, the dtach
client exits, and the dtach masters — outside the process group, in their own sessions —
are untouched, which is the entire persistence model. Killing the process group (not
just the direct child) also catches background jobs a user started from an argless shell.
What signal ttyd 1.7.4 sends on disconnect is **UNVERIFIED**; this is
`wtd`'s defined behavior, chosen to preserve the observable contract (dtach sessions
survive, per-connection processes do not).

On server shutdown the dtach sessions survive and are reattachable after restart — that
is the restart contract `systemctl restart wt.service` already has today (`CLAUDE.md`,
deploying section).

## 10. Ping, pong, and liveness

- `wtd` MUST answer WS ping frames with pongs carrying the same payload (RFC 6455
  obligation, but stated because the iOS client's health depends on it: it pings every
  **20 s** and closes on ping failure [iOS Networking/WtdConnection.swift: pingTimer]).
- `wtd` MUST NOT impose any idle/read timeout shorter than **60 s**, so the 20 s client
  ping always lands with margin.
- `wtd` MUST NOT rely on client pings for liveness: **browser JavaScript cannot send WS
  ping frames at all** — ttyd's web client has no ping — so a "no ping means dead"
  policy would disconnect every browser user.
- For its own dead-peer detection `wtd` SHOULD send a server ping every 30 s and close a
  connection only after **three consecutive pings go unanswered** — 90 s of tolerance, and
  a single lost pong costs nothing. Endpoints auto-pong at the transport layer (browsers and
  URLSession both), so a healthy idle client always passes. Why bother: a phone that vanished
  mid-connection (elevator, dead battery) otherwise leaves a `wt` process and a dtach client
  attached forever, pinning the session's `attached` state (see `session-lifecycle.md`).
- The signal is the **pong specifically**, not inbound traffic in general. This document
  previously said "no inbound traffic of any kind (pong, data, or close)", which promised more
  leniency than `keepAlive` gives: a peer sending INPUT but suppressing pongs is still reaped.
  Tracking last-read time across both pumps would make the looser statement true, and is
  deliberately not done — no real client is in that position, since auto-pong is a transport
  obligation, and it would add shared state between goroutines to cover a client that is
  already violating RFC 6455.
- A failed ping on an already-broken socket ends the connection immediately rather than
  consuming the remaining budget: no later probe on a dead socket can succeed, so waiting
  would only delay the teardown that frees the session's `attached` state.
- Unsolicited pongs MUST be ignored (RFC 6455 permits them).

The iOS app backgrounds with ~30 s of socket grace, then the socket dies and the app
auto-reconnects on foreground [iOS Networking/WtdConnection.swift: backgroundTaskID]. The 90 s reap
never races that: the dead socket produces a TCP error server-side first, and even when
it does not, 90 s > the grace window.

## 11. Flow control (PAUSE / RESUME)

Both opcodes exist on the wire [GT], and **neither known client sends them today**: the
iOS client defines them and marks them "unused in v1"
[iOS Networking/WtdProtocol.swift: case pause], and the production fleet runs without any client
issuing them [GT].

`wtd` v1 policy:

- MUST parse and accept `'2'` and `'3'` without error — never close, never log-spam.
- MAY treat them as no-ops. This is safe because real backpressure already exists one
  layer down, though it differs by connection shape (section 9). On an **argless**
  connection: when the WS write buffer to a slow client exceeds a threshold, `wtd` MUST
  stop reading the PTY, at which point the foreground process blocks writing to the
  PTY — exactly the behavior of a real terminal with output stopped. On a **named**
  connection the hub MUST NOT stop reading: one slow client would stall the session for
  every other client attached to it, and apply backpressure all the way into the shell.
  A named client past the backlog budget is dropped instead, and replays on reconnect.
  Bounded memory either way, no protocol needed.
- If a future client needs explicit flow control, the semantics are: PAUSE = stop
  reading the PTY, RESUME = start again. Advertise it as a `flow-control` feature in
  `GET /api/v1/meta` before any client depends on it (`openapi.yaml`, features
  registry) — the meta features list is the anti-skew mechanism between these two repos.

## 12. Security at the protocol layer

The threat model (see `CLAUDE.md` and the README): a **writable, unauthenticated shell**
as the service user, protected only by which interface it binds to. Every rule here
answers one question — *does this widen who can reach the shell?*

- **Origin check on the upgrade (section 2).** Browsers apply no same-origin policy to
  WebSockets: any web page the user visits can open `ws://<tailnet-ip>:7681/ws` with
  subprotocol `tty`, complete the trivial handshake, and own a shell. The `Origin`
  header is the only signal, so: `Origin` present and authority != request `Host`
  authority → HTTP 403, no upgrade. This breaks neither client — `wtd`'s own pages are
  same-origin, and native URLSession sends no `Origin` header. **Confirmed by observation**
  rather than packet capture: `POST`, `DELETE` and the `/ws` upgrade all succeed from the real
  app against a server that 403s on an authority mismatch, which is only possible if no
  `Origin` is sent (reported in #19, verified across create, delete and attach). The
  `-allow-cross-origin` flag stays as the escape hatch if a future OS release changes that.
  Reverse proxies MUST preserve the original `Host` (e.g. nginx
  `proxy_set_header Host $host`) or the check rejects legitimate browsers.
  Known limit: DNS rebinding defeats an Origin-vs-Host comparison (both headers end up
  naming the attacker's hostname and match), but requires the victim's browser to be
  on the private network already — see the residual-exposure note in `openapi.yaml`
  for the full analysis and the reserved `WT_ALLOWED_HOSTS` fix.
- **Writable is not optional.** ttyd's `-W` is hardcoded on. Its absence has a uniquely
  cruel failure mode — input is *silently dropped*, the terminal looks fine, keystrokes
  do nothing (`CLAUDE.md`, client-contract table) — so `wtd` refuses to have the knob.
- **Session-name policy stays in one place** — `validateAttachName` (section 8). Names are
  untrusted network input; the `/` and `..` rejection
  [cmd/wtd/attach.go: func validateAttachName] and the single-quoting of the working directory
  [cmd/wtd/sessionops.go: func shellQuote] are
  the enforcement point. `wtd` adds only transport-safety limits, never a second,
  divergent policy.
- **Size and time limits** (sections 5, 6, 10) exist because every byte arrives
  pre-auth. There is deliberately no connection-count limit (ttyd `-m` dropped, see
  `compatibility.md`): a resource-exhaustion DoS from inside the tailnet is accepted by
  the threat model, and a limit would add a way for one stuck phone to lock out the
  owner.

## 13. Close codes

| Code | `wtd` sends it when | What a client should do |
|---|---|---|
| 1000 normal closure | The terminal's process exited (shell exit, `exit` at an argless prompt, PTY EOF), output flushed first. On a named connection this is the *hub's* dtach client ending, so every client on that session gets it. | Treat as final. Reconnect only on user action — a reconnect re-attaches to the deep-linked session, or opens a fresh shell if argless; safe but possibly not what the user wanted. |
| 1001 going away | `wtd` is shutting down (systemd stop/restart), or a warm hub was released by the cap (section 9). | Auto-reconnect with backoff. dtach sessions survive the restart; a deep-link profile rejoins its session unattended. |
| 1013 try again later | A named client fell more than 4 MiB behind on output and was dropped rather than being allowed to stall the session for other clients (section 9). | Reconnect immediately. The session is healthy and the replay buffer restores context. |
| 1002 protocol error | First message was not a parseable handshake (section 5). | Do not auto-retry; this is a client bug or a non-tty peer. |
| 1008 policy violation | Handshake timeout. | Fix the client; do not blind-retry. |
| 1009 message too big | Client message exceeded 8 KiB (handshake) / 1 MiB (after). | Client bug; retrying the same payload will fail the same way. |
| 1011 internal error | PTY allocation or spawn failed, or an unexpected server fault. `wtd` sends the title and preferences frames and then an OUTPUT frame carrying a one-line human-readable error, so the failure is visible *in the terminal* rather than only in a code. The frames come first even though nothing follows them, because a client that treats frame 1 as its window title would otherwise show the error there and never print it. | Retry with backoff is reasonable, but show the message: a misconfigured `WT_PICKER` fails identically every time, and the text is the only thing that says so. |

Reality check: the iOS client **does** branch on these codes. `shouldRetry`
[iOS Networking/WtdConnection.swift: shouldRetry] treats 1000, 1002, 1008 and 1009 as final and
retries everything else with backoff — it implements the dispositions in the table above. This
paragraph used to say the opposite, which was true before that client became wtd-only; the table
is load-bearing now, not aspirational.

Two traps for anyone else implementing it, both learned the hard way in that client:

- **A missing code is not a benign default.** On iOS a failed `receive()` surfaces a server close
  *before* the `didCloseWith` delegate does, and reports no code with it. Trusting the delegate
  meant a shell `exit` — 1000, "treat as final" — arrived codeless, fell through to the retry
  ladder, and **silently recreated the session the user had just exited**. Read the code off the
  task in the receive-failure path, not only from the delegate.
- **1013 is not representable on every platform.** `URLSessionWebSocketTask.CloseCode` has no case
  for it, so an iOS client cannot distinguish 1013 from a transport drop. Defaulting every
  unrecognised *or absent* code to retry-with-backoff lands on this table's intent anyway, which is
  why that is the recommended fallback rather than a special case per code.

ttyd 1.7.4's own close-code usage is **UNVERIFIED**; this table is `wtd`'s definition,
constrained to standard codes so any RFC 6455 client interprets them sensibly.

## 14. Failure modes, enumerated

| Failure | `wtd` behavior | Section |
|---|---|---|
| Start command exits (any status) | Flush output, close 1000. Exit status is logged, not transmitted (no frame exists for it). | 9, 13 |
| PTY EOF without process exit (rare) | Same as exit: flush, 1000, reap. | 9 |
| Spawn fails (missing `WT_PICKER`, PTY exhaustion) | Title and preferences frames, then an OUTPUT frame carrying a one-line error, then close **1011**. Both the private and the hub path, and on the hub path the failure happens inside `join` before any subscriber exists, so the close comes from the connection handler rather than the hub. | 13 |
| Malformed handshake | Close 1002; nothing spawned. | 5 |
| Handshake timeout | Close 1008. | 5 |
| Malformed RESIZE / unknown opcode / empty message | Ignore and log; connection lives. | 6 |
| Oversized message | Close 1009. **Argless:** kill the process group. **Named:** drop that client only; the session keeps running for everyone else, so reconnecting lands back in it with replay. | 6 |
| Client vanishes (TCP error, liveness reap) | **Argless:** SIGHUP process group → SIGTERM 2 s → SIGKILL 5 s; dtach masters unaffected. **Named:** unsubscribe only — the hub keeps its attachment and its buffer, which is what makes the next attach show context. | 9, 10 |
| Named client stops draining output (> 4 MiB behind) | Drop that client, close 1013. The session and every other client are unaffected. | 9, 13 |
| Server shutdown | **Named:** close 1001. **Argless:** the connection drops with no close frame — there is no registry of argless connections to iterate, deliberately, since they own nothing shared. Treat a codeless drop as reconnect-with-backoff, which is required anyway: a SIGKILLed server sends nothing to anybody. Then SIGHUP groups; bounded wait; exit. Sessions survive. | 9 |
| PAUSE/RESUME received | Accepted, no-op in v1. | 11 |
| Cross-origin upgrade attempt | HTTP 403, never upgraded. | 2, 12 |
| Bad/absent `?arg=` | Dropped value → a plain shell. Never a connection error. | 8 |

## 15. Limits summary

| Limit | Value | Bound by |
|---|---|---|
| Handshake deadline | 10 s | section 5 |
| Handshake columns/rows | integers 1..65535, else 80x25 | section 5 |
| Handshake max size | 8 KiB | section 5 |
| Post-handshake message max size | 1 MiB | section 6 |
| RESIZE columns/rows | integers 1..65535 | section 6 |
| `arg` values: max count / max length | 16 / 4096 bytes, drop-and-continue | section 8 |
| Idle timeout floor | never below 60 s; reap after 3 unanswered pings (90 s) | section 10 |
| Server ping interval | 30 s | section 10 |
| Kill escalation | SIGHUP → SIGTERM +2 s → SIGKILL +5 s | section 9 |
| Replay buffer per session | `WT_REPLAY_BYTES`, default 256 KiB (`0` disables); up to 1.25x that is retained, see section 7a | section 7a |
| Named client output backlog before drop | 4 MiB | section 9 |
| Warm hubs (held with no client) | 32, least-recently-idle released first, enforced when a hub is created. A compile-time backstop, deliberately neither a flag nor a config key | section 9 |
