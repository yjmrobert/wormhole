package main

import (
	"runtime/debug"
	"testing"
)

// TestResolveVersion verifies linker, module, and development version sources.
func TestResolveVersion(t *testing.T) {
	originalVersion := version
	originalReadBuildInfo := readBuildInfo
	t.Cleanup(func() {
		version = originalVersion
		readBuildInfo = originalReadBuildInfo
	})

	tests := []struct {
		name        string
		linker      string
		buildInfo   *debug.BuildInfo
		buildInfoOK bool
		want        string
	}{
		{name: "linker value", linker: "v1.2.3", want: "v1.2.3"},
		{name: "module version", buildInfo: &debug.BuildInfo{Main: debug.Module{Version: "v2.3.4"}}, buildInfoOK: true, want: "v2.3.4"},
		{name: "development module", buildInfo: &debug.BuildInfo{Main: debug.Module{Version: develBuildVersion}}, buildInfoOK: true, want: developmentVersion},
		{name: "empty module version", buildInfo: &debug.BuildInfo{}, buildInfoOK: true, want: developmentVersion},
		{name: "build info unavailable", want: developmentVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version = tt.linker
			readBuildInfo = func() (*debug.BuildInfo, bool) { return tt.buildInfo, tt.buildInfoOK }
			if got := resolveVersion(); got != tt.want {
				t.Fatalf("resolveVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveBuildSetting verifies VCS lookup and fallback behavior.
func TestResolveBuildSetting(t *testing.T) {
	originalReadBuildInfo := readBuildInfo
	t.Cleanup(func() { readBuildInfo = originalReadBuildInfo })

	tests := []struct {
		name        string
		value       string
		buildInfo   *debug.BuildInfo
		buildInfoOK bool
		want        string
	}{
		{name: "linker value", value: "abc123", want: "abc123"},
		{
			name:  "VCS setting",
			value: unknownBuildSetting,
			buildInfo: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: vcsRevisionKey, Value: ""},
				{Key: "vcs.modified", Value: "true"},
				{Key: vcsRevisionKey, Value: "def456"},
			}},
			buildInfoOK: true,
			want:        "def456",
		},
		{name: "missing setting retains fallback", value: unknownBuildSetting, buildInfo: &debug.BuildInfo{}, buildInfoOK: true, want: unknownBuildSetting},
		{name: "empty fallback becomes unknown", buildInfo: &debug.BuildInfo{}, buildInfoOK: true, want: unknownBuildSetting},
		{name: "build info unavailable", value: unknownBuildSetting, want: unknownBuildSetting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readBuildInfo = func() (*debug.BuildInfo, bool) { return tt.buildInfo, tt.buildInfoOK }
			if got := resolveBuildSetting(tt.value, vcsRevisionKey); got != tt.want {
				t.Fatalf("resolveBuildSetting(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

// TestRun exercises construction and execution of the default CLI.
func TestRun(t *testing.T) {
	originalVersion := version
	originalCommit := commit
	originalBuildDate := buildDate
	t.Cleanup(func() {
		version = originalVersion
		commit = originalCommit
		buildDate = originalBuildDate
	})
	version = "test-version"
	commit = "test-commit"
	buildDate = "test-date"
	if code := run([]string{"version"}); code != 0 {
		t.Fatalf("run(version) exit code = %d, want 0", code)
	}
}
