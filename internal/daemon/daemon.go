// Package daemon is wormholed: it consumes the user's inbox, materializes
// Copilot sessions for inbound asks and replies, publishes delivery
// receipts and keeps the local thread map current.
//
// Everything that decides whether inbound text may drive an agent lives
// here: the sender allowlist, hop and expiry caps, the per-sender and daily
// rate caps, and the inbox-vs-auto choice.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// MaxDeliver is how many delivery attempts a message gets before the daemon
// parks it on the dead-letter subject. It matches the consumer setting in
// the NATS deployment template.
const MaxDeliver = 5

// Daemon is one running wormholed.
type Daemon struct {
	cfg   *config.Config
	bus   bus.Bus
	store *threads.Store
	app   ghapp.App
	log   *slog.Logger
	host  string
	now   func() time.Time
	// paused reports whether the local kill switch is on.
	paused     func() bool
	pauseCheck time.Duration
	onHandled  func()
}

// Options are the daemon's collaborators. Zero values get sane defaults.
type Options struct {
	Config *config.Config
	Bus    bus.Bus
	Store  *threads.Store
	App    ghapp.App
	Log    *slog.Logger
	Host   string
	Now    func() time.Time
	Paused func() bool
	// PauseCheck is how often the kill switch is polled (default 2s).
	PauseCheck time.Duration
	// OnHandled, when set, is called after every delivery is handled.
	OnHandled func()
}

// New wires a daemon.
func New(o Options) *Daemon {
	d := &Daemon{cfg: o.Config, bus: o.Bus, store: o.Store, app: o.App, log: o.Log, host: o.Host, now: o.Now, paused: o.Paused, pauseCheck: o.PauseCheck, onHandled: o.OnHandled}
	if d.pauseCheck == 0 {
		d.pauseCheck = 2 * time.Second
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.host == "" {
		d.host, _ = os.Hostname()
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.paused == nil {
		d.paused = func() bool { _, err := os.Stat(config.PauseFile()); return err == nil }
	}
	return d
}

// Me is this daemon's identity as a peer.
func (d *Daemon) Me() envelope.Peer {
	return envelope.Peer{User: d.cfg.Handle, Host: d.host}
}

// Run subscribes and blocks until ctx is done. It also heartbeats the
// registry, retries unopened sessions and expires stale threads. While the
// local kill switch is on, the subscription is stopped so messages wait on
// the bus without spending delivery attempts.
func (d *Daemon) Run(ctx context.Context) error {
	if d.bus.User() != d.cfg.Handle {
		return fmt.Errorf("bus authenticated as %q but config handle is %q", d.bus.User(), d.cfg.Handle)
	}
	d.heartbeat(ctx)
	d.retryUnopened(ctx)
	if n, err := d.store.ExpireStale(ctx, d.now()); err == nil && n > 0 {
		d.log.Info("expired stale threads", "count", n)
	}
	var sub bus.Subscription
	ensure := func() error {
		paused := d.paused()
		if paused && sub != nil {
			d.log.Info("paused; stopped consuming")
			_ = sub.Stop()
			sub = nil
		}
		if !paused && sub == nil {
			s, err := d.bus.Subscribe(ctx, d.Handle)
			if err != nil {
				return fmt.Errorf("subscribe: %w", err)
			}
			sub = s
			d.log.Info("consuming", "handle", d.cfg.Handle)
		}
		return nil
	}
	if err := ensure(); err != nil {
		return err
	}
	defer func() {
		if sub != nil {
			_ = sub.Stop()
		}
	}()
	d.log.Info("wormholed running", "handle", d.cfg.Handle, "host", d.host, "senders", len(d.cfg.Senders), "paused", d.paused())
	ticker := time.NewTicker(d.cfg.Daemon.RegistryInterval)
	defer ticker.Stop()
	pauseTicker := time.NewTicker(d.pauseCheck)
	defer pauseTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.heartbeat(ctx)
			d.retryUnopened(ctx)
			_, _ = d.store.ExpireStale(ctx, d.now())
		case <-pauseTicker.C:
			if err := ensure(); err != nil {
				d.log.Warn("resubscribe failed", "err", err)
			}
		}
	}
}

