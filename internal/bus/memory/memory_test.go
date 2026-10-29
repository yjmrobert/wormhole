package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

func ask(from, to, body string) envelope.Envelope {
	return envelope.NewAsk(envelope.Peer{User: from, Host: from + "-laptop"}, to, body, time.Now())
}

type collector struct {
	mu   sync.Mutex
	got  []bus.Delivery
	wake chan struct{}
}

func newCollector() *collector { return &collector{wake: make(chan struct{}, 100)} }

func (c *collector) handler(b bus.Bus, ack bool) bus.Handler {
	return func(ctx context.Context, d bus.Delivery) {
		c.mu.Lock()
		c.got = append(c.got, d)
		c.mu.Unlock()
		if ack {
			_ = b.Ack(ctx, d.ID)
		}
		c.wake <- struct{}{}
	}
}

func (c *collector) wait(t *testing.T, n int) []bus.Delivery {
	t.Helper()
	deadline := time.After(5 * time.Second)
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

func TestDurableDeliveryInOrder(t *testing.T) {
	br := NewBroker()
	defer br.Close()
	alice, _ := br.Connect("alice")
	bob, _ := br.Connect("bob")
	ctx := context.Background()
	// Publish before bob subscribes: the queue must hold the messages.
	for _, body := range []string{"one", "two", "three"} {
		if err := alice.Publish(ctx, bus.InboxSubject("bob", "alice"), ask("alice", "bob", body)); err != nil {
			t.Fatal(err)
		}
	}
	c := newCollector()
	sub, err := bob.Subscribe(ctx, c.handler(bob, true))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Stop()
	got := c.wait(t, 3)
	for i, want := range []string{"one", "two", "three"} {
		if got[i].Env.Body != want || got[i].Sender != "alice" || got[i].Attempt != 1 {
			t.Fatalf("delivery %d = %+v", i, got[i])
		}
	}
	if br.Pending("bob") != 0 {
		t.Fatalf("pending after ack: %d", br.Pending("bob"))
	}
}

func TestPermissions(t *testing.T) {
	br := NewBroker()
	defer br.Close()
	alice, _ := br.Connect("alice")
	ctx := context.Background()
	err := alice.Publish(ctx, bus.InboxSubject("bob", "carol"), ask("carol", "bob", "spoof"))
	if !errors.Is(err, bus.ErrPermission) {
		t.Fatalf("spoofed sender accepted: %v", err)
	}
	if err := alice.Publish(ctx, bus.DLQSubject("bob"), ask("alice", "bob", "x")); !errors.Is(err, bus.ErrPermission) {
		t.Fatalf("foreign dlq accepted: %v", err)
	}
	if _, err := br.Connect("Not Valid"); err == nil {
		t.Fatal("invalid handle connected")
	}
}

func TestNakRedeliversThenDLQ(t *testing.T) {
	br := NewBroker(WithMaxDeliver(3))
	defer br.Close()
	alice, _ := br.Connect("alice")
	bob, _ := br.Connect("bob")
	ctx := context.Background()
	if err := alice.Publish(ctx, bus.InboxSubject("bob", "alice"), ask("alice", "bob", "flaky")); err != nil {
		t.Fatal(err)
	}
	c := newCollector()
	sub, _ := bob.Subscribe(ctx, func(ctx context.Context, d bus.Delivery) {
		c.mu.Lock()
		c.got = append(c.got, d)
		c.mu.Unlock()
		_ = bob.Nak(ctx, d.ID, 10*time.Millisecond)
		c.wake <- struct{}{}
	})
	defer sub.Stop()
	got := c.wait(t, 3)
	for i := range got {
		if got[i].Attempt != i+1 {
			t.Fatalf("attempt %d reported as %d", i+1, got[i].Attempt)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(br.DLQ("bob")) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if dl := br.DLQ("bob"); len(dl) != 1 || dl[0].Body != "flaky" {
		t.Fatalf("dlq = %+v", dl)
	}
	if br.Pending("bob") != 0 {
		t.Fatal("message still pending after dlq")
	}
	if err := bob.Ack(ctx, got[0].ID); !errors.Is(err, bus.ErrUnknownMessage) {
		t.Fatalf("stale ack: %v", err)
	}
}

func TestAckWaitRedelivers(t *testing.T) {
	br := NewBroker(WithAckWait(20 * time.Millisecond))
	defer br.Close()
	alice, _ := br.Connect("alice")
	bob, _ := br.Connect("bob")
	ctx := context.Background()
	_ = alice.Publish(ctx, bus.InboxSubject("bob", "alice"), ask("alice", "bob", "slow"))
	c := newCollector()
	sub, _ := bob.Subscribe(ctx, func(ctx context.Context, d bus.Delivery) {
		c.mu.Lock()
		c.got = append(c.got, d)
		n := len(c.got)
		c.mu.Unlock()
		if n == 2 {
			_ = bob.Ack(ctx, d.ID)
		}
		c.wake <- struct{}{}
	})
	defer sub.Stop()
	got := c.wait(t, 2)
	if got[1].Attempt != 2 {
		t.Fatalf("second delivery attempt = %d", got[1].Attempt)
	}
}

func TestOneInFlightAtATime(t *testing.T) {
	br := NewBroker()
	defer br.Close()
	alice, _ := br.Connect("alice")
	bob, _ := br.Connect("bob")
	ctx := context.Background()
	_ = alice.Publish(ctx, bus.InboxSubject("bob", "alice"), ask("alice", "bob", "first"))
	_ = alice.Publish(ctx, bus.InboxSubject("bob", "alice"), ask("alice", "bob", "second"))
	c := newCollector()
	var first bus.Delivery
	sub, _ := bob.Subscribe(ctx, func(ctx context.Context, d bus.Delivery) {
		c.mu.Lock()
		c.got = append(c.got, d)
		if len(c.got) == 1 {
			first = d
		}
		c.mu.Unlock()
		c.wake <- struct{}{}
	})
	defer sub.Stop()
	c.wait(t, 1)
	time.Sleep(30 * time.Millisecond)
	c.mu.Lock()
	n := len(c.got)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("second message delivered while first in flight: %d", n)
	}
	_ = bob.Ack(ctx, first.ID)
	got := c.wait(t, 2)
	if got[1].Env.Body != "second" {
		t.Fatal(got[1].Env.Body)
	}
}

func TestRegistry(t *testing.T) {
	br := NewBroker()
	defer br.Close()
	alice, _ := br.Connect("alice")
	bob, _ := br.Connect("bob")
	ctx := context.Background()
	_ = alice.Registry().Put(ctx, bus.Presence{Handle: "alice", Host: "alice-laptop", OS: "linux", Mode: "inbox", LastSeen: time.Now()})
	_ = bob.Registry().Put(ctx, bus.Presence{Handle: "bob", Host: "bob-laptop", OS: "linux", Mode: "inbox", LastSeen: time.Now()})
	list, err := alice.Registry().List(ctx)
	if err != nil || len(list) != 2 || list[0].Handle != "alice" || list[1].Handle != "bob" {
		t.Fatalf("%+v %v", list, err)
	}
}
