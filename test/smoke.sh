#!/usr/bin/env bash
# Install ttyd-ify on a throwaway machine with REAL systemd and prove the box serves a terminal.
#
# This is the sibling of test/install-uninstall.sh and the two cover different questions. That one
# runs in a plain container, where it has to stub `systemctl` (no init) and `wtd` (no Go), so what it
# can assert is that the right files land with the right modes and that the pre-flight refusals fire
# before anything is written. Everything about the *result* — a unit that starts, a port that
# answers, a session that survives a restart — was out of reach there, and therefore untested: CI
# had never once started wt.service (#79). This script is that half, and it needs an init to do it.
#
# So it deliberately does NOT re-assert what the sibling already covers. The uninstall block below
# is two assertions rather than eight for that reason; the file operations are checked in
# test/install-uninstall.sh, faster and without needing a privileged container.
#
#   make smoke        # builds the image, boots it, runs this, tears it down
#
# or by hand, which is what that target does and what the `smoke` job in .github/workflows/ci.yml
# does:
#
#   docker build -f test/Dockerfile.systemd -t ttyd-ify-systemd .
#   docker run -d --name wt-smoke --privileged --tmpfs /run -v "$PWD:/src:ro" ttyd-ify-systemd
#   docker exec wt-smoke /src/test/smoke.sh      # waits for systemd itself; see wait_for_systemd
#   docker rm -f wt-smoke
#
# The checkout is mounted READ-ONLY on purpose. Nothing here writes into it — install.sh only reads
# from its own directory — so a mount that cannot be written is a guarantee rather than a
# restriction, and it is the difference between running this against a scratch copy and running it
# against the working tree of the machine you are sitting on.
#
# Runs as root and calls ./install.sh directly rather than `make install`: the image has no sudo and
# no make, and root-with-WT_USER is a documented install path (`sudo WT_USER=alice ./install.sh`).
# The SUDO_USER-resolution path and the root refusal are covered by test/install-uninstall.sh, which
# does not need an init to check them.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

WT_USER=wtuser
SESSION="smoke-$$"   # unique per run: a session outlives the install by design (that is the last
                     # assertion), so a fixed name would make a second run in the same container hit
                     # 409 on the create instead of 201, and read as a regression.

# Every path this touches is absolute — /usr/local/bin, /etc/systemd/system, /etc/ttyd-ify — and it
# ends by uninstalling whatever is there. On a developer box that means stopping the live service and
# deleting its binaries, and unlike the stubbed sibling there is a real systemd here to obey it. So:
# containers are detected, and anything else has to say so out loud.
if [ ! -e /.dockerenv ] && [ ! -f /run/.containerenv ] && [ "${ALLOW_DESTRUCTIVE:-0}" != 1 ]; then
  echo "refusing to run outside a container. On this machine it would:" >&2
  echo "  - OVERWRITE /etc/ttyd-ify/config (WT_BIND -> localhost), which install.sh itself" >&2
  echo "    never does, and then DELETE /etc/ttyd-ify entirely via uninstall --purge" >&2
  echo "  - stop and delete wt.service, and /usr/local/bin/wtd, wt-serve, wt-bind.sh" >&2
  echo "  - create a local 'wtuser' account" >&2
  echo "Run 'make smoke' instead. ALLOW_DESTRUCTIVE=1 overrides this; the config is the part" >&2
  echo "you cannot get back, so read the list again before setting it." >&2
  exit 1
fi
[ "$(id -u)" = 0 ] || { echo "must run as root" >&2; exit 1; }
# The whole reason this script exists. A stubbed or absent systemd would make every assertion below
# vacuous, which is the state test/install-uninstall.sh is already in deliberately.
#
# Polled rather than read once: `docker run -d` returns before PID 1 has exec'd systemd, so reading
# this immediately is a race that tells you to use the systemd image *while you are using it*. A
# plain container never becomes systemd, so the timeout is what still distinguishes the two.
pid1_is_systemd() {
  for _ in $(seq 20); do
    [ "$(cat /proc/1/comm 2>/dev/null)" = systemd ] && return 0
    sleep 0.5
  done
  return 1
}
pid1_is_systemd || {
  echo "PID 1 never became systemd, so there is nothing here this script can assert." >&2
  echo "Use test/Dockerfile.systemd — a plain container cannot run wt.service." >&2
  exit 1; }