func (d *Daemon) heartbeat(ctx context.Context) {
	mode := "inbox"
	for _, s := range d.cfg.Senders {
		if s.Mode == config.ModeAuto {
			mode = "mixed"
			break
		}
	}
	if d.paused() {
		mode = "paused"
	}
	p := bus.Presence{Handle: d.cfg.Handle, Host: d.host, OS: runtime.GOOS, Mode: mode, LastSeen: d.now().UTC()}
	if err := d.bus.Registry().Put(ctx, p); err != nil {
		d.log.Warn("registry heartbeat failed", "err", err)
	}
}

// retryUnopened re-attempts the deep link for sessions that exist but were
// never shown in the app.
func (d *Daemon) retryUnopened(ctx context.Context) {
	if !*d.cfg.Daemon.OpenApp {
		return
	}
	list, err := d.store.List(ctx, threads.Filter{Direction: threads.DirectionIn, Unopened: true})
	if err != nil {
		return
	}
	for _, t := range list {
		if err := d.app.Open(ctx, t.LocalSessionID); err == nil {
			_, _ = d.store.Update(ctx, t.ID, func(t *threads.Thread) { t.Opened = true })
		}
	}
}

// Handle processes one delivery. It always ends with exactly one Ack or
// Nak on the delivery.
func (d *Daemon) Handle(ctx context.Context, dl bus.Delivery) {
	if d.onHandled != nil {
		defer d.onHandled()
	}
	e := dl.Env
	log := d.log.With("msg", e.ID, "type", string(e.Type), "from", dl.Sender, "attempt", dl.Attempt)

	// Identity: the subject's sender token is what the server authenticated.
	if e.From.User != dl.Sender {
		log.Warn("dropping message whose from.user does not match the authenticated sender", "claimed", e.From.User)
		d.reject(ctx, dl, "sender mismatch")
		return
	}
	if e.To.User != d.cfg.Handle {
		log.Warn("dropping message addressed to someone else", "to", e.To.User)
		d.ack(ctx, dl)
		return
	}
	if err := e.Validate(d.now()); err != nil {
		log.Warn("invalid envelope", "err", err)
		d.reject(ctx, dl, err.Error())
		return
	}
	// At-least-once: a message we already stored was fully handled by an
	// earlier attempt whose ack did not reach the bus.
	if seen, err := d.store.HasMessage(ctx, e.ID); err == nil && seen {
		log.Info("duplicate delivery, acking")
		d.ack(ctx, dl)
		return
	}
	if e.Hop > d.cfg.Limits.MaxHops {
		d.reject(ctx, dl, fmt.Sprintf("hop %d exceeds max_hops %d", e.Hop, d.cfg.Limits.MaxHops))
		return
	}

	switch e.Type {
	case envelope.TypeAck:
		d.handleAck(ctx, dl)
	case envelope.TypeAsk:
		d.handleAsk(ctx, dl)
	case envelope.TypeReply, envelope.TypeMessage:
		d.handleFollowUp(ctx, dl)
	default:
		d.reject(ctx, dl, "unknown type")
	}
}

func (d *Daemon) ack(ctx context.Context, dl bus.Delivery) {
	if err := d.bus.Ack(ctx, dl.ID); err != nil {
		d.log.Warn("ack failed", "msg", dl.Env.ID, "err", err)
	}
}

// reject acks the bus message so it does not redeliver and tells the sender
// why. A rejection is final: it is never retried.
func (d *Daemon) reject(ctx context.Context, dl bus.Delivery, reason string) {
	d.log.Info("rejected", "msg", dl.Env.ID, "reason", reason)
	if dl.Env.Type != envelope.TypeAck && envelope.ValidHandle(dl.Sender) {
		ack := envelope.NewAck(dl.Env, d.Me(), "rejected: "+reason, d.now())
		ack.To.User = dl.Sender
		if err := d.bus.Publish(ctx, bus.SubjectFor(ack), ack); err != nil {
			d.log.Warn("could not publish rejection", "err", err)
		}
	}
	d.ack(ctx, dl)
}

