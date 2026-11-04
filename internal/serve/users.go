package serve

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// users.json is the whole account database of an embedded bus: the admin,
// every user and every pending invite, with bcrypt hashes only. Commands
// such as `wormhole serve invite` edit it from another process; the running
// server watches its modification time and reloads its user list in place.
const (
	usersFile       = "users.json"
	adminSecretFile = "admin.secret"
	advertiseFile   = "advertise"
	adminUser       = "admin"
	joinUserPrefix  = "join-"
	// DefaultInviteTTL is how long an invite can be redeemed.
	DefaultInviteTTL = 24 * time.Hour
	secretBytes      = 32
)

// ErrUserExists is returned by Invite for a handle that already has a user.
var ErrUserExists = errors.New("user exists")

// ErrNotFound is returned for an unknown user or invite.
var ErrNotFound = errors.New("not found")

type usersDoc struct {
	Admin   credential             `json:"admin"`
	Users   map[string]userEntry   `json:"users"`
	Invites map[string]inviteEntry `json:"invites"`
}

type credential struct {
	Hash string `json:"hash"`
}

type userEntry struct {
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
}

type inviteEntry struct {
	Handle  string    `json:"handle"`
	By      string    `json:"by,omitempty"`
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
	// Rotate lets the invite replace an existing user's credential.
	Rotate bool `json:"rotate,omitempty"`
}

// UserInfo is one row of `wormhole serve users`.
type UserInfo struct {
	Handle  string    `json:"handle"`
	Created time.Time `json:"created"`
}

// InviteInfo is one pending invite.
type InviteInfo struct {
	ID      string    `json:"id"`
	Handle  string    `json:"handle"`
	By      string    `json:"by,omitempty"`
	Expires time.Time `json:"expires"`
}

// usersPath is where the account database lives under the state dir.
func usersPath(stateDir string) string { return filepath.Join(stateDir, usersFile) }

// loadUsers reads the database; a missing file is an empty one.
func loadUsers(stateDir string) (*usersDoc, error) {
	doc := &usersDoc{Users: map[string]userEntry{}, Invites: map[string]inviteEntry{}}
	raw, err := os.ReadFile(usersPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, doc); err != nil {
		return nil, fmt.Errorf("%s: %w", usersPath(stateDir), err)
	}
	if doc.Users == nil {
		doc.Users = map[string]userEntry{}
	}
	if doc.Invites == nil {
		doc.Invites = map[string]inviteEntry{}
	}
	return doc, nil
}

// saveUsers writes the database atomically with private modes.
func saveUsers(stateDir string, doc *usersDoc) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(usersPath(stateDir), append(raw, '\n'))
}

