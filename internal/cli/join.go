package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/serve"
)

// joinRetry is how long join keeps trying to reach the bus: `serve
// invite` may have been run a moment ago and the server polls for it.
var joinRetry = 10 * time.Second

// joinResult is the JSON result of `wormhole join`.
type joinResult struct {
	OK           bool     `json:"ok"`
	Handle       string   `json:"handle"`
	URL          string   `json:"url"`
	Config       string   `json:"config"`
	PasswordFile string   `json:"password_file"`
	TLSCA        string   `json:"tls_ca"`
	SendersAdded []string `json:"senders_added"`
	Next         string   `json:"next"`
}

// newJoinCommand joins this machine to a `wormhole serve` bus.
func (a *App) newJoinCommand(c *common) *cobra.Command {
	cmd := command("join <invite>", "Join a bus with an invite from wormhole serve")
	cmd.Long = "Exchange a one-time invite for a bus credential, write the config, the credential and the bus CA, and verify the connection.\nRun again with a new invite to rotate the credential; every other config key is left alone."
	cmd.Args = exactArgs(1, "wormhole join <invite>")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		inv, err := serve.Decode(args[0])
		if err != nil {
			return fail(ExitUsage, "%v", err)
		}
		inviter := whoInvited(inv.By)
		if inv.Expired(a.Now()) {
			return fail(ExitRefused, "this invite expired at %s; ask %s for a new invite", inv.Expires().Local().Format(time.RFC3339), inviter)
		}
		path := c.config
		if path == "" {
			path = config.DefaultPath()
		}
		dir := filepath.Dir(path)
		if d, ok := config.InGitTree(path); ok {
			return fail(ExitRefused, "%s is inside the git working tree %s and would hold your bus credential; set WORMHOLE_CONFIG_DIR or run from your home directory", path, d)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		caPath := filepath.Join(dir, "bus-ca.pem")
		pwPath := filepath.Join(dir, "password")
		if err := writePrivateAtomic(caPath, serve.CAPEM(inv.CA)); err != nil {
			return err
		}
		pw, err := a.exchangeInvite(ctx, inv, caPath)
		if err != nil {
			return err
		}
		if err := writePrivateAtomic(pwPath, []byte(pw+"\n")); err != nil {
			return err
		}
		busURL := strings.Join(inv.URLs, ",")
		added, err := writeJoinConfig(path, inv.Handle, busURL, pwPath, caPath, inv.By)
		if err != nil {
			return fail(ExitError, "write %s: %v", path, err)
		}
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		res := joinResult{OK: true, Handle: cfg.Handle, URL: busURL, Config: path, PasswordFile: pwPath, TLSCA: caPath, SendersAdded: added}
		b, err := a.NewBus(ctx, cfg)
		if err != nil {
			return fail(ExitUnavailable, "joined, but the bus refused the new credential: %v; run: wormhole doctor", err)
		}
		peer := ""
		listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if list, err := b.Registry().List(listCtx); err == nil {
			peer = pickPeer(list, cfg.Handle, inv.By, a.Now(), 2*cfg.Daemon.RegistryInterval)
		}
		cancel()
		_ = b.Close()
		if err := a.afterJoin(ctx, c, cfg, &res); err != nil {
			return err
		}
		next := "when someone is online: wormhole users"
		switch {
		case peer != "":
			res.Next = fmt.Sprintf("wormhole ask ^%s \"...\"", peer)
			next = "ask them something: " + res.Next
		case inv.By != "" && inv.By != cfg.Handle:
			res.Next = fmt.Sprintf("wormhole ask ^%s \"...\"", inv.By)
			next = fmt.Sprintf("when ^%s is online: %s", inv.By, res.Next)
		default:
			res.Next = "wormhole users"
		}
		a.emit(c, res, func(w io.Writer) {
			fmt.Fprintf(w, "joined %s as ^%s\n", inv.URLs[0], cfg.Handle)
			fmt.Fprintln(w, next)
		})
		return nil
	}
	return cmd
}

// afterJoin installs the skill and the user service; wired at integration.
func (a *App) afterJoin(ctx context.Context, c *common, cfg *config.Config, res *joinResult) error {
	return nil
}

// whoInvited names the person to ask for a new invite.
func whoInvited(by string) string {
	if by == "" {
		return "the person running wormhole serve"
	}
	return "^" + by
}

// pickPeer chooses whom to suggest asking first: the inviter when online,
// else any other online handle.
func pickPeer(list []bus.Presence, self, by string, now time.Time, window time.Duration) string {
	var online []string
	for _, p := range list {
		if p.Handle != self && now.Sub(p.LastSeen) < window {
			if p.Handle == by {
				return by
			}
			online = append(online, p.Handle)
		}
	}
	sort.Strings(online)
	if len(online) > 0 {
		return online[0]
	}
	return ""
}