// retryOrDeadLetter is the failure path for steps before the ack: nak with
// backoff, and after MaxDeliver attempts park the message and reject it.
func (d *Daemon) retryOrDeadLetter(ctx context.Context, dl bus.Delivery, cause error) {
	if dl.Attempt >= MaxDeliver {
		d.log.Error("giving up on message", "msg", dl.Env.ID, "err", cause)
		if err := d.bus.Publish(ctx, bus.DLQSubject(d.cfg.Handle), dl.Env); err != nil {
			d.log.Warn("could not publish to dlq", "err", err)
		}
		d.reject(ctx, dl, fmt.Sprintf("delivery failed after %d attempts", dl.Attempt))
		return
	}
	d.log.Warn("materialization failed, will retry", "msg", dl.Env.ID, "err", cause, "backoff", d.cfg.Daemon.NakBackoff)
	if err := d.bus.Nak(ctx, dl.ID, d.cfg.Daemon.NakBackoff); err != nil {
		d.log.Warn("nak failed", "err", err)
	}
}

func (d *Daemon) handleAck(ctx context.Context, dl bus.Delivery) {
	e := dl.Env
	t, err := d.store.Get(ctx, e.ThreadID)
	if err != nil {
		d.log.Info("ack for unknown thread", "thread", e.ThreadID)
		d.ack(ctx, dl)
		return
	}
	if t.Peer != dl.Sender {
		d.log.Warn("ack from someone other than the thread peer", "peer", t.Peer)
		d.ack(ctx, dl)
		return
	}
	reason, rejected := e.Rejected()
	_, err = d.store.Update(ctx, t.ID, func(t *threads.Thread) {
		switch {
		case rejected:
			t.State = threads.StateRejected
			t.LastPreview = "rejected: " + reason
		case t.State == threads.StateSent:
			t.State = threads.StateDelivered
		}
	})
	if err == nil {
		err = d.store.AddMessage(ctx, e, threads.DirectionIn)
	}
	if err != nil {
		d.retryOrDeadLetter(ctx, dl, err)
		return
	}
	d.ack(ctx, dl)
}

// rateLimited reports whether accepting another ask from sender would
// exceed the recipient's caps over the trailing 24 hours.
func (d *Daemon) rateLimited(ctx context.Context, sender string) (bool, string) {
	since := d.now().Add(-24 * time.Hour)
	if n, err := d.store.CountInbound(ctx, sender, since); err == nil && d.cfg.Limits.PerSenderPerDay > 0 && n >= d.cfg.Limits.PerSenderPerDay {
		return true, fmt.Sprintf("%d asks from ^%s in 24h", n, sender)
	}
	if n, err := d.store.CountInbound(ctx, "", since); err == nil && d.cfg.Limits.PerDay > 0 && n >= d.cfg.Limits.PerDay {
		return true, fmt.Sprintf("%d asks total in 24h", n)
	}
	return false, ""
}

