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

# Why `gh attestation verify` cannot run here, or nothing at all if it can (#89).
#
# `command -v gh` is a necessary test for "can this box check provenance" and not a sufficient one.
# Two boxes pass it and still cannot: Ubuntu 24.04's universe gh is 2.45.0, which has no
# `attestation` subcommand at all, and a headless server's gh is typically not logged in, while
# reading an attestation is an authenticated API call. Both were hit back to back on a first install.
#
# Probing rather than reading gh's error afterwards, because a probe is version-independent: a future
# gh that renames the subcommand degrades to the documented can't-check path instead of producing an
# unrecognized error that gets read as a bad artifact. The grep further down stays as a backstop for
# whatever this does not anticipate.
#
# `auth status` accepts a token from the environment as well as a login, so GH_TOKEN=... is enough
# and nothing has to be written to disk -- which is the right shape for the machine this script
# exists for.
gh_attest_blocker() {
  if ! "$GH" attestation --help >/dev/null 2>&1; then
    printf "this gh has no 'attestation' command (Ubuntu 24.04 ships 2.45.0, which predates it)"
  elif ! "$GH" auth status >/dev/null 2>&1; then
    printf 'this gh is not authenticated, and reading an attestation is an API call'
  fi
}

# What to say when the check is unavailable. One wording for every cause, because the operator's
# situation is identical in all of them: the checksum passed, the source is unverified, and here is
# how to close that. Deliberately does NOT mention WT_FETCH_ALLOW_UNSIGNED -- that flag answers "no
# attestation exists for this tag", and offering it here would teach someone whose gh is merely old
# to switch off a check that would have worked.
no_provenance() {
  note "$1, so build provenance was NOT checked — only the checksum, which travels with the
      binary and proves the transfer rather than the source. The binary was still written.
      To check provenance:
        gh auth login            # or: export GH_TOKEN=<a token with no scopes; attestations are public>
        gh attestation verify ./wtd --repo $REPO"
}

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
elif have "$GH" && [ -n "$(gh_attest_blocker)" ]; then
  # The same class of thing as gh not being installed: the check is unavailable, for a reason that
  # says nothing about the artifact. It used to land in the branch below and tell the operator their
  # download looked tampered with, which is a lie in both cases -- no attestation was found, because
  # gh never got as far as looking.
  no_provenance "$(gh_attest_blocker)"
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
  elif printf '%s' "$verify_out" | grep -qiE 'unknown command|gh auth login|GH_TOKEN|authentication token|not logged'; then
    # Backstop for a cause the probe above did not anticipate -- gh's own words saying it could not
    # run, rather than that the artifact failed. Kept even though the probe should have caught these,
    # because the failure mode of guessing wrong here is accusing a good release of being tampered
    # with, and that is the one outcome worth two overlapping guards.
    no_provenance "gh could not run 'attestation verify' here"
  else
    die "build provenance verification FAILED for $asset.
       This is not a missing attestation — one was found and did not check out, which is
       what a tampered binary looks like. Refusing it. Nothing was written.

$(printf '%s' "$verify_out" | sed 's/^/       /')"
  fi
else
  no_provenance "gh is not installed"
fi

install -m 0755 "$tmp/$asset" ./wtd
printf '    verified, wrote ./wtd (%s)\n' "$(./wtd -version)"

# The last line of a successful verification, so it had better not name the command that undoes
# one. `make install` runs `make build` first whenever Go is present, and that writes `-o wtd` over
# the bytes verified above (#110). It fails silently in the worst way available: both binaries stamp
# their version from `git describe`, so on a clean checkout at the tag the rebuild reports the same
# string, and the operator sees the release they asked for with no signal that the checksum and the
# attestation stopped describing what is now on disk.
#
# install.sh directly, then. Not `sudo make install` either — that nests sudo, which resets
# SUDO_USER to root and gets refused rather than installing a root-owned web shell. install.sh needs
# no Go and resolves the service user from SUDO_USER, so this one command is right on a box with a
# toolchain and on a box without one. `WT_USER=<login>` goes in front of it if the service should
# run as somebody else: sudo env WT_USER=alice ./install.sh
printf '    now run: sudo ./install.sh\n'
printf '             NOT make install — with Go on this box that rebuilds ./wtd from source\n'
printf '             and discards the binary just verified.\n'
