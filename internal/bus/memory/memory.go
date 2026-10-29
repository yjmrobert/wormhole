// Package memory is an in-process bus adapter for tests, demos and the
// end-to-end suite. One Broker plays the server; Connect returns a Bus
// bound to one user with the same publish and subscribe rules a NATS
// deployment enforces, so a test that passes here exercises the real
// subject scheme.
//
// Delivery semantics mirror a JetStream pull consumer with one message in
// flight: durable (messages published before a subscriber exists wait for
// it), at-least-once, redelivered after a nak delay or an ack wait, and
// parked on the user's dead-letter queue after MaxDeliver attempts.
package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// Defaults match the NATS deployment template.
const (
	DefaultMaxDeliver = 5
	DefaultAckWait    = 30 * time.Second
)

// Broker is the in-process server.
type Broker struct {
	mu         sync.Mutex
	seq        uint64
	queues     map[string]*queue
	registry   map[string]bus.Presence
	dlq        map[string][]envelope.Envelope
	maxDeliver int
	ackWait    time.Duration
	closed     bool
}

type message struct {
	seq        uint64
	subject    string
	env        envelope.Envelope
	deliveries int
	notBefore  time.Time
	inflight   bool
	ackDue     time.Time
}

type queue struct {
	pending []*message
	wake    chan struct{}
}

// Option configures a Broker.
type Option func(*Broker)

// WithMaxDeliver sets how many attempts a message gets before the DLQ.
func WithMaxDeliver(n int) Option { return func(b *Broker) { b.maxDeliver = n } }

// WithAckWait sets how long an unacknowledged delivery waits before redelivery.
func WithAckWait(d time.Duration) Option { return func(b *Broker) { b.ackWait = d } }

// NewBroker creates an empty broker.
func NewBroker(opts ...Option) *Broker {
	b := &Broker{
		queues:     map[string]*queue{},
		registry:   map[string]bus.Presence{},
		dlq:        map[string][]envelope.Envelope{},
		maxDeliver: DefaultMaxDeliver,
		ackWait:    DefaultAckWait,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

func (b *Broker) queueFor(user string) *queue {
	q, ok := b.queues[user]
	if !ok {
		q = &queue{wake: make(chan struct{}, 1)}
		b.queues[user] = q
	}
	return q
}

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// DLQ returns the dead-letter contents for user, oldest first.
func (b *Broker) DLQ(user string) []envelope.Envelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]envelope.Envelope(nil), b.dlq[user]...)
}

// Pending returns how many messages wait for user, including in-flight ones.
func (b *Broker) Pending(user string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if q, ok := b.queues[user]; ok {
		return len(q.pending)
	}
	return 0
}

// Connect returns a Bus authenticated as user. It mirrors what a NATS
// credentials file grants: publish only as oneself, subscribe only to
// one's own subjects.
func (b *Broker) Connect(user string) (bus.Bus, error) {
	if !envelope.ValidHandle(user) {
		return nil, fmt.Errorf("%w: %q", envelope.ErrHandle, user)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("broker closed")
	}
	b.queueFor(user)
	return &conn{broker: b, user: user, inflight: map[bus.MessageID]*message{}}, nil
}

// Close stops the broker. Existing connections stop delivering.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, q := range b.queues {
		q.signal()
	}
}

type conn struct {
	broker   *Broker
	user     string
	mu       sync.Mutex
	inflight map[bus.MessageID]*message
	closed   bool
}

func (c *conn) User() string { return c.user }

func (c *conn) Publish(ctx context.Context, subject string, env envelope.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !bus.MayPublish(c.user, subject) {
		return fmt.Errorf("%w: %s may not publish on %s", bus.ErrPermission, c.user, subject)
	}
	p, _ := bus.ParseSubject(subject)
	// Envelopes are carried as JSON on the wire; encode and decode here so
	// the memory adapter has the same fidelity as a real transport.
	raw, err := envelope.Marshal(env)
	if err != nil {
		return err
	}
	decoded, err := envelope.Unmarshal(raw)
	if err != nil {
		return err
	}
	b := c.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("broker closed")
	}
	if p.Kind == bus.KindDLQ {
		b.dlq[p.To] = append(b.dlq[p.To], decoded)
		return nil
	}
	b.seq++
	q := b.queueFor(p.To)
	q.pending = append(q.pending, &message{seq: b.seq, subject: subject, env: decoded})
	q.signal()
	return nil
}