func (d *Daemon) handleAsk(ctx context.Context, dl bus.Delivery) {
	e := dl.Env
	mode, allowed := d.cfg.SenderMode(dl.Sender)
	if !allowed {
		d.reject(ctx, dl, "sender not allowed")
		return
	}
	if d.paused() {
		// Run stops the subscription while paused; this only catches a
		// message already in flight when the switch flipped.
		d.log.Info("paused; leaving message on the bus", "msg", e.ID)
		_ = d.bus.Nak(ctx, dl.ID, d.cfg.Daemon.NakBackoff)
		return
	}
	// Over the cost caps, the ask is still accepted but a person decides
	// whether to spend the credits: it is forced into inbox mode.
	if limited, why := d.rateLimited(ctx, dl.Sender); limited && mode == config.ModeAuto {
		d.log.Info("rate cap reached; downgrading to inbox mode", "why", why)
		mode = config.ModeInbox
	}
	if mode == config.ModeAuto && len(d.cfg.Copilot.SafeTools) == 0 {
		mode = config.ModeInbox
	}
	kickoff := AskKickoff(d.cfg.Handle, e)
	t := threads.Thread{
		ID:            e.ThreadID,
		Peer:          dl.Sender,
		Direction:     threads.DirectionIn,
		Mode:          threads.Mode(mode),
		State:         threads.StatePending,
		RootSessionID: e.RootSessionID,
		Title:         threads.Preview(e.Body, 60),
		Repo:          e.Context.Repo,
		Workdir:       d.cfg.WorkdirFor(e.Context.Repo),
		ExpiresAt:     e.ExpiresAt,
		CreatedAt:     e.CreatedAt,
	}
	known := false
	if existing, err := d.store.Get(ctx, e.ThreadID); err == nil {
		// Thread id reuse: only the original peer may add to it.
		if existing.Peer != dl.Sender || existing.Direction != threads.DirectionIn {
			d.reject(ctx, dl, "thread id already in use")
			return
		}
		known = true
		t.LocalSessionID = existing.LocalSessionID
	}
	if mode == config.ModeAuto {
		t.State = threads.StateDelivered
		t.LocalSessionID = ghapp.NewSessionID()
	} else {
		// Inbox mode stages the kickoff locally; `wormhole open` starts the
		// session once a person has read the message.
		t.StagedKickoff = kickoff
	}
	// The thread row and the message copy exist before the kickoff runs:
	// the agent inside the session answers with `wormhole reply`, which
	// needs both on this machine. M0 saw that reply fail with "thread is
	// not on this machine" when they were written after the run.
	if err := d.record(ctx, e, t); err != nil {
		d.retryOrDeadLetter(ctx, dl, err)
		return
	}
	if mode == config.ModeAuto {
		err := d.app.Create(ctx, ghapp.Session{
			ID:             t.LocalSessionID,
			Name:           ghapp.SessionName(dl.Sender, e.Body),
			Kickoff:        kickoff,
			Workdir:        t.Workdir,
			Model:          d.cfg.Copilot.Model,
			AvailableTools: d.cfg.Copilot.SafeTools,
			ExtraArgs:      d.cfg.Copilot.ExtraArgs,
		})
		if err != nil {
			if !d.answeredSince(ctx, e) {
				// Undo the record so the retry is not taken for a duplicate.
				d.unrecord(ctx, e, !known)
				d.retryOrDeadLetter(ctx, dl, err)
				return
			}
			d.log.Warn("kickoff exited with an error after the agent had replied; keeping the thread", "msg", e.ID, "err", err)
		}
	}
	d.ack(ctx, dl)
	receipt := envelope.NewAck(e, d.Me(), "delivered", d.now())
	if err := d.bus.Publish(ctx, bus.SubjectFor(receipt), receipt); err != nil {
		d.log.Warn("could not publish receipt", "err", err)
	}
	d.log.Info("materialized ask", "thread", e.ThreadID, "mode", mode, "session", t.LocalSessionID)
	d.surface(ctx, t, fmt.Sprintf("^%s asked you something", dl.Sender), t.Title)
}

