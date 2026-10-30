package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/bus/memory"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// clock is a settable time source for the daemon under test.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// newClockFixture is newFixture with a fast housekeeping tick and a
// controllable clock.
func newClockFixture(t *testing.T, yaml string, start time.Time) (*fixture, *clock) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	ck := &clock{t: start}
	f := &fixture{broker: memory.NewBroker(memory.WithMaxDeliver(MaxDeliver)), app: &ghapp.Recorder{}, wake: make(chan struct{}, 100)}
	f.alice, _ = f.broker.Connect("alice")
	f.bobBus, _ = f.broker.Connect("bob")
	f.store, err = threads.Open(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.bob = New(Options{Config: cfg, Bus: f.bobBus, Store: f.store, App: f.app, Log: slog.New(slog.NewTextHandler(testWriter{t}, nil)), Host: "bob-laptop", Now: ck.now, Paused: func() bool { return f.paused.Load() }, PauseCheck: 20 * time.Millisecond, Tick: 20 * time.Millisecond, Exe: "/opt/wormhole"})
	sub, err := f.alice.Subscribe(context.Background(), func(ctx context.Context, d bus.Delivery) {
		f.mu.Lock()
		f.aliceIn = append(f.aliceIn, d)
		f.mu.Unlock()
		_ = f.alice.Ack(ctx, d.ID)
		f.wake <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sub.Stop(); f.store.Close(); f.broker.Close() })
	return f, ck
}

