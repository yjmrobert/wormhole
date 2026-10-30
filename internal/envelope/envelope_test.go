package envelope

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 29, 15, 44, 52, 0, time.UTC)

func alice() Peer {
	return Peer{User: "alice", Host: "alice-laptop", SessionID: "d29557f4-0000-4000-8000-000000000000"}
}

func TestNewAskIsValid(t *testing.T) {
	e := NewAsk(alice(), "bob", "What do you think about X?", now)
	if err := e.Validate(now); err != nil {
		t.Fatalf("valid ask rejected: %v", err)
	}
	if e.ThreadID != e.ID {
		t.Fatalf("thread_id %q != id %q", e.ThreadID, e.ID)
	}
	if e.Hop != 1 || e.RootSessionID != alice().SessionID {
		t.Fatalf("unexpected hop/root: %+v", e)
	}
	if !e.ExpiresAt.Equal(now.Add(DefaultTTL)) {
		t.Fatalf("expires_at = %v", e.ExpiresAt)
	}
}

func TestRoundTripJSON(t *testing.T) {
	e := NewAsk(alice(), "bob", "hello", now)
	e.Context = Context{Repo: "owner/repo", Ref: "PR #123"}
	b, err := Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"v":1`, `"type":"ask"`, `"in_reply_to":null`, `"cwd_hint":null`, `"human_reviewed":false`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json missing %s: %s", want, b)
		}
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != e.ID || got.Body != e.Body || got.Context.Repo != "owner/repo" || !got.CreatedAt.Equal(e.CreatedAt) {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, e)
	}
	if _, err := Unmarshal([]byte(`{"v":1,"bogus":true}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestFollowUpAndAck(t *testing.T) {
	ask := NewAsk(alice(), "bob", "q", now)
	bob := Peer{User: "bob", Host: "bob-laptop"}
	reply := NewFollowUp(TypeReply, ask, bob, "alice", "a", now.Add(time.Minute))
	if err := reply.Validate(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if reply.ThreadID != ask.ID || *reply.InReplyTo != ask.ID || reply.Hop != 2 || reply.RootSessionID != ask.RootSessionID {
		t.Fatalf("bad follow-up: %+v", reply)
	}
	ack := NewAck(ask, bob, "delivered", now)
	if err := ack.Validate(now); err != nil {
		t.Fatal(err)
	}
	if ack.To.User != "alice" || *ack.InReplyTo != ask.ID {
		t.Fatalf("bad ack: %+v", ack)
	}
	if _, rejected := ack.Rejected(); rejected {
		t.Fatal("delivered ack reported as rejected")
	}
	rej := NewAck(ask, bob, "rejected: sender not allowed", now)
	if reason, ok := rej.Rejected(); !ok || reason != "sender not allowed" {
		t.Fatalf("rejected ack not parsed: %q %v", reason, ok)
	}
	if _, ok := rej.Declined(); ok {
		t.Fatal("rejection reported as a decline")
	}
	dec := NewAck(ask, bob, DeclineStatus("  ask in standup "), now)
	if dec.Body != "declined: ask in standup" {
		t.Fatal(dec.Body)
	}
	if reason, ok := dec.Declined(); !ok || reason != "ask in standup" {
		t.Fatalf("declined ack not parsed: %q %v", reason, ok)
	}
	if _, ok := dec.Rejected(); ok {
		t.Fatal("decline reported as a rejection")
	}
	bare := NewAck(ask, bob, DeclineStatus(""), now)
	if reason, ok := bare.Declined(); !ok || reason != "" {
		t.Fatalf("bare decline not parsed: %q %v", reason, ok)
	}
	if _, ok := ask.Declined(); ok {
		t.Fatal("an ask is not a decline")
	}
}

func TestValidateRejects(t *testing.T) {
	base := func() Envelope { return NewAsk(alice(), "bob", "q", now) }
	cases := []struct {
		name string
		mut  func(*Envelope)
		want error
	}{
		{"version", func(e *Envelope) { e.V = 2 }, ErrVersion},
		{"id", func(e *Envelope) { e.ID = "nope" }, ErrID},
		{"type", func(e *Envelope) { e.Type = "shout" }, ErrType},
		{"thread", func(e *Envelope) { e.ThreadID = NewID() }, ErrThread},
		{"handle upper", func(e *Envelope) { e.To.User = "Bob" }, ErrHandle},
		{"handle dot", func(e *Envelope) { e.To.User = "bob.smith" }, ErrHandle},
		{"handle wildcard", func(e *Envelope) { e.From.User = "*" }, ErrHandle},
		{"hop zero", func(e *Envelope) { e.Hop = 0 }, ErrHop},
		{"body empty", func(e *Envelope) { e.Body = "  " }, ErrBody},
		{"body big", func(e *Envelope) { e.Body = strings.Repeat("x", MaxBodyBytes+1) }, ErrBody},
		{"body utf8", func(e *Envelope) { e.Body = "ok\xff" }, ErrBody},
		{"time order", func(e *Envelope) { e.ExpiresAt = e.CreatedAt }, ErrTime},
		{"expired", func(e *Envelope) { e.CreatedAt = now.Add(-time.Hour); e.ExpiresAt = now.Add(-time.Second) }, ErrExpired},
		{"reply without in_reply_to", func(e *Envelope) { e.Type = TypeReply }, ErrInReplyTo},
	}
	for _, c := range cases {
		e := base()
		c.mut(&e)
		err := e.Validate(now)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	if err := base().Validate(now); err != nil {
		t.Fatalf("base invalid: %v", err)
	}
}

func TestScanSecrets(t *testing.T) {
	hits := []string{
		"here is my key ghp_abcdefghijklmnopqrstuvwxyz0123",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
		"use password=hunter22 to log in",
		"AKIAABCDEFGHIJKLMNOP is the id",
		"curl https://user:pa55word@bus.example.internal/",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz",
		"token: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijklmnop",
	}
	for _, h := range hits {
		if len(ScanSecrets(h)) == 0 {
			t.Errorf("missed secret in %q", h)
		}
	}
	clean := []string{
		"What do you think about the retry policy in PR #123?",
		"The password field on the login form is misaligned.",
		"see https://github.com/owner/repo/pull/5",
		"we should rotate the token monthly",
	}
	for _, c := range clean {
		if ms := ScanSecrets(c); len(ms) != 0 {
			t.Errorf("false positive in %q: %v", c, ms)
		}
	}
}

func TestValidHandle(t *testing.T) {
	for _, ok := range []string{"alice", "bob2", "a", "dev-ops_1", strings.Repeat("a", 64)} {
		if !ValidHandle(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Alice", "-alice", "al.ice", "al ice", "al>ice", strings.Repeat("a", 65)} {
		if ValidHandle(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
