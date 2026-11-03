// Command wormhole is the CLI and daemon: see `wormhole help`.
package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/yjmrobert/wormhole/internal/cli"
)

const (
	unknownBuildSetting = "unknown"
	developmentVersion  = "dev"
	develBuildVersion   = "(devel)"
	vcsRevisionKey      = "vcs.revision"
	vcsTimeKey          = "vcs.time"
)

// Build metadata is set by release builds with -ldflags. The version variable
// name is retained for compatibility with existing build scripts.
var (
	version       = ""
	commit        = unknownBuildSetting
	buildDate     = unknownBuildSetting
	readBuildInfo = debug.ReadBuildInfo
)

// main runs the command and terminates with its exit status.
func main() {
	os.Exit(run(os.Args[1:]))
}

// run configures build metadata on the default CLI and executes args.
func run(args []string) int {
	app := cli.Default()
	app.Version = resolveVersion()
	app.Commit = resolveBuildSetting(commit, vcsRevisionKey)
	app.BuildDate = resolveBuildSetting(buildDate, vcsTimeKey)
	return app.Run(context.Background(), args)
}

// resolveVersion prefers linker metadata, then module build info, and falls
// back to a development label for local builds.
func resolveVersion() string {
	if version != "" {
		return version
	}
	if info, ok := readBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != develBuildVersion {
		return info.Main.Version
	}
	return developmentVersion
}

// resolveBuildSetting resolves linker metadata from Go VCS build settings,
// retaining a non-empty fallback when the requested setting is unavailable.
func resolveBuildSetting(value, key string) string {
	if value != "" && value != unknownBuildSetting {
		return value
	}
	if info, ok := readBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == key && setting.Value != "" {
				return setting.Value
			}
		}
	}
	if value != "" {
		return value
	}
	return unknownBuildSetting
}
