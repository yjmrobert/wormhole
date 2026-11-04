package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// `wormhole init` asks; it does not hand over a template. On a terminal it
// is a short form (handle, how to reach the bus, who may ask you,
// notifications) with a default for every answer; with --no-input, or off
// a terminal, every question is a flag. An invite, pasted or passed, hands
// over to join. The config is written from the documented example so its
// comments stay.

// Ways to reach the bus.
const (
	busByInvite   = "invite"
	busByPassword = "password"
	busByCreds    = "creds"
)

// initAnswers are the wizard's answers, from the form or the flags.
type initAnswers struct {
	Handle string
	Bus    string // busByInvite, busByPassword or busByCreds
	Invite string
	URL    string
	// PasswordFile is where the password lives; empty means `wormhole
	// login` stores it later.
	PasswordFile string
	Creds        string
	Senders      []string
	Notify       string
}

// initResult is what init reports.
type initResult struct {
	OK     bool     `json:"ok"`
	Path   string   `json:"path"`
	Handle string   `json:"handle"`
	URL    string   `json:"url,omitempty"`
	Next   []string `json:"next"`
	// Missing names what doctor will ask for.
	Missing []string `json:"missing,omitempty"`
}

// newInitCommand writes the configuration, by asking or from flags.
func (a *App) newInitCommand(c *common) *cobra.Command {
	var force bool
	var ans initAnswers
	var senders string
	cmd := command("init", "Set up this machine: a short form, or flags with --no-input")
	cmd.Long = "Writes ~/.config/wormhole/config.yaml (mode 600). On a terminal it asks four questions, each with\na default that works; with --no-input every question is a flag. Paste an invite from\n`wormhole serve invite` and it hands over to `wormhole join`."
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")
	cmd.Flags().StringVar(&ans.Handle, "handle", "", "your handle on the bus (default: $USER)")
	cmd.Flags().StringVar(&ans.Invite, "invite", "", "an invite from wormhole serve invite; joins instead of writing a config by hand")
	cmd.Flags().StringVar(&ans.URL, "url", "", "bus URL, tls://host:4222 (several, comma-separated)")
	cmd.Flags().StringVar(&ans.PasswordFile, "password-file", "", "file holding your bus password (default: wormhole login stores it)")
	cmd.Flags().StringVar(&ans.Creds, "creds", "", "a NATS .creds file instead of a password")
	cmd.Flags().StringVar(&senders, "senders", "", "who may ask you, comma-separated handles (inbox mode)")
	cmd.Flags().StringVar(&ans.Notify, "notify", "", "notifications: toast, actions, digest or off")
	cmd.Args = noArgs("wormhole init [--no-input] [--handle <you>] [--invite <token> | --url <bus> [--creds <file>]] [--senders a,b] [--notify toast]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if senders != "" {
			for _, h := range strings.Split(senders, ",") {
				if h = strings.TrimSpace(h); h != "" {
					ans.Senders = append(ans.Senders, h)
				}
			}
		}
		path := c.config
		if path == "" {
			path = config.DefaultPath()
		}
		if ans.Invite != "" {
			return a.initByInvite(cmd.Context(), c, ans.Invite)
		}
		if _, err := os.Stat(path); err == nil && !force {
			return fail(ExitRefused, "%s exists; change it with: wormhole senders add, wormhole join <invite>, or rewrite it with: wormhole init --force", path)
		}
		if d, ok := config.InGitTree(path); ok {
			return fail(ExitRefused, "%s is inside the git working tree %s; config must live outside any repository. Set WORMHOLE_CONFIG_DIR, or run from your home directory", path, d)
		}
		interactive := !c.noInput && !c.json && a.terminalForms()
		if interactive {
			filled, err := a.askInit(ans)
			if err != nil {
				return err
			}
			ans = filled
			if ans.Bus == busByInvite {
				return a.initByInvite(cmd.Context(), c, ans.Invite)
			}
		}
		res, err := writeInitConfig(path, ans, a.Getenv)
		if err != nil {
			return err
		}
		a.emit(c, res, func(w io.Writer) {
			fmt.Fprintf(w, "wrote %s (mode 600): handle ^%s, bus %s\n", res.Path, res.Handle, res.URL)
			for _, m := range res.Missing {
				fmt.Fprintf(w, "still needed: %s\n", m)
			}
			fmt.Fprintln(w, "next:")
			for _, n := range res.Next {
				fmt.Fprintf(w, "  %s\n", n)
			}
		})
		return nil
	}
	return cmd
}

