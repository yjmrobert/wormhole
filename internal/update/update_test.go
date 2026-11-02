package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	mode     int64
}

type fakeGitHub struct {
	server       *httptest.Server
	tag          string
	archive      []byte
	checksums    []byte
	latestStatus int

	mu   sync.Mutex
	path []string
}

// newFakeGitHub serves configurable release metadata and assets.
func newFakeGitHub(t *testing.T, tag string, archive []byte) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tag: tag, archive: archive}
	hash := sha256.Sum256(archive)
	f.checksums = []byte(fmt.Sprintf("%x  %s\n", hash, archiveName(tag)))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.path = append(f.path, r.URL.Path)
		f.mu.Unlock()
		switch r.URL.Path {
		case "/repos/acme/wormhole/releases/latest":
			if f.latestStatus != 0 {
				w.WriteHeader(f.latestStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": f.tag,
				"assets": []map[string]string{
					{"name": archiveName(f.tag), "browser_download_url": f.server.URL + "/download/archive"},
					{"name": "checksums.txt", "browser_download_url": f.server.URL + "/download/checksums"},
				},
			})
		case "/download/archive":
			_, _ = w.Write(f.archive)
		case "/download/checksums":
			_, _ = w.Write(f.checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// paths returns a snapshot of requested server paths.
func (f *fakeGitHub) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.path...)
}

// archiveName returns the expected release asset name for a tag.
func archiveName(tag string) string {
	return "wormhole_" + strings.TrimPrefix(tag, "v") + "_linux_amd64.tar.gz"
}

// makeArchive builds an in-memory tar.gz update archive.
func makeArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	compressed := gzip.NewWriter(&buf)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Name:     entry.name,
			Typeflag: typeflag,
			Mode:     mode,
			Size:     int64(len(entry.body)),
			Linkname: entry.linkname,
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testExecutable creates an installed binary and separate update-state directory.
func testExecutable(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, binaryName)
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, filepath.Join(root, "state")
}

// newTestUpdater constructs an updater against a fake GitHub release service.
func newTestUpdater(t *testing.T, f *fakeGitHub, exe, stateDir string, now *time.Time) *Updater {
	t.Helper()
	opts := Options{
		Owner:          "acme",
		Repo:           binaryName,
		APIBase:        f.server.URL,
		StateDir:       stateDir,
		ExecutablePath: exe,
		GOOS:           "linux",
		GOARCH:         "amd64",
	}
	if now != nil {
		opts.Now = func() time.Time { return *now }
	}
	u, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// readFile returns the contents of a test file as a string.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestAutomaticOptInThrottle verifies opt-in, update, and throttling behavior.
func TestAutomaticOptInThrottle(t *testing.T) {
	now := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	archive := makeArchive(t, tarEntry{name: binaryName, body: "new binary"})
	f := newFakeGitHub(t, "v1.1.0", archive)
	exe, stateDir := testExecutable(t)
	u := newTestUpdater(t, f, exe, stateDir, &now)

	if u.Enabled() {
		t.Fatal("automatic updates enabled by default")
	}
	result, err := u.CheckAndUpdate(context.Background(), "v1.0.0")
	if err != nil || result != (Result{}) {
		t.Fatalf("disabled result = %+v, err = %v", result, err)
	}
	if len(f.paths()) != 0 {
		t.Fatal("disabled updater made an HTTP request")
	}
	if err := u.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	result, err = u.CheckAndUpdate(context.Background(), "v1.0.0")
	if err != nil || !result.Checked || !result.Updated {
		t.Fatalf("first result = %+v, err = %v", result, err)
	}
	firstRequests := len(f.paths())
	if firstRequests != 3 {
		t.Fatalf("first check made %d requests, want 3", firstRequests)
	}
	result, err = u.CheckAndUpdate(context.Background(), "v1.0.0")
	if err != nil || result != (Result{}) {
		t.Fatalf("throttled result = %+v, err = %v", result, err)
	}
	if len(f.paths()) != firstRequests {
		t.Fatal("throttled updater made an HTTP request")
	}
	now = now.Add(CheckInterval)
	result, err = u.CheckAndUpdate(context.Background(), "v1.1.0")
	if err != nil || !result.Checked || result.Updated || result.Latest != "1.1.0" || result.Tag != "v1.1.0" {
		t.Fatalf("next result = %+v, err = %v", result, err)
	}
	if len(f.paths()) != firstRequests+1 {
		t.Fatal("check did not run at the throttle boundary")
	}
}

// TestAutomaticEnvironmentOverride verifies environment-based suppression of automatic checks.
func TestAutomaticEnvironmentOverride(t *testing.T) {
	for _, test := range []struct {
		value    string
		disabled bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"1", true},
		{"true", true},
		{"no", true},
		{"off", true},
	} {
		t.Run(fmt.Sprintf("value_%q", test.value), func(t *testing.T) {
			t.Setenv("WORMHOLE_NO_UPDATE_CHECK", test.value)
			archive := makeArchive(t, tarEntry{name: binaryName, body: "new"})
			f := newFakeGitHub(t, "v1.0.0", archive)
			exe, stateDir := testExecutable(t)
			u := newTestUpdater(t, f, exe, stateDir, nil)
			if err := u.SetEnabled(true); err != nil {
				t.Fatal(err)
			}
			result, err := u.CheckAndUpdate(context.Background(), "v1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if test.disabled && (result.Checked || len(f.paths()) != 0) {
				t.Fatalf("environment did not disable check: %+v, paths %v", result, f.paths())
			}
			if !test.disabled && (!result.Checked || len(f.paths()) != 1) {
				t.Fatalf("environment unexpectedly disabled check: %+v, paths %v", result, f.paths())
			}
		})
	}
}

