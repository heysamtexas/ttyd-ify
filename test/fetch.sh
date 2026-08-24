#!/usr/bin/env bash
# Exercise fetch.sh's refusals — the checksum gate and the provenance check — offline.
#
# `make fetch` is the only way onto a box with no Go toolchain, and its checksum gate is the one
# integrity check between a network download and a root-owned binary serving an unauthenticated
# shell. It had never executed anywhere (#84): the code lived inside a Makefile target, invisible to
# the linter, and nothing could drive its branches without publishing a broken release to do it.
# #86 moved it to fetch.sh; this is what that move was for.
#
# Everything here runs against test/fake-release.py on localhost, so there is no network dependency
# and a fixture can be as broken as the assertion needs. Safe to run anywhere — unlike the other two
# test scripts, this one writes only inside its own temp directory.
#
#   test/fetch.sh
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

WORK="$(mktemp -d)"
PORT=7690
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

fail=0
ok()   { printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=1; }
head() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# fetch <description> — run fetch.sh against the fixture server, into a scratch checkout so ./wtd
# from the real repo is never touched. Output goes to $WORK/out for the assertions to read.
run_fetch() {
  ( cd "$WORK/checkout" \
    && WT_FETCH_ORIGIN="http://127.0.0.1:$PORT" WT_REPO=fake/repo PATH="$WORK/bin:$PATH" \
       "$@" bash "$OLDPWD/fetch.sh" ) > "$WORK/out" 2>&1
}
refuses_with() { # <description> <expected substring>
  if run_fetch "${@:3}"; then
    bad "$1 (it accepted the download)"
    sed 's/^/      /' "$WORK/out"
  else
    if grep -qiF "$2" "$WORK/out"; then ok "$1"; else
      bad "$1 — refused, but not with its own message"
      sed 's/^/      /' "$WORK/out"
    fi
    # A refusal must leave nothing behind: the whole point is that a binary nothing vouches for
    # never reaches the disk this install will run from. An `if`, not `[ … ] && bad`, because that
    # idiom returns nonzero when the file is absent — as the last line of a function, under
    # `set -e`, that kills the run at its first passing assertion.
    if [ -e "$WORK/checkout/wtd" ]; then bad "$1 — refused but wrote ./wtd anyway"; fi
  fi
}

# The "binary" is a script that answers -version, because fetch.sh runs it to report what it got and
# nothing here needs a real server.
mkdir -p "$WORK/checkout" "$WORK/bin" "$WORK/releases/v9.9.9"
# shellcheck disable=SC2016  # $1 belongs to the stub being written, not to this script
printf '#!/bin/sh\n[ "$1" = -version ] && echo v9.9.9\nexit 0\n' > "$WORK/releases/v9.9.9/wtd-linux-amd64"
cp "$WORK/releases/v9.9.9/wtd-linux-amd64" "$WORK/releases/v9.9.9/wtd-linux-arm64"
( cd "$WORK/releases/v9.9.9" && sha256sum wtd-linux-amd64 wtd-linux-arm64 > SHA256SUMS )
# A second tag, so TAG= has something to pick that is not the latest.
mkdir -p "$WORK/releases/v0.0.1"
# shellcheck disable=SC2016  # as above
printf '#!/bin/sh\n[ "$1" = -version ] && echo v0.0.1\nexit 0\n' > "$WORK/releases/v0.0.1/wtd-linux-amd64"
cp "$WORK/releases/v0.0.1/wtd-linux-amd64" "$WORK/releases/v0.0.1/wtd-linux-arm64"
( cd "$WORK/releases/v0.0.1" && sha256sum wtd-linux-amd64 wtd-linux-arm64 > SHA256SUMS )

python3 test/fake-release.py "$PORT" "$WORK/releases" v9.9.9 &
SERVER_PID=$!
for _ in $(seq 40); do
  curl -fsS -o /dev/null "http://127.0.0.1:$PORT/fake/repo/releases/latest" 2>/dev/null && break
  sleep 0.25
done