fail=0
finished=0
ok()   { printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=1; }
head() { printf '\n\033[1m%s\033[0m\n' "$1"; }
skip() { printf '    skip %s (%s)\n' "$1" "$2"; }
# `have`, not a bare `command -v`, for the same reason install.sh defines one: passing the redirect
# to pass_if would silence pass_if's own ok/FAIL line rather than the lookup's output.
have() { command -v "$1" >/dev/null 2>&1; }
# Functions rather than `cmd && ok || bad`, because that idiom is not if-then-else: the bad branch
# also runs when the command succeeds and `ok` fails (SC2015). Same helpers as the sibling script.
pass_if()     { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }
pass_unless() { if "${@:2}"; then bad "$1"; else ok "$1"; fi; }
# eq <description> <got> <want> — reports both values, because "the version check failed" without
# the two strings sends whoever reads this log back to reproducing it by hand.
eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

# python3 rather than jq: the Makefile already requires python3 for the spec pipeline, and jq would
# be one more thing a fresh box needs. Field names arrive as argv rather than inside the program,
# so there is no eval of an interpolated string anywhere in here.
jfield() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$1" "$2"; }
jpid() {
  python3 -c 'import json,sys
d = json.load(open(sys.argv[1]))
print(next((s["pid"] for s in d if s["name"] == sys.argv[2]), "none"))' "$1" "$2"
}

# systemd needs a moment after the container starts before its private socket exists, and until then
# every systemctl call dies with "Failed to connect to bus" — which reads like a broken image rather
# than a race. `systemctl is-system-running --wait` cannot cover this itself: it needs the bus in
# order to wait on one, which is why neither the Makefile target nor the CI job calls it.
#
# It waits for a *settled* state, and `starting` is not one: boot still has systemd-tmpfiles ahead of
# it. `degraded` is accepted — a container legitimately fails units a real box does not, and none of
# them are ttyd-ify's.
wait_for_systemd() {
  for _ in $(seq 60); do
    case "$(systemctl is-system-running 2>/dev/null)" in
      running|degraded) return 0 ;;
    esac
    sleep 0.5
  done
  echo "systemd never finished booting in this container" >&2
  return 1
}

# ready — wait for the server to answer. /healthz exists for exactly this.
ready() {
  for _ in $(seq 40); do
    curl -fsS -o /dev/null --max-time 2 "$BASE/healthz" 2>/dev/null && return 0
    sleep 0.25
  done
  return 1
}

wait_for_systemd

# /var/tmp, not /tmp: /tmp is cleaned during boot, which deletes a scratch directory out from under
# this script and presents as a file that vanished rather than as a race.
WORK="$(mktemp -d /var/tmp/wt-smoke.XXXXXX)"
# Kept on failure, deliberately. Every response body and log this script reads lives in there, and a
# red run with no artifact left behind sends the next person back to reproducing it. `finished` also
# distinguishes a real verdict from an early abort: the bare `curl -fsS`/install calls below are
# hard failures under set -e, and without this line one of them would kill the script with no
# summary at all — the mistake install-uninstall.sh's must_install exists to avoid.
cleanup() {
  if [ "$finished" = 0 ]; then
    printf '\n\033[01;31maborted early\033[00m — a command failed outright, so the assertions after it never ran.\n' >&2
  fi
  if [ "$fail" = 0 ] && [ "$finished" = 1 ]; then rm -rf "$WORK"
  else printf 'logs and responses kept in %s\n' "$WORK" >&2; fi
}
trap cleanup EXIT

head "a fresh machine"
# The service user has to exist before the install names it. A home directory matters: sessions land
# in ~/.dtach, which is where the assertions below look for the socket.
id "$WT_USER" >/dev/null 2>&1 || useradd -m "$WT_USER"
WT_HOME="$(getent passwd "$WT_USER" | cut -d: -f6)"
# dtach is ttyd-ify's one runtime dependency and the image deliberately omits it, so this asserts the
# state install.sh is about to fix rather than assuming it — which is what makes the "installed
# dtach" assertion below evidence that install.sh did it.
#
# A skip, not a failure, when it is already there: uninstall.sh deliberately leaves the package
# alone, so a second run in the same container cannot see the fresh state. CI always starts from a
# new container, so there the strict path is the one that runs.
if have dtach; then
  skip "the fresh-machine dtach check" "already installed — a re-used container, not a fresh one"
else
  ok "dtach is not installed yet (install.sh has to do it)"
