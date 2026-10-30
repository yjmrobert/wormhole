package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// watch is `wormhole inbox --watch`: the inbox kept open in the terminal,
// redrawn as messages arrive, with the selected message's full text below
// the list and one key per verb. It is the terminal where inbox mode's
// promise is kept: the person reads the whole kickoff here before enter
// (normal tools) or a (safe profile) runs it; s snoozes, d declines.
func (a *App) watch(ctx context.Context, c *common) error {
	if c.json {
		return fail(ExitUsage, "--watch and --json are exclusive; use `wormhole inbox --json` for the list")
	}
	in, ok := a.Stdin.(*os.File)
	if !ok || !isTerminal(in) {
		return fail(ExitRefused, "inbox --watch needs a terminal")
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
	w := &watcher{app: a, c: c, cfg: cfg, store: store, in: in, out: a.Stdout, keys: make(chan string, 16)}
	return w.run(ctx)
}

type watcher struct {
	app   *App
	c     *common
	cfg   *config.Config
	store *threads.Store
	in    *os.File
	out   io.Writer
	keys  chan string
	state *term.State

	items  []threads.Thread
	sel    int
	scroll int
	status string
	// input, when non-nil, is a line being typed in the status row.
	input *lineInput
}

type lineInput struct {
	prompt string
	buf    []rune
}

// Key names for escape sequences the view understands.
const (
	keyUp     = "\x1b[A"
	keyDown   = "\x1b[B"
	keyPgUp   = "\x1b[5~"
	keyPgDown = "\x1b[6~"
	keyEsc    = "\x1b"
	keyCtrlC  = "\x03"
	keyEnter  = "\r"
	keyBksp   = "\x7f"
)

func (w *watcher) run(ctx context.Context) error {
	if err := w.raw(); err != nil {
		return fail(ExitError, "raw mode: %v", err)
	}
	defer w.cooked()
	go w.readKeys()
	fmt.Fprint(w.out, "\x1b[?25l") // hide the cursor
	defer fmt.Fprint(w.out, "\x1b[?25h\x1b[2J\x1b[H")
	if err := w.refresh(ctx); err != nil {
		return err
	}
	w.render()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := w.refresh(ctx); err == nil {
				w.render()
			}
		case key := <-w.keys:
			quit, err := w.handle(ctx, key)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			w.render()
		}
	}
}

func (w *watcher) raw() error {
	st, err := term.MakeRaw(int(w.in.Fd()))
	if err != nil {
		return err
	}
	w.state = st
	return nil
}

func (w *watcher) cooked() {
	if w.state != nil {
		_ = term.Restore(int(w.in.Fd()), w.state)
		w.state = nil
	}
}

// readKeys forwards stdin to the key channel: an escape sequence as one
// key, plain bytes one by one.
func (w *watcher) readKeys() {
	buf := make([]byte, 32)
	for {
		n, err := w.in.Read(buf)
		if err != nil {
			close(w.keys)
			return
		}
		chunk := string(buf[:n])
		if strings.HasPrefix(chunk, keyEsc) {
			w.keys <- chunk
			continue
		}
		for _, r := range chunk {
			w.keys <- string(r)
		}
	}
}

// refresh reloads the inbox, keeping the selection on the same thread.
func (w *watcher) refresh(ctx context.Context) error {
	list, err := w.app.staged(ctx, w.store, true)
	if err != nil {
		return err
	}
	var selID string
	if w.sel < len(w.items) {
		selID = w.items[w.sel].ID
	}
	w.items = list
	w.sel = 0
	for i, t := range list {
		if t.ID == selID {
			w.sel = i
		}
	}
	return nil
}

func (w *watcher) selected() (threads.Thread, bool) {
	if w.sel < 0 || w.sel >= len(w.items) {
		return threads.Thread{}, false
	}
	return w.items[w.sel], true
}

// handle acts on one key. It returns true when the view should close.
func (w *watcher) handle(ctx context.Context, key string) (bool, error) {
	switch key {
	case "q", keyCtrlC:
		return true, nil
	case "j", keyDown:
		if w.sel < len(w.items)-1 {
			w.sel++
			w.scroll = 0
		}
	case "k", keyUp:
		if w.sel > 0 {
			w.sel--
			w.scroll = 0
		}
	case "J", keyPgDown, " ":
		w.scroll += w.paneHeight()
	case "K", keyPgUp:
		w.scroll -= w.paneHeight()
		if w.scroll < 0 {
			w.scroll = 0
		}
	case "r":
		if err := w.refresh(ctx); err != nil {
			return false, err
		}
		w.status = "refreshed"
	case keyEnter, "\n":
		w.openSelected(ctx, false)
	case "a":
		w.openSelected(ctx, true)
	case "s":
		w.snoozeSelected(ctx)
	case "d":
		w.declineSelected(ctx)
	}
	return false, nil
}

