# wtd WebSocket protocol

The ttyd wire protocol, written down properly for the first time. Until now it existed
only in ttyd's C source and, partially re-derived, in the iOS client's Swift. Both repos
have guessed at it independently; this document is the contract that ends the guessing.

`wtd` MUST implement this protocol exactly, because the installed base cannot be updated
in lockstep: the iOS app ("WebClaude", `~/src/ios-claude-terminal`) is dev-signed and
installed on beta users' phones — a server change lands silently and breaks a phone with
no error anywhere. Wire compatibility with ttyd 1.7.4 is therefore a hard requirement,
not a preference.

Sibling documents: [`openapi.yaml`](openapi.yaml) (HTTP surface),
[`compatibility.md`](compatibility.md) (ttyd flag/behavior matrix),
[`session-lifecycle.md`](session-lifecycle.md) (dtach session states).

## Sources and verification legend

Every claim in this document carries one of these tags. If a behavior could not be
traced to a source, it is marked **UNVERIFIED** — implementers must check it empirically
rather than trust it.

| Tag | Meaning |
|---|---|
| **[GT]** | Verified ground truth: extracted from ttyd 1.7.4's own served web client on the live instance, or confirmed by the iOS app working against that instance in production. |
| **[iOS path:line]** | The iOS client source, rooted at `~/src/ios-claude-terminal/WebClaude/`. |
| **[bin/wt:line]**, **[bin/wt-serve:line]** | This repo's scripts. |
| **[LAB]** | Empirically verified on this box, 2026-07-24, dtach 0.9. |
| **UNVERIFIED** | Believed but not confirmed. Do not build on it without checking. |

Requirements language: MUST / MUST NOT / SHOULD / MAY per RFC 2119. Normative
requirements bind `wtd`. Statements about ttyd and the clients are descriptive.

## 1. Transport and endpoints

One HTTP listener, one port (default **7681** — it is also the default in the iOS
client's `ServerProfile.port` [Models/ServerProfile.swift:7], so changing it means every
beta user edits their profile). Plain HTTP and `ws://` — no TLS in v1; the iOS app opens
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
  profile has one [Models/ServerProfile.swift:27-38]. The token URL is
  `http://host:port[/prefix]/token` with **no** query string
  [Models/ServerProfile.swift:40-42]. [GT]
- `pathPrefix` in the iOS client exists for reverse-proxy deployments
  [Models/ServerProfile.swift:9]. `wtd` itself serves at `/` — the proxy must strip the
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
| No subprotocol offered | `wtd` SHOULD accept the upgrade and proceed without selecting one. | Lenient toward hand-rolled clients; both known clients do offer `tty`. |
| Other subprotocols only (no `tty`) | `wtd` MUST reject the upgrade (HTTP 400). | The client is speaking something else; handing it a shell stream is wrong. |
| Origin | If an `Origin` header is present and its authority (host[:port]) differs from the request's `Host` authority, `wtd` MUST reject the upgrade with HTTP 403 before switching protocols. | See section 12 — this closes cross-site WebSocket shell hijacking. |
| Query string | Preserved and parsed for `arg` (section 8). Unknown query parameters MUST be ignored. | ttyd's client forwards all of `location.search` [GT]; future client-side params must not break old servers. |

Client quirk worth knowing: the iOS client sets `Sec-WebSocket-Protocol: tty` as a raw
request header rather than through the WebSocket API's protocols parameter
[Networking/TtydConnection.swift:115]. That means iOS likely does *not* enforce the echo
the way browsers do — but `wtd` MUST echo anyway, because browsers do enforce it.

## 3. Compression and extensions

`wtd` SHOULD NOT negotiate `permessage-deflate` or other extensions in v1 (decline the
offer; negotiation is opt-in, so declining is always legal). Whether ttyd 1.7.4
negotiates it is **UNVERIFIED**; no known client requires it, and terminal output is
latency-sensitive small writes where compression buys little and buys bugs.

## 4. The token dance (`GET /token`)

