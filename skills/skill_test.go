package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddedSkillIsTheFile keeps the embedded bytes and the file in step.
func TestEmbeddedSkillIsTheFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("wormhole", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(Wormhole) {
		t.Fatal("embedded skill differs from skills/wormhole/SKILL.md")
	}
	if Version() == "" {
		t.Fatal("embedded skill has no version")
	}
}

// TestVersionOf reads the frontmatter only.
func TestVersionOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"---\nname: x\nversion: 1.2.3\n---\nbody\n", "1.2.3"},
		{"---\nname: x\n---\nversion: 9.9.9\n", ""},
		{"version: 1.0.0\n", ""},
		{"", ""},
	} {
		if got := VersionOf([]byte(tc.in)); got != tc.want {
			t.Errorf("VersionOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
