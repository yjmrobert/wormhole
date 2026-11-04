package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// How the wizard reaches the bus, and with which credential.
const (
	reachInvite = "invite"
	reachURL    = "url"
	authPass    = "password"
	authCreds   = "creds"

	defaultBusURL = "tls://bus.example.internal:4222"
	sendersNote   = "A bare handle waits in your inbox; auto mode runs headless and is set per sender with: wormhole senders add <handle> --auto"
)

// initAnswers are the wizard's questions, filled from flags, defaults and
// the form.
type initAnswers struct {
	Handle       string
	Reach        string // reachInvite or reachURL
	Invite       string
	URL          string
	Auth         string // authPass or authCreds
	PasswordFile string
	Creds        string
	Senders      []string
	Notify       string
}

// initResult is what `wormhole init` reports.
type initResult struct {
	OK      bool     `json:"ok"`
	Path    string   `json:"path"`
	Handle  string   `json:"handle"`
	URL     string   `json:"url"`
	Auth    string   `json:"auth"`
	Senders []string `json:"senders"`
	Notify  string   `json:"notify"`
	// Skipped names questions left unanswered; doctor names them too.
	Skipped []string `json:"skipped,omitempty"`
	Next    []string `json:"next"`
}

// joinWithInvite joins the bus an invite describes.
// replaced at integration by the join command's implementation
func (a *App) joinWithInvite(ctx context.Context, c *common, token string) error {
	_, _, _ = ctx, c, token
	return fail(ExitError, "wormhole join is not wired yet")
}

// interactive reports whether init may show its form.
func (a *App) interactive(c *common) bool {
	if c.noInput || c.json {
		return false
	}
	in, inOK := a.Stdin.(*os.File)
	out, outOK := a.Stdout.(*os.File)
	return inOK && outOK && isTerminal(in) && isTerminal(out)
}

// getenv reads the environment through the App.
func (a *App) getenv(k string) string {
	if a.Getenv != nil {
		return a.Getenv(k)
	}
	return os.Getenv(k)
}

// defaultHandle is $USER when it is a valid handle.
func defaultHandle(getenv func(string) string) string {
	if u := strings.ToLower(strings.TrimSpace(getenv("USER"))); envelope.ValidHandle(u) {
		return u
	}
	return ""
}

