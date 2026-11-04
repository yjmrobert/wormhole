// Package systemd renders and manages systemd user units. It shells out to
// `systemctl --user` through a Runner, so tests use FakeRunner and never
// touch the machine's service manager.
package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNoUserSession means systemctl is missing or there is no systemd user
// manager to talk to (a container, an SSH login without lingering, a
// desktop without systemd). The caller falls back to a foreground process.
var ErrNoUserSession = errors.New("no systemd user session")

// Unit is a simple service unit.
type Unit struct {
	// Name is the unit name without ".service".
	Name        string
	Description string
	// Comment is written as # lines above [Unit].
	Comment string
	// ExecStart is the command as argv; Render quotes it for systemd.
	ExecStart  []string
	After      []string
	PartOf     []string
	WantedBy   []string
	Restart    string
	RestartSec int
}

// Render returns the unit file's content.
func (u Unit) Render() string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(u.Comment), "\n") {
		if u.Comment == "" {
			break
		}
		fmt.Fprintf(&b, "# %s\n", strings.TrimSpace(line))
	}
	if u.Comment != "" {
		b.WriteString("\n")
	}
	b.WriteString("[Unit]\n")
	if u.Description != "" {
		fmt.Fprintf(&b, "Description=%s\n", u.Description)
	}
	if len(u.After) > 0 {
		fmt.Fprintf(&b, "After=%s\n", strings.Join(u.After, " "))
	}
	if len(u.PartOf) > 0 {
		fmt.Fprintf(&b, "PartOf=%s\n", strings.Join(u.PartOf, " "))
	}
	b.WriteString("\n[Service]\n")
	quoted := make([]string, len(u.ExecStart))
	for i, a := range u.ExecStart {
		quoted[i] = QuoteArg(a)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(quoted, " "))
	if u.Restart != "" {
		fmt.Fprintf(&b, "Restart=%s\n", u.Restart)
	}
	if u.RestartSec > 0 {
		fmt.Fprintf(&b, "RestartSec=%d\n", u.RestartSec)
	}
	if len(u.WantedBy) > 0 {
		fmt.Fprintf(&b, "\n[Install]\nWantedBy=%s\n", strings.Join(u.WantedBy, " "))
	}
	return b.String()
}

// QuoteArg quotes one ExecStart word: % and $ are escaped so systemd does
// not expand them, and a word with spaces, quotes or backslashes is
// double-quoted with C-style escapes.
func QuoteArg(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// Runner runs a program and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (output string, err error)
}

// ExecRunner runs real programs.
type ExecRunner struct{}

// Run executes name with args. A missing binary wraps exec.ErrNotFound.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// FakeRunner records commands and answers from Responses; tests use it.
type FakeRunner struct {
	mu sync.Mutex
	// Calls holds "name arg arg..." per run, in order.
	Calls []string
	// Responses maps a command line (as recorded) or its prefix to an
	// answer. The longest matching key wins; no match succeeds with no
	// output.
	Responses map[string]FakeResponse
}

// FakeResponse is a canned answer.
type FakeResponse struct {
	Output string
	Err    error
}

// Run records the call and returns the matching response.
func (f *FakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, line)
	best, found := "", false
	for k := range f.Responses {
		if strings.HasPrefix(line, k) && len(k) >= len(best) {
			best, found = k, true
		}
	}
	if !found {
		return "", nil
	}
	r := f.Responses[best]
	return r.Output, r.Err
}

// Snapshot returns a copy of the recorded calls.
func (f *FakeRunner) Snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

// UserUnitDir is $XDG_CONFIG_HOME/systemd/user, default ~/.config/systemd/user.
func UserUnitDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

// UserUnitPath is where the user unit name lives.
func UserUnitPath(name string) string {
	return filepath.Join(UserUnitDir(), name+".service")
}

// Step is one systemctl invocation and how it went.
type Step struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Output  string `json:"output,omitempty"`
}

// Result is what InstallUser did.
type Result struct {
	UnitPath string `json:"unit_path"`
	// Changed is true when the unit file was written or rewritten.
	Changed bool   `json:"changed"`
	Steps   []Step `json:"steps"`
	// ImportError records a failed import-environment; it does not fail
	// the install.
	ImportError string `json:"import_error,omitempty"`
	// Fallback is what to do instead when there is no user session.
	Fallback string `json:"fallback,omitempty"`
}

// systemctl runs `systemctl --user args...` and records the step.
func systemctl(ctx context.Context, r Runner, steps *[]Step, args ...string) (string, error) {
	full := append([]string{"--user"}, args...)
	out, err := r.Run(ctx, "systemctl", full...)
	if steps != nil {
		*steps = append(*steps, Step{Command: "systemctl " + strings.Join(full, " "), OK: err == nil, Output: strings.TrimSpace(out)})
	}
	if err != nil && noSession(out, err) {
		return out, fmt.Errorf("%w: %s", ErrNoUserSession, firstLine(out, err))
	}
	return out, err
}

