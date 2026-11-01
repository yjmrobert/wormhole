// Command wormhole is the CLI and daemon: see `wormhole help`.
package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/yjmrobert/wormhole/internal/cli"
)

// Build metadata is set by release builds with -ldflags. The version variable
// name is retained for compatibility with existing build scripts.
var (
	version   = ""
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	app := cli.Default()
	app.Version = resolveVersion()
	app.Commit = resolveBuildSetting(commit, "vcs.revision")
	app.BuildDate = resolveBuildSetting(buildDate, "vcs.time")
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

func resolveBuildSetting(value, key string) string {
	if value != "" && value != "unknown" {
		return value
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == key && setting.Value != "" {
				return setting.Value
			}
		}
	}
	if value != "" {
		return value
	}
	return "unknown"
}
