package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// Notification action keys. The daemon acts on the key the person pressed.
const (
	ActionShow    = "show"
	ActionLater   = "later"
	ActionDecline = "decline"
)

// announce tells the person about an event on t, honoring daemon.notify
// and quiet_hours. title and body are the per-message text; what reaches
// the screen depends on the mode:
//
//   - toast: one notification per event, as before.
//   - actions: a staged message gets one low-urgency bubble for the inbox
//     (replacing the previous one) with Show, Later and Decline buttons;
//     anything else is a toast.
//   - digest: nothing now; the next summary counts it.
//   - quiet hours (any mode): nothing now; one summary when the window
//     ends.
func (d *Daemon) announce(ctx context.Context, t threads.Thread, title, body string) {
	mode := d.cfg.Daemon.Notify
	if !mode.Enabled() {
		return
	}
	staged := t.StagedKickoff != ""
	d.mu.Lock()
	if d.cfg.Daemon.InQuietHours(d.now()) {
		d.held++
		if !staged {
			d.opened++
		}
		d.mu.Unlock()
		d.log.Debug("quiet hours; holding notification", "thread", t.ID)
		return
	}
	if mode == config.NotifyDigest {
		d.pending++
		if !staged {
			d.opened++
		}
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	if mode == config.NotifyActions && staged {
		d.bubble(ctx, t, title, body)
		return
	}
	d.toast(ctx, t, title, body)
}

// toast is the plain per-message notification.
func (d *Daemon) toast(ctx context.Context, t threads.Thread, title, body string) {
	hint := "wormhole inbox"
	if t.LocalSessionID != "" && t.StagedKickoff == "" {
		hint = "thread " + t.ID
	}
	if _, err := d.app.Notify(ctx, ghapp.Notification{Title: title, Body: strings.TrimSpace(body) + "\n" + hint}); err != nil {
		d.log.Debug("notification failed", "err", err)
	}
}

// bubble shows the inbox bubble for a newly staged message and, in the
// background, acts on the button the person presses. A bubble still
// waiting when the next message arrives is replaced: its notify-send is
// cancelled (the bubble on screen stays until the new one takes its id)
// so that only the newest bubble's buttons are live.
func (d *Daemon) bubble(ctx context.Context, t threads.Thread, title, body string) {
	waiting, _ := d.waiting(ctx)
	meta := fmt.Sprintf("%d waiting in your inbox", len(waiting))
	if t.Repo != "" {
		meta = t.Repo + " · " + meta
	}
	n := ghapp.Notification{
		Title: title,
		Body:  strings.TrimSpace(body) + "\n" + meta,
		Low:   true,
		Actions: []ghapp.Action{
			{Key: ActionShow, Label: "Show"},
			{Key: ActionLater, Label: "Later · " + shortDuration(d.cfg.Daemon.SnoozeDefault)},
			{Key: ActionDecline, Label: "Decline"},
		},
	}
	d.mu.Lock()
	if d.bubbleCancel != nil {
		d.bubbleCancel()
	}
	bctx, cancel := context.WithCancel(ctx)
	d.bubbleCancel = cancel
	n.ReplaceID = d.bubbleID
	d.mu.Unlock()
	go func() {
		res, err := d.app.Notify(bctx, n)
		if err != nil {
			if bctx.Err() == nil {
				d.log.Debug("notification failed", "err", err)
			}
			return
		}
		d.mu.Lock()
		if res.ID != 0 {
			d.bubbleID = res.ID
		}
		d.mu.Unlock()
		if res.Plain {
			d.log.Info("notify-send has no action buttons here; showing plain toasts (daemon.notify: toast silences this)")
			return
		}
		d.act(ctx, t.ID, res.Action)
	}()
}

// act carries out a notification button for the thread the bubble named.
// Later and Decline run nothing, so a button press is enough; Show opens
// the inbox view where approval happens.
func (d *Daemon) act(ctx context.Context, threadID, action string) {
	switch action {
	case "":
		return
	case ActionShow:
		if err := d.app.ShowInbox(ctx, d.showCommand()); err != nil {
			d.log.Warn("could not show the inbox", "err", err)
		}
	case ActionLater:
		until := d.now().Add(d.cfg.Daemon.SnoozeDefault)
		t, err := d.store.Get(ctx, threadID)
		if err != nil || t.StagedKickoff == "" {
			return
		}
		if _, err := d.store.Update(ctx, t.ID, func(u *threads.Thread) { u.SnoozedUntil = until }); err != nil {
			d.log.Warn("could not snooze", "thread", threadID, "err", err)
			return
		}
		d.log.Info("snoozed from notification", "thread", threadID, "until", until.Format(time.RFC3339))
	case ActionDecline:
		t, err := d.store.Get(ctx, threadID)
		if err != nil || t.StagedKickoff == "" {
			return
		}
		if _, err := Decline(ctx, d.bus, d.store, d.Me(), t, "", d.now()); err != nil {
			d.log.Warn("could not decline", "thread", threadID, "err", err)
			return
		}
		d.log.Info("declined from notification", "thread", threadID, "peer", t.Peer)
	default:
		d.log.Debug("unknown notification action", "action", action)
	}
}

// showCommand is what the Show button runs: the configured command, else
// a terminal with `wormhole inbox --watch`.
func (d *Daemon) showCommand() []string {
	if len(d.cfg.Daemon.ShowCommand) > 0 {
		return d.cfg.Daemon.ShowCommand
	}
	term := os.Getenv("TERMINAL")
	if term == "" {
		term = "x-terminal-emulator"
	}
	return []string{term, "-e", d.exe, "inbox", "--watch"}
}

// waiting lists the staged messages that are not snoozed right now.
func (d *Daemon) waiting(ctx context.Context) ([]threads.Thread, error) {
	list, err := d.store.List(ctx, threads.Filter{Staged: true})
	if err != nil {
		return nil, err
	}
	now := d.now()
	out := list[:0]
	for _, t := range list {
		if !t.Snoozed(now) {
			out = append(out, t)
		}
	}
	return out, nil
}

// housekeeping runs on every tick: it wakes snoozed messages whose time
// has come and sends the summary that digest mode or the end of quiet
// hours owes.
func (d *Daemon) housekeeping(ctx context.Context) {
	now := d.now()
	if due, err := d.store.List(ctx, threads.Filter{SnoozeDue: &now}); err == nil {
		for _, t := range due {
			u, err := d.store.Update(ctx, t.ID, func(u *threads.Thread) { u.SnoozedUntil = time.Time{} })
			if err != nil {
				continue
			}
			d.log.Info("snooze over", "thread", u.ID)
			d.announce(ctx, u, fmt.Sprintf("^%s is still waiting", u.Peer), u.Title)
		}
	}
	d.mu.Lock()
	quiet := d.cfg.Daemon.InQuietHours(now)
	want := !quiet && d.held > 0
	if !quiet && d.cfg.Daemon.Notify == config.NotifyDigest && d.pending > 0 && now.Sub(d.lastDigest) >= d.cfg.Daemon.DigestEvery {
		want = true
	}
	if !want {
		d.mu.Unlock()
		return
	}
	opened := d.opened
	d.held, d.pending, d.opened = 0, 0, 0
	d.lastDigest = now
	replace := d.bubbleID
	d.mu.Unlock()
	d.summary(ctx, opened, replace)
}

// summary is one low-urgency notification for everything that is waiting.
func (d *Daemon) summary(ctx context.Context, opened int, replace uint32) {
	waiting, err := d.waiting(ctx)
	if err != nil {
		return
	}
	if len(waiting) == 0 && opened == 0 {
		return
	}
	var lines []string
	if len(waiting) > 0 {
		peers := map[string]bool{}
		var names []string
		for _, t := range waiting {
			if !peers[t.Peer] {
				peers[t.Peer] = true
				names = append(names, "^"+t.Peer)
			}
		}
		lines = append(lines, fmt.Sprintf("%d waiting in your inbox (%s)", len(waiting), strings.Join(names, ", ")))
	}
	if opened > 0 {
		noun := "sessions"
		if opened == 1 {
			noun = "session"
		}
		lines = append(lines, fmt.Sprintf("%d %s opened in the app", opened, noun))
	}
	lines = append(lines, "wormhole inbox")
	res, err := d.app.Notify(ctx, ghapp.Notification{Title: "Wormhole inbox", Body: strings.Join(lines, "\n"), Low: true, ReplaceID: replace})
	if err != nil {
		d.log.Debug("summary notification failed", "err", err)
		return
	}
	if res.ID != 0 {
		d.mu.Lock()
		d.bubbleID = res.ID
		d.mu.Unlock()
	}
}

// shortDuration formats 2h0m0s as 2h and 90m0s as 90m.
func shortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return d.String()
	}
}