# gh present and happy. Stubbed rather than skipped: without it every run below would take the
# "gh is not installed" path and the verification branches would go untested, which is the same
# unexercised-gate problem this file exists to fix.
cat > "$WORK/bin/gh" <<'STUB'
#!/bin/sh
# `gh attestation verify <file> --repo <repo>`
exit 0
STUB
chmod +x "$WORK/bin/gh"

head "the happy path"
if run_fetch; then
  ok "fetched the latest release"
  # if/else throughout rather than `cmd && ok || bad`: that idiom also runs `bad` when the command
  # succeeds and `ok` fails, which is SC2015 and the reason the sibling scripts define pass_if.
  if grep -q 'v9.9.9' "$WORK/out"; then ok "resolved the tag through the redirect"; else bad "did not report the resolved tag"; fi
  if [ -x "$WORK/checkout/wtd" ]; then ok "wrote an executable ./wtd"; else bad "no ./wtd"; fi
  if grep -q 'provenance ok' "$WORK/out"; then ok "verified build provenance"; else bad "did not verify provenance"; fi
  # #110: the instruction printed at the end of a verification must not be the one that discards
  # it. `make install` rebuilds over ./wtd whenever Go is present, and stamps the same version, so
  # the operator gets a locally-built binary while believing they deployed an attested one. Both
  # directions asserted, because naming install.sh while still mentioning make install would read
  # as offering a choice between them.
  if grep -q 'now run: sudo \./install\.sh' "$WORK/out"; then
    ok "points at install.sh, which deploys the bytes it verified"
  else
    bad "does not name sudo ./install.sh as the next step (#110)"
  fi
  if grep -qE 'now run.*make install|^ *make install' "$WORK/out"; then
    bad "still tells you to run make install, which rebuilds over the verified binary (#110)"
  else
    ok "does not send you to make install"
  fi
else
  bad "the happy path failed"; sed 's/^/      /' "$WORK/out"
fi

head "TAG picks a release, which is how you roll back without a compiler"
rm -f "$WORK/checkout/wtd"
if run_fetch env TAG=v0.0.1; then
  got="$("$WORK/checkout/wtd" -version)"
  if [ "$got" = v0.0.1 ]; then ok "TAG=v0.0.1 fetched v0.0.1, not the latest"; else bad "TAG ignored (got '$got')"; fi
else
  bad "TAG= was refused"; sed 's/^/      /' "$WORK/out"
fi
rm -f "$WORK/checkout/wtd"
refuses_with "an unknown tag says which one it could not find" "cannot download" env TAG=v0.0.0-nope

head "the checksum gate (#84 — none of this had ever run)"
rm -f "$WORK/checkout/wtd"
# Tampered: the asset changes, SHA256SUMS does not. This is the branch that stands between a
# substituted download and a binary that gets installed as root.
cp "$WORK/releases/v9.9.9/wtd-linux-amd64" "$WORK/asset.bak"
printf '#!/bin/sh\necho pwned\n' > "$WORK/releases/v9.9.9/wtd-linux-amd64"
refuses_with "a binary that does not match SHA256SUMS is refused" "CHECKSUM MISMATCH"
cp "$WORK/asset.bak" "$WORK/releases/v9.9.9/wtd-linux-amd64"

# Absent from the manifest. The awk yields nothing, and "nothing to compare against" must not read
# as "nothing wrong".
cp "$WORK/releases/v9.9.9/SHA256SUMS" "$WORK/sums.bak"
grep -v 'wtd-linux-amd64' "$WORK/sums.bak" > "$WORK/releases/v9.9.9/SHA256SUMS"
refuses_with "an asset missing from SHA256SUMS is refused" "not listed"
cp "$WORK/sums.bak" "$WORK/releases/v9.9.9/SHA256SUMS"

# sha256sum writes ` *name` in binary mode. The awk accepts both spellings, and a release built that
# way must not read as an unlisted asset.
sed 's/  wtd-linux/ *wtd-linux/' "$WORK/sums.bak" > "$WORK/releases/v9.9.9/SHA256SUMS"
rm -f "$WORK/checkout/wtd"
if run_fetch; then ok "the ' *name' binary-mode spelling is accepted too"; else
  bad "rejected a valid SHA256SUMS written in binary mode"; sed 's/^/      /' "$WORK/out"
