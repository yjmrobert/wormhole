package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/serve"
)

// `wormhole join <invite>` is one machine joining a bus: it redeems the
// invite for a credential, writes the config and the credential with
// private modes, verifies the connection, installs the skill into every
// agent it finds, starts the daemon as a user service and prints the one
// thing to do next.

const (
	caFileName       = "bus-ca.pem"
	passwordFileName = "password"
	joinTimeout      = 15 * time.Second
)

// joinOptions are join's flags.
type joinOptions struct {
	noSkill   bool
	noService bool
	// hostname overrides the host reported to the bus (tests).
	hostname string
	// timeout bounds the exchange (tests); 0 means joinTimeout.
	timeout time.Duration
}

// joinResult is what join reports.
type joinResult struct {
	OK           bool           `json:"ok"`
	Handle       string         `json:"handle"`
	URL          string         `json:"url"`
	Config       string         `json:"config"`
	PasswordFile string         `json:"password_file"`
	TLSCA        string         `json:"tls_ca"`
	SendersAdded []string       `json:"senders_added"`
	Skills       []skillInstall `json:"skills,omitempty"`
	Service      *serviceResult `json:"service,omitempty"`
	// Next is the command to run now.
	Next string `json:"next"`
	// Peer is the handle Next names, online or the inviter.
	Peer       string `json:"peer,omitempty"`
	PeerOnline bool   `json:"peer_online"`
}

// newJoinCommand joins this machine to a bus from an invite.
func (a *App) newJoinCommand(c *common) *cobra.Command {
	var o joinOptions
	cmd := command("join <invite>", "Join a bus from an invite: config, credential, skill, daemon service")
	cmd.Long = "Redeems a one-time invite from `wormhole serve invite` for a credential, writes config.yaml and\nthe credential with private modes, verifies the connection, installs the skill into every agent\nfound on this machine and starts the daemon as a user service. Run it again with a new invite\nto rotate the credential; everything else in the config is left alone."
	cmd.Flags().BoolVar(&o.noSkill, "no-skill", false, "do not install the skill into detected agents")
	cmd.Flags().BoolVar(&o.noService, "no-service", false, "do not install or start the daemon service")
	cmd.Args = exactArgs(1, "wormhole join <invite>")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var w io.Writer
		if !c.json {
			w = a.Stdout
		}
		res, err := a.joinWithInvite(cmd.Context(), c, args[0], o, w)
		if err != nil {
			return err
		}
		a.emit(c, res, func(w io.Writer) {
			fmt.Fprintf(w, "joined %s as ^%s\n", res.URL, res.Handle)
			if len(res.SendersAdded) > 0 {
				fmt.Fprintf(w, "^%s may ask you; their asks wait in your inbox until you read them (undo: wormhole senders remove %s)\n", res.SendersAdded[0], res.SendersAdded[0])
			}
			switch {
			case res.PeerOnline:
				fmt.Fprintf(w, "ask them something: %s\n", res.Next)
			case res.Peer != "":
				fmt.Fprintf(w, "when ^%s is online: %s\n", res.Peer, res.Next)
			default:
				fmt.Fprintf(w, "next: %s\n", res.Next)
			}
		})
		return nil
	}
	return cmd
}

