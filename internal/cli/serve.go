package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/serve"
	"github.com/yjmrobert/wormhole/internal/systemd"
)

// `wormhole serve` hosts the bus: an embedded NATS server with its own TLS
// material and users. `serve install` runs it as a user service and joins
// this machine to it; `serve invite` mints the one-time token another
// machine joins with.

const serveStartTimeout = 15 * time.Second

// serveStateDir is where the embedded bus keeps its material.
func serveStateDir() string { return filepath.Join(config.StateDir(), "serve") }

// serveFlags are shared by serve and serve install.
type serveFlags struct {
	listen    string
	advertise []string
	invites   []string
	handle    string
	ttl       time.Duration
}

// serveStatus is the JSON serve prints before it blocks.
type serveStatus struct {
	OK       bool         `json:"ok"`
	URLs     []string     `json:"urls"`
	StateDir string       `json:"state_dir"`
	CA       string       `json:"ca_fingerprint"`
	Invites  []inviteLine `json:"invites"`
}

// inviteLine is one minted invite.
type inviteLine struct {
	Handle  string `json:"handle"`
	Token   string `json:"token"`
	Expires string `json:"expires"`
}

// newServeCommand hosts the bus.
func (a *App) newServeCommand(c *common) *cobra.Command {
	var f serveFlags
	cmd := command("serve", "Host the bus: an embedded NATS server with TLS, users and invites")
	cmd.Long = "Runs the bus in this process until interrupted. The first run generates a CA, a server certificate\nand the admin credential under the state directory; later runs reuse them. `serve install` runs\nit as a user service instead and joins this machine; `serve invite` mints invites for the others."
	cmd.Flags().StringVar(&f.listen, "listen", serve.DefaultListen, "address to listen on (host:port)")
	cmd.Flags().StringArrayVar(&f.advertise, "advertise", nil, "extra host[:port] other machines reach this one by; put in invites and the certificate")
	cmd.Flags().StringArrayVar(&f.invites, "invite", nil, "mint an invite for this handle at start (repeatable)")
	cmd.Flags().DurationVar(&f.ttl, "ttl", serve.DefaultInviteTTL, "how long minted invites stay valid")
	cmd.Args = noArgs("wormhole serve [--listen :4222] [--advertise host] [--invite <handle>]...")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return a.runServe(cmd.Context(), c, f)
	}
	cmd.AddCommand(a.newServeInstallCommand(c), a.newServeInviteCommand(c), a.newServeUsersCommand(c), a.newServeRevokeCommand(c))
	return cmd
}

// runServe starts the bus in the foreground.
func (a *App) runServe(ctx context.Context, c *common, f serveFlags) error {
	log := slog.New(slog.NewTextHandler(a.Stderr, nil))
	if c.json {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	s, err := serve.Start(ctx, serve.Options{StateDir: serveStateDir(), Listen: f.listen, Advertise: f.advertise, Hostname: a.Hostname, Log: log, Now: a.Now})
	if err != nil {
		return fail(ExitUnavailable, "%v", err)
	}
	defer s.Close()
	st := serveStatus{OK: true, URLs: s.ClientURLs(), StateDir: s.StateDir(), CA: s.CAFingerprint()}
	by := a.localHandle(c)
	for _, h := range f.invites {
		line, err := a.mintInvite(h, by, f.ttl, false, s.ClientURLs())
		if err != nil {
			return err
		}
		st.Invites = append(st.Invites, line)
	}
	a.emit(c, st, func(w io.Writer) { a.printServeStatus(w, st, true) })
	<-ctx.Done()
	return nil
}

// printServeStatus tells the person what is running and what to copy.
func (a *App) printServeStatus(w io.Writer, st serveStatus, foreground bool) {
	fmt.Fprintf(w, "bus listening on %s\n", strings.Join(st.URLs, " "))
	fmt.Fprintf(w, "state: %s (CA %.16s)\n", st.StateDir, st.CA)
	for _, inv := range st.Invites {
		fmt.Fprintf(w, "\non %s's machine run (valid until %s):\n  wormhole join %s\n", inv.Handle, inv.Expires, inv.Token)
	}
	fmt.Fprintf(w, "\ninvite someone with: wormhole serve invite <handle>\n")
	if foreground {
		fmt.Fprintf(w, "run it as a service instead: wormhole serve install\n")
	}
}

// localHandle is this machine's handle when a config loads, for `by`.
func (a *App) localHandle(c *common) string {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return ""
	}
	return cfg.Handle
}

// mintInvite mints one invite and formats it.
func (a *App) mintInvite(handle, by string, ttl time.Duration, rotate bool, urls []string) (inviteLine, error) {
	if !envelope.ValidHandle(handle) {
		return inviteLine{}, fail(ExitNotFound, invalidHandleFormat, handle)
	}
	tok, inv, err := serve.Mint(serveStateDir(), handle, serve.InviteOptions{By: by, TTL: ttl, Rotate: rotate, URLs: urls, Now: a.Now})
	if err != nil {
		if errors.Is(err, serve.ErrUserExists) {
			return inviteLine{}, fail(ExitRefused, "%v", err)
		}
		return inviteLine{}, fail(ExitUnavailable, "%v", err)
	}
	return inviteLine{Handle: handle, Token: tok, Expires: time.Unix(inv.Expires, 0).Local().Format(time.RFC822)}, nil
}

