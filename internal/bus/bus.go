// Package bus defines the transport interface Wormhole needs from a message
// bus. An adapter must provide exactly three things: at-least-once delivery
// with explicit ack/nak, a durable per-user subscription, and a small
// registry of handles with presence. Anything a transport cannot do natively
// (per-user ACLs, retention) is a documented deployment requirement for that
// adapter, not something emulated here.
//
// # Subjects
//
// Subjects carry both ends of a message so that the server, not the
// envelope, is what binds a message to its sender:
//
//	wh.inbox.<to>.<from>   ask, reply and message envelopes
//	wh.ack.<to>.<from>     delivery receipts
//	wh.dlq.<user>          messages a daemon gave up on
//
// A user's credentials may only publish on subjects whose last token is
// their own handle (wh.inbox.*.<self>, wh.ack.*.<self>, wh.dlq.<self>) and
// only subscribe to their own inbox and ack subjects. The daemon then trusts
// from.user only when it equals the sender token of the subject it arrived
// on, never the envelope alone.
package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// Prefix is the first subject token of every Wormhole subject.
const Prefix = "wh"

// Subject kinds.
const (
	KindInbox = "inbox"
	KindAck   = "ack"
	KindDLQ   = "dlq"
)

// ErrSubject is returned for a subject that does not follow the scheme.
var ErrSubject = errors.New("malformed subject")

// ErrPermission is returned when a user tries to publish or subscribe on a
// subject their credentials do not allow.
var ErrPermission = errors.New("permission denied")

// ErrUnknownMessage is returned by Ack/Nak for an id that is not in flight.
var ErrUnknownMessage = errors.New("unknown message id")

// InboxSubject is where from sends asks, replies and messages to to.
func InboxSubject(to, from string) string {
	return fmt.Sprintf("%s.%s.%s.%s", Prefix, KindInbox, to, from)
}

// AckSubject is where from sends delivery receipts to to.
func AckSubject(to, from string) string {
	return fmt.Sprintf("%s.%s.%s.%s", Prefix, KindAck, to, from)
}

// DLQSubject is where user's own daemon parks messages it gave up on.
func DLQSubject(user string) string {
	return fmt.Sprintf("%s.%s.%s", Prefix, KindDLQ, user)
}

// SubjectFor picks the subject an envelope must be published on.
func SubjectFor(e envelope.Envelope) string {
	if e.Type == envelope.TypeAck {
		return AckSubject(e.To.User, e.From.User)
	}
	return InboxSubject(e.To.User, e.From.User)
}

// Parsed is the decomposition of a subject.
type Parsed struct {
	Kind string
	To   string
	From string // empty for dlq subjects
}

// ParseSubject splits a subject into its kind and parties.
func ParseSubject(s string) (Parsed, error) {
	parts := strings.Split(s, ".")
	if len(parts) < 3 || parts[0] != Prefix {
		return Parsed{}, fmt.Errorf("%w: %q", ErrSubject, s)
	}
	switch parts[1] {
	case KindInbox, KindAck:
		if len(parts) != 4 || !envelope.ValidHandle(parts[2]) || !envelope.ValidHandle(parts[3]) {
			return Parsed{}, fmt.Errorf("%w: %q", ErrSubject, s)
		}
		return Parsed{Kind: parts[1], To: parts[2], From: parts[3]}, nil
	case KindDLQ:
		if len(parts) != 3 || !envelope.ValidHandle(parts[2]) {
			return Parsed{}, fmt.Errorf("%w: %q", ErrSubject, s)
		}
		return Parsed{Kind: parts[1], To: parts[2]}, nil
	}
	return Parsed{}, fmt.Errorf("%w: %q", ErrSubject, s)
}

// MayPublish reports whether user is allowed to publish on subject under
// the scheme above. Adapters whose server enforces this natively (NATS)
// still call it so the error surfaces locally and immediately.
func MayPublish(user, subject string) bool {
	p, err := ParseSubject(subject)
	if err != nil {
		return false
	}
	switch p.Kind {
	case KindInbox, KindAck:
		return p.From == user
	case KindDLQ:
		return p.To == user
	}
	return false
}

// MessageID identifies one delivery of one message for Ack and Nak. It is
// opaque and adapter-specific.
type MessageID string

// Delivery is one inbound message handed to a Handler.
type Delivery struct {
	ID      MessageID
	Subject string
	// Sender is the sender token taken from the subject: the identity the
	// server authenticated, as opposed to Env.From.User which is a claim.
	Sender string
	Env    envelope.Envelope
	// Attempt is 1 for the first delivery and increments on redelivery.
	Attempt int
}

// Handler receives deliveries one at a time. It must call Bus.Ack or
// Bus.Nak for every delivery it is handed; an unacknowledged delivery is
// redelivered by the adapter after its ack wait.
type Handler func(ctx context.Context, d Delivery)

// Subscription is a running durable subscription.
type Subscription interface {
	// Stop stops delivery. Messages not yet acked stay on the bus.
	Stop() error
}

// Presence is what each daemon publishes about itself to the registry.
type Presence struct {
	Handle   string    `json:"handle"`
	Host     string    `json:"host"`
	OS       string    `json:"os"`
	Mode     string    `json:"mode"`
	LastSeen time.Time `json:"last_seen"`
}

// Registry is the handle directory. Each daemon writes its own entry on
// start and periodically; anyone can list it.
type Registry interface {
	Put(ctx context.Context, p Presence) error
	List(ctx context.Context) ([]Presence, error)
}

// Bus is one authenticated connection to the message bus.
type Bus interface {
	// User is the handle this connection is authenticated as.
	User() string
	// Publish sends env on subject. It returns ErrPermission when the
	// connection may not publish there.
	Publish(ctx context.Context, subject string, env envelope.Envelope) error
	// Subscribe starts the durable, at-least-once subscription for this
	// user's inbox and ack subjects.
	Subscribe(ctx context.Context, h Handler) (Subscription, error)
	// Ack marks a delivery as handled; it will not be redelivered.
	Ack(ctx context.Context, id MessageID) error
	// Nak asks for redelivery after delay.
	Nak(ctx context.Context, id MessageID, delay time.Duration) error
	// Registry gives access to the handle directory.
	Registry() Registry
	// Close releases the connection. In-flight deliveries are redelivered
	// later by the bus.
	Close() error
}
