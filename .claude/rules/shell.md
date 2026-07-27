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
| `bin/wt` | The picker + direct-attach. The server's start command — runs fresh per connection. |
| `bin/wt-serve` | The only launcher: config → bind IP → `exec wtd … wt`. Refuses to start if `WT_AUTH`/`WT_TTYD_ARGS` are set; warns and ignores a retired `WT_WEB_PORT`. |
| `bin/wt-bind.sh` | `resolve_ip`, sourced (not executed). One implementation since ttyd retired (#23). |
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
  `WT_REPLAY_BYTES=0 ./bin/wt-serve` does work. `WT_CONFIG` is the only real env-level knob.
  `bin/wt` is the opposite: it reads no config, so `WT_DIR` and `WT_PROJECTS` from the environment
  *do* work. The `: "${WT_X:=default}"` lines are no-config fallbacks, not overrides — `wt-serve`
  defaults `WT_BIND` to `localhost` (safe when no config exists) while the shipped config says
  `tailscale`.
- **Any setting `bin/wt` reads must be `export`ed in the launcher.** Sourcing the config only
  creates shell variables, and `wt` is the server's *child* — an un-exported setting silently never
  arrives, with no error anywhere. `WT_PROJECTS` was broken exactly this way (documented in the
  README, inert in practice) until it got an explicit export. Verify propagation by pointing
  `WT_WEB_BIN` at a stub that runs `env | grep '^WT_'`; nothing else will tell you.
- **A new script means updating two lists**: the `lint` target in `Makefile` *and* the `shellcheck`
  line in `.github/workflows/ci.yml`. They are duplicated and CI will not tell you a file was
  skipped.
- **A new setting touches at least three places**: `: "${WT_X:=…}"` in the consuming script,
  `etc/config.example`, and the README config table. If `bin/wt` is the consumer it needs a
  **fourth** — an `export` in `wt-serve`.
- **`install.sh` never clobbers `/etc/ttyd-ify/*`.** Changing a default means editing
  `etc/*.example`, which only affects *fresh* installs — an existing install keeps its old value.
  **There is exactly one existing install: this box.** So read `/etc/ttyd-ify/config` and handle
  what is actually in it; do not design a migration for configs nobody has (`CLAUDE.md`, audience).
  Retiring a key is the case that tempts you into it — #23 shipped a `WT_WEB_PORT` warn-and-ignore
  path for a key this box's config did not even contain. `WT_AUTH`/`WT_TTYD_ARGS` refusing to start
  is worth keeping anyway, because those two are access controls and the cost of losing one
  silently is not proportional to how many boxes exist.
- **`install.sh` always overwrites every binary it installs** (#26, #30) — launchers, helper and
  `wtd` alike. Skipping an existing one meant a changed file never reached the box while the log
  said success, and for `wtd` that shipped the previous server onto this box. There is no `FORCE`
  any more; `cmp` decides only whether the log says `installed` or `unchanged`. The config is the
  opposite and stays that way: never clobbered.
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
printf 'WT_BIND=localhost\nWT_PORT=7682\nWT_PICKER=%s/bin/wt\nWT_WEB_BIN=%s/wtd\n' "$PWD" "$PWD" > "$S/config"
WT_CONFIG="$S/config" WT_DIR="$S/sockets" ./bin/wt-serve    # → http://127.0.0.1:7682
WT_DIR=/tmp/dtach-test ./bin/wt                             # picker alone, throwaway sockets
```

`WT_WEB_BIN` matters: without it the launcher execs `/usr/local/bin/wtd`, so you test the
*installed* server and your `make build` output goes unexercised.

Then check the things a client actually touches, without an app or a simulator:

```sh
curl -fsS http://127.0.0.1:7682/token        # → {"token": ""} — GETed on every connect
curl -fsS http://127.0.0.1:7682/api/v1/meta  # proves it is wtd, and lists features[]
# and http://127.0.0.1:7682/?arg=demo drives the same $1 branch the app's sessionArg uses
```

The refusals are worth exercising too, since each one is a security control: add
`WT_AUTH=x` or `WT_TTYD_ARGS=-R` to the scratch config and the launcher must exit 1 naming the
key; add `WT_WEB_PORT=7683` and it must warn and still bind `WT_PORT`.

Never reuse the live bind+port: `wt-serve` resolves it, wtd fails with
`bind: address already in use` on 7681, and it exits nonzero. Harmless, but confusing.

Keep a scratch `WT_DIR` short — `mktemp -d` is fine, a deep path is not. `<dir>/<name>.sock` has to
fit in 107 bytes or nothing can connect to the sessions you create there.

**Local testing can't tell working from broken for project shortcuts.** On this machine
`~/.config/wt/projects` is a symlink to `/etc/ttyd-ify/projects`, so `wt` finds shortcuts via its own
fallback *and* via the config key. A fresh beta install has no symlink. Test shortcut changes with
`WT_PROJECTS` pointed somewhere else entirely.

Beyond `shellcheck` and `test/install-uninstall.sh` the shell side has no tests of its own.
