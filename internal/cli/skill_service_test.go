package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/systemd"
	"github.com/yjmrobert/wormhole/skills"
)

func TestSkillInstallAndStatus(t *testing.T) {
	f := newCLIFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COPILOT_HOME", "")
	f.app.LookPath = func(string) (string, error) { return "", errors.New("nope") }
	if code := f.run("skill", "install"); code != ExitNotFound || !strings.Contains(f.errOut.String(), "--dir") {
		t.Fatalf("no agent: %d %s", code, f.errOut.String())
	}
	if code := f.run("skill", "install", "--agent", "unknown"); code != ExitUsage {
		t.Fatalf("unknown agent: %d", code)
	}
	f.app.LookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	f.out.Reset()
	if code := f.run("skill", "status", "--json"); code != ExitOK {
		t.Fatalf("status: %d %s", code, f.errOut.String())
	}
	var res struct {
		Version string         `json:"version"`
		Agents  []skillInstall `json:"agents"`
	}
	_ = json.Unmarshal(f.out.Bytes(), &res)
	if res.Version != skills.Version() || len(res.Agents) != 1 || res.Agents[0].Installed || res.Agents[0].Agent != "copilot" {
		t.Fatalf("status = %+v", res)
	}
	f.out.Reset()
	if code := f.run("skill", "install"); code != ExitOK || !strings.Contains(f.out.String(), "installed skill "+skills.Version()+" for copilot") {
		t.Fatalf("install: %d %s %s", code, f.out.String(), f.errOut.String())
	}
	want := filepath.Join(home, ".copilot", "skills", "wormhole", "SKILL.md")
	b, err := os.ReadFile(want)
	if err != nil || string(b) != string(skills.Wormhole) {
		t.Fatalf("skill file: %v", err)
	}
	f.out.Reset()
	if code := f.run("skill", "install", "--json"); code != ExitOK {
		t.Fatalf("reinstall: %d", code)
	}
	_ = json.Unmarshal(f.out.Bytes(), &res)
	if !res.Agents[0].Current || res.Agents[0].Changed {
		t.Fatalf("reinstall = %+v", res.Agents)
	}
	dir := filepath.Join(home, "custom")
	if code := f.run("skill", "install", "--dir", dir); code != ExitOK {
		t.Fatalf("--dir: %d %s", code, f.errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "wormhole", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestServiceInstallStatusUninstall(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	runner := &systemd.FakeRunner{Output: map[string]string{"is-active": "active", "is-enabled": "enabled"}}
	f.app.Systemctl = runner
	f.app.Executable = func() (string, error) { return "/home/bob/.local/bin/wormhole", nil }
	if code := f.run("service", "install", "--json"); code != ExitOK {
		t.Fatalf("install: %d %s", code, f.errOut.String())
	}
	var res serviceResult
	_ = json.Unmarshal(f.out.Bytes(), &res)
	if !res.Installed || res.Unit != "wormhole" || res.Fallback != "" {
		t.Fatalf("result = %+v", res)
	}
	unit, err := os.ReadFile(res.Path)
	if err != nil || !strings.Contains(string(unit), "ExecStart=/home/bob/.local/bin/wormhole daemon --config "+f.cfgPath+"\n") {
		t.Fatalf("unit = %s, %v", unit, err)
	}
	f.out.Reset()
	if code := f.run("service", "status"); code != ExitOK || !strings.Contains(f.out.String(), "wormhole: active, enabled") {
		t.Fatalf("status: %d %s", code, f.out.String())
	}
	f.out.Reset()
	if code := f.run("service", "uninstall"); code != ExitOK {
		t.Fatalf("uninstall: %d %s", code, f.errOut.String())
	}
	if _, err := os.Stat(res.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unit still there")
	}

	// No user session: ok, with the fallback.
	f.app.Systemctl = &systemd.FakeRunner{Missing: true}
	f.out.Reset()
	if code := f.run("service", "install", "--json"); code != ExitOK {
		t.Fatalf("install without systemd: %d %s", code, f.errOut.String())
	}
	_ = json.Unmarshal(f.out.Bytes(), &res)
	if res.Installed || !strings.Contains(res.Fallback, "wormhole daemon") {
		t.Fatalf("fallback = %+v", res)
	}
}

// TestDemo is the CI smoke test: the whole narrated round trip, fast.
func TestDemo(t *testing.T) {
	f := newCLIFixture(t)
	start := time.Now()
	if code := f.run("demo", "--fast", "--json"); code != ExitOK {
		t.Fatalf("demo: %d %s %s", code, f.out.String(), f.errOut.String())
	}
	var res struct {
		OK    bool       `json:"ok"`
		Steps []demoStep `json:"steps"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Steps) < 10 {
		t.Fatalf("steps = %+v", res.Steps)
	}
	for _, s := range res.Steps {
		if !s.OK {
			t.Errorf("step %s failed: %s", s.Name, s.Detail)
		}
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("demo took %s", time.Since(start))
	}
	f.out.Reset()
	if code := f.run("demo", "--fast"); code != ExitOK || !strings.Contains(f.out.String(), "wormhole reply") || !strings.Contains(f.out.String(), "declined: taking it to standup") {
		t.Fatalf("narrated demo: %d\n%s\n%s", code, f.out.String(), f.errOut.String())
	}
}
