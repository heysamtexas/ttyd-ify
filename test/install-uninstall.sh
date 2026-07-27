#!/usr/bin/env bash
# Exercise the real install and uninstall paths on a throwaway machine.
#
# This exists because install.sh and uninstall.sh write to /usr/local/bin,
# /etc/ttyd-ify and /etc/systemd/system — absolute paths with no override — so they cannot
# be tested on a box you care about. uninstall.sh in particular went a long time with no
# execution behind it at all, because running it on a dev box disables the live service and
# deletes the installed binaries.
#
#   docker run --rm -v "$PWD:/src:ro" ubuntu:24.04 bash -c 'apt-get update -qq && \
#     apt-get install -y -qq dtach && cp -r /src /w && cd /w && test/install-uninstall.sh'
#
# systemctl and wtd are stubbed: a container has no systemd and no Go toolchain, and the point
# here is the file operations, the service-user resolution, the unit rendering and the
# pre-flight refusals — not systemd, and not the server.
set -euo pipefail

# Refuse to run anywhere that looks real. Every path this touches is absolute, so a
# careless invocation on a workstation would disable a running service and delete its
# binaries. Containers are detected; anything else has to say so out loud.
if [ ! -e /.dockerenv ] && [ ! -f /run/.containerenv ] && [ "${ALLOW_DESTRUCTIVE:-0}" != 1 ]; then
  echo "refusing to run outside a container: this installs to /usr/local/bin and" >&2
  echo "/etc/systemd/system, and uninstalls whatever is already there." >&2
  echo "Run it in a container, or set ALLOW_DESTRUCTIVE=1 if you truly mean it." >&2
  exit 1
fi
[ "$(id -u)" = 0 ] || { echo "must run as root" >&2; exit 1; }

fail=0
ok()   { printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=1; }
head() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# pass_if / pass_unless <description> <command...>
# Functions rather than `cmd && ok || bad`, because that idiom is not if-then-else: the
# `bad` branch also runs when the command succeeds but `ok` fails (shellcheck SC2015).
pass_if()   { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }
pass_unless() { if "${@:2}"; then bad "$1"; else ok "$1"; fi; }
# Two jobs, deliberately separate. silent_install returns the install's real exit status, for
# use inside pass_if. must_install is for the setup steps between assertions: it reports and
# keeps going, because a bare `install || exit` there kills this script with no FAIL line and
# no clue which block died. Neither discards the log.
silent_install() { WT_USER=testuser ./install.sh >/tmp/install.log 2>&1; }
must_install() {
  if ! silent_install; then bad "setup install failed: $1"; cat /tmp/install.log; fi
}

# systemd is absent in a container. The stub records what it was asked to do, because the
# install's most security-relevant new step is *stopping* the retired unit — with a bare
# `exit 0` stub you could delete that line and every assertion below would still pass, leaving
# a second shell port listening.
SYSTEMCTL_LOG=/tmp/systemctl.log
if [ ! -x /usr/bin/systemctl ] || ! systemctl --version >/dev/null 2>&1; then
  cat > /usr/bin/systemctl <<'STUB'
#!/bin/sh
echo "$@" >> /tmp/systemctl.log
# is-active must answer "no": a container runs nothing, and claiming otherwise would make
# install.sh describe a running service that does not exist.
case "$*" in *is-active*) exit 3 ;; esac
exit 0
STUB
  chmod +x /usr/bin/systemctl
fi
# install.sh refuses to run without a server binary (#23: there is no ttyd to fall back to),
# and this container has no Go. A stub that answers -version is enough for every file
# operation under test.
if [ ! -f ./wtd ]; then
  # shellcheck disable=SC2016  # $1 belongs to the stub being written, not to this script
  printf '#!/bin/sh\n[ "$1" = -version ] && echo "wtd stub"\nexit 0\n' > ./wtd
  chmod +x ./wtd
fi
id testuser >/dev/null 2>&1 || useradd -m testuser

BINARIES=(wt wt-serve wt-bind.sh)
UNITS=(/etc/systemd/system/wt.service)

head "install"
: > "$SYSTEMCTL_LOG"
WT_USER=testuser ./install.sh >/tmp/install.log 2>&1 || { cat /tmp/install.log; exit 1; }
pass_if "enabled and started wt.service" grep -q 'enable --now wt.service' "$SYSTEMCTL_LOG"

for b in "${BINARIES[@]}"; do
  pass_if "installed $b" test -e "/usr/local/bin/$b"
done
# Sourced, not executed — installing it executable would imply it is a command.
pass_unless "wt-bind.sh is not executable (it is sourced, not run)" test -x /usr/local/bin/wt-bind.sh
for u in "${UNITS[@]}"; do
  if grep -q '^User=testuser' "$u" 2>/dev/null; then ok "$(basename "$u") rendered User=testuser"
  else bad "$(basename "$u") missing or wrong User="; fi
