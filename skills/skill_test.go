package skills

import (
	"os"
	"strings"
	"testing"
)

func TestEmbeddedSkillMatchesFile(t *testing.T) {
	b, err := os.ReadFile("wormhole/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(Wormhole) {
		t.Fatal("embedded skill differs from the file")
	}
	if v := Version(); v == "" || !strings.Contains(string(Wormhole), "version: "+v) {
		t.Fatalf("version = %q", v)
	}
}
