package tray

import (
	"bufio"
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const wait = 5 * time.Second

// startBus runs a private session bus for one test and returns its address.
func startBus(t *testing.T) string {
	t.Helper()
	exe, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	cmd := exec.Command(exe, "--session", "--print-address=1", "--nofork", "--nopidfile")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("reading bus address: %v", err)
	}
	return strings.TrimSpace(line)
}

// connect opens one connection to the bus at addr.
func connect(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	conn, err := dbus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Auth(nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Hello(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// fakeWatcher plays org.kde.StatusNotifierWatcher.
type fakeWatcher struct {
	got chan string
}

func (w *fakeWatcher) RegisterStatusNotifierItem(service string) *dbus.Error {
	w.got <- service
	return nil
}

func (w *fakeWatcher) RegisterStatusNotifierHost(service string) *dbus.Error {
	return nil
}

// startWatcher exports the fake watcher on conn and takes the watcher name.
func startWatcher(t *testing.T, conn *dbus.Conn) *fakeWatcher {
	t.Helper()
	w := &fakeWatcher{got: make(chan string, 8)}
	if err := conn.Export(w, watcherObj, watcherDst); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.RequestName(watcherDst, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("request watcher name: %v (%d)", err, reply)
	}
	return w
}

// expectRegistration waits for the watcher to hear from service.
func expectRegistration(t *testing.T, w *fakeWatcher, service string) {
	t.Helper()
	select {
	case got := <-w.got:
		if got != service {
			t.Fatalf("registered %q, want %q", got, service)
		}
	case <-time.After(wait):
		t.Fatal("watcher never heard RegisterStatusNotifierItem")
	}
}

// getProp reads one property of the indicator over the bus.
func getProp(t *testing.T, conn *dbus.Conn, dest string, path dbus.ObjectPath, iface, name string) interface{} {
	t.Helper()
	v, err := conn.Object(dest, path).GetProperty(iface + "." + name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return v.Value()
}

// node is a decoded dbusmenu layout node.
type node struct {
	id    int32
	props map[string]dbus.Variant
	kids  []node
}

// decodeLayout turns the wire form of (ia{sv}av) into a node tree.
func decodeLayout(t *testing.T, v interface{}) node {
	t.Helper()
	var l layout
	if err := dbus.Store([]interface{}{v}, &l); err != nil {
		t.Fatalf("decode layout: %v", err)
	}
	n := node{id: l.ID, props: l.Props}
	for _, c := range l.Children {
		n.kids = append(n.kids, decodeLayout(t, c.Value()))
	}
	return n
}

// getLayout calls GetLayout on the menu and decodes the reply.
func getLayout(t *testing.T, conn *dbus.Conn, dest string) node {
	t.Helper()
	var rev uint32
	var raw interface{}
	err := conn.Object(dest, menuPath).Call(menuIface+".GetLayout", 0, int32(0), int32(-1), []string{}).Store(&rev, &raw)
	if err != nil {
		t.Fatalf("GetLayout: %v", err)
	}
	return decodeLayout(t, raw)
}

func (n node) label() string {
	v, _ := n.props["label"].Value().(string)
	return v
}

func (n node) enabled() bool {
	v, _ := n.props["enabled"].Value().(bool)
	return v
}

func (n node) isSeparator() bool {
	v, _ := n.props["type"].Value().(string)
	return v == "separator"
}

// click sends a clicked event to entry id.
func click(t *testing.T, conn *dbus.Conn, dest string, id int32) {
	t.Helper()
	err := conn.Object(dest, menuPath).Call(menuIface+".Event", 0, id, "clicked", dbus.MakeVariant(int32(0)), uint32(0)).Err
	if err != nil {
		t.Fatalf("Event(%d): %v", id, err)
	}
}

// recv waits for one value on ch.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatalf("no %s within %v", what, wait)
		panic("unreachable")
	}
}

func TestIndicator(t *testing.T) {
	addr := startBus(t)
	hostConn := connect(t, addr)
	w := startWatcher(t, hostConn)
	itemConn := connect(t, addr)

	shown := make(chan struct{}, 8)
	shownItem := make(chan string, 8)
	paused := make(chan bool, 8)
	acts := Actions{
		Show:      func() { shown <- struct{}{} },
		ShowItem:  func(id string) { shownItem <- id },
		SetPaused: func(p bool) { paused <- p },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ind, err := ConnectBus(ctx, itemConn, slog.New(slog.DiscardHandler), acts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ind.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// 1. The watcher hears about the item under its bus name.
	expectRegistration(t, w, ind.name)
	if !strings.HasPrefix(ind.name, "org.kde.StatusNotifierItem-") {
		t.Fatalf("bus name %q", ind.name)
	}

	// 2. The empty state over the bus.
	if got := getProp(t, hostConn, ind.name, itemPath, itemIface, "Title"); got != "Wormhole" {
		t.Errorf("Title = %q", got)
	}
	if got := getProp(t, hostConn, ind.name, itemPath, itemIface, "IconName"); got != "mail-read" {
		t.Errorf("IconName = %q", got)
	}
	if got := getProp(t, hostConn, ind.name, itemPath, itemIface, "Menu"); got != menuPath {
		t.Errorf("Menu = %v", got)
	}
	if got := getProp(t, hostConn, ind.name, itemPath, itemIface, "Status"); got != "Active" {
		t.Errorf("Status = %q", got)
	}
	if got := getProp(t, hostConn, ind.name, menuPath, menuIface, "Version"); got != uint32(3) {
		t.Errorf("menu Version = %v", got)
	}
	var tt tip
	if err := dbus.Store([]interface{}{getProp(t, hostConn, ind.name, itemPath, itemIface, "ToolTip")}, &tt); err != nil {
		t.Fatalf("ToolTip: %v", err)
	}
	if tt.IconName != "mail-read" || tt.Title != "Wormhole" || tt.Text != "inbox empty" {
		t.Errorf("ToolTip = %+v", tt)
	}
	for _, path := range []dbus.ObjectPath{itemPath, menuPath} {
		var xml string
		if err := hostConn.Object(ind.name, path).Call(introspectIface+".Introspect", 0).Store(&xml); err != nil {
			t.Errorf("Introspect %s: %v", path, err)
		} else if !strings.Contains(xml, "<interface name=\"org.freedesktop.DBus.Properties\">") {
			t.Errorf("Introspect %s lacks the properties interface", path)
		}
	}

	// 5. Signals on Update, subscribed before the update happens.
	for _, m := range []dbus.MatchOption{dbus.WithMatchMember("NewIcon"), dbus.WithMatchMember("LayoutUpdated")} {
		if err := hostConn.AddMatchSignal(dbus.WithMatchSender(ind.name), m); err != nil {
			t.Fatal(err)
		}
	}
	sigs := make(chan *dbus.Signal, 32)
	hostConn.Signal(sigs)

	// 3. Two waiting messages.
	items := []Item{
		{ID: "t1", Peer: "alice", Preview: "is the retry policy sane?", Age: "3m ago"},
		{ID: "t2", Peer: "bob", Preview: "review my_branch please", Age: "1h ago"},
	}
	ind.Update(State{Items: items})

	if got := getProp(t, hostConn, ind.name, itemPath, itemIface, "Title"); got != "Wormhole · 2 waiting" {
		t.Errorf("Title = %q", got)
	}
	if got := getProp(t, hostConn, ind.name, itemPath, itemIface, "IconName"); got != "mail-unread" {
		t.Errorf("IconName = %q", got)
	}
	root := getLayout(t, hostConn, ind.name)
	if root.id != 0 || root.props["children-display"].Value() != "submenu" {
		t.Errorf("root = %+v", root)
	}
	if len(root.kids) != 6 {
		t.Fatalf("root has %d children, want 6", len(root.kids))
	}
	k := root.kids
	if k[0].label() != "Inbox · 2 waiting" || k[0].enabled() {
		t.Errorf("header = %q enabled=%v", k[0].label(), k[0].enabled())
	}
	if k[1].label() != "^alice: is the retry policy sane?  (3m ago)" || !k[1].enabled() {
		t.Errorf("entry 1 = %q", k[1].label())
	}
	if k[2].label() != "^bob: review my__branch please  (1h ago)" || !k[2].enabled() {
		t.Errorf("entry 2 = %q", k[2].label())
	}
	if !k[3].isSeparator() {
		t.Errorf("entry 3 = %+v, want separator", k[3].props)
	}
	if k[4].label() != "Open inbox" || !k[4].enabled() {
		t.Errorf("entry 4 = %q", k[4].label())
	}
	if k[5].label() != "Pause daemon" || !k[5].enabled() {
		t.Errorf("entry 5 = %q", k[5].label())
	}

	// 5. Both signals arrived for that update.
	seen := map[string]bool{}
	for len(seen) < 2 {
		s := recv(t, sigs, "signal")
		switch s.Name {
		case itemIface + ".NewIcon":
			if s.Path != itemPath {
				t.Errorf("NewIcon on %s", s.Path)
			}
			seen["icon"] = true
		case menuIface + ".LayoutUpdated":
			if s.Path != menuPath || len(s.Body) != 2 || s.Body[1] != int32(0) {
				t.Errorf("LayoutUpdated = %+v", s)
			}
			seen["layout"] = true
		}
	}

	// 4. Clicks reach the actions.
	click(t, hostConn, ind.name, k[4].id)
	recv(t, shown, "Show")
	click(t, hostConn, ind.name, k[2].id)
	if got := recv(t, shownItem, "ShowItem"); got != "t2" {
		t.Errorf("ShowItem(%q)", got)
	}
	click(t, hostConn, ind.name, k[5].id)
	if got := recv(t, paused, "SetPaused"); !got {
		t.Error("SetPaused(false), want true")
	}
	click(t, hostConn, ind.name, k[0].id) // disabled header: nothing happens
	if err := hostConn.Object(ind.name, menuPath).Call(menuIface+".Event", 0, int32(9999), "clicked", dbus.MakeVariant(""), uint32(0)).Err; err == nil {
		t.Error("Event on an unknown id succeeded")
	}
	var missing []int32
	err = hostConn.Object(ind.name, menuPath).Call(menuIface+".EventGroup", 0, []eventSpec{
		{ID: k[4].id, EventID: "clicked", Data: dbus.MakeVariant("")},
		{ID: 9999, EventID: "clicked", Data: dbus.MakeVariant("")},
	}).Store(&missing)
	if err != nil {
		t.Fatalf("EventGroup: %v", err)
	}
	recv(t, shown, "Show from EventGroup")
	if len(missing) != 1 || missing[0] != 9999 {
		t.Errorf("EventGroup missing = %v", missing)
	}

	// Activate on the icon shows the inbox too.
	if err := hostConn.Object(ind.name, itemPath).Call(itemIface+".Activate", 0, int32(1), int32(2)).Err; err != nil {
		t.Fatalf("Activate: %v", err)
	}
	recv(t, shown, "Show from Activate")

	// Paused: the entry flips and the title says so.
	ind.Update(State{Paused: true})
	root = getLayout(t, hostConn, ind.name)
	if last := root.kids[len(root.kids)-1]; last.label() != "Resume daemon" {
		t.Errorf("pause entry = %q", last.label())
	}
	if got := getProp(t, hostConn, ind.name, itemPath, itemIface, "Title"); got != "Wormhole · paused" {
		t.Errorf("Title = %q", got)
	}
	if root.kids[0].label() != "Inbox empty" {
		t.Errorf("header = %q", root.kids[0].label())
	}
	click(t, hostConn, ind.name, root.kids[len(root.kids)-1].id)
	if got := recv(t, paused, "SetPaused"); got {
		t.Error("SetPaused(true), want false")
	}

	// GetGroupProperties and GetProperty agree with the layout.
	var group []itemProps
	if err := hostConn.Object(ind.name, menuPath).Call(menuIface+".GetGroupProperties", 0, []int32{openID, 9999}, []string{"label"}).Store(&group); err != nil {
		t.Fatalf("GetGroupProperties: %v", err)
	}
	if len(group) != 1 || group[0].ID != openID || group[0].Props["label"].Value() != "Open inbox" || len(group[0].Props) != 1 {
		t.Errorf("GetGroupProperties = %+v", group)
	}
	var one dbus.Variant
	if err := hostConn.Object(ind.name, menuPath).Call(menuIface+".GetProperty", 0, openID, "enabled").Store(&one); err != nil {
		t.Fatalf("GetProperty: %v", err)
	}
	if one.Value() != true {
		t.Errorf("GetProperty(enabled) = %v", one.Value())
	}
	var need bool
	if err := hostConn.Object(ind.name, menuPath).Call(menuIface+".AboutToShow", 0, int32(0)).Store(&need); err != nil || need {
		t.Errorf("AboutToShow = %v, %v", need, err)
	}
}

func TestRegistersWhenWatcherAppears(t *testing.T) {
	addr := startBus(t)
	itemConn := connect(t, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	ind, err := ConnectBus(ctx, itemConn, log, Actions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ind.Close() }()
	if !strings.Contains(logs.String(), "level=INFO") || !strings.Contains(logs.String(), "no system tray host") {
		t.Errorf("missing info line, got %q", logs.String())
	}

	// The watcher shows up later; the indicator must notice.
	hostConn := connect(t, addr)
	w := startWatcher(t, hostConn)
	expectRegistration(t, w, ind.name)

	// The watcher restarts: a new owner means registering again.
	if _, err := hostConn.ReleaseName(watcherDst); err != nil {
		t.Fatal(err)
	}
	hostConn2 := connect(t, addr)
	w2 := startWatcher(t, hostConn2)
	expectRegistration(t, w2, ind.name)
	select {
	case s := <-w.got:
		t.Errorf("old watcher heard %q after releasing the name", s)
	default:
	}
}

func TestCloseReleasesName(t *testing.T) {
	addr := startBus(t)
	hostConn := connect(t, addr)
	startWatcher(t, hostConn)
	itemConn := connect(t, addr)
	ind, err := ConnectBus(context.Background(), itemConn, nil, Actions{})
	if err != nil {
		t.Fatal(err)
	}
	name := ind.name
	if err := ind.Close(); err != nil {
		t.Fatal(err)
	}
	var has bool
	if err := hostConn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, name).Store(&has); err != nil {
		t.Fatal(err)
	}
	if has {
		t.Errorf("%s still owned after Close", name)
	}
	if itemConn.Connected() != true {
		t.Error("ConnectBus closed a connection it was given")
	}
}

func TestLabels(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"my_branch", "my__branch"},
		{"a__b", "a____b"},
		{"_", "__"},
		{"", ""},
	}
	for _, c := range cases {
		if got := escapeLabel(c.in); got != c.want {
			t.Errorf("escapeLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := itemLabel(Item{Peer: "alice", Preview: "fix_it", Age: "3m ago"}); got != "^alice: fix__it  (3m ago)" {
		t.Errorf("itemLabel = %q", got)
	}
	if got := itemLabel(Item{Peer: "bob", Preview: "hello"}); got != "^bob: hello" {
		t.Errorf("itemLabel without age = %q", got)
	}
	two := State{Items: []Item{{ID: "a"}, {ID: "b"}}}
	if got := headerLabel(State{}); got != "Inbox empty" {
		t.Errorf("headerLabel(empty) = %q", got)
	}
	if got := headerLabel(two); got != "Inbox · 2 waiting" {
		t.Errorf("headerLabel(two) = %q", got)
	}
	if got := title(State{Paused: true}); got != "Wormhole · paused" {
		t.Errorf("title(paused) = %q", got)
	}
	two.Paused = true
	if got := title(two); got != "Wormhole · 2 waiting · paused" {
		t.Errorf("title(two, paused) = %q", got)
	}
	if got := tipText(two); got != "2 waiting in your inbox · daemon paused" {
		t.Errorf("tipText = %q", got)
	}
	if got := iconName(two); got != "mail-unread" {
		t.Errorf("iconName(two) = %q", got)
	}
}
