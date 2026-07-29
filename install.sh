#!/usr/bin/env bash
# install.sh — install ttyd-ify (browser terminal + dtach session picker) as a systemd service.
# Idempotent: safe to re-run. Binaries always match the checkout; the config is never clobbered.
#
#   make install                   # preferred — do NOT prefix with sudo (see WT_USER below)
#   sudo ./install.sh              # equivalent when run from a login shell
#   sudo WT_USER=alice ./install.sh
#   sudo NO_ENABLE=1 ./install.sh  # install but don't start/enable the service
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NO_ENABLE="${NO_ENABLE:-0}"
PREFIX="${PREFIX:-/usr/local/bin}"
CONF_DIR=/etc/ttyd-ify
CONF_FILE="$CONF_DIR/config"
UNIT=/etc/systemd/system/wt.service
OLD_WEB_UNIT=/etc/systemd/system/wt-web.service   # retired by #23; cleaned up below

have() { command -v "$1" >/dev/null 2>&1; }
log()  { printf '\033[01;36m==>\033[00m %s\n' "$*"; }
skip() { printf '    skip %s (%s)\n' "$1" "$2"; }
note() { printf '\033[01;33mnote:\033[00m %s\n' "$*"; }
die()  { printf '\033[01;31merror:\033[00m %s\n' "$*" >&2; exit 1; }

# conf_value <KEY> — the value the launcher will see for KEY, or empty.
#
# By *sourcing* the file, because that is what bin/wt-serve does. A regex-shaped second parser
# is not a smaller version of this, it is a different answer: `WT_AUTH=""` and
# `WT_AUTH= # cleared` look non-empty to a `sed` pattern while the shell sees empty, and
# `export WT_AUTH=user:pass` looks empty while the shell sees a password. Since the whole job
# of the pre-flight below is to predict what the launcher will decide, that agreement has to
# come from construction rather than from matching enough shapes.
conf_value() {
  [ -r "$CONF_FILE" ] || return 0
  # set +u: a config referencing an unset variable must not abort the install.
  # shellcheck source=/dev/null
  ( set +u; . "$CONF_FILE" >/dev/null 2>&1; printf '%s' "${!1-}" )
}

[ "$(id -u)" = 0 ] || die "must run as root (use: make install  or  sudo ./install.sh)"

# Which account the web shell runs as. Resolved in priority order, and the source is
# printed, because getting this wrong silently is the worst failure this script has:
#   1. WT_USER=<name>  — explicit, always wins, the only way to select root
#   2. $SUDO_USER      — the human behind `sudo ./install.sh` / `make install`
#   3. owner of this checkout — for a root-login box (no sudo, so SUDO_USER is unset).
#      Whoever cloned the repo is whose shell we are about to publish; that beats root.
WT_USER_SOURCE=WT_USER
if [ -z "${WT_USER:-}" ]; then
  if [ -n "${SUDO_USER:-}" ]; then
    WT_USER="$SUDO_USER"
    WT_USER_SOURCE="SUDO_USER"
  else
    WT_USER="$(stat -c %U "$SCRIPT_DIR" 2>/dev/null || echo root)"
    WT_USER_SOURCE="owner of $SCRIPT_DIR"
  fi
fi
id "$WT_USER" >/dev/null 2>&1 || die "WT_USER='$WT_USER' is not a valid user"

# Refuse, don't warn. A root-owned writable shell reachable over the network is a
# privilege escalation, so it must be asked for by name rather than fallen into. The
# usual accidents: `sudo make install` (the recipe's own sudo nests and resets SUDO_USER
# to root) and a root-login box where the checkout is also root-owned.
if [ "$WT_USER" = root ] && [ "$WT_USER_SOURCE" != WT_USER ]; then
  die "refusing to install a web shell that runs as root.
       Resolved WT_USER=root from: $WT_USER_SOURCE

       Fix — name the account that should own the terminal sessions:
         WT_USER=<login> ./install.sh          (running as root)
         make install WT_USER=<login>          (running as a normal user)

       Did you run 'sudo make install'? Drop the sudo: 'make install'.
       To genuinely run as root, pass WT_USER=root explicitly."
fi
log "service user: $WT_USER (from $WT_USER_SOURCE)"
[ "$WT_USER" = root ] && printf '\033[01;33mwarning:\033[00m running the web shell as root was requested explicitly; this is discouraged\n'

