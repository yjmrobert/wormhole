package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// stagedThread loads the thread a verb acts on and checks that a message is
// waiting on it.
func (a *App) stagedThread(ctx context.Context, store *threads.Store, ref string) (threads.Thread, error) {
	t, err := a.resolveThread(ctx, store, ref)
	if err != nil {
		return threads.Thread{}, err
	}
	if t.StagedKickoff == "" {
		return threads.Thread{}, fail(ExitRefused, "nothing is waiting on thread %s (state %s); see what is: wormhole inbox", t.ID, t.State)
	}
	return t, nil
}

// parseSnooze is daemon.ParseSnooze with the CLI's usage exit code.
func parseSnooze(s string, def time.Duration) (time.Duration, error) {
	d, err := daemon.ParseSnooze(s, def)
	if err != nil {
		return 0, fail(ExitUsage, "%v", err)
	}
	return d, nil
}

type snoozeResult struct {
	OK           bool   `json:"ok"`
	ThreadID     string `json:"thread_id"`
	SnoozedUntil string `json:"snoozed_until,omitempty"`
}

// snoozeThread hides a staged message for a while. It is local and reversible.
func (a *App) newSnoozeCommand(c *common) *cobra.Command {
	cmd := command("snooze <thread> [duration|off]", "Hide a staged message until later")
	cmd.Args = rangeArgs(1, 2, "wormhole snooze <thread> [<duration>|off]")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		length := ""
		if len(args) == 2 {
			length = args[1]
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
		t, err := a.stagedThread(cmd.Context(), store, args[0])
		if err != nil {
			return err
		}
		res, err := a.snoozeThread(cmd.Context(), store, t, d)
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
	return cmd
}

// snoozeThread applies a snooze duration and formats its result.
func (a *App) snoozeThread(ctx context.Context, store *threads.Store, t threads.Thread, d time.Duration) (snoozeResult, error) {
	until, err := daemon.Snooze(ctx, store, t, a.Now(), d)
	if err != nil {
		if errors.Is(err, daemon.ErrNothingStaged) {
			return snoozeResult{}, fail(ExitRefused, "%v", err)
		}
		return snoozeResult{}, err
	}
	res := snoozeResult{OK: true, ThreadID: t.ID}
	if !until.IsZero() {
		res.SnoozedUntil = until.UTC().Format(utcTimestampLayout)
	}
	return res, nil
}

type declineResult struct {
	OK       bool   `json:"ok"`
	ThreadID string `json:"thread_id"`
	Peer     string `json:"peer"`
	Status   string `json:"status"`
}

// newDeclineCommand closes a staged message without running it and tells the asker.
func (a *App) newDeclineCommand(c *common) *cobra.Command {
	var yes bool
	cmd := command("decline <thread> [reason]", "Decline a staged message and notify the sender")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation")
	cmd.Args = rangeArgs(1, 2, `wormhole decline <thread> ["<reason>"]`)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		reason := ""
		if len(args) == 2 {
			reason, err = a.readText(args[1])
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
		t, err := a.stagedThread(cmd.Context(), store, args[0])
		if err != nil {
			return err
		}
		if !yes {
			if c.noInput {
				return fail(ExitRefused, noInputReviewMessage)
			}
			if !a.IsTerminal() {
				return fail(ExitRefused, "not a terminal: confirm with the person, then rerun with --yes")
			}
			ok, err := a.confirm(declinePrompt(t, reason))
			if err != nil {
				return err
			}
			if !ok {
				return fail(ExitRefused, notDeclinedMessage+"; when ready: wormhole decline %s", t.ID)
			}
		}
		res, err := a.declineThread(cmd.Context(), cfg, store, t, reason)
		if err != nil {
			return err
		}
		a.emit(c, res, func(w io.Writer) {
			fmt.Fprintf(w, "declined thread %s; ^%s will see: %s\n", res.ThreadID, res.Peer, res.Status)
		})
		return nil
	}
	return cmd
}

// declinePrompt describes the receipt a sender will receive.
func declinePrompt(t threads.Thread, reason string) string {
	return fmt.Sprintf("Decline this message from ^%s? They will see: %s", t.Peer, envelope.DeclineStatus(reason))
}

// declineThread connects to the bus and sends the decline receipt.
func (a *App) declineThread(ctx context.Context, cfg *config.Config, store *threads.Store, t threads.Thread, reason string) (declineResult, error) {
	b, err := a.NewBus(ctx, cfg)
	if err != nil {
		return declineResult{}, fail(ExitUnavailable, busErrorFormat, err)
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
