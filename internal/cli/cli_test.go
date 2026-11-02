package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// contractApp returns an app with fixed metadata and captured output.
func contractApp(out, errOut *bytes.Buffer) *App {
	return &App{
		Stdin:      strings.NewReader(""),
		Stdout:     out,
		Stderr:     errOut,
		IsTerminal: func() bool { return false },
		Version:    "1.2.3",
		Commit:     "abc1234",
		BuildDate:  "2026-11-01",
	}
}

// TestVersionAndCommandDiscovery verifies version fields and command metadata.
func TestVersionAndCommandDiscovery(t *testing.T) {
	var out, errOut bytes.Buffer
	app := contractApp(&out, &errOut)
	if code := app.Run(context.Background(), []string{"version", "--json"}); code != ExitOK {
		t.Fatalf("version code %d: %s", code, errOut.String())
	}
	var version map[string]string
	if err := json.Unmarshal(out.Bytes(), &version); err != nil {
		t.Fatal(err)
	}
	if version["version"] != "1.2.3" || version["commit"] != "abc1234" || version["build_date"] != "2026-11-01" {
		t.Fatalf("version = %v", version)
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"commands", "--flags", "--json"}); code != ExitOK {
		t.Fatalf("commands code %d: %s", code, errOut.String())
	}
	var doc commandDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 || doc.Command.Name != "wormhole" || len(doc.Command.Commands) == 0 || len(doc.Command.Flags) == 0 {
		t.Fatalf("command document = %+v", doc)
	}
}

// TestUsageErrorsRemainStructuredJSON verifies stable JSON usage failures.
func TestUsageErrorsRemainStructuredJSON(t *testing.T) {
	for _, args := range [][]string{{"not-a-command", "--json"}, {"ask", "--json"}, {"open", "--json"}} {
		var out, errOut bytes.Buffer
		app := contractApp(&out, &errOut)
		if code := app.Run(context.Background(), args); code != ExitUsage {
			t.Fatalf("%v code %d, stderr %q", args, code, errOut.String())
		}
		var res map[string]any
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("%v output %q: %v", args, out.String(), err)
		}
		if res["ok"] != false || res["code"] != float64(ExitUsage) || res["error"] == "" {
			t.Fatalf("%v result %v", args, res)
		}
	}
}

// TestHelpAndCompletionStayPlain verifies redirected output contains no ANSI styling.
func TestHelpAndCompletionStayPlain(t *testing.T) {
	for _, args := range [][]string{{"--help", "--json"}, {"completion", "bash"}} {
		var out, errOut bytes.Buffer
		app := contractApp(&out, &errOut)
		if code := app.Run(context.Background(), args); code != ExitOK {
			t.Fatalf("%v code %d: %s", args, code, errOut.String())
		}
		if strings.Contains(out.String(), "\x1b[") {
			t.Fatalf("%v emitted ANSI output", args)
		}
	}
}

// TestNoInputDoesNotConfirm verifies noninteractive mode refuses confirmation.
func TestNoInputDoesNotConfirm(t *testing.T) {
	app := contractApp(&bytes.Buffer{}, &bytes.Buffer{})
	reviewed, err := app.review(&common{noInput: true}, envelope.Envelope{}, false)
	if reviewed {
		t.Fatal("--no-input marked a message as reviewed")
	}
	exit, ok := err.(*exitError)
	if !ok || exit.code != ExitRefused {
		t.Fatalf("review error = %v", err)
	}
}
