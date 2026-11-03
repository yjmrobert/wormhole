package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/config"
	selfupdate "github.com/yjmrobert/wormhole/internal/update"
)

const automaticUpdateTimeout = 30 * time.Second

type releaseUpdater interface {
	CheckAndUpdate(context.Context, string) (selfupdate.Result, error)
	ForceUpdate(context.Context, string) (selfupdate.Result, error)
	Enabled() bool
	SetEnabled(bool) error
}

// newReleaseUpdater constructs the GitHub-backed release updater.
func newReleaseUpdater() (releaseUpdater, error) {
	return selfupdate.New(selfupdate.Options{StateDir: config.StateDir()})
}

// runAutomaticUpdate performs eligible opt-in updates before a command runs.
func (a *App) runAutomaticUpdate(c *common) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if a.NewUpdater == nil || c.json || c.noInput || a.IsTerminal == nil || !a.IsTerminal() || updateExempt(cmd) || selfupdate.AutomaticChecksDisabled() {
			return nil
		}
		u, err := a.NewUpdater()
		if err == nil {
			var res selfupdate.Result
			ctx, cancel := context.WithTimeout(cmd.Context(), automaticUpdateTimeout)
			res, err = u.CheckAndUpdate(ctx, a.Version)
			cancel()
			if err == nil && res.Updated {
				fmt.Fprintf(a.Stderr, "wormhole: updated to %s; restarting the command\n", res.Tag)
				if a.Reexec != nil {
					err = a.Reexec()
				}
			}
		}
		if err != nil {
			fmt.Fprintf(a.Stderr, "wormhole: update check failed: %v\n", err)
		}
		return nil
	}
}

// updateExempt reports whether a command must never trigger an automatic update.
func updateExempt(cmd *cobra.Command) bool {
	if cmd == cmd.Root() {
		return true
	}
	for cmd.Parent() != nil && cmd.Parent() != cmd.Root() {
		cmd = cmd.Parent()
	}
	switch cmd.Name() {
	case "daemon", "update", "version", "completion", "commands", "man", "help", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true
	default:
		return false
	}
}

type updateOutput struct {
	OK      bool   `json:"ok"`
	Enabled bool   `json:"enabled"`
	Checked bool   `json:"checked"`
	Latest  string `json:"latest,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Updated bool   `json:"updated"`
}

// newUpdateCommand installs updates and controls automatic-check preferences.
func (a *App) newUpdateCommand(c *common) *cobra.Command {
	var enable, disable bool
	cmd := command("update", "Install a newer release or control automatic checks")
	cmd.Flags().BoolVar(&enable, "enable", false, "enable daily automatic update checks")
	cmd.Flags().BoolVar(&disable, "disable", false, "disable automatic update checks")
	cmd.Args = noArgs("wormhole update [--enable|--disable]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if enable && disable {
			return fail(ExitUsage, "--enable and --disable are exclusive")
		}
		if a.NewUpdater == nil {
			return fail(ExitUnavailable, "self-update is not configured")
		}
		if enable && selfupdate.IsDevelopmentVersion(a.Version) {
			return fail(ExitUnavailable, "automatic updates are unavailable for development build %q; use a tagged release", a.Version)
		}
		u, err := a.NewUpdater()
		if err != nil {
			return wrap(ExitUnavailable, err)
		}
		if enable || disable {
			if err := u.SetEnabled(enable); err != nil {
				return wrap(ExitError, err)
			}
			res := updateOutput{OK: true, Enabled: enable}
			a.emit(c, res, func(w io.Writer) {
				fmt.Fprintf(w, "automatic update checks enabled: %t\n", enable)
			})
			return nil
		}
		res, err := u.ForceUpdate(cmd.Context(), a.Version)
		if err != nil {
			return wrap(ExitUnavailable, err)
		}
		out := updateOutput{OK: true, Enabled: u.Enabled(), Checked: res.Checked, Latest: res.Latest, Tag: res.Tag, Updated: res.Updated}
		a.emit(c, out, func(w io.Writer) {
			switch {
			case res.Updated:
				fmt.Fprintf(w, "updated to %s; restart a running daemon with: systemctl --user restart wormhole\n", res.Tag)
			case res.Latest != "":
				fmt.Fprintf(w, "already up to date (%s)\n", res.Tag)
			default:
				fmt.Fprintln(w, "already up to date")
			}
		})
		return nil
	}
	return cmd
}
