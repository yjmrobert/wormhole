// Package cli implements the wormhole command. Every command takes --json
// and returns a stable exit code so agents parse results instead of
// scraping prose.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"charm.land/fang/v2"
	"github.com/spf13/cobra"

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
	Commit    string
	BuildDate string
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
		Commit:    "unknown",
		BuildDate: "unknown",
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
	json    bool
	config  string
	noInput bool
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

func command(use, short string) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, DisableFlagsInUseLine: true}
	cmd.Flags().SetInterspersed(true)
	return cmd
}

func exactArgs(n int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return fail(ExitUsage, "usage: %s", usage)
		}
		return nil
	}
}

func rangeArgs(min, max int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < min || len(args) > max {
			return fail(ExitUsage, "usage: %s", usage)
		}
		return nil
	}
}

func noArgs(usage string) cobra.PositionalArgs {
	return exactArgs(0, usage)
}

func (a *App) newRootCommand() (*cobra.Command, *common) {
	c := &common{}
	root := command("wormhole", "Open a wormhole between two Copilot sessions on two machines.")
	root.Long = "Wormhole sends questions and replies between Copilot sessions on two machines.\nEvery command accepts --json and --config. Exit codes: 0 ok, 1 error, 2 usage, 3 refused, 4 not found, 5 unavailable."
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().BoolVar(&c.json, "json", false, "machine-readable output")
	root.PersistentFlags().StringVar(&c.config, "config", "", "config file (default "+config.DefaultPath()+")")
	root.PersistentFlags().BoolVar(&c.noInput, "no-input", false, "forbid interactive prompts (does not imply --yes)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fail(ExitUsage, "%v", err)
	})
	root.SetVersionTemplate(fmt.Sprintf("wormhole {{.Version}}\ncommit: %s\nbuild date: %s\n", a.commit(), a.buildDate()))
	root.AddCommand(
		a.newAskCommand(c),
		a.newReplyCommand(c),
		a.newSendCommand(c),
		a.newInboxCommand(c),
		a.newOpenCommand(c),
		a.newSnoozeCommand(c),
		a.newDeclineCommand(c),
		a.newThreadsCommand(c),
		a.newUsersCommand(c),
		a.newDaemonCommand(c),
		a.newDoctorCommand(c),
		a.newLoginCommand(c),
		a.newInitCommand(c),
		a.newAdminCommand(c),
		a.newCompletionCommand(),
		a.newCommandsCommand(c),
		a.newVersionCommand(c),
		a.newManCommand(),
	)
	return root, c
}

func (a *App) commit() string {
	if a.Commit == "" {
		return "unknown"
	}
	return a.Commit
}

func (a *App) buildDate() string {
	if a.BuildDate == "" {
		return "unknown"
	}
	return a.BuildDate
}

func (a *App) printVersion(w io.Writer) {
	fmt.Fprintf(w, "wormhole %s\ncommit: %s\nbuild date: %s\n", a.Version, a.commit(), a.buildDate())
}

func jsonRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--json" {
			return true
		}
		if value, ok := strings.CutPrefix(arg, "--json="); ok {
			b, err := strconv.ParseBool(value)
			return err != nil || b
		}
	}
	return false
}

// plainWriter keeps Fang from detecting a terminal for JSON-mode help.
type plainWriter struct{ io.Writer }

// Run executes args and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	root, _ := a.newRootCommand()
	root.SetArgs(args)
	jsonOut := jsonRequested(args)
	if jsonOut {
		root.SetOut(plainWriter{a.Stdout})
		root.SetErr(plainWriter{a.Stderr})
	}
	err := fang.Execute(
		ctx,
		root,
		fang.WithVersion(a.Version),
		fang.WithCommit(a.Commit),
		fang.WithoutManpage(),
		fang.WithErrorHandler(func(w io.Writer, styles fang.Styles, err error) {
			if !jsonOut {
				fang.DefaultErrorHandler(w, styles, err)
			}
		}),
	)
	if err == nil {
		return ExitOK
	}
	code := ExitError
	var ee *exitError
	if errors.As(err, &ee) {
		code = ee.code
	} else if strings.HasPrefix(err.Error(), "unknown command") {
		code = ExitUsage
	}
	if jsonOut {
		_ = json.NewEncoder(a.Stdout).Encode(map[string]any{"ok": false, "error": err.Error(), "code": code})
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
