#!/usr/bin/env sh
# Grep the tree for deployment-specific terms that must never be committed.
# Terms come from a private, gitignored file: one per line, case-insensitive
# fixed strings (your org name, bus hostname, internal repo names, colleague
# handles, ticket prefixes). Wire it as a pre-commit hook:
#   ln -s ../../scripts/leakcheck.sh .git/hooks/pre-commit
set -eu
cd "$(dirname "$0")/.."
terms="${WORMHOLE_FORBIDDEN_TERMS:-.wormhole-forbidden-terms}"
if [ ! -f "$terms" ]; then
  echo "leakcheck: no $terms file; nothing to check (create one with your private terms)" >&2
  exit 0
fi
# Only committed and staged files, never the terms file itself.
files=$(git ls-files; git diff --cached --name-only --diff-filter=ACMR)
status=0
for f in $(printf '%s\n' "$files" | sort -u); do
  [ -f "$f" ] || continue
  [ "$f" = "$terms" ] && continue
  if grep -n -i -F -f "$terms" "$f" >/dev/null 2>&1; then
    echo "leakcheck: forbidden term in $f:" >&2
    grep -n -i -F -f "$terms" "$f" | sed 's/^/  /' >&2
    status=1
  fi
done
exit $status
