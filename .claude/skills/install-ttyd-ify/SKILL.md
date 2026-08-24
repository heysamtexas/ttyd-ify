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
make variables: `make install WT_USER=<login>` chooses the account. There is no flag for
"actually replace the binaries" — they always match the checkout.

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
that port, and its `version` is the honest answer for which build is *running* — the install
replaces the binary on disk but never restarts a running service, so those two can differ.

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
| `/api/v1/meta` version is not what you just built | installing does not restart, so the old process is still serving | `sudo systemctl restart wt.service` (drops clients) |
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
affect that service; it runs the installed copies at `/usr/local/bin/wt-serve` and `wtd`.

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

**A restart drops every client's connection. Whether it kills YOUR shell is a separate question,
and the answer is usually no.** These were run together as one warning and the combination was
false — the old text said the `dtach` sessions and everything inside them survive, and then that
your command chain dies, which cannot both be true when your agent is one of the things inside
them (#76).

**Ask the human before restarting.** That stands on its own and has nothing to do with your
survival: someone else may be mid-task, and `tailscale status` lists ~7 machines that can reach
`:7681`. A phone attached right now loses its connection.

**Your shell's fate is decided by ancestry, not by how you connected.** `wt.service` sets
`KillMode=process` (#21), so a restart signals only `wtd`:

| What sits between your shell and PID 1 | Survives a restart? | Why |
|---|---|---|
| a `dtach` master | **yes** | `wtd` never parented it, so `KillMode=process` leaves the whole subtree alone |
| `wtd` itself | **no**, deliberately | the connection's teardown signals the process *group* with escalation (`cmd/wtd/ws.go`, `runTerminal`) |
| neither | unaffected | SSH, or a checkout on a box where nothing is attached |

**Do not infer this from the connection shape.** A named (`?arg=`) connection is *usually* the
first row and an argless one is *always* the second, but a named connection whose name
`validateAttachName` rejects gets a shell `wtd` parents itself, kept named so it still shares and
replays (`cmd/wtd/attach.go`, `terminalCommand`). A client reaches that with a `/`, `..`, or an
over-long session arg. Ancestry is ground truth; the shape is a proxy with exceptions.

```sh
# Does restarting wt.service kill THIS shell?
v="unaffected: not under wt.service"; p=$$
while [ -n "$p" ] && [ "$p" != 1 ]; do
  case "$(cat /proc/$p/comm 2>/dev/null)" in
    dtach) v="SURVIVES: dtach master $p is above you"; break;;
    wtd)   v="DIES: wtd $p is above you, no dtach between"; break;;
  esac
  p=$(awk '/^PPid:/{print $2}' /proc/$p/status 2>/dev/null)
done; echo "$v"
```

Three things in there are corrections to the one-liner this replaces, all reproduced on a real box
rather than reasoned about:

- **`comm`, not `cmdline`.** A wrapper shell's `cmdline` contains the whole script text, so a check
  that greps for `wtd` matches *itself* on the first iteration. `comm` is the executable name only.
- **`PPid:` from `/proc/$p/status`, not field 4 of `/proc/$p/stat`.** `comm` sits in `stat`
  unquoted, so a process named `next-server (v1` shifts every field: field 4 reads `S`, the next
  read fails, and the old loop's `[ "$p" != 1 ]` stays true forever. It hangs, inside your Bash
  tool. There is a live example of such a process on this box.
- **Three outcomes.** SSH matches neither pattern. The old form printed nothing there, and
  "nothing means you die" is the same over-caution in a new place.

**Still put a restart in its own step, and not just because the human's view drops.** Output your
surviving chain writes during the gap is *gone* — not merely unseen. Rings are saved before the
hubs close (`cmd/wtd/main.go`, shutdown), hubs are created lazily when a client joins
(`cmd/wtd/hub.go`), and `dtach` keeps no buffer of its own. So bytes printed between SIGTERM and
the next client connecting are in no ring, no saved file and no scrollback.

**Two costs of a restart that are easy to misread afterwards:**

- **`agentStatus` goes `null` for every session.** It is the one piece of session state `wtd` holds
  that is not derivable from the sockets, and the ring store does not carry it (`cmd/wtd/hub.go`,
  the `status` field). The picker renders it, so a healthy running agent shows blank until it next
  reports — which looks like a dead session and is not.
- **A reconnecting client is told there is a gap.** It emits *"replay includes output saved before
  a server restart; anything printed while it was down is not shown"*. Expected, not a bug.

Before restarting, prefer the server's own verdict on whether sessions will survive over grepping
the unit file — it evaluates the *loaded* unit rather than the template on disk
(`cmd/wtd/survival.go`):

```sh
journalctl -u wt.service --no-pager | grep 'wtd: WARNING' | tail -3   # silence is the good answer
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
