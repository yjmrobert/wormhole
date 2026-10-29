// Package e2e runs the full round trip on one machine: two config dirs, two
// bus users, two daemons, the CLI on both sides. alice asks bob from a
// session, bob's daemon materializes it, bob replies from inside that
// session, and alice's daemon lands the reply.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/bus/memory"
	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/cli"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// side is one user's machine: config, state, app recorder and CLI.
type side struct {
	handle  string
	cfgPath string
	dbPath  string
	app     *ghapp.Recorder
	cfg     *config.Config
	newBus  func(ctx context.Context, cfg *config.Config) (bus.Bus, error)
	session string // the Copilot session the CLI pretends to run in
}

func (s *side) cli(t *testing.T, stdin string, args ...string) (int, map[string]any, string) {
	t.Helper()
	var out, errb bytes.Buffer
	app := &cli.App{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		IsTerminal: func() bool { return false },
		Getenv: func(k string) string {
			if k == cli.SessionEnv {
				return s.session
			}
			return ""
		},
		NewBus:    s.newBus,
		NewGHApp:  func(*config.Config) ghapp.App { return s.app },
		OpenStore: func() (*threads.Store, error) { return threads.Open(s.dbPath) },
		Now:       time.Now,
		Hostname:  s.handle + "-laptop",
		Version:   "test",
	}
	code := app.Run(context.Background(), append([]string{args[0], "--json", "--config", s.cfgPath}, args[1:]...))
	var parsed map[string]any
	if strings.TrimSpace(out.String()) != "" {
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("%s output is not json: %q (%v)", args[0], out.String(), err)
		}
	}
	return code, parsed, errb.String()
}

func (s *side) runDaemon(t *testing.T) {
	t.Helper()
	store, err := threads.Open(s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.newBus(context.Background(), s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := daemon.New(daemon.Options{Config: s.cfg, Bus: b, Store: store, App: s.app, Host: s.handle + "-laptop",
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})), Paused: func() bool { return false }})
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; b.Close(); store.Close() })
}

