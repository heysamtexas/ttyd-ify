# ttyd 1.7.4 ↔ wtd compatibility matrix

`wtd` replaces ttyd 1.7.4 under an installed base that cannot be updated in lockstep:
the iOS client is dev-signed onto beta users' phones, the two repos share no CI and no
type checking, and a server change lands silently (`CLAUDE.md`). This matrix is the
explicit list of what `wtd` keeps, extends, drops, and hardcodes — so nobody has to
diff C against Go to find out what a phone will do.

Sibling documents: [`ws-protocol.md`](ws-protocol.md) (the wire contract),
[`openapi.yaml`](openapi.yaml) (HTTP surface), [`session-lifecycle.md`](session-lifecycle.md).

**Verification legend** — [GT] verified ground truth (extracted from ttyd 1.7.4's served
client on the live instance, or proven by the production iOS app working against it);
[file:line] source citations (`iOS` = `~/src/ios-claude-terminal/WebClaude/`);
[ttyd docs] ttyd's documented flag behavior, not empirically re-verified here;
**UNVERIFIED** = believed, needs a check before anyone builds on it.

**Classification legend**

| Class | Meaning |
|---|---|
| **Match** | Byte/behavior identical to ttyd 1.7.4. Clients cannot tell the difference. |
| **Extends** | ttyd behavior preserved, plus something ttyd does not do. Old clients unaffected. |
| **Hardcodes** | A ttyd flag whose value is frozen into `wtd` with no knob. |
| **Omits** | A ttyd capability `wtd` deliberately does not have. |
| **Diverges** | Observable behavior intentionally different. Each one is justified inline. |

## 1. HTTP surface

| Observable | ttyd 1.7.4 | wtd | Class | Why |
|---|---|---|---|---|
| `GET /token` | `{"token": ""}` without `-c` [GT] | Identical, and *always* empty — no auth exists | Match (value hardcoded) | The iOS app GETs it every connect and ignores failure [iOS Networking/TtydConnection.swift:174-190]; serving it keeps logs clean and lenient-but-not-identical clients working. |
| `GET /ws` upgrade, subprotocol `tty` | Selects and echoes `tty` [GT] | Identical, plus an Origin check | Extends | Echo is mandatory for browsers (RFC 6455). Origin check: see row below and `ws-protocol.md` section 12. |
| Handshake accepted as text **or** binary frame | Accepts both — its own client sends binary, the iOS app sends text, both work in production [GT] | Accepts both, explicitly specified | Match | This is the number-one compatibility trap; `ws-protocol.md` section 5 gives it a named callout. |
| Opcode-prefixed framing, both directions | `'0'/'1'/'2'/'3'` client→server; `'0'/'1'/'2'` server→client [GT] | Identical | Match | The whole point. Byte layouts in `ws-protocol.md` section 6. |
| `?arg=` appended to start-command argv | Only with `-a` [GT; enabled in production, bin/wt-serve:45] | Always on, no flag | Hardcodes | Deep-link (`sessionArg`) is the iOS app's hot path; a build without it silently strands every reconnect at the menu (`CLAUDE.md`). A knob whose wrong setting breaks phones invisibly should not exist. |
| `GET /` | ttyd's xterm.js terminal page, always [GT] | **Picker HTML** when no `?arg=`; terminal page when `?arg=` present (even empty) | Diverges | Deliberate feature (`picker-ui`). Safe because no installed client fetches `/`: the iOS app speaks only `/ws` + `/token` [iOS Models/ServerProfile.swift:27-42], and a human in a browser gets a *better* landing page. The terminal page forwards `location.search` to `/ws` exactly as ttyd's client does [GT], so `/?arg=demo` still exercises the deep-link `$1` branch for testing (`CLAUDE.md`, commands section). |
| Static assets | Bundled web client served by ttyd | wtd serves its own self-contained pages, relative URLs only | Diverges | wtd owns its UI. Relative URLs keep pages working behind a path-stripping reverse proxy. What asset paths ttyd exposes beyond `/` is **UNVERIFIED** and irrelevant — no known client fetches them. |
| Cross-origin policy | None known; **UNVERIFIED** whether ttyd checks Origin anywhere | `Origin` != `Host` authority → 403 on `/ws` upgrade and all mutating API routes; CORS never granted | Extends | An unauthenticated shell plus no-SOP-for-WebSockets means any web page can otherwise open a shell on a tailnet IP. Neither real client is affected (`ws-protocol.md` section 12). |
| JSON API (`/api/v1/*`, `/healthz`, `/openapi.json`) | Absent | New | Extends | Feature-detected via `GET /api/v1/meta` `features[]` — the anti-skew list. Clients MUST NOT assume these exist on a server that doesn't advertise them. |

