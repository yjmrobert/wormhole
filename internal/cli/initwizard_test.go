package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yjmrobert/wormhole/internal/config"
)

// initApp is a non-interactive App for init tests with $USER set.
func initApp(t *testing.T, user string) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	f := newCLIFixture(t)
	f.app.Getenv = func(k string) string {
		if k == "USER" {
			return user
		}
		return ""
	}
	return f.app, f.out, f.errOut
}

// TestInitNoInputWritesLoadableConfig answers every question with flags.
func TestInitNoInputWritesLoadableConfig(t *testing.T) {
	app, out, errOut := initApp(t, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "wh", "config.yaml")
	code := app.Run(context.Background(), []string{"init", "--no-input", "--config", path,
		"--handle", "alice", "--url", "tls://bus.example.internal:4222,tls://bus2.example.internal:4222",
		"--senders", "bob,carol", "--senders", "dave", "--notify", "digest"})
	if code != ExitOK {
		t.Fatalf("init = %d %s %s", code, out.String(), errOut.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Handle != "alice" || cfg.Bus.URL != "tls://bus.example.internal:4222,tls://bus2.example.internal:4222" || cfg.Daemon.Notify != config.NotifyDigest {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Bus.PasswordFile != filepath.Join(dir, "wh", "password") || cfg.Bus.Creds != "" {
		t.Fatalf("credential = %q / %q", cfg.Bus.PasswordFile, cfg.Bus.Creds)
	}
	var got []string
	for _, s := range cfg.Senders {
		if s.Mode != config.ModeInbox {
			t.Fatalf("sender %+v is not inbox mode", s)
		}
		got = append(got, s.Handle)
	}
	if strings.Join(got, ",") != "bob,carol,dave" {
		t.Fatalf("senders = %v", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	for _, keep := range []string{"# Who may open a wormhole to you.", "  # - {handle: carol, mode: auto}", "\n\nlimits:\n"} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("config lost %q", keep)
		}
	}
	text := out.String()
	for _, want := range []string{"wrote " + path, "next:", "wormhole login", "wormhole service install", `wormhole ask bob "..."`} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "edit this file") {
		t.Fatal("init told the person to edit the file")
	}

	// It exists now: refused without --force, replaced with it.
	out.Reset()
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--config", path, "--json"}); code != ExitRefused || !strings.Contains(out.String(), "--force") {
		t.Fatalf("second init = %d %s", code, out.String())
	}
	creds := filepath.Join(t.TempDir(), "alice.creds")
	if err := os.WriteFile(creds, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--force", "--config", path, "--handle", "alice", "--creds", creds, "--json"}); code != ExitOK {
		t.Fatalf("init --creds = %d %s", code, out.String())
	}
	var res initResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Auth != authCreds || len(res.Senders) != 0 || res.Next[0] != "wormhole skill install" {
		t.Fatalf("init --creds result = %s (%v)", out.String(), err)
	}
	cfg, err = config.Load(path)
	if err != nil || cfg.Bus.Creds != creds || cfg.Bus.PasswordFile != "" || len(cfg.Senders) != 0 || cfg.Bus.URL != defaultBusURL || cfg.Daemon.Notify != config.NotifyToast {
		t.Fatalf("creds config = %+v, %v", cfg, err)
	}
}

// TestInitDefaultsAndSkips uses $USER and skips a handle it cannot guess.
func TestInitDefaultsAndSkips(t *testing.T) {
	app, out, _ := initApp(t, "bob")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if code := app.Run(context.Background(), []string{"init", "--config", path}); code != ExitOK {
		t.Fatalf("init = %d", code)
	}
	if cfg, err := config.Load(path); err != nil || cfg.Handle != "bob" {
		t.Fatalf("default handle: %+v %v", cfg, err)
	}
	if !strings.Contains(out.String(), "nobody may ask you yet") {
		t.Fatalf("output = %q", out.String())
	}

	app, out, _ = initApp(t, "Not A Handle")
	path = filepath.Join(t.TempDir(), "config.yaml")
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--config", path, "--json"}); code != ExitOK {
		t.Fatalf("init without a handle = %d %s", code, out.String())
	}
	var res initResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || strings.Join(res.Skipped, ",") != "handle" {
		t.Fatalf("result = %s", out.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--force", "--config", path}); code != ExitOK || !strings.Contains(out.String(), "wormhole doctor will name it") {
		t.Fatalf("skip text = %q", out.String())
	}
}

// TestInitUsageErrors rejects bad flags before writing anything.
func TestInitUsageErrors(t *testing.T) {
	app, out, _ := initApp(t, "alice")
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, args := range [][]string{
		{"--handle", "Bad Handle"},
		{"--senders", "ok,Not/Valid"},
		{"--senders", "alice"},
		{"--notify", "loud"},
		{"--password-file", "/a", "--creds", "/b"},
		{"--invite", "tok", "--url", "tls://x:4222"},
	} {
		out.Reset()
		if code := app.Run(context.Background(), append([]string{"init", "--no-input", "--json", "--config", path}, args...)); code != ExitUsage {
			t.Errorf("init %v = %d %s", args, code, out.String())
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a usage error wrote the config")
	}
	// Inside a git working tree: refused.
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--config", filepath.Join(repo, "config.yaml")}); code != ExitRefused {
		t.Fatalf("init in a git tree = %d", code)
	}
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--config", path, "--password-file", filepath.Join(repo, "pw")}); code != ExitRefused {
		t.Fatalf("password file in a git tree = %d", code)
	}
}

