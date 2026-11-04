package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/bus/memory"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

const (
	demoPause       = 1200 * time.Millisecond
	demoTimeout     = 30 * time.Second
	demoAsk         = "Does the retry loop in fetch() back off, or does it hammer the server?"
	demoAnswer      = "It backs off: 200ms doubling to 5s, with jitter. See retry.go."
	demoSecondAsk   = "Can you take the release notes this week?"
	demoDeclineText = "out this week; try again Monday"
)

// demoStep is one step of the demo's JSON summary.
type demoStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// demoSide is one person's machine inside the demo: a config, a thread
// store, a daemon and the CLI.
type demoSide struct {
	handle  string
	cfgPath string
	dbPath  string
	cfg     *config.Config
	app     ghapp.App
}

// demoAgent is the demo's Copilot app: it records calls like the test
// recorder and, when a session is created, plays the agent in it.
type demoAgent struct {
	*ghapp.Recorder
	onCreate func(ghapp.Session)
}

// Create records the session and starts the pretend agent.
func (g *demoAgent) Create(ctx context.Context, s ghapp.Session) error {
	if err := g.Recorder.Create(ctx, s); err != nil {
		return err
	}
	if g.onCreate != nil {
		g.onCreate(s)
	}
	return nil
}

// demo is one run of `wormhole demo`.
type demo struct {
	a       *App
	w       io.Writer
	pause   time.Duration
	timeout time.Duration
	root    string
	broker  *memory.Broker
	alice   *demoSide
	bob     *demoSide
	steps   []demoStep

	// The pretend agent's work, reported back to the narrator.
	agentMu  sync.Mutex
	agentOut string
	agentErr error
	agentWG  sync.WaitGroup
}

// newDemoCommand runs alice and bob on this machine over the memory bus.
func (a *App) newDemoCommand(c *common) *cobra.Command {
	var fast bool
	var timeout time.Duration
	cmd := command("demo", "Watch alice and bob trade an ask on this machine, no server needed")
	cmd.Long = "Runs two people, alice and bob, in this process over an in-memory bus: an ask, the inbox, open, the agent's reply, snooze and decline, each shown with the command that does it. Nothing is written outside a temporary directory, which is removed at the end."
	cmd.Flags().BoolVar(&fast, "fast", false, "no pauses between steps")
	cmd.Flags().DurationVar(&timeout, "timeout", demoTimeout, "how long one step may take")
	cmd.Args = noArgs("wormhole demo [--fast] [--timeout <duration>]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if timeout <= 0 {
			return fail(ExitUsage, "--timeout must be positive, such as --timeout 30s")
		}
		d := &demo{a: a, w: a.Stdout, pause: demoPause, timeout: timeout}
		if fast || c.json {
			d.pause = 0
		}
		if c.json {
			d.w = io.Discard
		}
		if err := d.run(cmd.Context()); err != nil {
			return err
		}
		a.emit(c, map[string]any{"ok": true, "steps": d.steps}, func(io.Writer) {})
		return nil
	}
	return cmd
}

