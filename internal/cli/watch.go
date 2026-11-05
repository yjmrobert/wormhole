package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

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
		return fail(ExitRefused, "inbox --watch needs a terminal; list the inbox instead with: wormhole inbox")
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

	w := newWatchModel(ctx, a, c, cfg, store)
	if err := w.refresh(ctx); err != nil {
		return err
	}
	p := tea.NewProgram(w, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(a.Stdout))
	w.releaseTerminal = p.ReleaseTerminal
	w.restoreTerminal = p.RestoreTerminal
	_, err = p.Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type watchMode int

const clockLayout = "15:04"

const (
	watchBrowse watchMode = iota
	watchSnoozeInput
	watchDeclineInput
	watchOpenConfirm
	watchDeclineConfirm
)

type watchTickMsg time.Time

type watchRefreshMsg struct {
	id     int
	manual bool
	items  []threads.Thread
	err    error
}

type watchOpenMsg struct {
	res        openResult
	err        error
	restoreErr error
}

type watchSnoozeMsg struct {
	thread threads.Thread
	res    snoozeResult
	err    error
}

type watchDeclineMsg struct {
	res declineResult
	err error
}

type watcher struct {
	ctx   context.Context
	app   *App
	c     *common
	cfg   *config.Config
	store *threads.Store

	width  int
	height int
	items  []threads.Thread
	sel    int
	status string
	mode   watchMode
	prompt string

	pending       threads.Thread
	pendingAgent  bool
	pendingReason string
	busy          bool

	viewport viewport.Model
	input    textinput.Model

	refreshing bool
	refreshID  int

	loadStaged      func(context.Context) ([]threads.Thread, error)
	openThread      func(context.Context, threads.Thread, bool) (openResult, error)
	snoozeThread    func(context.Context, threads.Thread, time.Duration) (snoozeResult, error)
	declineThread   func(context.Context, threads.Thread, string) (declineResult, error)
	releaseTerminal func() error
	restoreTerminal func() error
}

// newWatchModel constructs an inbox model with its production collaborators.
func newWatchModel(ctx context.Context, a *App, c *common, cfg *config.Config, store *threads.Store) *watcher {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.FillHeight = true
	in := textinput.New()
	in.SetVirtualCursor(false)
	w := &watcher{
		ctx:      ctx,
		app:      a,
		c:        c,
		cfg:      cfg,
		store:    store,
		width:    80,
		height:   24,
		viewport: vp,
		input:    in,
	}
	w.loadStaged = func(ctx context.Context) ([]threads.Thread, error) {
		return a.staged(ctx, store, true)
	}
	w.openThread = func(ctx context.Context, t threads.Thread, agent bool) (openResult, error) {
		return a.openThread(ctx, c, cfg, store, t, agent)
	}
	w.snoozeThread = func(ctx context.Context, t threads.Thread, d time.Duration) (snoozeResult, error) {
		return a.snoozeThread(ctx, store, t, d)
	}
	w.declineThread = func(ctx context.Context, t threads.Thread, reason string) (declineResult, error) {
		return a.declineThread(ctx, cfg, store, t, reason)
	}
	w.resize()
	return w
}

// Init starts the periodic inbox refresh schedule.
func (w *watcher) Init() tea.Cmd {
	return watchTick()
}

// watchTick schedules the next refresh event.
func watchTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return watchTickMsg(t) })
}

