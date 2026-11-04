package serve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"golang.org/x/crypto/bcrypt"

	"github.com/yjmrobert/wormhole/internal/bus"
	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// startTest runs a server on a free loopback port with cheap hashes.
func startTest(t *testing.T, clk *clock) *Server {
	t.Helper()
	o := Options{StateDir: filepath.Join(t.TempDir(), "serve"), Listen: "127.0.0.1:0", BcryptCost: bcrypt.MinCost,
		PollInterval: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Hostname: "alice-laptop"}
	if clk != nil {
		o.Now = clk.Now
	}
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// mint creates an invite for handle the way `serve invite` does.
func mint(t *testing.T, s *Server, handle string, now time.Time, opts ...InviteOptions) Invite {
	t.Helper()
	if len(opts) == 0 {
		opts = []InviteOptions{{BcryptCost: bcrypt.MinCost}}
	}
	tok, err := NewInvite(s.StateDir(), handle, "alice", time.Hour, now, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := Decode(tok)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// tlsFor trusts only the CA the invite carries.
func tlsFor(t *testing.T, inv Invite) *tls.Config {
	t.Helper()
	cert, err := x509.ParseCertificate(inv.CA)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// dialJoin connects as the invite's join user.
func dialJoin(t *testing.T, inv Invite) (*natsgo.Conn, error) {
	t.Helper()
	return natsgo.Connect(strings.Join(inv.URLs, ","), natsgo.UserInfo(inv.User(), inv.Password()), natsgo.Secure(tlsFor(t, inv)),
		natsgo.MaxReconnects(0), natsgo.Timeout(5*time.Second))
}

// exchange sends one join request and decodes the answer.
func exchange(t *testing.T, nc *natsgo.Conn, inv Invite, handle string) JoinResponse {
	t.Helper()
	body, _ := json.Marshal(JoinRequest{Handle: handle, Host: handle + "-laptop"})
	msg, err := nc.Request(inv.Subject(), body, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var resp JoinResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// eventually polls cond for a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

// connectUser connects as a joined user with the CA file, as the daemon does.
func connectUser(t *testing.T, s *Server, inv Invite, handle, pw string) (*natsbus.Bus, error) {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "bus-ca.pem")
	if err := os.WriteFile(ca, CAPEM(inv.CA), 0o600); err != nil {
		t.Fatal(err)
	}
	return natsbus.Connect(context.Background(), natsbus.Options{URL: strings.Join(s.ClientURLs(), ","), User: handle, Password: pw, TLSCA: ca,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// TestStartState checks the files serve keeps and their modes.
func TestStartState(t *testing.T) {
	s := startTest(t, nil)
	urls := s.ClientURLs()
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "tls://127.0.0.1:") || strings.HasSuffix(urls[0], ":0") {
		t.Fatalf("urls %v", urls)
	}
	if info, err := os.Stat(s.StateDir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v %v", info, err)
	}
	for _, f := range []string{caCertFile, caKeyFile, serverCertFile, serverKeyFile, usersFileName, adminSecretFile, advertiseFile} {
		info, err := os.Stat(filepath.Join(s.StateDir(), f))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, info, err)
		}
	}
	adv, err := ReadAdvertised(s.StateDir())
	if err != nil || strings.Join(adv, ",") != strings.Join(urls, ",") {
		t.Fatalf("advertise %v %v", adv, err)
	}
	cert, err := readCert(filepath.Join(s.StateDir(), serverCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cert.DNSNames, ","), "alice-laptop") || !strings.Contains(strings.Join(ipStrings(cert.IPAddresses), ","), "127.0.0.1") {
		t.Fatalf("SANs %v %v", cert.DNSNames, cert.IPAddresses)
	}
	f, _ := loadUsers(s.StateDir())
	if f.Admin.Hash == "" || !strings.HasPrefix(f.Admin.Hash, "$2") {
		t.Fatalf("admin hash %q", f.Admin.Hash)
	}
	// The plaintext admin password never reaches users.json.
	raw, _ := os.ReadFile(filepath.Join(s.StateDir(), usersFileName))
	pw, _ := os.ReadFile(filepath.Join(s.StateDir(), adminSecretFile))
	if strings.Contains(string(raw), strings.TrimSpace(string(pw))) {
		t.Fatal("admin password in users.json")
	}
}

// TestRestartKeepsCA checks the CA survives a restart and a SAN change
// reissues only the server certificate.
func TestRestartKeepsCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "serve")
	o := Options{StateDir: dir, Listen: "127.0.0.1:0", BcryptCost: bcrypt.MinCost, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Hostname: "alice-laptop"}
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	ca1, _ := os.ReadFile(filepath.Join(dir, caCertFile))
	srv1, _ := os.ReadFile(filepath.Join(dir, serverCertFile))
	s.Close()
	s, err = Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	srv2, _ := os.ReadFile(filepath.Join(dir, serverCertFile))
	s.Close()
	o.Advertise = []string{"bus.example.internal:4333"}
	s, err = Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ca3, _ := os.ReadFile(filepath.Join(dir, caCertFile))
	srv3, _ := os.ReadFile(filepath.Join(dir, serverCertFile))
	if string(ca1) != string(ca3) || string(srv1) != string(srv2) || string(srv2) == string(srv3) {
		t.Fatal("CA must persist; server cert must be reused until the names change")
	}
	if u := s.ClientURLs(); len(u) != 2 || u[1] != "tls://bus.example.internal:4333" {
		t.Fatalf("urls %v", u)
	}
}

// TestJoinExchange is the whole invite path over real TLS: mint, connect as
// the join user trusting only the token's CA, exchange, then use the bus as
// the new user with the template permissions.
func TestJoinExchange(t *testing.T) {
	s := startTest(t, nil)
	inv := mint(t, s, "bob", time.Now())
	// Minted while serving: the poller adds the join user.
	var nc *natsgo.Conn
	eventually(t, "join user", func() bool {
		var err error
		nc, err = dialJoin(t, inv)
		return err == nil
	})
	defer nc.Close()
	// A join for another handle is refused and does not spend the invite.
	if r := exchange(t, nc, inv, "carol"); r.OK || !strings.Contains(r.Error, "for ^bob, not ^carol") {
		t.Fatalf("wrong handle: %+v", r)
	}
	resp := exchange(t, nc, inv, "bob")
	if !resp.OK || resp.Password == "" {
		t.Fatalf("join: %+v", resp)
	}
	// Single use.
	if r := exchange(t, nc, inv, "bob"); r.OK || !strings.Contains(r.Error, "already used") {
		t.Fatalf("second join: %+v", r)
	}
	users, _ := ListUsers(s.StateDir())
	invites, _ := ListInvites(s.StateDir())
	if len(users) != 1 || users[0].Handle != "bob" || users[0].Created.IsZero() || len(invites) != 0 {
		t.Fatalf("users %v invites %v", users, invites)
	}
	raw, _ := os.ReadFile(filepath.Join(s.StateDir(), usersFileName))
	if strings.Contains(string(raw), resp.Password) || strings.Contains(string(raw), inv.Password()) {
		t.Fatal("plaintext secret in users.json")
	}

	bob, err := connectUser(t, s, inv, "bob", resp.Password)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	ctx := context.Background()
	if err := bob.Registry().Put(ctx, bus.Presence{Handle: "bob", Host: "bob-laptop", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ask := envelope.NewAsk(envelope.Peer{User: "bob", Host: "bob-laptop"}, "alice", "hi", time.Now())
	if err := bob.Publish(ctx, bus.SubjectFor(ask), ask); err != nil {
		t.Fatal(err)
	}
	// The template forbids bob from impersonating alice; the server says so.
	raw2, _ := natsgo.Connect(strings.Join(s.ClientURLs(), ","), natsgo.UserInfo("bob", resp.Password), natsgo.Secure(tlsFor(t, inv)))
	defer raw2.Close()
	errs := make(chan error, 1)
	raw2.SetErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err })
	_ = raw2.Publish("wh.inbox.alice.carol", []byte("x"))
	_ = raw2.Flush()
	select {
	case err := <-errs:
		if !strings.Contains(strings.ToLower(err.Error()), "permissions violation") {
			t.Fatalf("unexpected error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("impersonating publish was not refused")
	}

	// After the grace period the spent join user is gone.
	nc.Close()
	s.graceMu.Lock()
	for id, g := range s.grace {
		g.until = time.Now().Add(-time.Second)
		s.grace[id] = g
	}
	s.graceMu.Unlock()
	eventually(t, "join user removed", func() bool {
		c, err := dialJoin(t, inv)
		if err == nil {
			c.Close()
			return false
		}
		return true
	})
}

// TestTamperedSecretRefused checks a token whose secret was changed cannot
// authenticate.
func TestTamperedSecretRefused(t *testing.T) {
	s := startTest(t, nil)
	inv := mint(t, s, "bob", time.Now())
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	inv.Secret[0] ^= 0xff
	if c, err := dialJoin(t, inv); err == nil {
		c.Close()
		t.Fatal("tampered secret accepted")
	}
}

// TestInviteExpires checks an expired invite cannot join and is pruned.
func TestInviteExpires(t *testing.T) {
	clk := &clock{now: time.Now()}
	s := startTest(t, clk)
	inv := mint(t, s, "bob", clk.Now())
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	nc, err := dialJoin(t, inv)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	clk.Add(2 * time.Hour)
	// Connected before expiry: the responder still refuses.
	if r, err := s.join(inv.ID, JoinRequest{Handle: "bob"}); err == nil || r != "" {
		t.Fatalf("expired join accepted: %v", err)
	}
	eventually(t, "pruned", func() bool {
		f, _ := loadUsers(s.StateDir())
		return len(f.Invites) == 0
	})
	if c, err := dialJoin(t, inv); err == nil {
		c.Close()
		t.Fatal("expired join user still accepted")
	}
}

// TestRevoke checks a revoked user is dropped by the running server.
func TestRevoke(t *testing.T) {
	s := startTest(t, nil)
	inv := mint(t, s, "bob", time.Now())
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	pw, err := s.join(inv.ID, JoinRequest{Handle: "bob", Host: "bob-laptop"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := connectUser(t, s, inv, "bob", pw)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	if err := Revoke(s.StateDir(), "bob"); err != nil {
		t.Fatal(err)
	}
	if err := Revoke(s.StateDir(), "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	eventually(t, "bob refused", func() bool {
		b, err := connectUser(t, s, inv, "bob", pw)
		if err == nil {
			b.Close()
			return false
		}
		return true
	})
}

// TestInviteRules checks refusals and rotation.
func TestInviteRules(t *testing.T) {
	s := startTest(t, nil)
	now := time.Now()
	for _, h := range []string{"Bob", "admin", ""} {
		if _, err := NewInvite(s.StateDir(), h, "", time.Hour, now, nil); err == nil {
			t.Fatalf("%q accepted", h)
		}
	}
	if _, err := NewInvite(filepath.Join(t.TempDir(), "nothing"), "bob", "", time.Hour, now, nil); !errors.Is(err, ErrNotServing) || !strings.Contains(err.Error(), "run: wormhole serve") {
		t.Fatalf("no state: %v", err)
	}
	inv := mint(t, s, "bob", now)
	if _, err := s.join(inv.ID, JoinRequest{Handle: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInvite(s.StateDir(), "bob", "alice", time.Hour, now, nil); !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "wormhole serve revoke bob") {
		t.Fatalf("existing user: %v", err)
	}
	before, _ := loadUsers(s.StateDir())
	rot := mint(t, s, "bob", now, InviteOptions{Rotate: true, BcryptCost: bcrypt.MinCost})
	if _, err := s.join(rot.ID, JoinRequest{Handle: "bob"}); err != nil {
		t.Fatal(err)
	}
	after, _ := loadUsers(s.StateDir())
	if before.Users["bob"].Hash == after.Users["bob"].Hash {
		t.Fatal("rotation kept the old credential")
	}
	invites, _ := ListInvites(s.StateDir())
	if len(invites) != 0 {
		t.Fatalf("invites %v", invites)
	}
	urls := []string{"tls://bus.example.internal:4222"}
	tok, err := NewInvite(s.StateDir(), "carol", "", 0, now, urls, InviteOptions{BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Decode(tok)
	if c.URLs[0] != urls[0] || c.Expires().Sub(now) < DefaultInviteTTL-time.Second {
		t.Fatalf("carol %+v", c)
	}
	invites, _ = ListInvites(s.StateDir())
	if len(invites) != 1 || invites[0].Handle != "carol" || invites[0].By != "" {
		t.Fatalf("invites %v", invites)
	}
	if err := Revoke(s.StateDir(), "carol"); err != nil {
		t.Fatal(err)
	}
	if invites, _ = ListInvites(s.StateDir()); len(invites) != 0 {
		t.Fatalf("revoke left the invite: %v", invites)
	}
}

// TestClientURLs checks address selection.
func TestClientURLs(t *testing.T) {
	ips := interfaceIPsFixture()
	got := clientURLs("alice-laptop", "", 4222, ips, []string{"bus.example.internal", "[fd00::1]:4333"})
	want := "tls://alice-laptop:4222,tls://10.0.0.5:4222,tls://[fd00::5]:4222,tls://bus.example.internal:4222,tls://[fd00::1]:4333"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
	if got := clientURLs("alice-laptop", "10.0.0.5", 4222, ips, nil); strings.Join(got, ",") != "tls://10.0.0.5:4222" {
		t.Fatalf("specific listen: %v", got)
	}
	dns, ip := certNames("alice-laptop", "", ips, []string{"bus.example.internal:4333"})
	if strings.Join(dns, ",") != "alice-laptop,bus.example.internal,localhost" || strings.Join(ipStrings(ip), ",") != "10.0.0.5,127.0.0.1,::1,fd00::5" {
		t.Fatalf("sans %v %v", dns, ip)
	}
}

// interfaceIPsFixture is a fixed set of interface addresses.
func interfaceIPsFixture() []net.IP {
	return []net.IP{net.ParseIP("10.0.0.5").To4(), net.ParseIP("fd00::5")}
}
