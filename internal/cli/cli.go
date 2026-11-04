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
	"github.com/yjmrobert/wormhole/internal/systemd"
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

const (
	unknownMetadata       = "unknown"
	utcTimestampLayout    = "2006-01-02T15:04:05Z"
	invalidThreadIDFormat = "%q is not a thread id"
	invalidHandleFormat   = "%q is not a handle (lower-case letters, digits, - and _); see who is on the bus: wormhole users"
	threadNotFoundFormat  = "thread %s is not on this machine; list yours with: wormhole threads"
	busErrorFormat        = "bus: %v; check bus.url and the credential with: wormhole doctor"
	noInputReviewMessage  = "--no-input forbids confirmation prompts; review the message and rerun with --yes"
	notStartedMessage     = "not started; it stays in the inbox"
	notDeclinedMessage    = "not declined; it stays in the inbox"
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
	OpenStore  func() (*threads.Store, error)
	NewUpdater func() (releaseUpdater, error)
	Reexec     func() error
	// LookPath finds binaries on PATH (agent detection); nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Systemctl runs systemctl for the service commands; nil means the real one.
	Systemctl systemd.Runner
	// Executable is this binary's path for generated units; nil means os.Executable.
	Executable func() (string, error)
	Now        func() time.Time
	Hostname   string
	Version    string
	Commit     string
	BuildDate  string
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
		NewGHApp:   func(cfg *config.Config) ghapp.App { return ghapp.NewExec(cfg.Copilot.Binary) },
		OpenStore:  func() (*threads.Store, error) { return threads.Open(config.ThreadsDBPath()) },
		NewUpdater: newReleaseUpdater,
		Reexec:     reexecCurrentProcess,
		Now:        time.Now,
		Hostname:   host,
		Version:    "dev",
		Commit:     unknownMetadata,
		BuildDate:  unknownMetadata,
	}
}

// exitError carries an exit code with an error. silent means the command
// already reported the failure in its own output (doctor's JSON), so Run
// does not add an error object after it.
type exitError struct {
	code   int
	err    error
	silent bool
}

// Error returns the underlying command error message.
func (e *exitError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying command error.
func (e *exitError) Unwrap() error { return e.err }

// fail constructs a command error with a stable process exit code.
func fail(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

// wrap assigns an exit code unless err already carries one.
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

// loadConfig loads the selected configuration and maps failures to exit codes.
func (a *App) loadConfig(c *common) (*config.Config, error) {
	path := c.config
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(errors.Unwrap(err)) || errors.Is(err, os.ErrNotExist) {
			return nil, fail(ExitError, "no config at %s; join a bus with: wormhole join <invite>, or set up by hand with: wormhole init", path)
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

// command constructs a command with Wormhole's consistent flag behavior.
func command(use, short string) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, DisableFlagsInUseLine: true}
	cmd.Flags().SetInterspersed(true)
	return cmd
}

// exactArgs validates an exact positional argument count.
func exactArgs(n int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return fail(ExitUsage, "usage: %s", usage)
		}
		return nil
	}
}

// rangeArgs validates an inclusive positional argument range.
func rangeArgs(min, max int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < min || len(args) > max {
			return fail(ExitUsage, "usage: %s", usage)
		}
		return nil
	}
}

// noArgs rejects all positional arguments.
func noArgs(usage string) cobra.PositionalArgs {
	return exactArgs(0, usage)
}

// newRootCommand assembles the complete Wormhole command tree.
func (a *App) newRootCommand() (*cobra.Command, *common) {
	c := &common{}
	root := command("wormhole", "Open a wormhole between two Copilot sessions on two machines.")
	root.Long = "Wormhole sends questions and replies between Copilot sessions on two machines.\nEvery command accepts --json, --config, and --no-input. Exit codes: 0 ok, 1 error, 2 usage, 3 refused, 4 not found, 5 unavailable."
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	root.PersistentPreRunE = a.runAutomaticUpdate(c)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().BoolVar(&c.json, "json", false, "machine-readable output")
	root.PersistentFlags().StringVar(&c.config, "config", "", "config file (default "+config.DefaultPath()+")")
	root.PersistentFlags().BoolVar(&c.noInput, "no-input", false, "forbid interactive prompts (does not imply --yes)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fail(ExitUsage, "%v", err)
	})
	root.SetVersionTemplate(fmt.Sprintf("wormhole {{.Version}}\nbuild date: %s\n", a.buildDate()))
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
		a.newUpdateCommand(c),
		a.newManCommand(),
		a.newSendersCommand(c),
		a.newSkillCommand(c),
		a.newServiceCommand(c),
		a.newServeCommand(c),
		a.newJoinCommand(c),
		a.newDemoCommand(c),
	)
	a.registerCompletions(root, c)
	return root, c
}

// commit returns the commit metadata or its stable unknown fallback.
func (a *App) commit() string {
	if a.Commit == "" {
		return unknownMetadata
	}
	return a.Commit
}

// buildDate returns the build timestamp or its stable unknown fallback.
func (a *App) buildDate() string {
	if a.BuildDate == "" {
		return unknownMetadata
	}
	return a.BuildDate
}

// printVersion writes human-readable version and build metadata.
func (a *App) printVersion(w io.Writer) {
	fmt.Fprintf(w, "wormhole %s\ncommit: %s\nbuild date: %s\n", a.Version, a.commit(), a.buildDate())
}

// jsonRequested detects JSON output before Cobra parses the arguments.
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
	if jsonOut && !(ee != nil && ee.silent) {
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

// ago formats a timestamp as a compact duration relative to now.
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

// table writes aligned tabular output.
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

// sortedKeys returns map keys in deterministic lexical order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
