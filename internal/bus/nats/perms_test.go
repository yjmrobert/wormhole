package nats

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
)

// The embedded server (wormhole serve) and the VM path
// (deploy/nats/accounts.conf.example) must grant the same permissions.
func TestUserPermissionsMatchAccountsExample(t *testing.T) {
	accounts, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nats", "accounts.conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.conf"), accounts, 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("listen: 127.0.0.1:-1\njetstream { store_dir: %q }\ninclude ./accounts.conf\n", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := server.ProcessConfigFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, u := range opts.Users {
		if u.Username == "admin" {
			continue
		}
		seen++
		want := UserPermissions(u.Username)
		if got, exp := sorted(u.Permissions.Publish.Allow), sorted(want.Publish.Allow); fmt.Sprint(got) != fmt.Sprint(exp) {
			t.Errorf("%s publish:\n example %v\n code    %v", u.Username, got, exp)
		}
		if got, exp := sorted(u.Permissions.Subscribe.Allow), sorted(want.Subscribe.Allow); fmt.Sprint(got) != fmt.Sprint(exp) {
			t.Errorf("%s subscribe:\n example %v\n code    %v", u.Username, got, exp)
		}
		if len(u.Permissions.Publish.Deny) != 0 || len(u.Permissions.Subscribe.Deny) != 0 {
			t.Errorf("%s: the example has deny lists the code does not", u.Username)
		}
	}
	if seen != 2 {
		t.Fatalf("expected alice and bob in the example, saw %d users", seen)
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
