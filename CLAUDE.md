# ttyd-ify

A browser terminal for a Linux box: `ttyd` serves the web page, `wt` (a bash session picker) is the
start command it runs on each connection, and sessions live as `dtach` sockets so they survive the
client disconnecting. Managed by systemd. Being replaced by `wtd`, a Go server in this repo. No
build step for the shell half; two Go deps.

**Naming quirk:** the *project* is `ttyd-ify`, but every runtime artifact is `wt` — binaries,
`wt.service`, all `WT_*` keys. Keep the split; a rename would invalidate beta users'
`/etc/ttyd-ify/config` and muscle memory.

**Audience.** Sam's own machines plus a small beta group, and — for installs — another agent on a
machine it has never seen that cannot ask this repo's author anything. So state the one correct
command and name the traps explicitly. A hedge or a "you may want to" is a defect: it becomes a
question the human has to answer, or a guess. Favour a short correct path over contributor
ceremony, changelogs, or deprecation cycles.

**Platform: Linux with systemd, full stop.** The README's `dnf`/`pacman`/`brew` lines are courtesy
notes for getting `ttyd` + `dtach`, not a portability promise. Don't add abstraction for macOS or
non-systemd init speculatively.

## Security framing — the question every change gets

*Does this widen who can reach the shell?*

The threat model is explicit and accepted: **a writable, unauthenticated shell as the service user,
protected only by the interface it binds to.** The README, `etc/config.example`, and `install.sh`'s
closing banner each repeat that warning — preserve it when editing them.

- **Never bind `0.0.0.0`.** `resolve_ip` only yields a tailnet IP, `127.0.0.1`, an interface's
  address, or a literal. A wildcard would turn an unauthenticated shell into a public one.
- **Session names are untrusted input** — they arrive from a client over the network as `$1`. Keep
  the `*/*` / `*..*` rejection in `bin/wt`, and keep `${var@Q}` when interpolating a path into
  `bash -c`.
- **`WT_AUTH` stays empty by default**, because basic auth breaks Safari and every iOS *browser*.
  It does **not** break the native app — see `.claude/rules/ios-client.md` before concluding auth is
  impossible.
- Flag any diff touching bind resolution, auth, or session-name handling before committing.

## Two servers, mid-migration — know which one you are looking at

```sh
systemctl is-active wt.service wt-web.service   # ttyd on WT_PORT, wtd on WT_WEB_PORT
```

| | ttyd path (legacy) | wtd path (the future) |
|---|---|---|
| unit | `wt.service` → `bin/wt-serve` → `ttyd` | `wt-web.service` → `bin/wt-web-serve` → `wtd` |
| port | `WT_PORT` (7681) | `WT_WEB_PORT` (7683) |
| enabled by install | yes | **no** — opt in with `systemctl enable --now wt-web.service` |
| serves | `/`, `/token`, `/ws` | those **plus** `/api/v1/*`, `/openapi.json`, a picker, a terminal |

`wtd` replaces ttyd, **not** dtach: session persistence stays with dtach, and `bin/wt` is still the
start command for both, so session logic has exactly one implementation. `wtd` is wire-compatible
with ttyd and that is *verified* — the real iOS app connects to it unchanged, and
`cmd/wtd/conformance_test.go` diffs both servers in CI.

**Replay on attach has shipped and it is the reason to own the server at all.** For a deep-linked
(`?arg=`) connection `wtd` holds one dtach attachment per session and keeps a bounded ring of recent
output, so attaching replays context instead of showing a blank screen. Read `cmd/wtd/hub.go`'s
header before touching any of it.

## Commands

```sh
make lint                     # shellcheck + gofmt + go vet + go test -race + spec drift check
make build                    # build wtd — WITHOUT sudo
make spec                     # regenerate cmd/wtd/openapi.json from api/openapi.yaml
make install                  # deps + binaries + both units; the recipe calls sudo itself
make install FORCE=1          # also overwrite already-installed binaries
make install WT_USER=alice    # run the service as someone other than you
make uninstall                # keeps /etc/ttyd-ify;  PURGE=1 removes it too
journalctl -u wt.service -f       # ttyd path
journalctl -u wt-web.service -f   # wtd path
```

`make lint` is the verification — it runs the unit, wire-conformance, hub/replay and real-dtach
integration tests. There is no separate test target.

- **No `sudo` prefix on `make` targets.** The recipes add it themselves; an outer one nests, resets
  `SUDO_USER` to root, and `install.sh` refuses rather than installing a root-owned web shell.
- **Build unprivileged, install privileged.** `install.sh` never runs `go`, because it runs as root
  and would write root-owned files into the checkout and the Go build cache.
- **Installing new binaries does not restart the service**, so a code change can be on disk and not
  running. Confirm with `diff bin/wt-serve /usr/local/bin/wt-serve`, not the install log.
- **Ask before installing or restarting.** A restart drops every connected client — a phone
  mid-task, and your own terminal if this session arrived through ttyd. The `dtach` sessions
  survive, but your command chain dies at that line.
- **Never point tests at `~/.dtach`.** It holds real sessions on a developer box, possibly the one
  you are running in. Use `t.TempDir()`.

## Where the detail lives

This file is the always-loaded part. Depth loads on demand:

| Need | Load |
|---|---|
| Install, verify, or change a live deployment | the **`install-ttyd-ify` skill** — full procedure, verification, failure-mode table |
| The wire, the picker menu, anything a client sees | `.claude/rules/ios-client.md` (auto-loads with `bin/wt`, `cmd/wtd/ws.go`, `api/**`) |
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
