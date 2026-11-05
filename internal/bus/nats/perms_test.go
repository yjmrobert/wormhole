package nats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
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

// TestInboxIsolation verifies a user can only subscribe to their own reply
// inbox: the shared _INBOX.> wildcard would let any member read every
// other member's JetStream deliveries.
func TestInboxIsolation(t *testing.T) {
	url := startServer(t)
	nc, err := natsgo.Connect(url, natsgo.UserInfo("alice", "CHANGE_ME_alice"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	errCh := make(chan error, 4)
	nc.SetErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errCh <- err })
	for _, subj := range []string{"_INBOX.>", "_INBOX.bob.>", InboxPrefix("bob") + ".x"} {
		if _, err := nc.SubscribeSync(subj); err != nil {
			t.Fatal(err)
		}
		_ = nc.Flush()
		select {
		case err := <-errCh:
			if !errors.Is(err, natsgo.ErrPermissionViolation) {
				t.Fatalf("%s: unexpected error: %v", subj, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("server let alice subscribe to %s", subj)
		}
	}
	// Her own inbox prefix, which Connect configures, is allowed and the
	// registry (a KV watch, which needs an inbox) works through it.
	alice := connect(t, url, "alice")
	if _, err := alice.Registry().List(context.Background()); err != nil {
		t.Fatalf("registry through the per-user inbox: %v", err)
	}
}
