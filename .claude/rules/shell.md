---
paths:
  - "bin/**"
  - "*.sh"
  - "test/**"
  - "etc/**"
  - "systemd/**"
  - "docs/*.sh"
---

# The shell half

Bash, with a header comment explaining *why* the script exists rather than what each line does.
Keep it readable-in-a-browser-at-3am; no runtime deps. `log`/`skip`/`die` helpers with ANSI colour
in the install scripts; plain `printf` in `wt`.

| Path | Role |
|---|---|
| `bin/wt` | The picker + direct-attach. Start command for **both** servers — runs fresh per connection. |
| `bin/wt-serve` | Legacy launcher: config → bind IP → `exec ttyd … wt`. Frozen; deleted when ttyd retires. |
| `bin/wt-web-serve` | Launcher for `wtd`. Refuses to start if `WT_AUTH`/`WT_TTYD_ARGS` are set. |
| `bin/wt-bind.sh` | Canonical `resolve_ip`, sourced (not executed). `wt-serve` keeps a frozen copy. |
| `docs/bashrc-snippet.sh` | Documentation only — never installed or sourced. |
| `test/stub-start-command.sh` | Stands in for `bin/wt` in protocol tests, so they never touch `~/.dtach`. |

## Rules that have each broken something real

- **`bin/wt` intentionally omits `set -e`** (`set -uo pipefail` only) — the menu loop must survive a
  failed `dtach` rather than drop the connection. Don't add `-e`. `bin/wt-serve` uses full
  `set -euo pipefail`: one-shot launcher, different contract.
- **The config file beats the environment, and nothing is exported.** `wt-serve` sources
  `$WT_CONFIG` *after* the env exists, and `etc/config.example` assigns every key unconditionally
  except `WT_REPLAY_BYTES` (shipped commented out, so wtd's own default stays the single source of
  that number). So `WT_PORT=7682 ./bin/wt-serve` is silently ignored while
  `WT_REPLAY_BYTES=0 ./bin/wt-web-serve` does work. `WT_CONFIG` is the only real env-level knob.
  `bin/wt` is the opposite: it reads no config, so `WT_DIR` and `WT_PROJECTS` from the environment
  *do* work. The `: "${WT_X:=default}"` lines are no-config fallbacks, not overrides — `wt-serve`
  defaults `WT_BIND` to `localhost` (safe when no config exists) while the shipped config says
  `tailscale`.
- **Any setting `bin/wt` reads must be `export`ed in both launchers.** Sourcing the config only
  creates shell variables, and `wt` is the server's *child* — an un-exported setting silently never
  arrives, with no error anywhere. `WT_PROJECTS` was broken exactly this way (documented in the
  README, inert in practice) until it got an explicit export. Verify propagation with a stub `ttyd`
  early on `PATH` that runs `env | grep '^WT_'`; nothing else will tell you.
- **A new script means updating two lists**: the `lint` target in `Makefile` *and* the `shellcheck`
  line in `.github/workflows/ci.yml`. They are duplicated and CI will not tell you a file was
  skipped.
- **A new setting touches at least three places**: `: "${WT_X:=…}"` in the consuming script,
  `etc/config.example`, and the README config table. If `bin/wt` is the consumer it needs a
  **fourth** — an `export` in *both* `wt-serve` and `wt-web-serve`.
- **`install.sh` never clobbers `/etc/ttyd-ify/*`.** Changing a default means editing `etc/*.example`,
  which only affects *fresh* installs — existing beta users keep their old value. Plan for both
  populations.
- **`wt` exports `WT=1`** so a login shell can detect it is inside a web session and skip
  auto-launching a multiplexer (`docs/bashrc-snippet.sh`). Anything spawning a shell inside `wt`
  depends on this to avoid recursion.
- **`systemd/*.service` are templates** — `__WT_USER__` is `sed`-substituted at install time.

## dtach, not tmux/screen

No status bar, no splits — the *client* supplies scrollback while connected (xterm.js in a browser,
SwiftTerm in the app), and `dtach -r winch` redraws on attach because dtach keeps no buffer of its
own. `-z` passes Ctrl-Z through; detach is Ctrl-`\`, which a phone keyboard reaches via SwiftTerm's
accessory bar, so don't rebind it.

## Testing the shell side

A **scratch config file** is required — env vars alone don't work, per the precedence rule above:

```sh
S=$(mktemp -d)
printf 'WT_BIND=localhost\nWT_PORT=7682\nWT_AUTH=\nWT_PICKER=%s/bin/wt\n' "$PWD" > "$S/config"
WT_CONFIG="$S/config" WT_DIR="$S/sockets" ./bin/wt-serve    # → http://127.0.0.1:7682
WT_DIR=/tmp/dtach-test ./bin/wt                             # picker alone, throwaway sockets
```

Then check the two things a client actually touches, without an app or a simulator:

```sh
curl -fsS http://127.0.0.1:7682/token        # → {"token": ""} — GETed on every connect
# and http://127.0.0.1:7682/?arg=demo drives the same $1 branch the app's sessionArg uses
```

Never reuse the live bind+port: `wt-serve` resolves it, libwebsockets fails with
`lws_socket_bind: ERROR ... port 7681`, and it exits 1. Harmless, but confusing.

Keep a scratch `WT_DIR` short — `mktemp -d` is fine, a deep path is not. `<dir>/<name>.sock` has to
fit in 107 bytes or nothing can connect to the sessions you create there.

**Local testing can't tell working from broken for project shortcuts.** On this machine
`~/.config/wt/projects` is a symlink to `/etc/ttyd-ify/projects`, so `wt` finds shortcuts via its own
fallback *and* via the config key. A fresh beta install has no symlink. Test shortcut changes with
`WT_PROJECTS` pointed somewhere else entirely.

Beyond `shellcheck` and `test/install-uninstall.sh` the shell side has no tests of its own.
