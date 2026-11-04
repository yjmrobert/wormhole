package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectAndInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COPILOT_HOME", "")
	none := func(string) (string, error) { return "", errors.New("not found") }
	if got := Detect(context.Background(), none); len(got) != 0 {
		t.Fatalf("detected %v", got)
	}
	found := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	// Replace the version probe so the test does not run a binary.
	saved := Table
	Table = []Agent{{Name: "copilot", Binary: "copilot", SkillDir: Copilot.SkillDir, Version: func(context.Context, string) string { return "1.0.105" }}}
	defer func() { Table = saved }()
	got := Detect(context.Background(), found)
	if len(got) != 1 || got[0].Name != "copilot" || got[0].Path != "/usr/bin/copilot" || got[0].Version != "1.0.105" {
		t.Fatalf("detected = %+v", got)
	}
	want := filepath.Join(home, ".copilot", "skills", "wormhole", "SKILL.md")
	if got[0].SkillPath() != want {
		t.Fatalf("skill path = %s", got[0].SkillPath())
	}
	if installed, _ := SkillInstalled(want, []byte("x")); installed {
		t.Fatal("installed before install")
	}
	changed, err := InstallSkill(want, []byte("v1"))
	if err != nil || !changed {
		t.Fatalf("install = %v, %v", changed, err)
	}
	if changed, err := InstallSkill(want, []byte("v1")); err != nil || changed {
		t.Fatalf("reinstall = %v, %v", changed, err)
	}
	if installed, current := SkillInstalled(want, []byte("v2")); !installed || current {
		t.Fatalf("installed=%v current=%v", installed, current)
	}
	if changed, _ := InstallSkill(want, []byte("v2")); !changed {
		t.Fatal("update not applied")
	}
	b, _ := os.ReadFile(want)
	if string(b) != "v2" {
		t.Fatalf("content = %q", b)
	}
	t.Setenv("COPILOT_HOME", filepath.Join(home, "elsewhere"))
	if Copilot.SkillPath() != filepath.Join(home, "elsewhere", "skills", "wormhole", "SKILL.md") {
		t.Fatalf("COPILOT_HOME ignored: %s", Copilot.SkillPath())
	}
	if _, ok := Lookup("copilot"); !ok {
		t.Fatal("lookup")
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("lookup unknown")
	}
}
