package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// testCA returns a throwaway certificate DER for token tests.
func testCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wormhole-ca"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// sampleInvite is a valid invite for bob from alice.
func sampleInvite(t *testing.T) Invite {
	return Invite{V: 1, ID: envelope.NewID(), Handle: "bob", By: "alice", URLs: []string{"tls://bus.example.internal:4222", "tls://10.0.0.5:4222"},
		CA: testCA(t), Secret: newSecret(SecretSize), Exp: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC).Unix()}
}

// TestInviteRoundTrip checks Encode and Decode are inverses.
func TestInviteRoundTrip(t *testing.T) {
	inv := sampleInvite(t)
	tok := Encode(inv)
	if !strings.HasPrefix(tok, "wh1.") || strings.ContainsAny(tok, "=+/ \n") {
		t.Fatalf("token %q", tok)
	}
	got, err := Decode("  " + tok + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, inv) {
		t.Fatalf("got %+v\nwant %+v", got, inv)
	}
	inv.By = ""
	if got, err := Decode(Encode(inv)); err != nil || got.By != "" {
		t.Fatalf("empty by: %+v %v", got, err)
	}
}

// TestInviteExpiry checks Expired at, before and after the expiry.
func TestInviteExpiry(t *testing.T) {
	inv := sampleInvite(t)
	exp := inv.Expires()
	if inv.Expired(exp.Add(-time.Second)) || !inv.Expired(exp) || !inv.Expired(exp.Add(time.Hour)) {
		t.Fatal("expiry boundaries")
	}
}

// TestInviteTamper checks that changing any character either fails to
// decode or changes what the token says, which the server then refuses.
func TestInviteTamper(t *testing.T) {
	inv := sampleInvite(t)
	tok := Encode(inv)
	for i := len(TokenPrefix); i < len(tok); i++ {
		c := byte('A')
		if tok[i] == 'A' {
			c = 'B'
		}
		bad := tok[:i] + string(c) + tok[i+1:]
		got, err := Decode(bad)
		if err == nil && reflect.DeepEqual(got, inv) {
			t.Fatalf("position %d: tampered token decodes to the original", i)
		}
		if err != nil && !errors.Is(err, ErrNotInvite) {
			t.Fatalf("position %d: %v", i, err)
		}
	}
	// Truncation and appended data are refused.
	for _, bad := range []string{tok[:len(tok)-5], tok + "AAAA", tok + "."} {
		if _, err := Decode(bad); !errors.Is(err, ErrNotInvite) {
			t.Fatalf("%q decoded: %v", bad[len(bad)-6:], err)
		}
	}
}

// TestInviteRejects checks prefixes, versions and field validation.
func TestInviteRejects(t *testing.T) {
	inv := sampleInvite(t)
	tok := Encode(inv)
	for name, bad := range map[string]string{
		"empty":       "",
		"no prefix":   strings.TrimPrefix(tok, "wh1."),
		"wrong ver":   "wh2." + strings.TrimPrefix(tok, "wh1."),
		"other thing": "nats://bus.example.internal:4222",
		"not base64":  "wh1.!!!",
	} {
		_, err := Decode(bad)
		if !errors.Is(err, ErrNotInvite) || !strings.Contains(err.Error(), "ask the person running wormhole serve for a new one") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, mut := range map[string]func(*Invite){
		"version": func(i *Invite) { i.V = 2 },
		"id":      func(i *Invite) { i.ID = "nope" },
		"handle":  func(i *Invite) { i.Handle = "Bob" },
		"by":      func(i *Invite) { i.By = "A B" },
		"urls":    func(i *Invite) { i.URLs = nil },
		"url":     func(i *Invite) { i.URLs = []string{"tls://a:1,tls://b:2"} },
		"ca":      func(i *Invite) { i.CA = []byte("nope") },
		"secret":  func(i *Invite) { i.Secret = i.Secret[:8] },
		"exp":     func(i *Invite) { i.Exp = 0 },
	} {
		bad := inv
		mut(&bad)
		if _, err := Decode(Encode(bad)); !errors.Is(err, ErrNotInvite) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}