fi

# WT_BIND=tailscale is what etc/config.example ships, and it is right for the box this project runs
# on. It cannot resolve here, so the config is written first — install.sh never clobbers an existing
# one — with that single line changed and every other value straight from the example, so the
# shipped file stays under test.
#
# Writing it first is now *required*, not merely tidy: since #80, install.sh resolves the bind it is
# about to install and refuses when this machine does not have it — and a container has no tailscale,
# so an install that let the example's default through would be correctly refused. That refusal is
# asserted in test/install-uninstall.sh, which needs no init to check it. The other half of #80's fix
# — the installer no longer calling a unit that dies "active" — is asserted at the end of this file,
# because only real systemd can show it.
install -d -m 0755 /etc/ttyd-ify
sed 's/^WT_BIND=.*/WT_BIND=localhost/' etc/config.example > "$WORK/config"
grep -qx 'WT_BIND=localhost' "$WORK/config" || { echo "etc/config.example no longer sets WT_BIND — fix this sed" >&2; exit 1; }
# Read the port back out rather than hardcoding it. If the example's port changes, hardcoding turns
# this into "never answered /healthz on 7681" with no hint that the config is where to look.
# awk, not `sed | head -1`: under `set -o pipefail` head closing the pipe early kills sed with
# SIGPIPE and the whole script exits 141, which is a race that fires roughly whenever it feels like
# it. One process, no pipe, `exit` after the first match.
PORT="$(awk -F= '$1 == "WT_PORT" { print $2; exit }' "$WORK/config")"
case "$PORT" in
  ''|*[!0-9]*) echo "no numeric WT_PORT in etc/config.example (got '$PORT')" >&2; exit 1 ;;
esac
BASE="http://127.0.0.1:$PORT"
install -m 0640 -o root -g root "$WORK/config" /etc/ttyd-ify/config
ok "wrote /etc/ttyd-ify/config from etc/config.example: WT_BIND=localhost, WT_PORT=$PORT"

head "install"
WT_USER="$WT_USER" ./install.sh > "$WORK/install.log" 2>&1 || { cat "$WORK/install.log"; exit 1; }
sed 's/^/    | /' "$WORK/install.log"
# The one thing worth reading out of the install's own output: WT_USER_SOURCE, which nothing else
# reports. Whether the unit came up is asked of systemd below instead of grepped for here.
pass_if "named the service user it resolved, and where from" \
  grep -qF "service user: $WT_USER (from WT_USER)" "$WORK/install.log"
# The apt-get branch in install.sh, executed for the first time anywhere: the other container job
# pre-installs dtach, so until now nothing had ever run this.
pass_if "installed dtach, the one runtime dependency" have dtach

head "the unit is actually serving"
pass_if "wt.service is active" systemctl --quiet is-active wt.service
# The boot half. A container cannot reboot, so this is as close as this harness gets to proving the
# terminal comes back on its own.
pass_if "wt.service is enabled (comes back after a reboot)" systemctl --quiet is-enabled wt.service
if ready; then ok "answers /healthz on $PORT"; else bad "never answered /healthz on $PORT"; fi

# Polled, not read once: journald's ingestion is asynchronous relative to the server answering, so a
# single grep races it. No `|| true` either — if journalctl itself fails, that should surface as its
# own error rather than as a product assertion about a missing log line.
bound_line="wt-serve: wtd on 127.0.0.1:$PORT (bind=localhost"
found_bound=1
for _ in $(seq 20); do
  journalctl -u wt.service --no-pager > "$WORK/journal.log"
  if grep -qF "$bound_line" "$WORK/journal.log"; then found_bound=0; break; fi
  sleep 0.25
done
pass_if "logged where it bound, in the documented shape" test "$found_bound" = 0

# Who the shell actually belongs to. The stubbed job can only see `User=` as a string in a rendered
# unit; this is the running process, and getting it wrong is the worst failure install.sh has.
main_pid="$(systemctl show -p MainPID --value wt.service)"
eq "the server runs as $WT_USER" "$(stat -c %U "/proc/$main_pid" 2>/dev/null || echo unknown)" "$WT_USER"

