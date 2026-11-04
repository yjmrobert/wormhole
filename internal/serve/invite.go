package serve

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// An invite is the one thing that crosses between machines by hand. It is
// a single token, `wh1.` followed by base64url JSON, small enough for a
// chat message. The secret in it is the password of a temporary join user
// that can do one thing: ask the server for the real credential, once.
const (
	inviteVersion = 1
	invitePrefix  = "wh1."
)

// ErrNotInvite is returned for input that is not a wormhole invite.
var ErrNotInvite = errors.New("this is not a wormhole invite; ask the person running wormhole serve for a new one")

// ErrInviteExpired is returned for an invite past its expiry.
var ErrInviteExpired = errors.New("invite expired")

// Invite is the decoded token.
type Invite struct {
	Version int    `json:"v"`
	ID      string `json:"id"`
	Handle  string `json:"handle"`
	By      string `json:"by,omitempty"`
	// URLs are the bus addresses, tls://host:port, local names first.
	URLs []string `json:"urls"`
	// CA is the base64 DER of the CA certificate the bus presents.
	CA string `json:"ca"`
	// Secret is the join user's password, base64.
	Secret string `json:"secret"`
	// Expires is a Unix timestamp in seconds.
	Expires int64 `json:"exp"`
}

// Encode renders the token.
func Encode(inv Invite) string {
	raw, _ := json.Marshal(inv)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// Decode parses a token. It accepts surrounding whitespace and a leading
// `wormhole join `, since people paste whole lines.
func Decode(token string) (Invite, error) {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "wormhole join ")
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, invitePrefix) {
		return Invite{}, ErrNotInvite
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, invitePrefix))
	if err != nil {
		return Invite{}, fmt.Errorf("%w (%v)", ErrNotInvite, err)
	}
	var inv Invite
	if err := json.Unmarshal(raw, &inv); err != nil {
		return Invite{}, fmt.Errorf("%w (%v)", ErrNotInvite, err)
	}
	if inv.Version != inviteVersion {
		return Invite{}, fmt.Errorf("%w (version %d; this binary understands %d: update wormhole on one side)", ErrNotInvite, inv.Version, inviteVersion)
	}
	if !envelope.ValidID(inv.ID) || !envelope.ValidHandle(inv.Handle) || len(inv.URLs) == 0 || inv.CA == "" || inv.Secret == "" || inv.Expires == 0 {
		return Invite{}, fmt.Errorf("%w (incomplete)", ErrNotInvite)
	}
	if inv.By != "" && !envelope.ValidHandle(inv.By) {
		return Invite{}, fmt.Errorf("%w (bad inviter)", ErrNotInvite)
	}
	if _, err := base64.StdEncoding.DecodeString(inv.CA); err != nil {
		return Invite{}, fmt.Errorf("%w (bad certificate)", ErrNotInvite)
	}
	return inv, nil
}

// Expired reports whether the invite can no longer be redeemed at now.
func (inv Invite) Expired(now time.Time) bool {
	return !now.Before(time.Unix(inv.Expires, 0))
}

// CADER returns the CA certificate bytes.
func (inv Invite) CADER() []byte {
	der, _ := base64.StdEncoding.DecodeString(inv.CA)
	return der
}

// JoinUser is the NATS user name the invite authenticates as.
func (inv Invite) JoinUser() string { return joinUserPrefix + inv.ID }

// JoinSubject is where the join request goes.
func (inv Invite) JoinSubject() string { return joinSubject(inv.ID) }

// joinSubject is the request subject for one invite.
func joinSubject(id string) string { return joinSubjectPrefix + id }

// JoinRequest is what `wormhole join` sends.
type JoinRequest struct {
	Handle string `json:"handle"`
	Host   string `json:"host"`
}

// JoinResponse is what the server answers.
type JoinResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Password string `json:"password,omitempty"`
}
