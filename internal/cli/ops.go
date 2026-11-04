package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/inboxui"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// newDaemonCommand runs or controls the message subscriber.
func (a *App) newDaemonCommand(c *common) *cobra.Command {
	var once, pause, resume, verbose bool
	cmd := command("daemon", "Run the message subscriber")
	cmd.Flags().BoolVar(&once, "once", false, "process what is waiting, then exit when idle")
	cmd.Flags().BoolVar(&pause, "pause", false, "turn on the local kill switch and exit")
	cmd.Flags().BoolVar(&resume, "resume", false, "turn off the local kill switch and exit")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "debug logging")
	cmd.Args = noArgs("wormhole daemon [--once] [--pause|--resume]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		switch {
		case pause && resume:
			return fail(ExitUsage, "--pause and --resume are exclusive")
		case pause:
			if err := daemon.Pause(); err != nil {
				return err
			}
			a.emit(c, map[string]any{"ok": true, "paused": true}, func(w io.Writer) {
				fmt.Fprintln(w, "paused: the daemon leaves messages on the bus until: wormhole daemon --resume")
			})
			return nil
		case resume:
			if err := daemon.Resume(); err != nil {
				return err
			}
			a.emit(c, map[string]any{"ok": true, "paused": false}, func(w io.Writer) { fmt.Fprintln(w, "resumed") })
			return nil
		}
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		level := slog.LevelInfo
		if verbose {
			level = slog.LevelDebug
		}
		log := slog.New(slog.NewTextHandler(a.Stderr, &slog.HandlerOptions{Level: level}))
		store, err := a.OpenStore()
		if err != nil {
			return err
		}
		defer store.Close()
		b, err := a.NewBus(ctx, cfg)
		if err != nil {
			return fail(ExitUnavailable, busErrorFormat, err)
		}
		defer b.Close()
		app := a.NewGHApp(cfg)
		if e, ok := app.(*ghapp.Exec); ok {
			if err := e.Check(); err != nil {
				log.Warn("copilot not found; inbound asks will be staged but auto mode will fail", "err", err)
			}
		}
		ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		opts := daemon.Options{Config: cfg, Bus: b, Store: store, App: app, Log: log, Host: a.Hostname, Now: a.Now}
		if once {
			idle := newIdleTimer(3*time.Second, cancel)
			opts.OnHandled = idle.touch
			defer idle.stop()
		}
		// The inbox page shares the daemon's store, bus and app; its verbs are
		// the daemon package's, so the daemon cannot start it itself.
		var ui *inboxui.Server
		var uiLn net.Listener
		if cfg.Daemon.InboxUI != "" {
			ui, err = inboxui.New(inboxui.Options{Config: cfg, Store: store, Bus: b, App: app, Me: a.me(cfg), Log: log.With("component", "inbox-ui"), Now: a.Now})
			if err != nil {
				return err
			}
			uiLn, err = inboxui.Listen(cfg.Daemon.InboxUI)
			if err != nil {
				return fail(ExitUnavailable, "inbox page: %v", err)
			}
			defer uiLn.Close()
			opts.InboxURL = ui.URL(uiLn)
			if err := writePrivate(config.InboxURLFile(), opts.InboxURL+"\n"); err != nil {
				return err
			}
			defer os.Remove(config.InboxURLFile())
			log.Info("inbox page", "addr", uiLn.Addr().String(), "url_file", config.InboxURLFile())
		}
		d := daemon.New(opts)
		if ui != nil {
			go func() {
				if err := ui.Serve(ctx, uiLn); err != nil {
					log.Warn("inbox page stopped", "err", err)
				}
			}()
		}
		if cfg.Daemon.Tray {
			stop, err := a.startTray(ctx, cfg, store, d, log)
			if err != nil {
				log.Warn("tray indicator unavailable", "err", err)
			} else {
				defer stop()
			}
		}
		if err := d.Run(ctx); err != nil {
			return fail(ExitUnavailable, "%v", err)
		}
		return nil
	}
	return cmd
}