func (d *Daemon) handleFollowUp(ctx context.Context, dl bus.Delivery) {
	e := dl.Env
	t, err := d.store.Get(ctx, e.ThreadID)
	known := err == nil
	if known && t.Peer != dl.Sender {
		d.reject(ctx, dl, "not a party to this thread")
		return
	}
	mode, allowed := d.cfg.SenderMode(dl.Sender)
	if !known && !allowed {
		d.reject(ctx, dl, "sender not allowed")
		return
	}
	if !allowed {
		// The peer answered a thread we opened but is not in our allowlist:
		// accept the answer, but never run it unattended.
		mode = config.ModeInbox
	}
	if d.paused() {
		_ = d.bus.Nak(ctx, dl.ID, d.cfg.Daemon.NakBackoff)
		return
	}
	if mode == config.ModeAuto && len(d.cfg.Copilot.SafeTools) == 0 {
		mode = config.ModeInbox
	}
	if !known {
		// Reinstall or lost state: rebuild the row from the envelope.
		t = threads.Thread{
			ID: e.ThreadID, Peer: dl.Sender, Direction: threads.DirectionOut, State: threads.StateDelivered,
			RootSessionID: e.RootSessionID, Title: threads.Preview(e.Body, 60), Repo: e.Context.Repo,
			Workdir: d.cfg.WorkdirFor(e.Context.Repo), ExpiresAt: e.ExpiresAt, CreatedAt: e.CreatedAt,
		}
	}
	if t.Workdir == "" {
		t.Workdir = d.cfg.WorkdirFor(t.Repo)
	}
	kickoff := ReplyKickoff(d.cfg.Handle, e)
	// The message continues the session already tied to the thread, or,
	// for a thread this machine opened, the session that asked. Either
	// may be empty: the asking shell did not expose its session id, or
	// reply_in_place is off.
	target := ContinueSession(d.cfg, t)
	t.Mode = threads.Mode(mode)
	if e.Type == envelope.TypeReply {
		t.State = threads.StateReplied
	}
	if mode == config.ModeInbox {
		// Inbox mode stages the text; `wormhole open` appends it to the
		// target session, or starts a new one, once a person has read it.
		// The thread keeps its session id so open knows where to go, and
		// Opened is left alone so nothing is shown before then.
		t.StagedKickoff = kickoff
	}
	// Recorded before the run for the same reason as in handleAsk: a
	// `wormhole send` from inside the session needs the thread and this
	// message on disk.
	if err := d.record(ctx, e, t); err != nil {
		d.retryOrDeadLetter(ctx, dl, err)
		return
	}
	resumed := false
	if mode == config.ModeAuto {
		if target != "" {
			err := d.app.Resume(ctx, ghapp.Session{
				ID:             target,
				Kickoff:        kickoff,
				Workdir:        t.Workdir,
				Model:          d.cfg.Copilot.Model,
				AvailableTools: d.cfg.Copilot.SafeTools,
				ExtraArgs:      d.cfg.Copilot.ExtraArgs,
			})
			if err != nil {
				d.log.Warn("could not resume session; opening a new one", "session", target, "err", err)
			} else {
				resumed = true
				t.LocalSessionID = target
			}
		}
		if !resumed {
			sessionID := ghapp.NewSessionID()
			err := d.app.Create(ctx, ghapp.Session{
				ID:             sessionID,
				Name:           fmt.Sprintf("Reply from ^%s", dl.Sender),
				Kickoff:        kickoff,
				Workdir:        t.Workdir,
				Model:          d.cfg.Copilot.Model,
				AvailableTools: d.cfg.Copilot.SafeTools,
				ExtraArgs:      d.cfg.Copilot.ExtraArgs,
			})
			if err != nil {
				if !d.answeredSince(ctx, e) {
					d.unrecord(ctx, e, !known)
					d.retryOrDeadLetter(ctx, dl, err)
					return
				}
				d.log.Warn("kickoff exited with an error after the agent had answered; keeping the thread", "msg", e.ID, "err", err)
			}
			t.LocalSessionID = sessionID
		}
		// Only the session fields are touched here so that a reply the
		// agent sent during the run keeps the state it set.
		sessionID := t.LocalSessionID
		if u, err := d.store.Update(ctx, t.ID, func(u *threads.Thread) {
			u.LocalSessionID = sessionID
			u.Opened = false
			u.StagedKickoff = ""
		}); err == nil {
			t = u
		}
	}
	d.ack(ctx, dl)
	receipt := envelope.NewAck(e, d.Me(), "delivered", d.now())
	if err := d.bus.Publish(ctx, bus.SubjectFor(receipt), receipt); err != nil {
		d.log.Warn("could not publish receipt", "err", err)
	}
	d.log.Info("materialized follow-up", "thread", e.ThreadID, "mode", mode, "session", t.LocalSessionID, "resumed", resumed)
	d.surface(ctx, t, fmt.Sprintf("^%s replied", dl.Sender), threads.Preview(e.Body, 60))
}