done
pass_unless "no unmatched __WT_USER__ placeholders in units" grep -q '__WT_USER__' "${UNITS[@]}"
# Checked on the *installed* unit, not the template: make unit-guards already covers the
# template, and what actually decides whether a restart destroys live sessions (#21) is the
# file systemd reads. Only a rendering bug can separate the two, which is exactly the gap.
for u in "${UNITS[@]}"; do
  if grep -qx 'KillMode=process' "$u" 2>/dev/null; then ok "$(basename "$u") keeps sessions across a restart"
  else bad "$(basename "$u") lost KillMode=process — restarting it would destroy the sessions it created"; fi
done
pass_if "config created" test -f /etc/ttyd-ify/config
# The unit's ExecStart has to name a launcher that is actually there, or systemd restart-loops
# a box the install just called successful.
pass_if "wt.service ExecStart points at the installed launcher" \
  test -x "$(sed -n 's/^ExecStart=//p' /etc/systemd/system/wt.service)"

head "install is idempotent"
pass_if "second install exits 0" silent_install
WT_USER=testuser ./install.sh >/tmp/install2.log 2>&1 || true
pass_if "second install skips existing pieces" grep -q 'skip' /tmp/install2.log

head "a changed launcher reaches the box without FORCE=1 (#26)"
# The failure this guards: install skips an existing launcher, the log says success, and the
# box keeps running the old script. After #23 that could leave wt.service executing the
# *retired ttyd launcher* — active, serving a terminal, no API, and no sign anything is wrong.
printf '#!/bin/sh\necho stale launcher\n' > /usr/local/bin/wt-serve
must_install "stale launcher replacement"
pass_if "a stale wt-serve is replaced by the one in the checkout" \
  cmp -s bin/wt-serve /usr/local/bin/wt-serve

head "upgrading from the two-server layout (#23)"
# A box that opted into the Go server before ttyd was retired has a second unit and a second
# launcher. Both must go, or a second unauthenticated shell port keeps listening beside the
# one wt.service now owns. Deleting the files is not enough — the unit has to be *stopped*,
# which is why the stub records commands.
printf '#!/bin/sh\nexit 0\n' > /usr/local/bin/wt-web-serve
chmod +x /usr/local/bin/wt-web-serve
printf '[Service]\nExecStart=/usr/local/bin/wt-web-serve\n' > /etc/systemd/system/wt-web.service
: > "$SYSTEMCTL_LOG"
must_install "two-server upgrade"
pass_if "retired wt-web.service was stopped, not just deleted" \
  grep -q 'disable --now wt-web.service' "$SYSTEMCTL_LOG"
pass_unless "retired wt-web-serve removed" test -e /usr/local/bin/wt-web-serve
pass_unless "retired wt-web.service removed" test -e /etc/systemd/system/wt-web.service

head "a retired WT_WEB_PORT is a note, not a refusal"
# A config written from the example while both servers existed carries WT_WEB_PORT=7683. It must
# stay non-fatal: turning it into a refusal would block an upgrade over a key the operator never
# chose. Kept despite #23 having built this path for a config that did not contain the key —
# the test is cheap and pins the non-fatal half, which is the part worth not regressing.
cp /etc/ttyd-ify/config /tmp/config.orig
echo 'WT_WEB_PORT=7683' >> /etc/ttyd-ify/config
if WT_USER=testuser ./install.sh >/tmp/webport.log 2>&1; then
  pass_if "said it is retired and ignored" grep -q 'retired and ignored' /tmp/webport.log
else
  bad "install refused a config with WT_WEB_PORT set (must warn and proceed)"
fi
cp /tmp/config.orig /etc/ttyd-ify/config

head "config is never clobbered"
echo "WT_BIND=sentinel-value" > /etc/ttyd-ify/config
must_install "config-never-clobbered"
pass_if "existing config left untouched" grep -q 'sentinel-value' /etc/ttyd-ify/config

head "refusing a config wtd cannot honor"
# Both keys are security controls ttyd implemented and wtd does not. install.sh checks them
# before it writes anything, so the refusal arrives while the previous server is still running
# rather than in the journal of a box that no longer serves. Each asserts its own message, not
# merely a non-zero exit — "it failed" would also pass if it failed for the wrong reason.
cp /etc/ttyd-ify/config /tmp/config.bak

