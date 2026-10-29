package ghapp

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNewSessionID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewSessionID()
		if !re.MatchString(id) {
			t.Fatalf("not a v4 uuid: %s", id)
		}
		if seen[id] {
			t.Fatal("duplicate id")
		}
		seen[id] = true
	}
}

func TestSessionName(t *testing.T) {
	if got := SessionName("alice", "  What do\nyou think?"); got != "^alice: What do you think?" {
		t.Fatal(got)
	}
	long := SessionName("alice", strings.Repeat("x", 100))
	if len([]rune(long)) != len("^alice: ")+41 {
		t.Fatal(long)
	}
}

func TestDeepLinkIsIsolatedHere(t *testing.T) {
	if DeepLink("abc") != "ghapp://sessions/abc" {
		t.Fatal(DeepLink("abc"))
	}
	// Nothing outside this package may know the scheme.
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if strings.Contains(path, filepath.Join("internal", "ghapp")) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "ghapp://") {
			t.Errorf("%s mentions the deep-link scheme; only internal/ghapp may", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestExecArguments runs Create against a fake copilot script and checks the
// kickoff reaches it as a single argument.
func TestExecArguments(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\n"
	bin := filepath.Join(dir, "copilot")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := NewExec(bin)
	s := Session{ID: "sid", Name: "^alice: hi", Kickoff: "line one\nline two; rm -rf /", Workdir: filepath.Join(dir, "wd"), Model: "m", AvailableTools: []string{"view", "grep"}}
	if err := e.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	want := "-p\nline one\nline two; rm -rf /\n--session-id\nsid\n-n\n^alice: hi\n--model\nm\n--available-tools\nview,grep\n"
	if string(got) != want {
		t.Fatalf("args:\n%s\nwant:\n%s", got, want)
	}
	if err := e.Resume(context.Background(), "sid", "follow up", dir); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(out)
	if string(got) != "-p\nfollow up\n--resume=sid\n" {
		t.Fatalf("resume args: %q", got)
	}
}
