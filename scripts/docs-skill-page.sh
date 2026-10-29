#!/usr/bin/env sh
# Emit docs/skill.md from skills/wormhole/SKILL.md without its YAML
# frontmatter, so the docs site shows the same text the agent reads.
set -eu
cd "$(dirname "$0")/.."
echo "# The wormhole skill"
echo
echo "This is \`skills/wormhole/SKILL.md\` from the repository, also shipped in every release tarball."
echo
awk 'f>=2 {print} /^---$/ {f++}' skills/wormhole/SKILL.md | sed 's/^# Wormhole$/## Overview/'
