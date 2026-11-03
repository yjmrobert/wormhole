// Package update checks public GitHub releases and atomically replaces the
// running Wormhole executable after verifying its published SHA-256 checksum.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// CheckInterval is the minimum time between automatic check attempts.
	CheckInterval = 24 * time.Hour

	defaultOwner          = "yjmrobert"
	defaultRepo           = binaryName
	defaultAPIBase        = "https://api.github.com"
	binaryName            = "wormhole"
	maxBinarySize         = 256 << 20
	checksumsFileName     = "checksums.txt"
	developmentVersion    = "dev"
	developmentVersionAlt = "(devel)"
	noUpdateCheckEnv      = "WORMHOLE_NO_UPDATE_CHECK"
	httpScheme            = "http"
	httpsScheme           = "https"
	userAgent             = "wormhole-self-update"
	userAgentHeader       = "User-Agent"
	versionPrefix         = "v"
)

var (
	// ErrDevelopmentVersion reports that a manual update was requested from a
	// build whose version cannot be ordered against tagged releases.
	ErrDevelopmentVersion = errors.New("cannot update a development version")
	// ErrUnsupportedPlatform reports that no release archive is built for the
	// configured operating system or architecture.
	ErrUnsupportedPlatform = errors.New("self-update is unsupported on this platform")
)

// Options configures an Updater. StateDir is required. The zero values for
// the other fields use the public Wormhole repository and runtime defaults.
type Options struct {
	Owner    string
	Repo     string
	APIBase  string
	StateDir string

	HTTPClient     *http.Client
	Now            func() time.Time
	ExecutablePath string
	GOOS           string
	GOARCH         string
}

// Result describes a check or update. Checked reports that a release check
// was attempted rather than skipped. Latest is the latest semantic version
// without a leading "v", and Tag is its exact GitHub release tag.
type Result struct {
	Checked bool
	Latest  string
	Updated bool
	Tag     string
}

// Updater is safe for concurrent use within a process.
type Updater struct {
	mu sync.Mutex

	owner    string
	repo     string
	apiBase  string
	stateDir string
	client   *http.Client
	now      func() time.Time
	exePath  string
	goos     string
	goarch   string
	state    state
}

// New constructs an Updater and loads its persistent state.
func New(opts Options) (*Updater, error) {
	if strings.TrimSpace(opts.StateDir) == "" {
		return nil, errors.New("update: StateDir is required")
	}
	if opts.Owner == "" {
		opts.Owner = defaultOwner
	}
	if opts.Repo == "" {
		opts.Repo = defaultRepo
	}
	if opts.APIBase == "" {
		opts.APIBase = defaultAPIBase
	}
	base, err := url.Parse(opts.APIBase)
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != httpScheme && base.Scheme != httpsScheme) {
		return nil, fmt.Errorf("update: invalid API base URL %q", opts.APIBase)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	u := &Updater{
		owner:    opts.Owner,
		repo:     opts.Repo,
		apiBase:  strings.TrimRight(opts.APIBase, "/"),
		stateDir: opts.StateDir,
		client:   opts.HTTPClient,
		now:      opts.Now,
		exePath:  opts.ExecutablePath,
		goos:     opts.GOOS,
		goarch:   opts.GOARCH,
	}
	if err := u.loadState(); err != nil {
		return nil, err
	}
	return u, nil
}

// Enabled reports whether automatic update checks are opted in. It does not
// account for WORMHOLE_NO_UPDATE_CHECK, which only suppresses automatic calls.
func (u *Updater) Enabled() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state.Enabled
}

// SetEnabled persists the automatic update opt-in preference.
func (u *Updater) SetEnabled(enabled bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.state.Enabled = enabled
	if err := u.saveState(); err != nil {
		return fmt.Errorf("save update state: %w", err)
	}
	return nil
}

