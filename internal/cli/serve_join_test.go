package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/serve"
	"github.com/yjmrobert/wormhole/internal/systemd"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// joinApp is an App wired to a real NATS connection, with no agents on
// PATH and a fake systemctl.
type joinApp struct {
	app     *App
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	runner  *systemd.FakeRunner
	cfgPath string
}

func newJoinApp(t *testing.T) *joinApp {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WORMHOLE_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("WORMHOLE_CONFIG_DIR", filepath.Join(dir, "config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("COPILOT_HOME", "")
	runner := &systemd.FakeRunner{Output: map[string]string{"is-active": "inactive", "is-enabled": "disabled"}}
	j := &joinApp{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, runner: runner, cfgPath: filepath.Join(dir, "config", "config.yaml")}
	real := Default()
	j.app = &App{
		Stdin: strings.NewReader(""), Stdout: j.out, Stderr: j.errOut,
		IsTerminal: func() bool { return false },
		Getenv:     func(string) string { return "" },
		NewBus:     real.NewBus,
		OpenStore:  func() (*threads.Store, error) { return threads.Open(config.ThreadsDBPath()) },
		LookPath:   func(string) (string, error) { return "", errors.New("none") },
		Systemctl:  runner,
		Executable: func() (string, error) { return "/home/bob/.local/bin/wormhole", nil },
		Now:        time.Now,
		Hostname:   "localhost",
		Version:    "test",
	}
	return j
}

// run executes a command and returns its exit code and parsed JSON.
func (j *joinApp) run(args ...string) (int, map[string]any) {
	j.out.Reset()
	j.errOut.Reset()
	code := j.app.Run(context.Background(), append(args, "--json"))
	var res map[string]any
	if strings.TrimSpace(j.out.String()) != "" {
		_ = json.Unmarshal(j.out.Bytes(), &res)
	}
	// In --json mode the error travels in the JSON; mirror it to errOut so
	// assertions read one place.
	if msg, ok := res["error"].(string); ok {
		j.errOut.WriteString(msg)
	}
	return code, res
}

// startBus runs the embedded bus on a free port under the state dir.
func startBus(t *testing.T) *serve.Server {
	t.Helper()
	s, err := serve.Start(context.Background(), serve.Options{StateDir: serveStateDir(), Listen: "127.0.0.1:0", Hostname: "localhost", PollInterval: 30 * time.Millisecond, BcryptCost: bcrypt.MinCost, Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestServeInviteUsersRevokeAndJoin(t *testing.T) {
	j := newJoinApp(t)
	if code, _ := j.run("serve", "invite", "bob"); code != ExitUnavailable || !strings.Contains(j.errOut.String(), "wormhole serve") {
		t.Fatalf("invite before serve: %d %s", code, j.errOut.String())
	}
	startBus(t)
	code, res := j.run("serve", "invite", "bob", "--by", "alice")
	if code != ExitOK || res["handle"] != "bob" || res["by"] != "alice" {
		t.Fatalf("invite: %d %v %s", code, res, j.errOut.String())
	}
	token := res["token"].(string)
	code, res = j.run("serve", "users")
	if code != ExitOK || len(res["invites"].([]any)) != 1 || len(res["users"].([]any)) != 0 {
		t.Fatalf("users: %d %v", code, res)
	}

	// join: config, credential, CA, sender seeded, service installed.
	code, res = j.run("join", token, "--config", j.cfgPath)
	if code != ExitOK {
		t.Fatalf("join: %d %v %s %s", code, res, j.out.String(), j.errOut.String())
	}
	if res["handle"] != "bob" || res["senders_added"].([]any)[0] != "alice" || !strings.HasPrefix(res["url"].(string), "tls://") || res["peer"] != "alice" || res["peer_online"] != false {
		t.Fatalf("join result = %v", res)
	}
	svc := res["service"].(map[string]any)
	if svc["installed"] != true {
		t.Fatalf("service = %v", svc)
	}
	cfg, err := config.Load(j.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Handle != "bob" || len(cfg.Senders) != 1 || cfg.Senders[0].Handle != "alice" || cfg.Senders[0].Mode != config.ModeInbox || cfg.Bus.TLSCA != filepath.Join(filepath.Dir(j.cfgPath), "bus-ca.pem") || !strings.Contains(cfg.Bus.URL, ",tls://") {
		t.Fatalf("cfg = %+v", cfg)
	}
	for _, f := range []string{"password", "bus-ca.pem", "config.yaml"} {
		st, err := os.Stat(filepath.Join(filepath.Dir(j.cfgPath), f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, err, st)
		}
	}
	pw1, _ := os.ReadFile(cfg.Bus.PasswordFile)
	if !strings.HasSuffix(string(pw1), "\n") {
		t.Fatal("password file lacks a trailing newline")
	}
	raw, _ := os.ReadFile(j.cfgPath)
	if !strings.Contains(string(raw), "# Your handle.") || strings.Contains(string(raw), "- bob\n") {
		t.Fatalf("config comments or example senders wrong:\n%s", raw)
	}
	code, res = j.run("serve", "users")
	if code != ExitOK || len(res["users"].([]any)) != 1 || len(res["invites"].([]any)) != 0 {
		t.Fatalf("users after join: %v", res)
	}

	// A second join needs --rotate on the invite side, keeps the rest of
	// the config and changes the credential.
	if err := config.Edit(j.cfgPath, false, func(doc *yaml.Node) error { config.Set(doc, "daemon.notify", "digest"); return nil }); err != nil {
		t.Fatal(err)
	}
	if code, _ := j.run("serve", "invite", "bob"); code != ExitRefused || !strings.Contains(j.errOut.String(), "--rotate") {
		t.Fatalf("re-invite: %d %s", code, j.errOut.String())
	}
	code, res = j.run("serve", "invite", "bob", "--rotate", "--by", "alice")
	if code != ExitOK {
		t.Fatalf("rotate invite: %d", code)
	}
	j.runner.Output["is-active"] = "active"
	code, res = j.run("join", res["token"].(string), "--config", j.cfgPath)
	if code != ExitOK || len(res["senders_added"].([]any)) != 0 {
		t.Fatalf("second join: %d %v %s", code, res, j.errOut.String())
	}
	cfg, _ = config.Load(j.cfgPath)
	pw2, _ := os.ReadFile(cfg.Bus.PasswordFile)
	if string(pw1) == string(pw2) || cfg.Daemon.Notify != config.NotifyDigest || len(cfg.Senders) != 1 {
		t.Fatalf("second join did not rotate or lost settings: %+v", cfg)
	}
	restarted := false
	for _, call := range j.runner.Calls {
		if strings.Join(call, " ") == "systemctl --user restart wormhole.service" {
			restarted = true
		}
	}
	if !restarted {
		t.Fatalf("running daemon not restarted after rotation: %v", j.runner.Calls)
	}
	if code, _ := j.run("join", token, "--config", j.cfgPath); code != ExitRefused {
		t.Fatalf("spent invite: %d %s", code, j.errOut.String())
	}

	// revoke
	if code, _ := j.run("serve", "revoke", "carol"); code != ExitNotFound {
		t.Fatalf("revoke unknown: %d", code)
	}
	if code, _ := j.run("serve", "revoke", "bob"); code != ExitOK {
		t.Fatalf("revoke: %d %s", code, j.errOut.String())
	}
	code, res = j.run("serve", "users")
	if code != ExitOK || len(res["users"].([]any)) != 0 {
		t.Fatalf("users after revoke: %v", res)
	}
}

func TestJoinRefusals(t *testing.T) {
	j := newJoinApp(t)
	if code, _ := j.run("join", "hello", "--config", j.cfgPath); code != ExitUsage || !strings.Contains(j.errOut.String(), "not a wormhole invite") {
		t.Fatalf("garbage: %d %s", code, j.errOut.String())
	}
	s := startBus(t)
	past := func() time.Time { return time.Now().Add(-48 * time.Hour) }
	tok, _, err := serve.Mint(serveStateDir(), "bob", serve.InviteOptions{Now: past, BcryptCost: bcrypt.MinCost, By: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := j.run("join", tok, "--config", j.cfgPath); code != ExitRefused || !strings.Contains(j.errOut.String(), "expired") || !strings.Contains(j.errOut.String(), "wormhole serve invite bob") {
		t.Fatalf("expired: %d %s", code, j.errOut.String())
	}
	// Config inside a git tree.
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tok, _, _ = serve.Mint(serveStateDir(), "bob", serve.InviteOptions{BcryptCost: bcrypt.MinCost})
	if code, _ := j.run("join", tok, "--config", filepath.Join(repo, "config.yaml")); code != ExitRefused || !strings.Contains(j.errOut.String(), "WORMHOLE_CONFIG_DIR") {
		t.Fatalf("git tree: %d %s", code, j.errOut.String())
	}
	// Bus down: exit 5 naming the host and the inviter.
	inv, _ := serve.Decode(tok)
	inv.URLs = []string{"tls://127.0.0.1:1"}
	inv.By = "alice"
	_, err = j.app.joinWithInvite(context.Background(), &common{config: j.cfgPath}, serve.Encode(inv), joinOptions{timeout: time.Second}, nil)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != ExitUnavailable || !strings.Contains(err.Error(), "127.0.0.1:1") || !strings.Contains(err.Error(), "ask ^alice") {
		t.Fatalf("bus down: %v", err)
	}
	s.Close()
}

// lockedBuffer is a bytes.Buffer safe to read while another goroutine
// writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestServeForegroundAndInstall(t *testing.T) {
	j := newJoinApp(t)
	// Foreground: prints the status (with an invite) and blocks until the
	// context ends.
	out, errOut := &lockedBuffer{}, &lockedBuffer{}
	j.app.Stdout, j.app.Stderr = out, errOut
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- j.app.Run(ctx, []string{"serve", "--listen", "127.0.0.1:0", "--invite", "carol", "--json"})
	}()
	deadline := time.Now().Add(15 * time.Second)
	for strings.TrimSpace(out.String()) == "" {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve printed nothing: %s", errOut.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	var st serveStatus
	if err := json.Unmarshal([]byte(out.String()), &st); err != nil {
		t.Fatalf("status: %v %q", err, out.String())
	}
	if len(st.URLs) == 0 || len(st.Invites) != 1 || st.Invites[0].Handle != "carol" || !strings.HasPrefix(st.Invites[0].Token, "wh1.") {
		t.Fatalf("status = %+v", st)
	}
	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("serve exit %d: %s", code, errOut.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	j.app.Stdout, j.app.Stderr = j.out, j.errOut

	// install: unit written, fake systemd, the bus "started" is one we run
	// in-process on the same port, this machine joins as --handle, and an
	// invite for bob carries by=alice.
	s := startBus(t)
	port := s.ClientURLs()[len(s.ClientURLs())-1][strings.LastIndex(s.ClientURLs()[len(s.ClientURLs())-1], ":")+1:]
	code, res := j.run("serve", "install", "--listen", "127.0.0.1:"+port, "--handle", "alice", "--invite", "bob", "--config", j.cfgPath)
	if code != ExitOK {
		t.Fatalf("install: %d %v %s %s", code, res, j.out.String(), j.errOut.String())
	}
	unit, err := os.ReadFile(systemd.UserUnitPath(serveUnit))
	if err != nil || !strings.Contains(string(unit), "ExecStart=/home/bob/.local/bin/wormhole serve --listen 127.0.0.1:"+port+"\n") {
		t.Fatalf("unit = %s, %v", unit, err)
	}
	joined := res["joined"].(map[string]any)
	if joined["handle"] != "alice" {
		t.Fatalf("joined = %v", joined)
	}
	invites := res["invites"].([]any)
	if len(invites) != 1 {
		t.Fatalf("invites = %v", invites)
	}
	inv, err := serve.Decode(invites[0].(map[string]any)["token"].(string))
	if err != nil || inv.Handle != "bob" || inv.By != "alice" {
		t.Fatalf("invite = %+v, %v", inv, err)
	}
	cfg, err := config.Load(j.cfgPath)
	if err != nil || cfg.Handle != "alice" || len(cfg.Senders) != 0 {
		t.Fatalf("local config = %+v, %v", cfg, err)
	}
	// Running install again does not re-join.
	code, res = j.run("serve", "install", "--listen", "127.0.0.1:"+port, "--config", j.cfgPath)
	if code != ExitOK || res["joined"] != nil {
		t.Fatalf("second install: %d %v", code, res)
	}
}

// TestJoinFailureKeepsTrustAnchor checks that a refused join leaves the
// CA of the bus this machine is already on untouched.
func TestJoinFailureKeepsTrustAnchor(t *testing.T) {
	j := newJoinApp(t)
	startBus(t)
	code, res := j.run("serve", "invite", "bob", "--by", "alice")
	if code != ExitOK {
		t.Fatalf("invite: %d", code)
	}
	if code, _ := j.run("join", res["token"].(string), "--config", j.cfgPath); code != ExitOK {
		t.Fatalf("join: %d %s", code, j.errOut.String())
	}
	caPath := filepath.Join(filepath.Dir(j.cfgPath), "bus-ca.pem")
	before, _ := os.ReadFile(caPath)
	cfgBefore, _ := os.ReadFile(j.cfgPath)
	// An invite from a different bus that cannot be reached.
	inv, _ := serve.Decode(res["token"].(string))
	inv.ID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	inv.URLs = []string{"tls://127.0.0.1:1"}
	inv.CA = base64.StdEncoding.EncodeToString([]byte("not the same ca"))
	_, err := j.app.joinWithInvite(context.Background(), &common{config: j.cfgPath}, serve.Encode(inv), joinOptions{timeout: time.Second}, nil)
	if err == nil {
		t.Fatal("join to a dead bus succeeded")
	}
	after, _ := os.ReadFile(caPath)
	cfgAfter, _ := os.ReadFile(j.cfgPath)
	if string(after) != string(before) || string(cfgAfter) != string(cfgBefore) {
		t.Fatal("a failed join changed the CA or the config")
	}
	if entries, _ := os.ReadDir(filepath.Dir(j.cfgPath)); len(entries) != 3 {
		t.Fatalf("leftover files: %v", entries)
	}
}