// waitThread polls until pred holds for the thread or the deadline passes.
func waitThread(t *testing.T, f *fixture, id string, pred func(threads.Thread) bool) threads.Thread {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var th threads.Thread
	for time.Now().Before(deadline) {
		th, _ = f.store.Get(context.Background(), id)
		if pred(th) {
			return th
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("thread never reached the wanted state: %+v", th)
	return th
}

const actionsCfg = `
handle: bob
bus: {kind: memory}
senders: [alice]
copilot: {workdir: /tmp/wh-test}
daemon: {nak_backoff: 10ms, notify: actions, snooze_default: 90m, show_command: [kitty, wormhole, inbox, --watch]}
`

func TestDeclinedAckEndsOutboundThread(t *testing.T) {
	// bob asked alice; alice's person declined. bob's thread is over and
	// bob hears about it once.
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	ctx := context.Background()
	ask := outboundThread(t, f, "bob-root")
	dec := envelope.NewAck(ask, envelope.Peer{User: "alice", Host: "alice-laptop"}, envelope.DeclineStatus("taking it to standup"), time.Now())
	_ = f.alice.Publish(ctx, bus.SubjectFor(dec), dec)
	th := waitThread(t, f, ask.ID, func(th threads.Thread) bool { return th.State == threads.StateDeclined })
	if th.LastPreview != "declined: taking it to standup" {
		t.Fatalf("thread = %+v", th)
	}
	calls := f.waitOps(t, 1)
	if calls[0].Op != "notify" || calls[0].Title != "^alice declined your ask" || !strings.HasPrefix(calls[0].Body, "taking it to standup\n") {
		t.Fatalf("calls = %+v", calls)
	}
	msgs, _ := f.store.Messages(ctx, ask.ID)
	if len(msgs) != 2 || msgs[1].Type != envelope.TypeAck {
		t.Fatalf("messages = %+v", msgs)
	}
	open, _ := f.store.List(ctx, threads.Filter{Open: true})
	if len(open) != 0 {
		t.Fatal("declined thread still open")
	}
}

func TestDeclineTellsThePeerAndClosesTheThread(t *testing.T) {
	// alice asked bob (inbox mode); bob declines. alice's daemon would see
	// the ack; here alice's test connection does. A later message from
	// alice on the thread is refused.
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	ctx := context.Background()
	ask := aliceAsk("Can you look at the flaky e2e?")
	f.deliver(ask, "alice")
	f.waitAck(t, 1)
	th, _ := f.store.Get(ctx, ask.ID)
	now := time.Now()
	ack, err := Decline(ctx, f.bobBus, f.store, f.bob.Me(), th, "  ask in standup ", now)
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := ack.Declined(); !ok || reason != "ask in standup" || ack.To.User != "alice" || *ack.InReplyTo != ask.ID || ack.ThreadID != ask.ID {
		t.Fatalf("ack = %+v", ack)
	}
	acks := f.waitAck(t, 2)
	if got, _ := acks[1].Env.Declined(); got != "ask in standup" || acks[1].Sender != "bob" {
		t.Fatalf("alice got %+v from %s", acks[1].Env, acks[1].Sender)
	}
	th, _ = f.store.Get(ctx, ask.ID)
	if th.State != threads.StateDeclined || th.StagedKickoff != "" || th.LastPreview != "declined: ask in standup" {
		t.Fatalf("thread = %+v", th)
	}
	msgs, _ := f.store.Messages(ctx, ask.ID)
	if len(msgs) != 2 || msgs[1].Type != envelope.TypeAck || msgs[1].Direction != threads.DirectionOut {
		t.Fatalf("messages = %+v", msgs)
	}
	// Twice is an error: nothing is staged any more.
	if _, err := Decline(ctx, f.bobBus, f.store, f.bob.Me(), th, "", now); err == nil || !strings.Contains(err.Error(), "nothing is waiting") {
		t.Fatalf("second decline: %v", err)
	}
	// alice follows up anyway.
	msg := envelope.NewFollowUp(envelope.TypeMessage, ask, envelope.Peer{User: "alice"}, "bob", "please?", time.Now())
	f.deliver(msg, "alice")
	acks = f.waitAck(t, 3)
	if reason, ok := acks[2].Env.Rejected(); !ok || reason != "thread declined" {
		t.Fatalf("follow-up on declined thread: %+v", acks[2].Env)
	}
	// Nothing ran in the app.
	if ops := f.app.Ops(); len(ops) != 1 || ops[0] != "notify" {
		t.Fatalf("ops = %v", ops)
	}
}

func TestActionsModeBubbleButtons(t *testing.T) {
	f, ck := newClockFixture(t, actionsCfg, time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local))
	// The fake notifier presses a different button for each bubble.
	presses := make(chan string, 3)
	f.app.OnNotify = func(ctx context.Context, n ghapp.Notification) ghapp.NotifyResult {
		select {
		case key := <-presses:
			return ghapp.NotifyResult{ID: 7, Action: key}
		case <-ctx.Done():
			return ghapp.NotifyResult{}
		}
	}
	runDaemon(t, f)
	ctx := context.Background()

	// 1. Later: the message is snoozed for snooze_default.
	presses <- ActionLater
	first := aliceAsk("first")
	f.deliver(first, "alice")
	f.waitAck(t, 1)
	th := waitThread(t, f, first.ID, func(th threads.Thread) bool { return !th.SnoozedUntil.IsZero() })
	if want := ck.now().Add(90 * time.Minute); !th.SnoozedUntil.Equal(want) || th.StagedKickoff == "" {
		t.Fatalf("snoozed until %v, want %v: %+v", th.SnoozedUntil, want, th)
	}
	calls := f.waitOps(t, 1)
	n := calls[0].Notification
	if !n.Low || n.ReplaceID != 0 || len(n.Actions) != 3 || n.Actions[0].Key != ActionShow || n.Actions[1].Key != ActionLater || n.Actions[1].Label != "Later · 90m" || n.Actions[2].Key != ActionDecline {
		t.Fatalf("notification = %+v", n)
	}
	if n.Title != "^alice asked you something" || !strings.HasSuffix(n.Body, "\nowner/repo · 1 waiting in your inbox") {
		t.Fatalf("notification = %+v", n)
	}

	// 2. Show: the configured command runs; the bubble replaces the first.
	presses <- ActionShow
	second := aliceAsk("second")
	f.deliver(second, "alice")
	f.waitAck(t, 2)
	calls = f.waitOps(t, 3)
	if calls[1].Op != "notify" || calls[1].Notification.ReplaceID != 7 || !strings.HasSuffix(calls[1].Body, "1 waiting in your inbox") {
		t.Fatalf("second bubble = %+v", calls[1])
	}
	if calls[2].Op != "show" || strings.Join(calls[2].Argv, " ") != "kitty wormhole inbox --watch" {
		t.Fatalf("show = %+v", calls[2])
	}

	// 3. Decline: alice gets a declined receipt for the third ask.
	presses <- ActionDecline
	third := aliceAsk("third")
	f.deliver(third, "alice")
	acks := f.waitAck(t, 4)
	if _, ok := acks[3].Env.Declined(); !ok || acks[3].Env.ThreadID != third.ID {
		t.Fatalf("acks = %+v", acks[3].Env)
	}
	th = waitThread(t, f, third.ID, func(th threads.Thread) bool { return th.State == threads.StateDeclined })
	if th.StagedKickoff != "" {
		t.Fatalf("thread = %+v", th)
	}
	// The first two are still waiting (one snoozed, one not).
	waiting, _ := f.bob.waiting(ctx)
	if len(waiting) != 1 || waiting[0].ID != second.ID {
		t.Fatalf("waiting = %+v", waiting)
	}
}

func TestShowCommandDefault(t *testing.T) {
	f, _ := newClockFixture(t, strings.Replace(actionsCfg, ", show_command: [kitty, wormhole, inbox, --watch]", "", 1), time.Now())
	t.Setenv("TERMINAL", "")
	if got := strings.Join(f.bob.showCommand(), " "); got != "x-terminal-emulator -e /opt/wormhole inbox --watch" {
		t.Fatal(got)
	}
	t.Setenv("TERMINAL", "foot")
	if got := strings.Join(f.bob.showCommand(), " "); got != "foot -e /opt/wormhole inbox --watch" {
		t.Fatal(got)
	}
}

func TestSnoozeWakesOnce(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local)
	f, ck := newClockFixture(t, inboxCfg, start)
	runDaemon(t, f)
	ctx := context.Background()
	ask := aliceAsk("later please")
	f.deliver(ask, "alice")
	f.waitAck(t, 1)
	f.waitOps(t, 1)
	until := start.Add(time.Hour)
	if _, err := f.store.Update(ctx, ask.ID, func(u *threads.Thread) { u.SnoozedUntil = until }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if ops := f.app.Ops(); len(ops) != 1 {
		t.Fatalf("woke early: %v", ops)
	}
	ck.set(until)
	th := waitThread(t, f, ask.ID, func(th threads.Thread) bool { return th.SnoozedUntil.IsZero() })
	if th.StagedKickoff == "" {
		t.Fatalf("thread = %+v", th)
	}
	calls := f.waitOps(t, 2)
	if calls[1].Op != "notify" || calls[1].Title != "^alice is still waiting" || !strings.HasPrefix(calls[1].Body, "later please\n") {
		t.Fatalf("wake = %+v", calls[1])
	}
	time.Sleep(60 * time.Millisecond)
	if ops := f.app.Ops(); len(ops) != 2 {
		t.Fatalf("woke more than once: %v", ops)
	}
}

func TestDigestSummarizes(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local)
	cfg := strings.Replace(inboxCfg, "daemon: {nak_backoff: 10ms}", "daemon: {nak_backoff: 10ms, notify: digest, digest_every: 20m}", 1)
	f, ck := newClockFixture(t, cfg, start)
	runDaemon(t, f)
	f.deliver(aliceAsk("one"), "alice")
	f.deliver(aliceAsk("two"), "alice")
	f.waitAck(t, 2)
	time.Sleep(60 * time.Millisecond)
	if ops := f.app.Ops(); len(ops) != 0 {
		t.Fatalf("digest mode notified per message: %v", ops)
	}
	ck.set(start.Add(21 * time.Minute))
	calls := f.waitOps(t, 1)
	n := calls[0].Notification
	if n.Title != "Wormhole inbox" || !n.Low || !strings.HasPrefix(n.Body, "2 waiting in your inbox (^alice)\n") {
		t.Fatalf("digest = %+v", n)
	}
	// Nothing new: no second digest.
	ck.set(start.Add(45 * time.Minute))
	time.Sleep(60 * time.Millisecond)
	if ops := f.app.Ops(); len(ops) != 1 {
		t.Fatalf("digest repeated with nothing new: %v", ops)
	}
	// Something new: the next digest replaces the bubble.
	f.deliver(aliceAsk("three"), "alice")
	f.waitAck(t, 3)
	ck.set(start.Add(70 * time.Minute))
	calls = f.waitOps(t, 2)
	if calls[1].Notification.ReplaceID != 1 || !strings.HasPrefix(calls[1].Body, "3 waiting") {
		t.Fatalf("second digest = %+v", calls[1])
	}
}