// CheckAndUpdate performs an opted-in automatic check. Disabled, throttled,
// environment-suppressed, and non-release builds return an unchecked result.
// A failed attempt is timestamped so it is not retried until CheckInterval.
func (u *Updater) CheckAndUpdate(ctx context.Context, currentVersion string) (Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !u.state.Enabled || AutomaticChecksDisabled() {
		return Result{}, nil
	}
	if IsDevelopmentVersion(currentVersion) {
		return Result{}, nil
	}
	if _, err := parseSemanticVersion(currentVersion); err != nil {
		return Result{}, nil
	}
	now := u.now()
	if !u.state.LastCheck.IsZero() && now.Before(u.state.LastCheck.Add(CheckInterval)) {
		return Result{}, nil
	}
	u.state.LastCheck = now
	if err := u.saveState(); err != nil {
		return Result{}, fmt.Errorf("record update check: %w", err)
	}
	return u.checkAndUpdate(ctx, currentVersion)
}

// ForceUpdate checks and updates regardless of the opt-in preference,
// throttle, or automatic-check environment override.
func (u *Updater) ForceUpdate(ctx context.Context, currentVersion string) (Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	trimmed := strings.TrimSpace(currentVersion)
	if IsDevelopmentVersion(trimmed) {
		shown := trimmed
		if shown == "" {
			shown = "empty"
		}
		return Result{}, fmt.Errorf("%w %q; use a tagged release build for self-update", ErrDevelopmentVersion, shown)
	}
	if _, err := parseSemanticVersion(currentVersion); err != nil {
		return Result{}, fmt.Errorf("current version %q is not a tagged semantic version: %w", currentVersion, err)
	}
	return u.checkAndUpdate(ctx, currentVersion)
}

// AutomaticChecksDisabled reports whether the environment suppresses automatic checks.
func AutomaticChecksDisabled() bool {
	value := strings.TrimSpace(os.Getenv(noUpdateCheckEnv))
	return value != "" && value != "0" && !strings.EqualFold(value, "false")
}

// IsDevelopmentVersion reports whether a version cannot be ordered against releases.
func IsDevelopmentVersion(version string) bool {
	trimmed := strings.TrimSpace(version)
	return trimmed == "" || strings.EqualFold(trimmed, developmentVersion) || strings.EqualFold(trimmed, developmentVersionAlt) || strings.Contains(trimmed, "-")
}