// TestNonReleaseVersions verifies that development and malformed versions cannot update.
func TestNonReleaseVersions(t *testing.T) {
	archive := makeArchive(t, tarEntry{name: binaryName, body: "new"})
	f := newFakeGitHub(t, "v2.0.0", archive)
	exe, stateDir := testExecutable(t)
	u := newTestUpdater(t, f, exe, stateDir, nil)
	if err := u.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", "dev", "DEV", "(devel)", "nightly", "1.0", "v1.2.3.4", "v0.3.0-7-g439e102"} {
		result, err := u.CheckAndUpdate(context.Background(), version)
		if err != nil || result != (Result{}) {
			t.Errorf("automatic %q: result %+v, err %v", version, result, err)
		}
	}
	if len(f.paths()) != 0 {
		t.Fatal("non-release automatic version made an HTTP request")
	}
	for _, version := range []string{"", "dev", "(devel)", "v0.3.0-7-g439e102"} {
		if _, err := u.ForceUpdate(context.Background(), version); !errors.Is(err, ErrDevelopmentVersion) {
			t.Errorf("manual %q error = %v", version, err)
		}
	}
	if _, err := u.ForceUpdate(context.Background(), "nightly"); err == nil || !strings.Contains(err.Error(), "not a tagged semantic version") {
		t.Fatalf("manual invalid version error = %v", err)
	}
	if readFile(t, exe) != "old binary" {
		t.Fatal("non-release check changed executable")
	}
}

