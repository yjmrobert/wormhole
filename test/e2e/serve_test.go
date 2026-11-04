package e2e

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/bus"
	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/serve"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// TestServeJoinRoundTrip is the five-minute start on one machine: one
// embedded bus, two invites, two joins through the CLI, two daemons that
// authenticate with the credentials join wrote, one ask and one reply.
func TestServeJoinRoundTrip(t *testing.T) {
	srv, err := serve.Start(context.Background(), serve.Options{StateDir: filepath.Join(t.TempDir(), "serve"), Listen: "127.0.0.1:0",
		BcryptCost: bcrypt.MinCost, PollInterval: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	mint := func(handle, by string) string {
		tok, err := serve.NewInvite(srv.StateDir(), handle, by, time.Hour, time.Now(), nil, serve.InviteOptions{BcryptCost: bcrypt.MinCost})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	newBus := func(ctx context.Context, cfg *config.Config) (bus.Bus, error) {
		return natsbus.Connect(ctx, natsbus.Options{URL: cfg.Bus.URL, User: cfg.Handle, PasswordFile: cfg.Bus.PasswordFile, TLSCA: cfg.Bus.TLSCA,
			AckWait: 5 * time.Second, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}

	alice := joinedSide(t, "alice", mint("alice", ""), newBus, func(m map[string]any) {
		m["senders"] = []any{"bob"}
		m["copilot"] = map[string]any{"workdir": "/tmp/wh-e2e/alice"}
		m["daemon"] = map[string]any{"registry_interval": "1s", "reply_in_place": true}
	}, nil)
	if len(alice.cfg.Senders) != 1 || alice.cfg.Bus.TLSCA == "" || !strings.HasPrefix(alice.cfg.Bus.URL, "tls://127.0.0.1:") {
		t.Fatalf("alice config %+v", alice.cfg)
	}
	alice.runDaemon(t)
	waitFor(t, "alice registered", func() bool {
		code, out, _ := alice.cli(t, "", "users")
		users, _ := out["users"].([]any)
		return code == 0 && len(users) == 1
	})

	// bob joins while alice is online: join suggests asking her.
	bob := joinedSide(t, "bob", mint("bob", "alice"), newBus, func(m map[string]any) {
		if s, _ := m["senders"].([]any); len(s) != 1 || s[0] != "alice" {
			t.Fatalf("join did not seed the inviter: %v", m["senders"])
		}
		m["senders"] = []any{map[string]any{"handle": "alice", "mode": "auto"}}
		m["copilot"] = map[string]any{"workdir": "/tmp/wh-e2e/bob", "safe_tools": []any{"view", "grep"}}
		m["daemon"] = map[string]any{"registry_interval": "1s"}
	}, func(res map[string]any) {
		if res["next"] != `wormhole ask ^alice "..."` {
			t.Fatalf("bob next = %v", res["next"])
		}
	})
	bob.runDaemon(t)

	code, out, stderr := alice.cli(t, "", "ask", "bob", "What do you think about X?", "--yes")
	if code != 0 {
		t.Fatalf("ask: %d %v %s", code, out, stderr)
	}
	threadID, _ := out["thread_id"].(string)
	waitFor(t, "bob's session", func() bool { return len(bob.app.Snapshot()) >= 1 })
	create := bob.app.Snapshot()[0]
	if create.Op != "create" || !strings.HasSuffix(create.Session.Kickoff, "---\nWhat do you think about X?") {
		t.Fatalf("bob create %+v", create)
	}
	waitFor(t, "delivered", func() bool { return threadState(t, alice, threadID).State == threads.StateDelivered })
	bob.session = create.Session.ID
	code, out, stderr = bob.cli(t, "", "reply", threadID, "I think Y.", "--yes")
	if code != 0 {
		t.Fatalf("reply: %d %v %s", code, out, stderr)
	}
	waitFor(t, "alice's reply", func() bool { return threadState(t, alice, threadID).State == threads.StateReplied })
	if th := threadState(t, alice, threadID); !strings.Contains(th.StagedKickoff, "Reply from ^bob") {
		t.Fatalf("alice thread %+v", th)
	}
	users, _ := serve.ListUsers(srv.StateDir())
	if len(users) != 2 {
		t.Fatalf("serve users %v", users)
	}
}

// joinedSide runs `wormhole join` for handle into a fresh config dir, lets
// edit change the written config (the senders verbs are not here yet), and
// returns the side with the reloaded config.
func joinedSide(t *testing.T, handle, token string, newBus func(context.Context, *config.Config) (bus.Bus, error), edit func(map[string]any), check func(map[string]any)) *side {
	t.Helper()
	dir := t.TempDir()
	s := &side{handle: handle, cfgPath: filepath.Join(dir, "config", "config.yaml"), dbPath: filepath.Join(dir, "state", "threads.db"),
		app: &ghapp.Recorder{}, newBus: newBus, session: handle + "-root-session"}
	code, res, stderr := s.cli(t, "", "join", token)
	if code != 0 || res["ok"] != true || res["handle"] != handle {
		t.Fatalf("%s join: %d %v %s", handle, code, res, stderr)
	}
	if check != nil {
		check(res)
	}
	raw, err := os.ReadFile(s.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	raw, err = yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if s.cfg, err = config.Load(s.cfgPath); err != nil {
		t.Fatal(err)
	}
	if pw, err := os.ReadFile(s.cfg.Bus.PasswordFile); err != nil || len(strings.TrimSpace(string(pw))) < 32 {
		t.Fatalf("%s password file: %v", handle, err)
	}
	return s
}