# 0. Pre-flight — everything that can refuse runs BEFORE the first byte is written.
#
# This ordering is the whole point. ttyd is retired (#23), so there is no second server to
# fall back on: a refusal partway through would leave a box with a rewritten unit, no working
# server, and a human who has to work out what happened from a journal. Refusing up here
# leaves whatever is already installed running exactly as it was.
log "pre-flight"

# Which units are serving right now. Captured before anything is rewritten, because both
# answers change what the closing banner has to say.
WAS_ACTIVE=0
WEB_WAS_ACTIVE=0
if systemctl --quiet is-active wt.service 2>/dev/null; then WAS_ACTIVE=1; fi
if systemctl --quiet is-active wt-web.service 2>/dev/null; then WEB_WAS_ACTIVE=1; fi

# The config is about to be sourced by a root process. The launcher sources it as the service
# user, which is a lower bar: a config that another account can write would be root code
# execution here. Refuse rather than guess, and check it parses at all while we are looking —
# a config with a syntax error takes the *service* down at next restart, so finding out now is
# strictly better than finding out from a journal.
if [ -e "$CONF_FILE" ]; then
  if [ "$(stat -c %u "$CONF_FILE")" != 0 ]; then
    die "$CONF_FILE is not owned by root, and this script sources it as root.
       Fix the owner before installing:  chown root:root $CONF_FILE"
  fi
  if [ -n "$(find "$CONF_FILE" -maxdepth 0 -perm /022 2>/dev/null)" ]; then
    die "$CONF_FILE is writable by group or others, and this script sources it as root.
       Anyone who can write it could run commands as root:  chmod 644 $CONF_FILE"
  fi
  bash -n "$CONF_FILE" 2>/dev/null || die "$CONF_FILE is not valid shell syntax, so neither
       this script nor the server can read it (the service would fail at next restart).
       Check it with:  bash -n $CONF_FILE"
fi

# dtach is the one runtime dependency; the install itself happens in step 1. Checked here so
# an un-installable box refuses before anything is written.
have dtach || have apt-get || die "no apt-get; install 'dtach' with your package manager,
       then re-run.  (dnf install dtach / pacman -S dtach / brew install dtach)"

# wtd is not optional any more. Before retirement a Go-less box still got a working ttyd
# install, so skipping wtd was reasonable; now it would produce a unit whose ExecStart cannot
# start a server, restart-looping every 3 seconds.
#
# `-version` rather than `test -x`: presence is not the property that matters. A truncated
# download or a binary built for another architecture passes every file test and then fails to
# exec, which is the same restart loop by a different route.
WTD_SRC=""
if [ -f "$SCRIPT_DIR/wtd" ]; then
  WTD_SRC="$SCRIPT_DIR/wtd"
elif [ -x "$PREFIX/wtd" ]; then
  WTD_SRC="$PREFIX/wtd"
fi
if [ -z "$WTD_SRC" ]; then
  if have go; then
    die "no wtd binary. Build it first (NOT as root, so it does not write root-owned
       files into your checkout and Go cache):
         make build && make install"
  fi
  die "no wtd binary and no Go toolchain on this box. Fetch a released one for
       $(uname -m) — it is checksum-verified — then install. Run make fetch as
       yourself, NOT as root; it writes ./wtd into this checkout:
         make fetch && make install"
fi
[ -x "$WTD_SRC" ] || die "$WTD_SRC is not executable, so it cannot be run as a service.
       chmod +x it, or rebuild it with:  make build"
WTD_VERSION="$("$WTD_SRC" -version 2>/dev/null)" || WTD_VERSION=""
[ -n "$WTD_VERSION" ] || die "$WTD_SRC will not run on this machine ('$WTD_SRC -version'
       produced nothing). A truncated download or a binary for the wrong architecture
       ($(uname -m)) does this. Rebuild or re-fetch it:
         make build     # or: make fetch"
printf '    server binary: %s (%s)\n' "$WTD_SRC" "$WTD_VERSION"