// record writes the thread row and the inbound message copy.
func (d *Daemon) record(ctx context.Context, e envelope.Envelope, t threads.Thread) error {
	if err := d.store.Put(ctx, t); err != nil {
		return err
	}
	return d.store.AddMessage(ctx, e, threads.DirectionIn)
}

// unrecord removes what record wrote so that a redelivery is handled
// afresh instead of being acked as a duplicate. The thread row goes too
// when this delivery created it.
func (d *Daemon) unrecord(ctx context.Context, e envelope.Envelope, dropThread bool) {
	if err := d.store.DeleteMessage(ctx, e.ID); err != nil {
		d.log.Warn("could not roll back message copy", "msg", e.ID, "err", err)
	}
	if dropThread {
		if err := d.store.DeleteThread(ctx, e.ThreadID); err != nil {
			d.log.Warn("could not roll back thread row", "thread", e.ThreadID, "err", err)
		}
	}
}

// answeredSince reports whether something outbound was sent on e's thread
// after e arrived: the agent answered even though the kickoff process
// ended badly (a timeout, a non-zero exit after the reply).
func (d *Daemon) answeredSince(ctx context.Context, e envelope.Envelope) bool {
	last, err := d.store.LastMessage(ctx, e.ThreadID)
	return err == nil && last.ID != e.ID && last.Direction == threads.DirectionOut
}

// surface shows the result to the person: the deep link when a session
// exists, and a notification either way. Both are best effort and never
// affect the bus.
func (d *Daemon) surface(ctx context.Context, t threads.Thread, title, body string) {
	if t.LocalSessionID != "" && t.StagedKickoff == "" && *d.cfg.Daemon.OpenApp {
		if err := d.app.Open(ctx, t.LocalSessionID); err != nil {
			d.log.Warn("could not open session in app", "session", t.LocalSessionID, "err", err)
		} else {
			_, _ = d.store.Update(ctx, t.ID, func(t *threads.Thread) { t.Opened = true })
		}
	}
	if *d.cfg.Daemon.Notify {
		hint := "wormhole inbox"
		if t.LocalSessionID != "" {
			hint = "thread " + t.ID
		}
		if err := d.app.Notify(ctx, title, strings.TrimSpace(body)+"\n"+hint); err != nil {
			d.log.Debug("notification failed", "err", err)
		}
	}
}

// ContinueSession returns the session a follow-up on t should be appended
// to: the session already tied to the thread (one this daemon or `wormhole
// open` started for it), else, when reply_in_place is on, the session that
// asked on this machine. Empty when there is none: the asking shell did not
// expose its session id, or reply_in_place is off and no thread session
// exists yet.
func ContinueSession(cfg *config.Config, t threads.Thread) string {
	if t.LocalSessionID != "" {
		return t.LocalSessionID
	}
	if cfg.Daemon.ReplyInPlace != nil && !*cfg.Daemon.ReplyInPlace {
		return ""
	}
	if t.Direction == threads.DirectionOut {
		return t.RootSessionID
	}
	return ""
}

// ErrPaused is returned by Pause/Resume helpers when the state file cannot
// be managed.
var ErrPaused = errors.New("daemon paused")

// Pause writes the local kill switch.
func Pause() error {
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(config.PauseFile(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// Resume clears the local kill switch.
func Resume() error {
	err := os.Remove(config.PauseFile())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IsPaused reports whether the kill switch is on.
func IsPaused() bool {
	_, err := os.Stat(config.PauseFile())
	return err == nil
}
