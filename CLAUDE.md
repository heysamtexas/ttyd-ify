# ttyd-ify

A browser terminal for a Linux box: `wtd` (a Go server in this repo) serves the web page and
attaches to sessions itself, and sessions live as `dtach` sockets so they survive the client
disconnecting — and survive the server restarting. Managed by systemd. Two Go deps; the shell half
is now just the launcher and the installer, with no build step.

**Naming quirk:** the *project* is `ttyd-ify` and the Go binary is `wtd`, but every other runtime
artifact is `wt` — `wt`, `wt-serve`, `wt.service`, all `WT_*` keys. Keep the split; a rename would
invalidate this box's `/etc/ttyd-ify/config` and a lot of muscle memory. `ttyd` itself is
**retired** (#23)
and survives only as a test dependency — see the conformance job in `.github/workflows/ci.yml`.

**Audience — read this before designing for anyone.** Today: **two boxes** — this one (Sam's) and
**one beta user's**, which that user updates by handing instructions to an agent. More are
expected, so "beta users" is no longer aspirational. It is still a number you can count on one
hand, and only one of those boxes has a config you can read.

The consequence is still a rule, because getting it wrong is expensive in exactly one direction:
**do not build migration paths, compatibility shims, or upgrade docs for a fleet.** Two installs
is not a fleet, and speculative compatibility machinery is still the expensive mistake. What
changed is that "no other install exists" is no longer a safe assumption: a breaking change can
now strand a real operator you cannot reach, and `/etc/ttyd-ify/config` answers for this box only.
When a change would strand someone, the honest fix is usually a line in the README plus telling
Sam who needs to hear about it — not compatibility code.

Still true regardless of user count, and still worth writing for: **installs happen by an agent on
a machine it has never seen, which cannot ask this repo's author anything.** So state the one
correct command and name the traps explicitly. A hedge or a "you may want to" is a defect: it
becomes a question the human has to answer, or a guess. Favour a short correct path over
contributor ceremony, changelogs, or deprecation cycles.

**Platform: Linux with systemd, full stop.** The README's `dnf`/`pacman`/`brew` lines are courtesy
notes for getting `dtach`, not a portability promise. Don't add abstraction for macOS or
non-systemd init speculatively.

## Security framing — the question every change gets

*Does this widen who can reach the shell?*

The threat model is explicit and accepted: **a writable, unauthenticated shell as the service user,
protected only by the interface it binds to.** The README, `etc/config.example`, and `install.sh`'s
closing banner each repeat that warning — preserve it when editing them.

**The small user count above describes the deployment, not the blast radius.** This box binds its tailnet
address, and that tailnet is shared: `tailscale status` lists ~7 other people's machines, every one
of which can reach `:7681` and get this shell. So do not read the audience note as evidence that the
exposure is theoretical — it is the reason #27 was parked, and the premise was wrong. When a
decision turns on who can reach the port, run `tailscale status` rather than inferring it from the
user count.

- **Never bind `0.0.0.0`.** `resolve_ip` only yields a tailnet IP, `127.0.0.1`, an interface's
  address, or a literal. A wildcard would turn an unauthenticated shell into a public one.
- **Session names are untrusted input** — they arrive from a client over the network as `$1`. Keep
  the `/` and `..` rejection in `validateAttachName` (`cmd/wtd/attach.go`), and keep `shellQuote`
  when interpolating a path into `bash -c`.
- **`WT_AUTH` and `WT_TTYD_ARGS` make the server *and* the install refuse to start.** `wtd`
  implements neither, and both can carry an access restriction (basic auth, ttyd `-R`), so
  ignoring them would silently remove a control the operator configured. App-layer auth is a real
  future option, not a dead end (#27) — but **not as basic auth**: browsers cannot attach headers to
  a WebSocket upgrade at all, so the page would authenticate and the terminal would never connect.
  A login page setting a cookie is the shape that works, because cookies *are* sent on the upgrade.
  #27 has the design; read `.claude/rules/ios-client.md` before concluding auth is impossible on iOS.
- Flag any diff touching bind resolution, auth, or session-name handling before committing.

## One server — the shape of the thing

```sh
systemctl is-active wt.service            # wt.service → bin/wt-serve → wtd, on WT_PORT (7681)
```

One unit, one launcher, one listener. It serves `/`, `/token`, `/ws`, `/api/v1/*`,
`/openapi.json`, a picker and a terminal. There is no second port and no second unit: ttyd ran on
`WT_PORT` with `wtd` beside it on `WT_WEB_PORT` until #23 retired ttyd and moved `wtd` onto
`WT_PORT`. `WT_WEB_PORT` is warned about and ignored if an old config still sets it.

`wtd` replaced ttyd **and** the bash picker, but **not** dtach: session persistence stays with
dtach, because a session's parent being independent of the server is what lets the service restart
without killing anything. (shpool was evaluated and rejected as an alternative — all its sessions
die with its daemon; see `docs/shpool-evaluation.md` and #54 before reconsidering.) `wtd` runs `dtach -A` itself, so session logic has exactly one
implementation — `dtachArgs` in `cmd/wtd/attach.go`, shared by the API and the deep link, with
`TestDtachArgsCreateAndAttachAgree` asserting they cannot drift (#49). `wtd` is wire-compatible with ttyd
and that is *verified, not assumed* — the real iOS app connects to it unchanged, and the
`conformance` job in `.github/workflows/ci.yml` runs real ttyd 1.7.4 beside `wtd` and diffs them on
every change. **Do not delete that job when tidying ttyd away.** It is the only assertion of four
wire facts, and nothing else knows what ttyd did.

**Replay on attach has shipped and it is the reason to own the server at all.** For a deep-linked
(`?arg=`) connection `wtd` holds one dtach attachment per session and keeps a bounded ring of recent
output, so attaching replays context instead of showing a blank screen. Read `cmd/wtd/hub.go`'s
header before touching any of it.

## Commands

```sh
make lint                     # shellcheck + gofmt + go vet + go test -race + spec drift check
make smoke                    # install into a throwaway systemd container, prove it serves (docker)
make build                    # build wtd — WITHOUT sudo
make spec                     # regenerate cmd/wtd/openapi.json from api/openapi.yaml
make fetch                    # release binary for this box, checksum + provenance verified
make fetch TAG=v0.2.0         # a specific release — rollback without a Go toolchain
make install                  # deps + binaries + the unit; the recipe calls sudo itself
make install WT_USER=alice    # run the service as someone other than you
make uninstall                # keeps /etc/ttyd-ify;  PURGE=1 removes it too
journalctl -u wt.service -f
journalctl -u wt.service --no-pager | grep 'wt-serve: wtd on' | tail -1   # where it bound
```

`make lint` is the verification for anything in-process — it runs the unit, wire-conformance,
hub/replay and real-dtach integration tests, and there is no separate test target for those.

**`make smoke` is the verification for the install itself** (#79), and it is separate because it
needs docker and a privileged container, which a lint target must not. It boots `ubuntu:24.04` with
real systemd, installs as a fresh box does, and asserts the *result*: the unit starts and is enabled,
the listener is on the resolved address and never a wildcard, `/ws` carries real terminal I/O
(`test/wsprobe.py` types a command and reads the shell's output back), a session survives
`systemctl restart` — #21's actual invariant rather than a grep for `KillMode=process` — and
uninstall leaves running sessions alone. `test/install-uninstall.sh` (the `install` CI job) keeps the
faster half: file operations, modes, and refusals firing before anything is written, with `systemctl`
stubbed. Neither subsumes the other. Both refuse to run outside a container, because every path they
touch is absolute.

- **No `sudo` prefix on `make` targets.** The recipes add it themselves; an outer one nests, resets
  `SUDO_USER` to root, and `install.sh` refuses rather than installing a root-owned web shell.
- **Build unprivileged, install privileged.** `install.sh` never runs `go`, because it runs as root
  and would write root-owned files into the checkout and the Go build cache.
- **`make install` needs a `wtd` binary and refuses without one.** There is no ttyd to fall back
  on, so a unit whose `ExecStart` cannot start a server would just restart-loop. It refuses before
  writing anything, which is also how a box with `WT_AUTH` set keeps its working server.
- **Installing does not restart the service**, so new code can be on disk and not running. Every
  binary is overwritten to match the checkout (#26, #30), so `diff` against `/usr/local/bin` proves
  nothing about the *running* process — check `systemctl show -p ActiveEnterTimestamp wt.service`
  against when you installed, or just restart deliberately. `/api/v1/meta`'s `version` is the
  honest answer for what the Go half is actually running.
- **Ask before installing or restarting.** A restart drops every connected client's *connection* —
  a phone mid-task, and the browser tab you are watching this in. Ask because of **other people's**
  clients: `tailscale status` lists ~7 machines that can reach `:7681`. Sessions themselves survive.
- **Whether a restart kills your own shell depends on ancestry, not on how you connected.** A
  `dtach` master between you and PID 1 means you survive (`KillMode=process` signals only `wtd`);
  `wtd` directly above you means you do not. Do not guess from the connection shape — a named
  connection with an unusable name gets a shell `wtd` parents itself. Run this, don't reason:
  ```sh
  v="unaffected: not under wt.service"; p=$$
  while [ -n "$p" ] && [ "$p" != 1 ]; do
    case "$(cat /proc/$p/comm 2>/dev/null)" in
      dtach) v="SURVIVES: dtach master $p is above you"; break;;
      wtd)   v="DIES: wtd $p is above you, no dtach between"; break;;
    esac
    p=$(awk '/^PPid:/{print $2}' /proc/$p/status 2>/dev/null)
  done; echo "$v"
  ```
- **Still put a restart in its own step**, and not merely because your view drops. Output your
  surviving chain writes during the gap is in no ring, no saved file and no client scrollback:
  rings are saved before the hubs close, and nothing re-attaches until the next client connects.
- **Never point tests at `~/.dtach`.** It holds real sessions on a developer box, possibly the one
  you are running in. Use `t.TempDir()`.

## Where the detail lives

This file is the always-loaded part. Depth loads on demand:

| Need | Load |
|---|---|
| Install, verify, or change a live deployment | the **`install-ttyd-ify` skill** — full procedure, verification, failure-mode table |
| The wire, anything a client sees | `.claude/rules/ios-client.md` (auto-loads with `cmd/wtd/ws.go`, `cmd/wtd/attach.go`, `api/**`) |
| The Go server, its tests, the spec pipeline | `.claude/rules/go-server.md` (auto-loads with `cmd/wtd/**`) |
| Bash conventions, config precedence, launchers | `.claude/rules/shell.md` (auto-loads with `bin/**`, `*.sh`, `etc/**`) |
| **The specification itself** — WS protocol, OpenAPI, ttyd compatibility, session lifecycle | `api/` — source of truth, read it before changing behaviour it describes |

`api/openapi.yaml` is the source of truth for the HTTP surface; `cmd/wtd/openapi.json` is generated
by `make spec` and `make lint` fails on drift. **Its descriptions say what a client can observe and
must do — mechanism, provenance and `[LAB]` evidence belong in `api/*.md`, or in a `#` comment, which
`make spec` drops.** The document is served to clients, so a citation into files they do not have is
worse than none; `make spec-guards` enforces that and checks every `§N` pointer still resolves. The
*markdown* specs in `api/` remain unchecked prose — drift there is invisible to CI, so check them by
reading when you change behaviour.

The primary client is a native iOS app in a separate repo (`~/src/ios-claude-terminal`) with no
shared CI and no type checking across the boundary, so a server change can land silently and break
a shipped phone. Read the ios-client rule before touching the wire.