// checkAndUpdate downloads and installs a newer release after validating its metadata and checksum.
func (u *Updater) checkAndUpdate(ctx context.Context, currentVersion string) (Result, error) {
	current, _ := parseSemanticVersion(currentVersion)
	result := Result{Checked: true}
	release, err := u.latestRelease(ctx)
	if err != nil {
		return result, err
	}
	result.Tag = release.TagName
	latest, err := parseSemanticVersion(release.TagName)
	if err != nil {
		return result, fmt.Errorf("latest release tag %q is not semantic version: %w", release.TagName, err)
	}
	result.Latest = strings.TrimPrefix(strings.TrimSpace(release.TagName), versionPrefix)
	if compareSemanticVersions(current, latest) >= 0 {
		return result, nil
	}
	if u.goos != "linux" || (u.goarch != "amd64" && u.goarch != "arm64") {
		return result, fmt.Errorf("%w: %s/%s", ErrUnsupportedPlatform, u.goos, u.goarch)
	}
	exe, err := u.executable()
	if err != nil {
		return result, err
	}
	archiveName := binaryName + "_" + result.Latest + "_" + u.goos + "_" + u.goarch + ".tar.gz"
	archiveURL, err := release.assetURL(archiveName)
	if err != nil {
		return result, err
	}
	checksumsURL, err := release.assetURL(checksumsFileName)
	if err != nil {
		return result, err
	}
	checksums, err := u.downloadBytes(ctx, checksumsURL, 1<<20)
	if err != nil {
		return result, fmt.Errorf("download checksums: %w", err)
	}
	wantHash, err := checksumFor(checksums, archiveName)
	if err != nil {
		return result, err
	}
	archive, err := os.CreateTemp(filepath.Dir(exe), ".wormhole-download-*")
	if err != nil {
		return result, fmt.Errorf("create download in destination: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	gotHash, err := u.downloadArchive(ctx, archiveURL, archive)
	if err != nil {
		archive.Close()
		return result, fmt.Errorf("download archive: %w", err)
	}
	if gotHash != wantHash {
		return result, fmt.Errorf("archive SHA-256 mismatch for %s", archiveName)
	}
	applied, err := installArchive(archivePath, exe)
	result.Updated = applied
	if err != nil {
		return result, fmt.Errorf("install update: %w", err)
	}
	return result, nil
}

type release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// assetURL returns the unique HTTP(S) download URL for a named release asset.
func (r release) assetURL(name string) (string, error) {
	var found string
	for _, a := range r.Assets {
		if a.Name != name {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("release contains duplicate asset %q", name)
		}
		found = a.URL
	}
	if found == "" {
		return "", fmt.Errorf("release is missing asset %q", name)
	}
	parsed, err := url.Parse(found)
	if err != nil || parsed.Host == "" || (parsed.Scheme != httpScheme && parsed.Scheme != httpsScheme) {
		return "", fmt.Errorf("release asset %q has invalid download URL", name)
	}
	return found, nil
}

// latestRelease fetches and validates the repository's latest stable release metadata.
func (u *Updater) latestRelease(ctx context.Context) (release, error) {
	endpoint := u.apiBase + "/repos/" + url.PathEscape(u.owner) + "/" + url.PathEscape(u.repo) + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return release{}, fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set(userAgentHeader, userAgent)
	resp, err := u.client.Do(req)
	if err != nil {
		return release{}, fmt.Errorf("check latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("check latest release: GitHub returned %s", resp.Status)
	}
	var r release
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err := decoder.Decode(&r); err != nil {
		return release{}, fmt.Errorf("decode latest release: %w", err)
	}
	if r.Draft || r.Prerelease {
		return release{}, errors.New("latest release is a draft or prerelease")
	}
	return r, nil
}

// downloadBytes retrieves a response body while enforcing a maximum size.
func (u *Updater) downloadBytes(ctx context.Context, source string, limit int64) ([]byte, error) {
	resp, err := u.get(ctx, source)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

// downloadArchive streams an archive to dst and returns its SHA-256 digest.
func (u *Updater) downloadArchive(ctx context.Context, source string, dst *os.File) (string, error) {
	resp, err := u.get(ctx, source)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	hash := sha256.New()
	n, err := io.Copy(dst, io.TeeReader(io.LimitReader(resp.Body, maxBinarySize+1), hash))
	if err != nil {
		return "", err
	}
	if n > maxBinarySize {
		return "", fmt.Errorf("archive exceeds %d bytes", maxBinarySize)
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// get performs an update download request and requires a successful HTTP response.
func (u *Updater) get(ctx context.Context, source string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(userAgentHeader, userAgent)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}
	return resp, nil
}

// executable resolves the configured or running executable and requires a regular file.
func (u *Updater) executable() (string, error) {
	name := u.exePath
	if name == "" {
		var err error
		name, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate executable: %w", err)
		}
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("executable %q is not a regular file", resolved)
	}
	return resolved, nil
}

// checksumFor finds and validates the unique SHA-256 entry for an archive.
func checksumFor(data []byte, name string) (string, error) {
	var found string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		candidate := strings.TrimPrefix(fields[len(fields)-1], "*")
		if candidate != name {
			continue
		}
		hash := strings.ToLower(fields[0])
		if len(hash) != sha256.Size*2 {
			return "", fmt.Errorf("invalid SHA-256 checksum for %s", name)
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return "", fmt.Errorf("invalid SHA-256 checksum for %s: %w", name, err)
		}
		if found != "" {
			return "", fmt.Errorf("duplicate SHA-256 checksum for %s", name)
		}
		found = hash
	}
	if found == "" {
		return "", fmt.Errorf("%s has no entry for %s", checksumsFileName, name)
	}
	return found, nil
}
