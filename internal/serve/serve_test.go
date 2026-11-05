package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"golang.org/x/crypto/bcrypt"

	"github.com/yjmrobert/wormhole/internal/bus"
	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// start runs a bus on a free loopback port with a fast poller.
func start(t *testing.T, dir string) *Server {
	t.Helper()
	s, err := Start(context.Background(), Options{StateDir: dir, Listen: "127.0.0.1:0", Hostname: "alice-laptop", PollInterval: 30 * time.Millisecond, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// caFile writes the CA from an invite to a PEM file, as join does.
func caFile(t *testing.T, inv Invite) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bus-ca.pem")
	if err := os.WriteFile(p, CAPEM(inv.CADER()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInviteRoundTripAndTamper(t *testing.T) {
	inv := Invite{Version: 1, ID: envelope.NewID(), Handle: "bob", By: "alice", URLs: []string{"tls://alice-laptop:4222"}, CA: "AQID", Secret: "c2VjcmV0", Expires: time.Now().Add(time.Hour).Unix()}
	tok := Encode(inv)
	if !strings.HasPrefix(tok, "wh1.") || strings.ContainsAny(tok, "=+/ ") {
		t.Fatalf("token = %q", tok)
	}
	got, err := Decode("  wormhole join " + tok + "\n")
	if err != nil || fmt.Sprint(got) != fmt.Sprint(inv) {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	if got.Expired(time.Now()) || !got.Expired(time.Now().Add(2*time.Hour)) {
		t.Fatal("expiry")
	}
	for _, bad := range []string{"", "hello", "wh2." + tok[4:], tok[:len(tok)-10], "wh1.!!!"} {
		if _, err := Decode(bad); !errors.Is(err, ErrNotInvite) {
			t.Errorf("Decode(%q) = %v", bad, err)
		}
	}
	// A tampered payload either fails to decode or carries different data.
	raw := []byte(tok)
	raw[10] ^= 0x01
	if got2, err := Decode(string(raw)); err == nil && fmt.Sprint(got2) == fmt.Sprint(inv) {
		t.Fatal("tampered token decoded to the same invite")
	}
}

func TestInviteNeedsAServer(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Mint(dir, "bob", InviteOptions{}); err == nil || !strings.Contains(err.Error(), "wormhole serve") {
		t.Fatalf("invite without a bus: %v", err)
	}
}

func TestJoinExchange(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	if len(s.ClientURLs()) == 0 || !strings.HasPrefix(s.ClientURLs()[0], "tls://alice-laptop:") || !strings.Contains(strings.Join(s.ClientURLs(), ","), "127.0.0.1") {
		t.Fatalf("urls = %v", s.ClientURLs())
	}
	// Minted from "another process": only the file is touched.
	tok, _, err := Mint(dir, "bob", InviteOptions{By: "alice", BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := Decode(tok)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Handle != "bob" || inv.By != "alice" || len(inv.URLs) != len(s.ClientURLs()) {
		t.Fatalf("invite = %+v", inv)
	}
	// Loopback is listed last; the test machine can only reach that one.
	inv.URLs = []string{inv.URLs[len(inv.URLs)-1]}
	ca := caFile(t, inv)
	red, err := Redeem(context.Background(), inv, ca, "bob-laptop", 5*time.Second)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	pw := red.Password
	if pw == "" || red.URL != inv.URLs[0] {
		t.Fatalf("redeemed = %+v", red)
	}
	// The credential works and carries the Wormhole ACL.
	b, err := natsbus.Connect(context.Background(), natsbus.Options{URL: inv.URLs[0], User: "bob", Password: pw, TLSCA: ca})
	if err != nil {
		t.Fatalf("connect as bob: %v", err)
	}
	defer b.Close()
	ask := envelope.NewAsk(envelope.Peer{User: "bob", Host: "bob-laptop"}, "alice", "hello", time.Now())
	if err := b.Publish(context.Background(), bus.SubjectFor(ask), ask); err != nil {
		t.Fatalf("publish as bob: %v", err)
	}
	if err := b.Registry().Put(context.Background(), bus.Presence{Handle: "bob", Host: "bob-laptop", LastSeen: time.Now()}); err != nil {
		t.Fatalf("registry: %v", err)
	}
	// Single use: the invite is gone and so is its join user.
	if _, err := Redeem(context.Background(), inv, ca, "bob-laptop", time.Second); err == nil {
		t.Fatal("second redeem succeeded")
	}
	users, err := ListUsers(dir)
	if err != nil || len(users) != 1 || users[0].Handle != "bob" {
		t.Fatalf("users = %+v, %v", users, err)
	}
	invites, _ := ListInvites(dir)
	if len(invites) != 0 {
		t.Fatalf("invites = %+v", invites)
	}
	// A second invite for an existing user needs --rotate.
	if _, _, err := Mint(dir, "bob", InviteOptions{BcryptCost: bcrypt.MinCost}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("re-invite = %v", err)
	}
	tok2, _, err := Mint(dir, "bob", InviteOptions{Rotate: true, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	inv2, _ := Decode(tok2)
	inv2.URLs = inv.URLs
	red2, err := Redeem(context.Background(), inv2, ca, "bob-laptop", 5*time.Second)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	pw2 := red2.Password
	if pw2 == pw {
		t.Fatal("rotate kept the password")
	}
	if _, err := natsbus.Connect(context.Background(), natsbus.Options{URL: inv.URLs[0], User: "bob", Password: pw, TLSCA: ca}); err == nil {
		t.Fatal("old password still accepted after rotation")
	}
	// Revoke from another process; the watcher applies it.
	if err := Revoke(dir, "bob"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := natsgo.Connect(inv.URLs[0], natsgo.UserInfo("bob", pw2), natsgo.RootCAs(ca), natsgo.Timeout(time.Second))
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bob still accepted after revoke")
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := Revoke(dir, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke = %v", err)
	}
}

func TestExpiredInviteIsNotAUser(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	past := func() time.Time { return time.Now().Add(-2 * time.Hour) }
	tok, _, err := Mint(dir, "carol", InviteOptions{TTL: time.Hour, Now: past, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := Decode(tok)
	if !inv.Expired(time.Now()) {
		t.Fatal("not expired")
	}
	inv.URLs = []string{s.ClientURLs()[len(s.ClientURLs())-1]}
	if _, err := Redeem(context.Background(), inv, caFile(t, inv), "carol-laptop", time.Second); err == nil {
		t.Fatal("expired invite redeemed")
	}
	// The watcher prunes it from the file.
	deadline := time.Now().Add(3 * time.Second)
	for {
		list, _ := ListInvites(dir)
		if len(list) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired invite not pruned: %+v", list)
		}
		time.Sleep(30 * time.Millisecond)
	}
	// And a wrong handle is refused by the responder itself.
	tok, _, _ = Mint(dir, "dave", InviteOptions{BcryptCost: bcrypt.MinCost})
	inv, _ = Decode(tok)
	inv.URLs = []string{s.ClientURLs()[len(s.ClientURLs())-1]}
	inv.Handle = "erin"
	if _, err := Redeem(context.Background(), inv, caFile(t, inv), "x", 2*time.Second); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "for ^dave") {
		t.Fatalf("wrong handle: %v", err)
	}
}

func TestRestartKeepsCAAndUsers(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	fp := s.CAFingerprint()
	tok, _, _ := Mint(dir, "bob", InviteOptions{BcryptCost: bcrypt.MinCost})
	inv, _ := Decode(tok)
	inv.URLs = []string{s.ClientURLs()[len(s.ClientURLs())-1]}
	ca := caFile(t, inv)
	red, err := Redeem(context.Background(), inv, ca, "bob-laptop", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pw := red.Password
	s.Close()
	s2 := start(t, dir)
	if s2.CAFingerprint() != fp {
		t.Fatal("CA changed across restarts")
	}
	url := s2.ClientURLs()[len(s2.ClientURLs())-1]
	b, err := natsbus.Connect(context.Background(), natsbus.Options{URL: url, User: "bob", Password: pw, TLSCA: ca})
	if err != nil {
		t.Fatalf("bob after restart: %v", err)
	}
	b.Close()
	for _, f := range []string{caCertFile, caKeyFile, serverCertFile, serverKeyFile, usersFile, adminSecretFile, advertiseFile} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v mode %v", f, err, st.Mode())
		}
	}
	// Nothing in the state dir is a plaintext user password.
	raw, _ := os.ReadFile(filepath.Join(dir, usersFile))
	if strings.Contains(string(raw), pw) {
		t.Fatal("users.json holds a plaintext password")
	}
}

// TestStartErrorsReturn checks that a start that fails after the listener
// is up reports the error instead of hanging in Close.
func TestStartErrorsReturn(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	s.Close()
	// A stale admin secret no longer matches the stored hash.
	if err := os.WriteFile(filepath.Join(dir, adminSecretFile), []byte("wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Start(context.Background(), Options{StateDir: dir, Listen: "127.0.0.1:0", Hostname: "alice-laptop", BcryptCost: bcrypt.MinCost})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "admin connection") {
			t.Fatalf("start = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Start hung instead of returning the error")
	}
	// A CA with its key missing is refused, not silently replaced.
	if err := os.Remove(filepath.Join(dir, caKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Options{StateDir: dir, Listen: "127.0.0.1:0", BcryptCost: bcrypt.MinCost}); err == nil || !strings.Contains(err.Error(), caKeyFile) {
		t.Fatalf("half-present CA = %v", err)
	}
}

// TestJoinUserCannotReadOtherInboxes checks the join user's ACL: it may
// publish its request and read its own replies, nothing else.
func TestJoinUserCannotReadOtherInboxes(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	tok, _, _ := Mint(dir, "bob", InviteOptions{BcryptCost: bcrypt.MinCost})
	inv, _ := Decode(tok)
	url := s.ClientURLs()[len(s.ClientURLs())-1]
	ca := caFile(t, inv)
	deadline := time.Now().Add(5 * time.Second)
	var nc *natsgo.Conn
	for {
		var err error
		nc, err = natsgo.Connect(url, natsgo.UserInfo(inv.JoinUser(), inv.Secret), natsgo.RootCAs(ca), natsgo.Timeout(time.Second))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	defer nc.Close()
	errCh := make(chan error, 4)
	nc.SetErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errCh <- err })
	for _, subj := range []string{"_INBOX.>", "_INBOX.alice.>", "wh.inbox.alice.>", "$WH.join.>"} {
		if _, err := nc.SubscribeSync(subj); err != nil {
			t.Fatal(err)
		}
		_ = nc.Flush()
		select {
		case err := <-errCh:
			if !errors.Is(err, natsgo.ErrPermissionViolation) {
				t.Fatalf("%s: %v", subj, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("join user could subscribe to %s", subj)
		}
	}
	if err := nc.Publish("wh.inbox.alice.bob", []byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	select {
	case err := <-errCh:
		if !errors.Is(err, natsgo.ErrPermissionViolation) {
			t.Fatalf("publish: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("join user could publish on an inbox subject")
	}
}
