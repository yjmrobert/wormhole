package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/bus/memory"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

type fixture struct {
	broker *memory.Broker
	alice  bus.Bus // the sender's connection
	bob    *Daemon // the recipient
	bobBus bus.Bus
	app    *ghapp.Recorder
	store  *threads.Store
	paused atomic.Bool
	// acks received by alice
	mu      sync.Mutex
	aliceIn []bus.Delivery
	wake    chan struct{}
	// lastAsk is the envelope a Fail hook may act on.
	lastAsk envelope.Envelope
}

func newFixture(t *testing.T, yaml string) *fixture {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{broker: memory.NewBroker(memory.WithMaxDeliver(MaxDeliver)), app: &ghapp.Recorder{}, wake: make(chan struct{}, 100)}
	f.alice, _ = f.broker.Connect("alice")
	f.bobBus, _ = f.broker.Connect("bob")
	f.store, err = threads.Open(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.bob = New(Options{Config: cfg, Bus: f.bobBus, Store: f.store, App: f.app, Log: slog.New(slog.NewTextHandler(testWriter{t}, nil)), Host: "bob-laptop", Paused: func() bool { return f.paused.Load() }, PauseCheck: 20 * time.Millisecond})
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
	return f
}

// waitOps waits until the recorder has at least n calls.
func (f *fixture) waitOps(t *testing.T, n int) []ghapp.Call {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls := f.app.Snapshot(); len(calls) >= n {
			return calls
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d app calls, have %v", n, f.app.Ops())
	return nil
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func (f *fixture) waitAck(t *testing.T, n int) []bus.Delivery {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		if len(f.aliceIn) >= n {
			out := append([]bus.Delivery(nil), f.aliceIn...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		select {
		case <-f.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %d acks", n)
		}
	}
}

// deliver hands an envelope to bob's daemon the way the bus would.
func (f *fixture) deliver(e envelope.Envelope, sender string) bus.MessageID {
	ctx := context.Background()
	id := bus.MessageID("test/" + e.ID)
	// Use the real memory bus for the delivery so Ack/Nak have a target.
	_ = f.alice.Publish(ctx, bus.SubjectFor(e), e)
	return id
}

const inboxCfg = `
handle: bob
bus: {kind: memory}
senders: [alice]
copilot: {workdir: /tmp/wh-test}
daemon: {nak_backoff: 10ms}
`

const autoCfg = `
handle: bob
bus: {kind: memory}
senders: [{handle: alice, mode: auto}]
limits: {per_sender_per_day: 2, per_day: 10}
copilot: {workdir: /tmp/wh-test, safe_tools: [view, grep], model: test-model}
daemon: {nak_backoff: 10ms}
`

// inPlaceCfg is autoCfg with replies appended to the asking session.
var inPlaceCfg = strings.Replace(autoCfg, "daemon: {nak_backoff: 10ms}", "daemon: {nak_backoff: 10ms, reply_in_place: true}", 1)

func runDaemon(t *testing.T, f *fixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.bob.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func aliceAsk(body string) envelope.Envelope {
	e := envelope.NewAsk(envelope.Peer{User: "alice", Host: "alice-laptop", SessionID: "root-session"}, "bob", body, time.Now())
	e.HumanReviewed = true
	e.Context = envelope.Context{Repo: "owner/repo", Ref: "PR #1"}
	return e
}

func TestInboxModeStagesAndAcks(t *testing.T) {
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	ask := aliceAsk("What do you think about X?")
	f.deliver(ask, "alice")
	acks := f.waitAck(t, 1)
	if acks[0].Env.Type != envelope.TypeAck || acks[0].Env.Body != "delivered" || *acks[0].Env.InReplyTo != ask.ID || acks[0].Sender != "bob" {
		t.Fatalf("ack = %+v", acks[0].Env)
	}
	th, err := f.store.Get(context.Background(), ask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if th.State != threads.StatePending || th.Mode != threads.ModeInbox || th.LocalSessionID != "" || th.Peer != "alice" || th.Direction != threads.DirectionIn {
		t.Fatalf("thread = %+v", th)
	}
	if !strings.HasPrefix(th.StagedKickoff, "[wormhole] Message from ^alice (thread "+ask.ID+", human_reviewed: true).") || !strings.HasSuffix(th.StagedKickoff, "---\nWhat do you think about X?") {
		t.Fatalf("kickoff = %q", th.StagedKickoff)
	}
	if !strings.Contains(th.StagedKickoff, "wormhole reply "+ask.ID) || !strings.Contains(th.StagedKickoff, "repo owner/repo ref PR #1") {
		t.Fatalf("kickoff = %q", th.StagedKickoff)
	}
	f.waitOps(t, 1)
	ops := f.app.Ops()
	if len(ops) != 1 || ops[0] != "notify" {
		t.Fatalf("inbox mode must not run copilot: %v", ops)
	}
	if f.broker.Pending("bob") != 0 {
		t.Fatal("message not acked on the bus")
	}
	// Presence was published.
	list, _ := f.alice.Registry().List(context.Background())
	if len(list) != 1 || list[0].Handle != "bob" || list[0].Host != "bob-laptop" || list[0].Mode != "inbox" {
		t.Fatalf("registry = %+v", list)
	}
}

func TestAutoModeRunsCopilotReadOnly(t *testing.T) {
	f := newFixture(t, autoCfg)
	runDaemon(t, f)
	ask := aliceAsk("Review the retry policy please")
	f.deliver(ask, "alice")
	f.waitAck(t, 1)
	calls := f.waitOps(t, 3)
	if len(calls) != 3 || calls[0].Op != "create" || calls[1].Op != "open" || calls[2].Op != "notify" {
		t.Fatalf("ops = %v", f.app.Ops())
	}
	s := calls[0].Session
	if s.Name != "^alice: Review the retry policy please" || s.Model != "test-model" || strings.Join(s.AvailableTools, ",") != "view,grep" || s.Workdir != "/tmp/wh-test" {
		t.Fatalf("session = %+v", s)
	}
	if calls[1].SessionID != s.ID {
		t.Fatal("opened a different session than created")
	}
	th, _ := f.store.Get(context.Background(), ask.ID)
	if th.State != threads.StateDelivered || th.Mode != threads.ModeAuto || th.LocalSessionID != s.ID || !th.Opened {
		t.Fatalf("thread = %+v", th)
	}
}

func TestUnknownSenderRejected(t *testing.T) {
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	mallory, _ := f.broker.Connect("mallory")
	malloryIn := make(chan bus.Delivery, 1)
	sub, _ := mallory.Subscribe(context.Background(), func(ctx context.Context, d bus.Delivery) { _ = mallory.Ack(ctx, d.ID); malloryIn <- d })
	defer sub.Stop()
	ask := envelope.NewAsk(envelope.Peer{User: "mallory"}, "bob", "hi", time.Now())
	_ = mallory.Publish(context.Background(), bus.SubjectFor(ask), ask)
	select {
	case d := <-malloryIn:
		if reason, ok := d.Env.Rejected(); !ok || reason != "sender not allowed" {
			t.Fatalf("ack = %+v", d.Env)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection")
	}
	if _, err := f.store.Get(context.Background(), ask.ID); !errors.Is(err, threads.ErrNotFound) {
		t.Fatal("rejected ask was materialized")
	}
	if len(f.app.Ops()) != 0 {
		t.Fatalf("app touched for rejected sender: %v", f.app.Ops())
	}
}

func TestSpoofedFromRejected(t *testing.T) {
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	// alice is allowlisted, but claims to be carol inside the envelope.
	ask := envelope.NewAsk(envelope.Peer{User: "carol"}, "bob", "hi", time.Now())
	_ = f.alice.Publish(context.Background(), bus.InboxSubject("bob", "alice"), ask)
	acks := f.waitAck(t, 1)
	if reason, ok := acks[0].Env.Rejected(); !ok || reason != "sender mismatch" || acks[0].Env.To.User != "alice" {
		t.Fatalf("ack = %+v", acks[0].Env)
	}
}

func TestHopAndExpiryCaps(t *testing.T) {
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	ask := aliceAsk("deep")
	ask.Hop = 7
	f.deliver(ask, "alice")
	expired := aliceAsk("old")
	expired.CreatedAt = time.Now().Add(-10 * 24 * time.Hour)
	expired.ExpiresAt = time.Now().Add(-3 * 24 * time.Hour)
	f.deliver(expired, "alice")
	acks := f.waitAck(t, 2)
	reasons := map[string]string{}
	for _, a := range acks {
		r, _ := a.Env.Rejected()
		reasons[*a.Env.InReplyTo] = r
	}
	if !strings.Contains(reasons[ask.ID], "max_hops") || !strings.Contains(reasons[expired.ID], "expired") {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestRateCapDowngradesToInbox(t *testing.T) {
	f := newFixture(t, autoCfg) // per_sender_per_day: 2
	runDaemon(t, f)
	for i := 0; i < 3; i++ {
		f.deliver(aliceAsk("q"), "alice")
	}
	f.waitAck(t, 3)
	f.waitOps(t, 3)
	creates := 0
	for _, op := range f.app.Ops() {
		if op == "create" {
			creates++
		}
	}
	if creates != 2 {
		t.Fatalf("copilot ran %d times, want 2", creates)
	}
	list, _ := f.store.List(context.Background(), threads.Filter{State: threads.StatePending})
	if len(list) != 1 || list[0].Mode != threads.ModeInbox {
		t.Fatalf("over-cap ask not queued: %+v", list)
	}
}

func TestCreateFailureRetriesThenDeadLetters(t *testing.T) {
	f := newFixture(t, autoCfg)
	f.app.Fail = func(op string) error {
		if op == "create" {
			return errors.New("copilot exploded")
		}
		return nil
	}
	runDaemon(t, f)
	ask := aliceAsk("boom")
	f.deliver(ask, "alice")
	acks := f.waitAck(t, 1)
	if reason, ok := acks[0].Env.Rejected(); !ok || !strings.Contains(reason, "after 5 attempts") {
		t.Fatalf("ack = %+v", acks[0].Env)
	}
	if dlq := f.broker.DLQ("bob"); len(dlq) != 1 || dlq[0].ID != ask.ID {
		t.Fatalf("dlq = %+v", dlq)
	}
	creates := 0
	for _, op := range f.app.Ops() {
		if op == "create" {
			creates++
		}
	}
	if creates != MaxDeliver {
		t.Fatalf("create attempts = %d", creates)
	}
	// Each failed attempt rolled its record back, so nothing local claims
	// the ask was delivered.
	if _, err := f.store.Get(context.Background(), ask.ID); err == nil {
		t.Fatal("thread row survived a dead-lettered ask")
	}
	if seen, _ := f.store.HasMessage(context.Background(), ask.ID); seen {
		t.Fatal("message copy survived a dead-lettered ask")
	}
}

func TestPausedLeavesMessageOnBus(t *testing.T) {
	f := newFixture(t, inboxCfg)
	f.paused.Store(true)
	runDaemon(t, f)
	f.deliver(aliceAsk("wait"), "alice")
	time.Sleep(100 * time.Millisecond)
	if f.broker.Pending("bob") != 1 {
		t.Fatal("paused daemon consumed the message")
	}
	f.paused.Store(false)
	f.waitAck(t, 1)
}

func TestDuplicateDeliveryIsIdempotent(t *testing.T) {
	f := newFixture(t, autoCfg)
	runDaemon(t, f)
	ask := aliceAsk("once")
	f.deliver(ask, "alice")
	f.waitAck(t, 1)
	// Simulate the bus redelivering after a lost ack.
	f.bob.Handle(context.Background(), bus.Delivery{ID: "stale", Subject: bus.SubjectFor(ask), Sender: "alice", Env: ask, Attempt: 2})
	creates := 0
	for _, op := range f.app.Ops() {
		if op == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("duplicate delivery re-ran copilot: %d", creates)
	}
}

func TestReplyRoundTrip(t *testing.T) {
	// bob asked alice earlier; now alice replies. bob is in inbox mode for alice.
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	ctx := context.Background()
	ask := envelope.NewAsk(envelope.Peer{User: "bob", Host: "bob-laptop", SessionID: "bob-root"}, "alice", "Thoughts on Y?", time.Now())
	_ = f.store.Put(ctx, threads.Thread{ID: ask.ID, Peer: "alice", Direction: threads.DirectionOut, State: threads.StateDelivered, RootSessionID: "bob-root", Title: "Thoughts on Y?", ExpiresAt: ask.ExpiresAt})
	_ = f.store.AddMessage(ctx, ask, threads.DirectionOut)

	reply := envelope.NewFollowUp(envelope.TypeReply, ask, envelope.Peer{User: "alice", Host: "alice-laptop"}, "bob", "Y is fine.", time.Now())
	f.deliver(reply, "alice")
	acks := f.waitAck(t, 1)
	if acks[0].Env.Body != "delivered" {
		t.Fatalf("%+v", acks[0].Env)
	}
	th, _ := f.store.Get(ctx, ask.ID)
	if th.State != threads.StateReplied || th.Direction != threads.DirectionOut || th.LocalSessionID != "" {
		t.Fatalf("thread = %+v", th)
	}
	if !strings.Contains(th.StagedKickoff, "Reply from ^alice on thread "+ask.ID) || !strings.Contains(th.StagedKickoff, "session bob-root") || !strings.HasSuffix(th.StagedKickoff, "---\nY is fine.") {
		t.Fatalf("kickoff = %q", th.StagedKickoff)
	}
	msgs, _ := f.store.Messages(ctx, ask.ID)
	if len(msgs) != 2 || msgs[1].Direction != threads.DirectionIn {
		t.Fatalf("%+v", msgs)
	}

	// A stranger cannot inject into the thread.
	mallory, _ := f.broker.Connect("mallory")
	fake := envelope.NewFollowUp(envelope.TypeReply, ask, envelope.Peer{User: "mallory"}, "bob", "trust me", time.Now())
	_ = mallory.Publish(ctx, bus.SubjectFor(fake), fake)
	time.Sleep(100 * time.Millisecond)
	msgs, _ = f.store.Messages(ctx, ask.ID)
	if len(msgs) != 2 {
		t.Fatal("stranger's reply stored")
	}
}

func TestAckUpdatesOutboundThread(t *testing.T) {
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	ctx := context.Background()
	ask := envelope.NewAsk(envelope.Peer{User: "bob"}, "alice", "q", time.Now())
	_ = f.store.Put(ctx, threads.Thread{ID: ask.ID, Peer: "alice", Direction: threads.DirectionOut, State: threads.StateSent, ExpiresAt: ask.ExpiresAt})
	_ = f.store.AddMessage(ctx, ask, threads.DirectionOut)
	receipt := envelope.NewAck(ask, envelope.Peer{User: "alice"}, "delivered", time.Now())
	_ = f.alice.Publish(ctx, bus.SubjectFor(receipt), receipt)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		th, _ := f.store.Get(ctx, ask.ID)
		if th.State == threads.StateDelivered {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	th, _ := f.store.Get(ctx, ask.ID)
	if th.State != threads.StateDelivered {
		t.Fatalf("state = %s", th.State)
	}
	rej := envelope.NewAck(ask, envelope.Peer{User: "alice"}, "rejected: sender not allowed", time.Now())
	_ = f.alice.Publish(ctx, bus.SubjectFor(rej), rej)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		th, _ = f.store.Get(ctx, ask.ID)
		if th.State == threads.StateRejected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if th.State != threads.StateRejected || th.LastPreview != "rejected: sender not allowed" {
		t.Fatalf("thread = %+v", th)
	}
}

// outboundThread seeds a thread bob opened to alice from session bob-root.
func outboundThread(t *testing.T, f *fixture, root string) envelope.Envelope {
	t.Helper()
	ctx := context.Background()
	ask := envelope.NewAsk(envelope.Peer{User: "bob", Host: "bob-laptop", SessionID: root}, "alice", "Thoughts on Y?", time.Now())
	if err := f.store.Put(ctx, threads.Thread{ID: ask.ID, Peer: "alice", Direction: threads.DirectionOut, State: threads.StateDelivered, RootSessionID: root, Title: "Thoughts on Y?", Workdir: "/tmp/wh-test", ExpiresAt: ask.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	_ = f.store.AddMessage(ctx, ask, threads.DirectionOut)
	return ask
}

func TestReplyResumesAskingSessionInAutoMode(t *testing.T) {
	f := newFixture(t, inPlaceCfg)
	runDaemon(t, f)
	ctx := context.Background()
	ask := outboundThread(t, f, "bob-root")

	reply := envelope.NewFollowUp(envelope.TypeReply, ask, envelope.Peer{User: "alice", Host: "alice-laptop"}, "bob", "Y is fine.", time.Now())
	f.deliver(reply, "alice")
	calls := f.waitOps(t, 3)
	r := calls[0]
	if r.Op != "resume" || r.Session.ID != "bob-root" || r.Session.Model != "test-model" || strings.Join(r.Session.AvailableTools, ",") != "view,grep" || r.Session.Workdir != "/tmp/wh-test" {
		t.Fatalf("first call = %+v", r)
	}
	if !strings.Contains(r.Session.Kickoff, "Reply from ^alice on thread "+ask.ID) || !strings.Contains(r.Session.Kickoff, "session bob-root") || !strings.HasSuffix(r.Session.Kickoff, "---\nY is fine.") {
		t.Fatalf("kickoff = %q", r.Session.Kickoff)
	}
	if calls[1].Op != "open" || calls[1].SessionID != "bob-root" || calls[2].Op != "notify" || !strings.Contains(calls[2].Title, "^alice replied") {
		t.Fatalf("ops = %v", f.app.Ops())
	}
	th, _ := f.store.Get(ctx, ask.ID)
	if th.State != threads.StateReplied || th.LocalSessionID != "bob-root" || th.StagedKickoff != "" || !th.Opened || th.Mode != threads.ModeAuto {
		t.Fatalf("thread = %+v", th)
	}

	// A later message on the thread continues the same session.
	msg := envelope.NewFollowUp(envelope.TypeMessage, reply, envelope.Peer{User: "alice", Host: "alice-laptop"}, "bob", "One more thing.", time.Now())
	f.deliver(msg, "alice")
	calls = f.waitOps(t, 6)
	if calls[3].Op != "resume" || calls[3].Session.ID != "bob-root" || !strings.HasSuffix(calls[3].Session.Kickoff, "---\nOne more thing.") {
		t.Fatalf("follow-up call = %+v", calls[3])
	}
	if calls[4].Op != "open" || calls[4].SessionID != "bob-root" {
		t.Fatalf("ops = %v", f.app.Ops())
	}
}

func TestReplyFallsBackToNewSessionWhenResumeFails(t *testing.T) {
	f := newFixture(t, inPlaceCfg)
	f.app.Fail = func(op string) error {
		if op == "resume" {
			return errors.New("session is in use by the app")
		}
		return nil
	}
	runDaemon(t, f)
	ctx := context.Background()
	ask := outboundThread(t, f, "bob-root")
	reply := envelope.NewFollowUp(envelope.TypeReply, ask, envelope.Peer{User: "alice"}, "bob", "Y is fine.", time.Now())
	f.deliver(reply, "alice")
	calls := f.waitOps(t, 4)
	if calls[0].Op != "resume" || calls[0].Session.ID != "bob-root" {
		t.Fatalf("first call = %+v", calls[0])
	}
	c := calls[1]
	if c.Op != "create" || c.Session.Name != "Reply from ^alice" || c.Session.ID == "bob-root" || c.Session.ID == "" || strings.Join(c.Session.AvailableTools, ",") != "view,grep" {
		t.Fatalf("fallback = %+v", c)
	}
	if calls[2].Op != "open" || calls[2].SessionID != c.Session.ID || calls[3].Op != "notify" {
		t.Fatalf("ops = %v", f.app.Ops())
	}
	th, _ := f.store.Get(ctx, ask.ID)
	if th.State != threads.StateReplied || th.LocalSessionID != c.Session.ID || th.StagedKickoff != "" {
		t.Fatalf("thread = %+v", th)
	}
	acks := f.waitAck(t, 1)
	if acks[0].Env.Body != "delivered" {
		t.Fatalf("receipt = %+v", acks[0].Env)
	}
}

func TestReplyOpensNewSessionWithoutAskingSessionOrWhenDisabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		root string
	}{
		{"no asking session id", inPlaceCfg, ""},
		{"reply_in_place off (the default)", autoCfg, "bob-root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.cfg)
			runDaemon(t, f)
			ask := outboundThread(t, f, tc.root)
			reply := envelope.NewFollowUp(envelope.TypeReply, ask, envelope.Peer{User: "alice"}, "bob", "Y is fine.", time.Now())
			f.deliver(reply, "alice")
			calls := f.waitOps(t, 3)
			if calls[0].Op != "create" || calls[0].Session.Name != "Reply from ^alice" || calls[1].Op != "open" || calls[2].Op != "notify" {
				t.Fatalf("ops = %v calls = %+v", f.app.Ops(), calls)
			}
		})
	}
}

func TestInboxMessageOnOpenThreadStagesWithoutShowing(t *testing.T) {
	// bob already opened a session for alice's ask (inbox mode); a follow-up
	// message must wait in the inbox and remember which session to resume.
	f := newFixture(t, inboxCfg)
	runDaemon(t, f)
	ctx := context.Background()
	ask := aliceAsk("What do you think about X?")
	_ = f.store.Put(ctx, threads.Thread{ID: ask.ID, Peer: "alice", Direction: threads.DirectionIn, Mode: threads.ModeInbox, State: threads.StateDelivered, LocalSessionID: "bob-sess", Opened: true, Title: "What do you think about X?", ExpiresAt: ask.ExpiresAt})
	_ = f.store.AddMessage(ctx, ask, threads.DirectionIn)
	msg := envelope.NewFollowUp(envelope.TypeMessage, ask, envelope.Peer{User: "alice"}, "bob", "Also, W?", time.Now())
	f.deliver(msg, "alice")
	calls := f.waitOps(t, 1)
	if calls[0].Op != "notify" || !strings.Contains(calls[0].Title, "^alice replied") {
		t.Fatalf("calls = %+v", calls)
	}
	time.Sleep(50 * time.Millisecond)
	if ops := f.app.Ops(); len(ops) != 1 {
		t.Fatalf("unexpected app calls in inbox mode: %v", ops)
	}
	th, _ := f.store.Get(ctx, ask.ID)
	if th.LocalSessionID != "bob-sess" || !th.Opened || !strings.HasSuffix(th.StagedKickoff, "---\nAlso, W?") {
		t.Fatalf("thread = %+v", th)
	}
	if got := ContinueSession(f.bob.cfg, th); got != "bob-sess" {
		t.Fatalf("ContinueSession = %q", got)
	}
}

// agentReplies is what `wormhole reply` does from inside the kickoff:
// it needs the thread and the ask on disk, and it leaves the thread in
// state replied. It fails the way the CLI would when they are missing.
func agentReplies(t *testing.T, f *fixture, ask envelope.Envelope) error {
	t.Helper()
	ctx := context.Background()
	th, err := f.store.Get(ctx, ask.ID)
	if err != nil {
		return err
	}
	last, err := f.store.LastMessage(ctx, ask.ID)
	if err != nil {
		return err
	}
	if last.ID != ask.ID {
		return errors.New("last message is not the ask")
	}
	reply := envelope.NewFollowUp(envelope.TypeReply, ask, envelope.Peer{User: "bob", Host: "bob-laptop"}, "alice", "pong", time.Now())
	th.State = threads.StateReplied
	if err := f.store.Put(ctx, th); err != nil {
		return err
	}
	return f.store.AddMessage(ctx, reply, threads.DirectionOut)
}

func TestAgentCanReplyWhileKickoffRuns(t *testing.T) {
	// M0: the agent inside the headless session ran `wormhole reply` and
	// got "thread is not on this machine", because the daemon wrote the
	// thread only after the kickoff process exited.
	f := newFixture(t, autoCfg)
	var replyErr error
	f.app.Fail = func(op string) error {
		if op == "create" {
			replyErr = agentReplies(t, f, f.lastAsk)
		}
		return nil
	}
	runDaemon(t, f)
	ask := aliceAsk("Reply with the single word pong.")
	f.lastAsk = ask
	f.deliver(ask, "alice")
	acks := f.waitAck(t, 1)
	if acks[0].Env.Body != "delivered" {
		t.Fatalf("ack = %+v", acks[0].Env)
	}
	if replyErr != nil {
		t.Fatalf("reply from inside the kickoff failed: %v", replyErr)
	}
	calls := f.waitOps(t, 3)
	th, err := f.store.Get(context.Background(), ask.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon must not overwrite the state the reply set.
	if th.State != threads.StateReplied || th.LocalSessionID != calls[0].Session.ID || !th.Opened {
		t.Fatalf("thread = %+v", th)
	}
	msgs, _ := f.store.Messages(context.Background(), ask.ID)
	if len(msgs) != 2 || msgs[0].Direction != threads.DirectionIn || msgs[1].Direction != threads.DirectionOut {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestKickoffErrorAfterReplyKeepsThread(t *testing.T) {
	// The agent answered, then the copilot process ended badly (timeout,
	// non-zero exit). That is a delivered ask, not one to run again.
	f := newFixture(t, autoCfg)
	f.app.Fail = func(op string) error {
		if op == "create" {
			if err := agentReplies(t, f, f.lastAsk); err != nil {
				return err
			}
			return errors.New("copilot exited 1 after the reply")
		}
		return nil
	}
	runDaemon(t, f)
	ask := aliceAsk("Reply with the single word pong.")
	f.lastAsk = ask
	f.deliver(ask, "alice")
	acks := f.waitAck(t, 1)
	if acks[0].Env.Body != "delivered" {
		t.Fatalf("ack = %+v", acks[0].Env)
	}
	f.waitOps(t, 3)
	creates := 0
	for _, op := range f.app.Ops() {
		if op == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("create attempts = %d, want 1", creates)
	}
	th, err := f.store.Get(context.Background(), ask.ID)
	if err != nil || th.State != threads.StateReplied {
		t.Fatalf("thread = %+v err = %v", th, err)
	}
}

func TestFollowUpSendableWhileKickoffRuns(t *testing.T) {
	// Same ordering for a reply that opens a new session: a `wormhole
	// send` from inside it needs the reply on disk to build the next hop.
	f := newFixture(t, autoCfg)
	var sawReply error
	f.app.Fail = func(op string) error {
		if op == "create" {
			last, err := f.store.LastMessage(context.Background(), f.lastAsk.ThreadID)
			if err != nil {
				sawReply = err
			} else if last.Type != envelope.TypeReply || last.Direction != threads.DirectionIn {
				sawReply = errors.New("last message is not the inbound reply")
			}
		}
		return nil
	}
	runDaemon(t, f)
	ask := outboundThread(t, f, "")
	f.lastAsk = ask
	reply := envelope.NewFollowUp(envelope.TypeReply, ask, envelope.Peer{User: "alice"}, "bob", "Y is fine.", time.Now())
	f.deliver(reply, "alice")
	f.waitAck(t, 1)
	calls := f.waitOps(t, 3)
	if sawReply != nil {
		t.Fatalf("inside the kickoff: %v", sawReply)
	}
	th, _ := f.store.Get(context.Background(), ask.ID)
	if th.State != threads.StateReplied || th.LocalSessionID != calls[0].Session.ID {
		t.Fatalf("thread = %+v", th)
	}
}