## 2. WebSocket runtime behavior

| Observable | ttyd 1.7.4 | wtd | Class | Why |
|---|---|---|---|---|
| Server frames sent as binary | Its client sets `binaryType = "arraybuffer"` [GT]; frame type on the wire **UNVERIFIED** | Always binary, mandatory | Match (assumed) / specified | OUTPUT is raw bytes; invalid UTF-8 in a text frame is an RFC 6455 violation browsers enforce. `ws-protocol.md` section 6. |
| One process per connection | Fresh start-command per client connection [GT — every browser tab gets its own `wt` menu] | Identical | Match | Session sharing lives in dtach, not the server. |
| Kill start command on client disconnect | Happens [GT — detached `wt` processes do not accumulate]; exact signal **UNVERIFIED** | SIGHUP process group → SIGTERM +2 s → SIGKILL +5 s | Match (mechanism specified) | dtach masters sit outside the process group in their own sessions, so sessions survive — verified structurally and in the lab (`session-lifecycle.md`). |
| Ping/pong | libwebsockets answers pings [GT — the iOS 20 s ping has run against production since v1, iOS Networking/TtydConnection.swift:272-283]; server-side ping/idle defaults **UNVERIFIED** | Answers pings; server pings every 30 s; reaps only after 90 s of silence; idle floor 60 s | Match + specified | The 20 s client ping must always land inside any timeout, and browsers cannot ping at all. `ws-protocol.md` section 10. |
| Flow control (`'2'`/`'3'`) | Opcodes exist [GT]; server semantics **UNVERIFIED** | Accepted as no-ops; backpressure via bounded buffers + PTY read-stop (argless only — a named client past the backlog budget is dropped instead, `ws-protocol.md` section 9) | Match (for known clients) | No known client sends them [iOS Networking/TtydProtocol.swift:10-11]. Future use gated behind a `flow-control` feature flag. |
| Close codes | **UNVERIFIED** | Defined table (`ws-protocol.md` section 13) | Specified | The iOS client doesn't branch on codes [iOS Networking/TtydConnection.swift:304-310], so wtd is free to define clean ones. |
| `permessage-deflate` | **UNVERIFIED** whether negotiated | Not negotiated (offers declined) | Specified | Declining is always legal; compression buys little for interactive output. |
| Resize application | Applied per frame; coalescing behavior **UNVERIFIED** but the shipped redraw kick works against it [GT] | One `TIOCSWINSZ` per frame, in order, never coalesced — hard requirement | Match | The blank-screen kick dies silently under server-side debouncing. wtd now performs that kick itself, once per session (`hub.kick`), but the iOS client still sends its own [iOS Networking/TtydConnection.swift:244-258] and must keep working. `ws-protocol.md` section 7. |
| Output on attach | Nothing — ttyd relays a fresh process, and dtach sends no screen (measured: 64 bytes to a second client) | Recent output replayed before live output, per session (`scrollback-replay`) | **wtd is a superset** | Additive and invisible to an old client: replay arrives as ordinary OUTPUT frames with no new opcode, so a shipped iOS build renders it without knowing it exists. `ws-protocol.md` section 7a. |
| Process per connection | One per connection, always | One per connection when argless; one **shared and held** per session for `?arg=` | Divergence, deliberate | This is what makes replay possible at all, and it changes teardown: a named client disconnecting no longer ends the start command. Wire-invisible. `ws-protocol.md` section 9. |
| `TERM` for the spawned command | Default **UNVERIFIED** (ttyd has a `-T` flag) | `TERM=xterm-256color` | Match (assumed) | Both real clients are xterm-family emulators (xterm.js, SwiftTerm); this is what the fleet already renders. |

## 3. Production flags → wtd config

The live service runs `ttyd -i <resolved-ip> -p <port> -W -a <picker>` [bin/wt-serve:45].
`wtd` reads the same `/etc/ttyd-ify/config` (`WT_CONFIG` env override, and the file
**still beats the environment** — see section 5).

