package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// inboxItem is one staged inbound message awaiting a person.
type inboxItem struct {
	ThreadID     string `json:"thread_id"`
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

func inboxItemOf(t threads.Thread) inboxItem {
	kind := "ask"
	if t.Direction == threads.DirectionOut {
		kind = "reply"
	}
	it := inboxItem{ThreadID: t.ID, Peer: t.Peer, Kind: kind, Title: t.Title, Preview: t.LastPreview, Received: t.LastMessageAt.UTC().Format("2006-01-02T15:04:05Z"), Repo: t.Repo, Text: t.StagedKickoff}
	if !t.SnoozedUntil.IsZero() {
		it.SnoozedUntil = t.SnoozedUntil.UTC().Format("2006-01-02T15:04:05Z")
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

func (a *App) inbox(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	c.bind(fs)
	watch := fs.Bool("watch", false, "keep the inbox open in the terminal and act on messages with one key each")
	ui := fs.Bool("ui", false, "open the inbox page the daemon serves (daemon.inbox_ui) in the browser")
	all := fs.Bool("all", false, "include snoozed messages")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	if *watch && *ui {
		return fail(ExitUsage, "--watch and --ui are exclusive")
	}
	if *watch {
		return a.watch(ctx, c)
	}
	if *ui {
		return a.inboxUI(ctx, c)
	}
	store, err := a.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()
	list, err := a.staged(ctx, store, *all)
	if err != nil {
		return err
	}
	items := make([]inboxItem, 0, len(list))
	for _, t := range list {
		items = append(items, inboxItemOf(t))
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
			rows[i] = []string{it.ThreadID, "^" + it.Peer, it.Kind, when, it.Preview}
		}
		table(w, []string{"THREAD", "FROM", "KIND", "RECEIVED", "PREVIEW"}, rows)
		fmt.Fprintln(w, "\nopen one with: wormhole open <thread>   hand it to your agent: wormhole open <thread> --agent")
		fmt.Fprintln(w, "later: wormhole snooze <thread> [2h]      pass: wormhole decline <thread> [\"reason\"]      live view: wormhole inbox --watch")
	})
	return nil
}

// openResult is daemon.OpenResult with the ok field every command prints.
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

type openResult struct {
	OK bool `json:"ok"`
	daemon.OpenResult
}

// open runs the staged message for a thread if there is one (appending it
// to the session that asked, or starting a new session), then shows the
// session in the app. It always prints the --resume fallback.
func (a *App) open(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	c.bind(fs)
	noStart := fs.Bool("no-start", false, "do not run a staged message; just print it")
	yes := fs.Bool("yes", false, "run a staged message without showing it first")
	agent := fs.Bool("agent", false, "run the staged message headless with copilot.safe_tools and extra_args, as auto mode would, instead of with your normal tools")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail(ExitUsage, "usage: wormhole open <thread_id>")
	}
	if !envelope.ValidID(pos[0]) {
		return fail(ExitNotFound, "%q is not a thread id", pos[0])
	}
	cfg, err := a.loadConfig(c)
	if err != nil {
		return err
	}
	store, err := a.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()
	t, err := store.Get(ctx, pos[0])
	if err != nil {
		return fail(ExitNotFound, "thread %s is not on this machine", pos[0])
	}
	if t.StagedKickoff == "" && t.LocalSessionID == "" {
		return fail(ExitNotFound, "thread %s has no local session and nothing staged", t.ID)
	}
	if t.StagedKickoff != "" {
		if *noStart {
			fmt.Fprintln(a.Stdout, t.StagedKickoff)
			return nil
		}
		if err := a.checkAgentProfile(cfg, *agent); err != nil {
			return err
		}
		// Inbox mode's promise is that a person reads inbound text before
		// any tool runs on it. The message runs with the person's normal
		// tool set (or, with --agent, the auto-mode profile they chose),
		// so the person, not an agent, has to be the one asking: show it
		// and confirm, and off a terminal require --yes.
		if !*yes {
			if !a.IsTerminal() {
				return fail(ExitRefused, "not a terminal: show the staged message to the person (wormhole open --no-start), then rerun with --yes")
			}
			fmt.Fprintf(a.Stdout, "%s\n\n", t.StagedKickoff)
			ok, err := a.confirm(openPrompt(t, *agent))
			if err != nil {
				return err
			}
			if !ok {
				return fail(ExitRefused, "not started; it stays in the inbox")
			}
		}
	}
	res, err := a.openThread(ctx, c, cfg, store, t, *agent)
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

func (a *App) threads(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("threads", flag.ContinueOnError)
	c.bind(fs)
	open := fs.Bool("open", false, "only threads that still expect traffic")
	peer := fs.String("peer", "", "only threads with this handle")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	if *peer != "" && !envelope.ValidHandle(*peer) {
		return fail(ExitNotFound, "%q is not a valid handle", *peer)
	}
	store, err := a.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()
	_, _ = store.ExpireStale(ctx, a.Now())
	list, err := store.List(ctx, threads.Filter{Open: *open, Peer: *peer})
	if err != nil {
		return err
	}
	if list == nil {
		list = []threads.Thread{}
	}
	a.emit(c, map[string]any{"ok": true, "threads": list}, func(w io.Writer) {
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
			rows[i] = []string{t.ID, dir + " ^" + t.Peer, string(t.State), ago(t.LastMessageAt, a.Now()), sess, t.LastPreview}
		}
		table(w, []string{"THREAD", "PEER", "STATE", "LAST", "SESSION", "PREVIEW"}, rows)
	})
	return nil
}

type userRow struct {
	bus.Presence
	Online bool `json:"online"`
}

func (a *App) users(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("users", flag.ContinueOnError)
	c.bind(fs)
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	cfg, err := a.loadConfig(c)
	if err != nil {
		return err
	}
	b, err := a.NewBus(ctx, cfg)
	if err != nil {
		return fail(ExitUnavailable, "bus: %v", err)
	}
	defer b.Close()
	list, err := b.Registry().List(ctx)
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