func (w *watcher) openSelected(ctx context.Context, agent bool) {
	t, ok := w.selected()
	if !ok {
		return
	}
	if err := w.app.checkAgentProfile(w.cfg, agent); err != nil {
		w.status = err.Error()
		return
	}
	if !w.confirm(openPrompt(t, agent)) {
		w.status = "not started; it stays in the inbox"
		return
	}
	// The kickoff runs for up to minutes and prints progress; give it the
	// terminal back for that.
	w.cooked()
	fmt.Fprint(w.out, "\x1b[2J\x1b[H\x1b[?25h")
	res, err := w.app.openThread(ctx, w.c, w.cfg, w.store, t, agent)
	fmt.Fprint(w.out, "\x1b[?25l")
	_ = w.raw()
	switch {
	case err != nil:
		w.status = "open failed: " + err.Error()
	case res.Opened:
		w.status = fmt.Sprintf("opened session %s in the app", res.SessionID)
	default:
		w.status = fmt.Sprintf("session %s started; the app did not open (%s); fallback: %s", res.SessionID, res.OpenError, res.Fallback)
	}
	_ = w.refresh(ctx)
}

func (w *watcher) snoozeSelected(ctx context.Context) {
	t, ok := w.selected()
	if !ok {
		return
	}
	def := daemon.ShortDuration(w.cfg.Daemon.SnoozeDefault)
	line, ok := w.readLine(fmt.Sprintf("snooze ^%s for [%s, or off]: ", t.Peer, def))
	if !ok {
		w.status = "snooze cancelled"
		return
	}
	d, err := parseSnooze(strings.TrimSpace(line), w.cfg.Daemon.SnoozeDefault)
	if err != nil {
		w.status = err.Error()
		return
	}
	res, err := w.app.snoozeThread(ctx, w.store, t, d)
	switch {
	case err != nil:
		w.status = "snooze failed: " + err.Error()
	case res.SnoozedUntil == "":
		w.status = fmt.Sprintf("thread %s is back in the inbox", t.ID)
	default:
		w.status = fmt.Sprintf("snoozed ^%s until %s", t.Peer, res.SnoozedUntil)
	}
	_ = w.refresh(ctx)
}

func (w *watcher) declineSelected(ctx context.Context) {
	t, ok := w.selected()
	if !ok {
		return
	}
	reason, ok := w.readLine(fmt.Sprintf("decline ^%s; reason for them (optional, Esc cancels): ", t.Peer))
	if !ok {
		w.status = "decline cancelled"
		return
	}
	if !w.confirm(declinePrompt(t, reason)) {
		w.status = "not declined; it stays in the inbox"
		return
	}
	res, err := w.app.declineThread(ctx, w.cfg, w.store, t, reason)
	if err != nil {
		w.status = "decline failed: " + err.Error()
	} else {
		w.status = fmt.Sprintf("declined; ^%s will see: %s", res.Peer, res.Status)
	}
	_ = w.refresh(ctx)
}

// confirm asks a yes/no question in the status row and reads one key.
func (w *watcher) confirm(prompt string) bool {
	w.input = &lineInput{prompt: prompt + " [y/N] "}
	w.render()
	defer func() { w.input = nil }()
	for key := range w.keys {
		switch key {
		case "y", "Y":
			return true
		case keyCtrlC:
			return false
		default:
			return false
		}
	}
	return false
}

// readLine reads a line typed in the status row. Esc cancels.
func (w *watcher) readLine(prompt string) (string, bool) {
	w.input = &lineInput{prompt: prompt}
	w.render()
	defer func() { w.input = nil }()
	for key := range w.keys {
		switch key {
		case keyEnter, "\n":
			return string(w.input.buf), true
		case keyEsc, keyCtrlC:
			return "", false
		case keyBksp, "\b":
			if n := len(w.input.buf); n > 0 {
				w.input.buf = w.input.buf[:n-1]
			}
		default:
			for _, r := range key {
				if unicode.IsPrint(r) {
					w.input.buf = append(w.input.buf, r)
				}
			}
		}
		w.render()
	}
	return "", false
}

// size is the terminal size, with a fallback for odd stdouts.
func (w *watcher) size() (int, int) {
	if f, ok := w.out.(*os.File); ok {
		if cols, rows, err := term.GetSize(int(f.Fd())); err == nil && cols > 0 && rows > 0 {
			return cols, rows
		}
	}
	return 80, 24
}

