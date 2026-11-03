package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/bus/memory"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

const testThreadID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

var testNow = time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)

type cliFixture struct {
	app      *App
	out      *bytes.Buffer
	errOut   *bytes.Buffer
	cfg      *config.Config
	cfgPath  string
	dbPath   string
	broker   *memory.Broker
	bus      bus.Bus
	recorder *ghapp.Recorder
}

// newCLIFixture returns an isolated command environment backed by a real
// thread store, an in-memory bus, and a recording Copilot app.
func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WORMHOLE_STATE_DIR", filepath.Join(dir, "state"))
	cfgPath := filepath.Join(dir, "config.yaml")
	raw := fmt.Sprintf("handle: bob\nbus: {kind: memory}\nsenders: [alice]\ncopilot:\n  workdir: %s\n  safe_tools: [shell]\ndaemon:\n  registry_interval: 1m\n", dir)
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	broker := memory.NewBroker()
	t.Cleanup(broker.Close)
	b, err := broker.Connect("bob")
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	recorder := &ghapp.Recorder{}
	dbPath := filepath.Join(dir, "threads.db")
	f := &cliFixture{out: out, errOut: errOut, cfg: cfg, cfgPath: cfgPath, dbPath: dbPath, broker: broker, bus: b, recorder: recorder}
	f.app = &App{
		Stdin:      strings.NewReader(""),
		Stdout:     out,
		Stderr:     errOut,
		IsTerminal: func() bool { return false },
		Getenv:     func(string) string { return "" },
		NewBus:     func(context.Context, *config.Config) (bus.Bus, error) { return b, nil },
		NewGHApp:   func(*config.Config) ghapp.App { return recorder },
		OpenStore:  func() (*threads.Store, error) { return threads.Open(dbPath) },
		Now:        func() time.Time { return testNow },
		Hostname:   "test-host",
		Version:    "1.2.3",
	}
	return f
}

// run executes a command against the fixture's fresh root command.
func (f *cliFixture) run(args ...string) int {
	return f.app.Run(context.Background(), append(args, "--config", f.cfgPath))
}

