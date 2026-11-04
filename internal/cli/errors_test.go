package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// fixMarkers are what an error that names its fix contains: a command, or
// a config key the person sets.
var fixMarkers = []string{"run:", "wormhole ", "with: ", "--yes", "--force-unsafe", "--repo", "senders:", "bus.url", "copilot.", "daemon.", "WORMHOLE_"}

// namesFix reports whether msg names a command or config line.
func namesFix(msg string) bool {
	for _, m := range fixMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// rewriteConfig applies edit to the fixture's config file.
func rewriteConfig(t *testing.T, f *cliFixture, edit func(string) string) {
	t.Helper()
	raw, err := os.ReadFile(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cfgPath, []byte(edit(string(raw))), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestErrorsNameTheirFix drives each refusal (exit 3), not-found (exit 4)
// and unavailable (exit 5) condition a command can reach and checks the
// message ends with the command or config line that resolves it.
func TestErrorsNameTheirFix(t *testing.T) {
	staged := func(t *testing.T, f *cliFixture) { f.seedThread(t, nil) }
	withState := func(state threads.State) func(*testing.T, *cliFixture) {
		return func(t *testing.T, f *cliFixture) {
			f.seedThread(t, func(th *threads.Thread) { th.State = state; th.StagedKickoff = "" })
		}
	}
	busDown := func(t *testing.T, f *cliFixture) {
		f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) { return nil, errors.New("connection refused") }
	}
	cases := []struct {
		name  string
		setup func(*testing.T, *cliFixture)
		args  []string
		code  int
		want  string
	}{
		{"missing config", func(t *testing.T, f *cliFixture) { f.cfgPath = filepath.Join(t.TempDir(), "none.yaml") }, []string{"users"}, ExitError, "; run: wormhole init, or join a bus with: wormhole join <invite>"},
		{"malformed thread", nil, []string{"open", "zz!!"}, ExitNotFound, "list yours with: wormhole threads"},
		{"unknown thread id", nil, []string{"open", testThreadID}, ExitNotFound, "is not on this machine; list yours with: wormhole threads"},
		{"unknown prefix", nil, []string{"reply", "01ZZ", "x", "--yes"}, ExitNotFound, "list yours with: wormhole threads"},
		{"no thread with peer", nil, []string{"snooze", "^dave"}, ExitNotFound, "no open thread with ^dave; see: wormhole threads --peer dave"},
		{"invalid handle", nil, []string{"ask", "Not!", "hi", "--yes"}, ExitNotFound, "list handles with: wormhole users"},
		{"closed thread", withState(threads.StateClosed), []string{"reply", testThreadID[:8], "x", "--yes"}, ExitRefused, `is closed; start a new ask: wormhole ask ^alice "..."`},
		{"declined thread", withState(threads.StateDeclined), []string{"send", "alice", "--thread", testThreadID, "x", "--yes"}, ExitRefused, `is declined; start a new ask: wormhole ask ^alice "..."`},
		{"wrong peer", staged, []string{"send", "carol", "--thread", "^alice", "x", "--yes"}, ExitNotFound, "send to them with: wormhole send alice --thread " + testThreadID},
		{"nothing staged", withState(threads.StateDelivered), []string{"snooze", "^alice"}, ExitRefused, "(state delivered); see what is: wormhole inbox"},
		{"nothing to open", withState(threads.StateDelivered), []string{"open", "^alice"}, ExitNotFound, "nothing staged; see what is waiting: wormhole inbox"},
		{"max hops", func(t *testing.T, f *cliFixture) {
			f.seedThread(t, nil)
			rewriteConfig(t, f, func(s string) string { return s + "limits: {max_hops: 1}\n" })
			store := f.openFixtureStore(t)
			last, _ := store.LastMessage(context.Background(), testThreadID)
			prev, _ := last.Envelope()
			next := envelope.NewFollowUp(envelope.TypeMessage, prev, envelope.Peer{User: "alice"}, "bob", "more", testNow.Add(-time.Minute))
			if err := store.AddMessage(context.Background(), next, threads.DirectionIn); err != nil {
				t.Fatal(err)
			}
		}, []string{"reply", "^alice", "x", "--yes"}, ExitRefused, `max_hops (1); start a new ask if it still matters: wormhole ask ^alice "..."`},
		{"body too large", staged, []string{"reply", "^alice", strings.Repeat("x", envelope.MaxBodyBytes+1), "--yes"}, ExitRefused, "--repo owner/name"},
		{"secret in body", nil, []string{"ask", "alice", "password=hunter2hunter2", "--yes"}, ExitRefused, "--force-unsafe"},
		{"ask off a terminal", nil, []string{"ask", "alice", "hi"}, ExitRefused, "--yes"},
		{"open off a terminal", staged, []string{"open", "^alice"}, ExitRefused, "wormhole open ^alice --no-start), then rerun with --yes"},
		{"decline off a terminal", staged, []string{"decline", "^alice"}, ExitRefused, "rerun with --yes"},
		{"agent without safe_tools", func(t *testing.T, f *cliFixture) {
			f.seedThread(t, nil)
			rewriteConfig(t, f, func(s string) string { return strings.Replace(s, "  safe_tools: [shell]\n", "", 1) })
		}, []string{"open", "^alice", "--agent", "--yes"}, ExitRefused, "set copilot.safe_tools in your config"},
		{"session does not start", func(t *testing.T, f *cliFixture) {
			f.seedThread(t, nil)
			f.recorder.Fail = func(op string) error {
				if op == "create" {
					return errors.New("copilot: not found")
				}
				return nil
			}
		}, []string{"open", "^alice", "--yes"}, ExitUnavailable, "check copilot.binary in "},
		{"bus down on ask", busDown, []string{"ask", "alice", "hi", "--yes"}, ExitUnavailable, "bus: connection refused; check bus.url in "},
		{"bus down on decline", func(t *testing.T, f *cliFixture) { staged(t, f); busDown(t, f) }, []string{"decline", "^alice", "--yes"}, ExitUnavailable, ", then: wormhole login"},
		{"bus down on users", busDown, []string{"users"}, ExitUnavailable, ", then: wormhole login"},
		{"registry error", func(t *testing.T, f *cliFixture) {
			f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) {
				return &stubBus{Bus: f.bus, registry: &stubRegistry{err: errors.New("no bucket")}}, nil
			}
		}, []string{"users"}, ExitUnavailable, "registry: no bucket; check the bus with: wormhole doctor"},
		{"publish fails", func(t *testing.T, f *cliFixture) {
			f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) {
				return &stubBus{Bus: f.bus, publishErr: errors.New("timeout")}, nil
			}
		}, []string{"ask", "alice", "hi", "--yes"}, ExitUnavailable, "publish: timeout (is the bus reachable? run: wormhole doctor)"},
		{"publish not permitted", func(t *testing.T, f *cliFixture) {
			f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) {
				return &stubBus{Bus: f.bus, publishErr: fmt.Errorf("%w: carol may not publish", bus.ErrPermission)}, nil
			}
		}, []string{"ask", "alice", "hi", "--yes"}, ExitRefused, "check that handle bob in your config is your bus user, then: wormhole login"},
		{"inbox page off", nil, []string{"inbox", "--ui"}, ExitUnavailable, "set daemon.inbox_ui: 127.0.0.1:7788 in "},
		{"inbox page not running", func(t *testing.T, f *cliFixture) {
			rewriteConfig(t, f, func(s string) string {
				return strings.Replace(s, "daemon:\n", "daemon:\n  inbox_ui: 127.0.0.1:7788\n", 1)
			})
		}, []string{"inbox", "--ui"}, ExitUnavailable, "start the daemon: wormhole daemon"},
		{"watch without input", nil, []string{"inbox", "--watch", "--no-input"}, ExitRefused, "or list it with: wormhole inbox"},
		{"unknown command path", nil, []string{"commands", "nope"}, ExitNotFound, "list them with: wormhole commands"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			msg, code := runJSONError(t, f, tc.args...)
			if code != tc.code || !strings.Contains(msg, tc.want) {
				t.Fatalf("code %d (want %d), error %q (want %q)", code, tc.code, msg, tc.want)
			}
			if !namesFix(msg) {
				t.Fatalf("error %q names no fix", msg)
			}
		})
	}
}

// TestHelperErrorsNameTheirFix covers conditions that are impractical to
// reach through a whole command.
func TestHelperErrorsNameTheirFix(t *testing.T) {
	f := newCLIFixture(t)
	errs := map[string]error{}
	errs["watch off a terminal"] = f.app.watch(context.Background(), &common{config: f.cfgPath})
	_, errs["review with --no-input"] = f.app.review(&common{noInput: true}, envelope.Envelope{}, false)
	errs["agent profile"] = f.app.checkAgentProfile(&config.Config{}, true)
	for name, err := range errs {
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != ExitRefused || !namesFix(err.Error()) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if !strings.HasSuffix(errs["watch off a terminal"].Error(), "; use: wormhole inbox") {
		t.Fatalf("watch: %v", errs["watch off a terminal"])
	}
	if got := configFix("/x/config.yaml", errors.New("bad")); !namesFix(got) {
		t.Fatalf("configFix = %q", got)
	}
}
