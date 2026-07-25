# Session lifecycle

A session *is* a dtach socket: `$WT_DIR/<name>.sock`, default `~/.dtach` [bin/wt:18].
There is no registry, no database, no state file — the filesystem is the single source
of truth, and both pickers (the bash menu in `bin/wt`, the JSON API in `wtd`) observe
it independently. This document defines the states, exactly how each is observed, and
the cleanup ordering that keeps the two pickers from ever disagreeing about what
exists. Every phantom-session bug is a violation of something on this page.

Sibling documents: [`ws-protocol.md`](ws-protocol.md), [`openapi.yaml`](openapi.yaml),
[`compatibility.md`](compatibility.md).

**Verification legend** — [bin/wt:line] this repo; [LAB] empirically verified on this
box, 2026-07-24, dtach 0.9 (experiments described inline); [LIVE] observed on the
running production instance's `~/.dtach`, same date; **UNVERIFIED** = believed, check
before building on it.

## 1. States and transitions

```
                     dtach -n (API create)                    dtach client attaches
                  ┌────────────────────────▶ CREATED-DETACHED ─────────────┐
                  │                                ▲                       ▼
   NONEXISTENT ───┤                                │  last client      ATTACHED
                  │                                │  detaches (^\)       │ ▲
                  │  dtach -A (deep link,          └──── DETACHED ◀───────┘ │
                  │  socket absent) /                        │    another   │
                  └─ menu n) ──▶ ATTACHED                    └── client ────┘
                                                                 attaches

   ATTACHED or DETACHED ──(shell exits / DELETE)──▶ TERMINATED (socket unlinked)

   any state with a live master ──(master SIGKILLed, reboot)──▶ STALE (off-path)
```

