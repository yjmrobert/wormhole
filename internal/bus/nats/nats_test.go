package nats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats-server/v2/server"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// startServer runs an embedded nats-server using the shipped accounts
// template, so the permission lists in deploy/nats are what gets tested.
func startServer(t *testing.T) (url string) {
	t.Helper()
	accounts, err := filepath.Abs(filepath.Join("..", "..", "..", "deploy", "nats", "accounts.conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// include paths resolve relative to the including file: copy the
	// template next to the generated config.
	tpl, err := os.ReadFile(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accounts.conf"), tpl, 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "nats.conf")
	content := fmt.Sprintf("listen: 127.0.0.1:-1\nmax_payload: 64KB\njetstream { store_dir: %q }\ninclude ./accounts.conf\n", filepath.Join(dir, "js"))
	if err := os.WriteFile(conf, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := server.ProcessConfigFile(conf)
	if err != nil {
		t.Fatalf("process config: %v", err)
	}
	opts.NoLog, opts.NoSigs = true, true
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(srv.Shutdown)
	admin, err := AdminConnect(srv.ClientURL(), "admin", "CHANGE_ME_admin", "")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := EnsureInfrastructure(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	return srv.ClientURL()
}

func connect(t *testing.T, url, user string) *Bus {
	t.Helper()
	b, err := Connect(context.Background(), Options{URL: url, User: user, Password: "CHANGE_ME_" + user, AckWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

type collector struct {
	mu   sync.Mutex
	got  []bus.Delivery
	wake chan struct{}
}

func (c *collector) wait(t *testing.T, n int) []bus.Delivery {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		if len(c.got) >= n {
			out := append([]bus.Delivery(nil), c.got...)
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %d deliveries", n)
		}
	}
}

func ask(from, to, body string) envelope.Envelope {
	return envelope.NewAsk(envelope.Peer{User: from, Host: from + "-laptop"}, to, body, time.Now())
}

func TestRoundTripAndAck(t *testing.T) {
	url := startServer(t)
	alice := connect(t, url, "alice")
	bob := connect(t, url, "bob")
	ctx := context.Background()

	// Durable: publish before bob subscribes.
	a1 := ask("alice", "bob", "first")
	if err := alice.Publish(ctx, bus.SubjectFor(a1), a1); err != nil {
		t.Fatal(err)
	}
	// Duplicate publish of the same id is dropped by the server.
	if err := alice.Publish(ctx, bus.SubjectFor(a1), a1); err != nil {
		t.Fatal(err)
	}
	a2 := ask("alice", "bob", "second")
	if err := alice.Publish(ctx, bus.SubjectFor(a2), a2); err != nil {
		t.Fatal(err)
	}

	c := &collector{wake: make(chan struct{}, 10)}
	sub, err := bob.Subscribe(ctx, func(ctx context.Context, d bus.Delivery) {
		c.mu.Lock()
		c.got = append(c.got, d)
		c.mu.Unlock()
		if err := bob.Ack(ctx, d.ID); err != nil {
			t.Errorf("ack: %v", err)
		}
		c.wake <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Stop()
	got := c.wait(t, 2)
	if got[0].Env.Body != "first" || got[1].Env.Body != "second" || got[0].Sender != "alice" || got[0].Attempt != 1 {
		t.Fatalf("%+v", got)
	}
	time.Sleep(100 * time.Millisecond)
	c.mu.Lock()
	n := len(c.got)
	c.mu.Unlock()
	if n != 2 {
		t.Fatalf("duplicate publish delivered: %d", n)
	}

	// Acks flow back on the ack subject.
	ac := &collector{wake: make(chan struct{}, 10)}
	asub, _ := alice.Subscribe(ctx, func(ctx context.Context, d bus.Delivery) {
		ac.mu.Lock()
		ac.got = append(ac.got, d)
		ac.mu.Unlock()
		_ = alice.Ack(ctx, d.ID)
		ac.wake <- struct{}{}
	})
	defer asub.Stop()
	receipt := envelope.NewAck(a1, envelope.Peer{User: "bob"}, "delivered", time.Now())
	if err := bob.Publish(ctx, bus.SubjectFor(receipt), receipt); err != nil {
		t.Fatal(err)
	}
	r := ac.wait(t, 1)
	if r[0].Env.Type != envelope.TypeAck || r[0].Sender != "bob" || *r[0].Env.InReplyTo != a1.ID {
		t.Fatalf("%+v", r[0])
	}

	st, err := bob.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 0 || st.DLQ != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestServerEnforcesSenderIdentity(t *testing.T) {
	url := startServer(t)
	alice := connect(t, url, "alice")
	ctx := context.Background()
	// Local guard.
	spoof := ask("carol", "bob", "spoof")
	if err := alice.Publish(ctx, bus.SubjectFor(spoof), spoof); !errors.Is(err, bus.ErrPermission) {
		t.Fatalf("local guard: %v", err)
	}
	// Server guard: bypass the adapter and publish raw.
	nc, err := natsgo.Connect(url, natsgo.UserInfo("alice", "CHANGE_ME_alice"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	errCh := make(chan error, 1)
	nc.SetErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errCh <- err })
	data, _ := envelope.Marshal(spoof)
	if err := nc.Publish(bus.InboxSubject("bob", "carol"), data); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	select {
	case err := <-errCh:
		if !errors.Is(err, natsgo.ErrPermissionViolation) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server accepted a publish with a forged sender token")
	}
	// Nor can alice read bob's inbox.
	if _, err := nc.SubscribeSync(bus.InboxSubject("bob", "alice")); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	select {
	case err := <-errCh:
		if !errors.Is(err, natsgo.ErrPermissionViolation) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server let alice subscribe to bob's inbox")
	}
}

func TestNakRedeliveryAndDLQ(t *testing.T) {
	url := startServer(t)
	alice := connect(t, url, "alice")
	bob := connect(t, url, "bob")
	ctx := context.Background()
	a := ask("alice", "bob", "flaky")
	if err := alice.Publish(ctx, bus.SubjectFor(a), a); err != nil {
		t.Fatal(err)
	}
	c := &collector{wake: make(chan struct{}, 10)}
	sub, err := bob.Subscribe(ctx, func(ctx context.Context, d bus.Delivery) {
		c.mu.Lock()
		c.got = append(c.got, d)
		n := len(c.got)
		c.mu.Unlock()
		if n < 3 {
			_ = bob.Nak(ctx, d.ID, 50*time.Millisecond)
		} else {
			// Give up: park it and ack, as the daemon does.
			if err := bob.Publish(ctx, bus.DLQSubject("bob"), d.Env); err != nil {
				t.Errorf("dlq publish: %v", err)
			}
			_ = bob.Ack(ctx, d.ID)
		}
		c.wake <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Stop()
	got := c.wait(t, 3)
	for i := range got {
		if got[i].Attempt != i+1 {
			t.Fatalf("attempt %d reported as %d", i+1, got[i].Attempt)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	var st Status
	for time.Now().Before(deadline) {
		st, err = bob.Status(ctx)
		if err == nil && st.DLQ == 1 && st.Pending == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || st.DLQ != 1 || st.Pending != 0 {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestRegistry(t *testing.T) {
	url := startServer(t)
	alice := connect(t, url, "alice")
	bob := connect(t, url, "bob")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := alice.Registry().Put(ctx, bus.Presence{Handle: "alice", Host: "alice-laptop", OS: "linux", Mode: "inbox", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := bob.Registry().Put(ctx, bus.Presence{Handle: "bob", Host: "bob-laptop", OS: "linux", Mode: "inbox", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := alice.Registry().Put(ctx, bus.Presence{Handle: "bob", Host: "x"}); !errors.Is(err, bus.ErrPermission) {
		t.Fatalf("alice wrote bob's presence: %v", err)
	}
	list, err := bob.Registry().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	byHandle := map[string]bus.Presence{}
	for _, p := range list {
		byHandle[p.Handle] = p
	}
	if byHandle["alice"].Host != "alice-laptop" || byHandle["bob"].Host != "bob-laptop" || !byHandle["bob"].LastSeen.Equal(now) {
		t.Fatalf("list = %+v", list)
	}
}