// Update applies terminal, refresh, and command-completion events to the model.
func (w *watcher) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		w.width, w.height = msg.Width, msg.Height
		w.resize()
		return w, nil
	case watchTickMsg:
		if w.busy || w.refreshing {
			return w, watchTick()
		}
		return w, tea.Batch(w.nextRefresh(false), watchTick())
	case watchRefreshMsg:
		if msg.id != w.refreshID {
			return w, nil
		}
		w.refreshing = false
		if msg.err != nil {
			w.status = "refresh failed: " + msg.err.Error()
			return w, nil
		}
		w.setItems(msg.items)
		if msg.manual {
			w.status = "refreshed"
		}
		return w, nil
	case watchOpenMsg:
		w.busy = false
		switch {
		case msg.err != nil && msg.restoreErr != nil:
			w.status = fmt.Sprintf("open failed: %v; restore terminal: %v", msg.err, msg.restoreErr)
		case msg.err != nil:
			w.status = "open failed: " + msg.err.Error()
		case msg.restoreErr != nil:
			w.status = "restore terminal: " + msg.restoreErr.Error()
		case msg.res.Opened:
			w.status = fmt.Sprintf("opened session %s in the app", msg.res.SessionID)
		default:
			w.status = fmt.Sprintf("session %s started; the app did not open (%s); fallback: %s", msg.res.SessionID, msg.res.OpenError, msg.res.Fallback)
		}
		return w, w.nextRefresh(false)
	case watchSnoozeMsg:
		w.busy = false
		switch {
		case msg.err != nil:
			w.status = "snooze failed: " + msg.err.Error()
		case msg.res.SnoozedUntil == "":
			w.status = fmt.Sprintf("thread %s is back in the inbox", msg.thread.ID)
		default:
			w.status = fmt.Sprintf("snoozed ^%s until %s", msg.thread.Peer, msg.res.SnoozedUntil)
		}
		return w, w.nextRefresh(false)
	case watchDeclineMsg:
		w.busy = false
		if msg.err != nil {
			w.status = "decline failed: " + msg.err.Error()
		} else {
			w.status = fmt.Sprintf("declined; ^%s will see: %s", msg.res.Peer, msg.res.Status)
		}
		return w, w.nextRefresh(false)
	case tea.KeyPressMsg:
		return w.handleKey(msg)
	default:
		if w.mode == watchSnoozeInput || w.mode == watchDeclineInput {
			var cmd tea.Cmd
			w.input, cmd = w.input.Update(msg)
			return w, cmd
		}
	}
	return w, nil
}

// handleKey routes keyboard input according to the current interaction mode.
func (w *watcher) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if w.busy {
		return w, nil
	}
	switch w.mode {
	case watchSnoozeInput, watchDeclineInput:
		switch key {
		case "esc", "ctrl+c":
			if w.mode == watchSnoozeInput {
				w.status = "snooze cancelled"
			} else {
				w.status = "decline cancelled"
			}
			w.endInput()
			return w, nil
		case "enter":
			return w.submitInput()
		default:
			var cmd tea.Cmd
			w.input, cmd = w.input.Update(msg)
			return w, cmd
		}
	case watchOpenConfirm:
		if key == "y" || key == "Y" {
			t, agent := w.pending, w.pendingAgent
			w.endInput()
			w.busy = true
			w.status = "opening..."
			return w, w.openCmd(t, agent)
		}
		w.status = notStartedMessage
		w.endInput()
		return w, nil
	case watchDeclineConfirm:
		if key == "y" || key == "Y" {
			t, reason := w.pending, w.pendingReason
			w.endInput()
			w.busy = true
			w.status = "declining..."
			return w, w.declineCmd(t, reason)
		}
		w.status = notDeclinedMessage
		w.endInput()
		return w, nil
	}

	switch key {
	case "q", "ctrl+c":
		return w, tea.Quit
	case "j", "down":
		w.selectIndex(w.sel + 1)
	case "k", "up":
		w.selectIndex(w.sel - 1)
	case "J", "pgdown", "space":
		w.viewport.PageDown()
	case "K", "pgup":
		w.viewport.PageUp()
	case "r":
		if !w.refreshing {
			return w, w.nextRefresh(true)
		}
	case "enter":
		w.beginOpen(false)
	case "a":
		w.beginOpen(true)
	case "s":
		w.beginSnooze()
	case "d":
		w.beginDecline()
	}
	return w, nil
}

// beginOpen stages an open confirmation for the selected message.
func (w *watcher) beginOpen(agent bool) {
	t, ok := w.selected()
	if !ok {
		return
	}
	if err := w.app.checkAgentProfile(w.cfg, agent); err != nil {
		w.status = err.Error()
		return
	}
	w.mode = watchOpenConfirm
	w.pending = t
	w.pendingAgent = agent
	w.prompt = openPrompt(t, agent)
}

// beginSnooze opens the duration input for the selected message.
func (w *watcher) beginSnooze() {
	t, ok := w.selected()
	if !ok {
		return
	}
	def := daemon.ShortDuration(w.cfg.Daemon.SnoozeDefault)
	w.beginInput(watchSnoozeInput, fmt.Sprintf("snooze ^%s for [%s, or off]: ", t.Peer, def), t)
}

