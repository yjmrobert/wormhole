#!/usr/bin/env sh
# M0 test 3: does the app show a turn appended with --resume on a listed session?
set -eu
. "$(dirname "$0")/common.sh"
need copilot; need xdg-open; need uuidgen
id=$(uuidgen)
mkdir -p "$WH_WORKDIR"; cd "$WH_WORKDIR"
note "create session $id"
copilot -p "Reply with the single word: one" --session-id "$id" -n "m0 resume test"
note "open it in the app (watch the sidebar)"
xdg-open "ghapp://sessions/$id" || echo "CHECK: xdg-open failed; is DISPLAY set?"
printf 'Press enter once the session is visible and open in the app... '; read -r _
note "append a turn with --resume"
copilot -p "Reply with the single word: two" --resume="$id"
echo "QUESTION: did 'two' appear in the open session without reopening it? (yes / after reopen / never)"
echo "QUESTION: was there an inuse lock file for the session while the app had it open? (ls the app's session store)"