ttyd without `-c` serves `{"token": ""}` at `/token` [GT]. The iOS client GETs it on
**every** connect, **ignores all failures**, and falls back to an empty string
[Networking/TtydConnection.swift:174-190]. The token value is then placed in the
handshake's `AuthToken` field.

`wtd` requirements:

- `GET /token` MUST return HTTP 200 with JSON body `{"token": ""}` — always, because
  `wtd` has no auth (see `compatibility.md`, `-c` row).
- `wtd` MUST NOT require the token dance: the handshake's `AuthToken` is accepted with
  any value, including absent, and is otherwise ignored.

Why keep a vestigial endpoint at all: the iOS client GETs it unconditionally, and while
it tolerates failure today, a 404 in every connect log is noise, and third-party
ttyd-compatible clients may be less forgiving.

## 5. Handshake

After the upgrade, the **first WebSocket message from the client** is a JSON object with
no opcode prefix:

```json
{"AuthToken": "", "columns": 80, "rows": 24}
```

Field names are exact: `AuthToken` (capital A, capital T), `columns`, `rows` — the iOS
client encodes precisely this struct [Networking/TtydProtocol.swift:21-33]; ttyd's web
client sends `JSON.stringify({AuthToken, columns, rows})` [GT].

> ### The frame-type trap — read this twice
>
> **ttyd's own web client sends the handshake as a BINARY frame**
> (`textEncoder.encode(...)`) [GT].
> **The iOS client sends it as a TEXT frame** (`.string(...)`)
> [Networking/TtydConnection.swift:295, and the comment above it].
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
| `columns`/`rows` absent, non-integer, or outside 1..9999 | Use defaults 80x24 for the missing/invalid dimension; log; continue. | Closing would strand a buggy client with a blank screen and no message anywhere. Lenient-and-log matches the repo's graceful-fallback philosophy (`bin/wt:44` drops a malformed arg and shows the picker rather than erroring). |
| Unknown extra keys | Ignore. | Future clients must be able to add fields without breaking old servers. |
| Payload is not parseable JSON, or not an object | Close **1002** (protocol error). Nothing is spawned. | The peer does not speak this protocol; do not attach a shell to it. |
| No message within **10 s** of the upgrade | Close **1008** with reason `handshake timeout`. | Both real clients send it immediately in their open handler [Networking/TtydConnection.swift:288-302] [GT]; anything slower is a stuck or hostile peer holding a socket. |
| Handshake message larger than **8 KiB** | Close **1009**. | The legitimate handshake is under 100 bytes. |

The PTY MUST be created with the handshake's `columns`x`rows` **before** the start
command runs, so `bin/wt`'s menu renders at the client's real width from the first byte
(a portrait phone is ~40 columns — see `CLAUDE.md`, menu mode).

Immediately after a successful handshake the iOS client sends a duplicate resize frame
[Networking/TtydConnection.swift:297-298] — dims may have changed while connecting.
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
| INPUT | `'0'` (0x30) | raw bytes | Write verbatim to the PTY. [GT] [Networking/TtydProtocol.swift:35-40] |
| RESIZE_TERMINAL | `'1'` (0x31) | JSON `{"columns": N, "rows": N}` | Apply to the PTY (TIOCSWINSZ); kernel delivers SIGWINCH. [GT] [Networking/TtydProtocol.swift:42-46] |
| PAUSE | `'2'` (0x32) | none | Flow control; see section 11. [GT] |
| RESUME | `'3'` (0x33) | none | Flow control; see section 11. [GT] |

Message-type leniency: post-handshake client messages arrive as binary from both known
clients (iOS wraps everything in `Data` [Networking/TtydProtocol.swift:35-46]; ttyd's
web client sends binary [GT]). `wtd` MUST accept binary and SHOULD also accept text
messages by treating the payload as UTF-8 bytes — mirroring the leniency the iOS client
itself applies on receive [Networking/TtydConnection.swift:209-214].

