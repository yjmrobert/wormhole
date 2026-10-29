package bus

import (
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

func TestSubjects(t *testing.T) {
	if got := InboxSubject("bob", "alice"); got != "wh.inbox.bob.alice" {
		t.Fatal(got)
	}
	if got := AckSubject("alice", "bob"); got != "wh.ack.alice.bob" {
		t.Fatal(got)
	}
	if got := DLQSubject("bob"); got != "wh.dlq.bob" {
		t.Fatal(got)
	}
	ask := envelope.NewAsk(envelope.Peer{User: "alice"}, "bob", "q", time.Now())
	if SubjectFor(ask) != "wh.inbox.bob.alice" {
		t.Fatal(SubjectFor(ask))
	}
	ack := envelope.NewAck(ask, envelope.Peer{User: "bob"}, "delivered", time.Now())
	if SubjectFor(ack) != "wh.ack.alice.bob" {
		t.Fatal(SubjectFor(ack))
	}
}

func TestParseSubject(t *testing.T) {
	p, err := ParseSubject("wh.inbox.bob.alice")
	if err != nil || p.Kind != KindInbox || p.To != "bob" || p.From != "alice" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = ParseSubject("wh.dlq.bob")
	if err != nil || p.Kind != KindDLQ || p.To != "bob" {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{"", "wh", "wh.inbox.bob", "wh.inbox.bob.alice.extra", "xx.inbox.bob.alice", "wh.inbox.Bob.alice", "wh.inbox.*.alice", "wh.dlq.bob.alice"} {
		if _, err := ParseSubject(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestMayPublish(t *testing.T) {
	ok := []string{"wh.inbox.bob.alice", "wh.ack.bob.alice", "wh.dlq.alice"}
	for _, s := range ok {
		if !MayPublish("alice", s) {
			t.Errorf("alice should publish on %s", s)
		}
	}
	bad := []string{"wh.inbox.bob.carol", "wh.ack.alice.bob", "wh.dlq.bob", "wh.inbox.alice.alice.x", "garbage"}
	for _, s := range bad {
		if MayPublish("alice", s) {
			t.Errorf("alice should not publish on %s", s)
		}
	}
}
