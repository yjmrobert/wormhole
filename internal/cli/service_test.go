package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yjmrobert/wormhole/internal/systemd"
)

// unitKeys parses a unit file into "Section.Key" -> value, ignoring
// comments and blank lines.
func unitKeys(t *testing.T, content string) map[string]string {
	t.Helper()
	out := map[string]string{}
	section := ""
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "["):
			section = strings.Trim(line, "[]")
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				t.Fatalf("bad unit line %q", line)
			}
			out[section+"."+k] = v
		}
	}
	return out
}

// TestDaemonUnitMatchesShippedUnit compares the generated unit with
// deploy/systemd/wormhole.service key by key; only ExecStart's binary path
// differs, because the generated one names the running executable.
func TestDaemonUnitMatchesShippedUnit(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", "wormhole.service"))
	if err != nil {
		t.Fatal(err)
	}
	f := newCLIFixture(t)
	u, err := f.app.daemonUnit(&common{})
	if err != nil {
		t.Fatal(err)
	}
	want, got := unitKeys(t, string(shipped)), unitKeys(t, u.Render())
	wantExec, gotExec := strings.Fields(want["Service.ExecStart"]), u.ExecStart
	delete(want, "Service.ExecStart")
	delete(got, "Service.ExecStart")
	if len(want) != len(got) {
		t.Fatalf("keys differ:\nshipped   %v\ngenerated %v", want, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, shipped unit has %q", k, got[k], v)
		}
	}
	if exe, _ := os.Executable(); !filepath.IsAbs(gotExec[0]) || filepath.Base(gotExec[0]) != filepath.Base(exe) {
		t.Errorf("ExecStart binary = %q, want the running executable", gotExec[0])
	}
	if strings.Join(gotExec[1:], " ") != strings.Join(wantExec[1:], " ") {
		t.Errorf("ExecStart args = %v, shipped %v", gotExec[1:], wantExec[1:])
	}

	// An explicit --config is passed on to the daemon.
	u, _ = f.app.daemonUnit(&common{config: "rel/config.yaml"})
	if n := len(u.ExecStart); n != 4 || u.ExecStart[2] != "--config" || !filepath.IsAbs(u.ExecStart[3]) {
		t.Errorf("ExecStart with --config = %v", u.ExecStart)
	}
}

// TestServiceInstall runs install, status and uninstall against the fake.
func TestServiceInstall(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newCLIFixture(t)
	fake := &systemd.FakeRunner{Responses: map[string]systemd.FakeResponse{
		"systemctl --user is-active":  {Output: "active\n"},
		"systemctl --user is-enabled": {Output: "enabled\n"},
	}}
	f.app.Systemctl = fake
	if code := f.run("service", "install", "--json"); code != ExitOK {
		t.Fatalf("install = %d %s", code, f.out.String())
	}
	var res serviceResult
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Installed || !res.Changed || !res.Active || res.Fallback != "" || res.Unit != "wormhole.service" || strings.Join(res.Imported, " ") != "DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS" {
		t.Fatalf("install = %+v", res)
	}
	calls := strings.Join(fake.Snapshot(), "\n")
	for _, want := range []string{
		"systemctl --user daemon-reload",
		"systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS",
		"systemctl --user enable --now wormhole.service",
	} {
		if !strings.Contains(calls, want) {
			t.Fatalf("calls lack %q:\n%s", want, calls)
		}
	}
	if b, err := os.ReadFile(res.UnitPath); err != nil || !strings.Contains(string(b), "WantedBy=graphical-session.target") {
		t.Fatalf("unit file: %v", err)
	}

	f.out.Reset()
	if code := f.run("service", "install"); code != ExitOK || !strings.Contains(f.out.String(), "is unchanged") || !strings.Contains(f.out.String(), "it is running") {
		t.Fatalf("reinstall = %d %q", code, f.out.String())
	}
	f.out.Reset()
	if code := f.run("service", "status"); code != ExitOK || !strings.Contains(f.out.String(), "wormhole.service: active, enabled") {
		t.Fatalf("status = %d %q", code, f.out.String())
	}
	f.out.Reset()
	if code := f.run("service", "uninstall", "--json"); code != ExitOK || !strings.Contains(f.out.String(), `"removed": true`) {
		t.Fatalf("uninstall = %d %q", code, f.out.String())
	}
	if _, err := os.Stat(res.UnitPath); !os.IsNotExist(err) {
		t.Fatalf("unit file after uninstall: %v", err)
	}
	f.out.Reset()
	if code := f.run("service", "status"); code != ExitOK || !strings.Contains(f.out.String(), "run: wormhole service install") {
		t.Fatalf("status after uninstall = %d %q", code, f.out.String())
	}
	if code := f.run("service"); code != ExitUsage {
		t.Fatalf("bare service = %d", code)
	}

	// enable failing is an error naming the fix.
	f.app.Systemctl = &systemd.FakeRunner{Responses: map[string]systemd.FakeResponse{"systemctl --user enable": {Output: "Unit is masked.", Err: errors.New("exit status 1")}}}
	f.out.Reset()
	if code := f.run("service", "install", "--json"); code != ExitError || !strings.Contains(f.out.String(), "systemctl --user status wormhole.service") {
		t.Fatalf("masked install = %d %q", code, f.out.String())
	}
}

// TestServiceNoUserSession falls back to a terminal daemon with exit 0.
func TestServiceNoUserSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newCLIFixture(t)
	f.app.Systemctl = &systemd.FakeRunner{Responses: map[string]systemd.FakeResponse{
		"systemctl": {Output: "Failed to connect to bus: No medium found", Err: errors.New("exit status 1")},
	}}
	if code := f.run("service", "install", "--json"); code != ExitOK {
		t.Fatalf("install = %d %s", code, f.out.String())
	}
	var res serviceResult
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Fallback != serviceFallback || !res.OK {
		t.Fatalf("install = %+v", res)
	}
	f.out.Reset()
	if code := f.run("service", "install"); code != ExitOK || f.out.String() != "no systemd user session; run the daemon in a terminal: wormhole daemon\n" {
		t.Fatalf("install text = %d %q", code, f.out.String())
	}
	f.out.Reset()
	if code := f.run("service", "status", "--json"); code != ExitOK || !strings.Contains(f.out.String(), `"fallback"`) {
		t.Fatalf("status = %d %q", code, f.out.String())
	}
	f.out.Reset()
	if code := f.run("service", "uninstall"); code != ExitOK || !strings.Contains(f.out.String(), "was not installed") {
		t.Fatalf("uninstall = %d %q", code, f.out.String())
	}
	var w strings.Builder
	if r, err := f.app.installDaemonService(t.Context(), &common{}, &w); err != nil || r.Installed || !strings.Contains(w.String(), "wormhole daemon") {
		t.Fatalf("installDaemonService = %+v %v", r, err)
	}
}
