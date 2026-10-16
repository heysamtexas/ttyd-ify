# ttyd-ify

A browser terminal for your Linux box, with a **session picker** — pick from persistent
`dtach` sessions or spawn a new one, all from a web page. `wtd` (a small Go server in this
repo) serves it, [`dtach`](https://github.com/crigler/dtach) keeps the sessions alive, and
systemd manages the whole thing. No tmux, no multiplexer chrome.

Designed to be reached over a private network (a [Tailscale](https://tailscale.com)/
[Headscale](https://headscale.net) tailnet, a VPN, or `localhost` via a tunnel) so you can
get a shell on your dev box from a laptop or phone browser.

> ### ⚠ Security: this serves a writable shell, unauthenticated unless you set a password
> Without `WT_PASSWORD`, anyone who can reach the port gets a shell as the service user. With
> it, anyone who can reach the port can try to guess it. **ttyd-ify is only as private as the
> interface it binds to.** It binds to a trusted interface by default
> (`WT_BIND=tailscale`) and **never** to `0.0.0.0`. Do not open a firewall port to it and
> do not expose it to the public internet. Control access at the network layer (tailnet
> ACLs, a VPN, an SSH tunnel, or a source-IP allowlist). See [Security](#security).

## How it works

```
browser  ──ws──▶  wtd (bound to your tailnet/localhost)  ──▶  dtach session
```

`wtd` serves a session list at `/`, and opening one attaches to its `dtach` socket in
`~/.dtach/`. Because sessions live in `dtach` and not in the server, they keep running after you
close the tab — and after you restart the service. Reconnect and they're still there, with recent
output replayed so you land on context instead of a blank screen. The replayed tail survives a
service restart too — saved to `/run/wt` on shutdown, so a full stop or a reboot clears it — and
output printed while the server itself was down is beyond it either way.

## Requirements

- Linux with **systemd**
- `dtach` (installed for you on Debian/Ubuntu; see below for other distros)
- A private network to bind to (Tailscale/Headscale, a VPN, or localhost+tunnel)
- Either a Go toolchain (`make build`) or one command to fetch a checksum-verified release
  binary (`make fetch`) — `make install` will tell you which you need

## Install

```sh
git clone https://github.com/heysamtexas/ttyd-ify
cd ttyd-ify
make install                 # installs deps + binaries + a systemd unit, runs as you
```

`make install` builds the server first if you have Go. **No Go on the box?** Run
`make fetch` first — it downloads a checksum-verified release binary. There is no fallback
server, so `make install` refuses rather than leaving you with a unit that cannot start.

Run `make install` **without** `sudo` — it calls `sudo` itself, and prefixing another one
hides your username, so the installer would resolve the service user as root (it refuses
rather than doing that). To pick a different account: `make install WT_USER=alice`.

The installer prints `service user: <name>` — that's whose shell the terminal hands out.

Then edit `/etc/ttyd-ify/config` (bind target, port) and:

```sh
sudo systemctl restart wt.service
```

Open `http://<your-bind-ip>:7681` from a device on the same private network. Confirm it came
up with `journalctl -u wt.service --no-pager | grep 'wt-serve: wtd on' | tail -1`, which logs
the address it bound.

> **Installing this with an agent?** (e.g. "Claude, install this repo on my box.")
> [`CLAUDE.md`](CLAUDE.md) is written for that reader: preflight checks, the one correct
> install command, what to verify, and a table of known failure modes. Point it there
> first — it's more precise than this README about the traps.

Non-Debian distros: install the dep first (`sudo dnf install dtach`, `sudo pacman -S dtach`,
`brew install dtach`), then `make install`.

## Configure

`/etc/ttyd-ify/config`:

| Key            | Default          | Meaning |
|----------------|------------------|---------|
| `WT_BIND`      | `tailscale`      | `tailscale` (auto tailnet IP), `localhost`, an interface (`eth0`, `wg0`), or a literal IP |
| `WT_PORT`      | `7681`           | Port the server listens on |
| `WT_PROJECTS`  | `/etc/ttyd-ify/projects` | Optional "new session" shortcuts |
| `WT_WEB_BIN`   | `/usr/local/bin/wtd` | Path to the `wtd` binary |
| `WT_REPLAY_BYTES` | `262144`      | Recent output `wtd` replays on attach, per session. `0` disables replay |
| `WT_PASSWORD`  | *(unset)*        | Password for **every** route — see [Password](#password-optional). Unset means none |
| `WT_FILES_ROOT` | *(unset)*      | Directory served read-only at `/files/`. Needs `WT_PASSWORD`. Unset means no `/files/` at all — see [Browse files](#browse-files-optional) |
| `WT_UPLOAD_DIR` | `~/uploads`    | Where the terminal page's upload button puts files. Absolute path; created on first upload. A taken name gets `-1`, `-2`, … — an upload never overwrites |

**`WT_BIND` has to resolve on the machine you are installing on.** The shipped default is
`tailscale`, so a box without Tailscale up would get a server that starts, cannot bind, and is
restarted every three seconds. `make install` resolves the value first and **refuses**, before
writing anything, rather than reporting a successful install over a dead service
([#80](https://github.com/heysamtexas/ttyd-ify/issues/80)). On a box with no tailnet, write the
config before installing — the install never overwrites one:

```sh
sudo mkdir -p /etc/ttyd-ify
sudo sh -c "sed 's|^WT_BIND=.*|WT_BIND=localhost|' etc/config.example > /etc/ttyd-ify/config"
make install
```

`localhost` means the terminal is reachable only through an SSH tunnel. An interface name or an
address this box holds work the same way. After starting, the install also checks the service is
*still* running a few seconds later and fails loudly with the log if it is not — `systemctl` reports
a `Type=simple` unit as started the moment it execs, which is how a dying server used to look
healthy.

Three keys were retired with `ttyd` (see [Security](#security) for the first two):
`WT_AUTH` and `WT_TTYD_ARGS` make the install **and** the server refuse to start if you set
them, because `wtd` implements neither and silently dropping a restriction you configured is
worse than not starting — move a `WT_AUTH` password to `WT_PASSWORD`. `WT_WEB_PORT` was
`wtd`'s port while both servers ran side by side; it is warned about and ignored. So is
`WT_FILES_PASSWD`, the old `/files/`-only password file, which `WT_PASSWORD` replaced. Your config keeps whatever it already had — `make install`
never overwrites `/etc/ttyd-ify/config` — so it tells you up front if one of these is set.

`make install` writes the config **`0640 root:<service user>`** — root-owned, readable only by
the account the terminal runs as, because `WT_BIND` is what limits who can reach the shell. Two
consequences: read it with `sudo`, and **re-run the install if you change `WT_USER`**, or the
new service user cannot read it. `wt-serve` refuses to start in that case rather than silently
falling back to `WT_BIND=localhost` and logging an address you never configured. A *missing*
config is fine and stays quiet — the defaults are the answer there.

`/etc/ttyd-ify/projects` — optional `name /path` per line. A new session whose name matches a
shortcut starts `cd`'d into that path, whether you create it from the browser picker or by
deep-linking it.

## Use

- **Pick a session**: open `http://<host>:7681/` for the list, and click one to attach. Detach
  with **Ctrl-\\**; the session keeps running.
- **Direct attach**: `http://<host>:7681/?arg=<name>` opens session `<name>` straight away,
  creating it if it does not exist — so a saved link works before the session has ever existed,
  and after a reboot. Clients that speak the socket directly use
  `ws://<host>:7681/ws?arg=<name>`. Handy for terminal apps that let you save a per-session URL
  (e.g. on iOS).
- **From a shell on the box**: `dtach -a ~/.dtach/<name>.sock` attaches to the same session over
  SSH. `wtd` and `dtach` share the socket directory, so either route reaches the same shell.

### Password (optional)

One password in `/etc/ttyd-ify/config` covers every route — the terminal, `/ws`, the session
API and `/files/` — except `/healthz`. It is off unless you set it:

```sh
sudoedit /etc/ttyd-ify/config            # add WT_PASSWORD='<12+ characters>'
sudo systemctl restart wt.service        # drops every connected client; sessions survive
```

- **Rules**, or the install and the server refuse to start: at least 12 characters, printable
  ASCII only, no space at either end. `WT_PASSWORD=` with nothing after it is refused too. Quote
  it if it has spaces or shell characters.
- **Browsers** get a sign-in page and a 30-day cookie. A restart keeps them signed in.
- **To sign every browser out**, change the password and restart. To do it without changing the
  password: `sudo rm /run/wt/session.key && sudo systemctl restart wt.service` (a reboot does the
  same). There is no per-browser sign-out.
- **The cookie is not private to this port.** Browsers scope cookies by host name, not port, so
  a signed-in browser also sends `wtd_session` to every other HTTP service it reaches at the same
  address — a dev server, a k3s NodePort — and any of them can read it and replay it against
  `wtd` for up to 30 days. Do not sign in from a browser that visits untrusted services on this
  box's address. Serving `wtd` under a host name of its own would fix this and is not done here.
- **The iOS app** needs a build that sends the password (ios-claude-terminal#15). An older build
  gets `401` on every request and cannot connect, so **update the app before you set this**.
- **curl and scripts**: `curl -H "Authorization: Bearer $PW" …`, or `curl -u ":$PW" …`.
- It is stored in plaintext in a file only root and the service user can read. That user already
  has the shell this protects, so there is nothing to hide from them.

A wrong password costs about a second, and each address can have only a few checks in flight. That is the only
brute-force defence, which is why the length rule exists.

### Browse files (optional)

`wtd` can also serve one directory tree **read-only** at `http://<host>:7681/files/`: directory
listings, files shown inline, and markdown rendered with GitHub-flavoured tables and mermaid
diagrams. It is off unless you set a root, and the picker shows a `files` link only when it is
on. It requires the [password](#password-optional): **the server refuses to start** with
`WT_FILES_ROOT` set and no `WT_PASSWORD`.

```sh
sudoedit /etc/ttyd-ify/config            # WT_PASSWORD=... and WT_FILES_ROOT=/home/<you>/src
sudo systemctl restart wt.service        # drops every connected client; sessions survive
```

Its links are absolute under `/files/`, so unlike the rest of `wtd` it does not work behind a
path-stripping reverse proxy.

## wtd, the Go server

`wtd` replaced `ttyd` as of [#23](https://github.com/heysamtexas/ttyd-ify/issues/23). It
speaks ttyd's WebSocket protocol exactly — verified on every change by a CI job that runs the
real ttyd beside it and diffs them frame by frame — so **existing clients work against it
unchanged**: same port, no app rebuild, no profile edit. And it adds what ttyd cannot:

- **Replay on attach** — reattaching to a session shows its recent output instead of a
  blank screen. `dtach` keeps no screen buffer, so `wtd` holds the attachment and remembers
  the tail (`WT_REPLAY_BYTES`; `0` turns it off).
- **`GET /api/v1/sessions`** — a JSON list of sessions with live/idle state and working
  directory, so a client can *discover* sessions instead of being told a name. Plus
  create and delete.
- **A browser session picker at `/`**, and a terminal at `/?arg=<name>`.
- **Help at `/help`** — a living FAQ of terminal gotchas (newlines in Claude Code, the
  detach key, replay limits). The terminal page shows it behind a `?` button; add new
  findings to `cmd/wtd/web/help.html`.
- **`GET /api/v1/host`** — load average against the CPU count, memory, memory/CPU/IO stall,
  disk, and what each session's process tree costs. The terminal page renders it in a panel
  behind a `≡` button, next to `?`. It exists because sessions are cheap to start and nothing
  used to say what they cost: thirteen of them once starved this box while every external
  health check still reported it healthy. The picker shows the same figures per session, plus the
  remaining headroom, because the decision they inform — start another session, or don't — is made
  there.

  **Sizing, since the numbers surprise people:** a session holding a coding agent costs a few
  hundred megabytes, and they accumulate. Measured on this box in September 2026: seven sessions,
  3.3 GiB, averaging ~477 MiB each. There is no limit on how many you can start and no swap on
  this host, so the ceiling arrives without a slow phase — budget roughly `N × 0.5 GiB` and leave
  several gigabytes for the machine itself.
- **`GET /api/v1/sessions/{name}/prompts`** — the messages you have sent to the agent in a
  session, listed in the terminal panel so finding what you last asked for is not a scrollback
  hunt. Needs the optional hook below; without it the list is simply empty.
- **`GET /openapi.json`** — the served spec, so the contract is machine-readable rather
  than folklore. Full documents live in [`api/`](api/).

### Recording your prompts (optional)

`install.sh` puts `wt-prompt-hook` in `/usr/local/bin` but wires up nothing. To start recording,
add this to `~/.claude/settings.json` for the account the service runs as:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "/usr/local/bin/wt-prompt-hook" } ] }
    ]
  }
}
```

That is the whole setup. Three things worth knowing:

- **It only records inside a web session.** The hook exits immediately when `WT_SESSION` is unset,
  so an ordinary SSH shell or a local terminal writes nothing.
- **It cannot break your agent.** The hook always exits 0, deliberately: a `UserPromptSubmit` hook
  that fails can block the prompt from being submitted, so every failure inside it — no `python3`,
  a full disk, a payload it cannot parse — is silent and successful. Run it by hand with
  `WT_DEBUG=1` if prompts are not appearing.
- **The one trap:** the hook writes to `/run/wt/prompts`, matching the `RuntimeDirectory` the
  shipped unit provides. If you passed `wtd` a different `-state-dir`, set `WT_PROMPT_DIR` to that
  path plus `/prompts` or the hook will write where nothing reads.

Only what you type is recorded, never the agent's replies. It lives on tmpfs beside the replay
buffers, mode 0600, and is gone at reboot.

`dtach` still owns session persistence, deliberately: a dtach session's parent is
independent of the server, so restarting `wtd` drops clients but leaves sessions running.

`make install` needs the binary in place, so build it (or fetch it) first:

```sh
make build      # needs Go; run WITHOUT sudo
make install
```

No Go on the box? `make fetch` downloads the release binary for your architecture and
**verifies its checksum** before writing it, then `make install` as usual.

```sh
make fetch                  # the latest release
make fetch TAG=v0.2.0       # a specific one — how you roll back without a compiler
```

[`CHANGELOG.md`](CHANGELOG.md) says what each tag changed, so you can pick one on purpose. It is
also what the release page shows.

**With Go on the box, `make install` does not install what you fetched.** It runs `make build`
first whenever `go` is present, and that writes `./wtd` — the same path `make fetch` wrote — so the
verified download is replaced by a local rebuild. Both are stamped from `git describe`, so on a
clean checkout at a tag they report the *same* version and neither `wtd -version` nor
`/api/v1/meta` can tell you which one is running. To deploy the release artifact, skip the
wrapper:

```sh
make fetch TAG=v0.6.1       # verified download → ./wtd
sudo ./install.sh           # installs ./wtd as-is; `make install` would rebuild over it
```

Two things it checks, and they are not the same check. The **checksum** travels with the binary, so
it proves the download arrived intact. The **build provenance attestation** is signed by GitHub and
proves the binary came from this repo's release workflow at a specific commit — the thing a checksum
served from the same URL cannot tell you. Verifying it needs a `gh` that can actually run
`attestation verify`, which means new enough to have the subcommand — Ubuntu 24.04's is not — and
authenticated, since reading an attestation is an API call. When it can't, `make fetch` says so
loudly, names which of those is missing, and proceeds on the checksum: requiring the GitHub CLI
would strand the machine this command exists for. A verification that actually *runs* and *fails*
is fatal, and says something quite different.

Releases before
[#85](https://github.com/heysamtexas/ttyd-ify/issues/85) have no attestation, and a missing one is
refused too — it looks the same as a substituted binary from the client side. For those tags:
`WT_FETCH_ALLOW_UNSIGNED=1 make fetch TAG=v0.3.0`, or build from source.

**Upgrading from a box that still ran ttyd?** `make install` replaces it in place on the same
port and cleans up the retired second unit. It refuses up front — before writing anything, so
your working server keeps running — if your config sets `WT_AUTH` or `WT_TTYD_ARGS`, an
unusable `WT_PASSWORD`, or `WT_FILES_ROOT` without `WT_PASSWORD`. One
thing it deliberately does not do is restart a running service, because that drops every
connected client; it tells you the command and leaves it to you.

## Security

The first access control is the network; the [password](#password-optional) is the second, and
it is off by default:

- It's meant to sit behind a private network. Put access control **there**: tailnet ACLs, a
  VPN, an SSH tunnel to `localhost`, or a host firewall source-IP allowlist. If that network is
  shared — a tailnet with other people's machines on it — also set `WT_PASSWORD`.
- The password is a sign-in page plus a cookie for browsers, and a Bearer header for the app and
  curl. It is **not HTTP basic auth** as ttyd did it (`WT_AUTH`): that breaks Safari and all iOS
  browsers, because WebKit does not send basic-auth credentials on the WebSocket upgrade. If
  `WT_AUTH` is set in your config, both the install and the server refuse to start; move the
  password to `WT_PASSWORD`.
- It travels in plain HTTP. On a tailnet the network encrypts it; on anything else, use an SSH
  tunnel.
- Default bind is a specific trusted interface; it will refuse to help you bind `0.0.0.0`.
- The [file browser](#browse-files-optional) shows files without running any script they
  contain: markdown pages allow only `wtd`'s own scripts, and other files are served sandboxed. A
  README from a cloned repo therefore cannot reach the terminal on the same origin.

If your login shell auto-starts tmux/screen, see [`docs/bashrc-snippet.sh`](docs/bashrc-snippet.sh)
to keep it from recursing inside web sessions (`wtd` sets `WT=1`).

Every session also gets **`WT_SESSION=<name>`**, which is how a program inside a session can
report on itself — an agent hook has no other way to learn which session it is in, since the pty
carries no name. It is unset, not empty, on a connection with no session behind it. Quote it: a
session reached by deep link can be named things the create API would refuse, including names with
spaces.

## Uninstall

```sh
make uninstall            # remove service + binaries, keep /etc/ttyd-ify
make uninstall PURGE=1    # also remove config
```

Running `dtach` sessions and the `dtach` package are left alone. (If the box predates
[#23](https://github.com/heysamtexas/ttyd-ify/issues/23) it may still have a `ttyd` package
installed; nothing uses it, and removing it is up to you.)

## License

MIT — see [LICENSE](LICENSE).
