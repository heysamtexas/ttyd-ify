#!/usr/bin/env bash
# uninstall.sh — remove ttyd-ify. Keeps /etc/ttyd-ify config unless --purge.
# Does NOT remove the dtach package, and does NOT touch running dtach sessions.
#
#   sudo ./uninstall.sh            # remove service + binaries, keep config
#   sudo ./uninstall.sh --purge    # also remove /etc/ttyd-ify
set -euo pipefail

PREFIX="${PREFIX:-/usr/local/bin}"
CONF_DIR=/etc/ttyd-ify
UNIT=/etc/systemd/system/wt.service
# Retired by #23. Still removed here: a box installed while both servers existed may have it,
# and an uninstall that leaves a unit behind is not an uninstall.
OLD_WEB_UNIT=/etc/systemd/system/wt-web.service
PURGE=0; [ "${1:-}" = "--purge" ] && PURGE=1

log() { printf '\033[01;36m==>\033[00m %s\n' "$*"; }
[ "$(id -u)" = 0 ] || { echo "must run as root (sudo)" >&2; exit 1; }

log "stopping + disabling wt.service (and the retired wt-web.service)"
# Either unit may be absent — wt-web.service was only ever opt-in, and is gone entirely on a
# box installed after #23 — so every step here tolerates absence.
systemctl disable --now wt.service 2>/dev/null || true
systemctl disable --now wt-web.service 2>/dev/null || true
rm -f "$UNIT" "$OLD_WEB_UNIT"
systemctl daemon-reload

log "removing binaries"
# wt-web-serve is the retired second launcher and wt the retired bash picker; both are still
# removed, because an install predating their retirement has them on disk and this is the only
# thing that will ever clean them up.
rm -f "$PREFIX/wt" "$PREFIX/wt-serve" "$PREFIX/wt-narrate" "$PREFIX/wt-web-serve" "$PREFIX/wt-bind.sh" "$PREFIX/wtd"

if [ "$PURGE" = 1 ]; then
  log "purging $CONF_DIR"
  rm -rf "$CONF_DIR"
else
  log "keeping $CONF_DIR (use --purge to remove)"
fi

echo "done. (the dtach package and any running dtach sessions were left alone.)"
