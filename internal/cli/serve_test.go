package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yjmrobert/wormhole/internal/bus"
	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/serve"
)

// syncBuffer is a bytes.Buffer safe for a command writing while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// serveEnv isolates the state and config directories for one test.
type serveEnv struct {
	root     string
	stateDir string
	cfgDir   string
}

// newServeEnv points WORMHOLE_STATE_DIR and WORMHOLE_CONFIG_DIR at temp dirs.
func newServeEnv(t *testing.T) *serveEnv {
	t.Helper()
	root := t.TempDir()
	e := &serveEnv{root: root, stateDir: filepath.Join(root, "state"), cfgDir: filepath.Join(root, "config")}
	t.Setenv("WORMHOLE_STATE_DIR", e.stateDir)
	t.Setenv("WORMHOLE_CONFIG_DIR", e.cfgDir)
	t.Setenv("WORMHOLE_CONFIG", "")
	oldRetry, oldCost := joinRetry, serveBcryptCost
	joinRetry, serveBcryptCost = 3*time.Second, bcrypt.MinCost
	t.Cleanup(func() { joinRetry, serveBcryptCost = oldRetry, oldCost })
	return e
}

// startServe runs the embedded bus in the env's state directory.
func (e *serveEnv) startServe(t *testing.T) *serve.Server {
	t.Helper()
	s, err := serve.Start(context.Background(), serve.Options{StateDir: serve.DefaultStateDir(), Listen: "127.0.0.1:0", BcryptCost: bcrypt.MinCost,
		PollInterval: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// realBus connects the way Default does.
func realBus(ctx context.Context, cfg *config.Config) (bus.Bus, error) {
	return natsbus.Connect(ctx, natsbus.Options{URL: cfg.Bus.URL, User: cfg.Handle, Creds: cfg.Bus.Creds, PasswordFile: cfg.Bus.PasswordFile, TLSCA: cfg.Bus.TLSCA,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// serveApp is an App with the real bus and clock.
func serveApp(stdout io.Writer, host string) *App {
	return &App{
		Stdin: strings.NewReader(""), Stdout: stdout, Stderr: io.Discard,
		IsTerminal: func() bool { return false }, Getenv: func(string) string { return "" },
		NewBus: realBus, Now: time.Now, Hostname: host, Version: "test",
	}
}

// runJSON runs a command with --json and decodes its output.
func runJSON(t *testing.T, a *App, args ...string) (int, map[string]any) {
	t.Helper()
	var out bytes.Buffer
	a.Stdout = &out
	code := a.Run(context.Background(), append(args, "--json"))
	var m map[string]any
	if s := strings.TrimSpace(out.String()); s != "" {
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("%v: not json: %q", args, s)
		}
	}
	return code, m
}

// runText runs a command and returns its human output.
func runText(t *testing.T, a *App, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	a.Stdout = &out
	code := a.Run(context.Background(), args)
	return code, out.String()
}

// TestServeInviteUsersRevoke drives the management verbs against state a
// running server owns.
func TestServeInviteUsersRevoke(t *testing.T) {
	e := newServeEnv(t)
	app := serveApp(io.Discard, "alice-laptop")
	// Nothing served yet: the error names the fix.
	code, out := runJSON(t, app, "serve", "invite", "bob")
	if code != ExitUnavailable || !strings.Contains(out["error"].(string), "run: wormhole serve") {
		t.Fatalf("no state: %d %v", code, out)
	}
	s := e.startServe(t)

	code, text := runText(t, app, "serve", "invite", "bob", "--by", "alice")
	if code != 0 || !strings.HasPrefix(text, "on bob's machine run: wormhole join wh1.") {
		t.Fatalf("invite: %d %q", code, text)
	}
	code, out = runJSON(t, app, "serve", "invite", "carol", "--ttl", "2h")
	if code != 0 || out["ok"] != true || out["handle"] != "carol" || !strings.HasPrefix(out["token"].(string), "wh1.") || out["expires"] == nil {
		t.Fatalf("invite json: %d %v", code, out)
	}
	inv, err := serve.Decode(out["token"].(string))
	if err != nil || inv.By != "" || inv.URLs[0] != s.ClientURLs()[0] || time.Until(inv.Expires()) > 2*time.Hour {
		t.Fatalf("carol invite %+v %v", inv, err)
	}
	for _, bad := range [][]string{{"serve", "invite", "Bob"}, {"serve", "invite"}, {"serve", "invite", "dave", "--ttl", "0s"}, {"serve", "revoke", "No/pe"}} {
		if code, _ := runJSON(t, app, bad...); code != ExitUsage {
			t.Fatalf("%v: code %d", bad, code)
		}
	}

	code, out = runJSON(t, app, "serve", "users")
	invites, _ := out["invites"].([]any)
	users, _ := out["users"].([]any)
	if code != 0 || len(invites) != 2 || len(users) != 0 {
		t.Fatalf("users: %d %v", code, out)
	}
	first := invites[0].(map[string]any)
	if first["handle"] != "carol" || first["by"] != "" || first["expires"] == nil {
		t.Fatalf("soonest invite first: %v", first)
	}
	code, text = runText(t, app, "serve", "users")
	if code != 0 || !strings.Contains(text, "INVITED") || !strings.Contains(text, "^alice") {
		t.Fatalf("users text: %q", text)
	}

	code, out = runJSON(t, app, "serve", "revoke", "carol")
	if code != 0 || out["revoked"] != true {
		t.Fatalf("revoke: %d %v", code, out)
	}
	if code, _ := runJSON(t, app, "serve", "revoke", "carol"); code != ExitNotFound {
		t.Fatalf("second revoke: %d", code)
	}
	if code, _ := runJSON(t, app, "commands", "serve"); code != 0 {
		t.Fatalf("serve not in the command tree: %d", code)
	}
}

// TestJoin joins a machine, rejoins with a rotated invite, and checks the
// refusals.
func TestJoin(t *testing.T) {
	e := newServeEnv(t)
	s := e.startServe(t)
	alice := serveApp(io.Discard, "alice-laptop")
	bob := serveApp(io.Discard, "bob-laptop")
	_, out := runJSON(t, alice, "serve", "invite", "bob", "--by", "alice")
	token := out["token"].(string)

	code, out := runJSON(t, bob, "join", token)
	if code != 0 {
		t.Fatalf("join: %d %v", code, out)
	}
	cfgPath := filepath.Join(e.cfgDir, "config.yaml")
	pwPath := filepath.Join(e.cfgDir, "password")
	caPath := filepath.Join(e.cfgDir, "bus-ca.pem")
	url := strings.Join(s.ClientURLs(), ",")
	if out["ok"] != true || out["handle"] != "bob" || out["url"] != url || out["config"] != cfgPath || out["password_file"] != pwPath || out["tls_ca"] != caPath ||
		out["next"] != `wormhole ask ^alice "..."` {
		t.Fatalf("join result %v", out)
	}
	if added, _ := out["senders_added"].([]any); len(added) != 1 || added[0] != "alice" {
		t.Fatalf("senders_added %v", out["senders_added"])
	}
	for _, p := range []string{cfgPath, pwPath, caPath} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, info, err)
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Handle != "bob" || cfg.Bus.URL != url || cfg.Bus.TLSCA != caPath || cfg.Bus.PasswordFile != pwPath || len(cfg.Senders) != 1 || cfg.Senders[0].Handle != "alice" || cfg.Senders[0].Mode != config.ModeInbox {
		t.Fatalf("config %+v", cfg)
	}
	raw, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), "# Who may open a wormhole to you.") {
		t.Fatalf("fresh config lost the example's comments:\n%s", raw)
	}
	if strings.Contains(string(raw), "# tls_ca:") || strings.Count(string(raw), "tls_ca:") != 1 {
		t.Fatalf("tls_ca written twice:\n%s", raw)
	}
	pw1, _ := os.ReadFile(pwPath)
	if !strings.HasSuffix(string(pw1), "\n") {
		t.Fatal("password file without a trailing newline")
	}

	// The invite is spent.
	if code, out := runJSON(t, bob, "join", token); code != ExitRefused {
		t.Fatalf("spent invite: %d %v", code, out)
	}

	// Someone edits the config; a rejoin with a rotated credential keeps it.
	edited := strings.Replace(string(raw), "per_day: 50", "per_day: 7 # mine", 1)
	edited = strings.Replace(edited, "  - alice\n", "  - alice\n  - {handle: carol, mode: inbox}\n", 1)
	if err := os.WriteFile(cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runJSON(t, alice, "serve", "invite", "bob"); code != ExitRefused || !strings.Contains(out["error"].(string), "--rotate") {
		t.Fatalf("invite for a joined handle: %d %v", code, out)
	}
	_, out = runJSON(t, alice, "serve", "invite", "bob", "--by", "alice", "--rotate")
	code, out = runJSON(t, bob, "join", out["token"].(string))
	if code != 0 {
		t.Fatalf("rejoin: %d %v", code, out)
	}
	if added, _ := out["senders_added"].([]any); len(added) != 0 {
		t.Fatalf("rejoin added senders: %v", added)
	}
	pw2, _ := os.ReadFile(pwPath)
	if string(pw1) == string(pw2) {
		t.Fatal("rejoin kept the old credential")
	}
	raw2, _ := os.ReadFile(cfgPath)
	cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw2), "per_day: 7 # mine") || cfg.Limits.PerDay != 7 || len(cfg.Senders) != 2 || !strings.Contains(string(raw2), "# Who may open a wormhole to you.") {
		t.Fatalf("rejoin rewrote other keys:\n%s", raw2)
	}
	// The old credential stops working once the server reloads.
	cfg.Bus.PasswordFile = filepath.Join(t.TempDir(), "old")
	if err := os.WriteFile(cfg.Bus.PasswordFile, pw1, 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := realBus(context.Background(), cfg); err == nil {
		b.Close()
		t.Fatal("old credential still accepted")
	}

	// The human form names the bus and the next step.
	_, out = runJSON(t, alice, "serve", "invite", "bob", "--by", "alice", "--rotate")
	code, text := runText(t, bob, "join", out["token"].(string))
	want := "joined " + s.ClientURLs()[0] + " as ^bob\nwhen ^alice is online: wormhole ask ^alice \"...\"\n"
	if code != 0 || text != want {
		t.Fatalf("join text: %d %q, want %q", code, text, want)
	}

	// Garbage and expiry.
	if code, out := runJSON(t, bob, "join", "nats://bus.example.internal:4222"); code != ExitUsage || !strings.Contains(out["error"].(string), "not a wormhole invite") {
		t.Fatalf("garbage: %d %v", code, out)
	}
	_, out = runJSON(t, alice, "serve", "invite", "carol", "--by", "alice", "--ttl", "1h")
	late := serveApp(io.Discard, "carol-laptop")
	late.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if code, out := runJSON(t, late, "join", out["token"].(string)); code != ExitRefused || !strings.Contains(out["error"].(string), "ask ^alice for a new invite") {
		t.Fatalf("expired: %d %v", code, out)
	}
}

// TestJoinRefusesGitTree checks the config is never written into a
// repository and the invite is not spent.
func TestJoinRefusesGitTree(t *testing.T) {
	e := newServeEnv(t)
	e.startServe(t)
	repo := filepath.Join(e.root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORMHOLE_CONFIG_DIR", filepath.Join(repo, "cfg"))
	app := serveApp(io.Discard, "bob-laptop")
	_, out := runJSON(t, app, "serve", "invite", "bob")
	code, res := runJSON(t, app, "join", out["token"].(string))
	if code != ExitRefused || !strings.Contains(res["error"].(string), "set WORMHOLE_CONFIG_DIR or run from your home directory") {
		t.Fatalf("git tree: %d %v", code, res)
	}
	if _, err := os.Stat(filepath.Join(repo, "cfg")); !os.IsNotExist(err) {
		t.Fatalf("wrote into the repository: %v", err)
	}
	if invites, _ := serve.ListInvites(serve.DefaultStateDir()); len(invites) != 1 {
		t.Fatalf("invite spent: %v", invites)
	}
}

// TestJoinUnreachable checks the not-running error names the host and fix.
func TestJoinUnreachable(t *testing.T) {
	e := newServeEnv(t)
	s := e.startServe(t)
	joinRetry = 500 * time.Millisecond
	tok, err := serve.NewInvite(s.StateDir(), "bob", "alice", time.Hour, time.Now(), []string{"tls://127.0.0.1:1"}, serve.InviteOptions{BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	code, out := runJSON(t, serveApp(io.Discard, "bob-laptop"), "join", tok)
	if code != ExitUnavailable || !strings.Contains(out["error"].(string), "is wormhole serve running on 127.0.0.1? ask ^alice to check `wormhole serve users`") {
		t.Fatalf("unreachable: %d %v", code, out)
	}
}

// TestServeForeground runs `wormhole serve` itself, joins with the invite it
// printed, and stops it with the context.
func TestServeForeground(t *testing.T) {
	newServeEnv(t)
	out := &syncBuffer{}
	app := serveApp(out, "alice-laptop")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- app.Run(ctx, []string{"serve", "--listen", "127.0.0.1:0", "--invite", "bob", "--json"})
	}()
	var res struct {
		OK       bool     `json:"ok"`
		URLs     []string `json:"urls"`
		StateDir string   `json:"state_dir"`
		Invites  []struct {
			Handle string `json:"handle"`
			Token  string `json:"token"`
		} `json:"invites"`
	}
	deadline := time.Now().Add(20 * time.Second)
	for json.Unmarshal([]byte(out.String()), &res) != nil {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no serve output: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !res.OK || len(res.URLs) != 1 || res.StateDir != serve.DefaultStateDir() || len(res.Invites) != 1 || res.Invites[0].Handle != "bob" {
		cancel()
		t.Fatalf("serve output %+v", res)
	}
	code, jr := runJSON(t, serveApp(io.Discard, "bob-laptop"), "join", res.Invites[0].Token)
	cancel()
	if got := <-done; got != 0 {
		t.Fatalf("serve exit %d", got)
	}
	if code != 0 {
		t.Fatalf("join against serve: %d %v", code, jr)
	}
}
