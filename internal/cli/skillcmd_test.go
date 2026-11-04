package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yjmrobert/wormhole/skills"
)

// withAgents points the fixture's LookPath at a fake PATH holding names.
func withAgents(t *testing.T, f *cliFixture, names ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COPILOT_HOME", "")
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, n := range names {
		p := filepath.Join(bin, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+n+" 1.0.105'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		found[n] = p
	}
	f.app.LookPath = func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
	return home
}

// TestSkillInstall installs into the detected agent and reports current
// on the second run.
func TestSkillInstall(t *testing.T) {
	f := newCLIFixture(t)
	home := withAgents(t, f, "copilot")
	path := filepath.Join(home, ".copilot", "skills", "wormhole", "SKILL.md")
	if code := f.run("skill", "install"); code != ExitOK {
		t.Fatalf("install exited %d: %s", code, f.errOut.String())
	}
	if want := "installed skill " + skills.Version() + " for copilot at " + path + "\n"; f.out.String() != want {
		t.Fatalf("install output = %q, want %q", f.out.String(), want)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != string(skills.Wormhole) {
		t.Fatalf("skill file = %v", err)
	}
	f.out.Reset()
	if code := f.run("skill", "install", "--json", "--agent", "copilot"); code != ExitOK {
		t.Fatalf("second install exited %d", code)
	}
	var res struct {
		OK        bool           `json:"ok"`
		Installed []skillInstall `json:"installed"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil || !res.OK || len(res.Installed) != 1 || res.Installed[0].Changed || res.Installed[0].Path != path || res.Installed[0].Agent != "copilot" {
		t.Fatalf("second install = %s (%v)", f.out.String(), err)
	}

	f.out.Reset()
	if code := f.run("skill", "status", "--json"); code != ExitOK {
		t.Fatalf("status exited %d", code)
	}
	var st struct {
		OK     bool          `json:"ok"`
		Agents []skillStatus `json:"agents"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &st); err != nil || len(st.Agents) != 1 {
		t.Fatalf("status = %s (%v)", f.out.String(), err)
	}
	if a := st.Agents[0]; a.Name != "copilot" || a.Version != "copilot 1.0.105" || a.SkillPath != path || !a.Installed || !a.Current || !strings.HasSuffix(a.Path, "/bin/copilot") {
		t.Fatalf("status agent = %+v", a)
	}

	// A stale copy is reported as out of date.
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if code := f.run("skill", "status"); code != ExitOK || !strings.Contains(f.out.String(), "out of date") || !strings.Contains(f.out.String(), "wormhole skill install") {
		t.Fatalf("stale status = %d %q", code, f.out.String())
	}
}

// TestSkillInstallNothingDetected is exit 4 with the fix, unless --dir.
func TestSkillInstallNothingDetected(t *testing.T) {
	f := newCLIFixture(t)
	withAgents(t, f)
	if code := f.run("skill", "install", "--json"); code != ExitNotFound || !strings.Contains(f.out.String(), `install the Copilot CLI, or pass --dir \u003cskills directory\u003e`) {
		t.Fatalf("install with nothing detected = %d %s", code, f.out.String())
	}
	f.out.Reset()
	if code := f.run("skill", "install", "--agent", "copilot", "--json"); code != ExitNotFound {
		t.Fatalf("install --agent copilot, not on PATH = %d %s", code, f.out.String())
	}
	f.out.Reset()
	if code := f.run("skill", "install", "--agent", "nope", "--json"); code != ExitUsage || !strings.Contains(f.out.String(), "copilot") {
		t.Fatalf("install --agent nope = %d %s", code, f.out.String())
	}
	dir := filepath.Join(t.TempDir(), "skills")
	f.out.Reset()
	if code := f.run("skill", "install", "--dir", dir); code != ExitOK {
		t.Fatalf("install --dir = %d %s", code, f.errOut.String())
	}
	path := filepath.Join(dir, "wormhole", "SKILL.md")
	if !strings.Contains(f.out.String(), "installed skill "+skills.Version()+" at "+path) {
		t.Fatalf("install --dir output = %q", f.out.String())
	}
	f.out.Reset()
	if code := f.run("skill", "status", "--json"); code != ExitOK || !strings.Contains(f.out.String(), `"agents": []`) {
		t.Fatalf("status with nothing detected = %d %s", code, f.out.String())
	}
	f.out.Reset()
	if code := f.run("skill", "status"); code != ExitOK || !strings.Contains(f.out.String(), "no agent detected") {
		t.Fatalf("status text = %q", f.out.String())
	}
	if code := f.run("skill"); code != ExitUsage {
		t.Fatalf("bare skill = %d", code)
	}
}

// TestInstallSkillsHook is what join calls.
func TestInstallSkillsHook(t *testing.T) {
	f := newCLIFixture(t)
	withAgents(t, f, "copilot")
	var w strings.Builder
	res, err := f.app.installSkills(t.Context(), &common{}, &w)
	if err != nil || len(res) != 1 || !res[0].Changed || !strings.HasPrefix(w.String(), "installed skill") {
		t.Fatalf("installSkills = %+v %v %q", res, err, w.String())
	}
	if _, err := f.app.installSkills(t.Context(), &common{}, nil); err != nil {
		t.Fatal(err)
	}
}