# The settings wtd cannot honor. The launcher refuses on these too, but by then the switch has
# happened and the box is down; this is the copy a human or an agent actually reads, while the
# old server is still running. Both are access controls, so ignoring either would remove a
# restriction the operator deliberately configured — see api/compatibility.md section 4.
if [ -n "$(conf_value WT_AUTH)" ]; then
  die "WT_AUTH is set in $CONF_FILE, and wtd does not implement basic auth.
       Refusing to install rather than replace your server with one that ignores it.

       ttyd, which did implement it, is retired (#23). To continue:
         1. clear the value, so the line reads exactly  WT_AUTH=
         2. control access at the network layer instead — a tailnet ACL, a source-IP
            allowlist, or WT_BIND=localhost plus an SSH tunnel
         3. re-run the install
       Basic auth as a wtd feature is tracked in #27. Nothing was changed."
fi
if [ -n "$(conf_value WT_TTYD_ARGS)" ]; then
  die "WT_TTYD_ARGS is set in $CONF_FILE, and ttyd is retired (#23) — there is no
       server left for raw ttyd flags to apply to. wtd would ignore them, including any
       that restrict access such as -c (credentials) or -R (read-only).

       Clear the value, so the line reads exactly  WT_TTYD_ARGS=  and re-run.
       Nothing was changed."
fi

# Retiring wt-web.service means stopping it, and this install may be arriving *through* it.
# Same test cmd/wtd/survival.go uses to name its own unit.
if [ "$WEB_WAS_ACTIVE" = 1 ] && grep -q 'wt-web\.service' /proc/self/cgroup 2>/dev/null; then
  note "this session came in through wt-web.service, which this install retires. Your
      connection drops when it stops, mid-install — the dtach sessions survive, but you
      will not see the rest of this output. Reconnect on WT_PORT afterwards."
fi

# 1. dependencies. dtach only: ttyd is no longer a runtime dependency. CI still installs it to
# diff the two servers' wire behaviour (.github/workflows/ci.yml), which is a test-only
# dependency of this repo, not of a deployment.
log "dependencies: dtach"
if have dtach; then
  skip "dtach" "already installed"
else
  apt-get update -qq
  apt-get install -y dtach
fi

# 2. binaries. wtd goes first, then the launcher that execs it, then the helper the launcher
# sources: every intermediate state is then one that could actually run, rather than a launcher
# pointing at a server that is not there yet.
#
# Everything here installs what the checkout says, unconditionally (#26, #30). An existing file
# is not evidence that it is the right file: skipping one meant a *changed* binary never reached
# the box while the install log said success, which is how a rename could leave wt.service
# executing the previous server — active, serving a terminal, and not the server you think. wtd
# used to be exempt behind FORCE=1, and that exemption shipped the pre-#23 server on this very
# box; the flag is gone because the checkout is the source of truth for this too.
#
# `cmp` decides only what to *report*, so a reinstall that changes nothing says so instead of
# claiming work it did not do.
log "binaries -> $PREFIX"
if [ "$WTD_SRC" != "$SCRIPT_DIR/wtd" ]; then
  # Pre-flight proved $PREFIX/wtd runs, and there is nothing in the checkout to replace it with.
  skip "wtd" "already installed ($WTD_VERSION), and no ./wtd here to replace it"
elif cmp -s "$WTD_SRC" "$PREFIX/wtd"; then
  skip "wtd" "unchanged ($WTD_VERSION)"
else
  was="none"
  if [ -x "$PREFIX/wtd" ]; then
    was="$("$PREFIX/wtd" -version 2>/dev/null)" || was="unknown"
  fi
  install -m 0755 "$WTD_SRC" "$PREFIX/wtd"
  printf '    installed %s (%s -> %s)\n' "$PREFIX/wtd" "$was" "$WTD_VERSION"
fi

# wt-serve is the only executable script left to install: the bash picker it used to launch was
# retired when wtd took over attaching to dtach (#49). Was a loop over two names.
install -m 0755 "$SCRIPT_DIR/bin/wt-serve" "$PREFIX/wt-serve"
printf '    installed %s\n' "$PREFIX/wt-serve"

# wt-bind.sh is sourced, not executed, so it is installed non-executable — and it must land
# beside the launcher, which is how wt-serve finds it in both a checkout and an install. A
# missing helper makes wt-serve refuse to start rather than lose bind resolution silently, so
# this is not optional.
install -m 0644 "$SCRIPT_DIR/bin/wt-bind.sh" "$PREFIX/wt-bind.sh"
printf '    installed %s (sourced helper)\n' "$PREFIX/wt-bind.sh"

# 3. config (never clobber an existing config)
log "config -> $CONF_DIR"
install -d -m 0755 "$CONF_DIR"
for f in config projects; do
  if [ -e "$CONF_DIR/$f" ]; then
    skip "$f" "exists, left untouched"
  else
    install -m 0644 "$SCRIPT_DIR/etc/$f.example" "$CONF_DIR/$f"
    printf '    created %s\n' "$CONF_DIR/$f"
  fi
done

# A config written while both servers existed still carries WT_WEB_PORT. The launcher warns and
# ignores it; say so here too, because this is where someone is watching.
retired_web_port="$(conf_value WT_WEB_PORT)"
if [ -n "$retired_web_port" ]; then
  note "WT_WEB_PORT=$retired_web_port in $CONF_FILE is retired and ignored — WT_PORT is the
      port now. Delete the line. If WT_WEB_PORT was not $(conf_value WT_PORT), set WT_PORT to
      that value or your clients have to change ports."
fi

# 4. systemd units (render User=)
log "systemd units"
sed "s|__WT_USER__|$WT_USER|" "$SCRIPT_DIR/systemd/wt.service" > "$UNIT"
printf '    %s (User=%s)\n' "$UNIT" "$WT_USER"

# Retire the second unit, and its launcher with it. Stopping it drops any client connected to
# it — but its dtach sessions survive (KillMode=process), and leaving it enabled would keep a
# second unauthenticated shell port listening beside the one wt.service now owns. Removing a
# port is the safe direction, which is why this happens without asking.
#
# Disable before deleting the launcher, and in this whole block before `enable --now` below: a
# box that followed the old migration advice has WT_WEB_PORT=7681, so its old server must have
# let go of the port before wtd tries to claim it.
if [ -e "$OLD_WEB_UNIT" ]; then
  systemctl disable --now wt-web.service 2>/dev/null || true
  if systemctl --quiet is-active wt-web.service 2>/dev/null; then
    note "wt-web.service would not stop. It is still serving a shell on its own port, which
      nothing manages any more. Stop it by hand:  systemctl stop wt-web.service"
  fi
  rm -f "$OLD_WEB_UNIT"
  printf '    removed wt-web.service (retired; its dtach sessions are untouched)\n'
fi
if [ -e "$PREFIX/wt-web-serve" ]; then
  rm -f "$PREFIX/wt-web-serve"
  printf '    removed %s (retired with its unit)\n' "$PREFIX/wt-web-serve"
fi

systemctl daemon-reload

# 5. enable + start
if [ "$NO_ENABLE" = 1 ]; then
  if [ "$WAS_ACTIVE" = 1 ]; then
    log "NO_ENABLE=1 — the unit is already running the previous server; complete the switch with: systemctl restart wt.service"
  else
    log "NO_ENABLE=1 — not starting; run: systemctl enable --now wt.service"
  fi
  if [ "$WEB_WAS_ACTIVE" = 1 ]; then
    note "wt-web.service was serving and has been stopped, and NO_ENABLE=1 means nothing
      replaced it. This box is serving no terminal right now."
  fi
else
  log "enabling + starting wt.service"
  systemctl enable --now wt.service
  systemctl --no-pager --quiet is-active wt.service && printf '    active\n' || printf '    (check: systemctl status wt.service)\n'
fi

show_bind="$(conf_value WT_BIND)"
show_port="$(conf_value WT_PORT)"
cat <<EOF

ttyd-ify installed.
  edit    $CONF_FILE     (WT_BIND / WT_PORT / shortcuts)
  status  systemctl status wt.service
  logs    journalctl -u wt.service -f
  bound   journalctl -u wt.service --no-pager | grep 'wt-serve: wtd on' | tail -1
EOF

# `enable --now` starts a stopped unit but does not restart a running one, so a box that was
# already serving is still running the process it started with — the old binary, and on an
# upgrade from before #23 that means ttyd rather than wtd. Saying "installed" and stopping
# there is how a code change ends up on disk and not running.
if [ "$WAS_ACTIVE" = 1 ] && [ "$NO_ENABLE" != 1 ]; then
  cat <<'EOF'
The unit was already running, so the process serving right now is still the previous one.
One command completes the switch — and it DROPS every connected client (a phone mid-task,
and your own terminal if this session came in through it). dtach sessions survive it:
  sudo systemctl restart wt.service
EOF
fi

cat <<EOF

SECURITY: this is a writable, unauthenticated shell. It is only as private as the
interface it binds to (WT_BIND=${show_bind:-?}, port ${show_port:-7681}).
Keep it on a trusted network (tailnet/localhost). Never expose it to the public internet.
EOF
