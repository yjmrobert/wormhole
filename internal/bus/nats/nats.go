// Package nats is the JetStream adapter: the first real bus behind
// internal/bus. One stream (WORMHOLE, subjects wh.>) holds every message;
// each user has a durable pull consumer filtered to their own inbox and ack
// subjects; the WORMHOLE_USERS key-value bucket is the handle registry.
//
// Per-user ACLs are a deployment requirement, not something this adapter
// emulates: deploy/nats/accounts.conf.example grants each user publish
// rights only on subjects ending in their own handle. The adapter checks
// bus.MayPublish locally as well so a misconfiguration fails fast.
package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// Deployment constants. They must match deploy/nats/*.example.
const (
	StreamName   = "WORMHOLE"
	BucketName   = "WORMHOLE_USERS"
	StreamMaxAge = 7 * 24 * time.Hour
	// MaxMsgSize leaves room for the envelope around a 32 KB body.
	MaxMsgSize = 64 * 1024
	MaxDeliver = 5
	// AckWait covers a headless kickoff run: the daemon acks only once the
	// session exists, and M0 measured first runs over two minutes.
	AckWait     = 10 * time.Minute
	PresenceTTL = time.Hour
)

// ConsumerName is the durable consumer for a user.
func ConsumerName(user string) string { return "wh-" + user }

// Options configure a connection.
type Options struct {
	URL  string
	User string
	// Exactly one of Password, PasswordFile or Creds is used.
	Password     string
	PasswordFile string
	Creds        string
	TLSCA        string
	Log          *slog.Logger
	// AckWait overrides the consumer ack wait (tests).
	AckWait time.Duration
}

// Bus is one authenticated JetStream connection.
type Bus struct {
	nc   *natsgo.Conn
	js   jetstream.JetStream
	user string
	log  *slog.Logger

	mu       sync.Mutex
	inflight map[bus.MessageID]jetstream.Msg
	ackWait  time.Duration
}

// Connect dials the server and verifies the connection.
func Connect(ctx context.Context, o Options) (*Bus, error) {
	if !envelope.ValidHandle(o.User) {
		return nil, fmt.Errorf("%w: %q", envelope.ErrHandle, o.User)
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	opts := []natsgo.Option{
		natsgo.Name("wormhole/" + o.User),
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(2 * time.Second),
		natsgo.Timeout(10 * time.Second),
		natsgo.DisconnectErrHandler(func(_ *natsgo.Conn, err error) {
			if err != nil {
				o.Log.Warn("bus disconnected", "err", err)
			}
		}),
		natsgo.ReconnectHandler(func(nc *natsgo.Conn) { o.Log.Info("bus reconnected", "url", nc.ConnectedUrlRedacted()) }),
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
			// Permission violations surface here asynchronously.
			o.Log.Warn("bus error", "err", err)
		}),
	}
	switch {
	case o.Creds != "":
		opts = append(opts, natsgo.UserCredentials(o.Creds))
	case o.PasswordFile != "":
		raw, err := os.ReadFile(o.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read password file: %w", err)
		}
		opts = append(opts, natsgo.UserInfo(o.User, strings.TrimSpace(string(raw))))
	case o.Password != "":
		opts = append(opts, natsgo.UserInfo(o.User, o.Password))
	default:
		return nil, errors.New("nats: no credentials configured")
	}
	if o.TLSCA != "" {
		opts = append(opts, natsgo.RootCAs(o.TLSCA))
	}
	nc, err := natsgo.Connect(o.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", redact(o.URL), err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	aw := o.AckWait
	if aw == 0 {
		aw = AckWait
	}
	return &Bus{nc: nc, js: js, user: o.User, log: o.Log, inflight: map[bus.MessageID]jetstream.Msg{}, ackWait: aw}, nil
}

func redact(url string) string {
	if i := strings.Index(url, "@"); i >= 0 {
		if j := strings.Index(url, "://"); j >= 0 && j < i {
			return url[:j+3] + "***" + url[i:]
		}
	}
	return url
}