// noSession recognizes systemctl's "nothing to talk to" failures.
func noSession(out string, err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	for _, s := range []string{
		"Failed to connect to bus",
		"Failed to connect to user scope bus",
		"No medium found",
		"DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined",
		"System has not been booted with systemd",
	} {
		if strings.Contains(out, s) || strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}

// firstLine summarizes a failure for an error message.
func firstLine(out string, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); line != "" {
		return line
	}
	return err.Error()
}

// InstallUser writes u as a user unit, reloads the manager, imports the
// named environment variables into it, and enables and starts the unit.
// Without a user manager it returns ErrNoUserSession and Result.Fallback,
// and leaves no new unit file behind.
func InstallUser(ctx context.Context, r Runner, u Unit, importEnv []string) (Result, error) {
	path := UserUnitPath(u.Name)
	res := Result{UnitPath: path, Fallback: "no systemd user session; run in a terminal: " + strings.Join(u.ExecStart, " ")}
	content := u.Render()
	old, readErr := os.ReadFile(path)
	existed := readErr == nil
	if !existed || string(old) != content {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return res, err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return res, err
		}
		res.Changed = true
	}
	if _, err := systemctl(ctx, r, &res.Steps, "daemon-reload"); err != nil {
		if errors.Is(err, ErrNoUserSession) && !existed {
			_ = os.Remove(path)
			res.Changed = false
		}
		return res, err
	}
	res.Fallback = ""
	if len(importEnv) > 0 {
		args := append([]string{"import-environment"}, importEnv...)
		if out, err := systemctl(ctx, r, &res.Steps, args...); err != nil {
			res.ImportError = firstLine(out, err)
		}
	}
	unit := u.Name + ".service"
	if out, err := systemctl(ctx, r, &res.Steps, "enable", "--now", unit); err != nil {
		return res, fmt.Errorf("systemctl --user enable --now %s: %s", unit, firstLine(out, err))
	}
	if existed && res.Changed {
		// A running service keeps the old unit until it restarts.
		if out, err := systemctl(ctx, r, &res.Steps, "try-restart", unit); err != nil {
			return res, fmt.Errorf("systemctl --user try-restart %s: %s", unit, firstLine(out, err))
		}
	}
	return res, nil
}

// Status is a user unit's state.
type Status struct {
	Name      string `json:"name"`
	UnitPath  string `json:"unit_path"`
	Installed bool   `json:"installed"`
	// Active is is-active's answer: active, inactive, failed, ...
	Active string `json:"active"`
	// Enabled is is-enabled's answer: enabled, disabled, not-found, ...
	Enabled string `json:"enabled"`
}

// Running reports whether the unit is active.
func (s Status) Running() bool { return s.Active == "active" }

// StatusOf asks the user manager about name. is-active and is-enabled exit
// non-zero for "no", so their output is the answer either way.
func StatusOf(ctx context.Context, r Runner, name string) (Status, error) {
	path := UserUnitPath(name)
	st := Status{Name: name, UnitPath: path}
	if _, err := os.Stat(path); err == nil {
		st.Installed = true
	}
	unit := name + ".service"
	out, err := systemctl(ctx, r, nil, "is-active", unit)
	if errors.Is(err, ErrNoUserSession) {
		return st, err
	}
	st.Active = answer(out, "unknown")
	out, err = systemctl(ctx, r, nil, "is-enabled", unit)
	if errors.Is(err, ErrNoUserSession) {
		return st, err
	}
	st.Enabled = answer(out, "unknown")
	return st, nil
}

// answer is the first line of a systemctl query's output.
func answer(out, def string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if line == "" {
		return def
	}
	return strings.TrimSpace(line)
}

// UninstallResult is what Uninstall did.
type UninstallResult struct {
	UnitPath string `json:"unit_path"`
	Removed  bool   `json:"removed"`
	Steps    []Step `json:"steps"`
}

// Uninstall stops and disables name, removes its unit file and reloads the
// manager. Without a user session it still removes the file.
func Uninstall(ctx context.Context, r Runner, name string) (UninstallResult, error) {
	path := UserUnitPath(name)
	res := UninstallResult{UnitPath: path}
	unit := name + ".service"
	_, sessErr := systemctl(ctx, r, &res.Steps, "disable", "--now", unit)
	if err := os.Remove(path); err == nil {
		res.Removed = true
	} else if !os.IsNotExist(err) {
		return res, err
	}
	if errors.Is(sessErr, ErrNoUserSession) {
		return res, sessErr
	}
	if _, err := systemctl(ctx, r, &res.Steps, "daemon-reload"); err != nil {
		return res, err
	}
	return res, nil
}
