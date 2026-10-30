// Package config loads and validates ~/.config/wormhole/config.yaml and
// enforces the leak guards that keep a deployment's private material out of
// the public repository: a world-readable config or a credentials file that
// lives inside a git working tree is refused outright.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// Delivery modes for inbound asks from a sender.
const (
	ModeInbox = "inbox"
	ModeAuto  = "auto"
)

// Defaults.
const (
	DefaultPerSenderPerDay  = 20
	DefaultPerDay           = 50
	DefaultRegistryInterval = 5 * time.Minute
	DefaultCopilotBinary    = "copilot"
	DefaultBusKind          = "nats"
	DefaultNakBackoff       = 30 * time.Second
)

// Config is the whole file.
type Config struct {
	// Handle is this user's wormhole handle. It must equal the bus user the
	// credentials authenticate as.
	Handle  string            `yaml:"handle"`
	Bus     Bus               `yaml:"bus"`
	Senders []Sender          `yaml:"senders"`
	Limits  Limits            `yaml:"limits"`
	Copilot Copilot           `yaml:"copilot"`
	Repos   map[string]string `yaml:"repos"`
	Daemon  Daemon            `yaml:"daemon"`

	// Path the config was loaded from (not in the file).
	Path string `yaml:"-"`
}

// Bus is how to reach the message bus.
type Bus struct {
	Kind string `yaml:"kind"`
	URL  string `yaml:"url"`
	// Creds is a NATS .creds file (JWT + nkey seed), the M4 auth scheme.
	Creds string `yaml:"creds"`
	// User and PasswordFile are the v0 static-user scheme.
	User         string `yaml:"user"`
	PasswordFile string `yaml:"password_file"`
	// TLSCA is a PEM bundle to trust for the bus connection.
	TLSCA string `yaml:"tls_ca"`
}

// Sender is one allowlisted peer. In YAML it may be a bare handle (inbox
// mode) or a mapping with an explicit mode.
type Sender struct {
	Handle string `yaml:"handle"`
	Mode   string `yaml:"mode"`
}

// UnmarshalYAML accepts both `- alice` and `- {handle: alice, mode: auto}`.
func (s *Sender) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Handle = n.Value
		s.Mode = ModeInbox
		return nil
	}
	type plain Sender
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*s = Sender(p)
	if s.Mode == "" {
		s.Mode = ModeInbox
	}
	return nil
}

// Limits are the recipient-side cost and safety caps.
type Limits struct {
	PerSenderPerDay int `yaml:"per_sender_per_day"`
	PerDay          int `yaml:"per_day"`
	MaxHops         int `yaml:"max_hops"`
}

// Copilot configures how sessions are materialized.
type Copilot struct {
	Binary string `yaml:"binary"`
	Model  string `yaml:"model"`
	// Workdir is where sessions start when no repo mapping applies.
	Workdir string `yaml:"workdir"`
	// SafeTools is the --available-tools set for auto mode. The daemon
	// refuses auto mode when it is empty. M0 showed that answering needs
	// the shell tool (the agent runs `wormhole reply`), so the set is not
	// read-only by itself: ExtraArgs carry the permission flags that pin
	// the shell down to that command.
	SafeTools []string `yaml:"safe_tools"`
	// ExtraArgs are appended verbatim to every copilot invocation the
	// daemon makes (and to `wormhole open`): the Copilot CLI's permission
	// flags, model options, anything the installed version needs.
	ExtraArgs []string `yaml:"extra_args"`
}

// Daemon tunes the subscriber.
type Daemon struct {
	RegistryInterval time.Duration `yaml:"registry_interval"`
	NakBackoff       time.Duration `yaml:"nak_backoff"`
	OpenApp          *bool         `yaml:"open_app"`
	Notify           *bool         `yaml:"notify"`
	// ReplyInPlace appends a reply to the session that asked (or to the
	// session already tied to the thread) instead of opening a new
	// "Reply from" session. A new session is still used when the asking
	// session's id is unknown or the resume fails. Off by default: M0
	// showed the appended turn lands in the session's transcript but not
	// in the context of an agent the app is running in that session.
	ReplyInPlace *bool `yaml:"reply_in_place"`
}

// Leak-guard errors.
var (
	ErrWorldReadable = errors.New("config file is readable by other users")
	ErrInGitTree     = errors.New("credentials file is inside a git working tree")
	ErrInvalid       = errors.New("invalid config")
)

