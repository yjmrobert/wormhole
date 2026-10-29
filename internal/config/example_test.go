package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite deploy/config.yaml.example from config.Example")

// The shipped example must be the one `wormhole init` writes.
func TestShippedExampleMatches(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "config.yaml.example")
	if *update {
		if err := os.WriteFile(path, []byte(Example), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != Example {
		t.Fatal("deploy/config.yaml.example differs from config.Example; regenerate with: go test ./internal/config -run TestShippedExampleMatches -update")
	}
}