fi
cp "$WORK/sums.bak" "$WORK/releases/v9.9.9/SHA256SUMS"

# A gh that can run the check, and fails the verify call with the given message.
#
# Every case needs `attestation --help` and `auth status` to succeed, because fetch.sh probes both
# before it will attempt a verification (#89) — and a real gh that rejects a binary does support
# both. The earlier stubs exited 1 for every invocation, including the probe, which is not a gh that
# exists: it modelled the verify call and nothing else. Writing them through one helper keeps the
# next case from reintroducing that.
stub_gh() { # <verify stderr>
  cat > "$WORK/bin/gh" <<STUB
#!/bin/sh
case "\$1" in
  attestation) [ "\$2" = --help ] && exit 0
               echo "$1" >&2; exit 1 ;;
  auth)        exit 0 ;;
esac
exit 0
STUB
  chmod +x "$WORK/bin/gh"
}

head "build provenance (#85)"
rm -f "$WORK/checkout/wtd"
# Found and invalid. This is the one that must never be advisory: an attestation that fails to
# verify is what a tampered artifact looks like.
stub_gh "signature verification failed"
refuses_with "a failed provenance check is fatal" "verification FAILED"

# Nothing published for this digest — every release before #85. Also fatal, because a substituted
# binary produces the same answer, and the message has to name both rather than guess.
stub_gh "no attestations found for subject"
refuses_with "a release with no attestation is refused, and named as such" "no build provenance is published"

# The wording real gh actually uses, measured against v0.3.0: a bare HTTP 404 from the attestations
# API, with the phrase "no attestation" nowhere in it. Matching only the friendly wording sent this
# down the tampering branch, which accused a release of being substituted for the crime of predating
# the feature. This stub is that exact output.
stub_gh "Error: HTTP 404: Not Found (https://api.github.com/repos/fake/repo/attestations/sha256:abc?per_page=30)"
refuses_with "gh's real 404 reads as 'none published', not as tampering" "no build provenance is published"

# ...with one documented way through, for the tags that genuinely predate it.
rm -f "$WORK/checkout/wtd"
if run_fetch env WT_FETCH_ALLOW_UNSIGNED=1; then
  ok "WT_FETCH_ALLOW_UNSIGNED=1 is the way past an unsigned tag"
  if grep -q 'NOT checked' "$WORK/out"; then ok "and it says so loudly"; else bad "skipped the check quietly"; fi
else
  bad "WT_FETCH_ALLOW_UNSIGNED=1 did not work"; sed 's/^/      /' "$WORK/out"
fi

# No gh at all: the fresh box this script exists for. Proceeds — making the GitHub CLI a hard
# requirement would strand exactly that machine — but must say what was skipped.
#
# Via the WT_FETCH_GH seam, not by deleting the stub: this developer box has a real gh further down
# PATH, which then ran against the fixture repo and 404'd. Absence is the branch under test, and
# absence cannot be stubbed onto PATH.
rm -f "$WORK/checkout/wtd"
if run_fetch env WT_FETCH_GH=/nonexistent/gh; then
  ok "a box without gh still gets a binary"
  if grep -q 'provenance was NOT checked' "$WORK/out"; then
    ok "and is told the check did not happen"
  else
    bad "skipped provenance silently"
  fi
else
  bad "refused to install on a box without gh"; sed 's/^/      /' "$WORK/out"
fi

