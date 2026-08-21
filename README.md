# ttyd-ify

A browser terminal for your Linux box, with a **session picker** — pick from persistent
`dtach` sessions or spawn a new one, all from a web page. `wtd` (a small Go server in this
repo) serves it, [`dtach`](https://github.com/crigler/dtach) keeps the sessions alive, and
systemd manages the whole thing. No tmux, no multiplexer chrome.

Designed to be reached over a private network (a [Tailscale](https://tailscale.com)/
[Headscale](https://headscale.net) tailnet, a VPN, or `localhost` via a tunnel) so you can
get a shell on your dev box from a laptop or phone browser.

> ### ⚠ Security: this serves a writable, unauthenticated shell
> Anyone who can reach the port gets a shell as the service user. **ttyd-ify is only as
> private as the interface it binds to.** It binds to a trusted interface by default
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
worse than not starting. `WT_WEB_PORT` was `wtd`'s port while both servers ran side by side;
it is warned about and ignored. Your config keeps whatever it already had — `make install`
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
- **`GET /openapi.json`** — the served spec, so the contract is machine-readable rather
  than folklore. Full documents live in [`api/`](api/).
- **`GET /api/v1/sessions/{name}/narration`** — one or two sentences saying what a session
  just did, written to be *spoken* rather than read. For using a session hands-free; see
  below.

`dtach` still owns session persistence, deliberately: a dtach session's parent is
independent of the server, so restarting `wtd` drops clients but leaves sessions running.

### Narration — hearing what a session is doing

Reading a terminal aloud does not work. A speech engine says "hash one eight seven" for `#187`
and recites URLs a character at a time, and an agent's closing message runs to a couple of
hundred words where two sentences were wanted. So the summary is written by the agent's own side:
[`bin/wt-narrate`](bin/wt-narrate) runs as a Claude Code hook when a turn ends, and `wtd` serves
the file it writes. Nothing in the server calls a model, and no API key goes near
`/etc/ttyd-ify/config`.

`make install` puts the script in place but **does not enable it** — the hook belongs in your own
`~/.claude/settings.json`, and the snippet to paste is in the script's header. It needs a
`-state-dir`, which `wt-serve` passes from systemd automatically.

A session with nothing to say answers `404`, which is the normal case and not an error.

Two things to know before you go looking for a bug:

- **Sessions created before you installed this cannot narrate.** A `dtach` master captures its
  environment once and keeps it for the session's whole life, so a session that started without
  `WT_SESSION` and `WT_NARRATION_DIR` will never have them — the same trap that makes a session
  born without `TERM` stay colourless. It fails silently, because from inside there is nothing to
  distinguish it from not being a web session. Recreate the session.
- **The browser voice needs the page open and the screen on.** iOS stops speech synthesis when the
  phone locks, and throttles a background tab's timers to about once a minute, so the poll stops
  too. Good enough to judge whether the summaries are worth hearing; not yet good enough for a
  pocket. Real audio on the lock screen is
  [#119](https://github.com/heysamtexas/ttyd-ify/issues/119).

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
served from the same URL cannot tell you. Verifying it needs the `gh` CLI; `make fetch` uses it when
it is installed and says loudly when it is not, because requiring it would strand the machine this
command exists for. A verification that *fails* is fatal.

Releases before
[#85](https://github.com/heysamtexas/ttyd-ify/issues/85) have no attestation, and a missing one is
refused too — it looks the same as a substituted binary from the client side. For those tags:
`WT_FETCH_ALLOW_UNSIGNED=1 make fetch TAG=v0.3.0`, or build from source.

**Upgrading from a box that still ran ttyd?** `make install` replaces it in place on the same
port and cleans up the retired second unit. It refuses up front — before writing anything, so
your working server keeps running — if your config sets `WT_AUTH` or `WT_TTYD_ARGS`. One
thing it deliberately does not do is restart a running service, because that drops every
connected client; it tells you the command and leaves it to you.

## Security

ttyd-ify does **not** authenticate at the app layer, on purpose:

- It's meant to sit behind a private network. Put access control **there**: tailnet ACLs, a
  VPN, an SSH tunnel to `localhost`, or a host firewall source-IP allowlist.
- There is **no HTTP basic auth**. ttyd had it (`WT_AUTH`), it was off by default because
  **it breaks Safari and all iOS browsers** — WebKit does not send basic-auth credentials on
  the WebSocket upgrade, so the terminal never connects — and `wtd` does not implement it. If
  `WT_AUTH` is set in your config, both the install and the server refuse to start rather
  than serve an unauthenticated shell to someone who asked for a password. Clear it and use
  network-layer control. (It is a real future option, not a dead end:
  [#27](https://github.com/heysamtexas/ttyd-ify/issues/27).)
- Default bind is a specific trusted interface; it will refuse to help you bind `0.0.0.0`.

If your login shell auto-starts tmux/screen, see [`docs/bashrc-snippet.sh`](docs/bashrc-snippet.sh)
to keep it from recursing inside web sessions (`wtd` sets `WT=1`).

Every session also gets `WT_SESSION=<name>`, which is the only thing in the environment that says
*which* session you are in. Useful in a shell prompt, and it is how a program running inside a
session reports on itself. An argless connection is not a session and does not get it.

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
