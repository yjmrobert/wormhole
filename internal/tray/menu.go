package tray

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

// Menu entry ids. Fixed entries keep theirs; message entries count from
// firstItemID and may be renumbered by an update, which LayoutUpdated
// announces.
const (
	rootID      int32 = 0
	headerID    int32 = 1
	openID      int32 = 2
	pauseID     int32 = 3
	separatorID int32 = 4
	firstItemID int32 = 100
)

// entry is one line of the menu.
type entry struct {
	id        int32
	label     string
	enabled   bool
	separator bool
	action    func()
}

// layout is the dbusmenu node (ia{sv}av); children wrap the same type.
type layout struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

// itemProps is one element of GetGroupProperties' reply (ia{sv}).
type itemProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

// eventSpec is one element of EventGroup's argument (isvu).
type eventSpec struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

// errUnknownID is the reply for an id the menu does not have.
var errUnknownID = dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []interface{}{"unknown menu id"})

// menu implements com.canonical.dbusmenu for the item.
type menu struct {
	conn  *dbus.Conn
	log   *slog.Logger
	act   Actions
	props *prop.Properties

	mu       sync.Mutex
	revision uint32
	entries  []entry
}

// newMenu builds the menu for the empty state.
func newMenu(conn *dbus.Conn, log *slog.Logger, a Actions) *menu {
	m := &menu{conn: conn, log: log, act: a}
	m.entries = m.build(State{})
	return m
}

// update rebuilds the entries for s and announces the new layout.
func (m *menu) update(s State) {
	m.mu.Lock()
	m.entries = m.build(s)
	m.revision++
	rev := m.revision
	m.mu.Unlock()
	if err := m.conn.Emit(menuPath, menuIface+".LayoutUpdated", rev, rootID); err != nil {
		m.log.Debug("menu signal failed", "err", err)
	}
}

// build lists the entries for s, in display order.
func (m *menu) build(s State) []entry {
	out := []entry{{id: headerID, label: headerLabel(s)}}
	for n, it := range s.Items {
		id := it.ID
		out = append(out, entry{
			id:      firstItemID + int32(n),
			label:   itemLabel(it),
			enabled: true,
			action:  func() { m.showItem(id) },
		})
	}
	out = append(out,
		entry{id: separatorID, separator: true},
		entry{id: openID, label: "Open inbox", enabled: true, action: m.show},
	)
	pause := entry{id: pauseID, label: "Pause daemon", enabled: true, action: func() { m.setPaused(true) }}
	if s.Paused {
		pause.label = "Resume daemon"
		pause.action = func() { m.setPaused(false) }
	}
	return append(out, pause)
}

// show runs Actions.Show when set.
func (m *menu) show() {
	if m.act.Show != nil {
		m.act.Show()
	}
}

// showItem runs Actions.ShowItem, falling back to Show.
func (m *menu) showItem(id string) {
	if m.act.ShowItem != nil {
		m.act.ShowItem(id)
		return
	}
	m.show()
}

// setPaused runs Actions.SetPaused when set.
func (m *menu) setPaused(paused bool) {
	if m.act.SetPaused != nil {
		m.act.SetPaused(paused)
	}
}

// headerLabel is the disabled first line.
func headerLabel(s State) string {
	if n := len(s.Items); n > 0 {
		return fmt.Sprintf("Inbox · %d waiting", n)
	}
	return "Inbox empty"
}

// itemLabel is the menu line for one message: "^alice: preview  (3m ago)".
func itemLabel(it Item) string {
	label := "^" + it.Peer + ": " + it.Preview
	if it.Age != "" {
		label += "  (" + it.Age + ")"
	}
	return escapeLabel(label)
}

// escapeLabel doubles underscores, which dbusmenu otherwise reads as
// mnemonic markers.
func escapeLabel(s string) string {
	return strings.ReplaceAll(s, "_", "__")
}

// find returns the entry with id.
func (m *menu) find(id int32) (entry, bool) {
	for _, e := range m.entries {
		if e.id == id {
			return e, true
		}
	}
	return entry{}, false
}

