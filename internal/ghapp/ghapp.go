// Package ghapp wraps the GitHub Copilot CLI and app: creating a headless
// session with a kickoff prompt, appending a turn to an existing session,
// showing a session in the app and raising a desktop notification.
//
// This is the only package that knows the ghapp:// deep-link scheme. It is
// undocumented app behavior and may change on any update; `wormhole doctor
// --deep-link` exercises it and `wormhole open` prints the --resume
// fallback when it breaks.
package ghapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Session describes a session to create, or (for Resume) the session to
// append a turn to.
type Session struct {
	// ID is the client-chosen session UUID (--session-id), or the session
	// being resumed (--resume).
	ID string
	// Name is the sidebar name (-n). Ignored by Resume.
	Name string
	// Kickoff is the prompt (-p). It is passed as one argument and never
	// interpolated into a shell string.
	Kickoff string
	// Workdir is the directory the session starts in.
	Workdir string
	// Model is the optional --model id.
	Model string
	// AvailableTools, when non-empty, restricts the run to --available-tools.
	AvailableTools []string
	// ExtraArgs are appended verbatim to every copilot invocation: the
	// deployment's permission flags (which commands the agent may run) and
	// anything else the app version needs. They are never shell-expanded.
	ExtraArgs []string
}

// App is what the daemon needs from the Copilot app.
type App interface {
	// Create starts a new session and runs the kickoff headless.
	Create(ctx context.Context, s Session) error
	// Resume appends s.Kickoff as a new turn on the existing session s.ID
	// and runs it headless, honoring Model and AvailableTools.
	Resume(ctx context.Context, s Session) error
	// Open shows the session in the app. Best effort.
	Open(ctx context.Context, sessionID string) error
	// Notify raises a desktop notification. Best effort.
	Notify(ctx context.Context, title, body string) error
}

// DeepLink returns the app URL for a session.
func DeepLink(sessionID string) string {
	return "ghapp://sessions/" + sessionID
}

// ResumeCommand is the fallback a person can run when the deep link fails.
func ResumeCommand(sessionID string) string {
	return "copilot --resume=" + sessionID
}

// NewSessionID returns a random version-4 UUID in canonical form.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// SessionName builds the sidebar name: "^alice: <first 40 chars>".
func SessionName(peer, body string) string {
	first := strings.Join(strings.Fields(body), " ")
	r := []rune(first)
	if len(r) > 40 {
		first = string(r[:40]) + "…"
	}
	return "^" + peer + ": " + first
}

// Exec is the real App backed by the copilot binary, xdg-open and notify-send.
type Exec struct {
	Binary   string // copilot
	Opener   string // xdg-open
	Notifier string // notify-send
	Timeout  time.Duration
}

// NewExec returns an Exec with Linux defaults.
func NewExec(binary string) *Exec {
	if binary == "" {
		binary = "copilot"
	}
	return &Exec{Binary: binary, Opener: "xdg-open", Notifier: "notify-send", Timeout: 30 * time.Minute}
}

// ErrNotInstalled is returned when a required binary is missing from PATH.
var ErrNotInstalled = errors.New("binary not found on PATH")

// RunError is a failed command. Error() names the command and its exit
// status only: the captured output can quote the kickoff, so it is kept
// apart for callers that show it to the person (doctor), and never ends up
// in the daemon's logs through the error string.
type RunError struct {
	Cmd    string
	Err    error
	Output string
}

func (e *RunError) Error() string { return e.Cmd + ": " + e.Err.Error() }
func (e *RunError) Unwrap() error { return e.Err }

// Check verifies the copilot binary is on PATH.
func (e *Exec) Check() error {
	if _, err := exec.LookPath(e.Binary); err != nil {
		return fmt.Errorf("%w: %s", ErrNotInstalled, e.Binary)
	}
	return nil
}

func (e *Exec) run(ctx context.Context, dir string, name string, args ...string) error {
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return &RunError{Cmd: name, Err: err, Output: msg}
	}
	return nil
}

// Create runs: copilot -p <kickoff> --session-id <id> -n <name> [--model m]
// [--available-tools a,b] [extra args].
func (e *Exec) Create(ctx context.Context, s Session) error {
	if err := os.MkdirAll(s.Workdir, 0o755); err != nil {
		return fmt.Errorf("workdir: %w", err)
	}
	args := []string{"-p", s.Kickoff, "--session-id", s.ID, "-n", s.Name}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if len(s.AvailableTools) > 0 {
		args = append(args, "--available-tools", strings.Join(s.AvailableTools, ","))
	}
	args = append(args, s.ExtraArgs...)
	return e.run(ctx, s.Workdir, e.Binary, args...)
}

// Resume runs: copilot -p <prompt> --resume=<id> [--model m]
// [--available-tools a,b] [extra args]. It fails when the session does not exist on
// this machine or the app holds it locked; callers fall back to Create.
func (e *Exec) Resume(ctx context.Context, s Session) error {
	if s.Workdir != "" {
		if err := os.MkdirAll(s.Workdir, 0o755); err != nil {
			return fmt.Errorf("workdir: %w", err)
		}
	}
	args := []string{"-p", s.Kickoff, "--resume=" + s.ID}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if len(s.AvailableTools) > 0 {
		args = append(args, "--available-tools", strings.Join(s.AvailableTools, ","))
	}
	args = append(args, s.ExtraArgs...)
	return e.run(ctx, s.Workdir, e.Binary, args...)
}

// Open runs: xdg-open ghapp://sessions/<id>.
func (e *Exec) Open(ctx context.Context, sessionID string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return errors.New("no DISPLAY or WAYLAND_DISPLAY in the daemon's environment; run: systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return e.run(ctx, "", e.Opener, DeepLink(sessionID))
}

// Notify runs: notify-send <title> <body>.
func (e *Exec) Notify(ctx context.Context, title, body string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return e.run(ctx, "", e.Notifier, "--app-name=wormhole", title, body)
}

// Call is one recorded App invocation.
type Call struct {
	Op        string // create, resume, open, notify
	Session   Session
	SessionID string
	Prompt    string
	Title     string
	Body      string
}

// Recorder is an App for tests. It records calls and can be told to fail.
type Recorder struct {
	mu    sync.Mutex
	Calls []Call
	// Fail, when non-nil, decides whether an op fails.
	Fail func(op string) error
}

func (r *Recorder) record(c Call) error {
	r.mu.Lock()
	r.Calls = append(r.Calls, c)
	r.mu.Unlock()
	if r.Fail != nil {
		return r.Fail(c.Op)
	}
	return nil
}

func (r *Recorder) Create(ctx context.Context, s Session) error {
	return r.record(Call{Op: "create", Session: s})
}

func (r *Recorder) Resume(ctx context.Context, s Session) error {
	return r.record(Call{Op: "resume", Session: s, SessionID: s.ID, Prompt: s.Kickoff})
}

func (r *Recorder) Open(ctx context.Context, sessionID string) error {
	return r.record(Call{Op: "open", SessionID: sessionID})
}

func (r *Recorder) Notify(ctx context.Context, title, body string) error {
	return r.record(Call{Op: "notify", Title: title, Body: body})
}

// Ops returns the recorded operation names in order.
func (r *Recorder) Ops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.Calls))
	for i, c := range r.Calls {
		out[i] = c.Op
	}
	return out
}

// Snapshot returns a copy of the recorded calls.
func (r *Recorder) Snapshot() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.Calls...)
}