| Malformed input | `wtd` behavior | Why |
|---|---|---|
| Zero-length message (no opcode byte) | Ignore. | iOS's own parser shrugs at empty frames [Networking/TtydProtocol.swift:49]; symmetric leniency. |
| Unknown opcode byte | Ignore, log once per connection per opcode. | The iOS client ignores unknown *server* opcodes [Networking/TtydProtocol.swift:55]; a server killing a live shell over one stray byte is strictly worse. Same philosophy as `bin/wt` omitting `set -e` (`bin/wt:15-16`): survive, don't drop the connection. |
| RESIZE payload not valid JSON, or `columns`/`rows` missing/non-integer/outside 1..9999 | Ignore the frame, log. | Same rationale. Input keeps flowing; a bad resize must not cost the user their session. |
| Any message larger than **1 MiB** | Close **1009**. | Memory safety on an unauthenticated port. 1 MiB comfortably covers the largest realistic paste. |

### Server → client

| Opcode | Byte | Payload | Semantics |
|---|---|---|---|
| OUTPUT | `'0'` (0x30) | raw PTY bytes | Client feeds these to its terminal emulator. [GT] [Networking/TtydProtocol.swift:52] |
| SET_WINDOW_TITLE | `'1'` (0x31) | UTF-8 string | Client displays it (iOS: `windowTitle` [Networking/TtydConnection.swift:219-220]; web: `document.title`). [GT] |
| SET_PREFERENCES | `'2'` (0x32) | JSON | Client options for ttyd's own web UI. iOS ignores it [Networking/TtydConnection.swift:221-222]. [GT] |

Server frame rules:

- All server frames MUST be sent as **binary** WS messages. OUTPUT payload is raw bytes
  with no UTF-8 guarantee — a multi-byte character can split across two frames — and
  RFC 6455 requires text frames to be valid UTF-8, so a text OUTPUT frame is a protocol
  violation that browsers will kill the connection over. Both clients set
  `binaryType = "arraybuffer"` / handle `Data` [GT] [Networking/TtydConnection.swift:209-214].
- `wtd` MUST NOT transform, validate, or re-chunk OUTPUT on UTF-8 boundaries — pass raw
  bytes in order. The emulator (xterm.js, SwiftTerm) owns decoding.
- `wtd` MAY coalesce consecutive PTY reads into one OUTPUT frame but MUST NOT reorder
  bytes and MUST NOT hold output on a timer beyond single-digit milliseconds — this is
  an interactive terminal, and the blank-screen redraw kick (section 7) depends on
  prompt delivery.
- After a successful handshake `wtd` MUST send one SET_WINDOW_TITLE frame and SHOULD
  send one SET_PREFERENCES frame with payload `{}`. Recommended title: `wt@<hostname>`,
  or `<arg>@<hostname>` when a session arg was given. Nothing parses the title — iOS
  displays it verbatim, the web page puts it in `document.title` — so the format is
  free, but the frame's existence is expected. ttyd 1.7.4's exact title string and its
  title/preferences ordering are **UNVERIFIED**; no known client is order-sensitive, so
  `wtd` fixes the order as title-then-preferences.

## 7. Resize semantics — no coalescing, ever

`wtd` MUST apply every RESIZE frame to the PTY immediately, in arrival order, one
`TIOCSWINSZ` per frame, even when consecutive frames are redundant or net to zero.

This is load-bearing, not pedantry. dtach keeps no screen buffer, so a reattach shows
blank until something writes. The workaround is two-sided (see `CLAUDE.md`): `bin/wt`
attaches with `dtach ... -r winch` [bin/wt:26], and the iOS app "jiggles" the size on
connect — `rows-1` at t+0.4 s, then the real size at t+0.55 s
[Networking/TtydConnection.swift:244-258] — to force a SIGWINCH through to the session
so full-screen TUIs repaint. A server that debounces or coalesces resizes would collapse
`rows-1, rows` into a net-zero change, no SIGWINCH would fire, and blank-on-attach would
regress with no error anywhere on either side.

Debouncing is the **client's** job and already happens there: the iOS client debounces
interactive resizes at 100 ms [Networking/TtydConnection.swift:149-158]. The server's
job is to be faithful.

