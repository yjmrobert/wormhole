package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// inboxItem is one staged inbound message awaiting a person.
type inboxItem struct {
	ThreadID string `json:"thread_id"`
	// Short is the shortest unique prefix of the id on this machine and
	// Ref the ^peer or ^peer/n form; every command accepts either.
	Short        string `json:"short"`
	Ref          string `json:"ref,omitempty"`
	Peer         string `json:"peer"`
	Kind         string `json:"kind"` // ask or reply
	Title        string `json:"title"`
	Preview      string `json:"preview"`
	Received     string `json:"received"`
	Repo         string `json:"repo,omitempty"`
	SnoozedUntil string `json:"snoozed_until,omitempty"`
	// Text is the full staged message, so an agent triaging the inbox can
	// show it to the person without a second command.
	Text string `json:"text"`
}

// inboxItemOf converts a stored thread into the inbox output schema.
func inboxItemOf(t threads.Thread) inboxItem {
	kind := "ask"
	if t.Direction == threads.DirectionOut {
		kind = "reply"
	}
	it := inboxItem{ThreadID: t.ID, Peer: t.Peer, Kind: kind, Title: t.Title, Preview: t.LastPreview, Received: t.LastMessageAt.UTC().Format(utcTimestampLayout), Repo: t.Repo, Text: t.StagedKickoff}
	if !t.SnoozedUntil.IsZero() {
		it.SnoozedUntil = t.SnoozedUntil.UTC().Format(utcTimestampLayout)
	}
	return it
}

// staged lists the messages waiting in the inbox, newest first. Snoozed
// ones are left out unless all is set.
func (a *App) staged(ctx context.Context, store *threads.Store, all bool) ([]threads.Thread, error) {
	list, err := store.List(ctx, threads.Filter{Open: true, Staged: true})
	if err != nil {
		return nil, err
	}
	now := a.Now()
	out := make([]threads.Thread, 0, len(list))
	for _, t := range list {
		if !all && t.Snoozed(now) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// newInboxCommand lists staged messages or opens an interactive inbox view.
func (a *App) newInboxCommand(c *common) *cobra.Command {
	var watch, ui, all bool
	cmd := command("inbox", "List staged messages waiting for review")
	cmd.Flags().BoolVar(&watch, "watch", false, "keep the inbox open in the terminal and act on messages with one key each")
	cmd.Flags().BoolVar(&ui, "ui", false, "open the inbox page the daemon serves (daemon.inbox_ui) in the browser")
	cmd.Flags().BoolVar(&all, "all", false, "include snoozed messages")
	cmd.Args = noArgs("wormhole inbox [--watch|--ui] [--all]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if watch && ui {
			return fail(ExitUsage, "--watch and --ui are exclusive")
		}
		if watch {
			if c.noInput {
				return fail(ExitRefused, "inbox --watch requires interactive input")
			}
			return a.watch(cmd.Context(), c)
		}
		if ui {
			return a.inboxUI(cmd.Context(), c)
		}
		store, err := a.OpenStore()
		if err != nil {
			return err
		}
		defer store.Close()
		list, err := a.staged(cmd.Context(), store, all)
		if err != nil {
			return err
		}
		short, refs := a.refsFor(cmd.Context(), store)
		items := make([]inboxItem, 0, len(list))
		for _, t := range list {
			it := inboxItemOf(t)
			it.Short, it.Ref = short[t.ID], refs[t.ID]
			items = append(items, it)
		}
		a.emit(c, map[string]any{"ok": true, "inbox": items}, func(w io.Writer) {
			if len(items) == 0 {
				fmt.Fprintln(w, "inbox empty")
				return
			}
			rows := make([][]string, len(items))
			for i, it := range items {
				when := it.Received
				if it.SnoozedUntil != "" {
					when = "snoozed until " + it.SnoozedUntil
				}
				rows[i] = []string{refOrShort(it.ThreadID, short, refs), it.ThreadID, "^" + it.Peer, it.Kind, when, it.Preview}
			}
			table(w, []string{"REF", "THREAD", "FROM", "KIND", "RECEIVED", "PREVIEW"}, rows)
			fmt.Fprintln(w, "\nopen one with: wormhole open ^<peer>   hand it to your agent: wormhole open ^<peer> --agent")
			fmt.Fprintln(w, "later: wormhole snooze ^<peer> [2h]      pass: wormhole decline ^<peer> [\"reason\"]      live view: wormhole inbox --watch")
		})
		return nil
	}
	return cmd
}

// inboxUI opens the inbox page a running daemon serves.
func (a *App) inboxUI(ctx context.Context, c *common) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return err
	}
	url, err := inboxURL()
	if err != nil {
		if cfg.Daemon.InboxUI == "" {
			return fail(ExitUnavailable, "the inbox page is off; set daemon.inbox_ui (127.0.0.1:7788) and restart the daemon")
		}
		return fail(ExitUnavailable, "the inbox page is not running (no %s); start the daemon", config.InboxURLFile())
	}
	opened := false
	var openErr string
	if !c.json {
		if err := a.NewGHApp(cfg).ShowInbox(ctx, []string{"xdg-open", url}); err != nil {
			openErr = err.Error()
		} else {
			opened = true
		}
	}
	a.emit(c, map[string]any{"ok": true, "url": url, "opened": opened}, func(w io.Writer) {
		if opened {
			fmt.Fprintf(w, "opened %s\n", url)
		} else {
			fmt.Fprintf(w, "open this in your browser: %s\n(%s)\n", url, openErr)
		}
	})
	return nil
}