// TestAutomaticRecordsFailedAttempts verifies failed checks are throttled and persisted.
func TestAutomaticRecordsFailedAttempts(t *testing.T) {
	now := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	f := newFakeGitHub(t, "v2.0.0", nil)
	f.latestStatus = http.StatusBadGateway
	exe, stateDir := testExecutable(t)
	u := newTestUpdater(t, f, exe, stateDir, &now)
	if err := u.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	result, err := u.CheckAndUpdate(context.Background(), "v1.0.0")
	if err == nil || !result.Checked {
		t.Fatalf("failed check result = %+v, err = %v", result, err)
	}
	now = now.Add(time.Hour)
	result, err = u.CheckAndUpdate(context.Background(), "v1.0.0")
	if err != nil || result.Checked || len(f.paths()) != 1 {
		t.Fatalf("failed attempt was not throttled: %+v, %v, paths %v", result, err, f.paths())
	}
	reloaded := newTestUpdater(t, f, exe, stateDir, &now)
	result, err = reloaded.CheckAndUpdate(context.Background(), "v1.0.0")
	if err != nil || result.Checked || len(f.paths()) != 1 {
		t.Fatalf("failed attempt was not persisted: %+v, %v", result, err)
	}
	now = now.Add(CheckInterval - time.Hour)
	_, err = reloaded.CheckAndUpdate(context.Background(), "v1.0.0")
	if err == nil || len(f.paths()) != 2 {
		t.Fatalf("retry after interval: err %v, paths %v", err, f.paths())
	}
}

// TestForceBypassesPolicy verifies manual checks ignore automatic-update policy.
func TestForceBypassesPolicy(t *testing.T) {
	t.Setenv("WORMHOLE_NO_UPDATE_CHECK", "1")
	archive := makeArchive(t, tarEntry{name: binaryName, body: "new"})
	f := newFakeGitHub(t, "v1.0.0", archive)
	exe, stateDir := testExecutable(t)
	u := newTestUpdater(t, f, exe, stateDir, nil)
	if err := u.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := u.CheckAndUpdate(context.Background(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORMHOLE_NO_UPDATE_CHECK", "0")
	if _, err := u.CheckAndUpdate(context.Background(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := u.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORMHOLE_NO_UPDATE_CHECK", "1")
	result, err := u.ForceUpdate(context.Background(), "v1.0.0")
	if err != nil || !result.Checked || result.Updated || len(f.paths()) != 2 {
		t.Fatalf("forced result = %+v, err = %v, paths = %v", result, err, f.paths())
	}
}

func TestChecksumMismatchLeavesExecutableUnchanged(t *testing.T) {
	archive := makeArchive(t, tarEntry{name: binaryName, body: "new binary"})
	f := newFakeGitHub(t, "v1.1.0", archive)
	f.checksums = []byte(fmt.Sprintf("%064d  %s\n", 0, archiveName(f.tag)))
	exe, stateDir := testExecutable(t)
	u := newTestUpdater(t, f, exe, stateDir, nil)
	result, err := u.ForceUpdate(context.Background(), "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("checksum error = %v", err)
	}
	if result.Updated || readFile(t, exe) != "old binary" {
		t.Fatalf("checksum failure changed executable: %+v", result)
	}
	assertNoTemporaryFiles(t, filepath.Dir(exe))
}

func TestSuccessfulReplacementResolvesSymlink(t *testing.T) {
	archive := makeArchive(t,
		tarEntry{name: "README.md", body: "documentation"},
		tarEntry{name: "./wormhole", body: "new binary", mode: 0o600},
		tarEntry{name: "examples/link", typeflag: tar.TypeSymlink, linkname: "../README.md"},
	)
	f := newFakeGitHub(t, "v1.10.0", archive)
	exe, stateDir := testExecutable(t)
	link := filepath.Join(t.TempDir(), "wormhole-link")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	u := newTestUpdater(t, f, link, stateDir, nil)
	result, err := u.ForceUpdate(context.Background(), "v1.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if result != (Result{Checked: true, Latest: "1.10.0", Updated: true, Tag: "v1.10.0"}) {
		t.Fatalf("result = %+v", result)
	}
	if got := readFile(t, exe); got != "new binary" {
		t.Fatalf("executable content = %q", got)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode = %o", info.Mode().Perm())
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("executable symlink was replaced")
	}
	assertNoTemporaryFiles(t, filepath.Dir(exe))
}

func TestStateModesAndPersistence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission modes")
	}
	exe, stateDir := testExecutable(t)
	f := newFakeGitHub(t, "v1.0.0", nil)
	u := newTestUpdater(t, f, exe, stateDir, nil)
	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode = %o", info.Mode().Perm())
	}
	if err := u.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(stateDir, stateFileName)
	info, err = os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o", info.Mode().Perm())
	}
	reloaded := newTestUpdater(t, f, exe, stateDir, nil)
	if !reloaded.Enabled() {
		t.Fatal("enabled state was not persisted")
	}
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(statePath, 0o644); err != nil {
		t.Fatal(err)
	}
	newTestUpdater(t, f, exe, stateDir, nil)
	dirInfo, _ := os.Stat(stateDir)
	fileInfo, _ := os.Stat(statePath)
	if dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("reload modes: dir %o, file %o", dirInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func TestBadArchivesLeaveExecutableUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		archive func(*testing.T) []byte
	}{
		{"not gzip", func(t *testing.T) []byte { return []byte("not an archive") }},
		{"missing executable", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: "README.md", body: "docs"})
		}},
		{"path traversal", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: "../wormhole", body: "evil"})
		}},
		{"absolute path", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: "/wormhole", body: "evil"})
		}},
		{"symlink executable", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: binaryName, typeflag: tar.TypeSymlink, linkname: "/tmp/evil"})
		}},
		{"directory executable", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: binaryName, typeflag: tar.TypeDir})
		}},
		{"duplicate executable", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: binaryName, body: "first"}, tarEntry{name: "./wormhole", body: "second"})
		}},
		{"nested executable only", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: "bin/wormhole", body: "evil"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := test.archive(t)
			f := newFakeGitHub(t, "v2.0.0", archive)
			exe, stateDir := testExecutable(t)
			u := newTestUpdater(t, f, exe, stateDir, nil)
			result, err := u.ForceUpdate(context.Background(), "v1.0.0")
			if err == nil {
				t.Fatal("bad archive succeeded")
			}
			if result.Updated || readFile(t, exe) != "old binary" {
				t.Fatalf("bad archive changed executable: %+v", result)
			}
			assertNoTemporaryFiles(t, filepath.Dir(exe))
		})
	}
}

func TestUnwritableDestinationLeavesExecutableUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires a non-root Unix user")
	}
	archive := makeArchive(t, tarEntry{name: binaryName, body: "new"})
	f := newFakeGitHub(t, "v2.0.0", archive)
	exe, stateDir := testExecutable(t)
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	u := newTestUpdater(t, f, exe, stateDir, nil)
	result, err := u.ForceUpdate(context.Background(), "v1.0.0")
	if err == nil {
		t.Fatal("update in unwritable directory succeeded")
	}
	if result.Updated || readFile(t, exe) != "old binary" {
		t.Fatalf("unwritable destination changed executable: %+v", result)
	}
}

func TestSemanticVersionOrdering(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"1.9.0",
		"1.10.0",
		"999999999999999999999999.0.0",
	}
	for i := 1; i < len(ordered); i++ {
		a, err := parseSemanticVersion(ordered[i-1])
		if err != nil {
			t.Fatal(err)
		}
		b, err := parseSemanticVersion("v" + ordered[i])
		if err != nil {
			t.Fatal(err)
		}
		if compareSemanticVersions(a, b) >= 0 || compareSemanticVersions(b, a) <= 0 {
			t.Errorf("incorrect ordering: %q < %q", ordered[i-1], ordered[i])
		}
	}
	a, _ := parseSemanticVersion("v1.2.3+one")
	b, _ := parseSemanticVersion("1.2.3+two")
	if compareSemanticVersions(a, b) != 0 {
		t.Fatal("build metadata affected precedence")
	}
	for _, invalid := range []string{"1.2", "1.2.3.4", "01.2.3", "1.2.3-01", "1.2.3-", "1.2.3+"} {
		if _, err := parseSemanticVersion(invalid); err == nil {
			t.Errorf("accepted invalid semantic version %q", invalid)
		}
	}
}

