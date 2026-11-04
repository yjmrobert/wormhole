package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/systemd"
)

// The daemon and the embedded bus run as systemd user services the binary
// writes itself. `wormhole service` is the daemon's; `wormhole serve
// install` is the bus's.

const (
	daemonUnit  = "wormhole"
	serveUnit   = "wormhole-serve"
	daemonFallb = "no systemd user session here; run the daemon in a terminal instead: wormhole daemon"
)

// displayEnv are the variables xdg-open, notify-send and the Copilot
// CLI's keychain need from the graphical session.
var displayEnv = []string{"DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "XAUTHORITY"}

// serviceResult is what a service install reports.
type serviceResult struct {
	OK bool `json:"ok"`
	systemd.Result
	Unit string `json:"unit"`
}

// systemctl is the runner, through the App's hook in tests.
func (a *App) systemctl() systemd.Runner {
	if a.Systemctl != nil {
		return a.Systemctl
	}
	return systemd.ExecRunner{}
}

// executable is this binary's path, for ExecStart.
func (a *App) executable() string {
	if a.Executable != nil {
		if p, err := a.Executable(); err == nil {
			return p
		}
	}
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "wormhole"
}

// daemonUnitFor is the generated daemon unit; it matches the shipped
// deploy/systemd/wormhole.service.
func (a *App) daemonUnitFor(configPath string) systemd.Unit {
	exec := []string{a.executable(), "daemon"}
	if configPath != "" {
		exec = append(exec, "--config", configPath)
	}
	return systemd.Unit{
		Name:        daemonUnit,
		Description: "Wormhole daemon",
		ExecStart:   exec,
		After:       []string{"graphical-session.target", "network-online.target"},
		PartOf:      []string{"graphical-session.target"},
		WantedBy:    []string{"graphical-session.target"},
		Restart:     "on-failure",
		RestartSec:  5,
	}
}

// installDaemonService writes, enables and starts the daemon unit. It is
// `wormhole service install` and the last step of `wormhole join`.
// Without a systemd user session it returns ok with the fallback filled.
func (a *App) installDaemonService(ctx context.Context, c *common, w io.Writer) (serviceResult, error) {
	res, err := systemd.InstallUser(ctx, a.systemctl(), a.daemonUnitFor(c.config), displayEnv, daemonFallb)
	out := serviceResult{OK: true, Result: res, Unit: daemonUnit}
	switch {
	case errors.Is(err, systemd.ErrNoUserSession):
		if w != nil {
			fmt.Fprintf(w, "wrote %s; %s\n", res.Path, daemonFallb)
		}
		return out, nil
	case err != nil:
		return out, fail(ExitUnavailable, "systemd refused the daemon unit: %v; the unit is at %s, start it by hand with: systemctl --user enable --now %s", err, res.Path, daemonUnit)
	}
	if w != nil {
		fmt.Fprintf(w, "daemon running as a user service (%s); logs: journalctl --user -u %s -f\n", res.Path, daemonUnit)
		if !res.ImportedEnv {
			fmt.Fprintf(w, "could not import the display environment; notifications may be silent until: systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS\n")
		}
	}
	return out, nil
}

// newServiceCommand manages the daemon's user service.
func (a *App) newServiceCommand(c *common) *cobra.Command {
	cmd := command("service", "Run the daemon as a systemd user service")
	cmd.Long = "Generates the daemon's user unit from this binary's path, imports the display environment it\nneeds for notifications and the app, and enables it. join runs install for you."
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }

	install := command("install", "Write, enable and start the daemon unit")
	install.Args = noArgs("wormhole service install")
	install.RunE = func(cmd *cobra.Command, _ []string) error {
		if _, err := a.loadConfig(c); err != nil {
			return err
		}
		var w io.Writer
		if !c.json {
			w = a.Stdout
		}
		res, err := a.installDaemonService(cmd.Context(), c, w)
		if err != nil {
			return err
		}
		a.emit(c, res, func(io.Writer) {})
		return nil
	}

	status := command("status", "Show whether the daemon unit is installed and running")
	status.Args = noArgs("wormhole service status")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		st := systemd.Query(cmd.Context(), a.systemctl(), daemonUnit)
		a.emit(c, map[string]any{"ok": true, "unit": daemonUnit, "status": st}, func(w io.Writer) {
			if !st.Exists {
				fmt.Fprintf(w, "no unit at %s; install it with: wormhole service install\n", st.Path)
				return
			}
			fmt.Fprintf(w, "%s: %s, %s (%s)\n", daemonUnit, st.Active, st.Enabled, st.Path)
			if st.Active != "active" {
				fmt.Fprintf(w, "start it with: systemctl --user start %s; logs: journalctl --user -u %s\n", daemonUnit, daemonUnit)
			}
		})
		return nil
	}

	uninstall := command("uninstall", "Stop and remove the daemon unit")
	uninstall.Args = noArgs("wormhole service uninstall")
	uninstall.RunE = func(cmd *cobra.Command, _ []string) error {
		path, err := systemd.Uninstall(cmd.Context(), a.systemctl(), daemonUnit)
		if err != nil {
			return err
		}
		a.emit(c, map[string]any{"ok": true, "unit": daemonUnit, "path": path}, func(w io.Writer) {
			fmt.Fprintf(w, "removed %s; the daemon is stopped. Run it by hand with: wormhole daemon\n", path)
		})
		return nil
	}
	cmd.AddCommand(install, status, uninstall)
	return cmd
}
