#!/usr/bin/env bash
# uninstall.sh — remove ttyd-ify. Keeps /etc/ttyd-ify config unless --purge.
# Does NOT remove the ttyd/dtach packages, and does NOT touch running dtach sessions.
#
#   sudo ./uninstall.sh            # remove service + binaries, keep config
#   sudo ./uninstall.sh --purge    # also remove /etc/ttyd-ify
set -euo pipefail

PREFIX="${PREFIX:-/usr/local/bin}"
CONF_DIR=/etc/ttyd-ify
UNIT=/etc/systemd/system/wt.service
WEB_UNIT=/etc/systemd/system/wt-web.service
PURGE=0; [ "${1:-}" = "--purge" ] && PURGE=1

log() { printf '\033[01;36m==>\033[00m %s\n' "$*"; }
[ "$(id -u)" = 0 ] || { echo "must run as root (sudo)" >&2; exit 1; }

log "stopping + disabling wt.service and wt-web.service"
# wt-web.service may never have been enabled (install.sh writes it but leaves it off), so
# every step here tolerates absence.
systemctl disable --now wt.service 2>/dev/null || true
systemctl disable --now wt-web.service 2>/dev/null || true
rm -f "$UNIT" "$WEB_UNIT"
systemctl daemon-reload

log "removing binaries"
rm -f "$PREFIX/wt" "$PREFIX/wt-serve" "$PREFIX/wt-web-serve" "$PREFIX/wt-bind.sh" "$PREFIX/wtd"

if [ "$PURGE" = 1 ]; then
  log "purging $CONF_DIR"
  rm -rf "$CONF_DIR"
else
  log "keeping $CONF_DIR (use --purge to remove)"
fi

echo "done. (ttyd/dtach packages and any running dtach sessions were left alone.)"