// newServeInviteCommand mints an invite.
func (a *App) newServeInviteCommand(c *common) *cobra.Command {
	var ttl time.Duration
	var by string
	var rotate bool
	cmd := command("invite <handle>", "Mint a one-time invite and print the join command to send")
	cmd.Flags().DurationVar(&ttl, "ttl", serve.DefaultInviteTTL, "how long the invite stays valid")
	cmd.Flags().StringVar(&by, "by", "", "the inviting handle (default: this machine's handle); seeded as the invitee's first sender")
	cmd.Flags().BoolVar(&rotate, "rotate", false, "allow an invite for a handle already on the bus; their join replaces the credential")
	cmd.Args = exactArgs(1, "wormhole serve invite <handle> [--ttl 24h] [--rotate]")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if by == "" {
			by = a.localHandle(c)
		}
		if by == args[0] {
			by = ""
		}
		line, err := a.mintInvite(args[0], by, ttl, rotate, nil)
		if err != nil {
			return err
		}
		a.emit(c, map[string]any{"ok": true, "handle": line.Handle, "token": line.Token, "expires": line.Expires, "by": by}, func(w io.Writer) {
			fmt.Fprintf(w, "on %s's machine run (valid until %s):\n  wormhole join %s\n", line.Handle, line.Expires, line.Token)
		})
		return nil
	}
	return cmd
}

// newServeUsersCommand lists users and pending invites.
func (a *App) newServeUsersCommand(c *common) *cobra.Command {
	cmd := command("users", "List the users on this bus and the pending invites")
	cmd.Args = noArgs("wormhole serve users")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		users, err := serve.ListUsers(serveStateDir())
		if err != nil {
			return wrap(ExitError, err)
		}
		invites, err := serve.ListInvites(serveStateDir())
		if err != nil {
			return wrap(ExitError, err)
		}
		a.emit(c, map[string]any{"ok": true, "users": users, "invites": invites}, func(w io.Writer) {
			if len(users) == 0 && len(invites) == 0 {
				fmt.Fprintf(w, "nobody yet; mint an invite with: wormhole serve invite <handle>\n")
				return
			}
			rows := make([][]string, 0, len(users)+len(invites))
			for _, u := range users {
				rows = append(rows, []string{"^" + u.Handle, "user", "since " + u.Created.Local().Format("2006-01-02")})
			}
			for _, i := range invites {
				state := "invited"
				if !a.Now().Before(i.Expires) {
					state = "invite expired"
				}
				by := ""
				if i.By != "" {
					by = "by ^" + i.By + ", "
				}
				rows = append(rows, []string{"^" + i.Handle, state, by + "until " + i.Expires.Local().Format(time.RFC822)})
			}
			table(w, []string{"HANDLE", "STATE", "DETAIL"}, rows)
		})
		return nil
	}
	return cmd
}

// newServeRevokeCommand removes a user.
func (a *App) newServeRevokeCommand(c *common) *cobra.Command {
	cmd := command("revoke <handle>", "Remove a user (and any invite for them); the running bus reloads")
	cmd.Args = exactArgs(1, "wormhole serve revoke <handle>")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := serve.Revoke(serveStateDir(), args[0]); err != nil {
			if errors.Is(err, serve.ErrNotFound) {
				return fail(ExitNotFound, "%v", err)
			}
			return wrap(ExitError, err)
		}
		a.emit(c, map[string]any{"ok": true, "handle": args[0]}, func(w io.Writer) {
			fmt.Fprintf(w, "^%s removed; their daemon is disconnected within seconds. Their messages stay on the bus until: nats stream purge WORMHOLE --subject 'wh.inbox.%s.>'\n", args[0], args[0])
		})
		return nil
	}
	return cmd
}

// serveInstallResult is what serve install reports.
type serveInstallResult struct {
	serveStatus
	Service serviceResult `json:"service"`
	Joined  *joinResult   `json:"joined,omitempty"`
}