# refuses_with <key> <config line> <expected substring, on one line of the message>
#
# Stages a sentinel launcher and a copy of the unit first, so this asserts the *ordering* the
# pre-flight exists for — refuse before the first byte is written — and not merely a non-zero
# exit. Checking "the launcher exists" would pass whether or not the refusal came first.
refuses_with() {
  cp /tmp/config.bak /etc/ttyd-ify/config
  printf '%s\n' "$2" >> /etc/ttyd-ify/config
  printf '#!/bin/sh\necho SENTINEL\n' > /usr/local/bin/wt-serve
  cp /etc/systemd/system/wt.service /tmp/unit.bak
  : > "$SYSTEMCTL_LOG"
  if WT_USER=testuser ./install.sh >/tmp/refuse.log 2>&1; then
    bad "install accepted $1"
  else
    pass_if "refused $1 with its own message" grep -qF "$3" /tmp/refuse.log
    pass_if "refused $1 before writing the launcher" grep -q SENTINEL /usr/local/bin/wt-serve
    pass_if "refused $1 before rewriting the unit" cmp -s /tmp/unit.bak /etc/systemd/system/wt.service
    # Mutations only. The pre-flight legitimately runs `is-active` to work out what is serving,
    # and that changes nothing.
    pass_unless "refused $1 before changing systemd state" \
      grep -qE 'enable|disable|daemon-reload|restart|^stop| stop' "$SYSTEMCTL_LOG"
  fi
}
refuses_with WT_AUTH       'WT_AUTH=user:pass' 'wtd does not implement basic auth'
refuses_with WT_TTYD_ARGS  'WT_TTYD_ARGS=-R'   'server left for raw ttyd flags'

# The shapes a regex-based config reader got wrong. An empty-but-decorated value must NOT
# refuse (the launcher sources the file and sees empty), and `export` must refuse (the launcher
# sees the password). A second parser disagreeing with the launcher is the bug this covers.
cp /tmp/config.bak /etc/ttyd-ify/config
printf 'WT_AUTH=""\nWT_TTYD_ARGS=   # cleared per #23\n' >> /etc/ttyd-ify/config
pass_if "an empty-but-quoted WT_AUTH installs fine" silent_install
cp /tmp/config.bak /etc/ttyd-ify/config
printf 'export WT_AUTH=user:pass\n' >> /etc/ttyd-ify/config
if WT_USER=testuser ./install.sh >/tmp/refuse.log 2>&1; then
  bad "install accepted an exported WT_AUTH"
else
  pass_if "refused an exported WT_AUTH" grep -qF 'wtd does not implement basic auth' /tmp/refuse.log
fi

# Sourced as root, so an untrustworthy config is refused rather than executed.
cp /tmp/config.bak /etc/ttyd-ify/config
chmod 666 /etc/ttyd-ify/config
if WT_USER=testuser ./install.sh >/tmp/perm.log 2>&1; then
  bad "install sourced a world-writable config as root"
else
  pass_if "refused a world-writable config" grep -q 'writable by group or others' /tmp/perm.log
fi
chmod 644 /etc/ttyd-ify/config
printf 'WT_AUTH="unterminated\n' > /etc/ttyd-ify/config
if WT_USER=testuser ./install.sh >/tmp/syntax.log 2>&1; then
  bad "install accepted a config that is not valid shell"
else
  pass_if "refused a config that will not parse" grep -q 'not valid shell syntax' /tmp/syntax.log
fi
cp /tmp/config.bak /etc/ttyd-ify/config

head "uninstall"
./uninstall.sh >/tmp/uninstall.log 2>&1 || { cat /tmp/uninstall.log; exit 1; }
for b in "${BINARIES[@]}" wtd; do
  pass_unless "removed $b" test -e "/usr/local/bin/$b"
done
for u in "${UNITS[@]}"; do
  pass_unless "removed $(basename "$u")" test -e "$u"
done
pass_if "config kept without --purge" test -d /etc/ttyd-ify

head "uninstall --purge and idempotency"
must_install "reinstall before purge"
./uninstall.sh --purge >/dev/null 2>&1
pass_unless "--purge removed config" test -d /etc/ttyd-ify
pass_if "uninstall on a clean box exits 0 (idempotent)" ./uninstall.sh

head "refusing a root service user"
# The accident this guards is `sudo make install`, where a nested sudo resets SUDO_USER to
# root and the service would quietly end up running a root-owned shell.
if env -u WT_USER SUDO_USER=root ./install.sh >/tmp/rootcheck.log 2>&1; then
  bad "install accepted an implicit root service user"
else
  pass_if "refused an implicit root service user (not just any failure)" \
    grep -q 'refusing to install a web shell that runs as root' /tmp/rootcheck.log
fi

head "refusing to install with no server binary"
# Last, because it deliberately leaves the box without one. Before #23 a Go-less box still got
# a working ttyd install, so skipping wtd was reasonable; now it would render a unit whose
# ExecStart cannot start anything, restart-looping every 3 seconds.
rm -f ./wtd /usr/local/bin/wtd
if WT_USER=testuser ./install.sh >/tmp/nowtd.log 2>&1; then
  bad "install accepted a box with no wtd binary"
else
  pass_if "refused with no wtd binary" grep -q 'no wtd binary' /tmp/nowtd.log
fi

echo
if [ "$fail" = 0 ]; then echo "all install/uninstall checks passed"; else echo "FAILURES above"; exit 1; fi
