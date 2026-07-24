# ttyd-ify

A browser terminal for a Linux box: `ttyd` serves the web page, `wt` (a bash session
picker) is the start command ttyd runs on each connection, and sessions live as `dtach`
sockets so they survive the client disconnecting. Managed by systemd. ~480 lines of bash,
no build step, no runtime deps beyond `ttyd` + `dtach`.

**Audience:** primarily Sam's own machines, plus a small set of beta users who install
from this README. Not a mass-market OSS project — so favor a short, correct install path
and accurate docs over contributor ceremony, changelogs, or deprecation cycles.

**Platform:** Linux with systemd, full stop. The README's `dnf`/`pacman`/`brew` lines are
courtesy notes for getting `ttyd` + `dtach`, not a portability promise — `install.sh`
hard-requires systemd, so the `brew` path can't actually complete on macOS. There is no
commitment to macOS or non-systemd init; don't add abstraction for them speculatively.

## The primary client is a native iOS app in another repo

**`~/src/ios-claude-terminal`** — "WebClaude", a native iOS/iPadOS ttyd client. It does
**not** wrap ttyd's web UI: it speaks ttyd's WebSocket protocol directly
(`URLSessionWebSocketTask`, subprotocol `tty`) and renders with SwiftTerm. Read
`WebClaude/Networking/TtydProtocol.swift` and `Models/ServerProfile.swift` before changing
anything on the wire — that's the whole client contract in ~100 lines.

It's built with XcodeGen + Xcode and dev-signed, **not** shipped through the App Store, so
it can be rebuilt freely. The risk isn't review latency — it's that the two repos have no
shared CI, no type checking across the boundary, and beta users' installed builds don't
update when this repo does. A server change lands silently and breaks a phone.

What the client depends on:

| Thing | Where | Consequence if changed |
|---|---|---|
| `ttyd -W` (writable) | `args=(…)` in `bin/wt-serve` | Input is **silently dropped** — terminal looks fine, keystrokes do nothing |
| `ttyd -a` (`--url-arg`) | same line | `?arg=` is ignored, so deep-link profiles fall back to the menu |
| `/ws` endpoint, `/token` GET | ttyd itself | App GETs `/token`, ignores failure, sends `AuthToken: ""` — fine with no `-c` |
| Port **7681** | `WT_PORT` default | It's `ServerProfile.port`'s default; editable per profile, so a change means "every beta user edits their profile", not a hard break |
| `wt <name>` attaches **or creates** | `dtach -A` in `bin/wt` | A saved deep-link must work before the session exists |
| Plain `ws://`, no TLS | — | The app opens ATS wholesale for tailnet `ws://`. Adding TLS/`wss` is a client-side change too, not a drop-in |

**Two client modes are both in use**, and they exercise different code paths here:

- **Menu mode** — profile with no `sessionArg`. Lands on the `wt` picker. The menu is
  rendered by SwiftTerm on a phone screen and read by a human — nothing parses it, so
  reformatting is safe, but keep it **narrow** (a portrait phone is ~40 cols) and keep the
  single-keystroke choices.
- **Deep-link mode** — profile with `sessionArg` set → `ws://host:7681/ws?arg=<name>` →
  arrives as `$1`. This is why the direct-attach branch matters: the app auto-reconnects
  after drops and backgrounding (~30s grace), and only a `sessionArg` profile rejoins its
  session unattended. Without one, every reconnect dumps the user back at the menu.

`wt` only reads `$1` — ttyd passes a single `?arg=`. A name containing `/` or `..` is
dropped and the picker renders instead of erroring; that's what a client sees on a
malformed URL, so keep the graceful fallback.

**Redraw is a two-sided workaround — don't remove one half.** dtach keeps no screen buffer,
so a reattach shows blank until something writes. `bin/wt` passes `dtach -r winch`; the app
*also* jiggles the window size on connect (`rows-1`, then real, in
`TtydConnection.scheduleRedrawKick`). If blank-on-attach ever regresses, check both repos.

The app pings every 20s, so don't introduce a server-side idle timeout below that.
`ServerProfile.pathPrefix` supports ttyd behind a reverse proxy — a deployment shape this
repo doesn't document but the client already handles.

## Layout

| Path | Role |
|---|---|
| `bin/wt-serve` | Reads config, resolves `WT_BIND` → a concrete IP, `exec ttyd … wt`. What systemd starts. |
| `bin/wt` | The picker + direct-attach. ttyd's start command — runs fresh per connection. |
| `install.sh` / `uninstall.sh` | Idempotent installer; renders the systemd unit. `make` just wraps these. |
| `systemd/wt.service` | Template — `__WT_USER__` is `sed`-substituted at install time. |
| `etc/config.example`, `etc/projects.example` | Copied to `/etc/ttyd-ify/{config,projects}` only if absent. |
| `docs/bashrc-snippet.sh` | Documentation only — never installed or sourced. |

