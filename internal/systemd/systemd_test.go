package systemd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testUnit() Unit {
	return Unit{
		Name: "demo", Description: "Demo daemon", Comment: "Written by a test.\nSecond line.",
		ExecStart: []string{"/opt/my tools/demo", "run", "--pct=50%", "$HOME"},
		After:     []string{"graphical-session.target", "network-online.target"},
		PartOf:    []string{"graphical-session.target"},
		WantedBy:  []string{"graphical-session.target"},
		Restart:   "on-failure", RestartSec: 5,
	}
}

// TestRender checks the layout and the ExecStart quoting.
func TestRender(t *testing.T) {
	want := `# Written by a test.
# Second line.

[Unit]
Description=Demo daemon
After=graphical-session.target network-online.target
PartOf=graphical-session.target

[Service]
ExecStart="/opt/my tools/demo" run --pct=50%% $$HOME
Restart=on-failure
RestartSec=5

[Install]
WantedBy=graphical-session.target
`
	if got := testUnit().Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
	for in, want := range map[string]string{
		"plain":     "plain",
		"":          `""`,
		`a"b`:       `"a\"b"`,
		`a\b`:       `"a\\b"`,
		"it's":      `"it's"`,
		"tab\there": `"tab\there"`,
	} {
		if got := QuoteArg(in); got != want {
			t.Errorf("QuoteArg(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestUserUnitPath honors XDG_CONFIG_HOME.
func TestUserUnitPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := UserUnitPath("x"), filepath.Join(home, ".config", "systemd", "user", "x.service"); got != want {
		t.Fatalf("UserUnitPath = %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	if got, want := UserUnitPath("x"), filepath.Join(home, "cfg", "systemd", "user", "x.service"); got != want {
		t.Fatalf("UserUnitPath = %q, want %q", got, want)
	}
}

// TestInstallUser writes the unit and runs systemctl in order.
func TestInstallUser(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	r := &FakeRunner{Responses: map[string]FakeResponse{
		"systemctl --user import-environment": {Output: "no such variable", Err: errors.New("exit status 1")},
	}}
	res, err := InstallUser(context.Background(), r, testUnit(), []string{"DISPLAY", "WAYLAND_DISPLAY"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user import-environment DISPLAY WAYLAND_DISPLAY",
		"systemctl --user enable --now demo.service",
	}
	if got := r.Snapshot(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v", got)
	}
	if !res.Changed || res.ImportError != "no such variable" || res.Fallback != "" || len(res.Steps) != 3 || res.Steps[1].OK {
		t.Fatalf("result = %+v", res)
	}
	b, err := os.ReadFile(res.UnitPath)
	if err != nil || string(b) != testUnit().Render() {
		t.Fatalf("unit file = %q, %v", b, err)
	}
	if info, _ := os.Stat(res.UnitPath); info.Mode().Perm() != 0o644 {
		t.Fatalf("unit mode = %v", info.Mode().Perm())
	}

	// Unchanged: no rewrite, no restart.
	r2 := &FakeRunner{}
	res, err = InstallUser(context.Background(), r2, testUnit(), nil)
	if err != nil || res.Changed || len(r2.Snapshot()) != 2 {
		t.Fatalf("reinstall = %+v %v calls %v", res, err, r2.Snapshot())
	}
	// Changed while installed: try-restart picks up the new unit.
	u := testUnit()
	u.RestartSec = 9
	r3 := &FakeRunner{}
	if res, err = InstallUser(context.Background(), r3, u, nil); err != nil || !res.Changed {
		t.Fatalf("upgrade = %+v %v", res, err)
	}
	if calls := r3.Snapshot(); calls[len(calls)-1] != "systemctl --user try-restart demo.service" {
		t.Fatalf("upgrade calls = %v", calls)
	}

	// enable failing is an error, not a fallback.
	r4 := &FakeRunner{Responses: map[string]FakeResponse{"systemctl --user enable": {Output: "Unit file is masked.", Err: errors.New("exit status 1")}}}
	if _, err := InstallUser(context.Background(), r4, testUnit(), nil); err == nil || errors.Is(err, ErrNoUserSession) || !strings.Contains(err.Error(), "masked") {
		t.Fatalf("enable failure = %v", err)
	}
}

// TestInstallUserNoSession falls back and leaves no new file behind.
func TestInstallUserNoSession(t *testing.T) {
	for name, resp := range map[string]FakeResponse{
		"no bus":       {Output: "Failed to connect to bus: No medium found", Err: errors.New("exit status 1")},
		"no systemctl": {Err: &exec.Error{Name: "systemctl", Err: exec.ErrNotFound}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			r := &FakeRunner{Responses: map[string]FakeResponse{"systemctl": resp}}
			res, err := InstallUser(context.Background(), r, testUnit(), []string{"DISPLAY"})
			if !errors.Is(err, ErrNoUserSession) {
				t.Fatalf("err = %v", err)
			}
			if res.Fallback == "" || res.Changed || len(r.Snapshot()) != 1 {
				t.Fatalf("result = %+v calls %v", res, r.Snapshot())
			}
			if _, err := os.Stat(res.UnitPath); !os.IsNotExist(err) {
				t.Fatalf("unit file left behind: %v", err)
			}
			if _, err := StatusOf(context.Background(), r, "demo"); !errors.Is(err, ErrNoUserSession) {
				t.Fatalf("status err = %v", err)
			}
		})
	}
}

// TestStatusAndUninstall reads is-active/is-enabled and removes the unit.
func TestStatusAndUninstall(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := InstallUser(context.Background(), &FakeRunner{}, testUnit(), nil); err != nil {
		t.Fatal(err)
	}
	r := &FakeRunner{Responses: map[string]FakeResponse{
		"systemctl --user is-active":  {Output: "inactive\n", Err: errors.New("exit status 3")},
		"systemctl --user is-enabled": {Output: "enabled\n"},
	}}
	st, err := StatusOf(context.Background(), r, "demo")
	if err != nil || !st.Installed || st.Active != "inactive" || st.Enabled != "enabled" || st.Running() {
		t.Fatalf("status = %+v %v", st, err)
	}
	r2 := &FakeRunner{}
	res, err := Uninstall(context.Background(), r2, "demo")
	if err != nil || !res.Removed {
		t.Fatalf("uninstall = %+v %v", res, err)
	}
	if got := strings.Join(r2.Snapshot(), "|"); got != "systemctl --user disable --now demo.service|systemctl --user daemon-reload" {
		t.Fatalf("uninstall calls = %s", got)
	}
	if res, err := Uninstall(context.Background(), &FakeRunner{}, "demo"); err != nil || res.Removed {
		t.Fatalf("second uninstall = %+v %v", res, err)
	}
}