// seedThread stores an inbound ask and its staged thread for inbox tests.
func (f *cliFixture) seedThread(t *testing.T, mutate func(*threads.Thread)) threads.Thread {
	t.Helper()
	store, err := threads.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ask := envelope.NewAsk(envelope.Peer{User: "alice", Host: "alice-host"}, "bob", "Please review this change", testNow.Add(-3*time.Minute))
	ask.ID = testThreadID
	ask.ThreadID = testThreadID
	thread := threads.Thread{
		ID: testThreadID, Peer: "alice", Direction: threads.DirectionIn,
		State: threads.StatePending, Title: "Review request", Repo: "owner/repo",
		StagedKickoff: "Please review this change", LastPreview: "Please review this change",
		LastMessageAt: ask.CreatedAt, CreatedAt: ask.CreatedAt, ExpiresAt: ask.ExpiresAt,
	}
	if mutate != nil {
		mutate(&thread)
	}
	if err := store.Put(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(context.Background(), ask, threads.DirectionIn); err != nil {
		t.Fatal(err)
	}
	return thread
}

// openFixtureStore opens the fixture database for direct assertions.
func (f *cliFixture) openFixtureStore(t *testing.T) *threads.Store {
	t.Helper()
	store, err := threads.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type stubRegistry struct {
	presences []bus.Presence
	err       error
}

// Put records no state; it exists to satisfy bus.Registry in focused tests.
func (r *stubRegistry) Put(context.Context, bus.Presence) error { return nil }

// List returns the configured registry response.
func (r *stubRegistry) List(context.Context) ([]bus.Presence, error) {
	return r.presences, r.err
}

type stubBus struct {
	bus.Bus
	registry   bus.Registry
	publishErr error
	published  []envelope.Envelope
}

// User identifies the stub connection as bob.
func (b *stubBus) User() string { return "bob" }

// Publish records envelopes or returns the configured publication error.
func (b *stubBus) Publish(_ context.Context, _ string, e envelope.Envelope) error {
	if b.publishErr != nil {
		return b.publishErr
	}
	b.published = append(b.published, e)
	return nil
}

// Registry returns the stub registry, falling back to the embedded bus.
func (b *stubBus) Registry() bus.Registry {
	if b.registry != nil {
		return b.registry
	}
	return b.Bus.Registry()
}

// Close satisfies bus.Bus without closing the shared in-memory connection.
func (b *stubBus) Close() error { return nil }

type failingReader struct{ err error }

// Read returns the configured error to exercise input failures.
func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// TestCoreHelpers covers error wrapping, text input, formatting, and small
// shared command helpers.
func TestCoreHelpers(t *testing.T) {
	if err := wrap(ExitError, nil); err != nil {
		t.Fatal(err)
	}
	plain := errors.New("plain")
	if got := wrap(ExitUnavailable, plain); got.(*exitError).code != ExitUnavailable || !errors.Is(got, plain) {
		t.Fatalf("wrap = %v", got)
	}
	marked := fail(ExitRefused, "stop")
	if got := wrap(ExitError, marked); got != marked {
		t.Fatal("wrap replaced an existing exit error")
	}

	f := newCLIFixture(t)
	cfg, err := f.app.loadConfig(&common{config: f.cfgPath})
	if err != nil || cfg.Handle != "bob" {
		t.Fatalf("load config = %+v, %v", cfg, err)
	}
	if _, err := f.app.loadConfig(&common{config: filepath.Join(t.TempDir(), "missing")}); err.(*exitError).code != ExitError {
		t.Fatalf("missing config error = %v", err)
	}
	badPath := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(badPath, []byte("not: valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.loadConfig(&common{config: badPath}); err == nil {
		t.Fatal("invalid config loaded")
	}

	if got, err := f.app.readText("literal"); err != nil || got != "literal" {
		t.Fatalf("literal = %q, %v", got, err)
	}
	f.app.Stdin = strings.NewReader("line one\nline two\n")
	if got, err := f.app.readText("-"); err != nil || got != "line one\nline two" {
		t.Fatalf("stdin = %q, %v", got, err)
	}
	f.app.Stdin = failingReader{err: plain}
	if _, err := f.app.readText("-"); !errors.Is(err, plain) {
		t.Fatalf("read error = %v", err)
	}

	for _, tc := range []struct {
		at   time.Time
		want string
	}{{time.Time{}, "-"}, {testNow.Add(-5 * time.Second), "5s ago"}, {testNow.Add(-5 * time.Minute), "5m ago"}, {testNow.Add(-5 * time.Hour), "5h ago"}, {testNow.Add(-72 * time.Hour), "3d ago"}} {
		if got := ago(tc.at, testNow); got != tc.want {
			t.Errorf("ago(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
	var tableOut bytes.Buffer
	table(&tableOut, []string{"A", "B"}, [][]string{{"one", "two"}})
	if !strings.Contains(tableOut.String(), "one") || !strings.Contains(tableOut.String(), "A") {
		t.Fatalf("table = %q", tableOut.String())
	}
	if got := sortedKeys(map[string]int{"z": 1, "a": 2}); strings.Join(got, ",") != "a,z" {
		t.Fatalf("keys = %v", got)
	}
	for _, tc := range []struct {
		args []string
		want bool
	}{{[]string{"--json"}, true}, {[]string{"--json=false"}, false}, {[]string{"--json=bad"}, true}, {[]string{"--", "--json"}, false}, {nil, false}} {
		if got := jsonRequested(tc.args); got != tc.want {
			t.Errorf("jsonRequested(%v) = %v", tc.args, got)
		}
	}

	var version bytes.Buffer
	app := &App{Version: "v1"}
	app.printVersion(&version)
	if !strings.Contains(version.String(), "commit: unknown") || !strings.Contains(version.String(), "build date: unknown") {
		t.Fatalf("version = %q", version.String())
	}
	if code := app.Run(context.Background(), nil); code != ExitOK {
		t.Fatalf("root help code = %d", code)
	}
}

// TestDefaultApp verifies environment wiring without starting external
// services.
func TestDefaultApp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WORMHOLE_STATE_DIR", dir)
	app := Default()
	if app.Stdin != os.Stdin || app.Stdout != os.Stdout || app.Stderr != os.Stderr || app.Now == nil || app.Hostname == "" {
		t.Fatalf("default app = %+v", app)
	}
	if app.Getenv("PATH") == "" || app.commit() != "unknown" || app.buildDate() != "unknown" {
		t.Fatal("default metadata or environment is wrong")
	}
	if _, err := app.NewBus(context.Background(), &config.Config{Bus: config.Bus{Kind: "memory"}}); err == nil {
		t.Fatal("default bus accepted an in-process kind")
	}
	if app.NewGHApp(&config.Config{}) == nil {
		t.Fatal("default app wrapper is nil")
	}
	store, err := app.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
}

// TestBuiltinCommands covers shell completion, command documentation, and man
// page output beyond the existing JSON contract tests.
func TestBuiltinCommands(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var out, errOut bytes.Buffer
			app := contractApp(&out, &errOut)
			if code := app.Run(context.Background(), []string{"completion", shell}); code != ExitOK || out.Len() == 0 {
				t.Fatalf("code %d output %d stderr %q", code, out.Len(), errOut.String())
			}
		})
	}
	var out, errOut bytes.Buffer
	app := contractApp(&out, &errOut)
	if code := app.Run(context.Background(), []string{"completion", "powershell"}); code != ExitUsage {
		t.Fatalf("unsupported shell code = %d", code)
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"commands", "version", "--flags"}); code != ExitOK || !strings.Contains(out.String(), "wormhole version") {
		t.Fatalf("human commands code %d output %q", code, out.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"commands", "missing", "--json"}); code != ExitNotFound {
		t.Fatalf("unknown path code = %d", code)
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"man"}); code != ExitOK || !strings.Contains(out.String(), "WORMHOLE") {
		t.Fatalf("man code %d output %q", code, out.String())
	}

	cmd := command("sample", "Sample command")
	cmd.Flags().BoolP("verbose", "v", false, "verbose output")
	info := describeCommand(cmd, false, true)
	out.Reset()
	printCommandInfo(&out, info, true, 1)
	if !strings.Contains(out.String(), "-v, --verbose") {
		t.Fatalf("printed command = %q", out.String())
	}
	cmd.Flags().Bool("hidden", false, "hidden")
	_ = cmd.Flags().MarkHidden("hidden")
	info = describeCommand(cmd, false, true)
	for _, flag := range info.Flags {
		if flag.Name == "hidden" {
			t.Fatal("hidden flag was documented")
		}
	}
}