Naming quirk: the **project** is `ttyd-ify`, but every runtime artifact is `wt` — binaries,
`wt.service`, all `WT_*` keys. Keep the split; a rename would also invalidate beta users'
`/etc/ttyd-ify/config` and muscle memory.

## Commands

```sh
make lint                     # shellcheck — the only automated check that exists
make install                  # deps + binaries + unit; the recipe calls sudo itself
make install FORCE=1          # also overwrite already-installed binaries
make install WT_USER=alice    # run the service as someone other than you
make uninstall                # keeps /etc/ttyd-ify;  PURGE=1 removes it too
journalctl -u wt.service -f   # wt-serve logs its resolved bind line here
systemctl status wt.service
```

**Run `make` targets without `sudo`.** The recipes call `sudo` themselves, so an outer one
*nests*: the inner `sudo` resets `SUDO_USER` to `root`, `install.sh` resolves `WT_USER=root`,
and you get a root-owned web shell on the tailnet. `install.sh` now **refuses** that (it used
to only warn, and the warning scrolled past ~15 lines of output) — you'll get an error
telling you to drop the `sudo`. Passing `WT_USER=root` explicitly still works if you mean it.

Because `sudo` resets the environment, the Makefile forwards `FORCE`/`WT_USER` through
`sudo env …` explicitly. If you add a variable to `install.sh`, add it there too or it will
silently not arrive — the same class of bug as the `export` trap in `wt-serve` below.

**Installing new binaries does not restart the service.** `install.sh` ends with
`systemctl enable --now`, which is a no-op on an already-active unit, so a code change is on
disk but not running until an explicit `sudo systemctl restart wt.service`. Confirm what's
actually live with `diff bin/wt-serve /usr/local/bin/wt-serve`, not by reading the install
log — and see the restart warning below before running it.

Test without touching the live service. Note that a **scratch config file** is required —
env vars alone don't work (see the precedence rule below):

```sh
S=$(mktemp -d)
printf 'WT_BIND=localhost\nWT_PORT=7682\nWT_AUTH=\nWT_PICKER=%s/bin/wt\n' "$PWD" > "$S/config"
WT_CONFIG="$S/config" WT_DIR="$S/sockets" ./bin/wt-serve    # → http://127.0.0.1:7682
WT_DIR=/tmp/dtach-test ./bin/wt                             # picker alone, throwaway sockets
```

Never reuse the live bind+port: `wt-serve` resolves it, libwebsockets fails with
`lws_socket_bind: ERROR ... port 7681`, and it exits 1. Harmless, but confusing.

**Exercise the deep-link path, not just the menu** — the menu passing proves nothing about
the client's hot path. ttyd's own web page forwards `location.search` to the socket, so
`http://127.0.0.1:7682/?arg=demo` drives the same `$1` branch the app's `sessionArg` uses,
no app or simulator needed. `curl -fsS http://127.0.0.1:7682/token` → `{"token": ""}`
confirms the endpoint the app GETs on every connect.

There is no test suite; verification is `make lint` plus the above by hand.

## Deploying to this machine

This repo is checked out on a box where ttyd-ify is **installed and running**
(`/usr/local/bin/wt{,-serve}`, `/etc/ttyd-ify/`, `wt.service` active). Editing `bin/` does
not affect the live service until it's reinstalled.

**Running `install.sh` at all rewrites the live unit — `NO_ENABLE=1` does not make it safe.**
`NO_ENABLE=1` only skips `enable --now`; step 4 still `sed`s
`/etc/systemd/system/wt.service` and runs `daemon-reload`. So testing the installer with a
deliberately odd `WT_USER` leaves that value in the live unit, primed to take effect on the
next restart — the running process keeps the old user, so nothing looks wrong. If you must
exercise `install.sh`, check `rg '^User=' /etc/systemd/system/wt.service` afterwards.
There is no `PREFIX` sanity check either: point it at a nonexistent directory and the script
dies at step 2 under `set -e`, which happens to be *before* the unit is written.

`systemctl restart wt.service` drops every connected client mid-session — including a phone
that may be attached right now, **and including the terminal you are running in**, if this
session arrived through ttyd. The `dtach` sessions survive and everything reattaches, but
your own command gets killed mid-run, so don't put a restart in the middle of a command
chain you need the output of. **Ask before installing or restarting.**

## Non-obvious rules

- **`bin/wt` intentionally omits `set -e`** (`set -uo pipefail` only) — the menu loop must
  survive a failed `dtach` rather than drop the connection. Don't add `-e`.
