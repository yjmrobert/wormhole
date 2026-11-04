package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/systemd"
)

const (
	// serviceName is the daemon's user unit, wormhole.service.
	serviceName = "wormhole"
	// serviceFallback is what to do without a systemd user session.
	serviceFallback = "no systemd user session; run the daemon in a terminal: wormhole daemon"
)

// serviceImportEnv is the graphical session's environment the daemon
// needs: xdg-open, notify-send and the Copilot CLI's keychain.
var serviceImportEnv = []string{"DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS"}

// serviceResult is what `wormhole service install` did.
type serviceResult struct {
	OK          bool           `json:"ok"`
	Installed   bool           `json:"installed"`
	Unit        string         `json:"unit"`
	UnitPath    string         `json:"unit_path,omitempty"`
	ExecStart   []string       `json:"exec_start"`
	Changed     bool           `json:"changed"`
	Active      bool           `json:"active"`
	Imported    []string       `json:"imported,omitempty"`
	ImportError string         `json:"import_error,omitempty"`
	Fallback    string         `json:"fallback,omitempty"`
	Steps       []systemd.Step `json:"steps"`
}

// systemctl is the App's Systemctl runner, defaulting to the real one.
func (a *App) systemctl() systemd.Runner {
	if a.Systemctl != nil {
		return a.Systemctl
	}
	return systemd.ExecRunner{}
}