// initByInvite hands an invite to join.
func (a *App) initByInvite(ctx context.Context, c *common, token string) error {
	var w io.Writer
	if !c.json {
		w = a.Stdout
	}
	res, err := a.joinWithInvite(ctx, c, token, joinOptions{}, w)
	if err != nil {
		return err
	}
	a.emit(c, res, func(w io.Writer) {
		fmt.Fprintf(w, "joined %s as ^%s\nnext: %s\n", res.URL, res.Handle, res.Next)
	})
	return nil
}

// terminalForms reports whether a huh form can run here.
func (a *App) terminalForms() bool {
	in, inOK := a.Stdin.(*os.File)
	out, outOK := a.Stdout.(*os.File)
	return inOK && outOK && isTerminal(in) && isTerminal(out)
}

// defaultHandle is $USER when it is a valid handle.
func defaultHandle(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if u := strings.ToLower(getenv("USER")); envelope.ValidHandle(u) {
		return u
	}
	return ""
}

// askInit runs the form. Flags given on the command line are the
// defaults the person sees.
func (a *App) askInit(ans initAnswers) (initAnswers, error) {
	if ans.Handle == "" {
		ans.Handle = defaultHandle(a.Getenv)
	}
	if ans.URL == "" {
		ans.URL = "tls://bus.example.internal:4222"
	}
	if ans.Notify == "" {
		ans.Notify = string(config.NotifyToast)
	}
	switch {
	case ans.Creds != "":
		ans.Bus = busByCreds
	case ans.Bus == "":
		ans.Bus = busByInvite
	}
	senders := strings.Join(ans.Senders, " ")
	validHandle := func(s string) error {
		if !envelope.ValidHandle(strings.TrimSpace(s)) {
			return errors.New("lower-case letters, digits, - and _, up to 64 characters")
		}
		return nil
	}
	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewInput().Title("Your handle").Description("What others type after ^ to reach you. Must equal your bus user.").Value(&ans.Handle).Validate(validHandle),
			huh.NewSelect[string]().Title("How do you reach the bus?").Options(
				huh.NewOption("I have an invite (from wormhole serve invite)", busByInvite),
				huh.NewOption("A bus URL and a password (wormhole login stores it)", busByPassword),
				huh.NewOption("A bus URL and a NATS .creds file", busByCreds),
			).Value(&ans.Bus),
		),
		huh.NewGroup(
			huh.NewInput().Title("Paste the invite").Description("The whole `wormhole join wh1...` line is fine.").Value(&ans.Invite).Validate(func(s string) error {
				if !strings.Contains(s, "wh1.") {
					return errors.New("that is not a wormhole invite; ask the person running wormhole serve for one")
				}
				return nil
			}),
		).WithHideFunc(func() bool { return ans.Bus != busByInvite }),
		huh.NewGroup(
			huh.NewInput().Title("Bus URL").Description("tls://host:4222; several addresses separated by commas").Value(&ans.URL),
			huh.NewInput().Title("Path to the .creds file").Value(&ans.Creds).Validate(func(s string) error {
				if ans.Bus != busByCreds {
					return nil
				}
				if _, err := os.Stat(config.Expand(strings.TrimSpace(s))); err != nil {
					return fmt.Errorf("cannot read %s", s)
				}
				return nil
			}),
		).WithHideFunc(func() bool { return ans.Bus != busByCreds }),
		huh.NewGroup(
			huh.NewInput().Title("Bus URL").Description("tls://host:4222; several addresses separated by commas").Value(&ans.URL),
		).WithHideFunc(func() bool { return ans.Bus != busByPassword }),
		huh.NewGroup(
			huh.NewInput().Title("Who may ask you?").Description("Handles separated by spaces; leave empty for nobody yet. A bare handle waits in your inbox until you read it;\nauto mode runs headless and is set per sender later with: wormhole senders add <handle> --auto").Value(&senders).Validate(func(s string) error {
				for _, h := range strings.Fields(s) {
					if !envelope.ValidHandle(h) {
						return fmt.Errorf("%q is not a handle", h)
					}
					if h == strings.TrimSpace(ans.Handle) {
						return errors.New("that is you")
					}
				}
				return nil
			}),
			huh.NewSelect[string]().Title("Notifications").Options(
				huh.NewOption("toast: one desktop notification per message", string(config.NotifyToast)),
				huh.NewOption("actions: one bubble for the inbox with Show, Later and Decline", string(config.NotifyActions)),
				huh.NewOption("digest: a summary every 20 minutes while something waits", string(config.NotifyDigest)),
				huh.NewOption("off", string(config.NotifyOff)),
			).Value(&ans.Notify),
		).WithHideFunc(func() bool { return ans.Bus == busByInvite }),
	}
	form := huh.NewForm(groups...).WithInput(a.Stdin.(*os.File)).WithOutput(a.Stdout.(*os.File))
	if os.Getenv("NO_COLOR") != "" {
		form = form.WithTheme(huh.ThemeFunc(huh.ThemeBase))
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return ans, fail(ExitRefused, "init cancelled; nothing written. Run it again, or without questions: wormhole init --no-input --handle <you> --url <bus>")
		}
		return ans, err
	}
	ans.Handle = strings.TrimSpace(ans.Handle)
	ans.Invite = strings.TrimSpace(ans.Invite)
	ans.URL = strings.TrimSpace(ans.URL)
	ans.Creds = strings.TrimSpace(ans.Creds)
	ans.Senders = strings.Fields(senders)
	return ans, nil
}