# The project's central security invariant, against a real listening socket rather than resolve_ip's
# output. A wildcard bind is what turns a private shell into a public one, and nothing else in CI
# looks at the socket. (`ss` puts `0.0.0.0:*` in the Peer column, so the port anchor is what keeps
# the wildcard check from matching every line.)
ss -ltn > "$WORK/listeners.log"
pass_if "listening on 127.0.0.1" grep -qE "127\.0\.0\.1:${PORT}\b" "$WORK/listeners.log"
pass_unless "not listening on a wildcard address" grep -qE "(0\.0\.0\.0|\*):${PORT}\b" "$WORK/listeners.log"

head "what a client sees"
# Integration-level on purpose: cmd/wtd's own tests drive these routes through httptest, in-process,
# with no install involved. What is under test here is the binary install.sh put on the box, started
# by systemd as another user, on the port a client dials. /token is left out — conformance_test.go
# already diffs it byte-for-byte against real ttyd, which is stricter than anything possible here.
curl -fsS --max-time 10 "$BASE/api/v1/meta" > "$WORK/meta.json"
# What this proves is that `-ldflags -X main.version` survives the whole path — build, install,
# launcher, running process — and arrives at a client as /api/v1/meta's version. It cannot catch
# "new code on disk, old process serving" in this harness, because the container installs once and
# starts once; that failure needs a box that was already running.
eq "/api/v1/meta reports the version of the binary that was installed" \
  "$(jfield "$WORK/meta.json" version)" "$(./wtd -version)"
pass_if "serves the picker at /" curl -fsS -o /dev/null --max-time 10 "$BASE/"

# The upgrade itself, with curl rather than a Go client, because what is under test is the installed
# server on its real port. wtd accepts an upgrade carrying no Origin header — the native client's
# shape — so this also checks the box does not 403 the iOS app.
#
# --max-time 3: curl is expected to time out AFTER the response headers are written, and it has to be
# curl that gives up first — a client that sends no handshake is closed 1008 by the server after 10s
# (api/ws-protocol.md §5), so 3 seconds leaves seven of margin and tests the upgrade, not the
# timeout. And this probe creates no session, which is why the POST below can still
# expect 201 rather than 409 on the same name: wtd reads the client's handshake before it spawns
# anything (cmd/wtd/ws.go, api/ws-protocol.md) and curl never speaks a WebSocket frame, so `?arg=` is
# parsed and dtach is never run. Verified, not assumed — it is the only reason this ordering is safe.
curl -sS --max-time 3 -o /dev/null -D "$WORK/ws.headers" \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  -H 'Sec-WebSocket-Protocol: tty' \
  "$BASE/ws?arg=$SESSION" >/dev/null 2>&1 || true
pass_if "/ws upgrades (101)" grep -q '^HTTP/1.1 101' "$WORK/ws.headers"
pass_if "/ws selects the tty subprotocol" grep -qi '^sec-websocket-protocol: tty' "$WORK/ws.headers"

head "a session, through the API a phone uses"
code="$(curl -sS --max-time 10 -o "$WORK/create.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d "{\"name\":\"$SESSION\"}" "$BASE/api/v1/sessions" || true)"
eq "POST /api/v1/sessions returns 201" "$code" "201"
pass_if "the dtach socket exists under $WT_USER's ~/.dtach" test -S "$WT_HOME/.dtach/$SESSION.sock"
curl -fsS --max-time 10 "$BASE/api/v1/sessions" > "$WORK/list.json"
pass_if "the session is listed" grep -qF "\"$SESSION\"" "$WORK/list.json"
# `pid` is nullable per the spec — a session whose shell the server cannot inspect reports null — and
# it is re-derived from /proc on every request, which is what makes it the strong signal for
# survival below. Both the missing-session and the null-pid cases have to be caught here.
pid_before="$(jpid "$WORK/list.json" "$SESSION")"
have_pid=1
case "$pid_before" in
  ''|none|None) bad "the listed session has no pid, so restart survival cannot be checked" ;;
  *)            have_pid=0; ok "the listed session has a pid ($pid_before)" ;;
esac
pass_if "the deep link serves the terminal page" curl -fsS -o /dev/null --max-time 10 "$BASE/?arg=$SESSION"

# The assertion this whole script is named for, and the only one that exchanges terminal I/O: a real
# handshake, a command typed into the session, and its output read back. Everything above is the HTTP
# surface — a server whose relay path was broken *when run under systemd as the service user* would
# pass all of it and fail here. See test/wsprobe.py for the framing and for why the marker proves a
# shell ran the command rather than bytes making a round trip.
if python3 test/wsprobe.py "127.0.0.1:$PORT" "$SESSION" > "$WORK/wsprobe.log" 2>&1; then
  ok "a terminal really works: input reached the shell and its output came back"
  sed 's/^/      /' "$WORK/wsprobe.log"