// assertNoTemporaryFiles checks that update cleanup removed staging files.
func assertNoTemporaryFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".wormhole-download-") || strings.HasPrefix(entry.Name(), ".wormhole-update-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

// TestNewValidationAndDefaults verifies updater option validation and runtime defaults.
func TestNewValidationAndDefaults(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted an empty state directory")
	}
	for _, apiBase := range []string{"://", "relative/path", "ftp://example.com"} {
		t.Run(apiBase, func(t *testing.T) {
			if _, err := New(Options{StateDir: t.TempDir(), APIBase: apiBase}); err == nil {
				t.Fatalf("New accepted API base %q", apiBase)
			}
		})
	}

	u, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if u.owner != defaultOwner || u.repo != defaultRepo || u.apiBase != defaultAPIBase {
		t.Fatalf("repository defaults = %q/%s at %q", u.owner, u.repo, u.apiBase)
	}
	if u.client == nil || u.now == nil || u.goos != runtime.GOOS || u.goarch != runtime.GOARCH {
		t.Fatalf("runtime defaults were not applied: %+v", u)
	}
}

// TestStateValidationFailures verifies corrupt state and persistence failures are reported.
func TestStateValidationFailures(t *testing.T) {
	t.Run("state path is directory", func(t *testing.T) {
		stateDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(stateDir, stateFileName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Options{StateDir: stateDir}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("New error = %v", err)
		}
	})
	t.Run("malformed state", func(t *testing.T) {
		stateDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(stateDir, stateFileName), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Options{StateDir: stateDir}); err == nil || !strings.Contains(err.Error(), "decode update state") {
			t.Fatalf("New error = %v", err)
		}
	})
	t.Run("state directory is file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state")
		if err := os.WriteFile(path, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Options{StateDir: path}); err == nil {
			t.Fatal("New accepted a file as the state directory")
		}
	})
	t.Run("save failure", func(t *testing.T) {
		stateDir := t.TempDir()
		u, err := New(Options{StateDir: stateDir})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(u.statePath(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := u.SetEnabled(true); err == nil {
			t.Fatal("SetEnabled succeeded with a directory at the state path")
		}
	})
	t.Run("automatic attempt save failure", func(t *testing.T) {
		stateDir := t.TempDir()
		u, err := New(Options{StateDir: stateDir})
		if err != nil {
			t.Fatal(err)
		}
		if err := u.SetEnabled(true); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(u.statePath()); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(u.statePath(), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := u.CheckAndUpdate(context.Background(), "v1.0.0"); err == nil || !strings.Contains(err.Error(), "record update check") {
			t.Fatalf("CheckAndUpdate error = %v", err)
		}
	})
}

// TestLatestReleaseValidation verifies malformed and non-release metadata is rejected.
func TestLatestReleaseValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed JSON", "not json"},
		{"draft", `{"tag_name":"v1.0.0","draft":true}`},
		{"prerelease", `{"tag_name":"v1.0.0","prerelease":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			u := &Updater{apiBase: server.URL, client: server.Client()}
			if _, err := u.latestRelease(context.Background()); err == nil {
				t.Fatal("latestRelease accepted invalid release metadata")
			}
		})
	}
}

// TestReleaseAssetURLValidation verifies asset selection and URL safety checks.
func TestReleaseAssetURLValidation(t *testing.T) {
	const name = "archive.tar.gz"
	tests := []struct {
		name    string
		release release
	}{
		{"missing", release{}},
		{"duplicate", release{Assets: []asset{{Name: name, URL: "https://example.com/one"}, {Name: name, URL: "https://example.com/two"}}}},
		{"invalid URL", release{Assets: []asset{{Name: name, URL: "://"}}}},
		{"relative URL", release{Assets: []asset{{Name: name, URL: "/download"}}}},
		{"unsupported scheme", release{Assets: []asset{{Name: name, URL: "ftp://example.com/archive"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.release.assetURL(name); err == nil {
				t.Fatal("assetURL accepted invalid assets")
			}
		})
	}
}

// TestDownloadAndExecutableValidation verifies download limits, HTTP errors, and executable checks.
func TestDownloadAndExecutableValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/large":
			_, _ = w.Write([]byte("too large"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	u := &Updater{client: server.Client()}
	if _, err := u.downloadBytes(context.Background(), server.URL+"/large", 4); err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Fatalf("downloadBytes error = %v", err)
	}
	if _, err := u.get(context.Background(), server.URL+"/missing"); err == nil {
		t.Fatal("get accepted a non-successful response")
	}
	if _, err := u.get(context.Background(), "://"); err == nil {
		t.Fatal("get accepted an invalid URL")
	}

	if _, err := (&Updater{exePath: filepath.Join(t.TempDir(), "missing")}).executable(); err == nil {
		t.Fatal("executable accepted a missing file")
	}
	if _, err := (&Updater{exePath: t.TempDir()}).executable(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("executable directory error = %v", err)
	}
	if _, err := (&Updater{}).executable(); err != nil {
		t.Fatalf("executable could not resolve the test process: %v", err)
	}
}

// TestChecksumValidation verifies checksum parsing and digest normalization.
func TestChecksumValidation(t *testing.T) {
	const name = "archive.tar.gz"
	const validHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name string
		data string
	}{
		{"missing", "not a checksum\n"},
		{"other archive", validHash + "  other.tar.gz\n"},
		{"short hash", "abc  " + name + "\n"},
		{"invalid hash", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz  " + name + "\n"},
		{"duplicate", validHash + "  " + name + "\n" + validHash + " *" + name + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := checksumFor([]byte(test.data), name); err == nil {
				t.Fatal("checksumFor accepted invalid checksums")
			}
		})
	}
	got, err := checksumFor([]byte(strings.ToUpper(validHash)+" *"+name+"\n"), name)
	if err != nil || got != validHash {
		t.Fatalf("checksumFor = %q, %v", got, err)
	}
}

// TestArchiveAndSemanticVersionEdgeCases verifies malformed archives and version identifiers fail safely.
func TestArchiveAndSemanticVersionEdgeCases(t *testing.T) {
	if _, err := installArchive(filepath.Join(t.TempDir(), "missing.tar.gz"), filepath.Join(t.TempDir(), binaryName)); err == nil {
		t.Fatal("installArchive opened a missing archive")
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte("not a tar archive")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	archivePath := filepath.Join(root, "bad.tar.gz")
	if err := os.WriteFile(archivePath, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installArchive(archivePath, filepath.Join(root, binaryName)); err == nil {
		t.Fatal("installArchive accepted malformed tar data")
	}
	if err := syncDir(filepath.Join(root, "missing")); err == nil {
		t.Fatal("syncDir opened a missing directory")
	}
	for _, invalid := range []string{"1.2.3-alpha_1", "1.2.3+build_1", "1.2.3-alpha..1"} {
		if _, err := parseSemanticVersion(invalid); err == nil {
			t.Errorf("parseSemanticVersion accepted %q", invalid)
		}
	}
	if isNumeric("") {
		t.Fatal("isNumeric accepted an empty string")
	}
}

// TestUpdateReleaseValidation verifies invalid release tags and unsupported platforms are rejected.
func TestUpdateReleaseValidation(t *testing.T) {
	archive := makeArchive(t, tarEntry{name: binaryName, body: "new"})
	exe, stateDir := testExecutable(t)
	f := newFakeGitHub(t, "not-semver", archive)
	u := newTestUpdater(t, f, exe, stateDir, nil)
	if _, err := u.ForceUpdate(context.Background(), "v1.0.0"); err == nil || !strings.Contains(err.Error(), "latest release tag") {
		t.Fatalf("invalid latest tag error = %v", err)
	}

	f = newFakeGitHub(t, "v2.0.0", archive)
	u = newTestUpdater(t, f, exe, stateDir, nil)
	u.goos = "plan9"
	if _, err := u.ForceUpdate(context.Background(), "v1.0.0"); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("unsupported platform error = %v", err)
	}
}