// parseSenders splits handles on spaces and commas, rejecting invalid ones,
// duplicates and the person's own handle.
func parseSenders(s, self string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	out := []string{}
	seen := map[string]bool{}
	for _, f := range fields {
		h := strings.TrimPrefix(f, "^")
		if !envelope.ValidHandle(h) {
			return nil, fmt.Errorf(invalidHandleFormat, f)
		}
		if h == self {
			return nil, fmt.Errorf("%s is your own handle", h)
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out, nil
}

// validNotify reports whether s is a daemon.notify mode.
func validNotify(s string) bool {
	switch config.NotifyMode(s) {
	case config.NotifyToast, config.NotifyActions, config.NotifyDigest, config.NotifyOff:
		return true
	}
	return false
}

// absPath expands ~ and makes p absolute.
func absPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	return filepath.Abs(config.Expand(p))
}

// renderConfig writes the answers into config.Example. yaml.v3 nodes
// locate each value, and the edit is spliced into the original text, so
// every comment and blank line of the example survives as written.
func renderConfig(ans initAnswers, dir string) ([]byte, error) {
	// Point the example's credential paths at the directory in use.
	src := strings.ReplaceAll(config.Example, "~/.config/wormhole", dir)
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config example is not a mapping")
	}
	root := doc.Content[0]
	lines := strings.Split(src, "\n")
	var edits []lineEdit

	setScalar := func(path []string, value string) error {
		n := lookup(root, path...)
		if n == nil || n.Kind != yaml.ScalarNode {
			return fmt.Errorf("config example has no scalar %s", strings.Join(path, "."))
		}
		edits = append(edits, lineEdit{line: n.Line, col: n.Column, old: n.Value, new: yamlScalar(value)})
		return nil
	}
	if err := setScalar([]string{"handle"}, ans.Handle); err != nil {
		return nil, err
	}
	if ans.URL != "" {
		if err := setScalar([]string{"bus", "url"}, ans.URL); err != nil {
			return nil, err
		}
	}
	if ans.Auth == authCreds {
		key := lookupKey(root, "bus", "password_file")
		if key == nil {
			return nil, errors.New("config example has no bus.password_file")
		}
		edits = append(edits, lineEdit{line: key.Line, col: key.Column, replaceRest: true, new: "creds: " + yamlScalar(ans.Creds)})
	} else if ans.PasswordFile != "" {
		if err := setScalar([]string{"bus", "password_file"}, ans.PasswordFile); err != nil {
			return nil, err
		}
	}
	if ans.Notify != "" {
		if err := setScalar([]string{"daemon", "notify"}, ans.Notify); err != nil {
			return nil, err
		}
	}

	// senders: replace the example's items with the answers, or turn the
	// key into an empty list.
	seq := lookup(root, "senders")
	key := lookupKey(root, "senders")
	if seq == nil || key == nil || seq.Kind != yaml.SequenceNode {
		return nil, errors.New("config example has no senders list")
	}
	var items []string
	for _, h := range ans.Senders {
		items = append(items, "- "+yamlScalar(h))
	}
	if len(items) == 0 {
		edits = append(edits, lineEdit{line: key.Line, col: key.Column, replaceRest: true, new: "senders: []"})
	}
	for i, item := range seq.Content {
		// The dash sits two columns before the item's value.
		e := lineEdit{line: item.Line, col: item.Column - 2, replaceRest: true, drop: len(items) == 0}
		if len(items) > 0 {
			if i == len(seq.Content)-1 {
				e.new = strings.Join(items, "\n"+strings.Repeat(" ", item.Column-3))
			} else {
				e.drop = true
			}
		}
		edits = append(edits, e)
	}

	sort.SliceStable(edits, func(i, j int) bool { return edits[i].line > edits[j].line })
	for _, e := range edits {
		idx := e.line - 1
		if idx < 0 || idx >= len(lines) {
			return nil, fmt.Errorf("config example: line %d out of range", e.line)
		}
		if e.drop {
			lines = append(lines[:idx], lines[idx+1:]...)
			continue
		}
		l := lines[idx]
		start := e.col - 1
		if start < 0 || start > len(l) {
			return nil, fmt.Errorf("config example: column %d out of range on line %d", e.col, e.line)
		}
		end := len(l)
		if !e.replaceRest {
			if !strings.HasPrefix(l[start:], e.old) {
				return nil, fmt.Errorf("config example: line %d does not hold %q", e.line, e.old)
			}
			end = start + len(e.old)
		}
		lines[idx] = l[:start] + e.new + l[end:]
	}
	out := strings.Join(lines, "\n")
	// The result must still be YAML a config.Load can read.
	var check yaml.Node
	if err := yaml.Unmarshal([]byte(out), &check); err != nil {
		return nil, fmt.Errorf("rendered config is not valid YAML: %v", err)
	}
	return []byte(out), nil
}

// lineEdit replaces text on one line of the example.
type lineEdit struct {
	line, col   int
	old, new    string
	replaceRest bool // replace from col to the end of the line
	drop        bool // remove the line
}

// lookupKey returns the key node at path in a mapping tree.
func lookupKey(n *yaml.Node, path ...string) *yaml.Node {
	for i, p := range path {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next, key *yaml.Node
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value == p {
				key, next = n.Content[j], n.Content[j+1]
				break
			}
		}
		if i == len(path)-1 {
			return key
		}
		n = next
	}
	return nil
}

// lookup returns the value node at path in a mapping tree.
func lookup(n *yaml.Node, path ...string) *yaml.Node {
	for _, p := range path {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value == p {
				next = n.Content[j+1]
				break
			}
		}
		n = next
	}
	return n
}