// openResult is daemon.OpenResult with the ok field every command prints.
type openResult struct {
	OK bool `json:"ok"`
	daemon.OpenResult
}

// newOpenCommand runs or prints a staged message and shows its session.
func (a *App) newOpenCommand(c *common) *cobra.Command {
	var noStart, yes, agent bool
	cmd := command("open <thread>", "Run a staged message and open its session")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "do not run a staged message; just print it")
	cmd.Flags().BoolVar(&yes, "yes", false, "run a staged message without showing it first")
	cmd.Flags().BoolVar(&agent, "agent", false, "run the staged message headless with copilot.safe_tools and extra_args, as auto mode would, instead of with your normal tools")
	cmd.Args = exactArgs(1, "wormhole open <thread>")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		store, err := a.OpenStore()
		if err != nil {
			return err
		}
		defer store.Close()
		t, err := a.resolveThread(cmd.Context(), store, args[0])
		if err != nil {
			return err
		}
		if t.StagedKickoff == "" && t.LocalSessionID == "" {
			return fail(ExitNotFound, "thread %s has no local session and nothing staged; see: wormhole threads", t.ID)
		}
		if t.StagedKickoff != "" {
			if noStart {
				fmt.Fprintln(a.Stdout, t.StagedKickoff)
				return nil
			}
			if err := a.checkAgentProfile(cfg, agent); err != nil {
				return err
			}
			// Inbox mode's promise is that a person reads inbound text before
			// any tool runs on it. The message runs with the person's normal
			// tool set (or, with --agent, the auto-mode profile they chose),
			// so the person, not an agent, has to be the one asking: show it
			// and confirm, and off a terminal require --yes.
			if !yes {
				if c.noInput {
					return fail(ExitRefused, "--no-input forbids confirmation prompts; review the staged message and rerun with --yes")
				}
				if !a.IsTerminal() {
					return fail(ExitRefused, "not a terminal: show the staged message to the person (wormhole open --no-start), then rerun with --yes")
				}
				fmt.Fprintf(a.Stdout, "%s\n\n", t.StagedKickoff)
				ok, err := a.confirm(openPrompt(t, agent))
				if err != nil {
					return err
				}
				if !ok {
					return fail(ExitRefused, notStartedMessage)
				}
			}
		}
		res, err := a.openThread(cmd.Context(), c, cfg, store, t, agent)
		if err != nil {
			return err
		}
		a.emit(c, res, func(w io.Writer) {
			if res.Resumed {
				fmt.Fprintf(w, "appended the message to session %s\n", res.SessionID)
			}
			if res.Opened {
				fmt.Fprintf(w, "opened session %s in the app\n", res.SessionID)
			} else {
				fmt.Fprintf(w, "could not open the app (%s)\n", res.OpenError)
			}
			fmt.Fprintf(w, "fallback: %s\n", res.Fallback)
		})
		return nil
	}
	return cmd
}

// openPrompt is the confirmation a person answers before a staged message
// runs.
func openPrompt(t threads.Thread, agent bool) string {
	if agent {
		return fmt.Sprintf("Hand this message from ^%s to your agent with the safe profile (copilot.safe_tools)?", t.Peer)
	}
	return fmt.Sprintf("Run this message from ^%s with your normal tools?", t.Peer)
}

// checkAgentProfile refuses --agent when no auto-mode profile is set, the
// same way the daemon refuses auto mode.
func (a *App) checkAgentProfile(cfg *config.Config, agent bool) error {
	if err := daemon.CheckAgentProfile(cfg, agent); err != nil {
		return fail(ExitRefused, "%v", err)
	}
	return nil
}

