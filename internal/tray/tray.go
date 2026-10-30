// Package tray shows a system-tray indicator for the inbox over D-Bus using
// the StatusNotifierItem protocol (KDE Plasma, GNOME's AppIndicator
// extension, Waybar and others). The icon reflects whether messages wait,
// the tooltip and title carry the count, and the menu lists the waiting
// messages with entries to open the inbox and to pause or resume the
// daemon.
//
// The package speaks the protocol itself with godbus; there is no cgo and
// no tray library. Message bodies never reach it: the caller passes a
// shortened preview, which is what the menu shows.
package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// Item is one message waiting in the inbox, as the menu shows it.
type Item struct {
	ID      string // thread id
	Peer    string // handle without the ^
	Preview string // one line, already shortened
	Age     string // e.g. "3m ago"
}

// State is what the indicator reflects.
type State struct {
	Items  []Item // waiting messages, newest first (snoozed ones are not included by the caller)
	Paused bool   // the daemon's local kill switch is on
}

// Actions are what the menu and the icon trigger. They are called from the
// D-Bus dispatch goroutine and must return quickly; callers do slow work on
// their own goroutines. Any may be nil.
type Actions struct {
	Show      func()            // left click on the icon, the "Open inbox" entry
	ShowItem  func(id string)   // click on one message entry; nil falls back to Show
	SetPaused func(paused bool) // "Pause daemon" / "Resume daemon" entry
}

// Protocol names.
const (
	itemPath   = dbus.ObjectPath("/StatusNotifierItem")
	itemIface  = "org.kde.StatusNotifierItem"
	menuPath   = dbus.ObjectPath("/MenuBar")
	menuIface  = "com.canonical.dbusmenu"
	watcherDst = "org.kde.StatusNotifierWatcher"
	watcherObj = dbus.ObjectPath("/StatusNotifierWatcher")
	busIface   = "org.freedesktop.DBus"
)

// retryInterval is how often registration is retried while no watcher has
// accepted the item, on top of registering the moment the watcher name
// appears on the bus. Tests shorten it.
var retryInterval = time.Minute

// callTimeout bounds every call to the watcher so a stuck tray host cannot
// stall the daemon.
const callTimeout = 5 * time.Second

// itemCount numbers the items this process registers; the protocol wants
// org.kde.StatusNotifierItem-<pid>-<n>.
var itemCount atomic.Int32

// Indicator is one registered tray item.
type Indicator struct {
	conn  *dbus.Conn
	own   bool // conn was opened by Connect and is closed by Close
	log   *slog.Logger
	name  string // the unique bus name this item owns
	props *prop.Properties
	menu  *menu
	act   Actions

	cancel  context.CancelFunc
	signals chan *dbus.Signal
	done    chan struct{}

	mu    sync.Mutex
	state State
}

// Connect connects to the session bus and calls ConnectBus.
func Connect(ctx context.Context, log *slog.Logger, a Actions) (*Indicator, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	i, err := ConnectBus(ctx, conn, log, a)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	i.own = true
	return i, nil
}