Values are validated per section 6 (integers 1..9999); anything else means the frame is
ignored. The kick's `rows-1` is client-side guarded to stay >= 1
[Networking/TtydConnection.swift:250].

## 8. `?arg=` → argv

ttyd's `-a`/`--url-arg` appends each `arg` query value to the start command's argv;
`wtd` has no such flag — the behavior is **always on** (see `compatibility.md`). This is
the deep-link hot path: an iOS profile with `sessionArg` connects to
`ws://host:7681/ws?arg=<name>`, which must become `wt <name>` so the app's
auto-reconnect rejoins its session unattended (`CLAUDE.md`, deep-link mode).

Rules:

- Each `arg` query parameter's value, URL-decoded, is appended to the argv in order of
  appearance: `wt <arg1> [<arg2> ...]`. Multiple `arg` values are ttyd's documented
  behavior (**UNVERIFIED** empirically against 1.7.4); `bin/wt` only reads `$1`
  [bin/wt:42], so extras are inert today. [GT for the single-arg path]
- An **empty** value (`?arg=`) MUST still produce an (empty) argv element. `bin/wt`
  treats empty `$1` as "no arg" and renders the menu [bin/wt:42] — `wtd`'s own terminal
  page may rely on this to reach the menu.
- Query parameters other than `arg` MUST be ignored (the web client forwards all of
  `location.search` [GT]).
- `wtd` MUST NOT validate or rewrite arg *content* beyond the transport-safety floor
  below. Session-name policy lives in exactly one place — `bin/wt`, which drops a name
  containing `/` or `..` and renders the picker instead of erroring [bin/wt:43-44]. If
  `wtd` grew its own filter, the two layers would inevitably drift and disagree about
  what a malformed URL does. A client on a bad URL gets the picker, never an error;
  preserve that.
- Transport-safety floor (things that cannot be passed through argv at all): a value
  containing a NUL byte, a value longer than 4096 bytes, or more than 16 `arg` values.
  On violation `wtd` MUST drop the offending value(s) and continue — degrading to the
  picker, same graceful shape as `bin/wt`'s own rejection — never close the connection.

## 9. Connection lifecycle

One WebSocket connection = one fresh process. `wtd` spawns the configured start command
(`WT_PICKER`, default `/usr/local/bin/wt` [bin/wt-serve:16]) on a new PTY per
connection, as the service user. Session persistence lives entirely in dtach behind
`bin/wt` — `wtd` never manages terminal state across connections (until
scrollback-replay ships; see `session-lifecycle.md`, forward-compatibility note).

Spawn requirements:

- New session and process group (`setsid`), PTY slave as controlling terminal, stdin/
  stdout/stderr on the PTY — the standard terminal spawn. The process group matters for
  cleanup: the dtach *client* the picker execs lives in this group; the dtach *masters*
  do not (they daemonize into their own sessions — structurally why sessions survive
  disconnects, confirmed by masters outliving their launcher [LAB]).
- Environment: the service user's environment, plus `TERM=xterm-256color` (both clients
  are xterm-family emulators; ttyd's default TERM is **UNVERIFIED** but this value is
  what the fleet already runs), plus every exported `WT_*` key per the export rule in
  `CLAUDE.md` — `wtd` MUST export `WT_DIR` and `WT_PROJECTS` (when configured) into the
  child environment, because `bin/wt` reads them from the environment only
  [bin/wt:18,31] and an un-exported setting silently never arrives. That exact bug
  already happened once (`WT_PROJECTS`, see `CLAUDE.md`).
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
| RUNNING | Message > 1 MiB | Close 1009; kill process group | CLOSING |
| RUNNING | PTY readable | Send OUTPUT frame(s) | RUNNING |
| RUNNING | Child exits or PTY EOF | Flush remaining output, then Close 1000 | CLOSING |
| RUNNING | PTY write fails (child gone) | Treat as child exit | CLOSING |
| RUNNING | Client Close frame / TCP error / liveness timeout (section 10) | Reply/close; SIGHUP the process group, escalate SIGTERM after 2 s, SIGKILL after 5 s; reap | CLOSED |
| RUNNING | Spawn already failed (race) — see failure table | | |
| any | `wtd` shutdown (SIGTERM from systemd) | Close every connection 1001; SIGHUP each process group; wait bounded (5 s); exit | CLOSED |

