package tray

import (
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// Standard interface names.
const (
	propIface       = "org.freedesktop.DBus.Properties"
	introspectIface = "org.freedesktop.DBus.Introspectable"
)

// itemNode is the introspection data for /StatusNotifierItem.
func itemNode() *introspect.Node {
	return &introspect.Node{Interfaces: []introspect.Interface{
		introspect.IntrospectData,
		prop.IntrospectData,
		{
			Name: itemIface,
			Methods: []introspect.Method{
				{Name: "Activate", Args: xyArgs()},
				{Name: "SecondaryActivate", Args: xyArgs()},
				{Name: "ContextMenu", Args: xyArgs()},
				{Name: "Scroll", Args: []introspect.Arg{
					{Name: "delta", Type: "i", Direction: "in"},
					{Name: "orientation", Type: "s", Direction: "in"},
				}},
			},
			Signals: []introspect.Signal{
				{Name: "NewTitle"}, {Name: "NewIcon"}, {Name: "NewToolTip"}, {Name: "NewStatus"},
			},
			Properties: []introspect.Property{
				roProp("Category", "s"), roProp("Id", "s"), roProp("Title", "s"),
				roProp("Status", "s"), roProp("WindowId", "i"), roProp("IconName", "s"),
				roProp("IconPixmap", "a(iiay)"), roProp("IconThemePath", "s"),
				roProp("OverlayIconName", "s"), roProp("OverlayIconPixmap", "a(iiay)"),
				roProp("AttentionIconName", "s"), roProp("AttentionIconPixmap", "a(iiay)"),
				roProp("AttentionMovieName", "s"), roProp("ToolTip", "(sa(iiay)ss)"),
				roProp("ItemIsMenu", "b"), roProp("Menu", "o"),
			},
		},
	}}
}

// menuNode is the introspection data for /MenuBar.
func menuNode() *introspect.Node {
	return &introspect.Node{Interfaces: []introspect.Interface{
		introspect.IntrospectData,
		prop.IntrospectData,
		{
			Name: menuIface,
			Methods: []introspect.Method{
				{Name: "GetLayout", Args: []introspect.Arg{
					{Name: "parentId", Type: "i", Direction: "in"},
					{Name: "recursionDepth", Type: "i", Direction: "in"},
					{Name: "propertyNames", Type: "as", Direction: "in"},
					{Name: "revision", Type: "u", Direction: "out"},
					{Name: "layout", Type: "(ia{sv}av)", Direction: "out"},
				}},
				{Name: "GetGroupProperties", Args: []introspect.Arg{
					{Name: "ids", Type: "ai", Direction: "in"},
					{Name: "propertyNames", Type: "as", Direction: "in"},
					{Name: "properties", Type: "a(ia{sv})", Direction: "out"},
				}},
				{Name: "GetProperty", Args: []introspect.Arg{
					{Name: "id", Type: "i", Direction: "in"},
					{Name: "name", Type: "s", Direction: "in"},
					{Name: "value", Type: "v", Direction: "out"},
				}},
				{Name: "Event", Args: []introspect.Arg{
					{Name: "id", Type: "i", Direction: "in"},
					{Name: "eventId", Type: "s", Direction: "in"},
					{Name: "data", Type: "v", Direction: "in"},
					{Name: "timestamp", Type: "u", Direction: "in"},
				}},
				{Name: "EventGroup", Args: []introspect.Arg{
					{Name: "events", Type: "a(isvu)", Direction: "in"},
					{Name: "idErrors", Type: "ai", Direction: "out"},
				}},
				{Name: "AboutToShow", Args: []introspect.Arg{
					{Name: "id", Type: "i", Direction: "in"},
					{Name: "needUpdate", Type: "b", Direction: "out"},
				}},
				{Name: "AboutToShowGroup", Args: []introspect.Arg{
					{Name: "ids", Type: "ai", Direction: "in"},
					{Name: "updatesNeeded", Type: "ai", Direction: "out"},
					{Name: "idErrors", Type: "ai", Direction: "out"},
				}},
			},
			Signals: []introspect.Signal{
				{Name: "LayoutUpdated", Args: []introspect.Arg{
					{Name: "revision", Type: "u"},
					{Name: "parent", Type: "i"},
				}},
				{Name: "ItemsPropertiesUpdated", Args: []introspect.Arg{
					{Name: "updatedProps", Type: "a(ia{sv})"},
					{Name: "removedProps", Type: "a(ias)"},
				}},
			},
			Properties: []introspect.Property{
				roProp("Version", "u"), roProp("TextDirection", "s"),
				roProp("Status", "s"), roProp("IconThemePath", "as"),
			},
		},
	}}
}

// xyArgs are the pointer coordinates every click method takes.
func xyArgs() []introspect.Arg {
	return []introspect.Arg{
		{Name: "x", Type: "i", Direction: "in"},
		{Name: "y", Type: "i", Direction: "in"},
	}
}

// roProp describes a read-only property.
func roProp(name, typ string) introspect.Property {
	return introspect.Property{Name: name, Type: typ, Access: "read"}
}
