package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExampleParses(t *testing.T) {
	cfg, err := Parse([]byte(Example))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Handle != "alice" || cfg.Bus.Kind != "nats" || cfg.Bus.User != "alice" {
		t.Fatalf("%+v", cfg)
	}
	if mode, ok := cfg.SenderMode("bob"); !ok || mode != ModeInbox {
		t.Fatalf("bob mode %q %v", mode, ok)
	}
	if _, ok := cfg.SenderMode("mallory"); ok {
		t.Fatal("unknown sender allowed")
	}
	if cfg.Limits.PerSenderPerDay != 20 || cfg.Limits.PerDay != 50 || cfg.Limits.MaxHops != 6 {
		t.Fatalf("%+v", cfg.Limits)
	}
	if !*cfg.Daemon.OpenApp || cfg.Daemon.Notify != NotifyToast || cfg.Daemon.DigestEvery != 20*time.Minute || cfg.Daemon.SnoozeDefault != 2*time.Hour || len(cfg.Daemon.QuietHours) != 0 {
		t.Fatalf("daemon defaults: %+v", cfg.Daemon)
	}
	// M0: an appended turn does not reach the agent the app runs in the
	// asking session, so in-place replies are opt-in.
	if *cfg.Daemon.ReplyInPlace {
		t.Fatal("reply_in_place must default to false")
	}
	if len(cfg.Copilot.ExtraArgs) != 0 {
		t.Fatalf("extra_args default = %v", cfg.Copilot.ExtraArgs)
	}
}