// TestInitInviteCallsJoin hands an invite to join's implementation.
func TestInitInviteCallsJoin(t *testing.T) {
	app, out, _ := initApp(t, "alice")
	path := filepath.Join(t.TempDir(), "config.yaml")
	code := app.Run(context.Background(), []string{"init", "--no-input", "--json", "--config", path, "--invite", "wh1-token"})
	if code != ExitError || !strings.Contains(out.String(), "wormhole join is not wired yet") {
		t.Fatalf("init --invite = %d %s", code, out.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("init --invite wrote a config itself")
	}
}

// TestRenderConfig covers the pure YAML edit.
func TestRenderConfig(t *testing.T) {
	raw, err := renderConfig(initAnswers{Handle: "alice", URL: "nats://10.0.0.5:4222", Auth: authPass, PasswordFile: "/secrets/pw: odd", Senders: []string{"bob"}, Notify: "off"}, "/home/alice/.config/wormhole")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, raw)
	}
	if cfg.Bus.URL != "nats://10.0.0.5:4222" || cfg.Bus.PasswordFile != "/secrets/pw: odd" || cfg.Daemon.Notify != config.NotifyOff || len(cfg.Senders) != 1 {
		t.Fatalf("cfg = %+v", cfg)
	}
	// Everything but the answered lines is the example, byte for byte.
	want := strings.Split(strings.ReplaceAll(config.Example, "~/.config/wormhole", "/home/alice/.config/wormhole"), "\n")
	got := strings.Split(string(raw), "\n")
	if len(got) != len(want) {
		t.Fatalf("line count %d != %d", len(got), len(want))
	}
	changed := 0
	for i := range got {
		if got[i] != want[i] {
			changed++
		}
	}
	if changed != 3 { // url, password_file, notify; handle and senders match the example
		t.Fatalf("%d lines changed:\n%s", changed, raw)
	}
	if s := yamlScalar("plain"); s != "plain" {
		t.Fatalf("yamlScalar = %q", s)
	}
	if h := defaultHandle(func(string) string { return "Alice" }); h != "alice" {
		t.Fatalf("defaultHandle = %q", h)
	}
	if s, err := parseSenders(" ^bob, carol bob ", "alice"); err != nil || strings.Join(s, ",") != "bob,carol" {
		t.Fatalf("parseSenders = %v %v", s, err)
	}
}

// TestInitForm builds the wizard without a terminal: every group, the
// invite branch hiding the URL questions.
func TestInitForm(t *testing.T) {
	ans := initAnswers{Handle: "alice", Reach: reachURL, Auth: authPass, URL: defaultBusURL, Notify: "toast"}
	text := "bob"
	form := initForm(&ans, &text, strings.NewReader(""), &bytes.Buffer{})
	if form == nil {
		t.Fatal("no form")
	}
	ans.Reach = reachInvite
	if form = initForm(&ans, &text, strings.NewReader(""), &bytes.Buffer{}); form == nil {
		t.Fatal("no form for the invite branch")
	}
	if (&App{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}}).interactive(&common{}) {
		t.Fatal("buffers counted as a terminal")
	}
}
