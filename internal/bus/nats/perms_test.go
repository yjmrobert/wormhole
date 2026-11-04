package nats

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
)

// TestUserPermissionsMatchTemplate keeps the embedded server's ACL and the
// VM template in deploy/nats from drifting apart.
func TestUserPermissionsMatchTemplate(t *testing.T) {
	tpl, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nats", "accounts.conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.conf"), tpl, 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(conf, []byte("listen: 127.0.0.1:-1\ninclude ./accounts.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := server.ProcessConfigFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, u := range opts.Users {
		if u.Username != "alice" && u.Username != "bob" {
			continue
		}
		seen++
		want := UserPermissions(u.Username)
		if u.Permissions == nil || u.Permissions.Publish == nil || u.Permissions.Subscribe == nil {
			t.Fatalf("%s: template has no permissions", u.Username)
		}
		assertSameSet(t, u.Username+" publish", u.Permissions.Publish.Allow, want.Publish.Allow)
		assertSameSet(t, u.Username+" subscribe", u.Permissions.Subscribe.Allow, want.Subscribe.Allow)
		if len(u.Permissions.Publish.Deny) != 0 || len(u.Permissions.Subscribe.Deny) != 0 {
			t.Fatalf("%s: template has deny lists UserPermissions does not", u.Username)
		}
	}
	if seen != 2 {
		t.Fatalf("found %d of alice and bob in the template", seen)
	}
}

// assertSameSet compares two allow lists ignoring order.
func assertSameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Fatalf("%s:\ntemplate %q\ncode     %q", what, g, w)
	}
}
