#!/usr/bin/env bash
# fetch.sh — download a released wtd binary for this machine, verify it, and write ./wtd.
#
# This is the only way onto a box with no Go toolchain, which is the box it is written for: curl,
# coreutils, and not much else. `make fetch` calls it; that stays the documented command.
#
#   ./fetch.sh                  # the latest release
#   TAG=v0.2.0 ./fetch.sh       # a specific one, which is how you roll back without a compiler
#
# It lived inside a Makefile recipe until #86. Moving it out is not tidying: as a `\`-continued blob
# inside a target, shellcheck never saw it and none of its refusals could be exercised without real
# network round trips — so the checksum gate, the one integrity check between a download and a
# root-owned binary serving an unauthenticated shell, had never executed anywhere (#84).
#
# TAG, not VERSION: the Makefile already has a VERSION, which is the string stamped into a build.
# Two meanings for one name on the same command line is how someone stamps a binary v0.2.0 by
# accident.
#
# Test hook: WT_FETCH_ORIGIN replaces https://github.com, so test/fetch.sh can serve fixtures from
# localhost and drive the refusals offline. It is not a supported knob for installing — pointing it
# somewhere else means fetching a shell server from somewhere else.
set -euo pipefail

REPO="${WT_REPO:-heysamtexas/ttyd-ify}"
ORIGIN="${WT_FETCH_ORIGIN:-https://github.com}"
TAG="${TAG:-}"
# Which gh to verify with. A seam, because "no gh on this box" is a branch that decides whether a
# security check runs, and absence cannot be stubbed onto PATH — test/fetch.sh points this at a path
# that does not exist. It also covers a real case: gh installed somewhere root's PATH misses.
GH="${WT_FETCH_GH:-gh}"

log()  { printf '\033[01;36m==>\033[00m %s\n' "$*"; }
note() { printf '\033[01;33mnote:\033[00m %s\n' "$*"; }
die()  { printf '\033[01;31merror:\033[00m %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

case "$(uname -m)" in
  x86_64)        arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "no release build for $(uname -m). Build from source instead (needs Go):
       make build" ;;
esac

if [ -n "$TAG" ]; then
  tag="$TAG"
  log "using the release you asked for: $tag ($arch)"
else
  log "resolving the latest release"
  # GitHub redirects /releases/latest to /releases/tag/<tag>, so the effective URL carries the name.
  # -I: headers only, since the page body is not wanted.
  url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$ORIGIN/$REPO/releases/latest")" \
    || die "cannot reach $ORIGIN/$REPO/releases/latest — is this box online?"
  tag="${url##*/}"
  case "$tag" in
    v*) ;;
    *) die "no published release yet for $REPO (resolved '$tag').
       Build from source instead (needs Go):  make build" ;;
  esac
  log "$tag ($arch)"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
base="$ORIGIN/$REPO/releases/download/$tag"
asset="wtd-linux-$arch"

curl -fsSL -o "$tmp/$asset" "$base/$asset" \
  || die "cannot download $asset from $tag.
       Check the tag exists and has that asset:  $ORIGIN/$REPO/releases"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" \
  || die "$tag has no SHA256SUMS, so the download cannot be verified. Refusing it."

log "verifying the checksum"
# Both forms, because sha256sum writes ` name` for text mode and ` *name` for binary.
want="$(awk -v f="$asset" '$2 == f || $2 == "*"f {print $1}' "$tmp/SHA256SUMS")"
[ -n "$want" ] || die "$asset is not listed in $tag's SHA256SUMS. Refusing a binary
       nothing vouches for."
got="$(sha256sum "$tmp/$asset" | cut -d' ' -f1)"
[ "$want" = "$got" ] || die "CHECKSUM MISMATCH — refusing it.
       want $want
       got  $got
       Either the download is damaged, or the file is not the one this release published."

# Provenance (#85). The checksum above proves the transfer, and only that: SHA256SUMS comes from the
# same URL as the binary, so whatever could replace one could replace the other. The attestation is
# signed by GitHub's workflow identity, and says this artifact was built by this repo's release
# workflow at a specific commit.
#
# Verified only when `gh` is here, and that is a deliberate hole rather than an oversight: this
# script exists for the machine that has nothing, and making the GitHub CLI a hard requirement for
# installing would strand exactly that machine. So the skip is loud — a silent one is how a check
# becomes decoration.
if [ "${WT_FETCH_ALLOW_UNSIGNED:-0}" = 1 ]; then
  note "WT_FETCH_ALLOW_UNSIGNED=1 — build provenance was NOT checked. That is the right answer
      only for a tag published before #85, which has none to check."
elif have "$GH"; then
  log "verifying build provenance"
  if verify_out="$("$GH" attestation verify "$tmp/$asset" --repo "$REPO" 2>&1)"; then
    printf '    provenance ok: built by %s\n' "$REPO"
  elif printf '%s' "$verify_out" | grep -qiE 'no attestation|HTTP 404'; then
    # Nothing is published for this artifact's digest. `HTTP 404` is what real gh says — measured
    # against v0.3.0, which predates #85 — and the wording matters because the alternative branch
    # accuses the download of being tampered with, which would be a lie about a release that simply
    # came first.
    #
    # Fatal either way, and the message says why: a *substituted* binary produces this same 404,
    # since its digest was never attested. Those two cannot be told apart from here, so it refuses
    # and names both instead of guessing.
    die "no build provenance is published for this $tag binary.
       Two things look identical from here: a release from before #85, which has no
       attestation to find, and a substituted binary, whose digest was never attested.
       If you know this tag predates provenance, re-run with:
         WT_FETCH_ALLOW_UNSIGNED=1 TAG=$tag ./fetch.sh
       Otherwise fetch a newer tag, or build from source:  make build"
  else
    die "build provenance verification FAILED for $asset.
       This is not a missing attestation — one was found and did not check out, which is
       what a tampered binary looks like. Refusing it. Nothing was written.

$(printf '%s' "$verify_out" | sed 's/^/       /')"
  fi
else
  note "gh is not installed, so build provenance was NOT checked — only the checksum, which
      travels with the binary and proves the transfer rather than the source. To check it:
        gh attestation verify ./wtd --repo $REPO"
fi

install -m 0755 "$tmp/$asset" ./wtd
printf '    verified, wrote ./wtd (%s)\n' "$(./wtd -version)"
printf '    now run: make install\n'