// Dir returns the config directory, honoring XDG_CONFIG_HOME.
func Dir() string {
	if d := os.Getenv("WORMHOLE_CONFIG_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "wormhole")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "wormhole")
}

// StateDir returns the state directory, honoring XDG_STATE_HOME.
func StateDir() string {
	if d := os.Getenv("WORMHOLE_STATE_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "wormhole")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "wormhole")
}

// DefaultPath is where Load looks when no path is given.
func DefaultPath() string {
	if p := os.Getenv("WORMHOLE_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(Dir(), "config.yaml")
}

// ThreadsDBPath is the thread map location.
func ThreadsDBPath() string { return filepath.Join(StateDir(), "threads.db") }

// PauseFile is the local kill switch: while it exists the daemon does not
// materialize anything.
func PauseFile() string { return filepath.Join(StateDir(), "paused") }

// Load reads, validates and guards the config at path.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if info.Mode().Perm()&0o004 != 0 {
		return nil, fmt.Errorf("%w: %s has mode %o; run: chmod 600 %s", ErrWorldReadable, path, info.Mode().Perm(), path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	cfg.Path = path
	if err := cfg.guardCredentials(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse decodes and validates YAML without touching the filesystem.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Bus.Kind == "" {
		c.Bus.Kind = DefaultBusKind
	}
	if c.Bus.User == "" && c.Bus.Creds == "" {
		c.Bus.User = c.Handle
	}
	if c.Limits.PerSenderPerDay == 0 {
		c.Limits.PerSenderPerDay = DefaultPerSenderPerDay
	}
	if c.Limits.PerDay == 0 {
		c.Limits.PerDay = DefaultPerDay
	}
	if c.Limits.MaxHops == 0 {
		c.Limits.MaxHops = envelope.DefaultMaxHops
	}
	if c.Copilot.Binary == "" {
		c.Copilot.Binary = DefaultCopilotBinary
	}
	if c.Daemon.RegistryInterval == 0 {
		c.Daemon.RegistryInterval = DefaultRegistryInterval
	}
	if c.Daemon.NakBackoff == 0 {
		c.Daemon.NakBackoff = DefaultNakBackoff
	}
	if c.Daemon.OpenApp == nil {
		c.Daemon.OpenApp = ptr(true)
	}
	if c.Daemon.Notify == nil {
		c.Daemon.Notify = ptr(true)
	}
	if c.Daemon.ReplyInPlace == nil {
		c.Daemon.ReplyInPlace = ptr(false)
	}
	for k, v := range c.Repos {
		c.Repos[k] = Expand(v)
	}
	c.Copilot.Workdir = Expand(c.Copilot.Workdir)
	c.Bus.Creds = Expand(c.Bus.Creds)
	c.Bus.PasswordFile = Expand(c.Bus.PasswordFile)
	c.Bus.TLSCA = Expand(c.Bus.TLSCA)
}

func ptr[T any](v T) *T { return &v }

func (c *Config) validate() error {
	if !envelope.ValidHandle(c.Handle) {
		return fmt.Errorf("%w: handle %q must match %s", ErrInvalid, c.Handle, "^[a-z0-9][a-z0-9_-]{0,63}$")
	}
	switch c.Bus.Kind {
	case "nats", "memory":
	default:
		return fmt.Errorf("%w: bus.kind %q (want nats or memory)", ErrInvalid, c.Bus.Kind)
	}
	if c.Bus.Kind == "nats" && c.Bus.URL == "" {
		return fmt.Errorf("%w: bus.url is required", ErrInvalid)
	}
	if c.Bus.Kind == "nats" && c.Bus.Creds == "" && c.Bus.PasswordFile == "" {
		return fmt.Errorf("%w: bus needs creds or password_file", ErrInvalid)
	}
	if c.Bus.User != "" && c.Bus.User != c.Handle {
		return fmt.Errorf("%w: bus.user %q must equal handle %q (the daemon trusts the bus identity)", ErrInvalid, c.Bus.User, c.Handle)
	}
	seen := map[string]bool{}
	for _, s := range c.Senders {
		if !envelope.ValidHandle(s.Handle) {
			return fmt.Errorf("%w: sender %q is not a valid handle", ErrInvalid, s.Handle)
		}
		if s.Handle == c.Handle {
			return fmt.Errorf("%w: %q cannot allowlist itself", ErrInvalid, s.Handle)
		}
		if seen[s.Handle] {
			return fmt.Errorf("%w: sender %q listed twice", ErrInvalid, s.Handle)
		}
		seen[s.Handle] = true
		if s.Mode != ModeInbox && s.Mode != ModeAuto {
			return fmt.Errorf("%w: sender %q mode %q (want inbox or auto)", ErrInvalid, s.Handle, s.Mode)
		}
		if s.Mode == ModeAuto && len(c.Copilot.SafeTools) == 0 {
			return fmt.Errorf("%w: sender %q uses auto mode but copilot.safe_tools is empty; auto mode needs a read-only tool set", ErrInvalid, s.Handle)
		}
	}
	if c.Limits.PerSenderPerDay < 0 || c.Limits.PerDay < 0 || c.Limits.MaxHops < 1 {
		return fmt.Errorf("%w: limits out of range", ErrInvalid)
	}
	for repo, dir := range c.Repos {
		if !strings.Contains(repo, "/") {
			return fmt.Errorf("%w: repos key %q must be owner/name", ErrInvalid, repo)
		}
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("%w: repos[%q] = %q must be an absolute path", ErrInvalid, repo, dir)
		}
	}
	return nil
}

// guardCredentials refuses credential files that live in a git working
// tree, so that private material cannot drift into a commit.
func (c *Config) guardCredentials() error {
	for _, p := range []string{c.Bus.Creds, c.Bus.PasswordFile} {
		if p == "" {
			continue
		}
		if dir, ok := InGitTree(p); ok {
			return fmt.Errorf("%w: %s is under %s", ErrInGitTree, p, dir)
		}
	}
	return nil
}

// InGitTree walks up from path and reports the repository root if any
// ancestor contains a .git entry.
func InGitTree(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(abs)
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Expand replaces a leading ~ with the home directory.
func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// SenderMode returns the delivery mode for handle, or false if the sender
// is not allowlisted.
func (c *Config) SenderMode(handle string) (string, bool) {
	for _, s := range c.Senders {
		if s.Handle == handle {
			return s.Mode, true
		}
	}
	return "", false
}

// WorkdirFor picks the directory an inbound session starts in: the local
// checkout mapped from repo, else the configured workdir, else the home
// directory.
func (c *Config) WorkdirFor(repo string) string {
	if repo != "" {
		if dir, ok := c.Repos[repo]; ok {
			return dir
		}
	}
	if c.Copilot.Workdir != "" {
		return c.Copilot.Workdir
	}
	home, _ := os.UserHomeDir()
	return home
}

// Example is a complete config with placeholder values, used by `wormhole
// init` and shipped as config.yaml.example.
const Example = `# Wormhole configuration. Keep this file private: chmod 600.
# Placeholders: replace alice/bob and bus.example.internal with your own.

# Your handle. Must equal the bus user your credentials authenticate as.
handle: alice

bus:
  kind: nats
  url: tls://bus.example.internal:4222
  # v0 static users: a file holding only the password, mode 600, outside any git tree.
  password_file: ~/.config/wormhole/password
  # M4 JWT auth: a NATS .creds file instead of password_file.
  # creds: ~/.config/wormhole/alice.creds
  # tls_ca: ~/.config/wormhole/bus-ca.pem

# Who may open a wormhole to you. A bare handle means inbox mode: a person
# reads the message before any tool runs. auto mode runs the kickoff
# headless with the read-only tool set below; opt in per sender.
senders:
  - bob
  # - {handle: carol, mode: auto}

limits:
  per_sender_per_day: 20
  per_day: 50
  max_hops: 6

copilot:
  binary: copilot
  # model: ""            # empty = the app default (admin policy may override)
  workdir: ~/wormhole
  # The --available-tools set for auto mode; empty refuses auto mode.
  # Answering needs the shell tool (the agent runs wormhole reply), so a
  # usable set is not read-only on its own: pin the shell down with the
  # Copilot CLI's permission flags in extra_args. Tested on Copilot CLI
  # 1.0.105: safe_tools [view, grep, glob, bash] plus allow/deny flags that
  # permit only wormhole reply; see docs/design.md, M0 results.
  safe_tools: []
  # Appended verbatim to every copilot run the daemon makes.
  extra_args: []

# Map a repo named in an ask's context to a local checkout.
repos:
  # "<owner>/<repo>": /home/alice/src/repo

daemon:
  registry_interval: 5m
  nak_backoff: 30s
  open_app: true
  notify: true
  # true appends a reply to the session that asked (copilot --resume).
  # Off by default: M0 showed the turn lands in the transcript but not in
  # the context of an agent the app is running in that session, and the
  # resume writes even while the app holds the session.
  reply_in_place: false
`