// joinWithInvite does the work of join; init's wizard calls it when an
// invite is pasted.
func (a *App) joinWithInvite(ctx context.Context, c *common, token string, o joinOptions, w io.Writer) (joinResult, error) {
	inv, err := serve.Decode(token)
	if err != nil {
		return joinResult{}, fail(ExitUsage, "%v", err)
	}
	by := inv.By
	if by == "" {
		by = "the person running wormhole serve"
	} else {
		by = "^" + by
	}
	if inv.Expired(a.Now()) {
		return joinResult{}, fail(ExitRefused, "this invite expired at %s; ask %s for a new one: wormhole serve invite %s", time.Unix(inv.Expires, 0).Local().Format(time.RFC822), by, inv.Handle)
	}
	path := c.config
	if path == "" {
		path = config.DefaultPath()
	}
	dir := filepath.Dir(path)
	if d, ok := config.InGitTree(path); ok {
		return joinResult{}, fail(ExitRefused, "%s is inside the git working tree %s; credentials must not live in a repository. Set WORMHOLE_CONFIG_DIR, or run join from your home directory", path, d)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return joinResult{}, err
	}
	caPath := filepath.Join(dir, caFileName)
	if err := config.WritePrivate(caPath, serve.CAPEM(inv.CADER())); err != nil {
		return joinResult{}, err
	}
	host := o.hostname
	if host == "" {
		host = a.Hostname
	}
	say(w, "contacting the bus at %s", firstHost(inv.URLs))
	timeout := o.timeout
	if timeout == 0 {
		timeout = joinTimeout
	}
	red, err := serve.Redeem(ctx, inv, caPath, host, timeout)
	if err != nil {
		if errors.Is(err, serve.ErrRefused) {
			return joinResult{}, fail(ExitRefused, "%v", err)
		}
		return joinResult{}, fail(ExitUnavailable, "could not reach the bus (%v); is wormhole serve running on %s? ask %s to check: wormhole serve users", err, firstHost(inv.URLs), by)
	}
	pwPath := filepath.Join(dir, passwordFileName)
	if err := config.WritePrivate(pwPath, []byte(red.Password+"\n")); err != nil {
		return joinResult{}, err
	}
	urls := orderURLs(inv.URLs, red.URL)
	_, statErr := os.Stat(path)
	fresh := errors.Is(statErr, os.ErrNotExist)
	res := joinResult{OK: true, Handle: inv.Handle, URL: urls[0], Config: path, PasswordFile: pwPath, TLSCA: caPath, SendersAdded: []string{}}
	err = config.Edit(path, true, func(doc *yaml.Node) error {
		config.Set(doc, "handle", inv.Handle)
		config.Set(doc, "bus.kind", "nats")
		config.Set(doc, "bus.url", strings.Join(urls, ","))
		config.Set(doc, "bus.password_file", pwPath)
		config.Set(doc, "bus.tls_ca", caPath)
		config.Unset(doc, "bus.creds")
		config.Unset(doc, "bus.user")
		if fresh || !config.Has(doc, "senders") {
			// The example's placeholder senders mean nothing here.
			config.SetList(doc, "senders", nil)
		}
		if inv.By != "" && inv.By != inv.Handle && config.AddSenderNode(doc, inv.By, config.ModeInbox) {
			res.SendersAdded = append(res.SendersAdded, inv.By)
		}
		return nil
	})
	if err != nil {
		return joinResult{}, wrap(ExitError, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return joinResult{}, wrap(ExitError, err)
	}
	say(w, "wrote %s and the credential (mode 600)", path)
	b, err := a.NewBus(ctx, cfg)
	if err != nil {
		return joinResult{}, fail(ExitUnavailable, "joined but the daemon's connection failed: %v; run: wormhole doctor", err)
	}
	res.Peer, res.PeerOnline = a.pickPeer(ctx, b, cfg, inv.By)
	_ = b.Close()
	if res.Peer != "" {
		res.Next = fmt.Sprintf("wormhole ask ^%s \"...\"", res.Peer)
	} else {
		res.Next = "wormhole users   # then: wormhole ask ^<handle> \"...\""
	}
	if err := a.afterJoin(ctx, c, o, &res, w); err != nil {
		return res, err
	}
	return res, nil
}

// afterJoin installs the skill and the daemon service. Neither failure
// undoes the join: the config and credential are in place and the error
// names what to run.
func (a *App) afterJoin(ctx context.Context, c *common, o joinOptions, res *joinResult, w io.Writer) error {
	if !o.noSkill {
		list, err := a.installSkills(ctx, "", "", w)
		var ee *exitError
		switch {
		case errors.As(err, &ee) && ee.code == ExitNotFound:
			say(w, "no agent found on this machine; when you install one: wormhole skill install")
		case err != nil:
			return wrap(ExitError, err)
		}
		res.Skills = list
	}
	if !o.noService {
		running := false
		if st := a.serviceStatus(ctx); st.Active == "active" {
			running = true
		}
		svc, err := a.installDaemonService(ctx, c, w)
		if err != nil {
			return err
		}
		if running && svc.Installed {
			// enable --now leaves a running daemon on its old credential.
			_ = a.restartService(ctx, daemonUnit)
		}
		res.Service = &svc
	}
	return nil
}

// pickPeer chooses who to ask first: the inviter when online, else any
// other online handle, else the inviter.
func (a *App) pickPeer(ctx context.Context, b bus.Bus, cfg *config.Config, by string) (string, bool) {
	list, err := b.Registry().List(ctx)
	if err != nil {
		return by, false
	}
	var online []string
	for _, p := range list {
		if p.Handle != cfg.Handle && a.Now().Sub(p.LastSeen) < 2*cfg.Daemon.RegistryInterval {
			online = append(online, p.Handle)
		}
	}
	for _, h := range online {
		if h == by {
			return h, true
		}
	}
	if len(online) > 0 {
		return online[0], true
	}
	return by, false
}

// firstHost is the host of the first URL, for messages.
func firstHost(urls []string) string {
	if len(urls) == 0 {
		return "the bus"
	}
	if u, err := url.Parse(urls[0]); err == nil && u.Host != "" {
		return u.Host
	}
	return urls[0]
}

// orderURLs puts the address that answered first, keeping the rest in
// the order the invite gave them.
func orderURLs(urls []string, first string) []string {
	if first == "" {
		return urls
	}
	out := []string{first}
	for _, u := range urls {
		if u != first {
			out = append(out, u)
		}
	}
	return out
}

// say prints a progress line when there is somewhere to print it.
func say(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", args...)
	}
}