// listHeight is how many list rows fit: a third of the screen, at least
// three, never more than there are items.
func (w *watcher) listHeight() int {
	_, rows := w.size()
	h := rows / 3
	if h < 3 {
		h = 3
	}
	if h > len(w.items) {
		h = len(w.items)
	}
	return h
}

// paneHeight is the number of message lines that fit under the list.
func (w *watcher) paneHeight() int {
	_, rows := w.size()
	h := rows - 1 - w.listHeight() - 1 - 2
	if h < 1 {
		h = 1
	}
	return h
}

const (
	ansiReset   = "\x1b[0m"
	ansiDim     = "\x1b[2m"
	ansiBold    = "\x1b[1m"
	ansiReverse = "\x1b[7m"
	ansiClear   = "\x1b[2J\x1b[H"
)

func (w *watcher) render() {
	cols, _ := w.size()
	now := w.app.Now()
	var b strings.Builder
	b.WriteString(ansiClear)

	waiting, snoozed := 0, 0
	for _, t := range w.items {
		if t.Snoozed(now) {
			snoozed++
		} else {
			waiting++
		}
	}
	header := fmt.Sprintf("%sWORMHOLE INBOX%s  ^%s · %d waiting · %d snoozed · %s", ansiBold, ansiReset, w.cfg.Handle, waiting, snoozed, now.Format("15:04"))
	b.WriteString(clip(header, cols+len(ansiBold)+len(ansiReset)) + "\r\n")

	if len(w.items) == 0 {
		b.WriteString("\r\n  inbox empty; new messages appear here as they arrive\r\n")
	} else {
		// Keep the selection in view.
		h := w.listHeight()
		first := 0
		if w.sel >= h {
			first = w.sel - h + 1
		}
		for i := first; i < first+h && i < len(w.items); i++ {
			t := w.items[i]
			kind := "ask"
			if t.Direction == threads.DirectionOut {
				kind = "reply"
			}
			when := ago(t.LastMessageAt, now)
			if t.Snoozed(now) {
				when = "snoozed until " + t.SnoozedUntil.Local().Format("15:04")
			}
			id := t.ID
			if cols < 120 {
				id = string([]rune(id)[:10])
			}
			where := t.Repo
			row := fmt.Sprintf("  %s  %-12s %-6s %-22s %s  %s", id, "^"+t.Peer, kind, when, where, t.LastPreview)
			row = clip(row, cols)
			if i == w.sel {
				row = ansiReverse + "▸" + row[len("  "):] + ansiReset
			}
			b.WriteString(row + "\r\n")
		}
	}
	b.WriteString(ansiDim + strings.Repeat("─", cols) + ansiReset + "\r\n")

	if t, ok := w.selected(); ok {
		lines := wrapLines(t.StagedKickoff, cols)
		pane := w.paneHeight()
		if max := len(lines) - pane; w.scroll > max {
			w.scroll = max
		}
		if w.scroll < 0 {
			w.scroll = 0
		}
		// The fixed preamble, up to and including the --- line, is dimmed
		// so the colleague's own words stand out.
		preambleEnd := -1
		for j, line := range lines {
			if strings.TrimSpace(line) == "---" {
				preambleEnd = j
				break
			}
		}
		for i := 0; i < pane; i++ {
			j := w.scroll + i
			if j >= len(lines) {
				b.WriteString("\r\n")
				continue
			}
			if j <= preambleEnd {
				b.WriteString(ansiDim + lines[j] + ansiReset + "\r\n")
			} else {
				b.WriteString(lines[j] + "\r\n")
			}
		}
	} else {
		for i := 0; i < w.paneHeight(); i++ {
			b.WriteString("\r\n")
		}
	}

	if w.input != nil {
		b.WriteString(clip(w.input.prompt+string(w.input.buf), cols-1) + "▏\r\n")
	} else {
		b.WriteString(clip(w.status, cols) + "\r\n")
	}
	keys := fmt.Sprintf("%senter%s open with my tools  %sa%s hand to agent  %ss%s snooze  %sd%s decline  %sj/k%s move  %sJ/K%s scroll  %sq%s quit",
		ansiBold, ansiReset, ansiBold, ansiReset, ansiBold, ansiReset, ansiBold, ansiReset, ansiBold, ansiReset, ansiBold, ansiReset, ansiBold, ansiReset)
	b.WriteString(keys)
	fmt.Fprint(w.out, b.String())
}

// clip cuts s to at most n runes, marking the cut with an ellipsis.
func clip(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// wrapLines splits text into lines no wider than cols runes.
func wrapLines(text string, cols int) []string {
	if cols < 1 {
		cols = 1
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > cols {
			out = append(out, string(r[:cols]))
			r = r[cols:]
		}
		out = append(out, string(r))
	}
	return out
}
