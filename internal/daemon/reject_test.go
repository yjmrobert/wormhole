package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// TestRejectReasonsNameTheFix drives every rejection a sender can meet and
// checks that the reason, which the asker reads as "rejected: <reason>",
// ends with what they can do about it.
func TestRejectReasonsNameTheFix(t *testing.T) {
	newAsk := `; start a new ask: wormhole ask ^bob "..."`
	cases := []struct {
		name  string
		cfg   string
		setup func(t *testing.T, f *fixture) envelope.Envelope
		want  string // the reason must end with this
	}{
		{
			name: "sender not allowed",
			cfg:  strings.Replace(inboxCfg, "senders: [alice]", "senders: [carol]", 1),
			setup: func(*testing.T, *fixture) envelope.Envelope {
				return aliceAsk("hi")
			},
			want: "sender not allowed; ask ^bob to run: wormhole senders add alice",
		},
		{
			name: "hop over max_hops",
			cfg:  inboxCfg,
			setup: func(*testing.T, *fixture) envelope.Envelope {
				e := aliceAsk("deep")
				e.Hop = 7
				return e
			},
			want: "hop 7 exceeds max_hops 6" + newAsk,
		},
		{
			name: "expired envelope",
			cfg:  inboxCfg,
			setup: func(*testing.T, *fixture) envelope.Envelope {
				e := aliceAsk("old")
				e.CreatedAt = time.Now().Add(-10 * 24 * time.Hour)
				e.ExpiresAt = time.Now().Add(-3 * 24 * time.Hour)
				return e
			},
			want: newAsk,
		},
		{
			name: "thread id in use",
			cfg:  strings.Replace(inboxCfg, "senders: [alice]", "senders: [alice, carol]", 1),
			setup: func(t *testing.T, f *fixture) envelope.Envelope {
				e := aliceAsk("again")
				put(t, f, threads.Thread{ID: e.ThreadID, Peer: "carol", Direction: threads.DirectionIn, State: threads.StatePending})
				return e
			},
			want: "thread id already in use" + newAsk,
		},
		{
			name: "not a party",
			cfg:  inboxCfg,
			setup: func(t *testing.T, f *fixture) envelope.Envelope {
				ask := aliceAsk("first")
				put(t, f, threads.Thread{ID: ask.ThreadID, Peer: "carol", Direction: threads.DirectionOut, State: threads.StateDelivered})
				return envelope.NewFollowUp(envelope.TypeMessage, ask, envelope.Peer{User: "alice"}, "bob", "me too", time.Now())
			},
			want: "not a party to this thread" + newAsk,
		},
		{
			name: "thread declined",
			cfg:  inboxCfg,
			setup: func(t *testing.T, f *fixture) envelope.Envelope {
				ask := aliceAsk("first")
				put(t, f, threads.Thread{ID: ask.ThreadID, Peer: "alice", Direction: threads.DirectionIn, State: threads.StateDeclined})
				return envelope.NewFollowUp(envelope.TypeMessage, ask, envelope.Peer{User: "alice"}, "bob", "please?", time.Now())
			},
			want: "thread declined" + newAsk,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.cfg)
			e := tc.setup(t, f)
			runDaemon(t, f)
			f.deliver(e, "alice")
			acks := f.waitAck(t, 1)
			reason, ok := acks[0].Env.Rejected()
			if !ok || !strings.HasSuffix(reason, tc.want) {
				t.Fatalf("reason = %q, want suffix %q", reason, tc.want)
			}
		})
	}
}

// put stores a thread row for a rejection case.
func put(t *testing.T, f *fixture, th threads.Thread) {
	t.Helper()
	now := time.Now()
	th.CreatedAt, th.LastMessageAt, th.ExpiresAt = now, now, now.Add(time.Hour)
	if err := f.store.Put(context.Background(), th); err != nil {
		t.Fatal(err)
	}
}
