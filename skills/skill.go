// Package skills embeds the agent skills Wormhole ships, so `wormhole skill
// install` and `wormhole join` write them from the binary instead of from a
// checkout of this repository.
package skills

import (
	_ "embed"
	"regexp"
)

// Wormhole is skills/wormhole/SKILL.md as built into this binary.
//
//go:embed wormhole/SKILL.md
var Wormhole []byte

var (
	versionLine   = regexp.MustCompile(`(?m)^version:\s*(\S+)\s*$`)
	frontmatterRE = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n`)
)

// Version returns the version: line of the embedded skill's frontmatter, or
// "" when it has none.
func Version() string {
	return VersionOf(Wormhole)
}

// VersionOf parses the version: line of a skill file's frontmatter.
func VersionOf(content []byte) string {
	fm := frontmatter(content)
	m := versionLine.FindSubmatch(fm)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// frontmatter returns the block between the leading --- lines, or nil.
func frontmatter(content []byte) []byte {
	m := frontmatterRE.FindSubmatch(content)
	if m == nil {
		return nil
	}
	return m[1]
}
