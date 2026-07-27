---
name: install-ttyd-ify
description: Install, verify, or change a live ttyd-ify deployment on a Linux box. Use for "install this", "set this up", "deploy it", any first-time setup, and for restarting or reinstalling over a service that is already running — including the security disclosure the install requires and the failure modes worth knowing before diagnosing from scratch.
---

# Installing ttyd-ify

**Installs mostly do not happen by a human following the README.** They happen by a human
pointing *their* agent at this repo and saying "install this." So you are probably on a machine
you have never seen, and you cannot ask this repo's author anything. State the one correct
command, name the traps, and make every claim verifiable by a command you can run. A hedge is a
defect — it turns into a question the human has to answer, or a guess.

**Platform: Linux with systemd, full stop.** The README's `dnf`/`pacman`/`brew` lines are
courtesy notes for getting `dtach`, not a portability promise — `install.sh` hard-requires
systemd, so the `brew` path cannot complete on macOS.

**First work out which situation you are in:**

```sh
systemctl is-active wt.service 2>/dev/null   # "active" → already installed, see the last section
```

## Step 0 — check the four things that actually decide success

Run this verbatim; each line prints a verdict, none of them change anything:

```sh
systemctl --version >/dev/null 2>&1 && echo "systemd: ok" || echo "systemd: MISSING — stop, this repo cannot install here"
command -v dtach >/dev/null 2>&1 && echo "deps: present" || echo "deps: absent — fine on Debian/Ubuntu (install.sh apt-gets dtach), otherwise install dtach first"
command -v go >/dev/null 2>&1 && echo "server binary: 'make build' will produce it" || echo "server binary: no Go — you must 'make fetch' first (see step 1)"
sudo -n true 2>/dev/null && echo "sudo: passwordless" || { [ "$(id -u)" = 0 ] && echo "sudo: running as root already" || echo "sudo: WILL PROMPT — see below"; }
tailscale ip -4 2>/dev/null | head -1 || echo "no tailscale — you must change WT_BIND, see step 3"
```