Why SIGHUP first: it is what a real terminal hangup delivers. bash exits, the dtach
client exits, and the dtach masters — outside the process group, in their own sessions —
are untouched, which is the entire persistence model. Killing the process group (not
just the direct child) also catches background jobs a user started from the `c)` shell
[bin/wt:85]. What signal ttyd 1.7.4 sends on disconnect is **UNVERIFIED**; this is
`wtd`'s defined behavior, chosen to preserve the observable contract (dtach sessions
survive, per-connection processes do not).

On server shutdown the dtach sessions survive and are reattachable after restart — that
is the restart contract `systemctl restart wt.service` already has today (`CLAUDE.md`,
deploying section).

## 10. Ping, pong, and liveness

- `wtd` MUST answer WS ping frames with pongs carrying the same payload (RFC 6455
  obligation, but stated because the iOS client's health depends on it: it pings every
  **20 s** and closes on ping failure [Networking/TtydConnection.swift:272-283]).
- `wtd` MUST NOT impose any idle/read timeout shorter than **60 s**, so the 20 s client
  ping always lands with margin.
- `wtd` MUST NOT rely on client pings for liveness: **browser JavaScript cannot send WS
  ping frames at all** — ttyd's web client has no ping — so a "no ping means dead"
  policy would disconnect every browser user.
- For its own dead-peer detection `wtd` SHOULD send a server ping every 30 s and close a
  connection only after **90 s with no inbound traffic of any kind** (pong, data, or
  close). Endpoints auto-pong at the transport layer (browsers and URLSession both), so
  a healthy idle client always passes. Why bother: a phone that vanished mid-connection
  (elevator, dead battery) otherwise leaves a `wt` process and a dtach client attached
  forever, pinning the session's `attached` state (see `session-lifecycle.md`).
- Unsolicited pongs MUST be ignored (RFC 6455 permits them).

The iOS app backgrounds with ~30 s of socket grace, then the socket dies and the app
auto-reconnects on foreground [Networking/TtydConnection.swift:74-96]. The 90 s reap
never races that: the dead socket produces a TCP error server-side first, and even when
it does not, 90 s > the grace window.

## 11. Flow control (PAUSE / RESUME)

Both opcodes exist on the wire [GT], and **neither known client sends them today**: the
iOS client defines them and marks them "unused in v1"
[Networking/TtydProtocol.swift:10-11], and the production fleet runs without any client
issuing them [GT].

`wtd` v1 policy:

