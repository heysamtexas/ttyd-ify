# shellcheck shell=bash
# wt-bind.sh — resolve a WT_BIND setting to one concrete IP address. Sourced, never run.
#
# The only implementation, sourced by bin/wt-serve.
#
# Never yields a wildcard, and that is the point. The security model of this project is
# "a writable, unauthenticated shell, protected only by the interface it is bound to", so
# resolving to 0.0.0.0 would silently turn a private shell into a public one. wtd also
# refuses a wildcard -listen of its own accord, as a backstop against a launcher bug.

# resolve_ip <WT_BIND value>
#
#   tailscale  -> this node's tailnet address
#   localhost  -> 127.0.0.1
#   <iface>    -> that interface's first IPv4 address
#   <anything> -> treated as a literal address
#
# Prints nothing when it cannot resolve; callers must treat empty output as fatal rather
# than falling back to a default, or a misconfigured host could end up bound somewhere
# unintended.
resolve_ip() {
  local bind="${1:?resolve_ip: need a WT_BIND value}"

  case "$bind" in
    tailscale) tailscale ip -4 2>/dev/null | head -1 ;;
    localhost) printf '127.0.0.1' ;;
    *)
      if ip link show dev "$bind" >/dev/null 2>&1; then
        ip -4 -o addr show dev "$bind" | awk '{print $4}' | cut -d/ -f1 | head -1
      else
        printf '%s' "$bind"
      fi
      ;;
  esac
}