The server is a Go binary (`wtd`) and `install.sh` **refuses without it** — there is no fallback
server since ttyd was retired (#23). It never builds it either, because it runs as root and would
leave root-owned files in the checkout and Go cache.

**If `sudo` will prompt, you cannot complete the install.** You can't answer a password prompt,
and the command hangs until it times out. Don't try to pipe a password in. Ask the human to run
the single install command themselves — in Claude Code they can run it in this session by
prefixing it with `!` — then carry on from step 2.

## Step 1 — install

One command, no `sudo` prefix — it builds the server first, then installs:

```sh
make install
```

**No Go toolchain on the box?** `make fetch && make install` instead. `make fetch` downloads the
release binary for this architecture and verifies its checksum before writing it.

`make install` is correct whether you are root or a normal user; the recipe adds `sudo` only when
needed. **Do not write `sudo make install`** — that nests sudo, which resets `SUDO_USER` to root,
and the installer will refuse (it used to silently produce a root-owned web shell). Pass extras as
make variables: `make install FORCE=1` to also replace an already-installed `wtd`,
`make install WT_USER=<login>` to choose the account.

**Everything that can refuse, refuses before the first byte is written** — no server binary, or a
config setting `wtd` cannot honor (see the failure-mode table). So a refusal here leaves whatever
was already installed running exactly as it was. That is the design; do not "fix" a refusal by
working around it.

**Which account will the terminal run as?** `install.sh` prints `service user: X (from Y)`. It
resolves `WT_USER` → `$SUDO_USER` → owner of the checkout, and **refuses** if that lands on root
without being named explicitly. On a root-login box with a root-owned clone you will get that
refusal; the fix is `make install WT_USER=<their login>`. Pick the human's normal account — that's
whose files, keys, and shell history the sessions will have.

## Step 2 — tell the human what they now have

Don't skip this and don't soften it: the service is a **writable, unauthenticated shell**. Anyone
who can reach the port gets that account's shell, no password. There is no app-layer auth by
default and that is deliberate. The only thing protecting it is which network interface it is
bound to. So confirm the bind target with them rather than assuming.

**If you are installing this on someone's machine for them, the disclosure is part of the job.**
They asked for a browser terminal; what they are getting is an unauthenticated shell as their own
user, reachable by anything that can route to the bound interface. Say that in plain words, name
the interface it ended up on, and confirm that's what they intended. Two answers should stop you
and send you back to them: they can't tell you what network the box is on, or they ask for
`0.0.0.0` / a public IP / a port forward. Neither is a config question; both change who can reach
their shell.

## Step 3 — bind target, the one setting that matters

The shipped default is `WT_BIND=tailscale` in `/etc/ttyd-ify/config`, which resolves this node's
tailnet IP.

| Their situation | Set `WT_BIND` to | Reachable from |
|---|---|---|
| On a tailnet (`tailscale ip -4` printed an address) | `tailscale` (default — nothing to do) | their other tailnet devices |
| No tailnet, wants access from elsewhere | `localhost`, then an SSH tunnel (`ssh -L 7681:127.0.0.1:7681 host`) | only through that tunnel |
| A WireGuard/VPN interface | the interface name, e.g. `wg0` | that VPN |

`resolve_ip` accepts only those forms plus a literal IP, and **never** a wildcard. If you find
yourself wanting `0.0.0.0`, stop — that publishes an unauthenticated shell to every network the
box is on. `install.sh` never overwrites an existing `/etc/ttyd-ify/config`, so editing it is safe
to redo.

After editing the config: `sudo systemctl restart wt.service`.

## Step 4 — verify. Do not report success without this

The service can be `active` while being unreachable (e.g. `WT_BIND=tailscale` with tailscaled
down makes `wt-serve` exit 1 and systemd retry).

```sh
systemctl is-active wt.service                       # → active
# Resolved bind line. Do NOT use `-n 20` here — a busy server logs several lines per client
# connection, so the bind line scrolls out and you get empty output, which reads like a
# failure when the service is fine.
journalctl -u wt.service --no-pager | grep 'wt-serve: wtd on' | tail -1
# That line carries the address AND port the server actually bound, which is the only
# trustworthy source for both — do not assume 127.0.0.1 (the default is WT_BIND=tailscale)
# and do not re-parse WT_PORT out of the config.
BOUND=$(journalctl -u wt.service --no-pager | grep -o 'wtd on [0-9.]*:[0-9]*' | tail -1 | cut -d' ' -f3)
curl -fsS "http://$BOUND/token"                      # → {"token": ""}
curl -fsS "http://$BOUND/api/v1/meta"                # → JSON incl. version + features[]
ps -o user=,cmd= -C wtd                              # → the intended user
```

`/api/v1/meta` is the load-bearing one: it proves you reached `wtd` and not some other thing on
that port, and its `version` is the honest answer for which build is *running* (the install skips
an existing `wtd` unless `FORCE=1`).

Tell the human the URL (`http://$BOUND`) and which account it lands them in.

## Known failure modes, so you don't diagnose them from scratch

| Symptom | Cause | Fix |
|---|---|---|
| `could not resolve WT_BIND='tailscale'` | tailscaled down, or no tailnet | `tailscale status`; or set `WT_BIND=localhost` |
| Unit restarts every 3s | `wt-serve` exits 1 — almost always bind resolution | `journalctl -u wt.service -n 20` |
| `bind: address already in use` | something already on `WT_PORT` (often a second copy, or a pre-#23 ttyd still running) | `ss -lntp \| grep "$PORT"` |
| Installer refuses, "resolved WT_USER=root" | `sudo make install`, or root-owned clone | `make install WT_USER=<login>` |
| Installer refuses, "no wtd binary" | nothing built or fetched yet; there is no fallback server | `make build` (or `make fetch`), then `make install` |
| Installer refuses, `WT_AUTH` / `WT_TTYD_ARGS` | the config asks for something `wtd` cannot do | clear that line — see below — then re-run |
| `/api/v1/meta` version is not what you just built | `install.sh` skips an existing `wtd` | `make install FORCE=1`, then restart |
| Terminal renders but keystrokes do nothing | *was* a missing `ttyd -W`; `wtd` hardcodes writable, so this is now a bug — report it | — |

## Upgrading a box that predates #23

Before #23 there were two servers: ttyd on `WT_PORT` and `wtd` opt-in on `WT_WEB_PORT`. Now there
is one — `wtd` on `WT_PORT` — and `make install` performs the switch: it replaces the launcher,
rewrites `wt.service`, and stops/removes the retired `wt-web.service` and its launcher. Two things
to know:

- **It refuses up front if `/etc/ttyd-ify/config` sets `WT_AUTH` or `WT_TTYD_ARGS`.** `wtd`
  implements neither, and both can carry an access restriction, so it will not quietly replace a
  server that honored them. The fix is one config edit: clear the line, use network-layer access
  control (tailnet ACL, source-IP allowlist, `WT_BIND=localhost` + SSH tunnel), re-run. **Do not
  advise the human to keep the old release instead** — say plainly that app-layer auth is gone and
  what replaces it.
- **A stale `WT_WEB_PORT` is warned about and ignored.** `WT_PORT` decides the port. Deleting the
  line is tidiness, not a fix.
- **The install stops `wt-web.service` without asking, and that may be the connection you are
  on.** Removing a second unauthenticated port is the safe direction, so it is not a prompt — but
  if your own session arrived through it, your output ends mid-install. The dtach sessions survive
  and the install completes; you just cannot see it. The pre-flight prints a warning when it
  detects this, and the fix is to reconnect on `WT_PORT` afterwards. If you are driving an install
  through the terminal it is about to retire, tell the human that first.

The port does not change for clients: 7681 was already the iOS client's default, which is why the
migration moved `wtd` onto it rather than the other way round.

## Already installed: changing a live deployment

This applies when `systemctl is-active wt.service` printed `active` — the case on Sam's own boxes,
where the checkout sits on the same machine as the running service. Editing `bin/` does **not**
affect that service; it runs the installed copies at `/usr/local/bin/wt{,-serve}`.

**Installing does not restart the service.** `install.sh` ends with `systemctl enable --now`, which
is a no-op on an already-active unit, so new code is on disk and not running until an explicit
restart. The install says so when it detects this. Comparing files does not answer the question —
the shell scripts are always overwritten, so they match the repo the moment the install finishes,
running or not. What is live:

```sh
systemctl show -p ActiveEnterTimestamp wt.service    # older than your install → not running it
# Ask the server itself. Derive host and port from the journal rather than assuming
# 127.0.0.1: the shipped default is WT_BIND=tailscale, and the server binds ONE address, so
# a localhost curl on a tailnet box is "connection refused" on a perfectly healthy service.
BOUND=$(journalctl -u wt.service --no-pager | grep -o 'wtd on [0-9.]*:[0-9]*' | tail -1 | cut -d' ' -f3)
curl -fsS "http://$BOUND/api/v1/meta"                # `version` is the running Go build
```

**Restarting is disruptive and can cut you off mid-command.** `systemctl restart wt.service` kills
the server, which drops every connected client: a phone attached right now, and — if your own
session arrived through the web terminal — the terminal you are typing in. The `dtach` sessions and
everything running inside them survive and reattach, so nothing is lost, but your command chain
dies at that line. Put a restart in its own step, never in the middle of a `&&` chain whose later
output you need. **Ask the human before restarting**; someone may be mid-task. Work out whether
your own session came through the server first:

```sh
# Walk up from this shell: a dtach master's parent tells you whether a server is above you.
p=$$; while [ "$p" != 1 ]; do tr '\0' ' ' < /proc/$p/cmdline; echo; p=$(awk '{print $4}' /proc/$p/stat); done | grep -E 'wtd|dtach' | head
```

**Running `install.sh` at all rewrites the live unit — `NO_ENABLE=1` does not make it safe.**
`NO_ENABLE=1` only skips `enable --now`; step 4 still `sed`s `/etc/systemd/system/wt.service` and
runs `daemon-reload`. So testing the installer with a deliberately odd `WT_USER` leaves that value
in the live unit, primed to take effect on the next restart — the running process keeps the old
user, so nothing looks wrong. If you must exercise `install.sh`, check
`rg '^User=' /etc/systemd/system/wt.service` afterwards. There is no `PREFIX` sanity check either:
point it at a nonexistent directory and the script dies at step 2 under `set -e`, which happens to
be *before* the unit is written.

## Uninstall

```sh
make uninstall            # keeps /etc/ttyd-ify
make uninstall PURGE=1    # removes it too
```
