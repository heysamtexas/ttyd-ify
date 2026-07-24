#!/usr/bin/env bash
# Stub start command for wire-protocol conformance tests.
#
# Stands in for bin/wt so a test can observe the protocol alone: argv delivery (does
# ?arg= arrive as $1?), output relay, input relay, and whether a resize frame actually
# reaches the pty. Deliberately involves no dtach and no sockets — the real picker would
# drag ~/.dtach and live sessions into a test that is only about frames on a wire.
#
# Protocol with the test: prints its argv on startup, then for each line of input either
# reports the terminal size (line == "size") or echoes it back with a prefix.
set -u

printf 'ARGV:[%s]\n' "$*"
printf 'TERM:[%s]\n' "${TERM:-}"
printf 'INITSIZE:%s\n' "$(stty size 2>/dev/null | tr ' ' 'x')"

while IFS= read -r line; do
  case "$line" in
    size) printf 'SIZE:%s\n' "$(stty size 2>/dev/null | tr ' ' 'x')" ;;
    quit) printf 'BYE\n'; exit 0 ;;
    *)    printf 'ECHO:%s\n' "$line" ;;
  esac
done
