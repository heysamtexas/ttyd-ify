# shellcheck shell=bash
# Optional: if your login shell auto-launches tmux/screen/a session picker, guard it
# so it does NOT fire inside a ttyd-ify web session (wt exports WT=1). Add to ~/.bashrc:
#
#   if [[ $- == *i* && -z "${TMUX:-}" && -z "${WT:-}" ]]; then
#       tmux attach || tmux    # ...or whatever you auto-launch
#   fi
#
# The `-z "${WT:-}"` clause is the important part: without it, every shell spawned
# inside wt would recurse into your multiplexer. ttyd-ify does not edit your shell
# config — copy this yourself if you need it.
