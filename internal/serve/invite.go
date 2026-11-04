package serve

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// TokenPrefix starts every invite token; the digit is the format version.
const TokenPrefix = "wh1."

// TokenVersion is the only invite format this binary reads and writes.
const TokenVersion = 1

// SecretSize is the length of an invite's one-time secret in bytes.
const SecretSize = 32

// ErrNotInvite is returned for anything Decode cannot read as an invite.
var ErrNotInvite = errors.New("this is not a wormhole invite; ask the person running wormhole serve for a new one")

// Invite is what an invite token carries: everything a machine needs to
// reach the bus once and exchange the secret for its own credential.
type Invite struct {
	V      int      `json:"v"`
	ID     string   `json:"id"`
	Handle string   `json:"handle"`
	By     string   `json:"by"`
	URLs   []string `json:"urls"`
	// CA is the DER of the bus CA certificate (base64 std in JSON).
	CA []byte `json:"ca"`
	// Secret is the join user's one-time password (base64 std in JSON).
	Secret []byte `json:"secret"`
	// Exp is the expiry in Unix seconds.
	Exp int64 `json:"exp"`
}

// Encode renders the token: TokenPrefix + base64url (no padding) of JSON.
func Encode(inv Invite) string {
	if inv.V == 0 {
		inv.V = TokenVersion
	}
	raw, _ := json.Marshal(inv)
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// Decode parses and validates a token. Every failure wraps ErrNotInvite.
func Decode(token string) (Invite, error) {
	token = strings.TrimSpace(token)
	body, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok {
		if strings.HasPrefix(token, "wh") && strings.Contains(token, ".") {
			return Invite{}, fmt.Errorf("%w (unsupported version %q; update wormhole)", ErrNotInvite, token[:strings.Index(token, ".")])
		}
		return Invite{}, ErrNotInvite
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return Invite{}, fmt.Errorf("%w (bad encoding)", ErrNotInvite)
	}
	var inv Invite
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&inv); err != nil || dec.More() {
		return Invite{}, fmt.Errorf("%w (bad payload)", ErrNotInvite)
	}
	if err := inv.validate(); err != nil {
		return Invite{}, fmt.Errorf("%w (%v)", ErrNotInvite, err)
	}
	return inv, nil
}

// validate checks every field a join depends on.
func (inv Invite) validate() error {
	switch {
	case inv.V != TokenVersion:
		return fmt.Errorf("unsupported version %d; update wormhole", inv.V)
	case !envelope.ValidID(inv.ID):
		return errors.New("bad id")
	case !envelope.ValidHandle(inv.Handle):
		return errors.New("bad handle")
	case inv.By != "" && !envelope.ValidHandle(inv.By):
		return errors.New("bad inviter")
	case len(inv.URLs) == 0:
		return errors.New("no bus address")
	case len(inv.Secret) != SecretSize:
		return errors.New("bad secret")
	case inv.Exp <= 0:
		return errors.New("no expiry")
	}
	for _, u := range inv.URLs {
		if u == "" || strings.ContainsAny(u, ", ") {
			return errors.New("bad bus address")
		}
	}
	if _, err := x509.ParseCertificate(inv.CA); err != nil {
		return errors.New("bad CA certificate")
	}
	return nil
}

// User is the NATS user the invite authenticates as.
func (inv Invite) User() string { return JoinUser(inv.ID) }

// Password is the join user's password: the secret as base64 std, since a
// NATS password travels as a JSON string.
func (inv Invite) Password() string { return base64.StdEncoding.EncodeToString(inv.Secret) }

// Subject is where the join request for this invite is sent.
func (inv Invite) Subject() string { return JoinSubject(inv.ID) }

// Expires is the expiry as a time.
func (inv Invite) Expires() time.Time { return time.Unix(inv.Exp, 0) }

// Expired reports whether the invite can no longer be used at now.
func (inv Invite) Expired(now time.Time) bool { return !now.Before(inv.Expires()) }
