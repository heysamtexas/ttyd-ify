# ttyd-ify

A browser terminal for a Linux box: `ttyd` serves the web page, `wt` (a bash session
picker) is the start command ttyd runs on each connection, and sessions live as `dtach`
sockets so they survive the client disconnecting. Managed by systemd. ~480 lines of bash,
no build step, no runtime deps beyond `ttyd` + `dtach`.

**Audience — read this first, it determines how to write everything else.** Installs mostly
do not happen by a human following the README. They happen by a human pointing *their* agent
at this repo and saying "install this." So the primary reader of this file is another agent,
on a machine it has never seen, that cannot ask this repo's author anything. Optimize for
that: state the one correct command, name the traps explicitly, and make every claim
verifiable by a command the agent can run. A hedge or a "you may want to" is a defect —
it turns into a question the human has to answer or, worse, a guess.

Beyond that: Sam's own machines plus a small beta group. Not a mass-market OSS project, so
favor a short correct install path over contributor ceremony, changelogs, or deprecation
cycles.

**First, work out which situation you are in** — the rest of this file splits on it:

```sh
systemctl is-active wt.service 2>/dev/null   # "active" → already installed, see "Already installed"
```

**Platform:** Linux with systemd, full stop. The README's `dnf`/`pacman`/`brew` lines are
courtesy notes for getting `ttyd` + `dtach`, not a portability promise — `install.sh`
hard-requires systemd, so the `brew` path can't actually complete on macOS. There is no
commitment to macOS or non-systemd init; don't add abstraction for them speculatively.

## Fresh install, start to finish

**Step 0 — check the three things that actually decide success.** Run this verbatim; each
line prints a verdict, none of them change anything:

```sh
systemctl --version >/dev/null 2>&1 && echo "systemd: ok" || echo "systemd: MISSING — stop, this repo cannot install here"
command -v ttyd dtach >/dev/null 2>&1 && echo "deps: present" || echo "deps: absent — fine on Debian/Ubuntu (install.sh apt-gets them), otherwise install ttyd+dtach first"
sudo -n true 2>/dev/null && echo "sudo: passwordless" || { [ "$(id -u)" = 0 ] && echo "sudo: running as root already" || echo "sudo: WILL PROMPT — see below"; }
tailscale ip -4 2>/dev/null | head -1 || echo "no tailscale — you must change WT_BIND, see step 3"
```

**If `sudo` will prompt, you cannot complete the install.** You can't answer a password
prompt, and the command will hang until it times out. Don't try to pipe a password in. Ask
the human to run the single install command themselves — in Claude Code they can run it in
this session by prefixing it with `!` — then carry on from step 2.

**Step 1 — install.** One command, no `sudo` prefix:

```sh
make install
```

`make install` is correct whether you are root or a normal user; the recipe adds `sudo`
only when needed. **Do not write `sudo make install`** — that nests sudo, which resets
`SUDO_USER` to root, and the installer will refuse (it used to silently produce a
root-owned web shell). Pass extras as make variables: `make install FORCE=1` to overwrite
existing binaries, `make install WT_USER=<login>` to choose the account.

**Which account will the terminal run as?** `install.sh` prints `service user: X (from Y)`.
It resolves `WT_USER` → `$SUDO_USER` → owner of the checkout, and **refuses** if that lands
on root without being named explicitly. On a root-login box with a root-owned clone you will
get that refusal; the fix is `make install WT_USER=<their login>`. Pick the human's normal
account — that's whose files, keys, and shell history the sessions will have.

**Step 2 — this is the point to tell the human what they now have.** Don't skip it and
don't soften it: the service is a **writable, unauthenticated shell**. Anyone who can reach
the port gets that account's shell, no password. There is no app-layer auth by default and
that is deliberate (see `WT_AUTH` below). The only thing protecting it is which network
interface it is bound to. So confirm the bind target with them rather than assuming.

**Step 3 — bind target, the one setting that matters.** The shipped default is
`WT_BIND=tailscale` in `/etc/ttyd-ify/config`, which resolves this node's tailnet IP.

| Their situation | Set `WT_BIND` to | Reachable from |
|---|---|---|
| On a tailnet (`tailscale ip -4` printed an address) | `tailscale` (default — nothing to do) | their other tailnet devices |
| No tailnet, wants access from elsewhere | `localhost`, then an SSH tunnel (`ssh -L 7681:127.0.0.1:7681 host`) | only through that tunnel |
| A WireGuard/VPN interface | the interface name, e.g. `wg0` | that VPN |