// beginDecline opens the reason input for the selected message.
func (w *watcher) beginDecline() {
	t, ok := w.selected()
	if !ok {
		return
	}
	w.beginInput(watchDeclineInput, fmt.Sprintf("decline ^%s; reason for them (optional, Esc cancels): ", t.Peer), t)
}

// beginInput enters a text-input mode for a pending thread action.
func (w *watcher) beginInput(mode watchMode, prompt string, t threads.Thread) {
	w.mode = mode
	w.prompt = prompt
	w.pending = t
	w.input.Reset()
	w.input.Prompt = ""
	w.input.SetWidth(max(1, w.width-len([]rune(prompt))-1))
	_ = w.input.Focus()
}

// endInput clears pending action state and returns to browse mode.
func (w *watcher) endInput() {
	w.mode = watchBrowse
	w.prompt = ""
	w.pending = threads.Thread{}
	w.pendingAgent = false
	w.pendingReason = ""
	w.input.Blur()
	w.input.Reset()
}

// submitInput validates and starts the pending snooze or decline action.
func (w *watcher) submitInput() (tea.Model, tea.Cmd) {
	t := w.pending
	value := w.input.Value()
	if w.mode == watchSnoozeInput {
		d, err := parseSnooze(strings.TrimSpace(value), w.cfg.Daemon.SnoozeDefault)
		if err != nil {
			w.status = err.Error()
			w.endInput()
			return w, nil
		}
		w.endInput()
		w.busy = true
		w.status = "snoozing..."
		return w, w.snoozeCmd(t, d)
	}

	w.mode = watchDeclineConfirm
	w.pendingReason = value
	w.prompt = declinePrompt(t, value)
	w.input.Blur()
	return w, nil
}

// openCmd opens a session while releasing and restoring the terminal.
func (w *watcher) openCmd(t threads.Thread, agent bool) tea.Cmd {
	return func() tea.Msg {
		var msg watchOpenMsg
		if w.releaseTerminal != nil {
			if err := w.releaseTerminal(); err != nil {
				msg.err = fmt.Errorf("release terminal: %w", err)
				return msg
			}
		}
		msg.res, msg.err = w.openThread(w.ctx, t, agent)
		if w.restoreTerminal != nil {
			msg.restoreErr = w.restoreTerminal()
		}
		return msg
	}
}

// snoozeCmd runs a snooze action asynchronously.
func (w *watcher) snoozeCmd(t threads.Thread, d time.Duration) tea.Cmd {
	return func() tea.Msg {
		res, err := w.snoozeThread(w.ctx, t, d)
		return watchSnoozeMsg{thread: t, res: res, err: err}
	}
}

// declineCmd runs a decline action asynchronously.
func (w *watcher) declineCmd(t threads.Thread, reason string) tea.Cmd {
	return func() tea.Msg {
		res, err := w.declineThread(w.ctx, t, reason)
		return watchDeclineMsg{res: res, err: err}
	}
}

// refresh reloads the inbox, keeping the selection on the same thread.
func (w *watcher) refresh(ctx context.Context) error {
	items, err := w.loadStaged(ctx)
	if err != nil {
		return err
	}
	w.setItems(items)
	return nil
}

// nextRefresh starts an asynchronous reload and identifies stale responses.
func (w *watcher) nextRefresh(manual bool) tea.Cmd {
	w.refreshID++
	id := w.refreshID
	w.refreshing = true
	return func() tea.Msg {
		items, err := w.loadStaged(w.ctx)
		return watchRefreshMsg{id: id, manual: manual, items: items, err: err}
	}
}

// setItems replaces inbox items while preserving selection by thread ID.
func (w *watcher) setItems(items []threads.Thread) {
	oldID := ""
	if t, ok := w.selected(); ok {
		oldID = t.ID
	}
	w.items = items
	w.sel = 0
	for i, t := range items {
		if t.ID == oldID {
			w.sel = i
			break
		}
	}
	newID := ""
	if t, ok := w.selected(); ok {
		newID = t.ID
	}
	w.setViewportContent()
	if oldID != newID {
		w.viewport.GotoTop()
	}
}

