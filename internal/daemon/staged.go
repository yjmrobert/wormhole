package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// The inbox verbs that act on a staged message live here, so that
// `wormhole open`, `wormhole inbox --watch`, the local inbox page and the
// notification buttons all run the same code. Decline is in decline.go.

// OpenResult is what running a staged message produced.
type OpenResult struct {
	ThreadID  string `json:"thread_id"`
	SessionID string `json:"session_id"`
	// Started is true when the staged message ran (Resumed says in which
	// session); false when the thread already had a session and only the
	// deep link was opened.
	Started bool `json:"started"`
	Resumed bool `json:"resumed"`
	// Agent is true when the run used the auto-mode profile.
	Agent bool `json:"agent,omitempty"`
	// Opened is true when the app showed the session; OpenError says why
	// not, and Fallback is the command a person can run instead.
	Opened    bool   `json:"opened"`
	Fallback  string `json:"fallback"`
	OpenError string `json:"open_error,omitempty"`
}

// Errors from OpenStaged and ParseSnooze. Callers compare with errors.Is.
var (
	ErrNoAgentProfile = errors.New("copilot.safe_tools is empty; running a message under the agent profile needs the auto-mode profile (see docs/design.md, M0 results)")
	ErrNothingToOpen  = errors.New("no local session and nothing staged")
	ErrStartSession   = errors.New("start session")
	ErrBadDuration    = errors.New("not a duration (try 2h, 45m, 1d, or off)")
)

// CheckAgentProfile refuses the agent profile when no auto-mode profile is
// configured, the same way the daemon refuses auto mode.
func CheckAgentProfile(cfg *config.Config, agent bool) error {
	if agent && len(cfg.Copilot.SafeTools) == 0 {
		return ErrNoAgentProfile
	}
	return nil
}

// OpenStaged runs t's staged message, if any, and shows the thread's
// session in the app. It is the one place a staged message turns into a
// Copilot run, and every caller shows the person the full text and gets a
// yes before coming here. With agent, the run is restricted like auto mode
// (safe_tools plus extra_args); otherwise it runs with the person's normal
// tools. progress, when non-nil, gets one line per slow step.
func OpenStaged(ctx context.Context, cfg *config.Config, store *threads.Store, app ghapp.App, t threads.Thread, agent bool, progress io.Writer) (OpenResult, threads.Thread, error) {
	res := OpenResult{ThreadID: t.ID, Agent: agent}
	if t.StagedKickoff == "" && t.LocalSessionID == "" {
		return res, t, fmt.Errorf("thread %s: %w", t.ID, ErrNothingToOpen)
	}
	if t.StagedKickoff != "" {
		if err := CheckAgentProfile(cfg, agent); err != nil {
			return res, t, err
		}
		workdir := t.Workdir
		if workdir == "" {
			workdir = cfg.WorkdirFor(t.Repo)
		}
		var tools []string
		if agent {
			tools = cfg.Copilot.SafeTools
		}
		sessionID := ContinueSession(cfg, t)
		if sessionID != "" {
			say(progress, "appending the message to session %s (it runs headless first; this can take a minute)", sessionID)
			if err := app.Resume(ctx, ghapp.Session{ID: sessionID, Kickoff: t.StagedKickoff, Workdir: workdir, Model: cfg.Copilot.Model, AvailableTools: tools, ExtraArgs: cfg.Copilot.ExtraArgs}); err != nil {
				say(progress, "could not resume session %s (%v); starting a new one", sessionID, err)
				sessionID = ""
			} else {
				res.Resumed = true
			}
		}
		if sessionID == "" {
			sessionID = ghapp.NewSessionID()
			name := ghapp.SessionName(t.Peer, t.Title)
			if t.Direction == threads.DirectionOut {
				name = fmt.Sprintf("Reply from ^%s", t.Peer)
			}
			say(progress, "starting session %s in %s (the kickoff runs headless first; this can take a minute)", sessionID, workdir)
			err := app.Create(ctx, ghapp.Session{ID: sessionID, Name: name, Kickoff: t.StagedKickoff, Workdir: workdir, Model: cfg.Copilot.Model, AvailableTools: tools, ExtraArgs: cfg.Copilot.ExtraArgs})
			if err != nil {
				return res, t, fmt.Errorf("%w: %v", ErrStartSession, err)
			}
		}
		res.Started = true
		var err error
		t, err = store.Update(ctx, t.ID, func(u *threads.Thread) {
			u.LocalSessionID = sessionID
			u.StagedKickoff = ""
			u.SnoozedUntil = time.Time{}
			u.Workdir = workdir
			if agent {
				u.Mode = threads.ModeAuto
			}
			if u.State == threads.StatePending {
				u.State = threads.StateDelivered
			}
		})
		if err != nil {
			return res, t, err
		}
	}
	res.SessionID = t.LocalSessionID
	res.Fallback = ghapp.ResumeCommand(t.LocalSessionID)
	if err := app.Open(ctx, t.LocalSessionID); err != nil {
		res.OpenError = err.Error()
	} else {
		res.Opened = true
		if u, err := store.Update(ctx, t.ID, func(u *threads.Thread) { u.Opened = true }); err == nil {
			t = u
		}
	}
	return res, t, nil
}

func say(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", args...)
	}
}

// ParseSnooze reads a snooze length: a Go duration (2h, 45m), days (1d),
// or off (or 0) to clear it. Empty means def.
func ParseSnooze(s string, def time.Duration) (time.Duration, error) {
	switch strings.TrimSpace(s) {
	case "":
		return def, nil
	case "off", "0":
		return 0, nil
	}
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q: %w", s, ErrBadDuration)
	}
	return d, nil
}

// Snooze hides t's staged message until now+d, or brings it back when d is
// zero. It returns the time it is hidden until (zero when cleared).
func Snooze(ctx context.Context, store *threads.Store, t threads.Thread, now time.Time, d time.Duration) (time.Time, error) {
	if t.StagedKickoff == "" {
		return time.Time{}, fmt.Errorf("thread %s: %w", t.ID, ErrNothingStaged)
	}
	var until time.Time
	if d > 0 {
		until = now.Add(d).Truncate(time.Second)
	}
	if _, err := store.Update(ctx, t.ID, func(u *threads.Thread) { u.SnoozedUntil = until }); err != nil {
		return time.Time{}, err
	}
	return until, nil
}
