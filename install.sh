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
WEB_UNIT=/etc/systemd/system/wt-web.service

have() { command -v "$1" >/dev/null 2>&1; }
log()  { printf '\033[01;36m==>\033[00m %s\n' "$*"; }
skip() { printf '    skip %s (%s)\n' "$1" "$2"; }
die()  { printf '\033[01;31merror:\033[00m %s\n' "$*" >&2; exit 1; }

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
for b in wt wt-serve wt-web-serve; do
  if [ -x "$PREFIX/$b" ] && [ "$FORCE" != 1 ]; then
    skip "$b" "present (FORCE=1 to overwrite)"
  else
    install -m 0755 "$SCRIPT_DIR/bin/$b" "$PREFIX/$b"
    printf '    installed %s\n' "$PREFIX/$b"
  fi
done

# wt-bind.sh is sourced, not executed, so it is installed non-executable — and it must
# land beside the launchers, which is how wt-web-serve finds it in both a checkout and an
# install. A missing helper makes wt-web-serve refuse to start rather than lose bind
# resolution silently, so this is not optional.
install -m 0644 "$SCRIPT_DIR/bin/wt-bind.sh" "$PREFIX/wt-bind.sh"
printf '    installed %s (sourced helper)\n' "$PREFIX/wt-bind.sh"

# 2b. the wtd binary (Go). Deliberately NOT built here: install.sh runs as root, and
# building as root writes root-owned artifacts into the invoking user's checkout and Go
# cache. Build unprivileged (`make build`) or drop a release binary in place, then install.
log "wtd binary -> $PREFIX/wtd"
if [ -x "$PREFIX/wtd" ] && [ "$FORCE" != 1 ]; then
  skip "wtd" "present (FORCE=1 to overwrite)"
elif [ -f "$SCRIPT_DIR/wtd" ]; then
  install -m 0755 "$SCRIPT_DIR/wtd" "$PREFIX/wtd"
  printf '    installed %s (%s)\n' "$PREFIX/wtd" "$("$PREFIX/wtd" -version 2>/dev/null || echo 'version unknown')"
elif have go; then
  printf '    no ./wtd found, but Go is installed — build it first (not as root):\n'
  printf '      make build   # or: go build -o wtd ./cmd/wtd\n'
  printf '    then re-run the install. Skipping wtd for now.\n'
else
  printf '    no ./wtd and no Go toolchain — download a release binary for this\n'
  printf '    architecture (%s), verify its checksum, save it as ./wtd, and re-run:\n' "$(uname -m)"
  printf '      https://github.com/heysamtexas/ttyd-ify/releases\n'
  printf '    Skipping wtd for now; the ttyd path (wt.service) works without it.\n'
fi

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

# 4. systemd units (render User=)
log "systemd units"
sed "s|__WT_USER__|$WT_USER|" "$SCRIPT_DIR/systemd/wt.service" > "$UNIT"
printf '    %s (User=%s)\n' "$UNIT" "$WT_USER"

# wt-web.service is written but NOT enabled. Enabling it here would open a second
# listening port on every existing install during an upgrade — a security-relevant change
# nobody asked for. wtd is opt-in until it has been trusted on a given box; the migration
# design is both running side by side, then retiring ttyd.
sed "s|__WT_USER__|$WT_USER|" "$SCRIPT_DIR/systemd/wt-web.service" > "$WEB_UNIT"
printf '    %s (User=%s, not enabled — see below)\n' "$WEB_UNIT" "$WT_USER"

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

wtd (the Go server that will replace ttyd) is installed but NOT enabled. It serves the
same terminal protocol plus a JSON session API and a browser picker, on WT_WEB_PORT so it
can run beside ttyd while you try it:
  sudo systemctl enable --now wt-web.service
  journalctl -u wt-web.service -f
Enabling it opens a SECOND port with the same access model as the first — read the warning
below before you do.

SECURITY: this is a writable, unauthenticated shell. It is only as private as the
interface it binds to (WT_BIND=${show_bind:-?}).
Keep it on a trusted network (tailnet/localhost). Never expose it to the public internet.
EOF
