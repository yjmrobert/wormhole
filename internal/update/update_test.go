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

func (f *fakeGitHub) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.path...)
}

func archiveName(tag string) string {
	return "wormhole_" + strings.TrimPrefix(tag, "v") + "_linux_amd64.tar.gz"
}

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

func testExecutable(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "wormhole")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, filepath.Join(root, "state")
}

func newTestUpdater(t *testing.T, f *fakeGitHub, exe, stateDir string, now *time.Time) *Updater {
	t.Helper()
	opts := Options{
		Owner:          "acme",
		Repo:           "wormhole",
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAutomaticOptInThrottle(t *testing.T) {
	now := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	archive := makeArchive(t, tarEntry{name: "wormhole", body: "new binary"})
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
			archive := makeArchive(t, tarEntry{name: "wormhole", body: "new"})
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

func TestNonReleaseVersions(t *testing.T) {
	archive := makeArchive(t, tarEntry{name: "wormhole", body: "new"})
	f := newFakeGitHub(t, "v2.0.0", archive)
	exe, stateDir := testExecutable(t)
	u := newTestUpdater(t, f, exe, stateDir, nil)
	if err := u.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", "dev", "DEV", "(devel)", "nightly", "1.0", "v1.2.3.4"} {
		result, err := u.CheckAndUpdate(context.Background(), version)
		if err != nil || result != (Result{}) {
			t.Errorf("automatic %q: result %+v, err %v", version, result, err)
		}
	}
	if len(f.paths()) != 0 {
		t.Fatal("non-release automatic version made an HTTP request")
	}
	for _, version := range []string{"", "dev", "(devel)"} {
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

func TestForceBypassesPolicy(t *testing.T) {
	t.Setenv("WORMHOLE_NO_UPDATE_CHECK", "1")
	archive := makeArchive(t, tarEntry{name: "wormhole", body: "new"})
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
	archive := makeArchive(t, tarEntry{name: "wormhole", body: "new binary"})
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
			return makeArchive(t, tarEntry{name: "wormhole", typeflag: tar.TypeSymlink, linkname: "/tmp/evil"})
		}},
		{"directory executable", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: "wormhole", typeflag: tar.TypeDir})
		}},
		{"duplicate executable", func(t *testing.T) []byte {
			return makeArchive(t, tarEntry{name: "wormhole", body: "first"}, tarEntry{name: "./wormhole", body: "second"})
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
	archive := makeArchive(t, tarEntry{name: "wormhole", body: "new"})
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
