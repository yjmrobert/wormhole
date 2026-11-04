package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// sendersOf runs a senders command with --json and decodes the list.
func sendersOf(t *testing.T, f *cliFixture, args ...string) []senderRow {
	t.Helper()
	f.out.Reset()
	if code := f.run(append(args, "--json")...); code != ExitOK {
		t.Fatalf("%v code %d: %s", args, code, f.out)
	}
	var res struct {
		OK      bool        `json:"ok"`
		Senders []senderRow `json:"senders"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil || !res.OK {
		t.Fatalf("%v output %s: %v", args, f.out, err)
	}
	return res.Senders
}

// TestSendersCommand lists, adds and removes senders through the CLI.
func TestSendersCommand(t *testing.T) {
	f := newCLIFixture(t)
	if got := sendersOf(t, f, "senders"); len(got) != 1 || got[0] != (senderRow{"alice", "inbox"}) {
		t.Fatalf("senders = %v", got)
	}
	if got := sendersOf(t, f, "senders", "add", "carol"); len(got) != 2 || got[1] != (senderRow{"carol", "inbox"}) {
		t.Fatalf("after add = %v", got)
	}
	if got := sendersOf(t, f, "senders", "add", "^carol", "--auto"); len(got) != 2 || got[1] != (senderRow{"carol", "auto"}) {
		t.Fatalf("after add --auto = %v", got)
	}
	info, err := os.Stat(f.cfgPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v, %v", info.Mode(), err)
	}
	if got := sendersOf(t, f, "senders", "remove", "carol"); len(got) != 1 || got[0].Handle != "alice" {
		t.Fatalf("after remove = %v", got)
	}

	f.out.Reset()
	if code := f.run("senders", "remove", "alice"); code != ExitOK || !strings.Contains(f.out.String(), "^alice may no longer ask you; restart the daemon") || !strings.Contains(f.out.String(), noSendersMessage) {
		t.Fatalf("remove last code %d:\n%s", code, f.out)
	}
	f.out.Reset()
	if code := f.run("senders"); code != ExitOK || strings.TrimSpace(f.out.String()) != noSendersMessage {
		t.Fatalf("empty list:\n%s", f.out)
	}
	if got := sendersOf(t, f, "senders", "add", "alice"); len(got) != 1 {
		t.Fatalf("re-add = %v", got)
	}
}

// TestSendersRefusals checks each refusal names its fix and leaves the file
// alone.
func TestSendersRefusals(t *testing.T) {
	f := newCLIFixture(t)
	before, err := os.ReadFile(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"senders", "add", "bob"}, ExitRefused, "bob is your own handle; you cannot add yourself to senders:, add someone else: wormhole senders add <handle>"},
		{[]string{"senders", "remove", "dave"}, ExitNotFound, "^dave is not under senders: in " + f.cfgPath + "; see who is: wormhole senders"},
		{[]string{"senders", "add", "Not!"}, ExitNotFound, "list handles with: wormhole users"},
		{[]string{"senders", "add"}, ExitUsage, "usage: wormhole senders add <handle> [--auto]"},
	}
	for _, tc := range cases {
		if msg, code := runJSONError(t, f, tc.args...); code != tc.code || !strings.Contains(msg, tc.want) {
			t.Fatalf("%v: code %d, error %q", tc.args, code, msg)
		}
	}

	raw := strings.Replace(string(before), "  safe_tools: [shell]\n", "", 1)
	if err := os.WriteFile(f.cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, code := runJSONError(t, f, "senders", "add", "carol", "--auto")
	if code != ExitRefused || !strings.Contains(msg, "set copilot.safe_tools in "+f.cfgPath) || !strings.Contains(msg, "wormhole senders add carol") {
		t.Fatalf("auto without safe_tools: code %d, %q", code, msg)
	}
	if after, _ := os.ReadFile(f.cfgPath); string(after) != raw {
		t.Fatalf("a refused edit changed the config:\n%s", after)
	}
}

// runJSONError runs args with --json and returns the error and exit code.
func runJSONError(t *testing.T, f *cliFixture, args ...string) (string, int) {
	t.Helper()
	f.out.Reset()
	code := f.run(append(args, "--json")...)
	var res struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(f.out.Bytes(), &res)
	return res.Error, code
}
