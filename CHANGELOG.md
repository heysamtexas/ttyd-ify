# Changelog

What each release changed, newest first. This file is the source: `make notes TAG=vX.Y.Z`
prints one section, the release workflow publishes it as the GitHub release body, and the
annotated tag carries the same text. There is one hand-written copy, and it is this one.

## v0.10.0 — astigmatism mode

*2026-09-24.* One commit since v0.9.0 (#140): a third switch in the terminal page's `≡` panel,
entirely in the browser. The `/ws` protocol and the API are unchanged. The iOS app needs no
update, and it doesn't get the mode, because it renders natively and never loads these pages.

**Astigmatism mode** is for eyes that see halos around bright text. The terminal used to be
xterm's default, pure white on pure black (21:1), which is the worst case for astigmatism:
bright strokes on a dark field bleed and ghost. This mode lowers the contrast instead of
raising it: off-white on dark grey, about 11:1, which still clears WCAG AAA (7:1).

- Text is set in **Intel One Mono**, a face designed with low-vision developers, in Medium
  rather than Regular. The face is vendored (OFL-1.1), so the page still works on a tailnet
  with no internet route.
- **Each tap cycles off → 14px → 18px → 24px.** The 14px step keeps today's size and changes
  only the look. A size change reflows the grid the way a window resize does.
- The 16 colours are softened, and each is at least 4.5:1 on the ground. Bold is heavier, not
  brighter. Text a program paints too close to its background, truecolor included, is lifted
  to 4.5:1. The cursor stops blinking.
- The picker, `/help` and the panel follow in the same face and colours. The setting belongs to
  the browser, like submarine mode, and the two modes can be on together.

**Upgrading from v0.9.0 is fetch, install, restart.** Nothing in `/etc/ttyd-ify/config` or
`wt.service` changed.

## v0.9.0 — submarine mode, and full screen

*2026-09-23.* One commit since v0.8.0 (#139): two switches in the terminal page's `≡` panel,
both entirely in the browser. The `/ws` protocol and the API are unchanged. The iOS app needs
no update, and it gets neither mode, because it renders natively and never loads these pages.

**Submarine mode** makes the page red on black, for working in a dark room without losing
night vision. It filters the whole page rather than recolouring it: every pixel becomes its
channel average in red, and green and blue are zero. That covers what programs paint in the
terminal too, truecolor included, which no palette reaches. The cost is that colours are told
apart only by brightness. The setting belongs to the browser, so every page and open tab on it
follows, and each other device keeps its own. The picker and `/help` go dark while it is on,
whatever the OS theme. It is served as `GET /sub.js` and loaded before first paint, so the
screen never flashes daylight colours on load. In this mode the corner buttons, the panel's
edges and the reconnect button are brighter, so you can find them in the dark.

**Full screen** hides the browser's tab bar and address bar. Escape is both the browser's way
out and a key vim and Claude Code need. In Chrome and Edge a tap of Escape reaches the
terminal and holding it exits full screen. In Firefox and Safari a tap exits, which `/help`
says. The switch is hidden on iPhone Safari, which has no full screen for pages.

**Upgrading from v0.8.0 is fetch, install, restart.** Nothing in `/etc/ttyd-ify/config` or
`wt.service` changed, so there is no config step and no new refusal to hit.

## v0.8.0 — the box tells you what it costs, and what you said to it

*2026-09-04.* Twelve commits since v0.7.0. A session is cheap to start and this repo made it
easy, so a box accumulates thirteen of them over nineteen days and nothing on any page says
what that costs. This release makes the machine's own state visible from inside it and from
the page you create sessions on, and it fixes the reason a Mac could not copy out of one.

**Two new endpoints, both behind feature flags, both additive.** The `/ws` protocol is
unchanged, and a client that ignores both flags is unaffected — the iOS app needs no update.
The one thing a shipped client can observe that is not an addition: JSON responses and
`/token` now carry the `Cache-Control: no-store` the spec has always declared.

- **`GET /api/v1/host`, and a `≡` flyout on the terminal page** (#126, #116) — load against
  the CPU count, memory, swap, memory/CPU/IO stall from `/proc/pressure`, disk, uptime, and
  each session's memory summed over its process tree. Feature flag `host-health`. The panel
  overlays rather than shrinking the terminal, so opening it never resizes the pty. Pressure
  is nullable and `null` means the kernel has no PSI — never `0.00`, which is the reading on
  a healthy box. This route lists a stale socket as a `pid: 0` row, because it does not reap.
  Also fixed here: `writeJSON` now actually sends the `Cache-Control: no-store` the spec has
  claimed since before any of this was polled by a browser.
- **`GET /api/v1/sessions/{name}/prompts`, and a `said` list in that panel** (#127) — the
  messages sent to the agent in a session, oldest first. Feature flag `session-prompts`.
  `wtd` only reads: a Claude Code `UserPromptSubmit` hook inside the session writes a small
  bounded file. `install.sh` puts `wt-prompt-hook` in `/usr/local/bin` and wires up nothing,
  so **until you add it to `~/.claude/settings.json` this endpoint answers 200 with no
  prompts, forever** — see the README. That is why an empty list must read "nothing
  recorded" and never "you have said nothing": a client cannot tell the two apart.

**Session cost and headroom in the picker** (#77 item 1). Each row carries its session's
memory; under the host line, the session count, their total, and what is left — coloured only
when it is tight or critical, and a box with no swap says so at those levels. Rows keep the
server's name order rather than sorting by size, which would reorder the list under someone
reaching for a link. A session's own size is never coloured: it is not misbehaving for being
large. Polls `/api/v1/host` on its own 15s timer and returns early when the tab is hidden,
because that document costs a walk of every session's process tree. Items 2 and 3 of #77 —
a session ceiling and idle age — are untouched, so **#77 stays open**.

**`WT_SESSION` is exported into every session's shell** (#111), so a program inside a session
can discover which session it is in. The pty carries no name and an agent hook is spawned with
no controlling terminal at all, which is what made the prompt list need this first. Set in
`sessionEnv`, which both the API and the deep link call, so the two creation paths agree by
construction rather than by two env lines asking each other to stay in step.

**Option+drag selects on macOS** (#137). Reported as "Cmd+C does not copy", and it was not a
clipboard bug: under a program holding mouse tracking — Claude Code, vim, htop, less — there
was no gesture on a Mac that produced a selection at all, so the copy handler had nothing to
write. xterm splits the force-selection modifier by platform and
`macOptionClickForcesSelection` defaults to false, so Shift+drag had always worked on Linux
and Windows and macOS had nothing. Option, because iTerm2 binds the same key. The cost: a
program wanting Option+click stops seeing it while mouse tracking is on. `help.html` names
both modifiers now, since a modifier nobody knows about is the same as no modifier.

Spec, docs and tests:

- `session-status` is in the features registry table and the `/api/v1/meta` example (#114).
  It was advertised by the server and defined nowhere, which is the one thing the registry
  exists to prevent.
- The restart question is ruled on **ancestry**, not on how you connected (#76): a `dtach`
  master above you means your command chain survives, `wtd` directly above you means it does
  not. Both `CLAUDE.md` and the install skill ship a check that answers it rather than a
  claim to reason from.
- `fetch.sh` no longer reports "I could not check the provenance" as "this looks tampered
  with", and points at `install.sh` rather than the command that undoes it (#89, #110).
- `session-lifecycle.md` §5's closing sentence, spliced from two drafts since 71b1066, is
  finished (#128). It is served to clients at `/docs/session-lifecycle.md`, where CI cannot
  see it.
- The builtin tests no longer flake on their own cleanup (#131): `HISTFILE` now points at
  `/dev/null` in the fixture, so bash stops writing history into the temp directory the test
  is concurrently removing. Four failures in 60 runs before, zero in 60 after.

Release notes themselves changed shape (#138): every release until now published the same
install boilerplate, and the tag message that said what changed was discarded.

**Upgrading from v0.7.0 is fetch, install, restart.** Nothing in `/etc/ttyd-ify/config` or
`wt.service` changed since v0.7.0, so there is no config action and no new refusal to trip
over. The one optional step is wiring `wt-prompt-hook` into the `~/.claude/settings.json` of the
account the service runs as — `WT_USER`, if you set one — should you want the prompt list.
Leaving it unwired costs nothing but an empty list. It also needs `python3`; without it the
hook is a silent no-op, by design.

## v0.7.0 — agent status in the session list

*2026-08-20.* Each session reports the agent status it last observed in band, so a client
listing sessions can tell which one is blocked waiting on a human instead of opening each in
turn.

- `GET /api/v1/sessions` carries `agentStatus`, behind a `session-status` feature flag.
- The session picker renders it, using the favicon's own colours and shapes.
- `api/ws-protocol.md` §6a updated: `wtd` now observes these bytes, having previously said it
  had no part in them. It still never alters them.

`null` means unknown — this server never observed that session — and is not the same as idle.
A status does not expire (#101). Also in this tag: the release notes and README name the one
command that installs the attested binary rather than rebuilding over it (#103).

## v0.6.1 — the session picker gets the terminal's mark

*2026-08-19.* v0.6.0 gave terminal tabs a status favicon and left the picker with the
browser's blank document glyph, so the page listing the terminals read as an unrelated tab
sitting beside them. The picker is not a session and has no status to report, so it takes the
identity half only: the same grey prompt mark a quiet terminal tab wears, drawn on a canvas
like the terminal's. Cosmetic only — no wire change, no server change, no client impact.

## v0.6.0 — agent status in the browser tab

*2026-08-19.* The browser terminal renders a per-session agent status in the tab's favicon:
grey prompt idle, cyan circle working, amber diamond finished-its-turn, red triangle blocked
waiting on you. A strip of identical-looking tabs now says which agent needs you.

The signal is in band. Anything running in a session writes
`ESC ] 1337 ; WTState=<state> BEL` to its own tty and it rides the OUTPUT frames `wtd`
already carries, so the server gained no endpoint, no state and no code — and an
unauthenticated port gained no new surface. The emitter is a one-line `printf`, so any agent
can drive it. See `api/ws-protocol.md` §6a.

The terminal page also stops discarding the session's own window title, which agents set to
the task they are working on, and honours the terminal bell as a weak attention signal for
agents that report nothing. No wire change: existing clients are unaffected.

## v0.5.0 — replay survives a service restart

*2026-08-10.* Each session's replay tail is saved to the unit's runtime directory and restored
on start, so restarting `wt.service` no longer costs a session its recent output (#92).
`wt.service` gained `RuntimeDirectory=wt`, `RuntimeDirectoryMode=0700` and
`RuntimeDirectoryPreserve=restart`; losing any of the three errors nowhere and silently turns
persistence off, so `make lint` guards all three.

- URLs in the terminal open on Cmd/Ctrl+click and show where they lead (#91).
- The shutdown guard actually guards, and `closeAll` waits for evictions (#93).

## v0.4.0 — the install is verified, not asserted

*2026-08-07.* Thirty-two commits since v0.3.0. The theme: this repo kept claiming things about
installing and running that nothing had ever checked, and each one turned out to be a real
defect once something did.

- CI installs ttyd-ify into a container running real systemd and proves the box serves a
  terminal — a session created through the API, a command typed over `/ws` and its output read
  back, and the session still alive with the same shell pid after a restart. That last one is
  #21's invariant, which had only ever been a grep for `KillMode=process` in a file (#79).
- The installer no longer reports success over a service that cannot run. It resolves
  `WT_BIND` before writing anything and refuses when the machine does not have it, then
  samples `MainPID` again after starting: `Type=simple` reports success the moment the exec
  succeeds, so a fresh box without Tailscale used to get a banner and a unit restart-looping
  every three seconds (#80).
- Release binaries carry a signed build provenance attestation and `make fetch` checks it. The
  checksum only ever proved the download, since it travels from the same URL as the binary.
  `make fetch TAG=` also exists now, so rolling back no longer requires a compiler on the box
  that by definition has none (#85, #86).

First release whose artifacts can be verified with
`gh attestation verify wtd-linux-amd64 --repo heysamtexas/ttyd-ify`. Earlier releases have no
attestation to find, and `make fetch` refuses them by default rather than guessing whether a
missing one means old or tampered.

## v0.3.0 — the spec becomes something a client can be built from

*2026-07-25.* Seventeen commits since v0.2.0, almost all of them making the published HTTP
surface true and sufficient. Several things the served spec asserted were not what the code
did, and several things a client needed were not in it at all.

- `Session.pid` is the session's *shell*, not the dtach master, and `cwd` is read from that
  same process. They were one level apart, so one JSON object described two processes.
- `pid` and `cwd` emit `null` rather than being omitted, so the shape no longer varies.
- `Session` gains `attachedCount`; `Meta` gains `maxSessionNameLength` and `apiPath`.
- New: `GET /api/v1/sessions/{name}`, so the `Location` header a create returns is fetchable.
- New: `GET /docs/{file}`, serving the documents the spec references.
- `DELETE` escalates to `SIGKILL`.
- The published session-name pattern no longer uses ECMA-262 lookaheads, which RE2 rejects —
  a generated Go or Rust client failed on it outright.
- A deep link (`?arg=`) refuses a name whose socket path would exceed 107 bytes, falling
  through to the picker instead of creating a session nothing can attach to.

No client-visible surface: a socket is never unlinked unless it is provably dead, `WT_DIR` is
normalized on both ends, and `/proc` decides when a socket cannot be dialled at all.

## v0.2.0 — replay recent output on attach

*2026-07-25.* `wtd` holds one dtach attachment per deep-linked session and keeps a bounded
ring of recent output, so attaching shows recent output instead of a blank screen. dtach keeps
no screen buffer, which is why nothing downstream of it could fix this and why `wtd` exists.

- Named (`?arg=`) connections join a shared session hub; argless ones keep their own private
  pty, since the picker cannot be shared.
- `attached` in `/api/v1/sessions` no longer comes from the socket's execute bit, which a held
  attachment pins on permanently.
- `WT_REPLAY_BYTES` tunes the buffer; `0` disables replay.

Wire-compatible with ttyd: replay arrives as ordinary OUTPUT frames with no new opcode, so
shipped clients render it without knowing it exists.

## v0.1.0 — first release with wtd

*2026-07-24.* ttyd-ify gains `wtd`, a Go server that replaces ttyd while keeping dtach for
session persistence. Wire-compatible with ttyd, verified against a live ttyd in CI and against
the real native iOS client, so existing clients connect unchanged.

- `GET /api/v1/sessions` — discover sessions with live/idle state and working directory, plus
  create and delete. Clients no longer have to be told a name.
- A browser session picker at `/`, and a terminal at `/?arg=<name>`.
- `GET /openapi.json` — the contract is machine-readable, not folklore.

`wtd` is installed but NOT enabled in this release: it opens a second unauthenticated shell
port, so enabling it is an explicit choice. Also fixes three bugs in the ttyd path:
`WT_PROJECTS` in the config file was inert, `sudo make install` silently rendered a root-owned
web shell, and `make install FORCE=1` silently skipped the binaries.
