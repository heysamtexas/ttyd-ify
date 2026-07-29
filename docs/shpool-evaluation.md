# shpool: evaluated and rejected

**Decision (2026-07-29, #54): wtd stays on dtach. shpool is not adopted — not as a
replacement, not as a second backend.** This document exists so the question is never
re-investigated from scratch: read [Re-open criteria](#re-open-criteria); if upstream has not
met both, stop here.

Evaluated: [shell-pool/shpool](https://github.com/shell-pool/shpool) v0.11.0 (2026-06-12), a
Rust session pooler maintained by a Google engineer under a non-Google org (Apache-2.0,
Google CLA, not an official Google product). Findings below were verified against upstream
source, issues, and packaging archives — not the README's marketing.

## The two disqualifiers

### 1. All sessions die with the daemon

shpool runs **one daemon that owns every session's pty** as a forkpty child, tracked in an
in-memory table (`HashMap` in `daemon/server.rs`). There is no on-disk session state. A
daemon crash, restart, or upgrade therefore kills every session on the box — and the
maintainer confirms this is working as intended
([shell-pool/shpool#377](https://github.com/shell-pool/shpool/issues/377): "you can also just
restart your daemon if you are ok with dropping your existing sessions"). Its own systemd
unit is `KillMode=mixed`, `TimeoutStopSec=2s`.

It gets worse: the socket unit outlives the daemon and any shpool client autodaemonizes, so
an attach after a daemon crash **silently succeeds into a fresh empty session under the same
name**. The iOS app's unattended auto-reconnect would land in a blank session that looks like
yours with all context gone — no error, no close code. That is the silent-data-loss class
that #5, #21, and #24 were fought to eliminate, reintroduced by architecture.

dtach is the inverse: each session is one independent process owning its own socket and pty,
parented to nothing wtd controls. That is the property `cmd/wtd/hub.go`'s header calls the
reason dtach is still here, and the reason `systemctl restart wt.service` drops clients but
kills no sessions. shpool solves "my ssh connection dropped"; it does not solve — it
*un*-solves — "my server restarted".

### 2. Packaging

No apt or Debian package (an unpublished `debian/` dir exists upstream), no prebuilt release
binaries ([shell-pool/shpool#327](https://github.com/shell-pool/shpool/issues/327), open).
The only Linux install path is `cargo install shpool`, requiring Rust ≥1.85 — Ubuntu 24.04's
apt ships 1.75. So installing it means rustup in the service user's home, which `install.sh`
cannot do: it runs as root and root never builds (see `.claude/rules/go-server.md`). The
install script's only honest move would be to refuse with instructions — the exact
human-blocking hedge CLAUDE.md defines as a defect. dtach is `apt install dtach`.

## Secondary blockers

Each of these alone is survivable; together they price in the rest of the decision:

- **One client per session.** A second `shpool attach` prints "already has a terminal
  attached" and exits 0 — a silent no-op. External SSH attach beside wtd's held hub (a
  supported, tested scenario on dtach, and the iOS app's stated recovery path) becomes
  impossible, and the wire's `attachedCount` loses two of its three signal sources.
- **Upgrade wedges every attach.** The client↔daemon protocol is minor-version-locked; on
  mismatch a non-background `shpool attach` prints a warning and **blocks on stdin waiting
  for a newline**. Spawned under wtd's pty, every hub spawn hangs forever after any shpool
  upgrade until wtd grows a spawn watchdog it has never needed.
- **Session names reject whitespace** (and `.`, `..`, `/`). The loose-listing contract —
  sessions named with spaces/non-ASCII stay listable and attachable — is published in
  `api/openapi.yaml` and must never tighten.
- **Detach key.** shpool intercepts Ctrl-Space Ctrl-q in the daemon; Ctrl-`\` is wtd's
  documented, phone-reachable detach key. Fixing it means depending on shpool's TOML
  keybinding config or a two-repo client change against shipped phones.
- **Restore-mode conflict.** shpool's scrollback restore repaints with screen-reset codes on
  attach, which double-paints against wtd's replay ring; it would have to be pinned to
  `session_restore_mode = "simple"` (restore nothing) in a config file in the service user's
  homedir that wtd does not own.
- **Upstream posture.** Bus factor 1 (next contributor has 6 commits to the maintainer's
  321), a breaking change in nearly every 0.x minor, two consecutive 2026 releases yanked
  over an uninvestigated attach hang, and the CLI as the only supported interface (the
  protocol crate says "you almost certainly don't need to use it directly" and broke
  0.3→0.4).

## Dual support was sketched and priced

A `backend` interface is feasible — `hubs.build` is already the attach seam — but honest
scope is **~30 files and ~2,500–3,500 LOC**, because the liveness model (socket-dir
enumeration, `/proc` walks, three-state staleness probes, kill-escalation with
socket-disappearance as the death certificate) is dtach-shaped all the way down.
`api/session-lifecycle.md` §2/§4/§5/§7 are derivations of dtach's mechanism presented as API
semantics and would need splitting into a neutral contract plus a dtach appendix. CI gains a
rustup + `cargo install` + daemon-fixture job. The cross-backend test suite immediately needs
backend-conditional expectations (external attach *counts* on dtach, *silently no-ops* on
shpool) — the tell that the backends do not share a contract. And it is the speculative
fleet-abstraction CLAUDE.md names as this repo's expensive mistake, built for a backend
nobody runs.

## What shpool offers that wtd already has

| shpool | wtd |
|---|---|
| scrollback restore (`session_restore_mode`) | the replay ring + `hub.kick` — the reason to own the server at all |
| `shpool list --json` | `GET /api/v1/sessions` |
| exit-status propagation | pty EIO → close code 1000 with a reason |

The one idea genuinely worth stealing someday: its `EVENTS.md` JSONL lifecycle stream
(`session.created|attached|detached|removed`) as a model for a push surface behind a
`session-events` feature flag. Not filed as work: the iOS app only pull-to-refreshes today
(`SessionListView.swift:200` in `ios-claude-terminal`), so nothing needs it yet.

## Re-open criteria

Reconsider shpool only if **both** become true upstream:

1. **Sessions survive a daemon restart** — persistent/re-adoptable sessions, which is an
   architecture rewrite, not a release note. Watch for on-disk session state or a
   handoff/re-exec mechanism, not a changelog line.
2. **It ships as an apt package or prebuilt release binaries** installable by root without a
   toolchain (their #327).

Anything short of both changes nothing above. If both land, the starting point is this
document plus #54, not a fresh investigation.
