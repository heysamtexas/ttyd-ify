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
courtesy notes for getting `ttyd` + `dtach`, not a portability promise — `install.sh`
hard-requires systemd, so the `brew` path cannot complete on macOS.

**First work out which situation you are in:**

```sh
systemctl is-active wt.service 2>/dev/null   # "active" → already installed, see the last section
```

## Step 0 — check the three things that actually decide success

Run this verbatim; each line prints a verdict, none of them change anything:

```sh
systemctl --version >/dev/null 2>&1 && echo "systemd: ok" || echo "systemd: MISSING — stop, this repo cannot install here"
command -v ttyd dtach >/dev/null 2>&1 && echo "deps: present" || echo "deps: absent — fine on Debian/Ubuntu (install.sh apt-gets them), otherwise install ttyd+dtach first"
sudo -n true 2>/dev/null && echo "sudo: passwordless" || { [ "$(id -u)" = 0 ] && echo "sudo: running as root already" || echo "sudo: WILL PROMPT — see below"; }
tailscale ip -4 2>/dev/null | head -1 || echo "no tailscale — you must change WT_BIND, see step 3"
```

**If `sudo` will prompt, you cannot complete the install.** You can't answer a password prompt,
and the command hangs until it times out. Don't try to pipe a password in. Ask the human to run
the single install command themselves — in Claude Code they can run it in this session by
prefixing it with `!` — then carry on from step 2.

## Step 1 — install

One command, no `sudo` prefix:

```sh
make install
```

`make install` is correct whether you are root or a normal user; the recipe adds `sudo` only when
needed. **Do not write `sudo make install`** — that nests sudo, which resets `SUDO_USER` to root,
and the installer will refuse (it used to silently produce a root-owned web shell). Pass extras as
make variables: `make install FORCE=1` to overwrite existing binaries, `make install WT_USER=<login>`
to choose the account.

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
# Resolved bind line. Do NOT use `-n 20` here — libwebsockets logs several lines per
# client connection, so the bind line scrolls out and you get empty output, which reads
# like a failure when the service is fine.
journalctl -u wt.service --no-pager | grep 'wt-serve: ttyd on' | tail -1
IP=$(journalctl -u wt.service --no-pager | grep -o 'ttyd on [0-9.]*' | tail -1 | cut -d' ' -f3)
curl -fsS "http://$IP:7681/token"                    # → {"token": ""}
ps -o user=,cmd= -C ttyd                             # → the intended user, and -W -a present
```

Tell the human the URL (`http://<ip>:7681`) and which account it lands them in.

## Known failure modes, so you don't diagnose them from scratch

| Symptom | Cause | Fix |
|---|---|---|
| `could not resolve WT_BIND='tailscale'` | tailscaled down, or no tailnet | `tailscale status`; or set `WT_BIND=localhost` |
| Unit restarts every 3s | `wt-serve` exits 1 — almost always bind resolution | `journalctl -u wt.service -n 20` |
| `lws_socket_bind: ERROR ... port 7681` | something already on the port (often a second copy) | `ss -lntp \| grep 7681` |
| Installer refuses, "resolved WT_USER=root" | `sudo make install`, or root-owned clone | `make install WT_USER=<login>` |
| Binaries "installed" but behavior unchanged | `install.sh` skips existing binaries | `make install FORCE=1`, then restart |
| Terminal renders but keystrokes do nothing | `ttyd -W` missing | don't remove `-W` from `bin/wt-serve` |

## The wtd path is opt-in

`install.sh` installs both servers but enables only ttyd. To use the Go server instead:

```sh
sudo systemctl enable --now wt-web.service     # wtd on WT_WEB_PORT (7683)
```

A box with no Go toolchain still installs the shell parts and tells you where to fetch a release
binary.

## Already installed: changing a live deployment

This applies when `systemctl is-active wt.service` printed `active` — the case on Sam's own boxes,
where the checkout sits on the same machine as the running service. Editing `bin/` does **not**
affect that service; it runs the installed copies at `/usr/local/bin/wt{,-serve}`. Check what is
actually live before debugging anything:

```sh
for b in wt wt-serve; do diff -q "bin/$b" "/usr/local/bin/$b" >/dev/null && echo "$b: live copy matches repo" || echo "$b: repo differs from live copy"; done
```

**Installing new binaries does not restart the service.** `install.sh` ends with
`systemctl enable --now`, which is a no-op on an already-active unit, so a code change is on disk
but not running until an explicit restart. Confirm what's live with
`diff bin/wt-serve /usr/local/bin/wt-serve`, not by reading the install log.

**Restarting is disruptive and can cut you off mid-command.** `systemctl restart wt.service` kills
`ttyd`, which drops every connected client: a phone attached right now, and — if your own session
arrived through ttyd — the terminal you are typing in. The `dtach` sessions and everything running
inside them survive and reattach, so nothing is lost, but your command chain dies at that line.
Put a restart in its own step, never in the middle of a `&&` chain whose later output you need.
**Ask the human before restarting**; someone may be mid-task. Work out which server your own
session came through first:

```sh
# Walk up from this shell: a dtach master's parent tells you which server, or none.
p=$$; while [ "$p" != 1 ]; do tr '\0' ' ' < /proc/$p/cmdline; echo; p=$(awk '{print $4}' /proc/$p/stat); done | grep -E 'ttyd|wtd|dtach' | head
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
