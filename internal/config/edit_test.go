package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddAndRemoveSenderKeepComments(t *testing.T) {
	p := writeCfg(t, Example)
	if err := AddSender(p, "carol", ModeInbox); err != nil {
		t.Fatal(err)
	}
	if err := AddSender(p, "carol", ModeInbox); !errors.Is(err, ErrExists) {
		t.Fatalf("second add = %v", err)
	}
	raw, _ := os.ReadFile(p)
	s := string(raw)
	for _, want := range []string{"- bob\n", "- carol\n", "# - {handle: carol, mode: auto}", "# Who may open a wormhole to you", "max_hops: 6"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	cfg, err := Load(p)
	if err != nil || len(cfg.Senders) != 2 || cfg.Senders[1].Handle != "carol" || cfg.Senders[1].Mode != ModeInbox {
		t.Fatalf("load = %+v, %v", cfg, err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if err := RemoveSender(p, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSender(p, "bob"); !errors.Is(err, ErrNotListed) {
		t.Fatalf("second remove = %v", err)
	}
	cfg, err = Load(p)
	if err != nil || len(cfg.Senders) != 1 || cfg.Senders[0].Handle != "carol" {
		t.Fatalf("after remove = %+v, %v", cfg, err)
	}
}

func TestAddSenderAutoAndEmptyList(t *testing.T) {
	p := writeCfg(t, "handle: alice\nbus: {kind: memory}\nsenders: []\ncopilot: {safe_tools: [view]}\n")
	if err := AddSender(p, "bob", ModeAuto); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil || len(cfg.Senders) != 1 || cfg.Senders[0].Mode != ModeAuto {
		t.Fatalf("auto = %+v, %v", cfg, err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "{handle: bob, mode: auto}") {
		t.Fatalf("auto entry not in flow style:\n%s", raw)
	}
	// Auto mode without safe_tools does not validate and is not written.
	p2 := writeCfg(t, "handle: alice\nbus: {kind: memory}\n")
	if err := AddSender(p2, "bob", ModeAuto); !errors.Is(err, ErrInvalid) {
		t.Fatalf("auto without safe_tools = %v", err)
	}
	// No senders key at all.
	if err := AddSender(p2, "bob", ModeInbox); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(p2)
	if err != nil || len(cfg.Senders) != 1 {
		t.Fatalf("no key = %+v, %v", cfg, err)
	}
	// Removing the last one leaves an empty list that still loads.
	if err := RemoveSender(p2, "bob"); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(p2); err != nil || len(cfg.Senders) != 0 {
		t.Fatalf("empty = %+v, %v", cfg, err)
	}
}

func TestEditSetsScalarsAndCreatesFromExample(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg", "config.yaml")
	err := Edit(p, true, func(doc *yaml.Node) error {
		Set(doc, "handle", "bob")
		Set(doc, "bus.url", "tls://a:4222,tls://10.0.0.5:4222")
		Set(doc, "bus.tls_ca", filepath.Join(filepath.Dir(p), "bus-ca.pem"))
		Set(doc, "bus.password_file", filepath.Join(filepath.Dir(p), "password"))
		Set(doc, "daemon.notify", "digest")
		SetList(doc, "senders", nil)
		if v, ok := Get(doc, "handle"); !ok || v != "bob" {
			return errors.New("get")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Handle != "bob" || cfg.Bus.URL != "tls://a:4222,tls://10.0.0.5:4222" || cfg.Daemon.Notify != NotifyDigest || len(cfg.Senders) != 0 || !strings.HasSuffix(cfg.Bus.TLSCA, "bus-ca.pem") {
		t.Fatalf("cfg = %+v", cfg)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "# Your handle.") {
		t.Fatalf("comments lost:\n%s", raw)
	}
	// An edit that produces an invalid config is refused and leaves the file alone.
	before := string(raw)
	err = Edit(p, false, func(doc *yaml.Node) error { Set(doc, "handle", "Not Valid"); return nil })
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid edit = %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(after) != before {
		t.Fatal("invalid edit changed the file")
	}
}
