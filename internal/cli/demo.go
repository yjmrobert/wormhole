package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/bus/memory"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// `wormhole demo` is the round trip on one machine: alice and bob, two
// daemons in this process over the in-memory bus, a fake agent that
// answers bob's session, and the whole verb set, narrated. No server, no
// agent, no second person, nothing left behind.

const (
	demoPause   = 700 * time.Millisecond
	demoTimeout = 30 * time.Second
)

var demoThreadRE = regexp.MustCompile(`thread ([0-9A-HJKMNP-TV-Z]{26})`)

// demoStep is one line of the summary.
type demoStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// demoSide is one person's machine inside the demo.
type demoSide struct {
	handle  string
	cfgPath string
	dbPath  string
	cfg     *config.Config
	app     *ghapp.Recorder
	parent  *App
	broker  *memory.Broker
	session string
}

// cli runs one command as this side and returns its exit code, parsed
// JSON and the human output of a second run is not needed: callers print
// their own narration.
func (s *demoSide) cli(ctx context.Context, args ...string) (int, map[string]any, string) {
	var out, errOut bytes.Buffer
	app := &App{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut,
		IsTerminal: func() bool { return false },
		Getenv: func(k string) string {
			if k == SessionEnv {
				return s.session
			}
			return ""
		},
		NewBus:    func(_ context.Context, cfg *config.Config) (bus.Bus, error) { return s.broker.Connect(cfg.Handle) },
		NewGHApp:  func(*config.Config) ghapp.App { return s.app },
		OpenStore: func() (*threads.Store, error) { return threads.Open(s.dbPath) },
		Now:       s.parent.Now,
		Hostname:  s.handle + "-laptop",
		Version:   s.parent.Version,
	}
	code := app.Run(ctx, append([]string{args[0], "--json", "--config", s.cfgPath}, args[1:]...))
	var parsed map[string]any
	if strings.TrimSpace(out.String()) != "" {
		_ = json.Unmarshal(out.Bytes(), &parsed)
	}
	msg := errOut.String()
	if e, ok := parsed["error"].(string); ok {
		msg = e
	}
	return code, parsed, msg
}

// thread loads a thread from this side's store.
func (s *demoSide) thread(ctx context.Context, id string) (threads.Thread, error) {
	store, err := threads.Open(s.dbPath)
	if err != nil {
		return threads.Thread{}, err
	}
	defer store.Close()
	return store.Get(ctx, id)
}

// newDemoCommand runs the narrated round trip.
func (a *App) newDemoCommand(c *common) *cobra.Command {
	var fast bool
	var timeout time.Duration
	cmd := command("demo", "See the round trip on this machine: alice asks bob, bob answers, no server needed")
	cmd.Long = "Runs alice and bob in this process over the in-memory bus with a stand-in for bob's agent,\nand walks through ask, inbox, open, reply, snooze and decline. Nothing is installed and\nnothing is left behind."
	cmd.Flags().BoolVar(&fast, "fast", false, "no pauses between steps")
	cmd.Flags().DurationVar(&timeout, "timeout", demoTimeout, "give up after this long")
	cmd.Args = noArgs("wormhole demo [--fast]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		d := &demo{a: a, c: c, fast: fast}
		steps, err := d.run(ctx)
		ok := err == nil
		a.emit(c, map[string]any{"ok": ok, "steps": steps}, func(io.Writer) {})
		if err != nil {
			return wrap(ExitError, err)
		}
		return nil
	}
	return cmd
}

// demo holds the run.
type demo struct {
	a     *App
	c     *common
	fast  bool
	steps []demoStep
	alice *demoSide
	bob   *demoSide
}

// say narrates unless --json.
func (d *demo) say(format string, args ...any) {
	if d.c.json {
		return
	}
	fmt.Fprintf(d.a.Stdout, format+"\n", args...)
}

