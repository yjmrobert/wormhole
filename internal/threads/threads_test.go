package threads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state", "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestThreadLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	ask := envelope.NewAsk(envelope.Peer{User: "alice", SessionID: "sess-1"}, "bob", "What about X?", now)
	th := Thread{ID: ask.ID, Peer: "bob", Direction: DirectionOut, State: StateSent, RootSessionID: "sess-1", Title: "What about X?", ExpiresAt: ask.ExpiresAt}
	if err := s.Put(ctx, th); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(ctx, ask, DirectionOut); err != nil {
		t.Fatal(err)
	}
	// Idempotent on redelivery.
	if err := s.AddMessage(ctx, ask, DirectionOut); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, ask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSent || got.LastMessageID != ask.ID || got.LastPreview != "What about X?" {
		t.Fatalf("%+v", got)
	}
	if _, err := s.Update(ctx, ask.ID, func(t *Thread) { t.State = StateDelivered }); err != nil {
		t.Fatal(err)
	}
	reply := envelope.NewFollowUp(envelope.TypeReply, ask, envelope.Peer{User: "bob"}, "alice", "I think Y.", now.Add(time.Minute))
	if err := s.AddMessage(ctx, reply, DirectionIn); err != nil {
		t.Fatal(err)
	}
	last, err := s.LastMessage(ctx, ask.ID)
	if err != nil || last.ID != reply.ID {
		t.Fatalf("%+v %v", last, err)
	}
	env, err := last.Envelope()
	if err != nil || env.Hop != 2 {
		t.Fatalf("%+v %v", env, err)
	}
	msgs, _ := s.Messages(ctx, ask.ID)
	if len(msgs) != 2 || msgs[0].ID != ask.ID {
		t.Fatalf("%+v", msgs)
	}
	open, _ := s.List(ctx, Filter{Open: true, Peer: "bob"})
	if len(open) != 1 {
		t.Fatalf("open threads: %d", len(open))
	}
	if _, err := s.Update(ctx, ask.ID, func(t *Thread) { t.State = StateClosed }); err != nil {
		t.Fatal(err)
	}
	open, _ = s.List(ctx, Filter{Open: true})
	if len(open) != 0 {
		t.Fatalf("closed thread still open")
	}
	if _, err := s.Get(ctx, envelope.NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing thread: %v", err)
	}
}

func TestCountInboundAndExpire(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 3; i++ {
		ask := envelope.NewAsk(envelope.Peer{User: "alice"}, "bob", "q", now)
		_ = s.Put(ctx, Thread{ID: ask.ID, Peer: "alice", Direction: DirectionIn, State: StatePending, ExpiresAt: ask.ExpiresAt})
		_ = s.AddMessage(ctx, ask, DirectionIn)
	}
	old := envelope.NewAsk(envelope.Peer{User: "carol"}, "bob", "q", now.Add(-48*time.Hour))
	_ = s.Put(ctx, Thread{ID: old.ID, Peer: "carol", Direction: DirectionIn, State: StatePending, ExpiresAt: old.ExpiresAt})
	_ = s.AddMessage(ctx, old, DirectionIn)
	n, _ := s.CountInbound(ctx, "alice", now.Add(-24*time.Hour))
	if n != 3 {
		t.Fatalf("alice count = %d", n)
	}
	n, _ = s.CountInbound(ctx, "", now.Add(-24*time.Hour))
	if n != 3 {
		t.Fatalf("total count = %d", n)
	}
	n, _ = s.CountInbound(ctx, "", now.Add(-72*time.Hour))
	if n != 4 {
		t.Fatalf("total 72h count = %d", n)
	}

	stale := envelope.NewAsk(envelope.Peer{User: "bob"}, "alice", "q", now.Add(-8*24*time.Hour))
	_ = s.Put(ctx, Thread{ID: stale.ID, Peer: "alice", Direction: DirectionOut, State: StateSent, ExpiresAt: stale.ExpiresAt})
	changed, err := s.ExpireStale(ctx, now)
	if err != nil || changed != 1 {
		t.Fatalf("expired %d, %v", changed, err)
	}
	th, _ := s.Get(ctx, stale.ID)
	if th.State != StateExpired {
		t.Fatal(th.State)
	}
}

func TestSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != SchemaVersion {
		t.Fatalf("user_version = %d, %v; want %d", v, err, SchemaVersion)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// A newer file is refused rather than misread.
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open newer schema: %v", err)
	}
}

func TestFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("threads db is not private: %v", info.Mode())
	}
}

func TestPreview(t *testing.T) {
	if got := Preview("  hello\n  world  ", 80); got != "hello world" {
		t.Fatal(got)
	}
	if got := Preview("abcdefghij", 5); got != "abcd…" {
		t.Fatal(got)
	}
}