// properties returns an entry's dbusmenu properties, limited to names when
// names is non-empty.
func (e entry) properties(names []string) map[string]dbus.Variant {
	all := map[string]dbus.Variant{}
	if e.separator {
		all["type"] = dbus.MakeVariant("separator")
	} else {
		all["label"] = dbus.MakeVariant(e.label)
	}
	all["enabled"] = dbus.MakeVariant(e.enabled)
	all["visible"] = dbus.MakeVariant(true)
	return filterProps(all, names)
}

// rootProperties are the root node's properties, limited to names when
// names is non-empty.
func rootProperties(names []string) map[string]dbus.Variant {
	return filterProps(map[string]dbus.Variant{
		"children-display": dbus.MakeVariant("submenu"),
	}, names)
}

// filterProps keeps the properties in names; an empty names keeps all.
func filterProps(all map[string]dbus.Variant, names []string) map[string]dbus.Variant {
	if len(names) == 0 {
		return all
	}
	out := map[string]dbus.Variant{}
	for _, n := range names {
		if v, ok := all[n]; ok {
			out[n] = v
		}
	}
	return out
}

// GetLayout returns the subtree at parentID down to recursionDepth levels
// (-1 for all).
func (m *menu) GetLayout(parentID int32, recursionDepth int32, propertyNames []string) (uint32, layout, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if parentID == rootID {
		root := layout{ID: rootID, Props: rootProperties(propertyNames), Children: []dbus.Variant{}}
		if recursionDepth != 0 {
			for _, e := range m.entries {
				root.Children = append(root.Children, dbus.MakeVariant(layout{
					ID:       e.id,
					Props:    e.properties(propertyNames),
					Children: []dbus.Variant{},
				}))
			}
		}
		return m.revision, root, nil
	}
	e, ok := m.find(parentID)
	if !ok {
		return 0, layout{}, errUnknownID
	}
	return m.revision, layout{ID: e.id, Props: e.properties(propertyNames), Children: []dbus.Variant{}}, nil
}

// GetGroupProperties returns the properties of the listed entries; an empty
// list means all of them.
func (m *menu) GetGroupProperties(ids []int32, propertyNames []string) ([]itemProps, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []itemProps{}
	if len(ids) == 0 {
		out = append(out, itemProps{ID: rootID, Props: rootProperties(propertyNames)})
		for _, e := range m.entries {
			out = append(out, itemProps{ID: e.id, Props: e.properties(propertyNames)})
		}
		return out, nil
	}
	for _, id := range ids {
		if id == rootID {
			out = append(out, itemProps{ID: rootID, Props: rootProperties(propertyNames)})
			continue
		}
		if e, ok := m.find(id); ok {
			out = append(out, itemProps{ID: e.id, Props: e.properties(propertyNames)})
		}
	}
	return out, nil
}

// GetProperty returns one property of one entry.
func (m *menu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var props map[string]dbus.Variant
	if id == rootID {
		props = rootProperties(nil)
	} else {
		e, ok := m.find(id)
		if !ok {
			return dbus.Variant{}, errUnknownID
		}
		props = e.properties(nil)
	}
	v, ok := props[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []interface{}{"unknown property"})
	}
	return v, nil
}

// Event delivers one event; "clicked" runs the entry's action.
func (m *menu) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if !m.event(id, eventID) {
		return errUnknownID
	}
	return nil
}

// EventGroup delivers several events and returns the ids it did not find.
func (m *menu) EventGroup(events []eventSpec) ([]int32, *dbus.Error) {
	missing := []int32{}
	for _, ev := range events {
		if !m.event(ev.ID, ev.EventID) {
			missing = append(missing, ev.ID)
		}
	}
	return missing, nil
}

// event runs the action for a click on id and reports whether id exists.
func (m *menu) event(id int32, eventID string) bool {
	if id == rootID {
		return true
	}
	m.mu.Lock()
	e, ok := m.find(id)
	m.mu.Unlock()
	if !ok {
		return false
	}
	if eventID == "clicked" && e.enabled && e.action != nil {
		e.action()
	}
	return true
}

// AboutToShow reports that the layout is already current.
func (m *menu) AboutToShow(id int32) (bool, *dbus.Error) {
	return false, nil
}

// AboutToShowGroup reports that every layout is already current.
func (m *menu) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}