// typed shows a command as the person would type it.
func (d *demo) typed(who, cmdline string) {
	d.say("\n[%s] $ wormhole %s", who, cmdline)
}

// pause waits between steps so the narration can be read.
func (d *demo) pause(ctx context.Context) {
	if d.fast || d.c.json {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(demoPause):
	}
}

// step records an outcome and stops the demo on failure.
func (d *demo) step(name string, err error, detail string) error {
	st := demoStep{Name: name, OK: err == nil, Detail: detail}
	if err != nil {
		st.Detail = err.Error()
	}
	d.steps = append(d.steps, st)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// side sets up one person's temp config and state.
func (d *demo) side(root string, handle, yaml string, broker *memory.Broker) (*demoSide, error) {
	dir := filepath.Join(root, handle)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := config.WritePrivate(cfgPath, []byte(yaml)); err != nil {
		return nil, err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	return &demoSide{handle: handle, cfgPath: cfgPath, dbPath: filepath.Join(dir, "threads.db"), cfg: cfg, app: &ghapp.Recorder{}, parent: d.a, broker: broker, session: handle + "-session"}, nil
}

// startDaemon runs a side's daemon until ctx ends.
func (d *demo) startDaemon(ctx context.Context, s *demoSide) (func(), error) {
	store, err := threads.Open(s.dbPath)
	if err != nil {
		return nil, err
	}
	b, err := s.broker.Connect(s.handle)
	if err != nil {
		store.Close()
		return nil, err
	}
	dctx, cancel := context.WithCancel(ctx)
	dm := daemon.New(daemon.Options{Config: s.cfg, Bus: b, Store: store, App: s.app, Host: s.handle + "-laptop", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Paused: func() bool { return false }, Now: d.a.Now})
	done := make(chan struct{})
	go func() { _ = dm.Run(dctx); close(done) }()
	return func() { cancel(); <-done; b.Close(); store.Close() }, nil
}

// waitFor polls until cond holds or ctx ends.
func waitFor(ctx context.Context, cond func() bool) error {
	for {
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

const demoAliceYAML = `handle: alice
bus: {kind: memory}
senders: [bob]
copilot: {workdir: %q}
daemon: {registry_interval: 1s, notify: toast}
`

const demoBobYAML = `handle: bob
bus: {kind: memory}
senders: [alice]
copilot: {workdir: %q}
daemon: {registry_interval: 1s, notify: toast}
`

// run is the script.
func (d *demo) run(ctx context.Context) ([]demoStep, error) {
	root, err := os.MkdirTemp("", "wormhole-demo-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	broker := memory.NewBroker()
	defer broker.Close()
	if d.alice, err = d.side(root, "alice", fmt.Sprintf(demoAliceYAML, root), broker); err != nil {
		return d.steps, d.step("setup", err, "")
	}
	if d.bob, err = d.side(root, "bob", fmt.Sprintf(demoBobYAML, root), broker); err != nil {
		return d.steps, d.step("setup", err, "")
	}
	// bob's stand-in agent: when his session starts with alice's question,
	// it answers through the CLI the way a real agent would.
	d.bob.app.Fail = func(op string) error {
		if op != "create" {
			return nil
		}
		calls := d.bob.app.Snapshot()
		m := demoThreadRE.FindStringSubmatch(calls[len(calls)-1].Session.Kickoff)
		if m == nil {
			return nil
		}
		d.bob.session = calls[len(calls)-1].Session.ID
		go func() {
			_, _, _ = d.bob.cli(ctx, "reply", m[1], "Looks sane to me: the backoff is capped and the jitter keeps retries apart. Ship it.", "--yes")
		}()
		return nil
	}
	stopAlice, err := d.startDaemon(ctx, d.alice)
	if err != nil {
		return d.steps, d.step("setup", err, "")
	}
	defer stopAlice()
	stopBob, err := d.startDaemon(ctx, d.bob)
	if err != nil {
		return d.steps, d.step("setup", err, "")
	}
	defer stopBob()
	d.say("Two people on one machine: alice and bob, each with a daemon, over an in-memory bus.")
	d.say("bob runs inbox mode for alice, alice runs inbox mode for bob: nothing runs until a person reads it.")
	if err := d.step("setup", waitFor(ctx, func() bool {
		code, out, _ := d.alice.cli(ctx, "users")
		users, _ := out["users"].([]any)
		return code == 0 && len(users) == 2
	}), "both daemons registered"); err != nil {
		return d.steps, err
	}
	d.typed("alice", `users`)
	d.say("  ^bob   bob-laptop   online")
	d.pause(ctx)

	// 1. alice asks bob.
	d.typed("alice", `ask ^bob "is the retry policy in PR #123 sane?" --repo owner/repo --context "PR #123"`)
	code, out, msg := d.alice.cli(ctx, "ask", "bob", "Is the retry policy in PR #123 sane? Capped exponential backoff, 5 attempts, jitter.", "--repo", "owner/repo", "--context", "PR #123", "--yes")
	threadID, _ := out["thread_id"].(string)
	if code != 0 || threadID == "" {
		return d.steps, d.step("ask", fmt.Errorf("exit %d: %s", code, msg), "")
	}
	d.say("  ask sent to ^bob on thread %s (you would type it as ^bob from now on)", threadID)
	_ = d.step("ask", nil, "thread "+threadID)
	if err := d.step("delivered", waitFor(ctx, func() bool {
		t, err := d.bob.thread(ctx, threadID)
		return err == nil && t.StagedKickoff != ""
	}), "bob's daemon staged it and sent a receipt"); err != nil {
		return d.steps, err
	}
	d.say("  bob's daemon staged it in his inbox and sent alice a delivery receipt; bob got a notification.")
	d.pause(ctx)

	// 2. bob reads it and opens it.
	d.typed("bob", "inbox")
	code, out, msg = d.bob.cli(ctx, "inbox")
	items, _ := out["inbox"].([]any)
	if code != 0 || len(items) != 1 {
		return d.steps, d.step("inbox", fmt.Errorf("exit %d, %d items: %s", code, len(items), msg), "")
	}
	it := items[0].(map[string]any)
	d.say("  REF     THREAD                      FROM    KIND  PREVIEW")
	d.say("  %-7s %s  ^alice  ask   %s", it["ref"], threadID, it["preview"])
	_ = d.step("inbox", nil, "one ask waiting")
	d.pause(ctx)
	d.typed("bob", "open ^alice --no-start")
	code, _, msg = d.bob.cli(ctx, "open", threadID, "--no-start")
	if code != 0 {
		return d.steps, d.step("read", fmt.Errorf("exit %d: %s", code, msg), "")
	}
	t, _ := d.bob.thread(ctx, threadID)
	for _, line := range strings.Split(t.StagedKickoff, "\n") {
		d.say("  | %s", line)
	}
	_ = d.step("read", nil, "the full kickoff, nothing ran")
	d.pause(ctx)
	d.typed("bob", "open ^alice --yes")
	code, out, msg = d.bob.cli(ctx, "open", threadID, "--yes")
	if code != 0 {
		return d.steps, d.step("open", fmt.Errorf("exit %d: %s", code, msg), "")
	}
	d.say("  started session %s with bob's normal tools; his agent reads the question and answers with: wormhole reply", out["session_id"])
	_ = d.step("open", nil, "session started")

	// 3. the reply lands in alice's inbox.
	if err := d.step("reply", waitFor(ctx, func() bool {
		t, err := d.alice.thread(ctx, threadID)
		return err == nil && t.State == threads.StateReplied && t.StagedKickoff != ""
	}), "bob's agent replied; alice's daemon staged it"); err != nil {
		return d.steps, err
	}
	d.say("  bob's agent ran: wormhole reply %s \"Looks sane to me...\" --yes", threadID)
	d.say("  alice's daemon received it; she runs inbox mode for bob, so it waits for her.")
	d.pause(ctx)
	d.typed("alice", "threads")
	code, out, _ = d.alice.cli(ctx, "threads")
	list, _ := out["threads"].([]any)
	if code == 0 && len(list) == 1 {
		row := list[0].(map[string]any)
		d.say("  REF   THREAD                      PEER    STATE    PREVIEW")
		d.say("  %-5s %s  -> ^bob  %-8s %s", row["ref"], threadID, row["state"], row["last_preview"])
	}
	d.typed("alice", "open ^bob --yes")
	code, out, msg = d.alice.cli(ctx, "open", threadID, "--yes")
	if code != 0 {
		return d.steps, d.step("open reply", fmt.Errorf("exit %d: %s", code, msg), "")
	}
	if out["resumed"] == true {
		d.say("  the reply is appended to the session that asked (%s) and shown in the app.", out["session_id"])
	} else {
		d.say("  a session named \"Reply from ^bob\" (%s) starts with the answer and is shown in the app", out["session_id"])
		d.say("  (daemon.reply_in_place: true appends it to the session that asked instead).")
	}
	_ = d.step("open reply", nil, "reply appended to the asking session")
	d.pause(ctx)

	// 4. the other verbs: bob asks alice, alice snoozes, then declines.
	d.typed("bob", `ask ^alice "can you look at the flaky e2e on main?"`)
	code, out, msg = d.bob.cli(ctx, "ask", "alice", "Can you look at the flaky e2e on main?", "--yes")
	second, _ := out["thread_id"].(string)
	if code != 0 || second == "" {
		return d.steps, d.step("second ask", fmt.Errorf("exit %d: %s", code, msg), "")
	}
	if err := d.step("second ask", waitFor(ctx, func() bool {
		t, err := d.alice.thread(ctx, second)
		return err == nil && t.StagedKickoff != ""
	}), "staged in alice's inbox"); err != nil {
		return d.steps, err
	}
	d.say("  staged in alice's inbox as ^bob")
	d.typed("alice", "snooze ^bob 2h")
	code, out, msg = d.alice.cli(ctx, "snooze", second, "2h")
	if code != 0 {
		return d.steps, d.step("snooze", fmt.Errorf("exit %d: %s", code, msg), "")
	}
	d.say("  hidden until %s; the daemon reminds her once when it comes due", out["snoozed_until"])
	_ = d.step("snooze", nil, "snoozed")
	d.typed("alice", "snooze ^bob off")
	if code, _, msg = d.alice.cli(ctx, "snooze", second, "off"); code != 0 {
		return d.steps, d.step("unsnooze", fmt.Errorf("exit %d: %s", code, msg), "")
	}
	d.say("  back in the inbox")
	_ = d.step("unsnooze", nil, "back in the inbox")
	d.pause(ctx)
	d.typed("alice", `decline ^bob "taking it to standup" --yes`)
	if code, _, msg = d.alice.cli(ctx, "decline", second, "taking it to standup", "--yes"); code != 0 {
		return d.steps, d.step("decline", fmt.Errorf("exit %d: %s", code, msg), "")
	}
	if err := d.step("decline", waitFor(ctx, func() bool {
		t, err := d.bob.thread(ctx, second)
		return err == nil && t.State == threads.StateDeclined
	}), "bob's thread shows declined with the reason"); err != nil {
		return d.steps, err
	}
	t, _ = d.bob.thread(ctx, second)
	d.say("  bob's thread now reads: %s", t.LastPreview)
	d.pause(ctx)

	d.say("\nThat was the round trip on one machine. On a real network:")
	d.say("  wormhole serve install --invite bob    # on one machine: the bus, and an invite for bob")
	d.say("  wormhole join <invite>                 # on bob's machine")
	d.say("  wormhole ask ^bob \"...\"                 # from any session with the skill installed")
	return d.steps, nil
}