// selected returns the selected thread when the index is valid.
func (w *watcher) selected() (threads.Thread, bool) {
	if w.sel < 0 || w.sel >= len(w.items) {
		return threads.Thread{}, false
	}
	return w.items[w.sel], true
}

// selectIndex moves selection to a valid, different item.
func (w *watcher) selectIndex(i int) {
	if i < 0 || i >= len(w.items) || i == w.sel {
		return
	}
	w.sel = i
	w.setViewportContent()
	w.viewport.GotoTop()
}

// setViewportContent displays the selected message or an empty viewport.
func (w *watcher) setViewportContent() {
	if t, ok := w.selected(); ok {
		w.viewport.SetContentLines(wrapLines(t.StagedKickoff, w.width))
	} else {
		w.viewport.SetContent("")
	}
}

// resize applies terminal dimensions and keeps active inputs usable.
func (w *watcher) resize() {
	if w.width < 1 {
		w.width = 1
	}
	if w.height < 1 {
		w.height = 1
	}
	w.viewport.SetWidth(w.width)
	w.viewport.SetHeight(w.paneHeight())
	w.setViewportContent()
	if w.mode == watchSnoozeInput || w.mode == watchDeclineInput {
		w.input.SetWidth(max(1, w.width-len([]rune(w.prompt))-1))
	}
}

// listHeight is how many list rows fit: a third of the screen, at least
// three, never more than there are items. The empty state occupies two rows.
func (w *watcher) listHeight() int {
	if len(w.items) == 0 {
		return 2
	}
	h := w.height / 3
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
	return max(1, w.height-w.listHeight()-4)
}

// View renders the inbox list, message details, and current interaction prompt.
func (w *watcher) View() tea.View {
	now := w.app.Now()
	waiting, snoozed := 0, 0
	for _, t := range w.items {
		if t.Snoozed(now) {
			snoozed++
		} else {
			waiting++
		}
	}

	lines := []string{clip(fmt.Sprintf("WORMHOLE INBOX  ^%s · %d waiting · %d snoozed · %s", w.cfg.Handle, waiting, snoozed, now.Format(clockLayout)), w.width)}
	if len(w.items) == 0 {
		lines = append(lines, "", "  inbox empty; new messages appear here as they arrive")
	} else {
		h := w.listHeight()
		first := max(0, w.sel-h+1)
		for i := first; i < first+h && i < len(w.items); i++ {
			t := w.items[i]
			kind := "ask"
			if t.Direction == threads.DirectionOut {
				kind = "reply"
			}
			when := ago(t.LastMessageAt, now)
			if t.Snoozed(now) {
				when = "snoozed until " + t.SnoozedUntil.Local().Format(clockLayout)
			}
			id := t.ID
			if w.width < 120 {
				if r := []rune(id); len(r) > 10 {
					id = string(r[:10])
				}
			}
			row := fmt.Sprintf("  %s  %-12s %-6s %-22s %s  %s", id, "^"+t.Peer, kind, when, t.Repo, t.LastPreview)
			if i == w.sel {
				row = "▸" + row[len("  "):]
			}
			lines = append(lines, clip(row, w.width))
		}
	}
	lines = append(lines, strings.Repeat("─", w.width))
	if w.viewport.TotalLineCount() > 0 {
		lines = append(lines, strings.Split(w.viewport.View(), "\n")...)
	} else {
		lines = append(lines, strings.Repeat("\n", w.paneHeight()-1))
	}

	statusY := len(lines)
	switch w.mode {
	case watchSnoozeInput, watchDeclineInput:
		lines = append(lines, clip(w.prompt+w.input.View(), w.width))
	case watchOpenConfirm, watchDeclineConfirm:
		lines = append(lines, clip(w.prompt+" [y/N] ", w.width))
	default:
		lines = append(lines, clip(w.status, w.width))
	}
	lines = append(lines, clip("enter open with my tools  a hand to agent  s snooze  d decline  j/k move  J/K scroll  q quit", w.width))

	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	if w.mode == watchSnoozeInput || w.mode == watchDeclineInput {
		if cursor := w.input.Cursor(); cursor != nil {
			cursor.Position.X += len([]rune(w.prompt))
			cursor.Position.Y += statusY
			v.Cursor = cursor
		}
	}
	return v
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