`resolve_ip` accepts only those forms plus a literal IP, and **never** a wildcard. If you
find yourself wanting `0.0.0.0`, stop — that publishes an unauthenticated shell to every
network the box is on. `install.sh` never overwrites an existing `/etc/ttyd-ify/config`, so
editing it is safe to redo.

After editing the config: `sudo systemctl restart wt.service`.

**Step 4 — verify. Do not report success without this**; the service can be `active` while
being unreachable (e.g. `WT_BIND=tailscale` with tailscaled down makes `wt-serve` exit 1 and
systemd retry).

```sh
systemctl is-active wt.service                       # → active
# Resolved bind line. Do NOT use `-n 20` here — libwebsockets logs several lines per
# client connection, so the bind line scrolls out and you get empty output, which reads
# like a failure when the service is fine.
journalctl -u wt.service --no-pager | grep 'wt-serve: ttyd on' | tail -1
IP=$(journalctl -u wt.service --no-pager | grep -o 'ttyd on [0-9.]*' | tail -1 | cut -d' ' -f3)
curl -fsS "http://$IP:7681/token"                    # → {"token": ""}
ps -o user=,cmd= -C ttyd                             # → the intended user, and -W -a present
```

Tell the human the URL (`http://<ip>:7681`) and which account it lands them in.

**Known failure modes, so you don't have to diagnose them from scratch:**

| Symptom | Cause | Fix |
|---|---|---|
| `could not resolve WT_BIND='tailscale'` | tailscaled down, or no tailnet | `tailscale status`; or set `WT_BIND=localhost` |
| Unit restarts every 3s | `wt-serve` exits 1 — almost always bind resolution | `journalctl -u wt.service -n 20` |
| `lws_socket_bind: ERROR ... port 7681` | something already on the port (often a second copy) | `ss -lntp \| grep 7681` |
| Installer refuses, "resolved WT_USER=root" | `sudo make install`, or root-owned clone | `make install WT_USER=<login>` |
| Binaries "installed" but behavior unchanged | `install.sh` skips existing binaries | `make install FORCE=1`, then restart |
| Terminal renders but keystrokes do nothing | `ttyd -W` missing | don't remove `-W` from `bin/wt-serve` |

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

**No `sudo` prefix on `make` targets** — the recipes add it themselves, and an outer one
nests and resets `SUDO_USER` to root; `install.sh` refuses rather than installing a
root-owned shell. Full reasoning in "Fresh install" above.

Because `sudo` resets the environment, the Makefile forwards `FORCE`/`WT_USER` through
`sudo env …` explicitly. **A new variable consumed by `install.sh` must be added to that
`env` list too**, or it silently won't arrive — the same class of bug as the `export` trap in
`wt-serve` below.

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

## Already installed: changing a live deployment

This applies when `systemctl is-active wt.service` printed `active` — the case on Sam's own
boxes, where the checkout sits on the same machine as the running service. Editing `bin/`
does **not** affect that service; it runs the installed copies at `/usr/local/bin/wt{,-serve}`.
Check what is actually live before debugging anything:

```sh
for b in wt wt-serve; do diff -q "bin/$b" "/usr/local/bin/$b" >/dev/null && echo "$b: live copy matches repo" || echo "$b: repo differs from live copy"; done
```

**Restarting is disruptive and can cut you off mid-command.** `systemctl restart wt.service`
kills `ttyd`, which drops every connected client: a phone attached right now, and — if your
own session arrived through ttyd — the terminal you are typing in. The `dtach` sessions and
everything running inside them survive and reattach, so nothing is lost, but your command
chain dies at that line. Put a restart in its own step, never in the middle of a `&&` chain
whose later output you need. **Ask the human before restarting**; someone may be mid-task.

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

**If you are installing this on someone's machine for them, the disclosure is part of the
job.** They asked for a browser terminal; what they are getting is an unauthenticated shell
as their own user, reachable by anything that can route to the bound interface. Say that in
plain words, name the interface it ended up on, and confirm that's what they intended — see
step 2 of "Fresh install". Two answers should stop you and send you back to them: they can't
tell you what network the box is on, or they ask for `0.0.0.0` / a public IP / a port
forward. Neither is a config question; both change who can reach their shell.