# #89: gh present but unable to run the check. Both of these used to land in the tampered-binary
# branch and tell the operator their download looked substituted — a lie, since no attestation was
# found because gh never got as far as looking. Both were hit back to back on a first install.
#
# The assertion that matters is not just "it proceeded": it is that the scary wording stays out of
# it, and that WT_FETCH_ALLOW_UNSIGNED is not suggested. An agent told its binary looks tampered
# with goes looking for a way past the refusal, and that flag is documented two paragraphs away in
# the README — which turns a cosmetic bug into a skipped security check.
proceeds_without_provenance() { # <description>
  if run_fetch; then
    if ! grep -q 'provenance was NOT checked' "$WORK/out"; then
      bad "$1 — proceeded but did not say the check was skipped"
      sed 's/^/      /' "$WORK/out"
    elif grep -qi 'tampered' "$WORK/out"; then
      bad "$1 — still accuses the binary of being tampered with (#89)"
      sed 's/^/      /' "$WORK/out"
    elif grep -q 'WT_FETCH_ALLOW_UNSIGNED' "$WORK/out"; then
      bad "$1 — offered the skip-provenance flag for an environmental problem (#89)"
      sed 's/^/      /' "$WORK/out"
    else
      ok "$1"
    fi
  else
    bad "$1 — refused, but this is the same situation as having no gh at all"
    sed 's/^/      /' "$WORK/out"
  fi
}

head "gh that cannot run the check is not a tampered binary (#89)"
rm -f "$WORK/checkout/wtd"
# Ubuntu 24.04's universe gh is 2.45.0, which has no `attestation` subcommand. Real gh answers an
# unknown subcommand with a usage dump, which is what buried the actual cause off-screen.
cat > "$WORK/bin/gh" <<'STUB'
#!/bin/sh
if [ "$1" = attestation ]; then
  echo 'unknown command "attestation" for "gh"' >&2
  echo "Usage:  gh <command> <subcommand> [flags]" >&2
  exit 1
fi
exit 0
STUB
chmod +x "$WORK/bin/gh"
proceeds_without_provenance "a gh too old for 'attestation' is not treated as tampering"
if grep -q "no 'attestation' command" "$WORK/out"; then
  ok "and names the real cause"
else
  bad "did not say why the check could not run"
fi

# gh new enough, never logged in. Reading an attestation is an authenticated API call, so this is
# the normal state of a headless server nobody has run `gh auth login` on.
cat > "$WORK/bin/gh" <<'STUB'
#!/bin/sh
case "$1" in
  attestation) [ "$2" = --help ] && exit 0
               echo "To get started with GitHub CLI, please run:  gh auth login" >&2; exit 1 ;;
  auth)        echo "You are not logged into any GitHub hosts." >&2; exit 1 ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/gh"
proceeds_without_provenance "an unauthenticated gh is not treated as tampering"
if grep -q 'not authenticated' "$WORK/out"; then
  ok "and names the real cause"
else
  bad "did not say why the check could not run"
fi

# The backstop: probe passes, the call itself still fails on auth. Nothing anticipates every cause,
# and guessing wrong here means accusing a good release, so gh's own words are matched too.
stub_gh "error: GH_TOKEN is not a valid authentication token"
proceeds_without_provenance "an auth failure the probe missed is caught by the backstop"

# And the one that must stay fatal: verification ran and the artifact did not check out.
# Cleared first because the three cases above each wrote ./wtd, correctly -- and refuses_with also
# asserts that a refusal leaves nothing behind, which a leftover from a passing case would trip.
rm -f "$WORK/checkout/wtd"
stub_gh "signature verification failed"
refuses_with "a real verification failure is still fatal after all this" "verification FAILED"

head "an architecture with no release build"
rm -f "$WORK/checkout/wtd"
# uname stubbed, because the alternative is owning one of every machine.
# shellcheck disable=SC2016  # $1/$@ belong to the stub being written
printf '#!/bin/sh\n[ "$1" = -m ] && echo mips64 || exec /usr/bin/uname "$@"\n' > "$WORK/bin/uname"
chmod +x "$WORK/bin/uname"
refuses_with "an unsupported arch is refused, naming the machine and the way out" "no release build for mips64"
rm -f "$WORK/bin/uname"

echo
if [ "$fail" = 0 ]; then echo "fetch: every refusal fired, and the happy path still works"; else echo "FAILURES above"; exit 1; fi
