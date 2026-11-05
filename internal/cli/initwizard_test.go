package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/config"
)

func TestInitNoInputWritesValidConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	app := contractApp(&out, &errOut)
	app.Getenv = func(k string) string {
		if k == "USER" {
			return "Bob"
		}
		return ""
	}
	path := filepath.Join(t.TempDir(), "cfg", "config.yaml")
	code := app.Run(context.Background(), []string{"init", "--no-input", "--url", "tls://bus.example.internal:4222,tls://10.0.0.5:4222", "--senders", "alice, carol", "--notify", "digest", "--json", "--config", path})
	if code != ExitOK {
		t.Fatalf("init: %d %s %s", code, out.String(), errOut.String())
	}
	var res initResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Handle != "bob" || len(res.Missing) != 0 || len(res.Next) == 0 || !strings.Contains(res.Next[0], "wormhole login") {
		t.Fatalf("result = %+v", res)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Handle != "bob" || cfg.Bus.URL != "tls://bus.example.internal:4222,tls://10.0.0.5:4222" || len(cfg.Senders) != 2 || cfg.Senders[1].Handle != "carol" || cfg.Daemon.Notify != config.NotifyDigest || cfg.Bus.PasswordFile != filepath.Join(filepath.Dir(path), "password") {
		t.Fatalf("cfg = %+v", cfg)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# Your handle.") || strings.Contains(strings.ToLower(string(raw)), "now edit") {
		t.Fatalf("config:\n%s", raw)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	// --force starts over from the example: a key the wizard does not
	// set (the CA of a previous bus) does not survive.
	if err := config.Edit(path, false, func(doc *yaml.Node) error { config.Set(doc, "bus.tls_ca", "/old/bus-ca.pem"); return nil }); err != nil {
		t.Fatal(err)
	}
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--force", "--handle", "bob", "--url", "tls://public:4222", "--config", path}); code != ExitOK {
		t.Fatalf("init --force: %d %s", code, errOut.String())
	}
	if cfg, _ := config.Load(path); cfg.Bus.TLSCA != "" || cfg.Bus.URL != "tls://public:4222" {
		t.Fatalf("--force kept old keys: %+v", cfg.Bus)
	}
	// Exists: refused without --force, and the fix is named.
	out.Reset()
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--config", path}); code != ExitRefused || !strings.Contains(errOut.String(), "--force") {
		t.Fatalf("exists: %d %s", code, errOut.String())
	}
	// Nothing known: the example is written and doctor is pointed at.
	out.Reset()
	app.Getenv = func(string) string { return "" }
	path2 := filepath.Join(t.TempDir(), "config.yaml")
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--json", "--config", path2}); code != ExitOK {
		t.Fatalf("bare init: %d %s", code, errOut.String())
	}
	_ = json.Unmarshal(out.Bytes(), &res)
	if len(res.Missing) != 3 || !strings.Contains(res.Missing[0], "--handle") || !strings.Contains(res.Missing[1], "--url") || !strings.Contains(res.Missing[2], "senders add") {
		t.Fatalf("missing = %v", res.Missing)
	}
	if _, err := config.Load(path2); err != nil {
		t.Fatalf("bare config does not load: %v", err)
	}
	// creds instead of a password.
	creds := filepath.Join(t.TempDir(), "bob.creds")
	_ = os.WriteFile(creds, []byte("x"), 0o600)
	path3 := filepath.Join(t.TempDir(), "config.yaml")
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--handle", "bob", "--url", "tls://b:4222", "--creds", creds, "--config", path3}); code != ExitOK {
		t.Fatalf("creds init: %d %s", code, errOut.String())
	}
	cfg, _ = config.Load(path3)
	if cfg.Bus.Creds != creds || cfg.Bus.PasswordFile != "" {
		t.Fatalf("creds cfg = %+v", cfg.Bus)
	}
	// Bad answers are usage errors.
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--handle", "Not Valid", "--config", filepath.Join(t.TempDir(), "c.yaml")}); code != ExitUsage {
		t.Fatalf("bad handle: %d", code)
	}
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--handle", "bob", "--senders", "bob", "--config", filepath.Join(t.TempDir(), "c.yaml")}); code != ExitUsage {
		t.Fatalf("self sender: %d", code)
	}
	// An invite hands over to join; garbage is a usage error from there.
	if code := app.Run(context.Background(), []string{"init", "--no-input", "--invite", "nope", "--config", filepath.Join(t.TempDir(), "c.yaml")}); code != ExitUsage || !strings.Contains(errOut.String(), "not a wormhole invite") {
		t.Fatalf("invite: %d %s", code, errOut.String())
	}
}
