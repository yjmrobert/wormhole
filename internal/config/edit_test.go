package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes raw to a private file in a fresh directory.
func writeConfig(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readSenders loads path and returns its senders as handle:mode pairs.
func readSenders(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("edited config does not parse: %v\n%s", err, raw)
	}
	out := make([]string, len(cfg.Senders))
	for i, s := range cfg.Senders {
		out[i] = s.Handle + ":" + s.Mode
	}
	return strings.Join(out, ",")
}

// readFile returns path's contents.
func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// autoExample is Example with an auto-mode profile, so auto senders validate.
var autoExample = strings.Replace(Example, "safe_tools: []", "safe_tools: [view, grep]", 1)

// TestAddRemoveSenderKeepsExampleIntact edits config.Example and checks that
// every other line, comments and blank lines included, is untouched.
func TestAddRemoveSenderKeepsExampleIntact(t *testing.T) {
	path := writeConfig(t, autoExample)
	if err := AddSender(path, "carol", ModeInbox); err != nil {
		t.Fatal(err)
	}
	if got := readSenders(t, path); got != "bob:inbox,carol:inbox" {
		t.Fatalf("senders = %s", got)
	}
	want := strings.Replace(autoExample, "  - bob\n", "  - bob\n  - carol\n", 1)
	if got := readFile(t, path); got != want {
		t.Fatalf("file changed beyond the new entry:\n%s", got)
	}
	if err := AddSender(path, "carol", ModeAuto); err != nil {
		t.Fatal(err)
	}
	if got := readSenders(t, path); got != "bob:inbox,carol:auto" {
		t.Fatalf("senders after mode change = %s", got)
	}
	if !strings.Contains(readFile(t, path), "  - {handle: carol, mode: auto}\n") {
		t.Fatalf("auto entry not written as a mapping:\n%s", readFile(t, path))
	}
	if err := RemoveSender(path, "carol"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != autoExample {
		t.Fatalf("add then remove did not restore the file:\n%s", got)
	}
	if err := RemoveSender(path, "bob"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if readSenders(t, path) != "" || !strings.Contains(got, "\nsenders: []\n") || !strings.Contains(got, "# Who may open a wormhole to you.") || !strings.Contains(got, "  # - {handle: carol, mode: auto}\n") {
		t.Fatalf("removing the last sender:\n%s", got)
	}
	if err := RemoveSender(path, "bob"); !errors.Is(err, ErrNoSender) {
		t.Fatalf("removing an absent sender: %v", err)
	}
	if err := AddSender(path, "bob", ModeInbox); err != nil {
		t.Fatal(err)
	}
	if got := readSenders(t, path); got != "bob:inbox" {
		t.Fatalf("senders = %s", got)
	}
}

// TestAddSenderShapes covers an empty flow list, a one-line flow list, a
// null value, a missing key and a file without a trailing newline.
func TestAddSenderShapes(t *testing.T) {
	const head = "# my config\nhandle: alice\nbus: {kind: memory}\n"
	const tail = "\n# trailing comment\ncopilot: {safe_tools: [view]}\n"
	cases := []struct {
		name, senders, want, file string
	}{
		{"empty flow", "senders: []  # nobody yet\n", "carol:inbox", "senders: # nobody yet\n  - carol\n"},
		{"flow list", "senders: [bob]\n", "bob:inbox,carol:inbox", "senders:\n  - bob\n  - carol\n"},
		{"null", "senders:\n", "carol:inbox", "senders:\n  - carol\n"},
		{"missing", "", "carol:inbox", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, head+tc.senders+tail)
			if err := AddSender(path, "carol", ModeInbox); err != nil {
				t.Fatal(err)
			}
			if got := readSenders(t, path); got != tc.want {
				t.Fatalf("senders = %s", got)
			}
			got := readFile(t, path)
			if !strings.HasPrefix(got, head) || !strings.Contains(got, "# trailing comment\n") {
				t.Fatalf("comments lost:\n%s", got)
			}
			if tc.file != "" && !strings.Contains(got, tc.file) {
				t.Fatalf("want %q in:\n%s", tc.file, got)
			}
			if tc.senders == "" && !strings.HasSuffix(got, "senders:\n  - carol\n") {
				t.Fatalf("missing key not appended:\n%s", got)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, %v", info.Mode(), err)
			}
		})
	}

	path := writeConfig(t, "handle: alice\nbus: {kind: memory}")
	if err := AddSender(path, "carol", ModeInbox); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "handle: alice\nbus: {kind: memory}\nsenders:\n  - carol\n" {
		t.Fatalf("no trailing newline:\n%q", got)
	}
}

// TestSenderEditFallsBackToNodes edits a list the text editor does not
// handle (a multi-line mapping entry) and checks comments survive.
func TestSenderEditFallsBackToNodes(t *testing.T) {
	raw := "handle: alice\nbus: {kind: memory}\n# who may ask\nsenders:\n  - handle: bob # my teammate\n    mode: inbox\ncopilot: {safe_tools: [view]}\n"
	path := writeConfig(t, raw)
	if err := AddSender(path, "carol", ModeAuto); err != nil {
		t.Fatal(err)
	}
	if got := readSenders(t, path); got != "bob:inbox,carol:auto" {
		t.Fatalf("senders = %s", got)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "# who may ask") || !strings.Contains(got, "# my teammate") {
		t.Fatalf("comments lost:\n%s", got)
	}
	if err := RemoveSender(path, "bob"); err != nil {
		t.Fatal(err)
	}
	if got := readSenders(t, path); got != "carol:auto" {
		t.Fatalf("senders = %s", got)
	}
}

// TestAddSenderRefusesInvalidResults checks nothing is written when the
// edit would not validate.
func TestAddSenderRefusesInvalidResults(t *testing.T) {
	path := writeConfig(t, Example)
	for _, tc := range []struct{ handle, mode, want string }{
		{"carol", ModeAuto, "copilot.safe_tools is empty"},
		{"alice", ModeInbox, "your own handle"},
		{"Not A Handle", ModeInbox, "not a valid handle"},
		{"carol", "sometimes", "want inbox or auto"},
	} {
		err := AddSender(path, tc.handle, tc.mode)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("AddSender(%s, %s) = %v", tc.handle, tc.mode, err)
		}
	}
	if got := readFile(t, path); got != Example {
		t.Fatal("a refused edit changed the file")
	}
	if err := AddSender(filepath.Join(t.TempDir(), "missing.yaml"), "carol", ModeInbox); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}
