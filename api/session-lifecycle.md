# Session lifecycle

> **Maintainer's copy, served for reference.** This document is written for someone working in
> the `ttyd-ify` repository. Bracketed citations — `[cmd/wtd/config.go: func (s *server) sessionDir]`, `[cmd/wtd/sessions.go: listSessions]` —
> name files in a repository you were not given, and tags like `[LAB]` and `[LIVE]` record how a
> claim was verified there. Neither is something you need in order to build a client: skip them,
> because every observable is stated in the prose beside them. What follows explains the session
> model *behind* the API, including server-side detail (systemd, install paths, `/proc`) you
> cannot observe over the wire. The contract a client codes against is
> [`/openapi.json`](/openapi.json).

A session *is* a dtach socket: `$WT_DIR/<name>.sock`, default `~/.dtach`
[cmd/wtd/config.go: func (s *server) sessionDir].
There is no registry, no database, no state file — the filesystem is the single source
of truth, and every surface that can create a session (the JSON API, the deep link) observes
it independently. This document defines the states, exactly how each is observed, and
the cleanup ordering that keeps the two pickers from ever disagreeing about what
exists. Every phantom-session bug is a violation of something on this page.

Sibling documents: [`ws-protocol.md`](ws-protocol.md), [`/openapi.json`](/openapi.json),
[`compatibility.md`](compatibility.md).

**Verification legend** — a path plus a colon plus a greppable anchor (never a line number, see `ws-protocol.md` section 0) cites this repo; [LAB] empirically verified on this
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
                  └─ deep link ──▶ ATTACHED                  └── client ────┘
                                                                 attaches

   ATTACHED or DETACHED ──(shell exits / DELETE)──▶ TERMINATED (socket unlinked)

   any state with a live master ──(master SIGKILLed, reboot)──▶ STALE (off-path)