- **`bin/wt-serve` uses full `set -euo pipefail`** — one-shot launcher, different contract.
- **Never bind `0.0.0.0`.** `resolve_ip` only yields a tailnet IP, `127.0.0.1`, an
  interface's address, or a literal. A wildcard path would turn an unauthenticated shell
  into a public one.
- **`WT_AUTH` stays empty by default — but know *why*, because the reason is narrower than
  the README implies.** Basic auth breaks Safari and every iOS *browser* (WebKit omits
  credentials on the WebSocket upgrade). It does **not** break the native app: the
  `BasicCredentials` seam in `TtydConnection.swift` already sets `Authorization` on both the
  `/token` GET and the upgrade — it's plumbed but has no UI yet. So for an app-only fleet,
  `WT_AUTH` is a real option, not a dead end, and enabling it is a two-repo change (expose
  the credential UI there, set the key here). Network-layer control stays the default
  recommendation; just don't dismiss auth as impossible on iOS.
- **Session names are untrusted input** — they arrive from a client over the network as
  `$1`. Keep the `*/*` / `*..*` rejection, and keep `${var@Q}` when interpolating a path
  into `bash -c`.
- **`dtach`, not tmux/screen.** No status bar, no splits — the *client* supplies scrollback
  (xterm.js in a browser, SwiftTerm in the app), and `dtach -r winch` redraws on attach
  because dtach keeps no buffer of its own. `-z` passes Ctrl-Z through; detach is Ctrl-`\`
  — which a phone keyboard reaches via SwiftTerm's accessory bar, so don't rebind it.
- **`wt` exports `WT=1`** so a login shell can detect it's inside a web session and skip
  auto-launching a multiplexer (`docs/bashrc-snippet.sh`). Anything spawning a shell inside
  `wt` depends on this to avoid recursion.
- **`install.sh` never clobbers `/etc/ttyd-ify/*`.** Changing a default means editing
  `etc/*.example`, which only affects *fresh* installs — existing beta users keep their old
  value. Plan for both populations.
- **The config file beats the environment, and nothing is exported.** `wt-serve` sources
  `$WT_CONFIG` *after* the env exists, and `etc/config.example` assigns every key
  unconditionally — so `WT_PORT=7682 ./bin/wt-serve` is silently ignored. `WT_CONFIG` is the
  only real env-level knob. `bin/wt` is the opposite: it reads no config, so `WT_DIR` and
  `WT_PROJECTS` from the environment *do* work. The `: "${WT_X:=default}"` lines are
  no-config fallbacks, not overrides — `wt-serve` defaults `WT_BIND` to `localhost` (safe
  when no config exists) while the shipped config says `tailscale`.
- **Any setting `bin/wt` reads must be `export`ed in `wt-serve`.** Sourcing the config only
  creates shell variables, and `wt` is ttyd's *child* — an un-exported setting silently never
  arrives, with no error anywhere. `WT_PROJECTS` was broken exactly this way (documented in
  the README, inert in practice) until it got an explicit export. Verify propagation with a
  stub `ttyd` early on `PATH` that runs `env | grep '^WT_'`; nothing else will tell you.
- **Local testing can't tell working from broken for project shortcuts.** On this machine
  `~/.config/wt/projects` is a symlink to `/etc/ttyd-ify/projects`, so `wt` finds shortcuts
  via its own fallback *and* via the config key. A fresh beta install has no symlink. Test
  shortcut changes with `WT_PROJECTS` pointed somewhere else entirely.

## Conventions

- Bash, with a header comment explaining *why* the script exists rather than what each line
  does. Keep it readable-in-a-browser-at-3am; no runtime deps.
- `log`/`skip`/`die` helpers with ANSI color in the install scripts; plain `printf` in `wt`.
- Config is sourced shell `KEY=value`; every key gets a `: "${WT_X:=default}"` in the
  consuming script.
- **A new script means updating two lists**: the `lint` target in `Makefile` *and* the
  `shellcheck` line in `.github/workflows/ci.yml`. They're duplicated and CI won't tell you
  a file was skipped.
- A new setting touches at least three places: `: "${WT_X:=…}"` in the consuming script,
  `etc/config.example`, and the README config table. If `bin/wt` (not `wt-serve`) is the
  consumer, it needs a **fourth**: an `export` in `wt-serve`, or it silently won't arrive —
  that's exactly how `WT_PROJECTS` broke.

## Security framing

Every change gets one question: *does this widen who can reach the shell?* The threat model
is explicit and accepted — a writable, unauthenticated shell as the service user, protected
only by the interface it binds to. The README, `etc/config.example`, and `install.sh`'s
closing banner each repeat that warning; preserve it when editing them.

Flag any diff touching bind resolution, auth, or session-name handling before committing.
