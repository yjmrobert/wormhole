package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/serve"
)

// serveBcryptCost hashes invite secrets and generated passwords; zero is
// serve.DefaultBcryptCost. Tests lower it.
var serveBcryptCost = 0

// servedInvite is one invite printed by serve and serve invite.
type servedInvite struct {
	Handle  string    `json:"handle"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// newServeCommand runs the embedded bus and manages its users.
// `wormhole serve install` (the systemd user unit) is added beside the
// subcommands below at integration, on top of the shared systemd helper.
func (a *App) newServeCommand(c *common) *cobra.Command {
	var listen string
	var advertise, invites []string
	cmd := command("serve", "Run the bus for a small team on this machine")
	cmd.Long = "Run an embedded NATS JetStream bus with its own TLS material and users, in the foreground until interrupted.\nState lives in " + serve.DefaultStateDir() + "."
	cmd.Flags().StringVar(&listen, "listen", serve.DefaultListen, "host:port to listen on")
	cmd.Flags().StringArrayVar(&advertise, "advertise", nil, "extra host[:port] the others should use (DNS name, VPN address); repeatable")
	cmd.Flags().StringArrayVar(&invites, "invite", nil, "mint an invite for this handle at start; repeatable")
	cmd.Args = noArgs("wormhole serve [--listen :4222] [--advertise host[:port]] [--invite <handle>]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		for _, h := range invites {
			if err := checkInviteHandle(h); err != nil {
				return err
			}
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		log := slog.New(slog.NewTextHandler(a.Stderr, nil))
		dir := serve.DefaultStateDir()
		srv, err := serve.Start(ctx, serve.Options{StateDir: dir, Listen: listen, Advertise: advertise, Log: log, Now: a.Now, BcryptCost: serveBcryptCost})
		if err != nil {
			return fail(ExitUnavailable, "serve: %v", err)
		}
		defer srv.Close()
		by := a.localHandle(c)
		minted := make([]servedInvite, 0, len(invites))
		for _, h := range invites {
			inv, err := a.mintInvite(dir, h, by, serve.DefaultInviteTTL, false, srv.ClientURLs())
			if err != nil {
				return err
			}
			minted = append(minted, inv)
		}
		if len(minted) > 0 {
			if err := srv.Reload(); err != nil {
				return fail(ExitError, "serve: %v", err)
			}
		}
		urls := srv.ClientURLs()
		a.emit(c, map[string]any{"ok": true, "urls": urls, "state_dir": dir, "invites": minted}, func(w io.Writer) {
			fmt.Fprintf(w, "serving the bus on %s\n", strings.Join(urls, ", "))
			fmt.Fprintf(w, "state: %s\n", dir)
			for _, inv := range minted {
				fmt.Fprintf(w, "on %s's machine run: wormhole join %s\n", inv.Handle, inv.Token)
			}
			fmt.Fprintln(w, "invite someone else with: wormhole serve invite <handle>")
		})
		<-ctx.Done()
		return nil
	}
	cmd.AddCommand(a.newServeInviteCommand(c), a.newServeUsersCommand(c), a.newServeRevokeCommand(c))
	return cmd
}

// checkInviteHandle rejects an invalid handle before anything starts.
func checkInviteHandle(h string) error {
	return validateHandle(strings.TrimPrefix(h, "^"))
}

// validateHandle reports a usage error for a malformed handle.
func validateHandle(h string) error {
	if !envelope.ValidHandle(h) {
		return fail(ExitUsage, invalidHandleFormat+"; handles are lowercase letters, digits, - and _, such as bob", h)
	}
	return nil
}

// localHandle is this machine's handle when a config loads, else empty.
func (a *App) localHandle(c *common) string {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return ""
	}
	return cfg.Handle
}

// mintInvite runs serve.NewInvite and maps its errors to exit codes.
func (a *App) mintInvite(dir, handle, by string, ttl time.Duration, rotate bool, urls []string) (servedInvite, error) {
	handle = strings.TrimPrefix(handle, "^")
	by = strings.TrimPrefix(by, "^")
	now := a.Now()
	tok, err := serve.NewInvite(dir, handle, by, ttl, now, urls, serve.InviteOptions{Rotate: rotate, BcryptCost: serveBcryptCost})
	switch {
	case errors.Is(err, serve.ErrExists):
		return servedInvite{}, fail(ExitRefused, "%v", err)
	case errors.Is(err, serve.ErrNotServing):
		return servedInvite{}, fail(ExitUnavailable, "%v", err)
	case err != nil && strings.Contains(err.Error(), "handle"):
		return servedInvite{}, fail(ExitUsage, "%v", err)
	case err != nil:
		return servedInvite{}, fail(ExitError, "%v", err)
	}
	inv, err := serve.Decode(tok)
	if err != nil {
		return servedInvite{}, fail(ExitError, "%v", err)
	}
	return servedInvite{Handle: handle, Token: tok, Expires: inv.Expires().UTC()}, nil
}

// newServeInviteCommand mints an invite; the running server picks it up.
func (a *App) newServeInviteCommand(c *common) *cobra.Command {
	var ttl time.Duration
	var by string
	var rotate bool
	cmd := command("invite <handle>", "Mint a one-time invite for a handle")
	cmd.Flags().DurationVar(&ttl, "ttl", serve.DefaultInviteTTL, "how long the invite stays valid")
	cmd.Flags().StringVar(&by, "by", "", "inviting handle written into the invite (default: this machine's handle)")
	cmd.Flags().BoolVar(&rotate, "rotate", false, "allow an invite for a handle that already joined; the join replaces its credential")
	cmd.Args = exactArgs(1, "wormhole serve invite <handle> [--ttl 24h] [--by <handle>] [--rotate]")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		if err := checkInviteHandle(args[0]); err != nil {
			return err
		}
		if ttl <= 0 {
			return fail(ExitUsage, "--ttl must be positive, such as 24h")
		}
		if by == "" {
			by = a.localHandle(c)
		}
		inv, err := a.mintInvite(serve.DefaultStateDir(), args[0], by, ttl, rotate, nil)
		if err != nil {
			return err
		}
		a.emit(c, map[string]any{"ok": true, "handle": inv.Handle, "token": inv.Token, "expires": inv.Expires}, func(w io.Writer) {
			fmt.Fprintf(w, "on %s's machine run: wormhole join %s\n", inv.Handle, inv.Token)
			fmt.Fprintf(w, "valid until %s, once\n", inv.Expires.Local().Format(time.RFC3339))
		})
		return nil
	}
	return cmd
}

// newServeUsersCommand lists users and pending invites.
func (a *App) newServeUsersCommand(c *common) *cobra.Command {
	cmd := command("users", "List the bus users and pending invites")
	cmd.Args = noArgs("wormhole serve users")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		dir := serve.DefaultStateDir()
		users, err := serve.ListUsers(dir)
		if err != nil {
			return fail(ExitError, "%v", err)
		}
		invites, err := serve.ListInvites(dir)
		if err != nil {
			return fail(ExitError, "%v", err)
		}
		a.emit(c, map[string]any{"ok": true, "users": users, "invites": invites}, func(w io.Writer) {
			if len(users) == 0 && len(invites) == 0 {
				fmt.Fprintln(w, "no users yet; invite someone with: wormhole serve invite <handle>")
				return
			}
			if len(users) > 0 {
				rows := make([][]string, len(users))
				for i, u := range users {
					rows[i] = []string{"^" + u.Handle, u.Created.Local().Format(time.RFC3339)}
				}
				table(w, []string{"USER", "JOINED"}, rows)
			}
			if len(invites) > 0 {
				if len(users) > 0 {
					fmt.Fprintln(w)
				}
				rows := make([][]string, len(invites))
				for i, inv := range invites {
					by := "-"
					if inv.By != "" {
						by = "^" + inv.By
					}
					rows[i] = []string{"^" + inv.Handle, by, inv.Expires.Local().Format(time.RFC3339)}
				}
				table(w, []string{"INVITED", "BY", "EXPIRES"}, rows)
			}
		})
		return nil
	}
	return cmd
}

// newServeRevokeCommand removes a user; the running server drops them.
func (a *App) newServeRevokeCommand(c *common) *cobra.Command {
	cmd := command("revoke <handle>", "Remove a user and their pending invites from the bus")
	cmd.Args = exactArgs(1, "wormhole serve revoke <handle>")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		h := strings.TrimPrefix(args[0], "^")
		if err := validateHandle(h); err != nil {
			return err
		}
		err := serve.Revoke(serve.DefaultStateDir(), h)
		switch {
		case errors.Is(err, serve.ErrNotFound):
			return fail(ExitNotFound, "%v", err)
		case err != nil:
			return fail(ExitError, "%v", err)
		}
		a.emit(c, map[string]any{"ok": true, "handle": h, "revoked": true}, func(w io.Writer) {
			fmt.Fprintf(w, "revoked ^%s; the running bus drops them within %s\n", h, serve.DefaultPollInterval)
		})
		return nil
	}
	return cmd
}