// yamlScalar renders s as a one-line YAML scalar, quoted when needed.
func yamlScalar(s string) string {
	b, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// initForm builds the wizard. Every field starts at a default that works.
func initForm(ans *initAnswers, senders *string, in io.Reader, out io.Writer) *huh.Form {
	handle := huh.NewInput().Title("Your handle").
		Description("What colleagues type after ^ to reach you. It must equal your bus user.").
		Value(&ans.Handle).
		Validate(func(s string) error {
			if !envelope.ValidHandle(s) {
				return errors.New("lowercase letters, digits, - and _, starting with a letter or digit")
			}
			return nil
		})
	reach := huh.NewSelect[string]().Title("How do you reach the bus?").
		Options(
			huh.NewOption("Paste an invite (from wormhole serve)", reachInvite),
			huh.NewOption("Bus URL and credential", reachURL),
		).Value(&ans.Reach)
	invite := huh.NewInput().Title("Invite").Description("The token wormhole serve printed.").Value(&ans.Invite).
		Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("paste the invite, or go back and choose a bus URL")
			}
			return nil
		})
	url := huh.NewInput().Title("Bus URL").Description("One address, or several separated by commas.").Value(&ans.URL).
		Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("the bus URL is required, such as " + defaultBusURL)
			}
			return nil
		})
	auth := huh.NewSelect[string]().Title("Credential").
		Options(
			huh.NewOption("Password (you type it next, with wormhole login)", authPass),
			huh.NewOption("A NATS .creds file", authCreds),
		).Value(&ans.Auth)
	creds := huh.NewInput().Title(".creds file").Value(&ans.Creds).
		Validate(func(s string) error {
			if _, err := os.Stat(config.Expand(strings.TrimSpace(s))); err != nil {
				return errors.New("no such file")
			}
			return nil
		})
	who := huh.NewInput().Title("Who may ask you?").
		Description("Handles separated by spaces; empty for nobody yet. " + sendersNote).
		Value(senders).
		Validate(func(s string) error {
			_, err := parseSenders(s, ans.Handle)
			return err
		})
	notify := huh.NewSelect[string]().Title("Notifications").
		Options(
			huh.NewOption("toast: one desktop notification per message", string(config.NotifyToast)),
			huh.NewOption("actions: one bubble for the inbox, with Show, Later and Decline", string(config.NotifyActions)),
			huh.NewOption("digest: one summary every 20 minutes", string(config.NotifyDigest)),
			huh.NewOption("off", string(config.NotifyOff)),
		).Value(&ans.Notify)

	viaInvite := func() bool { return ans.Reach == reachInvite }
	form := huh.NewForm(
		huh.NewGroup(handle),
		huh.NewGroup(reach),
		huh.NewGroup(invite).WithHideFunc(func() bool { return !viaInvite() }),
		huh.NewGroup(url, auth).WithHideFunc(viaInvite),
		huh.NewGroup(creds).WithHideFunc(func() bool { return viaInvite() || ans.Auth != authCreds }),
		huh.NewGroup(who).WithHideFunc(viaInvite),
		huh.NewGroup(notify).WithHideFunc(viaInvite),
	).WithInput(in).WithOutput(out)
	if os.Getenv("NO_COLOR") != "" {
		form = form.WithTheme(huh.ThemeFunc(huh.ThemeBase))
	}
	return form
}