type subscription struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *subscription) Stop() error {
	s.cancel()
	<-s.done
	return nil
}

func (c *conn) Subscribe(ctx context.Context, h bus.Handler) (bus.Subscription, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &subscription{cancel: cancel, done: make(chan struct{})}
	go c.loop(ctx, h, s.done)
	return s, nil
}

// loop delivers one message at a time, in sequence order, honoring nak
// delays and ack waits.
func (c *conn) loop(ctx context.Context, h bus.Handler, done chan struct{}) {
	defer close(done)
	b := c.broker
	b.mu.Lock()
	q := b.queueFor(c.user)
	b.mu.Unlock()
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return
		}
		now := time.Now()
		var next *message
		wait := time.Duration(-1)
		busy := false
		sort.Slice(q.pending, func(i, j int) bool { return q.pending[i].seq < q.pending[j].seq })
		for _, m := range q.pending {
			if m.inflight {
				if now.Before(m.ackDue) {
					busy = true
					if d := m.ackDue.Sub(now); wait < 0 || d < wait {
						wait = d
					}
					continue
				}
				// Ack wait elapsed: treat as a nak with no delay.
				m.inflight = false
				c.mu.Lock()
				for id, im := range c.inflight {
					if im == m {
						delete(c.inflight, id)
					}
				}
				c.mu.Unlock()
			}
			if now.Before(m.notBefore) {
				if d := m.notBefore.Sub(now); wait < 0 || d < wait {
					wait = d
				}
				continue
			}
			if !busy {
				next = m
				break
			}
		}
		if next == nil {
			b.mu.Unlock()
			var timer <-chan time.Time
			var stop func() bool
			if wait >= 0 {
				t := time.NewTimer(wait)
				timer, stop = t.C, t.Stop
			}
			select {
			case <-ctx.Done():
				if stop != nil {
					stop()
				}
				return
			case <-q.wake:
			case <-timer:
			}
			if stop != nil {
				stop()
			}
			continue
		}
		next.deliveries++
		if next.deliveries > b.maxDeliver {
			q.pending = remove(q.pending, next)
			b.dlq[c.user] = append(b.dlq[c.user], next.env)
			b.mu.Unlock()
			continue
		}
		next.inflight = true
		next.ackDue = now.Add(b.ackWait)
		id := bus.MessageID(fmt.Sprintf("%s/%d/%d", c.user, next.seq, next.deliveries))
		c.mu.Lock()
		c.inflight[id] = next
		c.mu.Unlock()
		parsed, _ := bus.ParseSubject(next.subject)
		d := bus.Delivery{ID: id, Subject: next.subject, Sender: parsed.From, Env: next.env, Attempt: next.deliveries}
		b.mu.Unlock()
		h(ctx, d)
	}
}

func remove(ms []*message, m *message) []*message {
	out := ms[:0]
	for _, x := range ms {
		if x != m {
			out = append(out, x)
		}
	}
	return out
}

func (c *conn) take(id bus.MessageID) (*message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.inflight[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", bus.ErrUnknownMessage, id)
	}
	delete(c.inflight, id)
	return m, nil
}

func (c *conn) Ack(ctx context.Context, id bus.MessageID) error {
	m, err := c.take(id)
	if err != nil {
		return err
	}
	b := c.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.queueFor(c.user)
	q.pending = remove(q.pending, m)
	q.signal()
	return nil
}

func (c *conn) Nak(ctx context.Context, id bus.MessageID, delay time.Duration) error {
	m, err := c.take(id)
	if err != nil {
		return err
	}
	b := c.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	m.inflight = false
	m.notBefore = time.Now().Add(delay)
	b.queueFor(c.user).signal()
	return nil
}

func (c *conn) Registry() bus.Registry { return registry{c.broker} }

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

type registry struct{ b *Broker }

func (r registry) Put(ctx context.Context, p bus.Presence) error {
	if !envelope.ValidHandle(p.Handle) {
		return fmt.Errorf("%w: %q", envelope.ErrHandle, p.Handle)
	}
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	r.b.registry[p.Handle] = p
	return nil
}

func (r registry) List(ctx context.Context) ([]bus.Presence, error) {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	out := make([]bus.Presence, 0, len(r.b.registry))
	for _, p := range r.b.registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}
