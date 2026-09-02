#!/usr/bin/env bash
# Exercises bin/wt-prompt-hook.
#
# Hermetic: writes only into its own temp directory via WT_PROMPT_DIR, touches no session, needs
# no container and no network. That is why it belongs in `make lint` alongside test/fetch.sh
# rather than in the smoke suite.
#
# The property most of this file is about is that **the hook always exits 0**. A Claude Code
# UserPromptSubmit hook that exits non-zero can stop the prompt being submitted at all, so a bug
# in here must never be able to get between someone and their agent. Every case below therefore
# asserts the exit status as well as the effect, and the failure cases assert that nothing was
# damaged on the way.
set -uo pipefail

export HOOK
HOOK="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/wt-prompt-hook"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export WT_PROMPT_DIR="$TMP/prompts"

fails=0
ok()  { printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fails=$((fails + 1)); }
head() { printf '\n\033[1m%s\033[0m\n' "$1"; }
# check <actual> <expected> <message>. A helper rather than `[ x = y ] && ok || bad`, which is not
# if-then-else: `bad` running because `ok` failed would report a passing check as a failure.
check() {
  if [ "$1" = "$2" ]; then ok "$3"; else bad "$3 (got '$1', want '$2')"; fi
}

# run <session> <stdin> -> sets RC
run() {
  local session="$1" payload="$2"
  if [ -z "$session" ]; then
    printf '%s' "$payload" | env -u WT_SESSION "$HOOK"
  else
    printf '%s' "$payload" | WT_SESSION="$session" "$HOOK"
  fi
  RC=$?
}

count() { # count <session>
  python3 -c "
import json,sys
try:
    print(len(json.load(open('$WT_PROMPT_DIR/$1.json'))['prompts']))
except Exception:
    print(-1)"
}

head "it records a prompt"
run ops '{"hook_event_name":"UserPromptSubmit","prompt":"first thing"}'
check "$RC" 0 "exit 0"
check "$(count ops)" 1 "one prompt recorded"
run ops '{"prompt":"second thing"}'
check "$(count ops)" 2 "appends rather than replacing"

# Order is the order they were said in, because a reader looking for "what did I last say" needs
# to know which end of the list to read.
if python3 -c "
import json,sys
p=json.load(open('$WT_PROMPT_DIR/ops.json'))['prompts']
sys.exit(0 if p[0]['text']=='first thing' and p[-1]['text']=='second thing' else 1)"; then
  ok "oldest first, newest last"
else
  bad "prompts are not in the order they were said"
fi

head "the file is private"
mode="$(stat -c '%a' "$WT_PROMPT_DIR/ops.json")"
check "$mode" 600 "the prompt file is 0600"
dmode="$(stat -c '%a' "$WT_PROMPT_DIR")"
check "$dmode" 700 "the directory is 0700"

head "not a web session: nothing recorded, nothing created"
rm -rf "$TMP/none"; WT_PROMPT_DIR="$TMP/none" run "" '{"prompt":"from an ssh shell"}'
check "$RC" 0 "exit 0 with WT_SESSION unset"
if [ -e "$TMP/none" ]; then
  bad "created $TMP/none outside a web session"
else
  ok "created nothing outside a web session"
fi

head "every failure path exits 0 and damages nothing"
before="$(count ops)"
while IFS= read -r payload; do
  run ops "$payload"
  check "$RC" 0 "exit 0 for payload ${payload:0:24}"
done <<'PAYLOADS'
not json at all
[]
null
{}
{"prompt":""}
{"prompt":"   "}
{"prompt":123}
{"prompt":null}
PAYLOADS
check "$(count ops)" "$before" "no malformed payload was recorded"

head "a name that could escape the directory is refused"
for name in '../escape' 'a/b' '..'; do
  run "$name" '{"prompt":"traversal"}'
  check "$RC" 0 "exit 0 for name $name"
done
if compgen -G "$TMP/escape*" >/dev/null || [ -e "$TMP/prompts/../escape.json" ]; then
  bad "a file was written outside the prompts directory"
else
  ok "nothing was written outside the prompts directory"
fi
check "$(count ops)" "$before" "the good file was untouched by the refused names"

head "bounds"
for i in $(seq 1 60); do run bounded "{\"prompt\":\"prompt $i\"}"; done
kept="$(count bounded)"
if [ "$kept" -le 50 ] && [ "$kept" -gt 0 ]; then
  ok "keeps $kept prompts, capped at 50"
else
  bad "kept $kept prompts, want between 1 and 50"
fi
if python3 -c "
import json,sys
p=json.load(open('$WT_PROMPT_DIR/bounded.json'))['prompts']
sys.exit(0 if p[-1]['text']=='prompt 60' else 1)"; then
  ok "the cap drops the oldest, not the newest"
else
  bad "the cap dropped the newest prompt"
fi

long="$(python3 -c 'print("x"*5000)')"
run long "$(python3 -c "import json;print(json.dumps({'prompt':'$long'}))")"
if python3 -c "
import json,sys
p=json.load(open('$WT_PROMPT_DIR/long.json'))['prompts'][0]
sys.exit(0 if len(p['text'])==2000 and p['truncated'] else 1)"; then
  ok "a very long prompt is shortened and marked truncated"
else
  bad "a very long prompt was not shortened or not marked"
fi

head "a corrupt file is discarded, not merged"
printf 'garbage{' > "$WT_PROMPT_DIR/corrupt.json"
run corrupt '{"prompt":"after corruption"}'
check "$(count corrupt)" 1 "recovered with just the new prompt"

head "text survives a round trip exactly"
if python3 - <<'PY'
import json, os, subprocess, sys
tricky = 'café "quoted" \\ backslash 你好\nsecond line\ttab'
env = dict(os.environ, WT_SESSION="utf")
subprocess.run([os.environ["HOOK"]], input=json.dumps({"prompt": tricky}), text=True, env=env)
got = json.load(open(os.environ["WT_PROMPT_DIR"] + "/utf.json"))["prompts"][0]["text"]
sys.exit(0 if got == tricky else 1)
PY
then
  ok "quotes, backslashes, newlines and non-ASCII are preserved"
else
  bad "text was mangled in the round trip"
fi

head "an unwritable directory is survivable"
WT_PROMPT_DIR=/proc/definitely/not/writable run ops '{"prompt":"nowhere to go"}'
check "$RC" 0 "exit 0 when the directory cannot be created"

if [ "$fails" -gt 0 ]; then
  printf '\n\033[31m%d check(s) failed\033[0m\n' "$fails"
  exit 1
fi
printf '\nprompt-hook: records prompts, and cannot get between a human and their agent\n'
