// Command wormhole is the CLI and daemon: see `wormhole help`.
package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/yjmrobert/wormhole/internal/cli"
)

// version is set by the release build (-ldflags "-X main.version=...").
var version = ""

func main() {
	app := cli.Default()
	app.Version = resolveVersion()
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}

func resolveVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
