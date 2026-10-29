package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The skill's frontmatter version and the CLI's default --skill-version
// must move together, so a daemon can tell which skill release produced a
// message.
func TestSkillVersionMatchesSkillFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "skills", "wormhole", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^version:\s*(\S+)\s*$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("SKILL.md has no version: line in its frontmatter")
	}
	if got := string(m[1]); got != SkillVersion {
		t.Fatalf("SKILL.md version %q != cli.SkillVersion %q; bump both together", got, SkillVersion)
	}
	// The skill tells agents which version to pass; keep that in step too.
	if !regexp.MustCompile(`--skill-version ` + regexp.QuoteMeta(SkillVersion)).Match(b) {
		t.Fatalf("SKILL.md does not pass --skill-version %s in its ask command", SkillVersion)
	}
}