// writePrivate writes a file readable by the owner only, atomically.
func writePrivate(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// newSecret returns secretBytes of randomness, base64 encoded.
func newSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// hash bcrypts a secret at the given cost (0 means bcrypt.DefaultCost).
func hash(secret string, cost int) (string, error) {
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte(secret), cost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// InviteOptions tune Mint.
type InviteOptions struct {
	// By is the inviting handle, shown to the invitee and seeded as their
	// first inbox-mode sender.
	By string
	// TTL is how long the invite can be redeemed (default DefaultInviteTTL).
	TTL time.Duration
	// Rotate allows an invite for a handle that already has a user; the
	// join then replaces that user's credential.
	Rotate bool
	// URLs are the bus addresses to put in the token. Empty reads the
	// advertise file the last `wormhole serve` run wrote.
	URLs []string
	Now  func() time.Time
	// BcryptCost is for tests; 0 means the default.
	BcryptCost int
}

// Mint creates a one-time invite for handle and returns its token. It works
// whether or not the server is running: the running server picks the new
// join user up from the file.
func Mint(stateDir, handle string, o InviteOptions) (string, Invite, error) {
	if !envelope.ValidHandle(handle) {
		return "", Invite{}, fmt.Errorf("%w: %q", envelope.ErrHandle, handle)
	}
	if o.By != "" && !envelope.ValidHandle(o.By) {
		return "", Invite{}, fmt.Errorf("%w: --by %q", envelope.ErrHandle, o.By)
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	ttl := o.TTL
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}
	urls := o.URLs
	if len(urls) == 0 {
		var err error
		if urls, err = readAdvertise(stateDir); err != nil {
			return "", Invite{}, err
		}
	}
	caCert, err := os.ReadFile(filepath.Join(stateDir, caCertFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", Invite{}, errors.New("no bus here yet; run: wormhole serve")
		}
		return "", Invite{}, err
	}
	cert, err := parseCert(caCert)
	if err != nil {
		return "", Invite{}, fmt.Errorf("%s: %w", filepath.Join(stateDir, caCertFile), err)
	}
	doc, err := loadUsers(stateDir)
	if err != nil {
		return "", Invite{}, err
	}
	if _, exists := doc.Users[handle]; exists && !o.Rotate {
		return "", Invite{}, fmt.Errorf("%w: ^%s is already on this bus; to give them a new credential run: wormhole serve invite %s --rotate, or remove them first with: wormhole serve revoke %s", ErrUserExists, handle, handle, handle)
	}
	secret, err := newSecret()
	if err != nil {
		return "", Invite{}, err
	}
	h, err := hash(secret, o.BcryptCost)
	if err != nil {
		return "", Invite{}, err
	}
	inv := Invite{
		Version: inviteVersion,
		ID:      envelope.NewID(),
		Handle:  handle,
		By:      o.By,
		URLs:    urls,
		CA:      base64.StdEncoding.EncodeToString(cert.Raw),
		Secret:  secret,
		Expires: now().Add(ttl).Unix(),
	}
	// One pending invite per handle: a new one replaces the old.
	for id, e := range doc.Invites {
		if e.Handle == handle {
			delete(doc.Invites, id)
		}
	}
	doc.Invites[inv.ID] = inviteEntry{Handle: handle, By: o.By, Hash: h, Expires: time.Unix(inv.Expires, 0).UTC(), Rotate: o.Rotate}
	if err := saveUsers(stateDir, doc); err != nil {
		return "", Invite{}, err
	}
	return Encode(inv), inv, nil
}

// Revoke removes a user and any invite for them. The running server
// reloads within its poll interval.
func Revoke(stateDir, handle string) error {
	doc, err := loadUsers(stateDir)
	if err != nil {
		return err
	}
	_, hadUser := doc.Users[handle]
	hadInvite := false
	for id, e := range doc.Invites {
		if e.Handle == handle {
			delete(doc.Invites, id)
			hadInvite = true
		}
	}
	if !hadUser && !hadInvite {
		return fmt.Errorf("%w: ^%s is not on this bus; see: wormhole serve users", ErrNotFound, handle)
	}
	delete(doc.Users, handle)
	return saveUsers(stateDir, doc)
}

// ListUsers returns the users sorted by handle.
func ListUsers(stateDir string) ([]UserInfo, error) {
	doc, err := loadUsers(stateDir)
	if err != nil {
		return nil, err
	}
	out := make([]UserInfo, 0, len(doc.Users))
	for h, u := range doc.Users {
		out = append(out, UserInfo{Handle: h, Created: u.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

// ListInvites returns the pending invites, expired ones included, sorted
// by handle.
func ListInvites(stateDir string) ([]InviteInfo, error) {
	doc, err := loadUsers(stateDir)
	if err != nil {
		return nil, err
	}
	out := make([]InviteInfo, 0, len(doc.Invites))
	for id, e := range doc.Invites {
		out = append(out, InviteInfo{ID: id, Handle: e.Handle, By: e.By, Expires: e.Expires})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

// readAdvertise reads the URLs the last serve run advertised.
func readAdvertise(stateDir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, advertiseFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("the bus has not been started here yet; run: wormhole serve (it records the addresses invites carry)")
		}
		return nil, err
	}
	var urls []string
	for _, line := range splitLines(string(raw)) {
		if line != "" {
			urls = append(urls, line)
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("no advertised addresses recorded; run: wormhole serve")
	}
	return urls, nil
}

// splitLines splits on newlines and trims each line.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			out = append(out, line)
			start = i + 1
		}
	}
	return out
}