func TestCopilotExtraArgs(t *testing.T) {
	cfg, err := Parse([]byte(`
handle: alice
bus: {kind: memory}
copilot: {safe_tools: [view, grep, glob, bash], extra_args: ["--allow-tool", "shell(wormhole:*)", "--deny-tool", "shell(wormhole ask:*)"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Copilot.ExtraArgs, " "); got != "--allow-tool shell(wormhole:*) --deny-tool shell(wormhole ask:*)" {
		t.Fatalf("extra_args = %q", got)
	}
}

func TestSenderForms(t *testing.T) {
	cfg, err := Parse([]byte(`
handle: bob
bus: {kind: memory}
senders:
  - alice
  - {handle: carol, mode: auto}
copilot:
  safe_tools: [view, grep]
`))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := cfg.SenderMode("alice"); m != ModeInbox {
		t.Fatal(m)
	}
	if m, _ := cfg.SenderMode("carol"); m != ModeAuto {
		t.Fatal(m)
	}
}

func TestNotifyModes(t *testing.T) {
	cases := map[string]NotifyMode{
		"":                NotifyToast,
		"notify: true":    NotifyToast,
		"notify: false":   NotifyOff,
		"notify: toast":   NotifyToast,
		"notify: actions": NotifyActions,
		"notify: digest":  NotifyDigest,
		"notify: off":     NotifyOff,
		"notify: \"off\"": NotifyOff,
		"notify: yes":     NotifyToast,
		"notify: no":      NotifyOff,
	}
	for src, want := range cases {
		cfg, err := Parse([]byte("handle: alice\nbus: {kind: memory}\ndaemon: {" + src + "}\n"))
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if cfg.Daemon.Notify != want {
			t.Errorf("%q: notify = %q, want %q", src, cfg.Daemon.Notify, want)
		}
	}
	if NotifyOff.Enabled() || !NotifyDigest.Enabled() {
		t.Fatal("Enabled")
	}
}

func TestQuietHours(t *testing.T) {
	cfg, err := Parse([]byte("handle: alice\nbus: {kind: memory}\ndaemon: {quiet_hours: [\"18:00\", \"09:00\"]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 30, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		h, m int
		in   bool
	}{{17, 59, false}, {18, 0, true}, {23, 30, true}, {0, 0, true}, {8, 59, true}, {9, 0, false}, {12, 0, false}} {
		if got := cfg.Daemon.InQuietHours(at(c.h, c.m)); got != c.in {
			t.Errorf("%02d:%02d in quiet hours = %v, want %v", c.h, c.m, got, c.in)
		}
	}
	day, _ := Parse([]byte("handle: alice\nbus: {kind: memory}\ndaemon: {quiet_hours: [\"12:00\", \"13:00\"]}\n"))
	if !day.Daemon.InQuietHours(at(12, 30)) || day.Daemon.InQuietHours(at(13, 0)) || day.Daemon.InQuietHours(at(3, 0)) {
		t.Fatal("same-day window")
	}
	none, _ := Parse([]byte("handle: alice\nbus: {kind: memory}\n"))
	if none.Daemon.InQuietHours(at(3, 0)) {
		t.Fatal("no window set")
	}
}

func TestValidation(t *testing.T) {
	bad := map[string]string{
		"notify mode":   "handle: alice\nbus: {kind: memory}\ndaemon: {notify: loud}\n",
		"notify list":   "handle: alice\nbus: {kind: memory}\ndaemon: {notify: [toast]}\n",
		"digest short":  "handle: alice\nbus: {kind: memory}\ndaemon: {digest_every: 5s}\n",
		"snooze short":  "handle: alice\nbus: {kind: memory}\ndaemon: {snooze_default: 10s}\n",
		"quiet one":     "handle: alice\nbus: {kind: memory}\ndaemon: {quiet_hours: [\"18:00\"]}\n",
		"quiet format":  "handle: alice\nbus: {kind: memory}\ndaemon: {quiet_hours: [\"6pm\", \"09:00\"]}\n",
		"quiet same":    "handle: alice\nbus: {kind: memory}\ndaemon: {quiet_hours: [\"09:00\", \"09:00\"]}\n",
		"handle":        "handle: Alice\nbus: {kind: memory}\n",
		"self sender":   "handle: alice\nbus: {kind: memory}\nsenders: [alice]\n",
		"dup sender":    "handle: alice\nbus: {kind: memory}\nsenders: [bob, bob]\n",
		"bad mode":      "handle: alice\nbus: {kind: memory}\nsenders: [{handle: bob, mode: yolo}]\n",
		"auto no tools": "handle: alice\nbus: {kind: memory}\nsenders: [{handle: bob, mode: auto}]\n",
		"no url":        "handle: alice\nbus: {kind: nats, password_file: /x}\n",
		"no creds":      "handle: alice\nbus: {kind: nats, url: nats://bus.example.internal:4222}\n",
		"user mismatch": "handle: alice\nbus: {kind: nats, url: nats://h:4222, user: bob, password_file: /x}\n",
		"unknown key":   "handle: alice\nbus: {kind: memory}\nextra: 1\n",
		"repo key":      "handle: alice\nbus: {kind: memory}\nrepos: {noslash: /tmp}\n",
		"repo rel":      "handle: alice\nbus: {kind: memory}\nrepos: {\"o/r\": rel/path}\n",
	}
	for name, src := range bad {
		if _, err := Parse([]byte(src)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestLoadGuards(t *testing.T) {
	dir := t.TempDir()
	pw := write(t, dir, "password", "hunter22\n", 0o600)
	src := "handle: alice\nbus:\n  kind: nats\n  url: nats://bus.example.internal:4222\n  password_file: " + pw + "\n"

	world := write(t, dir, "world.yaml", src, 0o644)
	if _, err := Load(world); !errors.Is(err, ErrWorldReadable) {
		t.Fatalf("world-readable accepted: %v", err)
	}

	private := write(t, dir, "config.yaml", src, 0o600)
	cfg, err := Load(private)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != private || cfg.Bus.PasswordFile != pw {
		t.Fatalf("%+v", cfg)
	}

	repo := filepath.Join(dir, "repo", "deploy")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "repo", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	inTree := write(t, repo, "password", "x", 0o600)
	leaky := write(t, dir, "leaky.yaml", strings.Replace(src, pw, inTree, 1), 0o600)
	if _, err := Load(leaky); !errors.Is(err, ErrInGitTree) {
		t.Fatalf("creds in git tree accepted: %v", err)
	}
}

func TestWorkdirFor(t *testing.T) {
	cfg, err := Parse([]byte("handle: alice\nbus: {kind: memory}\ncopilot: {workdir: /srv/wh}\nrepos: {\"owner/repo\": /src/repo}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkdirFor("owner/repo") != "/src/repo" || cfg.WorkdirFor("other/x") != "/srv/wh" || cfg.WorkdirFor("") != "/srv/wh" {
		t.Fatal("workdir mapping")
	}
}