```

| Transition | Trigger | Mechanism |
|---|---|---|
| nonexistent → attached | Deep link `/ws?arg=<name>` [cmd/wtd/attach.go: func attachCommand] | `dtach -A <sock> -z -r winch bash -c "cd <q>; exec bash"` — creates *and* attaches (`-A` = attach, create if needed). |
| nonexistent → created-detached | `POST /api/v1/sessions` | `dtach -n <sock> -z -r winch bash -c "cd <q>; exec bash"` — master starts, nobody attached. `-n` accepts `-z -r winch` [LAB]. See section 6 for mandatory parity. |
| created-detached / detached → attached | Deep link (`dtach -A`, socket exists → attaches), or `dtach -a` over SSH | dtach client connects to the socket; master sets the socket's owner-execute bit. |
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

That contract depends on **`KillMode=process` in `wt.service`**, and it is the one line
holding it up. Reparenting is not the whole story: a master forked by the server keeps the
server's cgroup for life, PID 1 parent or not, and systemd's default `KillMode=control-group`
signals the entire cgroup on stop. Under the default, restarting a unit destroys every session
that unit created — observed, with the shutdown log asserting the opposite as it happened
(#21). The `[LAB]` note above measured reparenting and never measured cgroup membership, which
is precisely how this went unnoticed. Deleting that line silently converts session persistence
from a guarantee into an accident of timing.

## 2. Observing state from the filesystem

| Question | Method | Verified |
|---|---|---|
| Does a session exist? | `$WT_DIR/<name>.sock` exists and is a socket (`S_ISSOCK`). Necessary but **not sufficient** — a stale socket also passes, which is why listing probes as well (section 5). | [cmd/wtd/sessions.go: listSessions] |
| Is it live (a master is listening)? | `connect(2)` on the socket succeeds. Stale → `ECONNREFUSED` [LAB]. Close immediately after; **write nothing** — an accidental byte would be dtach client-protocol noise (effect **UNVERIFIED**, so don't find out). **Three answers, not two** — see below. | [LAB] |
| Is someone attached? | Owner-execute bit on the socket: `srwx------` attached, `srw-------` idle. dtach's master toggles it on attach/detach. | Ground truth, 5 live sessions; re-confirmed [LIVE]: one `srwx` (this conversation's own session), four `srw`. |
| Which process is the master? | Walk `/proc` once; a candidate is any process whose `argv[0]` basename is `dtach` and whose arguments contain a `*.sock` path in `$WT_DIR`. The master is the candidate with a child that is **not** itself `dtach` (see below — "has a child" alone picks the wrong one). No external tools. An inode walk over `/proc/net/unix` would also work and is what an earlier draft of this document specified; the code has always used the argument match, which needs no second lookup to get from the socket to the pid. | [LAB] |
| Which process is the session's shell? | The master's direct child (`ppid == master`). `bash -c "cd ...; exec bash"` **exec**s, so the child pid never changes — the direct child *is* the shell. | [LAB: child of master was the exec'd command, same pid] |
| Where is it working? | `readlink /proc/<child>/cwd` — live, moves when the user `cd`s. | [LAB] |
| When was it created? | Socket mtime. dtach binds the socket at creation and nothing observed rewrites it; attach/detach toggles permissions, which touches *ctime*, not mtime. That mtime survives untouched for the socket's whole life is **UNVERIFIED** in the limit — treat `createdAt` as best-effort, not forensic. | [LIVE: mtimes match known session creation days] |

**"Is it live" has three answers, and only one of them authorizes unlinking.** A failed
`connect(2)` is not the same fact as `ECONNREFUSED`:

| Answer | Means | May the socket be unlinked? |
|---|---|---|
| connected | a master is listening | never |
| `ECONNREFUSED`, or the file is gone | nothing is bound to it: stale | yes — this is what reaping is for |
| failed, but the path is one `connect(2)` **cannot express** | the probe is impossible, not negative | only if `/proc` shows no dtach process holding it |
| any other failure | **could not find out** | **never** |

The bottom two rows are not defensive programming, they are a fixed bug. A socket path over
**107 bytes** (`sockaddr_un.sun_path` is 108 including its NUL; the boundary is exact [LAB: 107
binds, 108 → `AF_UNIX path too long`]) cannot be *named* in a `connect(2)` at all — but `dtach`
binds one anyway, by `chdir`ing and binding a short relative name. So the session is alive and
running while being unreachable by name, and reading "connect failed" as "stale" **deleted it**:
master and shell still running with no socket, which is the unrecoverable phantom section 7
describes [OBSERVED: `wtd: reaped stale sessions: demo`, seconds after creation, master and shell
both alive].

**But an unnameable socket that really is dead must not become immortal either**, and the first fix
for the above made it so — skipping every unprobeable socket left the stale ones listed forever,
refused by DELETE, and sessions that list but cannot be attached. Since reboot is the *common* case for staleness
(socket files survive it, no master does), first boot on such a box would strand every session it
had. So for an unnameable path the question moves to `/proc`: **does any live `dtach` process name
this socket?** If not, nothing is behind it, and that is stronger evidence than the probe it stands
in for — the probe is impossible, not merely negative. This fallback is deliberately **not**
extended to a dial that merely failed on a nameable path: there, something may well be listening,
and on a box with `hidepid` the `/proc` answer would be empty for everything and the reaper would
eat the lot.

Consequences worth stating explicitly, because they are not symmetrical:

- **Reaping** requires proof. It runs on the *read* path, so merely listing sessions deletes
  files, with no confirmation and nothing reported to the caller.
- **DELETE needs no connection either way.** Signalling the session's shell doesn't touch the
  socket — the pid comes from `/proc` — and the master unlinks its own socket when its child dies.
  So DELETE works on an unprobeable session whether it is alive (signal it) or dead (unlink it),
  and is the recovery path for both.
- **Creating** is refused up front instead: a name whose socket path would not fit gets
  `invalid_name` (400) naming how many characters do fit. Creating it would not fail — that is
  the problem. The deep link (`?arg=`) refuses the same names, in `validateAttachName` — same
  ceiling, different answer, because a client cannot read an error: it falls through to the
  ceiling, from one implementation shared with `POST` [cmd/wtd/attach.go: func validateAttachName].
  Every path a client can reach is now covered, which is what closed #16; the startup warning
  remains for a directory deep enough that short names are affected too.
- **Unlinking is guarded on both paths.** A concurrent `dtach -A` can self-heal a stale socket
  between the decision and the `unlink` — a phone reconnecting on a `sessionArg` deep link does
  this unprompted — so identity is compared across the window with `SameFile` and the liveness
  question asked again after. One implementation serves both callers; see section 7 step 2.

These derivations back the `Session` schema in `openapi.yaml`: `name` = basename minus
`.sock`; `attached` and `attachedCount` = the three-signal derivation below — the execute bit
alone stopped being sufficient when `scrollback-replay` shipped; `cwd`, `pid` = the child via
`/proc` (null when any
step fails — permissions, races, exotic states — rather than an error: one unreadable
session must not break the whole listing); `createdAt` = mtime as RFC 3339 UTC.

**This has happened (repeated from the schema because it bites):** `scrollback-replay` has
shipped, so `wtd` holds a persistent dtach attachment to every deep-linked session in order
to capture its output. For those sessions the execute bit is **pinned on and means
nothing** — including, note, for the *whole lifetime of the hub*, not just while a client is
connected. `attached` and `attachedCount` are both derived from:

1. `wtd`'s own count of WebSocket clients for that session — exact for anyone watching
   through this server.
2. **Plus** dtach clients found in `/proc` whose process group is not the hub's own held
   attachment. This is the only signal that can see an SSH `dtach -a` attach
   to a session `wtd` is holding warm; without it every such attach would read as detached,
   permanently.
3. If that sum is zero and no hub holds the session, the execute bit exactly as before.

Signals 1 and 2 are **summed, not tried in turn.** They count different populations and one
session can have both at once, so stopping at the first non-empty one would undercount. That
was invisible while this produced a boolean — either signal alone gave the right `true` — and
stopped being invisible the moment the sum became `attachedCount`.

Signal 3 cannot be counted: it is one bit. A session it reports as attached is therefore
`attached: true` with `attachedCount: null` — "someone is there and this server cannot
enumerate them" — never a fabricated `1`. Internally that is the pair `(true, 0)`, which the
counting signals can never return, since they only ever reach `true` by counting something.

The field's *meaning* — "someone is looking at it" — is the API contract; the execute bit is
an implementation detail no client may read directly. `attachedCount` counts that same
population best-effort: exact for viewers through this server, and for external ones only as
good as the `/proc` walk — which can miss a process it may not inspect, and which counts a
master whose shell has already exited as a viewer for the ~1 s before that master exits too
(`sessionShell` returns false for it, so `scanDtach` buckets it as a client). Both directions
are possible, which is why the schema promises "best-effort" and not "lower bound".

A hub's own dtach process must be identified as a *client*, and "has a child" is **not** the
test that does it. When `dtach -A` creates a session it forks the master, and that master stays
a child of the client for as long as the client lives — so the client has a child too.
Measured on a live box [LAB]:

```
2220052 (dtach client, spawned by wtd) -> 2220053 (dtach master) -> 2220054 (bash)
```

Only once the client exits does the master reparent to init, which is why "masters outlive
their launcher" is still true and still not sufficient. The master is therefore the dtach
process with a child that is **not itself dtach**. Getting this wrong misreads a hub's own
held attachment as its session's master, and takes `pid`/`cwd` with it.

## 3. Two ways to create, one story about what exists

A session can be created by `POST /api/v1/sessions` or by deep-linking `/ws?arg=<name>`, and
listing must show whatever either produced. Two rules keep that coherent:

1. **Read loose, create strict.** The deep link accepts nearly any name
   [cmd/wtd/attach.go: func validateAttachName] — `my proj` becomes `my proj.sock`. Listing MUST
   show such names and DELETE MUST accept them (byte-exact match against enumerated entries, no
   validation on the read/delete side). Creation via `POST` is stricter —
   `^[A-Za-z0-9._-]{1,64}$`, no leading `.`, no `..` substring. Of those, only the `..` rule still
   closes a real disagreement: a name containing `..` is refused by the deep link, so a saved iOS
   profile could never reach a session `POST` had created that way.

   The leading-`.` rule was inherited from the bash menu, which globbed `"$DIR"/*.sock` — bash
   globs skip dotfiles, so such a session was invisible there. That menu is gone, and listing never
   implemented the exclusion anyway: a session named `.hidden` **is** listed today
   [LAB: verified 2026-07-29]. The rule is kept deliberately, for the same kind of reason with a
   different observer — `ls $WT_DIR` does not show `.hidden.sock` either — and because dropping a
   create-side restriction widens the contract permanently. The deep link accepts such names, so
   the asymmetry is real and intended (#51).
2. **One staleness story.** Existence by `S_ISSOCK` alone cannot detect staleness; probing can.
   Rather than *listing differently* from what is attachable, listing reaps (section 5) so that
   what it reports and what can be attached converge.

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
| SSH `dtach -a` | Fails: `dtach: <sock>: Connection refused`, exit 1, **socket left in place** [LAB]. |
| Deep link (`dtach -A`) | **Self-heals**: `-A` detects the dead socket, recreates the session over it, attaches [LAB]. So a saved iOS profile silently recovers. |
| `dtach -n` (API create on a name that's stale) | wtd reaps the stale socket first (verified-dead → unlink), then creates — giving the API path the same self-healing the deep link gets from `-A`. |

## 5. Reaping — who removes a stale socket, and when

`wtd` reaps in two places, both using the same verified-dead test:

| When | What |
|---|---|
| `GET /api/v1/sessions` | Sweep `$WT_DIR` before the scan: unlink every confirmed-dead `*.sock` and omit it from the response. This handles the reboot case wholesale — the first listing after boot clears it, before any client sees a phantom. |
| `POST` / `DELETE` on a specific name | Probe that socket; dead → unlink (then proceed with create, or return 204 for delete). |

There is deliberately **no startup sweep**. It would add a third reaper for no benefit: `wtd`
serves no session state before its first request, so the first `GET` is early enough, and a sweep
that runs before the process is even listening is a sweep nobody can see the result of. (An earlier
draft of this document specified one; the code never had it.)

**Why mutating on GET is correct here** (it usually isn't): a stale socket is not a
session — it is litter left by a crash. The alternatives are worse: listing it makes
the API report a session nothing can attach to; hiding it without reaping leaves the socket
on disk forever, so the name stays unusable by `POST`; adding a `stale` field pushes a
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

**`GET /api/v1/host` reads the same listing and deliberately does not reap.** It exists to be
polled by an open panel, and a document a client refreshes every few seconds must not be the thing
that unlinks files. Two consequences a client has to know: a stale socket appears in that route's
`sessions` array — as a row with `pid: 0` and no cost — where `GET /api/v1/sessions` would have
removed it and left it out, so the first host reading after a reboot shows every session that
predates the reboot; and joining the two routes on `name` can therefore find rows on one side with
no partner on the other until something lists. Reaping stays on the routes above.

Reaping is deliberately **not** done from more than one place. Two independent reapers double the
race surface, and the read path is the one every client already takes — so listing makes the
deletion decisions.

## 6. Creation parity — the two paths must be indistinguishable

A session created by `POST` that differs *at all* from one created by deep link is a fork of the
runtime contract. Both build their argv from `dtachArgs`
[cmd/wtd/attach.go: func dtachArgs], and `TestDtachArgsCreateAndAttachAgree`
[cmd/wtd/attach_test.go: func TestDtachArgsCreateAndAttachAgree] asserts the only difference is
the mode flag — so this section documents a property the test enforces, rather than asking two
command lines to stay in step by hand. The rules:

| Aspect | Requirement | Why |
|---|---|---|
| Invocation | `dtach -n "$WT_DIR/<name>.sock" -z -r winch bash -c "cd <quoted>; exec bash"` | Byte-identical to the deep link's `-A` invocation apart from the mode flag, because both come from `dtachArgs` [cmd/wtd/attach.go: func dtachArgs]; `-n` creates without attaching, which is what the API needs since it has no terminal. `-n` accepts these flags [LAB]. `-z` (Ctrl-Z passthrough) and `-r winch` (redraw on attach) matter to the *attach* path, but keeping them here means the created master is argv-identical in `ps` and nothing diverges if dtach's flag semantics shift between create and attach time. |
| Environment | `WT=1`, `TERM=xterm-256color` and `WT_SESSION=<name>`, in the service user's normal environment. | Built in one place for both paths [cmd/wtd/attach.go: func sessionEnv], because this row is an invariant and it used to be maintained by two identical lines in two files. `WT=1` is how a login shell detects it is inside a web session and skips auto-launching tmux. A session created without it recurses into a multiplexer for any user with that snippet — inside a dtach session, which is precisely the mess the variable exists to prevent. `WT_DIR`/`WT_PROJECTS` are deliberately **not** in this list any more: `wtd` reads them itself rather than passing them to a child, which is what removed the silent-failure class behind #28. `TERM` is the one value that cannot be inherited: the server is a systemd unit and systemd supplies no usable `TERM`, so it must be set explicitly at creation the way the `/ws` paths set it. The master captures the environment permanently, so a session born without `TERM` renders colorless at every later attach and no client can repair it — the attaching client's `TERM` is the client's, and never reaches a shell the master has already started. Deleting and recreating the session is the only fix. `WT_SESSION` carries the session's own name, which nothing else in the environment does: a program inside a session cannot otherwise discover which session it is in, because the pty carries no name and an agent hook is spawned with no controlling terminal at all. It is **absent**, not empty, on a connection with no session behind it — an argless one, or one whose name was unusable — so `unset` is a usable test for "this is not a session". Two cautions for anything that consumes it: the value is safe to use as a filename only because both creation paths validate the name first, so a reader that turns it back into a path should re-apply that check rather than trust the variable; and because the attach side is deliberately more permissive than the create side, the value can contain spaces and non-ASCII, so shell consumers must quote it. |
| Command string quoting | The path is embedded in the `bash -c` string with POSIX single-quote escaping (`'` → `'\''`) [cmd/wtd/sessionops.go: func shellQuote]. Paths containing bytes < 0x20 or 0x7F are **rejected at validation** (`invalid_path`), never escaped. | The path inside `bash -c` is the one place request input meets a shell, and both create and attach build that string, so one quoter serves both. Refusing control bytes instead of escaping them keeps the quoter trivially auditable: bash's `${var@Q}` switches to `$'...'` encoding for control bytes, and *"our Go quoter perfectly reproduces bash's `@Q` in all cases"* is exactly the kind of claim that ends up false. Nothing needs to reproduce it — the requirement is a correct single-quoting, not bytewise agreement with bash. |
| Working directory resolution | `project` → look up in the projects file (name is the first whitespace-delimited token, the remainder is the path, blank lines and `#` comments ignored; an absent or unreadable file means no shortcuts rather than an error [cmd/wtd/config.go: func loadProjects]); its path must exist (`project_path_missing` otherwise). `path` → must be absolute, exist, be a directory (`invalid_path`). Neither → service user's `$HOME`. Both → `path_and_project`. | The **deep link** silently falls back to `$HOME` on a missing directory [cmd/wtd/attach.go: func attachWorkdir] — right for a human opening a terminal, wrong for a program: the API tells the caller instead of guessing. This is the one deliberate asymmetry between the two creation paths, and it is about who receives the answer, not about what a session is. The *default* (no path, no project → `$HOME`) is identical on both. |
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
  shell [LAB]. Listing then reports a session that cannot be attached — the exact
  botched-kill scenario this document exists to prevent.

Therefore the inviolable rules: **never unlink a socket that has a live master; never
signal the master while its child lives.** Kill from the leaves; let dtach clean up
after itself — the master unlinks its own socket when its child dies [LAB].

The procedure:

1. **Resolve.** Enumerate `$WT_DIR`; byte-match `{name}` against derived names — the
   request string is never used to build a path. No match → 404.
2. **Probe.** If the socket is provably dead (section 2), unlink it behind the identity
   guard and return **204**. Done.
3. **Identify.** The shell is the pid already resolved for `Session.pid` (section 2): the
   `dtach` process naming this socket whose child is not itself `dtach`, and then that
   child. Asserted again at the point of the signal — a master's `comm` is `dtach` and a
   shell's is not — because "never signal the master while its child lives" is the rule
   this whole procedure exists to keep. If the socket is not provably dead and no shell can
   be resolved → **500**, socket untouched, naming the probe state (never unlink what might
   be alive).
4. **Kill the shell, escalating:** SIGHUP (terminal-hangup semantics — lets bash run
   hooks and exit cleanly), wait 2 s; SIGTERM, wait 3 s; SIGKILL, wait 2 s. Signal the
   shell's process group if it leads one, else the pid. The group is not a nicety:
   a background job holds the pty open, so signalling only the pid leaves the master
   running and **DELETE fails outright** rather than merely orphaning the job [LAB].
5. **Let the master finish.** Each rung waits for the socket to disappear, which is how the
   master reports its child is gone — it unlinks its own socket [LAB]. Normally gone in well
   under a second.
6. Socket gone → **204**. Still standing after SIGKILL → **500** with `detail`, socket
   untouched, state still observable for a retry.

Worst-case wall time is the sum of the graces plus one probe — about 7 s — so the handler
stays synchronous: one request, one definitive answer, no job-polling machinery for an
endpoint whose common case completes in milliseconds.

**Two steps an earlier draft specified are deliberately absent.** A *master straggler* rung
(SIGTERM the master once its child is confirmed dead) would need the master's pid carried
through the program, which nothing else wants — and the master exits on child death [LAB],
so the rung would fire approximately never. A *socket straggler* unlink is redundant: once
no process holds the socket it is by definition stale, and the next listing reaps it. Both
were removed rather than implemented; if a master ever does straggle, the socket outlives it
by exactly as long as the master does.

The signal ladder is shared with a terminal's own teardown (`escalation` in `cmd/wtd/ws.go`),
because the two were specified identically and there is no reason for them to drift.

Deleting an **attached** session is legal: the attached dtach clients exit when the
session dies, and each connected client is closed with a normal 1000 —
the phone user watching that session lands back at the picker, not at a dropped
connection. The API does not second-guess the caller; the picker UI in front of a
human is the place for "are you sure".
