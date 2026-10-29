package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// inboxItem is one staged inbound message awaiting a person.
type inboxItem struct {
	ThreadID string `json:"thread_id"`
	Peer     string `json:"peer"`
	Kind     string `json:"kind"` // ask or reply
	Title    string `json:"title"`
	Preview  string `json:"preview"`
	Received string `json:"received"`
	Repo     string `json:"repo,omitempty"`
}

func (a *App) inbox(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	c.bind(fs)
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	store, err := a.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()
	list, err := store.List(ctx, threads.Filter{Open: true})
	if err != nil {
		return err
	}
	var items []inboxItem
	for _, t := range list {
		if t.StagedKickoff == "" {
			continue
		}
		kind := "ask"
		if t.Direction == threads.DirectionOut {
			kind = "reply"
		}
		items = append(items, inboxItem{ThreadID: t.ID, Peer: t.Peer, Kind: kind, Title: t.Title, Preview: t.LastPreview, Received: t.LastMessageAt.UTC().Format("2006-01-02T15:04:05Z"), Repo: t.Repo})
	}
	if items == nil {
		items = []inboxItem{}
	}
	a.emit(c, map[string]any{"ok": true, "inbox": items}, func(w io.Writer) {
		if len(items) == 0 {
			fmt.Fprintln(w, "inbox empty")
			return
		}
		rows := make([][]string, len(items))
		for i, it := range items {
			rows[i] = []string{it.ThreadID, "^" + it.Peer, it.Kind, it.Received, it.Preview}
		}
		table(w, []string{"THREAD", "FROM", "KIND", "RECEIVED", "PREVIEW"}, rows)
		fmt.Fprintln(w, "\nopen one with: wormhole open <thread>")
	})
	return nil
}

type openResult struct {
	OK        bool   `json:"ok"`
	ThreadID  string `json:"thread_id"`
	SessionID string `json:"session_id"`
	Started   bool   `json:"started"`
	Resumed   bool   `json:"resumed"`
	Opened    bool   `json:"opened"`
	Fallback  string `json:"fallback"`
	OpenError string `json:"open_error,omitempty"`
}

// open runs the staged message for a thread if there is one (appending it
// to the session that asked, or starting a new session), then shows the
// session in the app. It always prints the --resume fallback.
func (a *App) open(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	c.bind(fs)
	noStart := fs.Bool("no-start", false, "do not run a staged message; just print it")
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
	app := a.NewGHApp(cfg)
	res := openResult{OK: true, ThreadID: t.ID}
	if t.StagedKickoff == "" && t.LocalSessionID == "" {
		return fail(ExitNotFound, "thread %s has no local session and nothing staged", t.ID)
	}
	if t.StagedKickoff != "" {
		if *noStart {
			fmt.Fprintln(a.Stdout, t.StagedKickoff)
			return nil
		}
		workdir := t.Workdir
		if workdir == "" {
			workdir = cfg.WorkdirFor(t.Repo)
		}
		// A person asked for this, so it runs with their normal tool set:
		// no --available-tools restriction here.
		sessionID := daemon.ContinueSession(cfg, t)
		if sessionID != "" {
			if !c.json {
				fmt.Fprintf(a.Stderr, "appending the message to session %s (it runs headless first; this can take a minute)\n", sessionID)
			}
			if err := app.Resume(ctx, ghapp.Session{ID: sessionID, Kickoff: t.StagedKickoff, Workdir: workdir, Model: cfg.Copilot.Model}); err != nil {
				fmt.Fprintf(a.Stderr, "could not resume session %s (%v); starting a new one\n", sessionID, err)
				sessionID = ""
			} else {
				res.Resumed = true
			}
		}
		if sessionID == "" {
			sessionID = ghapp.NewSessionID()
			name := ghapp.SessionName(t.Peer, t.Title)
			if t.Direction == threads.DirectionOut {
				name = fmt.Sprintf("Reply from ^%s", t.Peer)
			}
			if !c.json {
				fmt.Fprintf(a.Stderr, "starting session %s in %s (the kickoff runs headless first; this can take a minute)\n", sessionID, workdir)
			}
			err := app.Create(ctx, ghapp.Session{ID: sessionID, Name: name, Kickoff: t.StagedKickoff, Workdir: workdir, Model: cfg.Copilot.Model})
			if err != nil {
				return fail(ExitUnavailable, "start session: %v", err)
			}
		}
		res.Started = true
		t, err = store.Update(ctx, t.ID, func(t *threads.Thread) {
			t.LocalSessionID = sessionID
			t.StagedKickoff = ""
			t.Workdir = workdir
			if t.State == threads.StatePending {
				t.State = threads.StateDelivered
			}
		})
		if err != nil {
			return err
		}
	}
	res.SessionID = t.LocalSessionID
	res.Fallback = ghapp.ResumeCommand(t.LocalSessionID)
	if err := app.Open(ctx, t.LocalSessionID); err != nil {
		res.OpenError = err.Error()
	} else {
		res.Opened = true
		_, _ = store.Update(ctx, t.ID, func(t *threads.Thread) { t.Opened = true })
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
