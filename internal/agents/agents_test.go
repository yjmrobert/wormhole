package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeLookPath finds only the named binaries, at the given paths.
func fakeLookPath(found map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
}

// TestCopilotSkillDir covers the default and COPILOT_HOME.
func TestCopilotSkillDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COPILOT_HOME", "")
	a, ok := Lookup("copilot")
	if !ok {
		t.Fatal("copilot is not in the table")
	}
	if got, want := a.SkillPath(), filepath.Join(home, ".copilot", "skills", "wormhole", "SKILL.md"); got != want {
		t.Fatalf("SkillPath = %q, want %q", got, want)
	}
	t.Setenv("COPILOT_HOME", filepath.Join(home, "elsewhere"))
	if got, want := a.SkillDir(), filepath.Join(home, "elsewhere", "skills"); got != want {
		t.Fatalf("SkillDir with COPILOT_HOME = %q, want %q", got, want)
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("Lookup found an unknown agent")
	}
	if names := Names(); len(names) != len(Table) || names[0] != "copilot" {
		t.Fatalf("Names = %v", names)
	}
}

// TestDetect finds agents through lookPath and reads their version.
func TestDetect(t *testing.T) {
	if got := Detect(context.Background(), fakeLookPath(nil)); len(got) != 0 {
		t.Fatalf("Detect with nothing on PATH = %+v", got)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "copilot")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'GitHub Copilot CLI 1.0.105'\necho 'more'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Detect(context.Background(), fakeLookPath(map[string]string{"copilot": bin}))
	if len(got) != 1 || got[0].Name != "copilot" || got[0].Path != bin || got[0].Version != "GitHub Copilot CLI 1.0.105" {
		t.Fatalf("Detect = %+v", got)
	}
	got = Detect(context.Background(), fakeLookPath(map[string]string{"copilot": filepath.Join(dir, "missing")}))
	if len(got) != 1 || got[0].Version != "" {
		t.Fatalf("Detect with a broken binary = %+v", got)
	}
}

// TestInstallSkill writes, reports changes and leaves a current file alone.
func TestInstallSkill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("COPILOT_HOME", "")
	a, _ := Lookup("copilot")
	if inst, cur := SkillInstalled(a, []byte("v1")); inst || cur {
		t.Fatal("skill reported installed before install")
	}
	path, changed, err := InstallSkill(a, []byte("v1"))
	if err != nil || !changed || path != a.SkillPath() {
		t.Fatalf("install = %q %v %v", path, changed, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("skill file mode = %v, %v", info, err)
	}
	if dinfo, _ := os.Stat(filepath.Dir(path)); dinfo.Mode().Perm() != 0o755 {
		t.Fatalf("skill dir mode = %v", dinfo.Mode().Perm())
	}
	if _, changed, err := InstallSkill(a, []byte("v1")); err != nil || changed {
		t.Fatalf("second install changed = %v, %v", changed, err)
	}
	if inst, cur := SkillInstalled(a, []byte("v2")); !inst || cur {
		t.Fatalf("stale skill = %v %v", inst, cur)
	}
	if _, changed, err := InstallSkill(a, []byte("v2")); err != nil || !changed {
		t.Fatalf("upgrade changed = %v, %v", changed, err)
	}
	if inst, cur := SkillInstalled(a, []byte("v2")); !inst || !cur {
		t.Fatalf("upgraded skill = %v %v", inst, cur)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}