// exchangeInvite connects as the invite's join user, trusting only the CA
// at caPath, and trades the secret for the user's password.
func (a *App) exchangeInvite(ctx context.Context, inv serve.Invite, caPath string) (string, error) {
	inviter := whoInvited(inv.By)
	servers := strings.Join(inv.URLs, ",")
	deadline := time.Now().Add(joinRetry)
	var nc *natsgo.Conn
	var lastErr error
	for {
		var err error
		nc, err = natsgo.Connect(servers, natsgo.Name("wormhole/join"), natsgo.UserInfo(inv.User(), inv.Password()),
			natsgo.RootCAs(caPath), natsgo.Timeout(3*time.Second), natsgo.MaxReconnects(0))
		if err == nil {
			break
		}
		lastErr = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
	if nc == nil {
		if isAuthError(lastErr) {
			return "", fail(ExitRefused, "the bus does not accept this invite: it was already used, revoked or replaced; ask %s for a new invite", inviter)
		}
		return "", fail(ExitUnavailable, "cannot reach the bus at %s: %v; is wormhole serve running on %s? ask %s to check `wormhole serve users`", servers, lastErr, urlHost(inv.URLs[0]), inviter)
	}
	defer nc.Close()
	body, _ := json.Marshal(serve.JoinRequest{Handle: inv.Handle, Host: a.Hostname})
	msg, err := nc.Request(inv.Subject(), body, 15*time.Second)
	if err != nil {
		return "", fail(ExitUnavailable, "the bus did not answer the join request: %v; ask %s to check that wormhole serve is running", err, inviter)
	}
	var resp serve.JoinResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return "", fail(ExitError, "malformed join response: %v; update wormhole on both machines", err)
	}
	if !resp.OK || resp.Password == "" {
		return "", fail(ExitRefused, "%s", resp.Error)
	}
	return resp.Password, nil
}

// isAuthError reports a NATS authentication refusal.
func isAuthError(err error) bool {
	return err != nil && (errors.Is(err, natsgo.ErrAuthorization) || strings.Contains(strings.ToLower(err.Error()), "authorization violation"))
}

// urlHost is the host part of a bus URL.
func urlHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return raw
}

// writePrivateAtomic writes path with mode 600 via a temporary file.
func writePrivateAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// writeJoinConfig writes or edits the config at path for a join and
// returns the senders it added. An existing file keeps its comments and
// every key but handle and the bus connection; a new one starts from the
// documented example.
func writeJoinConfig(path, handle, busURL, pwPath, caPath, by string) ([]string, error) {
	raw, err := os.ReadFile(path)
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		return nil, err
	}
	if fresh {
		example := strings.ReplaceAll(config.Example, "~/.config/wormhole", filepath.Dir(path))
		// Uncomment the example's tls_ca so the value lands where it is documented.
		example = strings.Replace(example, "\n  # tls_ca: ", "\n  tls_ca: ", 1)
		raw = []byte(example)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w (fix the YAML or move the file aside and join again)", err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the config is not a YAML mapping; move it aside and join again")
	}
	root := doc.Content[0]
	setScalar(root, "handle", handle)
	busNode := mapping(root, "bus")
	busNode.Style = 0
	setScalar(busNode, "kind", "nats")
	setScalar(busNode, "url", busURL)
	setScalar(busNode, "password_file", pwPath)
	setScalar(busNode, "tls_ca", caPath)
	// A join credential is a password: a creds file or a user from an
	// earlier setup would override it or fail validation.
	deleteKey(busNode, "creds")
	deleteKey(busNode, "user")
	senders := sequence(root, "senders")
	if fresh {
		senders.Content = nil
	}
	var added []string
	if by != "" && by != handle && !hasSender(senders, by) {
		senders.Content = append(senders.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: by})
		added = append(added, by)
	}
	if len(senders.Content) == 0 {
		senders.Style = yaml.FlowStyle
	} else if fresh {
		senders.Style = 0
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if added == nil {
		added = []string{}
	}
	return added, writePrivateAtomic(path, out.Bytes())
}

// lookup returns the value node for key in mapping m.
func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setScalar sets key in mapping m to a string value.
func setScalar(m *yaml.Node, key, value string) {
	if v := lookup(m, key); v != nil {
		v.Kind, v.Tag, v.Value, v.Style, v.Content = yaml.ScalarNode, "!!str", value, 0, nil
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

// mapping returns the mapping under key, creating or replacing a
// non-mapping value.
func mapping(m *yaml.Node, key string) *yaml.Node {
	if v := lookup(m, key); v != nil {
		if v.Kind != yaml.MappingNode {
			v.Kind, v.Tag, v.Value, v.Content = yaml.MappingNode, "!!map", "", nil
		}
		return v
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// sequence returns the sequence under key, creating it (or replacing an
// empty value) when needed.
func sequence(m *yaml.Node, key string) *yaml.Node {
	if v := lookup(m, key); v != nil {
		if v.Kind != yaml.SequenceNode {
			v.Kind, v.Tag, v.Value, v.Content = yaml.SequenceNode, "!!seq", "", nil
		}
		return v
	}
	v := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// deleteKey removes key from mapping m.
func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// hasSender reports whether the senders sequence lists handle, in either
// the bare or the mapping form.
func hasSender(seq *yaml.Node, handle string) bool {
	for _, n := range seq.Content {
		switch n.Kind {
		case yaml.ScalarNode:
			if n.Value == handle {
				return true
			}
		case yaml.MappingNode:
			if v := lookup(n, "handle"); v != nil && v.Value == handle {
				return true
			}
		}
	}
	return false
}