// newInitCommand is the config wizard, with a flag for every question.
func (a *App) newInitCommand(c *common) *cobra.Command {
	var force bool
	var flags initAnswers
	var senders []string
	cmd := command("init", "Write your configuration: handle, bus, senders, notifications")
	cmd.Long = "Asks for your handle, how to reach the bus (an invite, or a URL and a credential), who may ask you, and how you are notified. Every question has a default that works. Off a terminal, or with --no-input, each question takes a flag and an unanswered one is skipped; wormhole doctor then names it."
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")
	cmd.Flags().StringVar(&flags.Handle, "handle", "", "your handle (default $USER)")
	cmd.Flags().StringVar(&flags.Invite, "invite", "", "join the bus this invite describes (as wormhole join does)")
	cmd.Flags().StringVar(&flags.URL, "url", "", "bus URL (default "+defaultBusURL+")")
	cmd.Flags().StringVar(&flags.PasswordFile, "password-file", "", "file holding the bus password (default <config dir>/password, written by wormhole login)")
	cmd.Flags().StringVar(&flags.Creds, "creds", "", "NATS .creds file instead of a password")
	cmd.Flags().StringSliceVar(&senders, "senders", nil, "handles who may ask you, in inbox mode (a,b)")
	cmd.Flags().StringVar(&flags.Notify, "notify", "", "toast, actions, digest or off (default toast)")
	cmd.Args = noArgs("wormhole init [--no-input] [--handle <h>] [--invite <token> | --url <url> [--password-file <f> | --creds <f>]] [--senders a,b] [--notify toast|actions|digest|off] [--force]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		path := c.config
		if path == "" {
			path = config.DefaultPath()
		}
		if flags.PasswordFile != "" && flags.Creds != "" {
			return fail(ExitUsage, "--password-file and --creds are exclusive; pass one")
		}
		if flags.Invite != "" && (flags.URL != "" || flags.PasswordFile != "" || flags.Creds != "") {
			return fail(ExitUsage, "--invite carries the bus and the credential; drop --url, --password-file and --creds")
		}
		if flags.Notify != "" && !validNotify(flags.Notify) {
			return fail(ExitUsage, "--notify %q: want toast, actions, digest or off", flags.Notify)
		}
		if flags.Handle != "" && !envelope.ValidHandle(flags.Handle) {
			return fail(ExitUsage, invalidHandleFormat+"; a handle is lowercase letters, digits, - and _", flags.Handle)
		}
		if d, ok := config.InGitTree(path); ok {
			return fail(ExitRefused, "%s is inside the git working tree %s; config must live outside any repository: wormhole init --config ~/.config/wormhole/config.yaml", path, d)
		}
		exists := false
		if _, err := os.Stat(path); err == nil {
			exists = true
		}

		ans := flags
		if ans.Handle == "" {
			ans.Handle = defaultHandle(a.getenv)
		}
		parsed, err := parseSenders(strings.Join(senders, " "), ans.Handle)
		if err != nil {
			return fail(ExitUsage, "--senders: %v", err)
		}
		ans.Senders = parsed
		var skipped []string
		ans.Reach = reachURL
		if ans.Invite != "" {
			ans.Reach = reachInvite
		}
		ans.Auth = authPass
		if ans.Creds != "" {
			ans.Auth = authCreds
		}
		if ans.URL == "" {
			ans.URL = defaultBusURL
		}
		if ans.Notify == "" {
			ans.Notify = string(config.NotifyToast)
		}

		if a.interactive(c) {
			if exists && !force {
				ok, err := a.confirm(fmt.Sprintf("%s exists. Replace it?", path))
				if err != nil {
					return err
				}
				if !ok {
					return fail(ExitRefused, "%s kept; to change one setting, edit it with wormhole senders, or rerun: wormhole init --force", path)
				}
				exists = false
			}
			text := strings.Join(ans.Senders, " ")
			if err := initForm(&ans, &text, a.Stdin, a.Stdout).RunWithContext(ctx); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					return fail(ExitRefused, "nothing written; run it again: wormhole init")
				}
				return err
			}
			parsed, err := parseSenders(text, ans.Handle)
			if err != nil {
				return fail(ExitUsage, "senders: %v", err)
			}
			ans.Senders = parsed
			ans.Invite, ans.URL, ans.Creds = strings.TrimSpace(ans.Invite), strings.TrimSpace(ans.URL), strings.TrimSpace(ans.Creds)
		}

		if ans.Reach == reachInvite {
			return a.joinWithInvite(ctx, c, ans.Invite)
		}
		if exists && !force {
			return fail(ExitRefused, "%s exists; pass --force to overwrite: wormhole init --force", path)
		}
		if ans.Handle == "" {
			skipped = append(skipped, "handle")
		}
		dir := filepath.Dir(path)
		if ans.PasswordFile, err = absPath(ans.PasswordFile); err != nil {
			return err
		}
		if ans.Creds, err = absPath(ans.Creds); err != nil {
			return err
		}
		for _, p := range []string{ans.PasswordFile, ans.Creds} {
			if p == "" {
				continue
			}
			if d, ok := config.InGitTree(p); ok {
				return fail(ExitRefused, "%s is inside the git working tree %s; the daemon refuses credentials there; move it out and pass its new path", p, d)
			}
		}
		raw, err := renderConfig(ans, dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return err
		}
		// WriteFile keeps an existing file's mode; the config is private.
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}

		res := initResult{OK: true, Path: path, Handle: ans.Handle, URL: ans.URL, Auth: ans.Auth, Senders: ans.Senders, Notify: ans.Notify, Skipped: skipped}
		if res.Senders == nil {
			res.Senders = []string{}
		}
		if ans.Auth == authPass {
			res.Next = append(res.Next, "wormhole login")
		}
		res.Next = append(res.Next, "wormhole skill install", "wormhole service install")
		peer := "<handle>"
		if len(ans.Senders) > 0 {
			peer = ans.Senders[0]
		}
		res.Next = append(res.Next, fmt.Sprintf(`wormhole ask %s "..."`, peer))
		a.emit(c, res, func(w io.Writer) {
			if ans.Handle != "" {
				fmt.Fprintf(w, "wrote %s (mode 600) for ^%s on %s\n", path, ans.Handle, ans.URL)
			} else {
				fmt.Fprintf(w, "wrote %s (mode 600) on %s\n", path, ans.URL)
			}
			if len(ans.Senders) == 0 {
				fmt.Fprintln(w, "nobody may ask you yet; add a colleague with: wormhole senders add <handle>")
			} else {
				fmt.Fprintf(w, "who may ask you: %s (inbox mode). %s\n", strings.Join(ans.Senders, ", "), sendersNote)
			}
			for _, s := range skipped {
				fmt.Fprintf(w, "skipped %s: no flag and no default; wormhole doctor will name it, or set it now: wormhole init --force --%s <value>\n", s, s)
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
