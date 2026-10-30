package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// ErrNothingStaged is returned by Decline when the thread has no message
// waiting in the inbox: it was already opened, declined or never staged.
var ErrNothingStaged = errors.New("nothing is waiting on this thread")

// Decline tells the peer that the person read the staged message and passed
// on it, and closes the thread on this machine. The receipt reuses the ack
// type with body `declined` or `declined: <reason>`, so the peer's daemon
// moves its side of the thread to `declined` without any schema change.
// `wormhole decline`, the notification's Decline button and `wormhole inbox
// --watch` all come through here.
func Decline(ctx context.Context, b bus.Bus, store *threads.Store, me envelope.Peer, t threads.Thread, reason string, now time.Time) (envelope.Envelope, error) {
	if t.StagedKickoff == "" {
		return envelope.Envelope{}, fmt.Errorf("thread %s: %w", t.ID, ErrNothingStaged)
	}
	last, err := store.LastMessage(ctx, t.ID)
	if err != nil {
		return envelope.Envelope{}, fmt.Errorf("thread %s has no messages: %w", t.ID, err)
	}
	prev, err := last.Envelope()
	if err != nil {
		return envelope.Envelope{}, err
	}
	ack := envelope.NewAck(prev, me, envelope.DeclineStatus(reason), now)
	ack.To.User = t.Peer
	if err := ack.Validate(now); err != nil {
		return envelope.Envelope{}, fmt.Errorf("decline receipt: %w", err)
	}
	if err := b.Publish(ctx, bus.SubjectFor(ack), ack); err != nil {
		return envelope.Envelope{}, fmt.Errorf("publish decline: %w", err)
	}
	// The receipt is on the bus; the local record follows even if a step
	// here fails, so the state never claims the message is still waiting.
	_, err = store.Update(ctx, t.ID, func(u *threads.Thread) {
		u.State = threads.StateDeclined
		u.StagedKickoff = ""
		u.SnoozedUntil = time.Time{}
		u.LastPreview = ack.Body
	})
	if err != nil {
		return ack, err
	}
	return ack, store.AddMessage(ctx, ack, threads.DirectionOut)
}