| Flag | ttyd meaning | wtd | Class | Why |
|---|---|---|---|---|
| `-i <ip>` | Bind interface/address | `WT_BIND` resolution absorbed into wtd: `tailscale` → `tailscale ip -4 \| head -1`; `localhost` → `127.0.0.1`; an interface name → its first IPv4 address; anything else → literal address. Same semantics as `resolve_ip` [bin/wt-serve:25-36]. **Refuses `0.0.0.0`, `::`, and empty — even as an explicit literal — and exits nonzero at startup.** | Match + Extends (refusal) | Bind resolution is the security boundary: a wildcard turns an unauthenticated shell into a public one. wt-serve made the wildcard hard to reach; wtd makes it impossible. Unresolvable bind (tailscaled down) → same failure shape as wt-serve: clear stderr line, exit 1 [bin/wt-serve:38-43]. |
| `-p <port>` | Listen port | `WT_PORT`, default 7681 | Match | 7681 is also the iOS profile default [iOS Models/ServerProfile.swift:7] — changing it is "every beta user edits their profile", not a code change. |
| `-W` | Writable (accept input) | Always on, no knob | Hardcodes | The un-writable failure mode is *silently dropped input* — terminal looks fine, keystrokes do nothing (`CLAUDE.md`). The worst possible bug class for a remote fleet; wtd removes the footgun entirely. |
| `-a` | Enable `?arg=` → argv | Always on, no knob | Hardcodes | See section 1. |

## 4. Deliberately dropped flags

| Flag | ttyd meaning | wtd stance | Why dropped |
|---|---|---|---|
| `-c user:pass` | HTTP basic auth | **Omitted — and `wtd` MUST refuse to start if `WT_AUTH` is non-empty** (fail closed, not silently unauthenticated). | The nuance matters (`CLAUDE.md`): basic auth breaks Safari and every iOS *browser* (WebKit omits credentials on the WS upgrade) but **not** the native app — its `BasicCredentials` seam already plumbs `Authorization` onto both the `/token` GET and the upgrade [iOS Networking/TtydConnection.swift:6-13,116-118,180-182]; it just has no UI. So auth is a real *future* option, not a dead end — but it is a **two-repo change** (credential UI there, verification here), gated behind the reserved `auth-basic` feature flag. Until then, a configured-but-ignored `WT_AUTH` would mean an operator believes auth is on when it is not; refusing to start is the only honest response. |
| `-R` | Read-only mode | Omitted | The inverse of `-W`: a mode whose only effect is silently ignoring keystrokes. Nobody in this deployment wants it, and its failure mode is indistinguishable from a bug. |
| `-I <file>` | Custom index page | Omitted | wtd owns its pages (`picker-ui`); an operator-supplied index would fork the UI contract this spec exists to pin down. |
| `-b <path>` | Base path (serve under a URL prefix) | Omitted in v1; `base-path` feature name reserved | **This one matters and is not dismissed lightly**: the iOS client already supports `pathPrefix` for reverse-proxy deployments [iOS Models/ServerProfile.swift:8-9,23] — a shape `CLAUDE.md` notes the client handles but this repo never documented. The v1 contract: the proxy **must strip the prefix**, so `GET /prefix/ws` arrives at wtd as `/ws`; the client's `pathPrefix` and a stripping proxy compose correctly today. What is lost without `-b`: deployment behind a *non-stripping* proxy. If that shape ever becomes real, implement `base-path`, advertise it in `features[]`, and serve under both `/` and the prefix. wtd's own pages use relative URLs specifically so proxied deployments work. |
| `-6` | IPv6 listener | Omitted as a flag | `WT_BIND` accepts a literal IPv6 address (refusing `::`), which covers the realistic case (a tailnet IPv6 literal). The `tailscale` keyword stays IPv4 (`tailscale ip -4`, matching bin/wt-serve:27); a `tailscale6` keyword is possible future work, not speculative abstraction today. |
| `-t key=value` | Client UI options (theme, font...) delivered via the preferences frame | Omitted; wtd sends `'2'` + `{}` | The iOS client ignores preferences entirely [iOS Networking/TtydProtocol.swift:54, Networking/TtydConnection.swift:221-222]; wtd's own pages style themselves. An option channel with zero consumers is contract surface for free. |
| `-m <n>` | Max concurrent clients | Omitted (unlimited) | The threat model already accepts resource DoS from inside the private network (README, `CLAUDE.md`). A limit adds a new failure mode — a wedged phone connection locking the owner out — for no protection the interface binding doesn't already provide. |
| `-o` | Exit after the last client disconnects (single-use server; exact semantics **UNVERIFIED**, [ttyd docs]) | Omitted | wtd is a long-running systemd service; "server exits when clients leave" is the opposite of its job. |

