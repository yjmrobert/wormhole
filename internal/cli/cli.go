// Package cli implements the wormhole command. Every command takes --json
// and returns a stable exit code so agents parse results instead of
// scraping prose.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// Exit codes. They are part of the CLI contract.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitRefused     = 3 // secret scan hit, confirmation declined, policy
	ExitNotFound    = 4 // unknown handle or thread
	ExitUnavailable = 5 // bus or app unreachable
)

// App holds the command's collaborators so tests can substitute them.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// IsTerminal reports whether Stdin is interactive (confirmations).
	IsTerminal func() bool
	// Getenv reads the environment (COPILOT_AGENT_SESSION_ID).
	Getenv func(string) string
	// NewBus connects to the configured bus.
	NewBus func(ctx context.Context, cfg *config.Config) (bus.Bus, error)
	// NewGHApp builds the Copilot wrapper.
	NewGHApp func(cfg *config.Config) ghapp.App
	// OpenStore opens the thread map.
	OpenStore func() (*threads.Store, error)
	Now       func() time.Time
	Hostname  string
	Version   string
}

// Default returns an App wired to the real environment.
func Default() *App {
	host, _ := os.Hostname()
	return &App{
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		IsTerminal: func() bool { return isTerminal(os.Stdin) },
		Getenv:     os.Getenv,
		NewBus: func(ctx context.Context, cfg *config.Config) (bus.Bus, error) {
			if cfg.Bus.Kind != "nats" {
				return nil, fmt.Errorf("bus.kind %q is only available in-process", cfg.Bus.Kind)
			}
			return natsbus.Connect(ctx, natsbus.Options{URL: cfg.Bus.URL, User: cfg.Handle, Creds: cfg.Bus.Creds, PasswordFile: cfg.Bus.PasswordFile, TLSCA: cfg.Bus.TLSCA})
		},
		NewGHApp:  func(cfg *config.Config) ghapp.App { return ghapp.NewExec(cfg.Copilot.Binary) },
		OpenStore: func() (*threads.Store, error) { return threads.Open(config.ThreadsDBPath()) },
		Now:       time.Now,
		Hostname:  host,
		Version:   "dev",
	}
}

// exitError carries an exit code with an error.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func fail(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

func wrap(code int, err error) error {
	if err == nil {
		return nil
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return err
	}
	return &exitError{code: code, err: err}
}

// common flags every command accepts.
type common struct {
	json   bool
	config string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.BoolVar(&c.json, "json", false, "machine-readable output")
	fs.StringVar(&c.config, "config", "", "config file (default "+config.DefaultPath()+")")
}

func (a *App) loadConfig(c *common) (*config.Config, error) {
	path := c.config
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(errors.Unwrap(err)) || errors.Is(err, os.ErrNotExist) {
			return nil, fail(ExitError, "no config at %s; run: wormhole init", path)
		}
		return nil, wrap(ExitError, err)
	}
	return cfg, nil
}

// splitArgs lets flags appear before or after positionals, which is how
// agents tend to write them. Non-boolean flags consume the next argument.
func splitArgs(fs *flag.FlagSet, args []string) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) < 2 || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if strings.Contains(arg, "=") {
			continue
		}
		name := strings.TrimLeft(arg, "-")
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if bv, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bv.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return flags, positional
}

func (a *App) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(a.Stderr)
	flags, positional := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return nil, fail(ExitUsage, "%v", err)
	}
	return positional, nil
}

// emit prints v as JSON (--json) or via human, which writes the text form.
func (a *App) emit(c *common, v any, human func(w io.Writer)) {
	if c.json {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return
	}
	human(a.Stdout)
}

const usage = `wormhole: open a wormhole between two Copilot sessions on two machines.

Usage:
  wormhole ask <user> "<prompt>" [--repo owner/name] [--context "..."] [--session <id>] [--yes] [--force-unsafe]
  wormhole reply <thread_id> "<text>" [--yes]
  wormhole send <user> --thread <id> "<text>" [--yes]
  wormhole inbox [--watch|--ui] [--all]
  wormhole open <thread_id> [--agent] [--yes] [--no-start]
  wormhole snooze <thread_id> [<duration>|off]
  wormhole decline <thread_id> ["<reason>"] [--yes]
  wormhole threads [--open] [--peer <user>]
  wormhole users
  wormhole daemon [--once] [--pause|--resume]
  wormhole doctor [--deep-link]
  wormhole login [--creds <file>]
  wormhole init
  wormhole admin init --url <nats url> [--user admin] [--tls-ca <pem>]
  wormhole version

Every command accepts --json and --config <path>. A prompt or text of "-"
is read from stdin. Exit codes: 0 ok, 1 error, 2 usage, 3 refused,
4 not found, 5 bus or app unavailable.
`

// Run executes args and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(a.Stdout, usage)
		return ExitOK
	}
	cmd, rest := args[0], args[1:]
	var c common
	var err error
	switch cmd {
	case "ask":
		err = a.ask(ctx, &c, rest)
	case "reply":
		err = a.reply(ctx, &c, rest)
	case "send":
		err = a.send(ctx, &c, rest)
	case "inbox":
		err = a.inbox(ctx, &c, rest)
	case "open":
		err = a.open(ctx, &c, rest)
	case "snooze":
		err = a.snooze(ctx, &c, rest)
	case "decline":
		err = a.decline(ctx, &c, rest)
	case "threads":
		err = a.threads(ctx, &c, rest)
	case "users":
		err = a.users(ctx, &c, rest)
	case "daemon":
		err = a.daemon(ctx, &c, rest)
	case "doctor":
		err = a.doctor(ctx, &c, rest)
	case "login":
		err = a.login(ctx, &c, rest)
	case "init":
		err = a.initCmd(ctx, &c, rest)
	case "admin":
		err = a.admin(ctx, &c, rest)
	case "version":
		fs := flag.NewFlagSet("version", flag.ContinueOnError)
		c.bind(fs)
		if _, err = a.parse(fs, rest); err == nil {
			a.emit(&c, map[string]string{"version": a.Version}, func(w io.Writer) { fmt.Fprintf(w, "wormhole %s\n", a.Version) })
		}
	default:
		fmt.Fprintf(a.Stderr, "wormhole: unknown command %q\n\n%s", cmd, usage)
		return ExitUsage
	}
	if err == nil {
		return ExitOK
	}
	code := ExitError
	var ee *exitError
	if errors.As(err, &ee) {
		code = ee.code
	}
	if c.json {
		enc := json.NewEncoder(a.Stdout)
		_ = enc.Encode(map[string]any{"ok": false, "error": err.Error(), "code": code})
	} else {
		fmt.Fprintf(a.Stderr, "wormhole %s: %v\n", cmd, err)
	}
	return code
}

// readText resolves a body argument, reading stdin when it is "-".
func (a *App) readText(arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	b, err := io.ReadAll(a.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

func ago(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