// ConnectBus exports the item and its menu on conn, registers with
// org.kde.StatusNotifierWatcher and returns. When the watcher is not on the
// bus yet (no tray host running), it logs once at info level and keeps
// trying: it registers as soon as the watcher name appears (watch
// NameOwnerChanged on org.freedesktop.DBus for org.kde.StatusNotifierWatcher,
// and also re-register if the watcher restarts). ctx cancellation stops the
// retry loop. The initial state is empty (no items, not paused).
func ConnectBus(ctx context.Context, conn *dbus.Conn, log *slog.Logger, a Actions) (*Indicator, error) {
	if conn == nil {
		return nil, errors.New("tray: nil connection")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	i := &Indicator{conn: conn, log: log, act: a}
	i.menu = newMenu(conn, log, a)

	if err := i.export(); err != nil {
		i.unexport()
		return nil, err
	}

	name := fmt.Sprintf("%s-%d-%d", itemIface, os.Getpid(), itemCount.Add(1))
	reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil {
		i.unexport()
		return nil, fmt.Errorf("request bus name: %w", err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		i.unexport()
		return nil, fmt.Errorf("bus name %s is taken", name)
	}
	i.name = name

	// The match is added before the first attempt so a watcher that appears
	// between the attempt and the loop below is not missed.
	if err := conn.AddMatchSignal(i.watcherMatch()...); err != nil {
		_, _ = conn.ReleaseName(name)
		i.unexport()
		return nil, fmt.Errorf("watch for tray host: %w", err)
	}
	i.signals = make(chan *dbus.Signal, 16)
	conn.Signal(i.signals)

	registered := i.register(ctx) == nil
	if !registered {
		log.Info("no system tray host on the bus yet; the indicator appears when one starts")
	}

	rctx, cancel := context.WithCancel(ctx)
	i.cancel = cancel
	i.done = make(chan struct{})
	go i.retryLoop(rctx, registered)
	return i, nil
}

// Update replaces the state and refreshes title, icon, tooltip and menu,
// emitting the protocol's change signals.
func (i *Indicator) Update(s State) {
	i.mu.Lock()
	i.state = s
	i.mu.Unlock()

	i.props.SetMust(itemIface, "Title", title(s))
	i.props.SetMust(itemIface, "IconName", iconName(s))
	i.props.SetMust(itemIface, "ToolTip", toolTip(s))
	i.props.SetMust(itemIface, "Status", "Active")
	for _, sig := range []string{"NewTitle", "NewIcon", "NewToolTip", "NewStatus"} {
		if err := i.conn.Emit(itemPath, itemIface+"."+sig); err != nil {
			i.log.Debug("tray signal failed", "signal", sig, "err", err)
		}
	}
	i.menu.update(s)
}

// Close releases the bus name and connection (the one Connect opened;
// ConnectBus does not close a conn it was given).
func (i *Indicator) Close() error {
	i.cancel()
	<-i.done
	i.conn.RemoveSignal(i.signals)
	var errs []error
	if err := i.conn.RemoveMatchSignal(i.watcherMatch()...); err != nil && !errors.Is(err, dbus.ErrClosed) {
		errs = append(errs, err)
	}
	if _, err := i.conn.ReleaseName(i.name); err != nil && !errors.Is(err, dbus.ErrClosed) {
		errs = append(errs, fmt.Errorf("release bus name: %w", err))
	}
	i.unexport()
	if i.own {
		if err := i.conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// export puts the item, the menu, their properties and introspection data
// on the bus.
func (i *Indicator) export() error {
	c := i.conn
	s := i.snapshot()
	itemProps := prop.Map{itemIface: {
		"Category":            ro("Communications"),
		"Id":                  ro("wormhole"),
		"Title":               ro(title(s)),
		"Status":              ro("Active"),
		"WindowId":            ro(int32(0)),
		"IconName":            ro(iconName(s)),
		"IconPixmap":          ro([]pixmap{}),
		"IconThemePath":       ro(""),
		"OverlayIconName":     ro(""),
		"OverlayIconPixmap":   ro([]pixmap{}),
		"AttentionIconName":   ro(""),
		"AttentionIconPixmap": ro([]pixmap{}),
		"AttentionMovieName":  ro(""),
		"ToolTip":             ro(toolTip(s)),
		"ItemIsMenu":          ro(false),
		"Menu":                ro(menuPath),
	}}
	menuProps := prop.Map{menuIface: {
		"Version":       ro(uint32(3)),
		"TextDirection": ro("ltr"),
		"Status":        ro("normal"),
		"IconThemePath": ro([]string{}),
	}}

	steps := []func() error{
		func() error { return c.Export(sniObject{i}, itemPath, itemIface) },
		func() error {
			p, err := prop.Export(c, itemPath, itemProps)
			i.props = p
			return err
		},
		func() error { return c.Export(introspect.NewIntrospectable(itemNode()), itemPath, introspectIface) },
		func() error { return c.Export(i.menu, menuPath, menuIface) },
		func() error {
			p, err := prop.Export(c, menuPath, menuProps)
			i.menu.props = p
			return err
		},
		func() error { return c.Export(introspect.NewIntrospectable(menuNode()), menuPath, introspectIface) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return fmt.Errorf("export tray objects: %w", err)
		}
	}
	return nil
}

// unexport takes the objects off the bus again. Errors are ignored: the
// paths are constant and valid, so the only failure is a closed connection.
func (i *Indicator) unexport() {
	for _, iface := range []string{itemIface, propIface, introspectIface} {
		_ = i.conn.Export(nil, itemPath, iface)
	}
	for _, iface := range []string{menuIface, propIface, introspectIface} {
		_ = i.conn.Export(nil, menuPath, iface)
	}
}

// ro is a read-only property that emits no PropertiesChanged: the protocol
// has its own New* signals for that.
func ro(v interface{}) *prop.Prop {
	return &prop.Prop{Value: v, Emit: prop.EmitFalse}
}

// snapshot returns the current state.
func (i *Indicator) snapshot() State {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.state
}

// watcherMatch selects NameOwnerChanged for the watcher name.
func (i *Indicator) watcherMatch() []dbus.MatchOption {
	return []dbus.MatchOption{
		dbus.WithMatchSender(busIface),
		dbus.WithMatchInterface(busIface),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, watcherDst),
	}
}

// register asks the watcher to show this item.
func (i *Indicator) register(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	call := i.conn.Object(watcherDst, watcherObj).CallWithContext(cctx, watcherDst+".RegisterStatusNotifierItem", 0, i.name)
	return call.Err
}

// retryLoop registers again whenever the watcher name gains an owner (a
// tray host started or restarted) and, while unregistered, on a slow
// timer as well. It ends when ctx is cancelled or the connection closes.
func (i *Indicator) retryLoop(ctx context.Context, registered bool) {
	defer close(i.done)
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	try := func(why string) {
		if err := i.register(ctx); err != nil {
			if ctx.Err() == nil {
				i.log.Debug("tray registration failed", "why", why, "err", err)
			}
			registered = false
			return
		}
		i.log.Debug("registered with the system tray", "why", why)
		registered = true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-i.signals:
			if !ok {
				return
			}
			if sig.Name != busIface+".NameOwnerChanged" || len(sig.Body) != 3 {
				continue
			}
			name, _ := sig.Body[0].(string)
			newOwner, _ := sig.Body[2].(string)
			if name != watcherDst {
				continue
			}
			if newOwner == "" {
				registered = false
				continue
			}
			try("tray host appeared")
		case <-ticker.C:
			if !registered {
				try("retry")
			}
		}
	}
}

// title is the item's Title property.
func title(s State) string {
	t := "Wormhole"
	if n := len(s.Items); n > 0 {
		t += fmt.Sprintf(" · %d waiting", n)
	}
	if s.Paused {
		t += " · paused"
	}
	return t
}

// iconName follows the inbox: unread while something waits.
func iconName(s State) string {
	if len(s.Items) > 0 {
		return "mail-unread"
	}
	return "mail-read"
}

// pixmap is one ARGB32 image of the protocol's icon arrays. The indicator
// uses theme icons only, so the arrays stay empty.
type pixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

// tip is the ToolTip property: (sa(iiay)ss).
type tip struct {
	IconName string
	Pixmaps  []pixmap
	Title    string
	Text     string
}

// toolTip builds the ToolTip property.
func toolTip(s State) tip {
	return tip{IconName: iconName(s), Pixmaps: []pixmap{}, Title: title(s), Text: tipText(s)}
}

// tipText is the tooltip's body line.
func tipText(s State) string {
	text := "inbox empty"
	if n := len(s.Items); n > 0 {
		text = fmt.Sprintf("%d waiting in your inbox", n)
	}
	if s.Paused {
		text += " · daemon paused"
	}
	return text
}

// sniObject carries the org.kde.StatusNotifierItem methods.
type sniObject struct {
	i *Indicator
}

// Activate is the left click: show the inbox.
func (o sniObject) Activate(x, y int32) *dbus.Error {
	o.i.show()
	return nil
}

// SecondaryActivate is the middle click: show the inbox too.
func (o sniObject) SecondaryActivate(x, y int32) *dbus.Error {
	o.i.show()
	return nil
}

// ContextMenu is the right click; the host shows the menu itself.
func (o sniObject) ContextMenu(x, y int32) *dbus.Error {
	return nil
}

// Scroll is the wheel over the icon; nothing to do.
func (o sniObject) Scroll(delta int32, orientation string) *dbus.Error {
	return nil
}

// show runs Actions.Show when set.
func (i *Indicator) show() {
	if i.act.Show != nil {
		i.act.Show()
	}
}
