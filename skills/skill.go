// Package skills embeds the agent skill so the binary can install it
// (`wormhole skill install`, and `wormhole join` on its way through) and
// so the docs and the CLI agree on its version.
package skills

import (
	_ "embed"
	"regexp"
)

// Wormhole is skills/wormhole/SKILL.md as shipped in this binary.
//
//go:embed wormhole/SKILL.md
var Wormhole []byte

var versionRE = regexp.MustCompile(`(?m)^version:\s*(\S+)\s*$`)

// Version is the version in the skill's frontmatter, or "" if absent.
func Version() string {
	m := versionRE.FindSubmatch(Wormhole)
	if m == nil {
		return ""
	}
	return string(m[1])
}
