package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
)

func TestSendersAddRemove(t *testing.T) {
	f := newCLIFixture(t)
	if code := f.run("senders", "--json"); code != ExitOK {
		t.Fatalf("list: %d %s", code, f.errOut.String())
	}
	var res struct {
		Senders []senderRow `json:"senders"`
	}
	_ = json.Unmarshal(f.out.Bytes(), &res)
	if len(res.Senders) != 1 || res.Senders[0].Handle != "alice" {
		t.Fatalf("initial = %+v", res.Senders)
	}
	f.out.Reset()
	if code := f.run("senders", "add", "carol", "--auto", "--json"); code != ExitOK {
		t.Fatalf("add --auto: %d %s", code, f.errOut.String())
	}
	_ = json.Unmarshal(f.out.Bytes(), &res)
	if len(res.Senders) != 2 || res.Senders[1].Handle != "carol" || res.Senders[1].Mode != config.ModeAuto {
		t.Fatalf("after add = %+v", res.Senders)
	}
	if code := f.run("senders", "add", "carol"); code != ExitRefused || !strings.Contains(f.errOut.String(), "senders remove carol") {
		t.Fatalf("duplicate add: %d %s", code, f.errOut.String())
	}
	if code := f.run("senders", "add", "bob"); code != ExitUsage {
		t.Fatalf("add self: %d", code)
	}
	if code := f.run("senders", "add", "Bad Handle"); code != ExitNotFound {
		t.Fatalf("bad handle: %d", code)
	}
	if code := f.run("senders", "remove", "dave"); code != ExitNotFound {
		t.Fatalf("remove unknown: %d", code)
	}
	f.out.Reset()
	if code := f.run("senders", "remove", "alice", "--json"); code != ExitOK {
		t.Fatalf("remove: %d %s", code, f.errOut.String())
	}
	_ = json.Unmarshal(f.out.Bytes(), &res)
	if len(res.Senders) != 1 || res.Senders[0].Handle != "carol" {
		t.Fatalf("after remove = %+v", res.Senders)
	}
	cfg, err := config.Load(f.cfgPath)
	if err != nil || len(cfg.Senders) != 1 {
		t.Fatalf("reload = %+v, %v", cfg, err)
	}
}

func TestSendersAutoNeedsSafeTools(t *testing.T) {
	f := newCLIFixture(t)
	if err := config.Edit(f.cfgPath, false, func(doc *yaml.Node) error { config.SetList(doc, "copilot.safe_tools", nil); return nil }); err != nil {
		t.Fatal(err)
	}
	if code := f.run("senders", "add", "carol", "--auto"); code != ExitRefused || !strings.Contains(f.errOut.String(), "copilot.safe_tools") {
		t.Fatalf("auto without safe_tools: %d %s", code, f.errOut.String())
	}
}

func TestCompletionFallsBackToThreadPeers(t *testing.T) {
	f := newCLIFixture(t)
	seedThreads(t, f)
	f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) { return nil, errors.New("down") }
	root, c := f.app.newRootCommand()
	root.SetContext(context.Background())
	cmd, _, _ := root.Find([]string{"ask"})
	cmd.SetContext(context.Background())
	values, _ := cmd.ValidArgsFunction(cmd, nil, "")
	if strings.Join(values, ",") != "alice,bob" {
		t.Fatalf("fallback = %v", values)
	}
	values, _ = f.app.completeThreads(c, false)(cmd, nil, "^al")
	if len(values) != 2 || !strings.HasPrefix(values[0], "^alice\t") {
		t.Fatalf("thread refs = %v", values)
	}
	values, _ = f.app.completeThreads(c, true)(cmd, nil, "01B")
	if len(values) != 0 {
		t.Fatalf("staged-only completion offered a non-staged thread: %v", values)
	}
	values, _ = f.app.completeThreads(c, false)(cmd, nil, "01B")
	if len(values) != 1 {
		t.Fatalf("prefix completion = %v", values)
	}
}
