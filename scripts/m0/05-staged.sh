#!/usr/bin/env sh
# M0 test 5: can a session be created with the kickoff staged but not run?
# Tries the obvious candidates; none is known to work. Note which, if any,
# leaves a listed session whose first turn has not executed.
set -eu
. "$(dirname "$0")/common.sh"
need copilot; need uuidgen
mkdir -p "$WH_WORKDIR"; cd "$WH_WORKDIR"
note "copilot help (look for anything like --no-run, --draft, --stage, --dry-run)"
copilot --help 2>&1 | grep -iE 'run|draft|stage|dry|interactive|session' || true
for flag in "--no-run" "--dry-run" "--draft"; do
  id=$(uuidgen)
  note "try: copilot -p <kickoff> --session-id $id $flag"
  if copilot -p "STAGED: if you can read this, the prompt ran." --session-id "$id" -n "m0 staged $flag" "$flag" 2>&1 | tail -3; then
    echo "exit 0 with $flag: open ghapp://sessions/$id and check whether the prompt executed"
  else
    echo "flag $flag not accepted"
  fi
done
note "alternative: create an empty session, then let a person paste the kickoff"
id=$(uuidgen)
if copilot --session-id "$id" -n "m0 empty session" -p "" 2>&1 | tail -3; then
  echo "empty prompt accepted; check whether session $id is listed"
fi
echo "RECORD: the exact command that creates a listed, unexecuted session, or 'none'."
