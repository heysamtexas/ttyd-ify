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
in the install scripts.

| Path | Role |
|---|---|
| `bin/wt-serve` | The only launcher: config → bind IP → `exec wtd -listen … -session-dir …`. Refuses to start if `WT_AUTH`/`WT_TTYD_ARGS` are set, or if the config exists and is unreadable (#59); warns and ignores retired `WT_WEB_PORT` and `WT_PICKER`. |
| `bin/wt-bind.sh` | `resolve_ip`, sourced (not executed). One implementation since ttyd retired (#23). |
| `docs/bashrc-snippet.sh` | Documentation only — never installed or sourced. |
| `test/stub-start-command.sh` | An external start command for protocol tests, so they never touch dtach or `~/.dtach`. |

## Rules that have each broken something real

- **`bin/wt-serve` uses full `set -euo pipefail`** — one-shot launcher, so failing loudly is right.
  There is no longer a long-running shell script in this repo; `bin/wt` and its deliberately absent
  `-e` were retired with the picker (#49).
- **The config file beats the environment, and nothing is exported.** `wt-serve` sources
  `$WT_CONFIG` *after* the env exists, and `etc/config.example` assigns every key unconditionally
  except `WT_REPLAY_BYTES` (shipped commented out, so wtd's own default stays the single source of
  that number). So `WT_PORT=7682 ./bin/wt-serve` is silently ignored while
  `WT_REPLAY_BYTES=0 ./bin/wt-serve` does work. `WT_CONFIG` is the only real env-level knob.
  `wtd` reads `WT_DIR`/`WT_PROJECTS` from the environment too, but the launcher now passes them as
  `-session-dir`/`-projects-file` so the config wins (#28). The `: "${WT_X:=default}"` lines are
  no-config fallbacks, not overrides — `wt-serve`
  defaults `WT_BIND` to `localhost` (safe when no config exists) while the shipped config says
  `tailscale`.
- **Missing config and unreadable config are different states, and only one is silent** (#59).
  Missing takes the fallbacks above. Present-but-unreadable *refuses to start*, because every
  value the operator set is being ignored — `WT_BIND` included, and that is the access control.
  `install.sh` writes the config `0640 root:$WT_USER`, so the way to reach the refusal is to
  change the service user without re-installing. Note `[ -r "$f" ] && . "$f"` does **not** trip
  errexit: a failing left-hand side of an `&&` list is a tested condition, not a failed command,
  so any fail-open of this shape has to be checked explicitly. Only `config` is tightened;
  `projects` stays `0644` because shortcuts are not secrets.
- **Pass settings as flags, not exports.** Sourcing the config only creates shell variables, so a
  key a *child* reads from the environment silently never arrives, with no error anywhere.
  `WT_PROJECTS` was broken exactly this way (documented in the README, inert in practice) until it
  got an explicit export, and `WT_DIR` stayed broken until #28 — a key that reached nothing for as
  long as it existed. A flag either arrives or is visibly missing from the process's command line.
  Verify propagation by pointing `WT_WEB_BIN` at a stub that runs `echo "$@"; env | grep '^WT_'`;
  nothing else will tell you.
- **A new script means updating two lists**: the `lint` target in `Makefile` *and* the `shellcheck`
  line in `.github/workflows/ci.yml`. They are duplicated and CI will not tell you a file was
  skipped.
- **A new setting touches at least three places**: `: "${WT_X:=…}"` in the consuming script,
  `etc/config.example`, and the README config table. If `wtd` is the consumer it needs a
  **fourth** — a flag in `wt-serve`, plus the flag's own definition in `cmd/wtd/main.go`.
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
- **`wtd` sets `WT=1`** on every terminal it spawns so a login shell can detect it is inside a web
  session and skip auto-launching a multiplexer (`docs/bashrc-snippet.sh`). It is set in
  `cmd/wtd/attach.go` for both shapes and in `createSession` for the API; a session created without
  it recurses into tmux inside dtach, which is the mess the variable exists to prevent.
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
printf 'WT_BIND=localhost\nWT_PORT=7682\nWT_DIR=%s/sockets\nWT_WEB_BIN=%s/wtd\n' "$S" "$PWD" > "$S/config"
WT_CONFIG="$S/config" ./bin/wt-serve                        # → http://127.0.0.1:7682
# and to drive a session directly, without the server:
#   dtach -A /tmp/dtach-test/demo.sock -z -r winch bash
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
key; add `WT_WEB_PORT=7683` and it must warn and still bind `WT_PORT`; `chmod 000` the scratch
config and it must exit 1 naming the file rather than binding `localhost` silently. That last one
only reproduces as a **non-root** user — root can read any mode, which is why its assertion in
`test/install-uninstall.sh` drops privileges with `setpriv`.

Never reuse the live bind+port: `wt-serve` resolves it, wtd fails with
`bind: address already in use` on 7681, and it exits nonzero. Harmless, but confusing.

Keep a scratch `WT_DIR` short — `mktemp -d` is fine, a deep path is not. `<dir>/<name>.sock` has to
fit in 107 bytes or nothing can connect to the sessions you create there.

**Local testing can't tell working from broken for project shortcuts.** On this machine
`~/.config/wt/projects` is a symlink to `/etc/ttyd-ify/projects`, so `wtd` finds shortcuts via its own
fallback *and* via the config key. A fresh beta install has no symlink. Test shortcut changes with
`WT_PROJECTS` pointed somewhere else entirely.

Beyond `shellcheck` and `test/install-uninstall.sh` the shell side has no tests of its own.