func TestQuietHoursHoldThenSummarize(t *testing.T) {
	start := time.Date(2026, 10, 30, 22, 0, 0, 0, time.Local)
	cfg := strings.Replace(inboxCfg, "daemon: {nak_backoff: 10ms}", `daemon: {nak_backoff: 10ms, quiet_hours: ["18:00", "09:00"]}`, 1)
	f, ck := newClockFixture(t, cfg, start)
	runDaemon(t, f)
	f.deliver(aliceAsk("night"), "alice")
	f.waitAck(t, 1)
	time.Sleep(60 * time.Millisecond)
	if ops := f.app.Ops(); len(ops) != 0 {
		t.Fatalf("notified during quiet hours: %v", ops)
	}
	ck.set(time.Date(2026, 10, 31, 9, 0, 0, 0, time.Local))
	calls := f.waitOps(t, 1)
	if calls[0].Title != "Wormhole inbox" || !strings.HasPrefix(calls[0].Body, "1 waiting in your inbox (^alice)\n") {
		t.Fatalf("summary = %+v", calls[0])
	}
	// Daytime messages toast as usual.
	f.deliver(aliceAsk("day"), "alice")
	f.waitAck(t, 2)
	calls = f.waitOps(t, 2)
	if calls[1].Title != "^alice asked you something" {
		t.Fatalf("day = %+v", calls[1])
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{2 * time.Hour: "2h", 90 * time.Minute: "90m", 45 * time.Second: "45s"} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
}