// run sets up both machines, plays the script and tears everything down.
func (d *demo) run(parent context.Context) (err error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	d.root, err = os.MkdirTemp("", "wormhole-demo-")
	if err != nil {
		return fail(ExitError, "demo: %v; check that $TMPDIR is writable", err)
	}
	defer os.RemoveAll(d.root)
	d.broker = memory.NewBroker()
	defer d.broker.Close()

	stop, err := d.setUp(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		stop()
		d.agentWG.Wait()
	}()

	fmt.Fprintf(d.w, "wormhole demo: alice and bob on this machine, over an in-memory bus.\nNothing leaves this process; the Copilot app is played by the demo.\n")
	for _, s := range []struct {
		name string
		fn   func(context.Context) (string, error)
	}{
		{"start", d.stepStart},
		{"ask", d.stepAsk},
		{"inbox", d.stepInbox},
		{"open", d.stepOpen},
		{"reply", d.stepReply},
		{"snooze", d.stepSnooze},
		{"decline", d.stepDecline},
	} {
		stepCtx, stepCancel := context.WithTimeout(ctx, d.timeout)
		detail, err := s.fn(stepCtx)
		timedOut := errors.Is(stepCtx.Err(), context.DeadlineExceeded)
		stepCancel()
		if err != nil {
			if timedOut || errors.Is(err, context.DeadlineExceeded) {
				return fail(ExitError, "demo timed out at step %s after %s; rerun with a longer timeout: wormhole demo --timeout 60s", s.name, d.timeout)
			}
			return fail(ExitError, "demo step %s failed: %v; rerun it to see the narration: wormhole demo", s.name, err)
		}
		d.steps = append(d.steps, demoStep{Name: s.name, OK: true, Detail: detail})
		if err := d.sleep(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintf(d.w, "\nThat was the round trip on one machine. On a real network:\n  wormhole serve              on one machine\n  wormhole join <invite>      on the others\n  wormhole ask ^<handle> \"...\"\n")
	return nil
}

// setUp writes both configs and starts both daemons. stop shuts them down.
func (d *demo) setUp(ctx context.Context) (stop func(), err error) {
	d.alice, err = d.newSide("alice", "bob")
	if err != nil {
		return nil, err
	}
	d.bob, err = d.newSide("bob", "alice")
	if err != nil {
		return nil, err
	}
	d.alice.app = &demoAgent{Recorder: &ghapp.Recorder{}}
	d.bob.app = &demoAgent{Recorder: &ghapp.Recorder{}, onCreate: d.bobAnswers(ctx)}

	var stops []func()
	stop = func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
	for _, s := range []*demoSide{d.alice, d.bob} {
		fn, err := d.startDaemon(ctx, s)
		if err != nil {
			stop()
			return nil, err
		}
		stops = append(stops, fn)
	}
	return stop, nil
}

// newSide writes one person's config and returns their machine.
func (d *demo) newSide(handle, peer string) (*demoSide, error) {
	home := filepath.Join(d.root, handle)
	for _, sub := range []string{"config", "state", "work"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o700); err != nil {
			return nil, fail(ExitError, "demo: %v; check that $TMPDIR is writable", err)
		}
	}
	cfgPath := filepath.Join(home, "config", "config.yaml")
	raw := fmt.Sprintf("handle: %s\nbus: {kind: memory}\nsenders: [%s]\ncopilot: {workdir: %q}\ndaemon: {registry_interval: 1s}\n", handle, peer, filepath.Join(home, "work"))
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		return nil, fail(ExitError, "demo: %v; check that $TMPDIR is writable", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fail(ExitError, "demo config: %v", err)
	}
	return &demoSide{handle: handle, cfgPath: cfgPath, dbPath: filepath.Join(home, "state", "threads.db"), cfg: cfg}, nil
}

// startDaemon runs s's daemon until ctx ends; the returned func waits for
// it and releases its bus and store.
func (d *demo) startDaemon(ctx context.Context, s *demoSide) (func(), error) {
	store, err := threads.Open(s.dbPath)
	if err != nil {
		return nil, err
	}
	b, err := d.broker.Connect(s.handle)
	if err != nil {
		store.Close()
		return nil, err
	}
	dctx, cancel := context.WithCancel(ctx)
	dm := daemon.New(daemon.Options{
		Config: s.cfg, Bus: b, Store: store, App: s.app, Host: s.handle + "-laptop",
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Paused: func() bool { return false },
	})
	done := make(chan struct{})
	go func() { _ = dm.Run(dctx); close(done) }()
	return func() { cancel(); <-done; b.Close(); store.Close() }, nil
}

// cli runs a wormhole command as s, inside session (empty: a terminal),
// and returns its exit code and output.
func (d *demo) cli(ctx context.Context, s *demoSide, session string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	app := &App{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut,
		IsTerminal: func() bool { return false },
		Getenv: func(k string) string {
			if k == SessionEnv {
				return session
			}
			return ""
		},
		NewBus:    func(context.Context, *config.Config) (bus.Bus, error) { return d.broker.Connect(s.handle) },
		NewGHApp:  func(*config.Config) ghapp.App { return s.app },
		OpenStore: func() (*threads.Store, error) { return threads.Open(s.dbPath) },
		Now:       time.Now,
		Hostname:  s.handle + "-laptop",
		Version:   d.a.Version,
	}
	code := app.Run(ctx, append(append([]string{}, args...), "--config", s.cfgPath))
	return code, out.String(), errOut.String()
}

// show runs a command as s and prints it with its output, the way the
// person would see it in their terminal.
func (d *demo) show(ctx context.Context, s *demoSide, args ...string) (string, error) {
	fmt.Fprintf(d.w, "\n  %s$ wormhole %s\n", s.handle, shellJoin(args))
	code, out, errOut := d.cli(ctx, s, "", args...)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" {
			fmt.Fprintf(d.w, "    %s\n", line)
		}
	}
	if code != ExitOK {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		return out, fmt.Errorf("%s as %s exited %d: %s", shellJoin(args), s.handle, code, strings.TrimSpace(errOut))
	}
	return out, nil
}

// say prints one line of narration.
func (d *demo) say(format string, args ...any) {
	fmt.Fprintf(d.w, format+"\n", args...)
}