// openThread runs t's staged message, if any, and shows the thread's
// session in the app; see daemon.OpenStaged. `wormhole open` and `wormhole
// inbox --watch` both come here after the person has read the text and
// confirmed.
func (a *App) openThread(ctx context.Context, c *common, cfg *config.Config, store *threads.Store, t threads.Thread, agent bool) (openResult, error) {
	var progress io.Writer
	if !c.json {
		progress = a.Stderr
	}
	res, _, err := daemon.OpenStaged(ctx, cfg, store, a.NewGHApp(cfg), t, agent, progress)
	out := openResult{OK: err == nil, OpenResult: res}
	switch {
	case err == nil:
	case errors.Is(err, daemon.ErrNoAgentProfile):
		return out, fail(ExitRefused, "%v", err)
	case errors.Is(err, daemon.ErrNothingToOpen):
		return out, fail(ExitNotFound, "%v", err)
	case errors.Is(err, daemon.ErrStartSession):
		return out, fail(ExitUnavailable, "%v", err)
	default:
		return out, err
	}
	return out, nil
}

// newThreadsCommand lists and filters locally stored threads.
func (a *App) newThreadsCommand(c *common) *cobra.Command {
	var open bool
	var peer string
	cmd := command("threads", "List local threads")
	cmd.Flags().BoolVar(&open, "open", false, "only threads that still expect traffic")
	cmd.Flags().StringVar(&peer, "peer", "", "only threads with this handle")
	cmd.Args = noArgs("wormhole threads [--open] [--peer <user>]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if peer != "" && !envelope.ValidHandle(peer) {
			return fail(ExitNotFound, invalidHandleFormat, peer)
		}
		store, err := a.OpenStore()
		if err != nil {
			return err
		}
		defer store.Close()
		_, _ = store.ExpireStale(cmd.Context(), a.Now())
		list, err := store.List(cmd.Context(), threads.Filter{Open: open, Peer: peer})
		if err != nil {
			return err
		}
		if list == nil {
			list = []threads.Thread{}
		}
		short, refs := a.refsFor(cmd.Context(), store)
		rows := make([]threadRow, len(list))
		for i, t := range list {
			rows[i] = threadRow{Thread: t, Short: short[t.ID], Ref: refs[t.ID]}
		}
		a.emit(c, map[string]any{"ok": true, "threads": rows}, func(w io.Writer) {
			if len(list) == 0 {
				fmt.Fprintln(w, "no threads")
				return
			}
			rows := make([][]string, len(list))
			for i, t := range list {
				dir := "->"
				if t.Direction == threads.DirectionIn {
					dir = "<-"
				}
				sess := t.LocalSessionID
				if t.StagedKickoff != "" {
					sess = "(inbox)"
					if t.Snoozed(a.Now()) {
						sess = "(snoozed)"
					}
				}
				rows[i] = []string{refOrShort(t.ID, short, refs), t.ID, dir + " ^" + t.Peer, string(t.State), ago(t.LastMessageAt, a.Now()), sess, t.LastPreview}
			}
			table(w, []string{"REF", "THREAD", "PEER", "STATE", "LAST", "SESSION", "PREVIEW"}, rows)
		})
		return nil
	}
	return cmd
}

// threadRow is a thread with its short forms, for listings.
type threadRow struct {
	threads.Thread
	Short string `json:"short"`
	Ref   string `json:"ref,omitempty"`
}

type userRow struct {
	bus.Presence
	Online bool `json:"online"`
}

// newUsersCommand lists registered handles and their presence.
func (a *App) newUsersCommand(c *common) *cobra.Command {
	cmd := command("users", "List handles registered on the bus")
	cmd.Args = noArgs("wormhole users")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		b, err := a.NewBus(cmd.Context(), cfg)
		if err != nil {
			return fail(ExitUnavailable, busErrorFormat, err)
		}
		defer b.Close()
		list, err := b.Registry().List(cmd.Context())
		if err != nil {
			return fail(ExitUnavailable, "registry: %v", err)
		}
		rows := make([]userRow, 0, len(list))
		for _, p := range list {
			rows = append(rows, userRow{Presence: p, Online: a.Now().Sub(p.LastSeen) < 2*cfg.Daemon.RegistryInterval})
		}
		a.emit(c, map[string]any{"ok": true, "users": rows}, func(w io.Writer) {
			if len(rows) == 0 {
				fmt.Fprintln(w, "no daemons have registered on this bus")
				return
			}
			out := make([][]string, len(rows))
			for i, r := range rows {
				state := "offline"
				if r.Online {
					state = "online"
				}
				out[i] = []string{"^" + r.Handle, r.Host, r.OS, r.Mode, state, ago(r.LastSeen, a.Now())}
			}
			table(w, []string{"HANDLE", "HOST", "OS", "MODE", "PRESENCE", "LAST SEEN"}, out)
		})
		return nil
	}
	return cmd
}
