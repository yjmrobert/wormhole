package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorChecks runs doctor --json and indexes the checks by name.
func doctorChecks(t *testing.T, f *cliFixture) map[string]check {
	t.Helper()
	f.out.Reset()
	f.run("doctor", "--json")
	var res struct {
		Checks []map[string]any `json:"checks"`
	}
	// A failing doctor prints the report, then the error object.
	if err := json.NewDecoder(f.out).Decode(&res); err != nil {
		t.Fatalf("doctor output %s: %v", f.out, err)
	}
	out := map[string]check{}
	for _, raw := range res.Checks {
		if _, ok := raw["hint"]; ok {
			t.Fatalf("check %v still has a hint key", raw)
		}
		b, _ := json.Marshal(raw)
		var ch check
		_ = json.Unmarshal(b, &ch)
		if !ch.OK && !namesFix(ch.Fix) {
			t.Fatalf("failing check %q has no fix: %+v", ch.Name, ch)
		}
		out[ch.Name] = ch
	}
	return out
}

// TestDoctorChecksNameTheirFix checks the senders, inbox and credentials
// checks and that every failing check carries a fix.
func TestDoctorChecksNameTheirFix(t *testing.T) {
	f := newCLIFixture(t)
	f.seedThread(t, nil)
	checks := doctorChecks(t, f)
	if ch := checks["config"]; !ch.OK || ch.Detail != f.cfgPath {
		t.Fatalf("config = %+v", ch)
	}
	if ch := checks["credentials"]; !ch.OK || !strings.Contains(ch.Detail, "memory") {
		t.Fatalf("credentials = %+v", ch)
	}
	if ch := checks["senders"]; !ch.OK || ch.Detail != "alice" {
		t.Fatalf("senders = %+v", ch)
	}
	if ch := checks["inbox"]; !ch.OK || !strings.HasPrefix(ch.Detail, "1 pending") || !strings.Contains(ch.Fix, "wormhole open ^alice") {
		t.Fatalf("inbox = %+v", ch)
	}
	if ch := checks["bus"]; !ch.OK {
		t.Fatalf("bus = %+v", ch)
	}

	// An empty allowlist warns without failing.
	raw, _ := os.ReadFile(f.cfgPath)
	if err := os.WriteFile(f.cfgPath, []byte(strings.Replace(string(raw), "senders: [alice]", "senders: []", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch := doctorChecks(t, f)["senders"]; !ch.OK || ch.Detail != noSendersMessage {
		t.Fatalf("empty senders = %+v", ch)
	}

	// A nats config whose credential is missing fails with the login fix.
	dir := t.TempDir()
	nats := strings.Replace(string(raw), "bus: {kind: memory}", "bus: {kind: nats, url: nats://bus.example.internal:4222, password_file: "+filepath.Join(dir, "password")+"}", 1)
	if err := os.WriteFile(f.cfgPath, []byte(nats), 0o600); err != nil {
		t.Fatal(err)
	}
	ch := doctorChecks(t, f)["credentials"]
	if ch.OK || !strings.HasSuffix(ch.Detail, "password does not exist") || ch.Fix != "run: wormhole login, or rejoin with: wormhole join <invite>" {
		t.Fatalf("missing credentials = %+v", ch)
	}
	if err := os.WriteFile(filepath.Join(dir, "password"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch := doctorChecks(t, f)["credentials"]; !ch.OK {
		t.Fatalf("present credentials = %+v", ch)
	}
}

// TestDoctorConfigFix checks the fix for a config that does not load.
func TestDoctorConfigFix(t *testing.T) {
	f := newCLIFixture(t)
	f.cfgPath = filepath.Join(t.TempDir(), "missing.yaml")
	checks := doctorChecks(t, f)
	if ch := checks["config"]; ch.OK || ch.Fix != "run: wormhole init, or join a bus with: wormhole join <invite>" {
		t.Fatalf("missing config = %+v", ch)
	}
	if err := os.WriteFile(f.cfgPath, []byte("handle: bob\nbus: {kind: memory}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if ch := doctorChecks(t, f)["config"]; ch.OK || ch.Fix != "run: chmod 600 "+f.cfgPath {
		t.Fatalf("world-readable config = %+v", ch)
	}
	if err := os.WriteFile(f.cfgPath, []byte("handle: Bob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch := doctorChecks(t, f)["config"]; ch.OK || !strings.Contains(ch.Fix, f.cfgPath) {
		t.Fatalf("invalid config = %+v", ch)
	}
}
