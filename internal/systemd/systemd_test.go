package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderMatchesShippedUnit(t *testing.T) {
	u := Unit{Name: "wormhole", Description: "Wormhole daemon", ExecStart: []string{"/home/alice/.local/bin/wormhole", "daemon"},
		After: []string{"graphical-session.target", "network-online.target"}, PartOf: []string{"graphical-session.target"},
		WantedBy: []string{"graphical-session.target"}, Restart: "on-failure", RestartSec: 5}
	got := u.Render()
	for _, line := range []string{"[Unit]", "Description=Wormhole daemon", "After=graphical-session.target network-online.target", "PartOf=graphical-session.target", "[Service]", "ExecStart=/home/alice/.local/bin/wormhole daemon", "Restart=on-failure", "RestartSec=5", "[Install]", "WantedBy=graphical-session.target"} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
	// Every directive the shipped template sets is set here too.
	shipped, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", "wormhole.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(shipped), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		if !strings.Contains(got, line+"\n") {
			t.Errorf("shipped unit line %q is not generated", line)
		}
	}
	q := Unit{Name: "x", ExecStart: []string{"/opt/my tools/wormhole", "serve", "--listen", ":4222"}}.Render()
	if !strings.Contains(q, `ExecStart="/opt/my tools/wormhole" serve --listen :4222`) {
		t.Fatalf("quoting:\n%s", q)
	}
}

func TestInstallUserAndFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	r := &FakeRunner{}
	u := Unit{Name: "wormhole", ExecStart: []string{"/bin/wormhole", "daemon"}}
	res, err := InstallUser(context.Background(), r, u, []string{"DISPLAY", "WAYLAND_DISPLAY"}, "wormhole daemon")
	if err != nil || !res.Installed || !res.ImportedEnv || res.Fallback != "" {
		t.Fatalf("install = %+v, %v", res, err)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "import-environment", "DISPLAY", "WAYLAND_DISPLAY"}, {"systemctl", "--user", "enable", "--now", "wormhole.service"}}
	if len(r.Calls) != 3 {
		t.Fatalf("calls = %v", r.Calls)
	}
	for i := range want {
		if strings.Join(r.Calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v", i, r.Calls[i])
		}
	}
	st := Query(context.Background(), &FakeRunner{Output: map[string]string{"is-active": "active", "is-enabled": "enabled"}}, "wormhole")
	if !st.Exists || st.Active != "active" || st.Enabled != "enabled" {
		t.Fatalf("status = %+v", st)
	}

	// No user session: the file is written, the fallback is reported.
	r = &FakeRunner{Fail: map[string]error{"daemon-reload": errors.New("exit 1")}, Output: map[string]string{"daemon-reload": "Failed to connect to bus: No medium found"}}
	res, err = InstallUser(context.Background(), r, u, nil, "wormhole daemon")
	if !errors.Is(err, ErrNoUserSession) || res.Installed || res.Fallback != "wormhole daemon" {
		t.Fatalf("no session = %+v, %v", res, err)
	}
	r = &FakeRunner{Missing: true}
	if _, err := InstallUser(context.Background(), r, u, nil, "x"); !errors.Is(err, ErrNoUserSession) {
		t.Fatalf("missing systemctl = %v", err)
	}
	// Some other failure is reported as is.
	r = &FakeRunner{Fail: map[string]error{"enable": errors.New("exit 1")}, Output: map[string]string{"enable": "Unit is masked"}}
	if _, err := InstallUser(context.Background(), r, u, nil, "x"); errors.Is(err, ErrNoUserSession) || !strings.Contains(err.Error(), "masked") {
		t.Fatalf("other failure = %v", err)
	}
	p, err := Uninstall(context.Background(), &FakeRunner{}, "wormhole")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unit file not removed")
	}
}