// sleep pauses between steps so a person can read along.
func (d *demo) sleep(ctx context.Context) error {
	if d.pause == 0 {
		return nil
	}
	t := time.NewTimer(d.pause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitFor polls cond until it holds or ctx ends.
func waitFor(ctx context.Context, cond func() (bool, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// thread reads one thread from s's store; a missing thread is the zero
// value.
func (s *demoSide) thread(ctx context.Context, id string) (threads.Thread, error) {
	store, err := threads.Open(s.dbPath)
	if err != nil {
		return threads.Thread{}, err
	}
	defer store.Close()
	t, err := store.Get(ctx, id)
	if err != nil {
		return threads.Thread{}, nil
	}
	return t, nil
}

// newest returns s's most recent thread in direction dir with peer.
func (s *demoSide) newest(ctx context.Context, dir threads.Direction, peer string) (threads.Thread, error) {
	store, err := threads.Open(s.dbPath)
	if err != nil {
		return threads.Thread{}, err
	}
	defer store.Close()
	list, err := store.List(ctx, threads.Filter{Direction: dir, Peer: peer})
	if err != nil {
		return threads.Thread{}, err
	}
	var best threads.Thread
	for _, t := range list {
		if t.CreatedAt.After(best.CreatedAt) || best.ID == "" {
			best = t
		}
	}
	return best, nil
}

// bobAnswers plays bob's agent: once the session the open created is tied
// to its thread, it answers through the CLI from inside that session, the
// way the skill tells a real agent to.
func (d *demo) bobAnswers(ctx context.Context) func(ghapp.Session) {
	return func(s ghapp.Session) {
		d.agentWG.Add(1)
		go func() {
			defer d.agentWG.Done()
			var id string
			err := waitFor(ctx, func() (bool, error) {
				store, err := threads.Open(d.bob.dbPath)
				if err != nil {
					return false, err
				}
				defer store.Close()
				list, err := store.List(ctx, threads.Filter{})
				if err != nil {
					return false, err
				}
				for _, t := range list {
					if t.LocalSessionID == s.ID {
						id = t.ID
						return true, nil
					}
				}
				return false, nil
			})
			var out string
			if err == nil {
				var code int
				var errOut string
				code, out, errOut = d.cli(ctx, d.bob, s.ID, "reply", id, demoAnswer, "--yes")
				if code != ExitOK {
					err = fmt.Errorf("the agent's reply exited %d: %s", code, strings.TrimSpace(errOut))
				}
			}
			d.agentMu.Lock()
			d.agentOut, d.agentErr = out, err
			d.agentMu.Unlock()
		}()
	}
}

// stepStart waits for both daemons to register on the bus.
func (d *demo) stepStart(ctx context.Context) (string, error) {
	d.say("\n1. Two machines start: alice's and bob's daemons register on the bus.")
	err := waitFor(ctx, func() (bool, error) {
		code, out, _ := d.cli(ctx, d.alice, "", "users")
		return code == ExitOK && strings.Contains(out, "alice") && strings.Contains(out, "bob"), nil
	})
	if err != nil {
		return "", err
	}
	if _, err := d.show(ctx, d.alice, "users"); err != nil {
		return "", err
	}
	return "alice and bob registered on the in-memory bus", nil
}

// stepAsk has alice ask bob.
func (d *demo) stepAsk(ctx context.Context) (string, error) {
	d.say("\n2. alice asks bob. Her agent runs this when she writes \"ask ^bob ...\".")
	if _, err := d.show(ctx, d.alice, "ask", "bob", demoAsk, "--yes"); err != nil {
		return "", err
	}
	t, err := d.alice.newest(ctx, threads.DirectionOut, "bob")
	if err != nil {
		return "", err
	}
	if t.ID == "" {
		return "", errors.New("alice has no thread with bob after the ask")
	}
	d.say("  The thread is %s; alice can call it ^bob, her newest open thread with bob.", t.ID)
	return "thread " + t.ID, nil
}

// askThread is the thread alice opened in stepAsk.
func (d *demo) askThread(ctx context.Context) (string, error) {
	t, err := d.alice.newest(ctx, threads.DirectionOut, "bob")
	if err != nil || t.ID == "" {
		return "", errors.Join(errors.New("alice's ask is missing"), err)
	}
	return t.ID, nil
}

// stepInbox shows the ask waiting in bob's inbox, nothing run yet.
func (d *demo) stepInbox(ctx context.Context) (string, error) {
	id, err := d.askThread(ctx)
	if err != nil {
		return "", err
	}
	err = waitFor(ctx, func() (bool, error) {
		t, err := d.bob.thread(ctx, id)
		return t.StagedKickoff != "", err
	})
	if err != nil {
		return "", err
	}
	d.say("\n3. bob reads alice's messages before anything runs (inbox mode), so the ask waits in his inbox.")
	if _, err := d.show(ctx, d.bob, "inbox"); err != nil {
		return "", err
	}
	d.say("\n  He reads exactly what his agent would be given:")
	if _, err := d.show(ctx, d.bob, "open", id, "--no-start"); err != nil {
		return "", err
	}
	return "staged in bob's inbox; nothing ran", nil
}

// stepOpen has bob run the ask; the pretend agent answers.
func (d *demo) stepOpen(ctx context.Context) (string, error) {
	id, err := d.askThread(ctx)
	if err != nil {
		return "", err
	}
	d.say("\n4. bob runs it. A session starts in his Copilot app, and the agent answers with wormhole reply.")
	if _, err := d.show(ctx, d.bob, "open", id, "--yes"); err != nil {
		return "", err
	}
	err = waitFor(ctx, func() (bool, error) {
		d.agentMu.Lock()
		defer d.agentMu.Unlock()
		return d.agentOut != "" || d.agentErr != nil, d.agentErr
	})
	if err != nil {
		return "", err
	}
	d.agentMu.Lock()
	out := d.agentOut
	d.agentMu.Unlock()
	d.say("\n  bob's agent, inside that session:")
	d.say("  bob$ wormhole reply %s %s", id, shellJoin([]string{demoAnswer, "--yes"}))
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		d.say("    %s", line)
	}
	return "bob's agent replied", nil
}

// stepReply shows the reply reaching alice.
func (d *demo) stepReply(ctx context.Context) (string, error) {
	id, err := d.askThread(ctx)
	if err != nil {
		return "", err
	}
	err = waitFor(ctx, func() (bool, error) {
		t, err := d.alice.thread(ctx, id)
		return t.State == threads.StateReplied && t.StagedKickoff != "", err
	})
	if err != nil {
		return "", err
	}
	d.say("\n5. The reply lands on alice's machine. The thread is replied, and the answer waits in her inbox.")
	if _, err := d.show(ctx, d.alice, "threads"); err != nil {
		return "", err
	}
	if _, err := d.show(ctx, d.alice, "inbox"); err != nil {
		return "", err
	}
	return "thread " + id + " replied", nil
}

// stepSnooze has alice put the reply off and bring it back.
func (d *demo) stepSnooze(ctx context.Context) (string, error) {
	id, err := d.askThread(ctx)
	if err != nil {
		return "", err
	}
	d.say("\n6. Not now: alice snoozes the reply for an hour, then changes her mind.")
	if _, err := d.show(ctx, d.alice, "snooze", id, "1h"); err != nil {
		return "", err
	}
	out, err := d.show(ctx, d.alice, "inbox")
	if err != nil {
		return "", err
	}
	if strings.Contains(out, id) {
		return "", errors.New("the snoozed reply is still listed in the inbox")
	}
	if _, err := d.show(ctx, d.alice, "snooze", id, "off"); err != nil {
		return "", err
	}
	t, err := d.alice.thread(ctx, id)
	if err != nil {
		return "", err
	}
	if !t.SnoozedUntil.IsZero() {
		return "", errors.New("the reply is still snoozed after snooze off")
	}
	return "snoozed for 1h, then back in the inbox", nil
}

// stepDecline has bob ask alice, and alice decline with a reason.
func (d *demo) stepDecline(ctx context.Context) (string, error) {
	d.say("\n7. The other way round: bob asks alice, and she declines with a reason.")
	if _, err := d.show(ctx, d.bob, "ask", "alice", demoSecondAsk, "--yes"); err != nil {
		return "", err
	}
	t, err := d.bob.newest(ctx, threads.DirectionOut, "alice")
	if err != nil {
		return "", err
	}
	if t.ID == "" {
		return "", errors.New("bob has no thread with alice after the ask")
	}
	id := t.ID
	err = waitFor(ctx, func() (bool, error) {
		t, err := d.alice.thread(ctx, id)
		return t.StagedKickoff != "", err
	})
	if err != nil {
		return "", err
	}
	if _, err := d.show(ctx, d.alice, "decline", id, demoDeclineText, "--yes"); err != nil {
		return "", err
	}
	err = waitFor(ctx, func() (bool, error) {
		t, err := d.bob.thread(ctx, id)
		return t.State == threads.StateDeclined, err
	})
	if err != nil {
		return "", err
	}
	d.say("\n  bob sees why, on his side:")
	if _, err := d.show(ctx, d.bob, "threads", "--peer", "alice"); err != nil {
		return "", err
	}
	t, err = d.bob.thread(ctx, id)
	if err != nil {
		return "", err
	}
	return t.LastPreview, nil
}

// shellJoin quotes args for display the way a person would type them.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'$`\\!?*()&;|<>") {
			out[i] = "\"" + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(a) + "\""
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}