// newServeInstallCommand runs the bus as a user service and joins this
// machine to it.
func (a *App) newServeInstallCommand(c *common) *cobra.Command {
	var f serveFlags
	var noJoin bool
	cmd := command("install", "Run the bus as a systemd user service, join this machine, print invites")
	cmd.Long = "Writes the wormhole-serve user unit, starts it, joins this machine to the bus under --handle\n(default: $USER) when no config exists yet, and mints an invite per --invite handle."
	cmd.Flags().StringVar(&f.listen, "listen", serve.DefaultListen, "address to listen on (host:port)")
	cmd.Flags().StringArrayVar(&f.advertise, "advertise", nil, "extra host[:port] other machines reach this one by")
	cmd.Flags().StringArrayVar(&f.invites, "invite", nil, "mint an invite for this handle (repeatable)")
	cmd.Flags().StringVar(&f.handle, "handle", "", "this machine's handle on the bus (default: $USER)")
	cmd.Flags().DurationVar(&f.ttl, "ttl", serve.DefaultInviteTTL, "how long minted invites stay valid")
	cmd.Flags().BoolVar(&noJoin, "no-join", false, "do not join this machine to the bus")
	cmd.Args = noArgs("wormhole serve install [--invite <handle>]... [--handle <me>]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return a.runServeInstall(cmd.Context(), c, f, noJoin)
	}
	return cmd
}

// runServeInstall is serve install.
func (a *App) runServeInstall(ctx context.Context, c *common, f serveFlags, noJoin bool) error {
	var w io.Writer
	if !c.json {
		w = a.Stdout
	}
	urls, err := serve.Prepare(serve.Options{StateDir: serveStateDir(), Listen: f.listen, Advertise: f.advertise, Hostname: a.Hostname, Now: a.Now})
	if err != nil {
		return fail(ExitUnavailable, "%v", err)
	}
	exec := []string{a.executable(), "serve", "--listen", f.listen}
	for _, adv := range f.advertise {
		exec = append(exec, "--advertise", adv)
	}
	unit := systemd.Unit{
		Name:        serveUnit,
		Description: "Wormhole bus (embedded NATS server)",
		ExecStart:   exec,
		After:       []string{"network-online.target"},
		Wants:       []string{"network-online.target"},
		WantedBy:    []string{"default.target"},
		Restart:     "always",
		RestartSec:  5,
	}
	fallback := fmt.Sprintf("no systemd user session here; run the bus in a terminal instead: wormhole serve --listen %s", f.listen)
	svc, err := systemd.InstallUser(ctx, a.systemctl(), unit, nil, fallback)
	res := serveInstallResult{serveStatus: serveStatus{OK: true, URLs: urls, StateDir: serveStateDir()}, Service: serviceResult{OK: true, Result: svc, Unit: serveUnit}}
	switch {
	case errors.Is(err, systemd.ErrNoUserSession):
		say(w, "wrote %s; %s", svc.Path, fallback)
	case err != nil:
		return fail(ExitUnavailable, "systemd refused the bus unit: %v; the unit is at %s, start it by hand with: systemctl --user enable --now %s", err, svc.Path, serveUnit)
	default:
		// A unit that was already running keeps serving; make sure it
		// picked up the new flags.
		if rerr := a.restartService(ctx, serveUnit); rerr != nil {
			say(w, "could not restart %s: %v", serveUnit, rerr)
		}
		say(w, "bus running as a user service (%s); logs: journalctl --user -u %s -f", svc.Path, serveUnit)
		say(w, "keep it running after you log out: loginctl enable-linger")
		if err := waitForBus(ctx, f.listen, serveStartTimeout); err != nil {
			return fail(ExitUnavailable, "the bus did not come up on %s: %v; check: journalctl --user -u %s", f.listen, err, serveUnit)
		}
	}
	ca, err := os.ReadFile(filepath.Join(serveStateDir(), "ca.crt"))
	if err == nil {
		res.CA = serve.FingerprintPEM(ca)
	}
	running := svc.Installed
	// Join this machine first so the invites carry its handle as `by`.
	me := f.handle
	if me == "" {
		me = defaultHandle(a.Getenv)
	}
	if !noJoin && running {
		path := c.config
		if path == "" {
			path = config.DefaultPath()
		}
		if _, err := os.Stat(path); err == nil {
			say(w, "this machine already has %s; not joining again (rotate with: wormhole serve invite %s --rotate && wormhole join <invite>)", path, a.localHandle(c))
		} else if !envelope.ValidHandle(me) {
			say(w, "cannot pick a handle for this machine from $USER=%q; join it with: wormhole serve install --handle <handle>", me)
		} else {
			line, err := a.mintInvite(me, "", f.ttl, false, urls)
			if err != nil {
				return err
			}
			joined, err := a.joinWithInvite(ctx, c, line.Token, joinOptions{}, w)
			if err != nil {
				return err
			}
			res.Joined = &joined
		}
	}
	by := a.localHandle(c)
	for _, h := range f.invites {
		line, err := a.mintInvite(h, by, f.ttl, false, urls)
		if err != nil {
			return err
		}
		res.Invites = append(res.Invites, line)
	}
	a.emit(c, res, func(w io.Writer) {
		a.printServeStatus(w, res.serveStatus, false)
		if res.Joined != nil {
			fmt.Fprintf(w, "this machine joined as ^%s\n", res.Joined.Handle)
		}
	})
	return nil
}

// waitForBus waits until something accepts connections on listen.
func waitForBus(ctx context.Context, listen string, timeout time.Duration) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}
