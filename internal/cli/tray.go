package cli

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/threads"
	"github.com/yjmrobert/wormhole/internal/tray"
)

// startTray puts the inbox in the desktop panel for as long as the daemon
// runs: the icon and tooltip carry the waiting count, the menu lists the
// waiting messages and opens the inbox view, and the pause entry flips the
// local kill switch. The indicator follows the thread database, so a
// message opened from the CLI or the page leaves the menu within a second.
func (a *App) startTray(ctx context.Context, cfg *config.Config, store *threads.Store, d *daemon.Daemon, log *slog.Logger) (func(), error) {
	log = log.With("component", "tray")
	show := func() {
		go func() {
			if err := d.ShowInbox(ctx); err != nil {
				log.Warn("could not show the inbox", "err", err)
			}
		}()
	}
	ind, err := tray.Connect(ctx, log, tray.Actions{
		Show:     show,
		ShowItem: func(string) { show() },
		SetPaused: func(paused bool) {
			var err error
			if paused {
				err = daemon.Pause()
			} else {
				err = daemon.Resume()
			}
			if err != nil {
				log.Warn("could not flip the kill switch", "err", err)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	tctx, cancel := context.WithCancel(ctx)
	go func() {
		last := ""
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			state, err := trayState(tctx, store, a.Now())
			if err == nil {
				if fp := trayFingerprint(state); fp != last {
					last = fp
					ind.Update(state)
				}
			}
			select {
			case <-tctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		cancel()
		_ = ind.Close()
	}, nil
}

// trayState is what the indicator shows: the waiting (not snoozed) staged
// messages, newest first, and the kill switch.
func trayState(ctx context.Context, store *threads.Store, now time.Time) (tray.State, error) {
	list, err := store.List(ctx, threads.Filter{Open: true, Staged: true})
	if err != nil {
		return tray.State{}, err
	}
	s := tray.State{Paused: daemon.IsPaused()}
	for _, t := range list {
		if t.Snoozed(now) {
			continue
		}
		s.Items = append(s.Items, tray.Item{ID: t.ID, Peer: t.Peer, Preview: threads.Preview(t.LastPreview, 60), Age: ago(t.LastMessageAt, now)})
	}
	return s, nil
}

func trayFingerprint(s tray.State) string {
	var b strings.Builder
	for _, it := range s.Items {
		b.WriteString(it.ID + "|" + it.Preview + "|" + it.Age + ";")
	}
	if s.Paused {
		b.WriteString("paused")
	}
	return b.String()
}