- MUST parse and accept `'2'` and `'3'` without error — never close, never log-spam.
- MAY treat them as no-ops. This is safe because real backpressure already exists one
  layer down: when the WS write buffer to a slow client exceeds a threshold, `wtd` MUST
  stop reading the PTY, at which point the foreground process blocks writing to the
  PTY — exactly the behavior of a real terminal with output stopped. Bounded memory, no
  protocol needed.
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
  same-origin, and native URLSession sends no `Origin` header (**UNVERIFIED** — confirm
  with a packet capture from the app before shipping; if iOS ever sent one it would be
  a mismatch, and the check must be validated against the real app, not assumed).
  Reverse proxies MUST preserve the original `Host` (e.g. nginx
  `proxy_set_header Host $host`) or the check rejects legitimate browsers.
  Known limit: DNS rebinding defeats an Origin-vs-Host comparison (both headers end up
  naming the attacker's hostname and match), but requires the victim's browser to be
  on the private network already — see the residual-exposure note in `openapi.yaml`
  for the full analysis and the reserved `WT_ALLOWED_HOSTS` fix.
- **Writable is not optional.** ttyd's `-W` is hardcoded on. Its absence has a uniquely
  cruel failure mode — input is *silently dropped*, the terminal looks fine, keystrokes
  do nothing (`CLAUDE.md`, client-contract table) — so `wtd` refuses to have the knob.
- **Session-name policy stays in `bin/wt`** (section 8). Names are untrusted network
  input; the `*/*` / `*..*` rejection [bin/wt:44] and `${var@Q}` quoting [bin/wt:47] are
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
| 1000 normal closure | Start command exited (picker `d)` [bin/wt:86], shell exit, PTY EOF), output flushed first. | Treat as final. Reconnect only on user action — a reconnect just runs `wt` again, landing on the picker or the deep-linked session; safe but possibly not what the user wanted. |
| 1001 going away | `wtd` is shutting down (systemd stop/restart). | Auto-reconnect with backoff. dtach sessions survive the restart; a deep-link profile rejoins its session unattended. |
| 1002 protocol error | First message was not a parseable handshake (section 5). | Do not auto-retry; this is a client bug or a non-tty peer. |
| 1008 policy violation | Handshake timeout. | Fix the client; do not blind-retry. |
| 1009 message too big | Client message exceeded 8 KiB (handshake) / 1 MiB (after). | Client bug; retrying the same payload will fail the same way. |
| 1011 internal error | PTY allocation or spawn failed, or an unexpected server fault. Where possible `wtd` SHOULD first send an OUTPUT frame with a one-line human-readable error so the failure is visible *in the terminal*, then close. | Retry with backoff is reasonable. |

Reality check: the current iOS client does not branch on close codes — every close path
funnels into the same `closed(reason:)` state with a display string
[Networking/TtydConnection.swift:304-310], and the client itself closes with
`normalClosure` on teardown and `goingAway` on deinit
[Networking/TtydConnection.swift:67,268]. The table binds `wtd` (and future clients);
today's compatibility bar is only that closes carry a sane code and optional reason
text, which iOS displays.

ttyd 1.7.4's own close-code usage is **UNVERIFIED**; this table is `wtd`'s definition,
constrained to standard codes so any RFC 6455 client interprets them sensibly.

## 14. Failure modes, enumerated

| Failure | `wtd` behavior | Section |
|---|---|---|
| Start command exits (any status) | Flush output, close 1000. Exit status is logged, not transmitted (no frame exists for it). | 9, 13 |
| PTY EOF without process exit (rare) | Same as exit: flush, 1000, reap. | 9 |
| Spawn fails (missing `WT_PICKER`, PTY exhaustion) | OUTPUT frame with one-line error, close 1011. | 13 |
| Malformed handshake | Close 1002; nothing spawned. | 5 |
| Handshake timeout | Close 1008. | 5 |
| Malformed RESIZE / unknown opcode / empty message | Ignore and log; connection lives. | 6 |
| Oversized message | Close 1009; kill process group. | 6 |
| Client vanishes (TCP error, liveness reap) | SIGHUP process group → SIGTERM 2 s → SIGKILL 5 s; dtach masters unaffected. | 9, 10 |
| Server shutdown | Close all 1001; SIGHUP groups; bounded wait; exit. Sessions survive. | 9 |
| PAUSE/RESUME received | Accepted, no-op in v1. | 11 |
| Cross-origin upgrade attempt | HTTP 403, never upgraded. | 2, 12 |
| Bad/absent `?arg=` | Dropped value → `bin/wt` renders the picker. Never a connection error. | 8 |

## 15. Limits summary

| Limit | Value | Bound by |
|---|---|---|
| Handshake deadline | 10 s | section 5 |
| Handshake max size | 8 KiB | section 5 |
| Post-handshake message max size | 1 MiB | section 6 |
| RESIZE columns/rows | integers 1..9999 | section 6 |
| `arg` values: max count / max length | 16 / 4096 bytes, drop-and-continue | section 8 |
| Idle timeout floor | never below 60 s; reap at 90 s without inbound traffic | section 10 |
| Server ping interval | 30 s | section 10 |
| Kill escalation | SIGHUP → SIGTERM +2 s → SIGKILL +5 s | section 9 |