// User returns the authenticated handle.
func (b *Bus) User() string { return b.user }

// Publish sends an envelope with its id as the JetStream message id, so a
// retried publish within the duplicate window is dropped by the server.
func (b *Bus) Publish(ctx context.Context, subject string, env envelope.Envelope) error {
	if !bus.MayPublish(b.user, subject) {
		return fmt.Errorf("%w: %s may not publish on %s", bus.ErrPermission, b.user, subject)
	}
	data, err := envelope.Marshal(env)
	if err != nil {
		return err
	}
	// A dead-lettered copy keeps the envelope id, so it needs its own
	// message id or the duplicate window would swallow it.
	msgID := env.ID
	if p, err := bus.ParseSubject(subject); err == nil && p.Kind == bus.KindDLQ {
		msgID = "dlq-" + env.ID
	}
	if _, err := b.js.Publish(ctx, subject, data, jetstream.WithMsgID(msgID)); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

type subscription struct {
	cc jetstream.ConsumeContext
}

func (s *subscription) Stop() error {
	s.cc.Drain()
	return nil
}

// Subscribe creates (or updates) the user's durable consumer and starts
// pulling one message at a time.
func (b *Bus) Subscribe(ctx context.Context, h bus.Handler) (bus.Subscription, error) {
	cons, err := b.js.CreateOrUpdateConsumer(ctx, StreamName, jetstream.ConsumerConfig{
		Durable:        ConsumerName(b.user),
		FilterSubjects: []string{bus.Prefix + "." + bus.KindInbox + "." + b.user + ".>", bus.Prefix + "." + bus.KindAck + "." + b.user + ".>"},
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		MaxDeliver:     MaxDeliver,
		AckWait:        b.ackWait,
		MaxAckPending:  1,
	})
	if err != nil {
		return nil, fmt.Errorf("consumer %s: %w", ConsumerName(b.user), err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		b.deliver(ctx, m, h)
	}, jetstream.PullMaxMessages(1))
	if err != nil {
		return nil, fmt.Errorf("consume: %w", err)
	}
	return &subscription{cc: cc}, nil
}

func (b *Bus) deliver(ctx context.Context, m jetstream.Msg, h bus.Handler) {
	parsed, err := bus.ParseSubject(m.Subject())
	if err != nil {
		b.log.Warn("terminating message on malformed subject", "subject", m.Subject())
		_ = m.Term()
		return
	}
	env, err := envelope.Unmarshal(m.Data())
	if err != nil {
		// Undecodable payloads will never decode; do not spend retries.
		b.log.Warn("terminating undecodable message", "subject", m.Subject(), "err", err)
		_ = m.Term()
		return
	}
	attempt := 1
	if md, err := m.Metadata(); err == nil {
		attempt = int(md.NumDelivered)
	}
	// The reply subject is unique per delivery, which makes it a good id.
	id := bus.MessageID(m.Reply())
	b.mu.Lock()
	b.inflight[id] = m
	b.mu.Unlock()
	h(ctx, bus.Delivery{ID: id, Subject: m.Subject(), Sender: parsed.From, Env: env, Attempt: attempt})
}

func (b *Bus) take(id bus.MessageID) (jetstream.Msg, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.inflight[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", bus.ErrUnknownMessage, id)
	}
	delete(b.inflight, id)
	return m, nil
}

// Ack acknowledges a delivery.
func (b *Bus) Ack(ctx context.Context, id bus.MessageID) error {
	m, err := b.take(id)
	if err != nil {
		return err
	}
	return m.Ack()
}

// Nak requests redelivery after delay.
func (b *Bus) Nak(ctx context.Context, id bus.MessageID, delay time.Duration) error {
	m, err := b.take(id)
	if err != nil {
		return err
	}
	return m.NakWithDelay(delay)
}

// Registry returns the KV-backed handle directory.
func (b *Bus) Registry() bus.Registry { return &registry{b: b} }

// Close drains the connection.
func (b *Bus) Close() error {
	return b.nc.Drain()
}

// Status is what doctor reports about this user's view of the bus.
type Status struct {
	ConnectedURL string `json:"connected_url"`
	Pending      uint64 `json:"pending"`
	DLQ          uint64 `json:"dlq"`
}

// Status reports pending inbox depth and dead-letter depth.
func (b *Bus) Status(ctx context.Context) (Status, error) {
	st := Status{ConnectedURL: b.nc.ConnectedUrlRedacted()}
	s, err := b.js.Stream(ctx, StreamName)
	if err != nil {
		return st, fmt.Errorf("stream %s: %w", StreamName, err)
	}
	info, err := s.Info(ctx, jetstream.WithSubjectFilter(bus.DLQSubject(b.user)))
	if err != nil {
		return st, err
	}
	for _, n := range info.State.Subjects {
		st.DLQ += n
	}
	if ci, err := s.Consumer(ctx, ConsumerName(b.user)); err == nil {
		if cinfo, err := ci.Info(ctx); err == nil {
			st.Pending = cinfo.NumPending + uint64(cinfo.NumAckPending)
		}
	}
	return st, nil
}

type registry struct{ b *Bus }

func (r *registry) kv(ctx context.Context) (jetstream.KeyValue, error) {
	kv, err := r.b.js.KeyValue(ctx, BucketName)
	if err != nil {
		return nil, fmt.Errorf("registry bucket %s: %w (run: wormhole admin init)", BucketName, err)
	}
	return kv, nil
}

func (r *registry) Put(ctx context.Context, p bus.Presence) error {
	if p.Handle != r.b.user {
		return fmt.Errorf("%w: %s may only publish its own presence", bus.ErrPermission, r.b.user)
	}
	kv, err := r.kv(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, p.Handle, data)
	return err
}

// List reads every presence entry with one watch over the bucket, which
// needs no direct-get permission.
func (r *registry) List(ctx context.Context) ([]bus.Presence, error) {
	kv, err := r.kv(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w, err := kv.WatchAll(ctx, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, err
	}
	defer w.Stop()
	var out []bus.Presence
	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case e := <-w.Updates():
			if e == nil {
				// nil marks the end of the initial values.
				return out, nil
			}
			var p bus.Presence
			if err := json.Unmarshal(e.Value(), &p); err != nil || p.Handle != e.Key() {
				r.b.log.Warn("ignoring malformed registry entry", "key", e.Key())
				continue
			}
			out = append(out, p)
		}
	}
}