| Transition | Trigger | Mechanism |
|---|---|---|
| nonexistent → attached | Deep link `wt <name>` [bin/wt:47] or menu `n)` [bin/wt:83] | `dtach -A <sock> -z -r winch bash -c "cd <q>; exec bash"` — creates *and* attaches (`-A` = attach, create if needed). |
| nonexistent → created-detached | `POST /api/v1/sessions` | `dtach -n <sock> -z -r winch bash -c "cd <q>; exec bash"` — master starts, nobody attached. `-n` accepts `-z -r winch` [LAB]. See section 6 for mandatory parity. |
| created-detached / detached → attached | Deep link (`dtach -A`, socket exists → attaches) or menu number choice (`dtach -a`) [bin/wt:89] | dtach client connects to the socket; master sets the socket's owner-execute bit. |
| attached → detached | Client detaches (Ctrl-`\`) or its `wt` process dies (browser tab closes, phone reaps) | dtach client exits; on the *last* detach the master clears the execute bit. The session's processes keep running — the entire point. **Note:** a `wtd` hub's attachment does *not* exit when its WebSocket clients leave, so a deep-linked session stays in the attached state until its hub is released. |
| attached/detached → terminated | Session's shell exits (`exit`, or `DELETE /api/v1/sessions/{name}`) | Child dies → master sees PTY EOF → master exits and **unlinks its own socket** [LAB: socket gone within ~1 s of child exit]. |
| any → stale | Master dies without cleanup: SIGKILL [LAB], kernel OOM-kill, power loss, **reboot** (socket files in `$WT_DIR` persist on disk; every session's master is gone at boot) | Socket file remains; nothing listens on it. See section 4. |

Multiple simultaneous attachments to one session are legal dtach behavior — two clients
mirror the same terminal; the execute bit is set while at least one is attached. `wtd` relies
on this: a hub is one such attachment, held alongside whatever else is attached, and it
multiplexes its own WebSocket clients behind it rather than opening one dtach client each.

`systemctl restart wt.service` (and any `wtd` restart or crash) touches **none** of
this: masters and shells are in their own sessions, reparented away from the service
[LAB: masters survive their launcher]. Connected clients get dropped and reattach;
that's the restart contract in `CLAUDE.md`.

## 2. Observing state from the filesystem

| Question | Method | Verified |
|---|---|---|
| Does a session exist? | `$WT_DIR/<name>.sock` exists and is a socket (`S_ISSOCK`). Necessary but **not sufficient** — a stale socket also passes. `bin/wt`'s `sessions()` stops here (`[[ -S $f ]]`, bin/wt:52-55), which is exactly why phantoms appear in the menu. | [bin/wt:54] |
| Is it live (a master is listening)? | `connect(2)` on the socket succeeds. Stale → `ECONNREFUSED` [LAB]. Close immediately after; **write nothing** — an accidental byte would be dtach client-protocol noise (effect **UNVERIFIED**, so don't find out). | [LAB] |
| Is someone attached? | Owner-execute bit on the socket: `srwx------` attached, `srw-------` idle. dtach's master toggles it on attach/detach. | Ground truth, 5 live sessions; re-confirmed [LIVE]: one `srwx` (this conversation's own session), four `srw`. |
| Which process is the master? | Match the socket's inode in `/proc/net/unix` (last column = path, second-to-last = inode), then find the pid owning a `/proc/<pid>/fd/*` symlink to `socket:[<inode>]`. No external tools. | [LAB] |
| Which process is the session's shell? | The master's direct child (`ppid == master`). `bash -c "cd ...; exec bash"` **exec**s, so the child pid never changes — the direct child *is* the shell. | [LAB: child of master was the exec'd command, same pid] |
| Where is it working? | `readlink /proc/<child>/cwd` — live, moves when the user `cd`s. | [LAB] |
| When was it created? | Socket mtime. dtach binds the socket at creation and nothing observed rewrites it; attach/detach toggles permissions, which touches *ctime*, not mtime. That mtime survives untouched for the socket's whole life is **UNVERIFIED** in the limit — treat `createdAt` as best-effort, not forensic. | [LIVE: mtimes match known session creation days] |

These derivations back the `Session` schema in `openapi.yaml`: `name` = basename minus
`.sock`; `attached` = execute bit; `cwd`, `pid` = the child via `/proc` (null when any
step fails — permissions, races, exotic states — rather than an error: one unreadable
session must not break the whole listing); `createdAt` = mtime as RFC 3339 UTC.

**This has happened (repeated from the schema because it bites):** `scrollback-replay` has
shipped, so `wtd` holds a persistent dtach attachment to every deep-linked session in order
to capture its output. For those sessions the execute bit is **pinned on and means
nothing** — including, note, for the *whole lifetime of the hub*, not just while a client is
connected. `attached` is now derived as:

1. `wtd`'s own count of WebSocket clients for that session — exact for anyone watching
   through this server.
2. Otherwise, dtach clients found in `/proc` whose process group is not the hub's own held
   attachment. This is the only signal that can see an SSH `wt <name>` or a bash-menu attach
   to a session `wtd` is holding warm; without it every such attach would read as detached,
   permanently.
3. Otherwise, for a session no hub holds, the execute bit exactly as before.

The field's *meaning* — "someone is looking at it" — is the API contract; the execute bit is
an implementation detail no client may read directly.

A hub's own dtach process must be identified as a *client*, and "has a child" is **not** the
test that does it. When `dtach -A` creates a session it forks the master, and that master stays
a child of the client for as long as the client lives — so the client has a child too.
Measured on a live box [LAB]:

```
2220052 (dtach client, from bin/wt) -> 2220053 (dtach master) -> 2220054 (bash)
```

Only once the client exits does the master reparent to init, which is why "masters outlive
their launcher" is still true and still not sufficient. The master is therefore the dtach
process with a child that is **not itself dtach**. Getting this wrong misreads a hub's own
held attachment as its session's master, and takes `pid`/`cwd` with it.

## 3. The two pickers must not disagree

`bin/wt`'s menu and the API observe the same directory with different tools. Three
rules keep them telling the same story:

1. **Visibility parity.** The menu lists via the glob `"$DIR"/*.sock` [bin/wt:54];
   bash globs skip dotfiles, so `.foo.sock` is invisible to the menu. The API MUST
   apply the same exclusion (skip names starting with `.`) — otherwise the API lists
   sessions the menu cannot show.
2. **Read loose, create strict.** The menu accepts nearly any typed name
   [bin/wt:80-83] — `my proj` becomes `my proj.sock`. The API MUST list and MUST be
   able to DELETE such names (byte-exact match against enumerated entries, no
   validation on the read/delete side). Creation via the API is stricter —
   `^[A-Za-z0-9._-]{1,64}$`, no leading `.`, no `..` substring — and each extra rule
   exists to close a surface disagreement: leading `.` = invisible to the menu (rule 1);
   `..` anywhere = the deep-link path drops it [bin/wt:44], so a saved iOS profile
   could never reach the session the API just made.
3. **One staleness story.** The menu's `-S` test can't detect staleness; the API can.
   Rather than *listing differently*, the API reaps (section 5), converging both
   pickers on truth.

## 4. Stale sockets — the precise definition

**A stale socket is a socket file that satisfies `[[ -S $f ]]` but has no dtach master
accepting connections on it** (`connect()` → `ECONNREFUSED` [LAB]).

How they happen:

| Cause | Notes |
|---|---|
| Master SIGKILLed | [LAB] — and note the double damage: the socket goes stale **and the session's shell survives, orphaned** — running, invisible to every picker, unreachable forever. |
| Reboot / power loss | The common case. `$WT_DIR` is on disk; every socket survives the reboot, every master does not. First boot after a crash: *all* sockets are stale. |
| Kernel OOM-kill of the master | Same shape as SIGKILL. |

What each surface does when it meets one (all verified):

| Surface | Behavior |
|---|---|
| Menu listing | Lists the phantom — `[[ -S ]]` passes [bin/wt:54]. |
| Menu numbered attach (`dtach -a`) | Fails: `dtach: <sock>: Connection refused`, exit 1, **socket left in place** [LAB]. The menu loop survives and redraws — this is why `bin/wt` has no `set -e` [bin/wt:15-16]; the user sees the error and the phantom is still listed next redraw. |
| Deep link (`dtach -A`) | **Self-heals**: `-A` detects the dead socket, recreates the session over it, attaches [LAB]. So a saved iOS profile silently recovers; only the menu path exposes phantoms to humans. |
| `dtach -n` (API create on a name that's stale) | wtd reaps the stale socket first (verified-dead → unlink), then creates — giving the API path the same self-healing the deep link gets from `-A`. |

## 5. Reaping — who removes a stale socket, and when

`wtd` reaps in three places, all using the same verified-dead test:

| When | What |
|---|---|
| Startup | Sweep `$WT_DIR`: probe every `*.sock`; unlink confirmed-stale ones. This handles the reboot case wholesale, before any client sees a phantom. |
| `GET /api/v1/sessions` | Probe each entry during the scan; unlink confirmed-stale ones and omit them from the response. |
| `POST` / `DELETE` on a specific name | Probe that socket; stale → unlink (then proceed with create, or return 204 for delete). |

**Why mutating on GET is correct here** (it usually isn't): a stale socket is not a
session — it is litter left by a crash. The alternatives are worse: listing it makes
the API repeat the menu's known bug; hiding it without reaping makes the two pickers
permanently disagree (the menu keeps showing it); adding a `stale` field pushes a
server-side cleanup job onto every client. Reaping is idempotent, converges both
pickers, and matches what `dtach -A` already does to stale sockets on its own [LAB].

**The reaping race, and its guard.** Between wtd's `ECONNREFUSED` probe and its
`unlink()`, a concurrent `dtach -A` could have self-healed the same path — unlink now
and wtd destroys a brand-new live session's socket (orphaning its shell). Guard:
`stat` the path before the probe and again immediately before `unlink`; proceed only
if device+inode are unchanged. `dtach -A`'s self-heal necessarily replaces the inode
(it binds a fresh socket), so the recreated case is always detected. The residual
window (TOCTOU between the second stat and unlink) is accepted: closing it fully
requires `unlink`-by-inode, which POSIX does not offer; the window is microseconds; and
the failure mode is the pre-existing crash-litter shape, not a new one.

`bin/wt` is deliberately **not** taught to reap. The menu's job is to survive and
redraw [bin/wt:15-16], not to make deletion decisions over the network filesystem
semantics bash gives it; and two independent reapers doubles the race surface.

## 6. Creation parity — API sessions must be indistinguishable

An API-created session that differs *at all* from a menu-created one is a fork of the
runtime contract. The rules:

| Aspect | Requirement | Why |
|---|---|---|
| Invocation | `dtach -n "$WT_DIR/<name>.sock" -z -r winch bash -c "cd <quoted>; exec bash"` | Identical flags and command shape to the picker's `-A`/`-a` paths [bin/wt:26,47,83]; `-n` is the only difference (create without attach — the API has no terminal to attach). `-n` accepts these flags [LAB]. `-z` (Ctrl-Z passthrough) and `-r winch` (redraw on attach) are also re-supplied by `bin/wt` at every attach — but keeping them here means the created master is argv-identical in `ps`, and nothing ever diverges if dtach's flag semantics shift between create and attach time. |
| Environment | `WT=1` exported, plus `WT_DIR` and (when configured) `WT_PROJECTS`, plus `TERM=xterm-256color`, in the service user's normal environment. | `WT=1` is how a login shell detects it is inside a web session and skips auto-launching tmux [bin/wt:21-23, docs/bashrc-snippet.sh]. A session created without it recurses into a multiplexer for any user with that snippet — inside a dtach session, which is precisely the mess the variable exists to prevent. The export rule is `CLAUDE.md` canon: un-exported settings silently never arrive. |
| Command string quoting | The path is embedded in the `bash -c` string with POSIX single-quote escaping (`'` → `'\''`) — the same effect as `${d@Q}` [bin/wt:47]. Paths containing bytes < 0x20 or 0x7F are **rejected at validation** (`invalid_path`), never escaped. | The path inside `bash -c` is the one place request input meets a shell. `bin/wt` solved it with `${d@Q}` for its own (menu-typed) input; the API keeps the identical command shape, so it must solve the identical problem. Refusing control bytes instead of quoting them keeps the quoting function trivially auditable — `${d@Q}` switches to `$'...'` encoding for control bytes, and *"our Go quoter perfectly reproduces bash's `@Q` in all cases"* is exactly the kind of claim that ends up false. |
| Working directory resolution | `project` → look up in the projects file (same parser as bin/wt:30-37); its path must exist (`project_path_missing` otherwise). `path` → must be absolute, exist, be a directory (`invalid_path`). Neither → service user's `$HOME`. Both → `path_and_project`. | `bin/wt` silently falls back to `$HOME` on a missing directory [bin/wt:46,82] — right for a human mid-menu, wrong for a program: the API tells the caller instead of guessing. The *default* (no path, no project → `$HOME`) does match the picker. |
| Resulting state | `attached: false`, execute bit clear, `pid`/`cwd` resolvable immediately [LAB: child and cwd readable right after `dtach -n` returns]. | The `201` response body is a real `Session` object, not an optimistic echo. |

Validation ordering for POST: Origin policy → Content-Type → body size → JSON parse →
name rules → path/project rules → stale-reap → `dtach -n`. A lost `dtach -n` bind race
(concurrent create of the same name) surfaces as `already_exists` (409), same as if
the pre-check had caught it.

## 7. Cleanup ordering for DELETE — the part that prevents phantoms

Two wrong orderings, two distinct phantoms, both verified:

- **Unlink first, kill later (or never):** the master and shell keep running with no
  socket — a session that is running, invisible to both pickers, and now impossible to
  attach or delete. The worst outcome; nothing recovers it but manual `kill`.
- **Kill the master first (SIGKILL):** stale socket *and* an orphaned, still-running
  shell [LAB]. `bin/wt` then lists a phantom that cannot be attached — the exact
  botched-kill scenario this document exists to prevent.

Therefore the inviolable rules: **never unlink a socket that has a live master; never
signal the master while its child lives.** Kill from the leaves; let dtach clean up
after itself — the master unlinks its own socket when its child dies [LAB].

The procedure:

1. **Resolve.** Enumerate `$WT_DIR`; byte-match `{name}` against derived names — the
   request string is never used to build a path. No match → 404.
2. **Probe.** `connect()` the socket. `ECONNREFUSED` → stale: re-stat (section 5
   guard), unlink, **204**. Done.
3. **Identify.** Master pid via the `/proc/net/unix` inode walk; child = master's
   direct child (section 2). If the master is live but pids cannot be resolved →
   **500**, socket untouched (never unlink what might be alive).
4. **Kill the child, escalating:** SIGHUP (terminal-hangup semantics — lets bash run
   hooks and exit cleanly), wait 2 s; SIGTERM, wait 3 s; SIGKILL, wait 2 s. Signal the
   child's process group if it leads one (background jobs die too), else the pid.
5. **Let the master finish.** On child death the master exits and unlinks the socket
   itself [LAB]. Wait up to 3 s for the socket to disappear — normally it is gone in
   well under a second.
6. **Master straggler:** child confirmed dead but master alive past the wait —
   SIGTERM the master, wait 2 s. (SIGKILL as the final resort; this is the one case
   where killing the master is safe, because its child is already dead.)
7. **Socket straggler:** all processes confirmed dead but the file remains — now it
   is by definition stale; re-stat and unlink.
8. Socket gone → **204**. Anything still standing after the full escalation → **500**
   with `detail`, socket untouched, state still observable for a retry.

Worst-case wall time is bounded (~12 s) so the DELETE handler can stay synchronous —
one request, one definitive answer, no job-polling machinery for an endpoint whose
common case completes in milliseconds.

Deleting an **attached** session is legal: the attached dtach clients exit when the
session dies, and each connected `wt` menu survives and redraws [bin/wt:15-16,58-59] —
the phone user watching that session lands back at the picker, not at a dropped
connection. The API does not second-guess the caller; the picker UI in front of a
human is the place for "are you sure".