func newSide(t *testing.T, handle, yaml string, newBus func(ctx context.Context, cfg *config.Config) (bus.Bus, error)) *side {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return &side{handle: handle, cfgPath: cfgPath, dbPath: filepath.Join(dir, "state", "threads.db"), app: &ghapp.Recorder{}, cfg: cfg, newBus: newBus, session: handle + "-root-session"}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func threadState(t *testing.T, s *side, id string) threads.Thread {
	t.Helper()
	store, err := threads.Open(s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	th, err := store.Get(context.Background(), id)
	if err != nil {
		return threads.Thread{}
	}
	return th
}

// roundTrip is the scenario; it is run on both bus adapters.
func roundTrip(t *testing.T, alice, bob *side) {
	alice.runDaemon(t)
	bob.runDaemon(t)

	// Both daemons register.
	waitFor(t, "registry", func() bool {
		code, out, _ := alice.cli(t, "", "users")
		if code != 0 {
			return false
		}
		users, _ := out["users"].([]any)
		return len(users) == 2
	})

	// Secret scan refuses, --yes required when not a terminal.
	code, out, _ := alice.cli(t, "", "ask", "bob", "here: ghp_abcdefghijklmnopqrstuvwxyz0123", "--yes")
	if code != cli.ExitRefused || out["ok"] != false {
		t.Fatalf("secret not refused: %d %v", code, out)
	}
	code, _, _ = alice.cli(t, "", "ask", "bob", "What do you think about X?")
	if code != cli.ExitRefused {
		t.Fatalf("non-terminal ask without --yes accepted: %d", code)
	}

	// alice asks bob. Flags after positionals, as agents write them.
	code, out, stderr := alice.cli(t, "", "ask", "bob", "What do you think about X?", "--repo", "owner/repo", "--context", "PR #123", "--yes")
	if code != 0 {
		t.Fatalf("ask failed: %d %v %s", code, out, stderr)
	}
	threadID, _ := out["thread_id"].(string)
	if threadID == "" || out["human_reviewed"] != false || out["state"] != "sent" {
		t.Fatalf("ask result: %v", out)
	}

	// bob's daemon runs copilot in auto mode with the safe tool set.
	waitFor(t, "bob's session", func() bool { return len(bob.app.Snapshot()) >= 1 })
	calls := bob.app.Snapshot()
	create := calls[0]
	if create.Op != "create" || create.Session.Name != "^alice: What do you think about X?" || strings.Join(create.Session.AvailableTools, ",") != "view,grep" {
		t.Fatalf("bob create = %+v", create)
	}
	if !strings.Contains(create.Session.Kickoff, "wormhole reply "+threadID) || !strings.HasSuffix(create.Session.Kickoff, "---\nWhat do you think about X?") || create.Session.Workdir != "/tmp/wh-e2e/repo" {
		t.Fatalf("kickoff = %q workdir = %q", create.Session.Kickoff, create.Session.Workdir)
	}

	// alice sees the delivery receipt.
	waitFor(t, "delivered state", func() bool { return threadState(t, alice, threadID).State == threads.StateDelivered })

	// bob's agent, inside the relayed session, replies.
	bob.session = create.Session.ID
	code, out, stderr = bob.cli(t, "", "reply", threadID, "I think Y, because Z.", "--yes")
	if code != 0 {
		t.Fatalf("reply failed: %d %v %s", code, out, stderr)
	}
	if out["hop"] != float64(2) || out["to"] != "alice" {
		t.Fatalf("reply result: %v", out)
	}

	// alice is in inbox mode for bob: the reply is staged, not run.
	waitFor(t, "alice's reply thread", func() bool { return threadState(t, alice, threadID).State == threads.StateReplied })
	th := threadState(t, alice, threadID)
	if th.LocalSessionID != "" || !strings.Contains(th.StagedKickoff, "Reply from ^bob on thread "+threadID) || !strings.Contains(th.StagedKickoff, "session alice-root-session") {
		t.Fatalf("alice thread = %+v", th)
	}
	waitFor(t, "alice notification", func() bool {
		for _, c := range alice.app.Snapshot() {
			if c.Op == "notify" && strings.Contains(c.Title, "^bob replied") {
				return true
			}
		}
		return false
	})
	code, out, _ = alice.cli(t, "", "inbox")
	items, _ := out["inbox"].([]any)
	if code != 0 || len(items) != 1 || items[0].(map[string]any)["kind"] != "reply" {
		t.Fatalf("inbox = %v", out)
	}

	// alice opens it: the staged session starts with her normal tools.
	code, out, stderr = alice.cli(t, "", "open", threadID)
	if code != 0 || out["started"] != true || out["opened"] != true {
		t.Fatalf("open = %d %v %s", code, out, stderr)
	}
	var opened ghapp.Call
	for _, c := range alice.app.Snapshot() {
		if c.Op == "create" {
			opened = c
		}
	}
	if opened.Session.Name != "Reply from ^bob" || len(opened.Session.AvailableTools) != 0 || !strings.HasSuffix(opened.Session.Kickoff, "---\nI think Y, because Z.") {
		t.Fatalf("opened = %+v", opened)
	}
	if fb, _ := out["fallback"].(string); !strings.HasPrefix(fb, "copilot --resume=") {
		t.Fatalf("fallback = %v", out["fallback"])
	}
	code, out, _ = alice.cli(t, "", "inbox")
	if items, _ := out["inbox"].([]any); code != 0 || len(items) != 0 {
		t.Fatalf("inbox after open = %v", out)
	}

	// Follow-up on the open thread with send; the daemon on bob's side
	// treats it as a message.
	code, out, stderr = alice.cli(t, "", "send", "bob", "--thread", threadID, "Thanks, one more thing: W?", "--yes")
	if code != 0 || out["type"] != "message" || out["hop"] != float64(3) {
		t.Fatalf("send = %d %v %s", code, out, stderr)
	}
	waitFor(t, "bob follow-up session", func() bool {
		n := 0
		for _, c := range bob.app.Snapshot() {
			if c.Op == "create" {
				n++
			}
		}
		return n == 2
	})

	// Thread listings agree on both sides.
	code, out, _ = alice.cli(t, "", "threads", "--open", "--peer", "bob")
	list, _ := out["threads"].([]any)
	if code != 0 || len(list) != 1 || list[0].(map[string]any)["direction"] != "out" {
		t.Fatalf("alice threads = %v", out)
	}
	code, out, _ = bob.cli(t, "", "threads", "--peer", "alice")
	list, _ = out["threads"].([]any)
	if code != 0 || len(list) != 1 || list[0].(map[string]any)["direction"] != "in" || list[0].(map[string]any)["mode"] != "auto" {
		t.Fatalf("bob threads = %v", out)
	}

	// An unknown handle and an unknown thread are not-found errors.
	if code, _, _ := alice.cli(t, "", "reply", "01NOTATHREAD", "x", "--yes"); code != cli.ExitNotFound {
		t.Fatalf("bad thread code %d", code)
	}
	if code, _, _ := alice.cli(t, "", "ask", "Not/Valid", "x", "--yes"); code != cli.ExitNotFound {
		t.Fatalf("bad handle code %d", code)
	}
}

const aliceYAML = `
handle: alice
bus: {kind: %s, url: %q, password_file: %q}
senders: [bob]
copilot: {workdir: /tmp/wh-e2e/alice}
daemon: {registry_interval: 1s}
`

const bobYAML = `
handle: bob
bus: {kind: %s, url: %q, password_file: %q}
senders: [{handle: alice, mode: auto}]
copilot: {workdir: /tmp/wh-e2e/bob, safe_tools: [view, grep]}
repos: {"owner/repo": /tmp/wh-e2e/repo}
daemon: {registry_interval: 1s}
`

func passwordFile(t *testing.T, user string) string {
	p := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(p, []byte("CHANGE_ME_"+user+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRoundTripMemoryBus(t *testing.T) {
	broker := memory.NewBroker()
	defer broker.Close()
	newBus := func(ctx context.Context, cfg *config.Config) (bus.Bus, error) { return broker.Connect(cfg.Handle) }
	alice := newSide(t, "alice", fmt.Sprintf(aliceYAML, "memory", "", passwordFile(t, "alice")), newBus)
	bob := newSide(t, "bob", fmt.Sprintf(bobYAML, "memory", "", passwordFile(t, "bob")), newBus)
	roundTrip(t, alice, bob)
}

func TestRoundTripNATS(t *testing.T) {
	url := startNATS(t)
	newBus := func(ctx context.Context, cfg *config.Config) (bus.Bus, error) {
		return natsbus.Connect(ctx, natsbus.Options{URL: cfg.Bus.URL, User: cfg.Handle, PasswordFile: cfg.Bus.PasswordFile, AckWait: 5 * time.Second})
	}
	alice := newSide(t, "alice", fmt.Sprintf(aliceYAML, "nats", url, passwordFile(t, "alice")), newBus)
	bob := newSide(t, "bob", fmt.Sprintf(bobYAML, "nats", url, passwordFile(t, "bob")), newBus)
	roundTrip(t, alice, bob)
}

func startNATS(t *testing.T) string {
	t.Helper()
	accounts, err := os.ReadFile(filepath.Join("..", "..", "deploy", "nats", "accounts.conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.conf"), accounts, 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "nats.conf")
	content := fmt.Sprintf("listen: 127.0.0.1:-1\nmax_payload: 64KB\njetstream { store_dir: %q }\ninclude ./accounts.conf\n", filepath.Join(dir, "js"))
	if err := os.WriteFile(conf, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := server.ProcessConfigFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	opts.NoLog, opts.NoSigs = true, true
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(srv.Shutdown)
	admin, err := natsbus.AdminConnect(srv.ClientURL(), "admin", "CHANGE_ME_admin", "")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := natsbus.EnsureInfrastructure(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	return srv.ClientURL()
}
