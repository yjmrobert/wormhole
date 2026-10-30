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
	if _, err := s.Update(ctx, ask.ID, func(t *Thread) { t.State = StateDeclined }); err != nil {
		t.Fatal(err)
	}
	open, _ = s.List(ctx, Filter{Open: true})
	if len(open) != 0 {
		t.Fatalf("declined thread still open")
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

func TestMigrateFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild a version-1 file: no snoozed_until column.
	for _, q := range []string{
		`DROP INDEX threads_peer`, `DROP TABLE messages`, `DROP TABLE threads`,
		strings.Replace(schema, "  snoozed_until TEXT NOT NULL DEFAULT '',\n", "", 1),
		`PRAGMA user_version = 1`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ask := envelope.NewAsk(envelope.Peer{User: "alice"}, "bob", "q", time.Now())
	if _, err := s.db.Exec(`INSERT INTO threads (id, peer, direction, state, last_message_at, expires_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		ask.ID, "alice", "in", "pending", "2026-10-29T00:00:00Z", "2026-11-05T00:00:00Z", "2026-10-29T00:00:00Z", "2026-10-29T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != SchemaVersion {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	th, err := s.Get(context.Background(), ask.ID)
	if err != nil || !th.SnoozedUntil.IsZero() {
		t.Fatalf("%+v %v", th, err)
	}
}

func TestSnooze(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	ask := envelope.NewAsk(envelope.Peer{User: "alice"}, "bob", "q", now)
	th := Thread{ID: ask.ID, Peer: "alice", Direction: DirectionIn, State: StatePending, StagedKickoff: "k", ExpiresAt: ask.ExpiresAt}
	if err := s.Put(ctx, th); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, ask.ID)
	if !got.SnoozedUntil.IsZero() || got.Snoozed(now) {
		t.Fatalf("fresh thread snoozed: %+v", got)
	}
	until := now.Add(2 * time.Hour)
	if _, err := s.Update(ctx, ask.ID, func(t *Thread) { t.SnoozedUntil = until }); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, ask.ID)
	if !got.SnoozedUntil.Equal(until) || !got.Snoozed(now) || got.Snoozed(until) {
		t.Fatalf("%+v", got)
	}
	staged, _ := s.List(ctx, Filter{Staged: true})
	if len(staged) != 1 {
		t.Fatalf("staged = %d", len(staged))
	}
	early := now.Add(time.Hour)
	if due, _ := s.List(ctx, Filter{SnoozeDue: &early}); len(due) != 0 {
		t.Fatalf("snooze due early: %d", len(due))
	}
	late := now.Add(3 * time.Hour)
	if due, _ := s.List(ctx, Filter{SnoozeDue: &late}); len(due) != 1 {
		t.Fatalf("snooze not due: %d", len(due))
	}
	// Opening clears the staged kickoff, and with it the snooze's reach.
	if _, err := s.Update(ctx, ask.ID, func(t *Thread) { t.StagedKickoff = ""; t.SnoozedUntil = time.Time{} }); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.List(ctx, Filter{SnoozeDue: &late}); len(due) != 0 {
		t.Fatalf("opened thread still due: %d", len(due))
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
