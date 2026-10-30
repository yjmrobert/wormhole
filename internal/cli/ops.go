package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

func (a *App) daemon(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	c.bind(fs)
	once := fs.Bool("once", false, "process what is waiting, then exit when idle")
	pause := fs.Bool("pause", false, "turn on the local kill switch and exit")
	resume := fs.Bool("resume", false, "turn off the local kill switch and exit")
	verbose := fs.Bool("verbose", false, "debug logging")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	switch {
	case *pause && *resume:
		return fail(ExitUsage, "--pause and --resume are exclusive")
	case *pause:
		if err := daemon.Pause(); err != nil {
			return err
		}
		a.emit(c, map[string]any{"ok": true, "paused": true}, func(w io.Writer) {
			fmt.Fprintln(w, "paused: the daemon leaves messages on the bus until: wormhole daemon --resume")
		})
		return nil
	case *resume:
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
	if *verbose {
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
		return fail(ExitUnavailable, "bus: %v", err)
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
	if *once {
		idle := newIdleTimer(3*time.Second, cancel)
		opts.OnHandled = idle.touch
		defer idle.stop()
	}
	d := daemon.New(opts)
	if err := d.Run(ctx); err != nil {
		return fail(ExitUnavailable, "%v", err)
	}
	return nil
}

// idleTimer cancels when no activity has been seen for d.
type idleTimer struct {
	t *time.Timer
	d time.Duration
}

func newIdleTimer(d time.Duration, cancel func()) *idleTimer {
	return &idleTimer{t: time.AfterFunc(d, cancel), d: d}
}

func (i *idleTimer) touch() { i.t.Reset(i.d) }
func (i *idleTimer) stop()  { i.t.Stop() }

// check is one doctor result.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func (a *App) doctor(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	c.bind(fs)
	deepLink := fs.Bool("deep-link", false, "create a scratch session and open it in the app")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
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
	display := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	add("display", display, "", "systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS")

	if *deepLink {
		app := a.NewGHApp(cfg)
		id := ghapp.NewSessionID()
		err := app.Create(ctx, ghapp.Session{ID: id, Name: "wormhole doctor", Kickoff: "Reply with the single word: ok", Workdir: cfg.WorkdirFor(""), ExtraArgs: cfg.Copilot.ExtraArgs})
		if err != nil {
			add("deep-link: create session", false, err.Error(), "")
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

// login installs credentials into the config directory with private modes.
func (a *App) login(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	c.bind(fs)
	creds := fs.String("creds", "", "NATS .creds file to install (JWT auth)")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	cfg, err := a.loadConfig(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var dest string
	if *creds != "" {
		raw, err := os.ReadFile(*creds)
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
		pw, err := a.readSecret(fmt.Sprintf("bus password for %s: ", cfg.Handle))
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
	b, err := a.NewBus(ctx, cfg)
	if err != nil {
		return fail(ExitUnavailable, "credentials saved to %s but the bus refused them: %v", dest, err)
	}
	_ = b.Close()
	a.emit(c, map[string]any{"ok": true, "path": dest}, func(w io.Writer) {
		fmt.Fprintf(w, "credentials saved to %s and verified against %s\n", dest, cfg.Bus.URL)
	})
	return nil
}

func (a *App) initCmd(ctx context.Context, c *common, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	c.bind(fs)
	force := fs.Bool("force", false, "overwrite an existing config")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	path := c.config
	if path == "" {
		path = config.DefaultPath()
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fail(ExitRefused, "%s exists; pass --force to overwrite", path)
	}
	if d, ok := config.InGitTree(path); ok {
		return fail(ExitRefused, "%s is inside the git working tree %s; config must live outside any repository", path, d)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Point the example's credential paths at the directory actually in use.
	example := strings.ReplaceAll(config.Example, "~/.config/wormhole", filepath.Dir(path))
	if err := os.WriteFile(path, []byte(example), 0o600); err != nil {
		return err
	}
	a.emit(c, map[string]any{"ok": true, "path": path}, func(w io.Writer) {
		fmt.Fprintf(w, "wrote %s (mode 600)\nedit handle, bus.url and senders, then run: wormhole login && wormhole doctor\n", path)
	})
	return nil
}

func (a *App) admin(ctx context.Context, c *common, args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return fail(ExitUsage, "usage: wormhole admin init --url <nats url> [--user admin] [--password-file <file>] [--tls-ca <pem>]")
	}
	fs := flag.NewFlagSet("admin init", flag.ContinueOnError)
	c.bind(fs)
	url := fs.String("url", "", "bus URL")
	user := fs.String("user", "admin", "admin user")
	pwFile := fs.String("password-file", "", "file holding the admin password (prompted when omitted)")
	ca := fs.String("tls-ca", "", "PEM bundle to trust")
	if _, err := a.parse(fs, args[1:]); err != nil {
		return err
	}
	if *url == "" {
		return fail(ExitUsage, "--url is required")
	}
	var pw string
	if *pwFile != "" {
		raw, err := os.ReadFile(*pwFile)
		if err != nil {
			return err
		}
		// Like the daemon's password_file: the trailing newline every
		// editor and `echo` leave is not part of the password.
		pw = strings.TrimSpace(string(raw))
	} else {
		var err error
		if pw, err = a.readSecret(fmt.Sprintf("password for %s: ", *user)); err != nil {
			return err
		}
	}
	nc, err := natsbus.AdminConnect(*url, *user, pw, *ca)
	if err != nil {
		return fail(ExitUnavailable, "connect: %v", err)
	}
	defer nc.Close()
	if err := natsbus.EnsureInfrastructure(ctx, nc); err != nil {
		return fail(ExitUnavailable, "%v", err)
	}
	a.emit(c, map[string]any{"ok": true, "stream": natsbus.StreamName, "bucket": natsbus.BucketName}, func(w io.Writer) {
		fmt.Fprintf(w, "stream %s and bucket %s are ready on %s\n", natsbus.StreamName, natsbus.BucketName, *url)
	})
	return nil
}