// daemonUnit builds the daemon's user unit from the running executable,
// with the same targets and restart policy as deploy/systemd/wormhole.service.
func (a *App) daemonUnit(c *common) (systemd.Unit, error) {
	exe, err := os.Executable()
	if err != nil {
		return systemd.Unit{}, fail(ExitError, "cannot find the wormhole executable: %v; reinstall it, then run: wormhole service install", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	argv := []string{exe, "daemon"}
	if c.config != "" {
		abs, err := filepath.Abs(c.config)
		if err != nil {
			return systemd.Unit{}, err
		}
		argv = append(argv, "--config", abs)
	}
	return systemd.Unit{
		Name:        serviceName,
		Description: "Wormhole daemon",
		Comment:     "Written by `wormhole service install`; rerun it after moving the binary.\nRemove with: wormhole service uninstall",
		ExecStart:   argv,
		After:       []string{"graphical-session.target", "network-online.target"},
		PartOf:      []string{"graphical-session.target"},
		WantedBy:    []string{"graphical-session.target"},
		Restart:     "on-failure",
		RestartSec:  5,
	}, nil
}

// installDaemonService writes, enables and starts the daemon's user unit
// and writes what it did to w (nil: silent). Without a systemd user session
// it returns Installed false and the fallback, not an error. `wormhole
// join` calls it.
func (a *App) installDaemonService(ctx context.Context, c *common, w io.Writer) (serviceResult, error) {
	if w == nil {
		w = io.Discard
	}
	u, err := a.daemonUnit(c)
	if err != nil {
		return serviceResult{}, err
	}
	out := serviceResult{OK: true, Unit: serviceName + ".service", ExecStart: u.ExecStart}
	res, err := systemd.InstallUser(ctx, a.systemctl(), u, serviceImportEnv)
	out.Steps = res.Steps
	if errors.Is(err, systemd.ErrNoUserSession) {
		out.Fallback = serviceFallback
		fmt.Fprintln(w, serviceFallback)
		return out, nil
	}
	out.UnitPath, out.Changed = res.UnitPath, res.Changed
	if err != nil {
		out.OK = false
		return out, fail(ExitError, "%v; check with: systemctl --user status %s", err, out.Unit)
	}
	out.Installed = true
	if res.Changed {
		fmt.Fprintf(w, "wrote %s (ExecStart: %s)\n", res.UnitPath, strings.Join(u.ExecStart, " "))
	} else {
		fmt.Fprintf(w, "%s is unchanged\n", res.UnitPath)
	}
	if res.ImportError != "" {
		out.ImportError = res.ImportError
		fmt.Fprintf(w, "could not import %s into the user manager (%s); notifications may not show: systemctl --user import-environment %s\n", strings.Join(serviceImportEnv, " "), res.ImportError, strings.Join(serviceImportEnv, " "))
	} else {
		out.Imported = serviceImportEnv
		fmt.Fprintf(w, "imported %s into the user manager\n", strings.Join(serviceImportEnv, " "))
	}
	if st, err := systemd.StatusOf(ctx, a.systemctl(), serviceName); err == nil {
		out.Active = st.Running()
	}
	if out.Active {
		fmt.Fprintf(w, "enabled and started %s; it is running\n", out.Unit)
	} else {
		fmt.Fprintf(w, "enabled and started %s, but it is not running; see: journalctl --user -u %s\n", out.Unit, serviceName)
	}
	fmt.Fprintf(w, "logs: journalctl --user -u %s -f\n", serviceName)
	return out, nil
}

// newServiceCommand manages the daemon's systemd user unit.
func (a *App) newServiceCommand(c *common) *cobra.Command {
	cmd := command("service", "Run the daemon as a systemd user service")
	cmd.Args = noArgs("wormhole service install|status|uninstall")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		return fail(ExitUsage, "usage: wormhole service install|status|uninstall")
	}

	install := command("install", "Install, enable and start the daemon as a user service")
	install.Args = noArgs("wormhole service install")
	install.RunE = func(cmd *cobra.Command, _ []string) error {
		var human strings.Builder
		res, err := a.installDaemonService(cmd.Context(), c, &human)
		if err != nil {
			return err
		}
		a.emit(c, res, func(w io.Writer) { fmt.Fprint(w, human.String()) })
		return nil
	}

	status := command("status", "Show whether the daemon's user service is installed and running")
	status.Args = noArgs("wormhole service status")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		st, err := systemd.StatusOf(cmd.Context(), a.systemctl(), serviceName)
		res := map[string]any{"ok": true, "unit": serviceName + ".service", "unit_path": st.UnitPath, "installed": st.Installed, "active": st.Active, "enabled": st.Enabled}
		switch {
		case errors.Is(err, systemd.ErrNoUserSession):
			res["fallback"] = serviceFallback
			delete(res, "active")
			delete(res, "enabled")
		case err != nil:
			return fail(ExitError, "%v; check with: systemctl --user status %s.service", err, serviceName)
		}
		a.emit(c, res, func(w io.Writer) {
			switch {
			case res["fallback"] != nil:
				fmt.Fprintln(w, serviceFallback)
			case !st.Installed:
				fmt.Fprintf(w, "%s.service is not installed; run: wormhole service install\n", serviceName)
			default:
				fmt.Fprintf(w, "%s.service: %s, %s (%s)\n", serviceName, st.Active, st.Enabled, st.UnitPath)
				if !st.Running() {
					fmt.Fprintf(w, "start it with: systemctl --user start %s; logs: journalctl --user -u %s\n", serviceName, serviceName)
				}
			}
		})
		return nil
	}

	uninstall := command("uninstall", "Stop, disable and remove the daemon's user service")
	uninstall.Args = noArgs("wormhole service uninstall")
	uninstall.RunE = func(cmd *cobra.Command, _ []string) error {
		res, err := systemd.Uninstall(cmd.Context(), a.systemctl(), serviceName)
		noSession := errors.Is(err, systemd.ErrNoUserSession)
		if err != nil && !noSession {
			return fail(ExitError, "%v; check with: systemctl --user status %s.service", err, serviceName)
		}
		a.emit(c, map[string]any{"ok": true, "unit_path": res.UnitPath, "removed": res.Removed, "steps": res.Steps}, func(w io.Writer) {
			if res.Removed {
				fmt.Fprintf(w, "stopped, disabled and removed %s\n", res.UnitPath)
			} else {
				fmt.Fprintf(w, "%s.service was not installed\n", serviceName)
			}
			if noSession && res.Removed {
				fmt.Fprintln(w, "no systemd user session: the unit file is gone; stop a daemon running in a terminal with Ctrl-C")
			}
		})
		return nil
	}
	cmd.AddCommand(install, status, uninstall)
	return cmd
}