// writeInitConfig writes config.yaml from the example with the answers
// applied, validates it and says what is still missing.
func writeInitConfig(path string, ans initAnswers, getenv func(string) string) (initResult, error) {
	res := initResult{OK: true, Path: path}
	if ans.Handle == "" {
		ans.Handle = defaultHandle(getenv)
	}
	if ans.Handle == "" {
		ans.Handle = "alice"
		res.Missing = append(res.Missing, "your handle: wormhole init --handle <you> --force")
	}
	if !envelope.ValidHandle(ans.Handle) {
		return res, fail(ExitUsage, invalidHandleFormat+"; lower-case letters, digits, - and _", ans.Handle)
	}
	if ans.URL == "" {
		ans.URL = "tls://bus.example.internal:4222"
		res.Missing = append(res.Missing, "the bus address: wormhole init --url tls://<host>:4222 --force, or join one with: wormhole join <invite>")
	}
	if ans.Notify == "" {
		ans.Notify = string(config.NotifyToast)
	}
	dir := filepath.Dir(path)
	for _, h := range ans.Senders {
		if !envelope.ValidHandle(h) {
			return res, fail(ExitUsage, invalidHandleFormat, h)
		}
		if h == ans.Handle {
			return res, fail(ExitUsage, "^%s is you; --senders lists the other people", h)
		}
	}
	err := config.Edit(path, true, func(doc *yaml.Node) error {
		config.Set(doc, "handle", ans.Handle)
		config.Set(doc, "bus.kind", "nats")
		config.Set(doc, "bus.url", ans.URL)
		if ans.Creds != "" {
			config.Set(doc, "bus.creds", config.Expand(ans.Creds))
			config.Unset(doc, "bus.password_file")
		} else {
			pw := ans.PasswordFile
			if pw == "" {
				pw = filepath.Join(dir, passwordFileName)
			}
			config.Set(doc, "bus.password_file", config.Expand(pw))
			config.Unset(doc, "bus.creds")
		}
		config.SetList(doc, "senders", ans.Senders)
		config.Set(doc, "daemon.notify", ans.Notify)
		return nil
	})
	if err != nil {
		return res, wrap(ExitError, err)
	}
	// An existing file was overwritten with --force: Edit kept nothing
	// from it on purpose, the person asked for a fresh start.
	res.Handle, res.URL = ans.Handle, ans.URL
	if ans.Creds == "" {
		if _, err := os.Stat(config.Expand(firstNonEmpty(ans.PasswordFile, filepath.Join(dir, passwordFileName)))); err != nil {
			res.Next = append(res.Next, "wormhole login            # stores your bus password privately and checks the connection")
		}
	}
	if len(ans.Senders) == 0 {
		res.Missing = append(res.Missing, "who may ask you: wormhole senders add <handle>")
	}
	res.Next = append(res.Next, "wormhole skill install    # the skill into every agent on this machine", "wormhole service install  # the daemon as a user service", "wormhole doctor           # names anything still missing")
	return res, nil
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
