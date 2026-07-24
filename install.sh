#!/usr/bin/env bash
# install.sh — install ttyd-ify (browser terminal + dtach session picker) as a systemd service.
# Idempotent: safe to re-run; already-present pieces are skipped (FORCE=1 to overwrite binaries).
#
#   make install                   # preferred — do NOT prefix with sudo (see WT_USER below)
#   sudo ./install.sh              # equivalent when run from a login shell
#   sudo WT_USER=alice ./install.sh
#   sudo NO_ENABLE=1 ./install.sh  # install but don't start/enable the service
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FORCE="${FORCE:-0}"
NO_ENABLE="${NO_ENABLE:-0}"
PREFIX="${PREFIX:-/usr/local/bin}"
CONF_DIR=/etc/ttyd-ify
UNIT=/etc/systemd/system/wt.service

have() { command -v "$1" >/dev/null 2>&1; }
log()  { printf '\033[01;36m==>\033[00m %s\n' "$*"; }
skip() { printf '    skip %s (%s)\n' "$1" "$2"; }
die()  { printf '\033[01;31merror:\033[00m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "must run as root (use: make install  or  sudo ./install.sh)"

# User the service runs as: the invoking non-root user by default. Remember whether the
# caller named it, so an accidental root fallback can be told apart from a deliberate one.
WT_USER_EXPLICIT=0; [ -n "${WT_USER:-}" ] && WT_USER_EXPLICIT=1
WT_USER="${WT_USER:-${SUDO_USER:-root}}"
id "$WT_USER" >/dev/null 2>&1 || die "WT_USER='$WT_USER' is not a valid user"

# Refuse, don't warn: a root web shell is a real privilege escalation, and the usual way
# to get here is `sudo make install` — the recipe's own sudo nests, which clobbers
# SUDO_USER to root. A warning scrolls past the rest of the output; a failure doesn't.
if [ "$WT_USER" = root ] && [ "$WT_USER_EXPLICIT" != 1 ]; then
  die "refusing to run the web shell as root.
       Did you use 'sudo make install'? That nests sudo and loses your username.
       Use:  make install          (recommended)
         or: sudo WT_USER=<you> ./install.sh
       To really run as root, pass WT_USER=root explicitly."
fi
[ "$WT_USER" = root ] && printf '\033[01;33mwarning:\033[00m running the web shell as root was requested explicitly; this is discouraged\n'

# 1. dependencies
log "dependencies: ttyd, dtach"
if have ttyd && have dtach; then
  skip "ttyd+dtach" "already installed"
elif have apt-get; then
  apt-get update -qq
  apt-get install -y ttyd dtach
else
  die "no apt-get; install 'ttyd' and 'dtach' with your package manager, then re-run.
       (dnf install ttyd dtach / pacman -S ttyd dtach / brew install ttyd dtach)"
fi

# 2. binaries
log "binaries -> $PREFIX"
for b in wt wt-serve; do
  if [ -x "$PREFIX/$b" ] && [ "$FORCE" != 1 ]; then
    skip "$b" "present (FORCE=1 to overwrite)"
  else
    install -m 0755 "$SCRIPT_DIR/bin/$b" "$PREFIX/$b"
    printf '    installed %s\n' "$PREFIX/$b"
  fi
done

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

# 4. systemd unit (render User=)
log "systemd unit -> $UNIT"
sed "s|__WT_USER__|$WT_USER|" "$SCRIPT_DIR/systemd/wt.service" > "$UNIT"
printf '    User=%s\n' "$WT_USER"
systemctl daemon-reload

# 5. enable + start
if [ "$NO_ENABLE" = 1 ]; then
  log "NO_ENABLE=1 — not starting; run: systemctl enable --now wt.service"
else
  log "enabling + starting wt.service"
  systemctl enable --now wt.service
  systemctl --no-pager --quiet is-active wt.service && printf '    active\n' || printf '    (check: systemctl status wt.service)\n'
fi

show_bind="$(sed -n 's/^[[:space:]]*WT_BIND=//p' "$CONF_DIR/config" 2>/dev/null | head -1)"
cat <<EOF

ttyd-ify installed.
  edit    $CONF_DIR/config     (WT_BIND / WT_PORT / shortcuts)
  status  systemctl status wt.service
  logs    journalctl -u wt.service -f

SECURITY: this is a writable, unauthenticated shell. It is only as private as the
interface it binds to (WT_BIND=${show_bind:-?}).
Keep it on a trusted network (tailnet/localhost). Never expose it to the public internet.
EOF
