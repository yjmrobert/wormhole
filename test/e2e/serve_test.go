package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yjmrobert/wormhole/internal/bus"
	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/cli"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/serve"
	"github.com/yjmrobert/wormhole/internal/systemd"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// TestServeAndJoinRoundTrip is the five-minute start on one machine: the
// embedded bus, two invites, two joins into separate config dirs, two
// daemons on the credentials join wrote, and an ask that is answered.
func TestServeAndJoinRoundTrip(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "serve")
	srv, err := serve.Start(context.Background(), serve.Options{StateDir: stateDir, Listen: "127.0.0.1:0", Hostname: "localhost", PollInterval: 50 * time.Millisecond, BcryptCost: bcrypt.MinCost, Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	newBus := func(ctx context.Context, cfg *config.Config) (bus.Bus, error) {
		return natsbus.Connect(ctx, natsbus.Options{URL: cfg.Bus.URL, User: cfg.Handle, PasswordFile: cfg.Bus.PasswordFile, TLSCA: cfg.Bus.TLSCA, AckWait: 5 * time.Second})
	}
	join := func(handle, by string) *side {
		t.Helper()
		tok, _, err := serve.Mint(stateDir, handle, serve.InviteOptions{By: by, BcryptCost: bcrypt.MinCost})
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, handle)
		cfgPath := filepath.Join(dir, "config", "config.yaml")
		s := &side{handle: handle, cfgPath: cfgPath, dbPath: filepath.Join(dir, "state", "threads.db"), app: &ghapp.Recorder{}, newBus: newBus, session: handle + "-root-session"}
		app := &cli.App{
			Stdin: strings.NewReader(""), Stdout: os.Stdout, Stderr: os.Stderr,
			IsTerminal: func() bool { return false },
			Getenv:     func(string) string { return "" },
			NewBus:     newBus,
			NewGHApp:   func(*config.Config) ghapp.App { return s.app },
			OpenStore:  func() (*threads.Store, error) { return threads.Open(s.dbPath) },
			LookPath:   func(string) (string, error) { return "", errors.New("no agent") },
			Systemctl:  &systemd.FakeRunner{Missing: true},
			Now:        time.Now,
			Hostname:   handle + "-laptop",
			Version:    "test",
		}
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
		var out strings.Builder
		app.Stdout = &out
		if code := app.Run(context.Background(), []string{"join", tok, "--json", "--config", cfgPath}); code != 0 {
			t.Fatalf("join %s: exit %d: %s", handle, code, out.String())
		}
		var res map[string]any
		if err := json.Unmarshal([]byte(out.String()), &res); err != nil {
			t.Fatalf("join %s output: %v", handle, err)
		}
		if res["ok"] != true || res["handle"] != handle {
			t.Fatalf("join %s = %v", handle, res)
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		s.cfg = cfg
		return s
	}
	alice := join("alice", "")
	bob := join("bob", "alice")
	if len(alice.cfg.Senders) != 0 || len(bob.cfg.Senders) != 1 || bob.cfg.Senders[0].Handle != "alice" {
		t.Fatalf("senders: alice %v bob %v", alice.cfg.Senders, bob.cfg.Senders)
	}
	// alice lets bob answer her; bob already lets alice ask (seeded by join).
	code, _, stderr := alice.cli(t, "", "senders", "add", "bob")
	if code != 0 {
		t.Fatalf("senders add: %d %s", code, stderr)
	}
	alice.cfg, _ = config.Load(alice.cfgPath)

	alice.runDaemon(t)
	bob.runDaemon(t)
	waitFor(t, "registry", func() bool {
		code, out, _ := alice.cli(t, "", "users")
		users, _ := out["users"].([]any)
		return code == 0 && len(users) == 2
	})
	code, out, stderr := alice.cli(t, "", "ask", "bob", "Does the embedded bus work?", "--yes")
	if code != 0 {
		t.Fatalf("ask: %d %v %s", code, out, stderr)
	}
	threadID, _ := out["thread_id"].(string)
	waitFor(t, "bob's staged ask", func() bool { return threadState(t, bob, threadID).StagedKickoff != "" })
	waitFor(t, "delivered", func() bool { return threadState(t, alice, threadID).State == threads.StateDelivered })
	// bob reads it by reference and replies.
	if code, out, stderr := bob.cli(t, "", "open", "^alice", "--yes"); code != 0 {
		t.Fatalf("open ^alice: %d %v %s", code, out, stderr)
	}
	if code, out, stderr := bob.cli(t, "", "reply", "^alice", "Yes: TLS, bcrypt users, one-time invites.", "--yes"); code != 0 {
		t.Fatalf("reply: %d %v %s", code, out, stderr)
	}
	waitFor(t, "alice's reply", func() bool { return threadState(t, alice, threadID).State == threads.StateReplied })
	code, out, _ = alice.cli(t, "", "inbox")
	items, _ := out["inbox"].([]any)
	if code != 0 || len(items) != 1 || items[0].(map[string]any)["ref"] != "^bob" {
		t.Fatalf("inbox = %v", out)
	}
	// Revoking bob disconnects his daemon and his next publish fails.
	if err := serve.Revoke(stateDir, "bob"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob revoked", func() bool {
		code, _, _ := bob.cli(t, "", "users")
		return code == cli.ExitUnavailable
	})
}