// TestArgumentValidators checks exact and ranged positional validation.
func TestArgumentValidators(t *testing.T) {
	if err := exactArgs(1, "one")(nil, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := exactArgs(1, "one")(nil, nil); err.(*exitError).code != ExitUsage {
		t.Fatalf("exact error = %v", err)
	}
	validate := rangeArgs(1, 2, "one or two")
	for _, args := range [][]string{{"a"}, {"a", "b"}} {
		if err := validate(nil, args); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{nil, {"a", "b", "c"}} {
		if err := validate(nil, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// TestCompletionHandlesAndFailures covers registry completion and graceful
// fallbacks when dependencies fail.
func TestCompletionHandlesAndFailures(t *testing.T) {
	f := newCLIFixture(t)
	registry := &stubRegistry{presences: []bus.Presence{{Handle: "alice", Host: "one"}, {Handle: "alex", Host: "two"}}}
	f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) {
		return &stubBus{Bus: f.bus, registry: registry}, nil
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	values, directive := f.app.completeHandles(&common{config: f.cfgPath})(cmd, nil, "ali")
	if directive != cobra.ShellCompDirectiveNoFileComp || len(values) != 1 || values[0] != "alice\tone" {
		t.Fatalf("values %v directive %v", values, directive)
	}
	if values, _ := f.app.completeHandles(&common{})(cmd, []string{"used"}, ""); values != nil {
		t.Fatal("completed after positional argument")
	}
	f.app.NewBus = nil
	if values, _ := f.app.completeHandles(&common{})(cmd, nil, ""); values != nil {
		t.Fatal("completed without bus factory")
	}

	f = newCLIFixture(t)
	f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) { return nil, errors.New("offline") }
	if values, _ := f.app.completeHandles(&common{config: f.cfgPath})(cmd, nil, ""); values != nil {
		t.Fatal("completed with failed connection")
	}
	f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) {
		return &stubBus{Bus: f.bus, registry: &stubRegistry{err: errors.New("denied")}}, nil
	}
	if values, _ := f.app.completeHandles(&common{config: f.cfgPath})(cmd, nil, ""); values != nil {
		t.Fatal("completed with failed registry")
	}
	if values, _ := f.app.completeHandles(&common{config: filepath.Join(t.TempDir(), "missing")})(cmd, nil, ""); values != nil {
		t.Fatal("completed with missing config")
	}

	storePath := filepath.Join(t.TempDir(), "threads.db")
	f.app.OpenStore = func() (*threads.Store, error) { return nil, errors.New("locked") }
	if values, _ := f.app.completeThreadIDs(&common{}, false)(cmd, nil, ""); values != nil {
		t.Fatal("completed with failed store")
	}
	f.app.OpenStore = func() (*threads.Store, error) { return threads.Open(storePath) }
	if values, _ := f.app.completeThreadIDs(&common{}, false)(cmd, []string{"used"}, ""); values != nil {
		t.Fatal("completed thread after positional argument")
	}
	f.app.OpenStore = nil
	if values, _ := f.app.completeThreadIDs(&common{}, false)(cmd, nil, ""); values != nil {
		t.Fatal("completed without store factory")
	}
}

// TestGuardReviewAndTerminalInput covers outbound safety checks and the
// non-terminal confirmation and password fallbacks.
func TestGuardReviewAndTerminalInput(t *testing.T) {
	f := newCLIFixture(t)
	if err := f.app.guardBody("  \n", false); err.(*exitError).code != ExitUsage {
		t.Fatalf("empty body error = %v", err)
	}
	if err := f.app.guardBody(strings.Repeat("x", envelope.MaxBodyBytes+1), false); err.(*exitError).code != ExitRefused {
		t.Fatalf("large body error = %v", err)
	}
	secret := "use password=hunter22 to log in"
	if err := f.app.guardBody(secret, false); err.(*exitError).code != ExitRefused {
		t.Fatalf("secret error = %v", err)
	}
	if err := f.app.guardBody(secret, true); err != nil || !strings.Contains(f.errOut.String(), "warning") {
		t.Fatalf("forced secret = %v, stderr %q", err, f.errOut.String())
	}
	if err := f.app.guardBody("ordinary text", false); err != nil {
		t.Fatal(err)
	}
	if peer := f.app.me(f.cfg); peer.User != "bob" || peer.Host != "test-host" {
		t.Fatalf("peer = %+v", peer)
	}

	e := envelope.NewAsk(envelope.Peer{User: "bob"}, "alice", "hello", testNow)
	if reviewed, err := f.app.review(&common{}, e, true); err != nil || reviewed {
		t.Fatalf("--yes review = %v, %v", reviewed, err)
	}
	if _, err := f.app.review(&common{}, e, false); err.(*exitError).code != ExitRefused {
		t.Fatalf("nonterminal review = %v", err)
	}
	f.app.IsTerminal = func() bool { return true }
	f.app.Stdin = strings.NewReader("yes\n")
	if reviewed, err := f.app.review(&common{}, e, false); err != nil || !reviewed {
		t.Fatalf("confirmed review = %v, %v", reviewed, err)
	}
	f.app.Stdin = strings.NewReader("no\n")
	if _, err := f.app.review(&common{}, e, false); err.(*exitError).code != ExitRefused {
		t.Fatalf("declined review = %v", err)
	}
	f.app.Stdin = failingReader{err: errors.New("read failed")}
	if _, err := f.app.confirm("Continue?"); err == nil {
		t.Fatal("confirmation ignored read failure")
	}

	f.app.Stdin = strings.NewReader(" secret \n")
	value, err := f.app.readSecret(&common{}, "Password: ")
	if err != nil || value != "secret" {
		t.Fatalf("secret = %q, %v", value, err)
	}
	f.app.Stdin = failingReader{err: errors.New("read failed")}
	if _, err := f.app.readSecret(&common{}, "Password: "); err == nil {
		t.Fatal("password ignored read failure")
	}
	file, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if isTerminal(file) {
		t.Fatal("regular file reported as terminal")
	}
	t.Setenv("NO_COLOR", "1")
	if interactiveForm(strings.NewReader(""), io.Discard, huh.NewInput()) == nil {
		t.Fatal("interactive form is nil")
	}
}
