#!/usr/bin/env bash
# Exercise the real install and uninstall paths on a throwaway machine.
#
# This exists because install.sh and uninstall.sh write to /usr/local/bin,
# /etc/ttyd-ify and /etc/systemd/system — absolute paths with no override — so they cannot
# be tested on a box you care about. uninstall.sh in particular went a long time with no
# execution behind it at all, because running it on a dev box disables the live service and
# deletes the installed binaries.
#
#   docker run --rm -v "$PWD:/src:ro" ubuntu:24.04 bash -c 'cp -r /src /w && cd /w && test/install-uninstall.sh'
#
# systemctl is stubbed: a container has no systemd, and the point here is the file
# operations, the service-user resolution and the unit rendering — not systemd itself.
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
silent_install() { WT_USER=testuser ./install.sh >/dev/null 2>&1; }

# systemd is absent in a container; stub it so install.sh's set -e does not abort.
if ! command -v systemctl >/dev/null 2>&1; then
  printf '#!/bin/sh\nexit 0\n' > /usr/bin/systemctl
  chmod +x /usr/bin/systemctl
fi
id testuser >/dev/null 2>&1 || useradd -m testuser

BINARIES=(wt wt-serve wt-web-serve wt-bind.sh)
UNITS=(/etc/systemd/system/wt.service /etc/systemd/system/wt-web.service)

head "install"
WT_USER=testuser ./install.sh >/tmp/install.log 2>&1 || { cat /tmp/install.log; exit 1; }

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
  else bad "$(basename "$u") lost KillMode=process — restarting it would destroy its sessions"; fi
done
pass_if "config created" test -f /etc/ttyd-ify/config

head "install is idempotent"
pass_if "second install exits 0" silent_install
WT_USER=testuser ./install.sh >/tmp/install2.log 2>&1 || true
pass_if "second install skips existing pieces" grep -q 'skip' /tmp/install2.log

head "config is never clobbered"
echo "WT_BIND=sentinel-value" > /etc/ttyd-ify/config
WT_USER=testuser ./install.sh >/dev/null 2>&1
pass_if "existing config left untouched" grep -q 'sentinel-value' /etc/ttyd-ify/config

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
WT_USER=testuser ./install.sh >/dev/null 2>&1
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

echo
if [ "$fail" = 0 ]; then echo "all install/uninstall checks passed"; else echo "FAILURES above"; exit 1; fi