else
  bad "no terminal I/O over /ws"
  sed 's/^/      /' "$WORK/wsprobe.log"
fi

head "a restart drops clients but keeps sessions (#21)"
# The invariant KillMode=process exists for, against real systemd. Until now CI checked only that
# the string was present in the unit file (#24), which cannot catch systemd interpreting it
# differently, an ExecStop added later, or a dtach master ending up somewhere the setting does not
# cover.
systemctl restart wt.service
if ready; then ok "came back after a restart"; else bad "did not come back after a restart"; fi
curl -fsS --max-time 10 "$BASE/api/v1/sessions" > "$WORK/list2.json"
pid_after="$(jpid "$WORK/list2.json" "$SESSION")"
# Guarded on have_pid: without it, a session that never existed makes both sides the literal 'none'
# and this prints "the session survived" in green — the one line someone would read to conclude #21
# is fine.
if [ "$have_pid" = 0 ]; then
  eq "the session survived, same shell pid" "$pid_after" "$pid_before"
else
  bad "no pid before the restart, so nothing here can say the session survived"
fi

head "uninstall"
# Two assertions, not eight. Removing the unit file, the binaries and the config is checked in
# test/install-uninstall.sh, which does it faster and without a privileged container. These two are
# the ones that need a real init and a real session.
./uninstall.sh > "$WORK/uninstall.log" 2>&1 || { cat "$WORK/uninstall.log"; exit 1; }
pass_unless "wt.service is no longer active" systemctl --quiet is-active wt.service
# `kill -0`, not `test -S`: dtach unlinks its socket on a clean exit but a killed master leaves the
# file behind, so the socket existing is not evidence the session is alive — and this is the whole
# content of uninstall.sh's promise that it leaves running sessions alone.
if [ "$have_pid" = 0 ]; then
  pass_if "left the running dtach session alive" kill -0 "$pid_after"
else
  bad "no session to check, so uninstall's leave-sessions-alone promise is unverified"
fi

head "the install will not call a dying service installed (#80)"
# The general half of #80's fix. The pre-flight predicts the common cause — a bind this box does not
# have — but it cannot predict every way a start fails, and Type=simple plus Restart=on-failure means
# a single `is-active` sample says "active" for a unit that is looping. So install.sh samples MainPID
# again after three seconds and fails if the unit did not keep it.
#
# Reproduced with a port conflict, which is precisely a failure no pre-flight can see: something else
# already holds WT_PORT, wtd exits with 'address already in use', systemd restarts it forever.
# Deliberately last, because it leaves this container with an installed-but-dead service.
python3 -c "import socket, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('127.0.0.1', $PORT))
s.listen(1)
time.sleep(120)" &
squatter=$!
sleep 0.5
# --purge removed the config above, and a fresh install.sh would refuse on the example's tailscale
# bind before it ever got as far as starting anything. Put the localhost config back so the failure
# under test is the port and nothing else.
install -d -m 0755 /etc/ttyd-ify
install -m 0640 -o root -g root "$WORK/config" /etc/ttyd-ify/config
if WT_USER="$WT_USER" ./install.sh > "$WORK/dying.log" 2>&1; then
  bad "called the install a success while the service could not bind its port"
else
  pass_if "said the service will not stay running" grep -qF 'will not stay running' "$WORK/dying.log"
  pass_if "showed the log lines that say why" grep -qiE 'address (already )?in use' "$WORK/dying.log"
  pass_unless "did not print the success banner" grep -qF 'ttyd-ify installed.' "$WORK/dying.log"
fi
kill "$squatter" 2>/dev/null || true

# The session deliberately survived everything above, so nothing else will ever clean it up. By pid,
# not by name: a pattern would also match this script's own shell.
head "cleanup"
case "${pid_after:-none}" in
  ''|none|None) skip "reaping the smoke session" "there was never a live one" ;;
  *) kill "$pid_after" 2>/dev/null || true
     ok "reaped the smoke session (pid $pid_after)" ;;
esac

finished=1
echo
if [ "$fail" = 0 ]; then echo "smoke: the installed service served a terminal"; else echo "FAILURES above"; exit 1; fi
