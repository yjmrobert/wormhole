package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// stagedThread loads the thread a verb acts on and checks that a message is
// waiting on it.
func (a *App) stagedThread(ctx context.Context, store *threads.Store, id string) (threads.Thread, error) {
	if !envelope.ValidID(id) {
		return threads.Thread{}, fail(ExitNotFound, "%q is not a thread id", id)
	}
	t, err := store.Get(ctx, id)
	if err != nil {
		return threads.Thread{}, fail(ExitNotFound, "thread %s is not on this machine", id)
	}
	if t.StagedKickoff == "" {
		return threads.Thread{}, fail(ExitRefused, "nothing is waiting on thread %s (state %s)", id, t.State)
	}
	return t, nil
}

// parseSnooze reads a snooze length: a Go duration (2h, 45m), days (1d),
// or off to clear it.
func parseSnooze(s string, def time.Duration) (time.Duration, error) {
	switch s {
	case "":
		return def, nil
	case "off", "0":
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fail(ExitUsage, "%q is not a duration (try 2h, 45m, 1d, or off)", s)
	}
	return d, nil
}

type snoozeResult struct {
	OK           bool   `json:"ok"`
	ThreadID     string `json:"thread_id"`
	SnoozedUntil string `json:"snoozed_until,omitempty"`
}

// snooze hides a staged message from the inbox for a while. The daemon
// notifies once more when the time passes. It is local and reversible, so
// there is no confirmation.
func (a *App) snooze(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("snooze", flag.ContinueOnError)
	c.bind(fs)
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return fail(ExitUsage, "usage: wormhole snooze <thread_id> [<duration>|off]")
	}
	cfg, err := a.loadConfig(c)
	if err != nil {
		return err
	}
	length := ""
	if len(pos) == 2 {
		length = pos[1]
	}
	d, err := parseSnooze(length, cfg.Daemon.SnoozeDefault)
	if err != nil {
		return err
	}
	store, err := a.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()
	t, err := a.stagedThread(ctx, store, pos[0])
	if err != nil {
		return err
	}
	res, err := a.snoozeThread(ctx, store, t, d)
	if err != nil {
		return err
	}
	a.emit(c, res, func(w io.Writer) {
		if res.SnoozedUntil == "" {
			fmt.Fprintf(w, "thread %s is back in the inbox\n", t.ID)
			return
		}
		fmt.Fprintf(w, "thread %s snoozed until %s; the daemon will remind you once\n", t.ID, res.SnoozedUntil)
	})
	return nil
}

func (a *App) snoozeThread(ctx context.Context, store *threads.Store, t threads.Thread, d time.Duration) (snoozeResult, error) {
	var until time.Time
	if d > 0 {
		until = a.Now().Add(d).Truncate(time.Second)
	}
	if _, err := store.Update(ctx, t.ID, func(u *threads.Thread) { u.SnoozedUntil = until }); err != nil {
		return snoozeResult{}, err
	}
	res := snoozeResult{OK: true, ThreadID: t.ID}
	if !until.IsZero() {
		res.SnoozedUntil = until.UTC().Format("2006-01-02T15:04:05Z")
	}
	return res, nil
}

type declineResult struct {
	OK       bool   `json:"ok"`
	ThreadID string `json:"thread_id"`
	Peer     string `json:"peer"`
	Status   string `json:"status"`
}

// decline closes a staged message without running it and tells the asker.
// It publishes to the bus, so it asks first unless --yes.
func (a *App) decline(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("decline", flag.ContinueOnError)
	c.bind(fs)
	yes := fs.Bool("yes", false, "skip the confirmation")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return fail(ExitUsage, "usage: wormhole decline <thread_id> [\"<reason>\"]")
	}
	cfg, err := a.loadConfig(c)
	if err != nil {
		return err
	}
	reason := ""
	if len(pos) == 2 {
		reason, err = a.readText(pos[1])
		if err != nil {
			return err
		}
		if strings.TrimSpace(reason) != "" {
			if err := a.guardBody(reason, false); err != nil {
				return err
			}
		}
	}
	store, err := a.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()
	t, err := a.stagedThread(ctx, store, pos[0])
	if err != nil {
		return err
	}
	if !*yes {
		if !a.IsTerminal() {
			return fail(ExitRefused, "not a terminal: confirm with the person, then rerun with --yes")
		}
		ok, err := a.confirm(declinePrompt(t, reason))
		if err != nil {
			return err
		}
		if !ok {
			return fail(ExitRefused, "not declined; it stays in the inbox")
		}
	}
	res, err := a.declineThread(ctx, cfg, store, t, reason)
	if err != nil {
		return err
	}
	a.emit(c, res, func(w io.Writer) {
		fmt.Fprintf(w, "declined thread %s; ^%s will see: %s\n", res.ThreadID, res.Peer, res.Status)
	})
	return nil
}

func declinePrompt(t threads.Thread, reason string) string {
	return fmt.Sprintf("Decline this message from ^%s? They will see: %s", t.Peer, envelope.DeclineStatus(reason))
}

// declineThread connects to the bus and sends the decline receipt.
func (a *App) declineThread(ctx context.Context, cfg *config.Config, store *threads.Store, t threads.Thread, reason string) (declineResult, error) {
	b, err := a.NewBus(ctx, cfg)
	if err != nil {
		return declineResult{}, fail(ExitUnavailable, "bus: %v", err)
	}
	defer b.Close()
	ack, err := daemon.Decline(ctx, b, store, a.me(cfg), t, reason, a.Now())
	if err != nil {
		switch {
		case errors.Is(err, daemon.ErrNothingStaged):
			return declineResult{}, fail(ExitRefused, "%v", err)
		case errors.Is(err, bus.ErrPermission):
			return declineResult{}, fail(ExitRefused, "%v", err)
		case strings.HasPrefix(err.Error(), "publish"):
			return declineResult{}, fail(ExitUnavailable, "%v", err)
		}
		return declineResult{}, err
	}
	return declineResult{OK: true, ThreadID: t.ID, Peer: t.Peer, Status: ack.Body}, nil
}