`WT_TTYD_ARGS` (raw extra ttyd flags, bin/wt-serve:17,48): meaningless to wtd, and it
can carry security-relevant flags (`-c`, `-R`). **Non-empty `WT_TTYD_ARGS` → refuse to
start**, with a message naming this document. Silently ignoring it would silently drop
whatever behavior the operator thought they were adding. Empty (the shipped default,
etc/config.example:28) is fine — and remember existing installs keep their old config
file, so both populations must be considered (`CLAUDE.md`, install rules).

## 5. Config file contract

Same file, same keys, same quirks — an existing `/etc/ttyd-ify/config` must drop in
unchanged (beta users keep their configs across upgrades; `install.sh` never clobbers
`/etc/ttyd-ify/*`).

| Key | wt-serve today | wtd | Notes |
|---|---|---|---|
| `WT_BIND` | Resolved by `resolve_ip` [bin/wt-serve:25-36] | Honored, identical resolution + wildcard refusal | Section 3. Default without a config file: `localhost` [bin/wt-serve:13] — safe-by-default when unconfigured; the *shipped* config says `tailscale` [etc/config.example:12]. Keep both. |
| `WT_PORT` | ttyd `-p` | Honored | Default 7681. |
| `WT_AUTH` | ttyd `-c` when non-empty | **Refuse to start if non-empty** | Section 4, `-c` row. |
| `WT_PICKER` | Start command path | Honored | Default `/usr/local/bin/wt` [bin/wt-serve:16]. Reported as `terminalPath` in `/api/v1/meta`. |
| `WT_PROJECTS` | Exported to `wt` when set [bin/wt-serve:23] | Honored, exported to the child, **and** read by wtd itself for `GET /api/v1/projects` and `project:` creates — same file, same parser as bin/wt:30-37 | The export rule (`CLAUDE.md`): any setting `bin/wt` reads must be exported by the launcher or it silently never arrives — `WT_PROJECTS` was broken exactly that way once. wtd inherits the obligation. |
| `WT_TTYD_ARGS` | Appended raw to ttyd | **Refuse to start if non-empty** | Section 4, last paragraph. |
| `WT_DIR` | *(not a config key today — bin/wt reads it from the environment, default `~/.dtach` [bin/wt:18])* | **New config key.** wtd needs the socket directory for the sessions API, and it must be the *same* directory `bin/wt` uses — so wtd reads `WT_DIR` (default: service user's `$HOME/.dtach`) and **exports it** to every spawned picker. | Per `CLAUDE.md`, a new setting touches the consuming script's default, `etc/config.example`, and the README table — plus the export, since `bin/wt` is a consumer. Those three files are outside `api/`; landing this key is called out here so the implementation PR doesn't forget them. |

**Precedence quirk — preserved on purpose.** wt-serve sources the config *after* the
environment exists, and the shipped config assigns every key unconditionally — so the
file beats the environment and `WT_PORT=7682 wt-serve` is silently ignored; `WT_CONFIG`
is the only real env-level knob (`CLAUDE.md`; bin/wt-serve:9-11). wtd replicates this
exactly: config file wins, `WT_CONFIG` selects the file, `: "${KEY:=default}"`
fallbacks apply only when neither file nor environment set a key. Changing precedence
would silently re-configure every existing install whose operator has both an env var
and a config file — the class of invisible change this document set exists to prevent.
The scratch-config testing recipe in `CLAUDE.md` keeps working unchanged.

## 6. Things wtd must keep saying

The security warning appears in the README, `etc/config.example`, and `install.sh`'s
closing banner, and `CLAUDE.md` requires preserving it when editing them. wtd adds two
more places that must carry it: the startup log line (wt-serve's equivalent of
`wt-serve: ttyd on IP:PORT ...` [bin/wt-serve:50] — wtd MUST log its resolved bind the
same way, it is the line `journalctl -u wt.service` debugging depends on) and
`openapi.yaml`'s info description. Every surface that describes reachability repeats:
**writable, unauthenticated shell; only as private as the interface it binds to; never
a wildcard bind.**
