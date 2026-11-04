package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDemo is the smoke test: the whole narrated round trip, in JSON, fast.
func TestDemo(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	f := newCLIFixture(t)
	start := time.Now()
	if code := f.run("demo", "--fast", "--json"); code != ExitOK {
		t.Fatalf("demo exited %d: %s %s", code, f.out.String(), f.errOut.String())
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("demo took %s; CI uses it as the smoke test and allows 10s", d)
	}
	var res struct {
		OK    bool       `json:"ok"`
		Steps []demoStep `json:"steps"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &res); err != nil {
		t.Fatalf("demo output is not one JSON object: %q (%v)", f.out.String(), err)
	}
	want := []string{"start", "ask", "inbox", "open", "reply", "snooze", "decline"}
	if !res.OK || len(res.Steps) != len(want) {
		t.Fatalf("demo = %+v", res)
	}
	for i, s := range res.Steps {
		if s.Name != want[i] || !s.OK || s.Detail == "" {
			t.Fatalf("step %d = %+v, want %s ok", i, s, want[i])
		}
	}
	if got := res.Steps[len(res.Steps)-1].Detail; got != "declined: "+demoDeclineText {
		t.Fatalf("decline detail = %q", got)
	}
	// The temporary machines are gone.
	if left, _ := filepath.Glob(filepath.Join(tmp, "wormhole-demo-*")); len(left) != 0 {
		t.Fatalf("demo left %v behind", left)
	}
}

// TestDemoNarration checks the human form tells the story and ends with
// the real-network commands.
func TestDemoNarration(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	f := newCLIFixture(t)
	if code := f.run("demo", "--fast"); code != ExitOK {
		t.Fatalf("demo exited %d: %s", code, f.errOut.String())
	}
	out := f.out.String()
	for _, want := range []string{
		"alice$ wormhole ask bob", "^bob", "bob$ wormhole inbox", "--no-start", "bob$ wormhole reply",
		"alice$ wormhole snooze", " off", "alice$ wormhole decline", "declined: " + demoDeclineText,
		"wormhole join <invite>", `wormhole ask ^<handle> "..."`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("narration lacks %q:\n%s", want, out)
		}
	}
}

// TestDemoTimeout names the step that ran out of time.
func TestDemoTimeout(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	f := newCLIFixture(t)
	if code := f.run("demo", "--fast", "--timeout", "1ns"); code != ExitError {
		t.Fatalf("demo with 1ns timeout exited %d", code)
	}
	if !strings.Contains(strings.ToLower(f.errOut.String()), "timed out at step start") || !strings.Contains(f.errOut.String(), "--timeout") {
		t.Fatalf("timeout error = %q", f.errOut.String())
	}
	if code := f.run("demo", "--timeout", "0s"); code != ExitUsage {
		t.Fatalf("zero timeout exited %d", code)
	}
	if entries, _ := os.ReadDir(os.Getenv("TMPDIR")); len(entries) != 0 {
		t.Fatalf("demo left %v behind", entries)
	}
}