// EnsureInfrastructure creates the stream and the registry bucket. It needs
// an admin connection and is what `wormhole admin init` runs.
func EnsureInfrastructure(ctx context.Context, nc *natsgo.Conn) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        StreamName,
		Description: "Wormhole messages: asks, replies, acks and dead letters",
		Subjects:    []string{bus.Prefix + ".>"},
		Storage:     jetstream.FileStorage,
		Retention:   jetstream.LimitsPolicy,
		MaxAge:      StreamMaxAge,
		MaxMsgSize:  MaxMsgSize,
		Duplicates:  2 * time.Minute,
		Discard:     jetstream.DiscardOld,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", StreamName, err)
	}
	_, err = js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      BucketName,
		Description: "Wormhole handle registry: one key per handle, written by its daemon",
		History:     1,
		TTL:         PresenceTTL,
		Storage:     jetstream.FileStorage,
	})
	if err != nil {
		return fmt.Errorf("create bucket %s: %w", BucketName, err)
	}
	return nil
}

// AdminConnect dials with admin credentials for EnsureInfrastructure.
func AdminConnect(url, user, password, tlsCA string) (*natsgo.Conn, error) {
	opts := []natsgo.Option{natsgo.Name("wormhole/admin"), natsgo.UserInfo(user, password), natsgo.Timeout(10 * time.Second)}
	if tlsCA != "" {
		opts = append(opts, natsgo.RootCAs(tlsCA))
	}
	return natsgo.Connect(url, opts...)
}
