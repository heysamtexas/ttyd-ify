# ttyd-ify

A browser terminal for your Linux box, with a **session picker** — pick from persistent
`dtach` sessions or spawn a new one, all from a web page. Built on
[`ttyd`](https://github.com/tsl0922/ttyd) + [`dtach`](https://github.com/crigler/dtach),
managed by systemd. No tmux, no multiplexer chrome.

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
browser  ──ws──▶  ttyd (bound to your tailnet/localhost)  ──▶  wt (picker)  ──▶  dtach session
```

`wt` is the start command ttyd runs on each connection: it lists the `dtach` sockets in
`~/.dtach/`, lets you attach/create/detach, and — because sessions live in `dtach` — they
keep running after you close the tab. Reconnect and they're still there.

## Requirements

- Linux with **systemd**
- `ttyd` and `dtach` (installed for you on Debian/Ubuntu; see below for other distros)
- A private network to bind to (Tailscale/Headscale, a VPN, or localhost+tunnel)

## Install

```sh
git clone https://github.com/heysamtexas/ttyd-ify
cd ttyd-ify
make install                 # installs deps + binaries + a systemd unit, runs as you
```

Run `make install` **without** `sudo` — it calls `sudo` itself, and prefixing another one
hides your username, so the installer would resolve the service user as root (it refuses
rather than doing that). To pick a different account: `make install WT_USER=alice`.

The installer prints `service user: <name>` — that's whose shell the terminal hands out.

Then edit `/etc/ttyd-ify/config` (bind target, port) and:

```sh
sudo systemctl restart wt.service
```

Open `http://<your-bind-ip>:7681` from a device on the same private network. Confirm it came
up with `journalctl -u wt.service -n 20 | grep wt-serve:`, which logs the address it bound.

> **Installing this with an agent?** (e.g. "Claude, install this repo on my box.")
> [`CLAUDE.md`](CLAUDE.md) is written for that reader: preflight checks, the one correct
> install command, what to verify, and a table of known failure modes. Point it there
> first — it's more precise than this README about the traps.

Non-Debian distros: install the deps first (`sudo dnf install ttyd dtach`,
`sudo pacman -S ttyd dtach`, `brew install ttyd dtach`), then `make install`.

## Configure

`/etc/ttyd-ify/config`:

| Key            | Default          | Meaning |
|----------------|------------------|---------|
| `WT_BIND`      | `tailscale`      | `tailscale` (auto tailnet IP), `localhost`, an interface (`eth0`, `wg0`), or a literal IP |
| `WT_PORT`      | `7681`           | Port ttyd listens on |
| `WT_AUTH`      | *(empty)*        | ttyd basic auth `user:pass`. **Leave empty** — see [Security](#security) |
| `WT_PROJECTS`  | `/etc/ttyd-ify/projects` | Optional "new session" shortcuts |
| `WT_WEB_PORT`  | `7683`           | Port `wtd` listens on (see [wtd](#wtd-the-go-server)) |
| `WT_WEB_BIN`   | `/usr/local/bin/wtd` | Path to the `wtd` binary |
| `WT_REPLAY_BYTES` | `262144`      | Recent output `wtd` replays on attach, per session. `0` disables replay |

`/etc/ttyd-ify/projects` — optional `name /path` per line; choosing **n** then a name in
the menu starts a session `cd`'d into that path.

## Use

- **Menu**: connect and you get `list / n)ew / c)ancel-to-shell / d)isconnect`. Detach from a
  session with **Ctrl-\\** to return to the menu; the session keeps running.
- **Direct attach**: `wt <name>` jumps straight into session `<name>`. Over the web this is
  ttyd's `--url-arg`: point a client at `ws://host:7681/ws?arg=<name>`. Handy for terminal
  apps that let you save a per-session URL (e.g. on iOS).

## wtd, the Go server

`wtd` is a Go replacement for `ttyd`, and it is what the rest of this project is moving to.
It speaks ttyd's WebSocket protocol exactly, so **existing clients work against it
unchanged** — no app rebuild, no new profile beyond the port — and it adds what ttyd cannot:

- **`GET /api/v1/sessions`** — a JSON list of sessions with live/idle state and working
  directory, so a client can *discover* sessions instead of being told a name. Plus
  create and delete.
- **A browser session picker at `/`**, and a terminal at `/?arg=<name>`.
- **`GET /openapi.json`** — the served spec, so the contract is machine-readable rather
  than folklore. Full documents live in [`api/`](api/).

`dtach` still owns session persistence, deliberately: a dtach session's parent is
independent of the server, so restarting `wtd` drops clients but leaves sessions running.

It is installed but **not enabled**, because enabling it opens a second port:

```sh
make build                                   # needs Go; run WITHOUT sudo
make install
sudo systemctl enable --now wt-web.service   # opt in when you're ready
```

No Go on the box? `make fetch` downloads the release binary for your architecture and
**verifies its checksum** before writing it, then `make install` as usual.

Run it beside ttyd (different ports) until you trust it, then retire ttyd by pointing
`WT_WEB_PORT` at `7681` and disabling `wt.service`.

> ⚠ `wt-web-serve` **refuses to start** if `WT_AUTH` or `WT_TTYD_ARGS` is set, because
> `wtd` implements neither. Starting anyway would silently discard a restriction you asked
> for — basic auth, or a ttyd `-R` that made the terminal read-only.

## Security

ttyd-ify does **not** authenticate at the app layer, on purpose:

- It's meant to sit behind a private network. Put access control **there**: tailnet ACLs, a
  VPN, an SSH tunnel to `localhost`, or a host firewall source-IP allowlist.
- ttyd's HTTP basic auth (`WT_AUTH`) is intentionally off by default because **it breaks
  Safari and all iOS browsers** — WebKit does not send basic-auth credentials on the
  WebSocket upgrade, so the terminal never connects. If your clients are all Chromium/Firefox
  on desktop you *can* set `WT_AUTH=user:pass`, but network-layer control is recommended.
- Default bind is a specific trusted interface; it will refuse to help you bind `0.0.0.0`.

If your login shell auto-starts tmux/screen, see [`docs/bashrc-snippet.sh`](docs/bashrc-snippet.sh)
to keep it from recursing inside web sessions (`wt` sets `WT=1`).

## Uninstall

```sh
make uninstall            # remove service + binaries, keep /etc/ttyd-ify
make uninstall PURGE=1    # also remove config
```

Running `dtach` sessions and the `ttyd`/`dtach` packages are left alone.

## License

MIT — see [LICENSE](LICENSE).