// writePrivate writes a small state file readable by the user only.
func writePrivate(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// inboxURL reads the running daemon's inbox page address.
func inboxURL() (string, error) {
	b, err := os.ReadFile(config.InboxURLFile())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// idleTimer cancels when no activity has been seen for d.
type idleTimer struct {
	t *time.Timer
	d time.Duration
}

// newIdleTimer starts a timer that cancels after a period without activity.
func newIdleTimer(d time.Duration, cancel func()) *idleTimer {
	return &idleTimer{t: time.AfterFunc(d, cancel), d: d}
}

// touch restarts the idle timeout after handled activity.
func (i *idleTimer) touch() { i.t.Reset(i.d) }

// stop releases the idle timer.
func (i *idleTimer) stop() { i.t.Stop() }

// check is one doctor result.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// newDoctorCommand checks configuration, connectivity, and desktop integrations.
func (a *App) newDoctorCommand(c *common) *cobra.Command {
	var deepLink bool
	cmd := command("doctor", "Check configuration and local integrations")
	cmd.Flags().BoolVar(&deepLink, "deep-link", false, "create a scratch session and open it in the app")
	cmd.Args = noArgs("wormhole doctor [--deep-link]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		var checks []check
		add := func(name string, ok bool, detail, hint string) {
			checks = append(checks, check{Name: name, OK: ok, Detail: detail, Hint: hint})
		}

		path := c.config
		if path == "" {
			path = config.DefaultPath()
		}
		cfg, err := config.Load(path)
		if err != nil {
			hint := "run: wormhole init"
			if errors.Is(err, config.ErrWorldReadable) {
				hint = "run: chmod 600 " + path
			} else if errors.Is(err, config.ErrInGitTree) {
				hint = "move the credentials file out of the repository"
			}
			add("config", false, err.Error(), hint)
			return a.doctorReport(c, checks)
		}
		add("config", true, path, "")
		add("paused", !daemon.IsPaused(), config.PauseFile(), "wormhole daemon --resume")

		store, err := a.OpenStore()
		if err != nil {
			add("threads db", false, err.Error(), "")
		} else {
			defer store.Close()
			unopened, _ := store.List(ctx, threads.Filter{Direction: threads.DirectionIn, Unopened: true})
			add("threads db", true, config.ThreadsDBPath(), "")
			if len(unopened) > 0 {
				add("unopened sessions", false, fmt.Sprintf("%d session(s) exist but never opened in the app", len(unopened)), "wormhole open <thread>")
			}
			inbox, _ := store.List(ctx, threads.Filter{State: threads.StatePending})
			add("inbox", true, fmt.Sprintf("%d pending", len(inbox)), "")
		}

		busCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		b, err := a.NewBus(busCtx, cfg)
		if err != nil {
			add("bus", false, err.Error(), "check bus.url and credentials; wormhole login")
		} else {
			defer b.Close()
			add("bus", true, "connected as "+b.User(), "")
			if nb, ok := b.(*natsbus.Bus); ok {
				st, err := nb.Status(busCtx)
				if err != nil {
					add("stream", false, err.Error(), "ask the bus admin to run: wormhole admin init")
				} else {
					add("stream", true, fmt.Sprintf("%s, %d pending", st.ConnectedURL, st.Pending), "")
					add("dead letters", st.DLQ == 0, fmt.Sprintf("%d on %s", st.DLQ, "wh.dlq."+cfg.Handle), "inspect with the nats CLI; they are messages the daemon gave up on")
				}
			}
			if list, err := b.Registry().List(busCtx); err != nil {
				add("registry", false, err.Error(), "ask the bus admin to run: wormhole admin init")
			} else {
				add("registry", true, fmt.Sprintf("%d handle(s)", len(list)), "")
			}
		}

		if p, err := exec.LookPath(cfg.Copilot.Binary); err != nil {
			add("copilot", false, cfg.Copilot.Binary+" not on PATH", "install the Copilot CLI or set copilot.binary")
		} else {
			add("copilot", true, p, "")
		}
		for _, bin := range []string{"xdg-open", "notify-send"} {
			if _, err := exec.LookPath(bin); err != nil {
				add(bin, false, "not on PATH", "optional; opening the app and notifications need it")
			} else {
				add(bin, true, "", "")
			}
		}
		if cfg.Daemon.InboxUI != "" {
			url, err := inboxURL()
			if err != nil {
				add("inbox page", false, "not running", "start the daemon; it serves the page on "+cfg.Daemon.InboxUI)
			} else {
				client := &http.Client{Timeout: 3 * time.Second}
				resp, err := client.Get(strings.Replace(url, "/?t=", "/api/health?t=", 1))
				switch {
				case err != nil:
					add("inbox page", false, err.Error(), "the daemon that wrote "+config.InboxURLFile()+" is gone; restart the daemon")
				case resp.StatusCode != http.StatusOK:
					resp.Body.Close()
					add("inbox page", false, resp.Status, "restart the daemon")
				default:
					resp.Body.Close()
					add("inbox page", true, cfg.Daemon.InboxUI+" (wormhole inbox --ui)", "")
				}
			}
		}
		display := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
		add("display", display, "", "systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS")
		// Without the session bus the Copilot CLI waits on a keychain it cannot
		// reach; M0 measured a two-minute stall per kickoff.
		add("session bus", os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "", "", "DBUS_SESSION_BUS_ADDRESS is unset; copilot stalls on its keychain without it: systemctl --user import-environment DBUS_SESSION_BUS_ADDRESS")

		if deepLink {
			app := a.NewGHApp(cfg)
			id := ghapp.NewSessionID()
			err := app.Create(ctx, ghapp.Session{ID: id, Name: "wormhole doctor", Kickoff: "Reply with the single word: ok", Workdir: cfg.WorkdirFor(""), ExtraArgs: cfg.Copilot.ExtraArgs})
			if err != nil {
				var re *ghapp.RunError
				detail := err.Error()
				if errors.As(err, &re) && re.Output != "" {
					detail += ": " + re.Output
				}
				add("deep-link: create session", false, detail, "")
			} else {
				add("deep-link: create session", true, id, "")
				if err := app.Open(ctx, id); err != nil {
					add("deep-link: open", false, err.Error(), "fallback: "+ghapp.ResumeCommand(id))
				} else {
					add("deep-link: open", true, "check that 'wormhole doctor' appeared in the app sidebar", "if not, the deep link scheme changed; fallback: "+ghapp.ResumeCommand(id))
				}
			}
		}
		return a.doctorReport(c, checks)
	}
	return cmd
}

// doctorReport emits health checks and fails when any check is unhealthy.
func (a *App) doctorReport(c *common, checks []check) error {
	allOK := true
	for _, ch := range checks {
		if !ch.OK {
			allOK = false
		}
	}
	a.emit(c, map[string]any{"ok": allOK, "checks": checks}, func(w io.Writer) {
		for _, ch := range checks {
			mark := "ok  "
			if !ch.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(w, "%s %-24s %s\n", mark, ch.Name, ch.Detail)
			if !ch.OK && ch.Hint != "" {
				fmt.Fprintf(w, "     -> %s\n", ch.Hint)
			}
		}
	})
	if !allOK {
		return &exitError{code: ExitError, err: errors.New("some checks failed")}
	}
	return nil
}

// newLoginCommand installs credentials into the config directory with private modes.
func (a *App) newLoginCommand(c *common) *cobra.Command {
	var creds string
	cmd := command("login", "Install and verify bus credentials")
	cmd.Flags().StringVar(&creds, "creds", "", "NATS .creds file to install (JWT auth)")
	cmd.Args = noArgs("wormhole login [--creds <file>]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		dir := filepath.Dir(cfg.Path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		var dest string
		if creds != "" {
			raw, err := os.ReadFile(creds)
			if err != nil {
				return err
			}
			dest = cfg.Bus.Creds
			if dest == "" {
				dest = filepath.Join(dir, cfg.Handle+".creds")
			}
			if err := os.WriteFile(dest, raw, 0o600); err != nil {
				return err
			}
		} else {
			pw, err := a.readSecret(c, fmt.Sprintf("bus password for %s: ", cfg.Handle))
			if err != nil {
				return err
			}
			if pw == "" {
				return fail(ExitUsage, "empty password")
			}
			dest = cfg.Bus.PasswordFile
			if dest == "" {
				dest = filepath.Join(dir, "password")
			}
			if err := os.WriteFile(dest, []byte(pw+"\n"), 0o600); err != nil {
				return err
			}
		}
		if d, ok := config.InGitTree(dest); ok {
			_ = os.Remove(dest)
			return fail(ExitRefused, "%s is inside the git working tree %s; move your config directory out of it", dest, d)
		}
		// Reload so the new file is picked up, then verify the connection.
		cfg, err = a.loadConfig(c)
		if err != nil {
			return err
		}
		b, err := a.NewBus(cmd.Context(), cfg)
		if err != nil {
			return fail(ExitUnavailable, "credentials saved to %s but the bus refused them: %v", dest, err)
		}
		_ = b.Close()
		a.emit(c, map[string]any{"ok": true, "path": dest}, func(w io.Writer) {
			fmt.Fprintf(w, "credentials saved to %s and verified against %s\n", dest, cfg.Bus.URL)
		})
		return nil
	}
	return cmd
}

// newAdminCommand constructs NATS administration commands.
func (a *App) newAdminCommand(c *common) *cobra.Command {
	admin := command("admin", "Administer the NATS bus")
	admin.RunE = func(_ *cobra.Command, _ []string) error {
		return fail(ExitUsage, "usage: wormhole admin init --url <nats url> [--user admin] [--password-file <file>] [--tls-ca <pem>]")
	}

	var url, user, pwFile, ca string
	init := command("init", "Create the stream and registry bucket")
	init.Flags().StringVar(&url, "url", "", "bus URL")
	init.Flags().StringVar(&user, "user", "admin", "admin user")
	init.Flags().StringVar(&pwFile, "password-file", "", "file holding the admin password (prompted when omitted)")
	init.Flags().StringVar(&ca, "tls-ca", "", "PEM bundle to trust")
	init.Args = noArgs("wormhole admin init --url <nats url>")
	init.RunE = func(cmd *cobra.Command, _ []string) error {
		if url == "" {
			return fail(ExitUsage, "--url is required")
		}
		var pw string
		if pwFile != "" {
			raw, err := os.ReadFile(pwFile)
			if err != nil {
				return err
			}
			// Like the daemon's password_file: the trailing newline every
			// editor and `echo` leave is not part of the password.
			pw = strings.TrimSpace(string(raw))
		} else {
			var err error
			if pw, err = a.readSecret(c, fmt.Sprintf("password for %s: ", user)); err != nil {
				return err
			}
		}
		nc, err := natsbus.AdminConnect(url, user, pw, ca)
		if err != nil {
			return fail(ExitUnavailable, "connect: %v", err)
		}
		defer nc.Close()
		if err := natsbus.EnsureInfrastructure(cmd.Context(), nc); err != nil {
			return fail(ExitUnavailable, "%v", err)
		}
		a.emit(c, map[string]any{"ok": true, "stream": natsbus.StreamName, "bucket": natsbus.BucketName}, func(w io.Writer) {
			fmt.Fprintf(w, "stream %s and bucket %s are ready on %s\n", natsbus.StreamName, natsbus.BucketName, url)
		})
		return nil
	}
	admin.AddCommand(init)
	return admin
}
