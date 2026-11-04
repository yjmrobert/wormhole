package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// namesFix reports whether an error message ends with something a person
// can run or change: a command, or a config key.
func namesFix(msg string) bool {
	for _, marker := range []string{"wormhole ", "run:", "use ", "systemctl ", "chmod ", "--yes", "--force", "--dir", "--rotate", "bus.url", "senders", "copilot.", "daemon.", "WORMHOLE_CONFIG_DIR", "loginctl", "nats "} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// TestErrorsNameTheFix drives every refusal, not-found and unavailable
// condition the CLI can produce off a terminal and checks the message
// says what to do about it.
func TestErrorsNameTheFix(t *testing.T) {
	f := newCLIFixture(t)
	seedThreads(t, f)
	closed := "01ARZ3NDEKTSV4RRFFQ69G5FAA"
	pending := "01ARZ3NDEKTSV4RRFFQ69G5FAD"
	cases := []struct {
		name string
		args []string
		code int
		prep func()
	}{
		{"no config", []string{"users", "--config", filepath.Join(t.TempDir(), "missing.yaml")}, ExitError, nil},
		{"unknown thread", []string{"open", "01ARZ3NDEKTSV4RRFFQ69G5FZZ", "--no-start"}, ExitNotFound, nil},
		{"ambiguous prefix", []string{"open", "01ARZ", "--no-start"}, ExitUsage, nil},
		{"not a thread", []string{"open", "nope!", "--no-start"}, ExitNotFound, nil},
		{"no open thread with peer", []string{"open", "^carol", "--no-start"}, ExitNotFound, nil},
		{"nothing staged", []string{"snooze", closed, "2h"}, ExitRefused, nil},
		{"bad duration", []string{"snooze", pending, "soon"}, ExitUsage, nil},
		{"open needs yes", []string{"open", pending}, ExitRefused, nil},
		{"open no-input", []string{"open", pending, "--no-input"}, ExitRefused, nil},
		{"decline needs yes", []string{"decline", pending, "busy"}, ExitRefused, nil},
		{"ask needs yes", []string{"ask", "alice", "hello"}, ExitRefused, nil},
		{"ask no-input", []string{"ask", "alice", "hello", "--no-input"}, ExitRefused, nil},
		{"ask secret", []string{"ask", "alice", "token ghp_abcdefghijklmnopqrstuvwxyz0123456789", "--yes"}, ExitRefused, nil},
		{"ask bad handle", []string{"ask", "Not Valid", "hello", "--yes"}, ExitNotFound, nil},
		{"reply closed thread", []string{"reply", closed, "x", "--yes"}, ExitRefused, nil},
		{"send wrong peer", []string{"send", "bob", "--thread", pending, "x", "--yes"}, ExitNotFound, nil},
		{"threads bad peer", []string{"threads", "--peer", "Not Valid"}, ExitNotFound, nil},
		{"inbox ui off", []string{"inbox", "--ui"}, ExitUnavailable, nil},
		{"watch no-input", []string{"inbox", "--watch", "--no-input"}, ExitRefused, nil},
		{"senders remove unknown", []string{"senders", "remove", "nobody"}, ExitNotFound, nil},
		{"senders add self", []string{"senders", "add", "bob"}, ExitUsage, nil},
		{"commands unknown", []string{"commands", "nope"}, ExitNotFound, nil},
		{"skill no agent", []string{"skill", "install"}, ExitNotFound, func() { f.app.LookPath = func(string) (string, error) { return "", errors.New("no") } }},
		{"join garbage", []string{"join", "nope"}, ExitUsage, nil},
		{"serve invite no bus", []string{"serve", "invite", "carol"}, ExitUnavailable, func() { t.Setenv("WORMHOLE_STATE_DIR", t.TempDir()) }},
		{"bus down", []string{"users"}, ExitUnavailable, func() {
			f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) { return nil, errors.New("connection refused") }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.prep != nil {
				tc.prep()
			}
			f.out.Reset()
			f.errOut.Reset()
			args := append([]string{}, tc.args...)
			hasConfig := false
			for _, a := range args {
				if a == "--config" {
					hasConfig = true
				}
			}
			var code int
			if hasConfig {
				code = f.app.Run(context.Background(), append(args, "--json"))
			} else {
				code = f.run(append(args, "--json")...)
			}
			var res map[string]any
			_ = json.Unmarshal(f.out.Bytes(), &res)
			msg, _ := res["error"].(string)
			if code != tc.code {
				t.Fatalf("code %d, want %d: %s %s", code, tc.code, msg, f.errOut.String())
			}
			if !namesFix(msg) {
				t.Fatalf("error does not name a fix: %q", msg)
			}
		})
	}
}

// TestDoctorFixes checks that failing doctor checks carry a fix and that
// the new checks appear.
func TestDoctorFixes(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f.app.LookPath = func(string) (string, error) { return "", errors.New("no") }
	f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) { return nil, errors.New("connection refused") }
	// The fixture's memory bus needs no credential file; make it a nats
	// config with a missing password file to exercise that check.
	if err := os.WriteFile(f.cfgPath, []byte("handle: bob\nbus: {kind: nats, url: tls://bus.example.internal:4222, password_file: "+filepath.Join(t.TempDir(), "none")+"}\nsenders: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("doctor", "--json"); code != ExitError {
		t.Fatalf("doctor: %d %s", code, f.errOut.String())
	}
	var res struct {
		OK     bool    `json:"ok"`
		Checks []check `json:"checks"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	byName := map[string]check{}
	for _, c := range res.Checks {
		byName[c.Name] = c
		if !c.OK && !namesFix(c.Fix) {
			t.Errorf("check %s fails without a fix: %+v", c.Name, c)
		}
	}
	if byName["credentials"].OK || !strings.Contains(byName["credentials"].Fix, "wormhole login") {
		t.Fatalf("credentials = %+v", byName["credentials"])
	}
	if !byName["senders"].OK || !strings.Contains(byName["senders"].Fix, "senders add") {
		t.Fatalf("senders = %+v", byName["senders"])
	}
	if !strings.Contains(byName["service"].Fix, "service install") || !strings.Contains(byName["skill"].Fix, "skill install") {
		t.Fatalf("service = %+v skill = %+v", byName["service"], byName["skill"])
	}
	if strings.Contains(f.out.String(), `"hint"`) {
		t.Fatal("doctor still emits hint")
	}
	// A pending message names the peer to open.
	f2 := newCLIFixture(t)
	f2.seedThread(t, nil)
	f2.out.Reset()
	_ = f2.run("doctor", "--json")
	_ = json.Unmarshal(f2.out.Bytes(), &res)
	for _, c := range res.Checks {
		if c.Name == "inbox" && !strings.Contains(c.Fix, "wormhole open ^alice") {
			t.Fatalf("inbox = %+v", c)
		}
	}
	_ = threads.StatePending
}
