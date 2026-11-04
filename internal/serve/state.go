package serve

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// Files in the state directory.
const (
	usersFileName   = "users.json"
	adminSecretFile = "admin.secret"
	advertiseFile   = "advertise"
	lockFileName    = "users.json.lock"
	jetStreamDir    = "jetstream"
)

// AdminUser is the server's own user; it is only used in-process.
const AdminUser = "admin"

// DefaultBcryptCost hashes user and invite secrets.
const DefaultBcryptCost = 10

// DefaultInviteTTL is how long an invite stays valid.
const DefaultInviteTTL = 24 * time.Hour

// Errors callers map to exit codes.
var (
	// ErrExists: the handle already has a credential on this bus.
	ErrExists = errors.New("already on this bus")
	// ErrNotFound: no such user or invite.
	ErrNotFound = errors.New("not on this bus")
	// ErrNotServing: the state directory was never initialized by serve.
	ErrNotServing = errors.New("this machine has not run wormhole serve")
)

// DefaultStateDir is where serve keeps its state.
func DefaultStateDir() string { return filepath.Join(config.StateDir(), "serve") }

// usersFile is users.json.
type usersFile struct {
	Admin   adminEntry             `json:"admin"`
	Users   map[string]userEntry   `json:"users"`
	Invites map[string]inviteEntry `json:"invites"`
}

type adminEntry struct {
	Hash string `json:"hash"`
}

type userEntry struct {
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
}

type inviteEntry struct {
	Handle  string    `json:"handle"`
	By      string    `json:"by"`
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
	// Rotate lets the join replace an existing credential for Handle.
	Rotate bool `json:"rotate,omitempty"`
}

// UserInfo is one user as `serve users` lists it.
type UserInfo struct {
	Handle  string    `json:"handle"`
	Created time.Time `json:"created"`
}

// InviteInfo is one pending invite as `serve users` lists it.
type InviteInfo struct {
	ID      string    `json:"id"`
	Handle  string    `json:"handle"`
	By      string    `json:"by"`
	Expires time.Time `json:"expires"`
	Rotate  bool      `json:"rotate,omitempty"`
}

// ensureDir creates dir with mode 700 and tightens an existing one.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// writeFile writes path atomically with mode 600.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// loadUsers reads users.json; a missing file is an empty one.
func loadUsers(dir string) (*usersFile, error) {
	f := &usersFile{Users: map[string]userEntry{}, Invites: map[string]inviteEntry{}}
	raw, err := os.ReadFile(filepath.Join(dir, usersFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, f); err != nil {
		return nil, fmt.Errorf("%s: %w (fix or remove the file; removing it drops every user)", filepath.Join(dir, usersFileName), err)
	}
	if f.Users == nil {
		f.Users = map[string]userEntry{}
	}
	if f.Invites == nil {
		f.Invites = map[string]inviteEntry{}
	}
	return f, nil
}

// saveUsers writes users.json atomically.
func saveUsers(dir string, f *usersFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, usersFileName), append(raw, '\n'))
}

// lockTimeout bounds how long a writer waits for another one.
const lockTimeout = 10 * time.Second

// staleLock is the age after which a lock file is assumed abandoned.
const staleLock = 30 * time.Second

// lockState takes the users.json write lock: a file created exclusively,
// which works across processes on every platform.
func lockState(dir string) (func(), error) {
	p := filepath.Join(dir, lockFileName)
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { _ = os.Remove(p) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > staleLock {
			_ = os.Remove(p)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held by another wormhole serve command; remove it if none is running", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// update loads users.json under the lock, applies fn, and saves the
// result when fn returns nil.
func update(dir string, fn func(f *usersFile) error) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	unlock, err := lockState(dir)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := loadUsers(dir)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	return saveUsers(dir, f)
}

// newSecret returns n random bytes.
func newSecret(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// newPassword returns a random password as base64 std.
func newPassword() string { return base64.StdEncoding.EncodeToString(newSecret(SecretSize)) }

// hash bcrypts secret at cost (DefaultBcryptCost when zero).
func hash(secret []byte, cost int) (string, error) {
	if cost == 0 {
		cost = DefaultBcryptCost
	}
	h, err := bcrypt.GenerateFromPassword(secret, cost)
	return string(h), err
}

// InviteOptions tune NewInvite.
type InviteOptions struct {
	// Rotate allows an invite for a handle that already has a credential;
	// the join replaces it.
	Rotate bool
	// BcryptCost hashes the secret (DefaultBcryptCost when zero).
	BcryptCost int
}

// NewInvite mints an invite for handle, records it in users.json, and returns
// the token. urls nil reads the addresses the last `wormhole serve` run
// advertised. by is the inviting handle and may be empty.
func NewInvite(stateDir, handle, by string, ttl time.Duration, now time.Time, urls []string, opts ...InviteOptions) (string, error) {
	var o InviteOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if !envelope.ValidHandle(handle) {
		return "", fmt.Errorf("%q is not a valid handle (lowercase letters, digits, - and _; such as bob)", handle)
	}
	if handle == AdminUser {
		return "", fmt.Errorf("%q is reserved for the server itself; pick another handle", handle)
	}
	if by != "" && !envelope.ValidHandle(by) {
		return "", fmt.Errorf("--by %q is not a valid handle", by)
	}
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}
	ca, err := CACertDER(stateDir)
	if err != nil {
		return "", fmt.Errorf("%w: no CA in %s; run: wormhole serve", ErrNotServing, stateDir)
	}
	if urls == nil {
		if urls, err = ReadAdvertised(stateDir); err != nil {
			return "", err
		}
	}
	if len(urls) == 0 {
		return "", fmt.Errorf("%w: no bus address known; run: wormhole serve", ErrNotServing)
	}
	inv := Invite{V: TokenVersion, ID: envelope.NewID(), Handle: handle, By: by, URLs: urls, CA: ca, Secret: newSecret(SecretSize), Exp: now.Add(ttl).Unix()}
	h, err := hash([]byte(inv.Password()), o.BcryptCost)
	if err != nil {
		return "", err
	}
	err = update(stateDir, func(f *usersFile) error {
		if _, ok := f.Users[handle]; ok && !o.Rotate {
			return fmt.Errorf("^%s is %w; to give %s a new credential run: wormhole serve invite %s --rotate (or remove them first: wormhole serve revoke %s)", handle, ErrExists, handle, handle, handle)
		}
		f.Invites[inv.ID] = inviteEntry{Handle: handle, By: by, Hash: h, Expires: inv.Expires().UTC(), Rotate: o.Rotate}
		return nil
	})
	if err != nil {
		return "", err
	}
	return Encode(inv), nil
}

// Revoke removes handle's credential and any invite pending for it. A
// running server drops the user within its poll interval.
func Revoke(stateDir, handle string) error {
	return update(stateDir, func(f *usersFile) error {
		found := false
		if _, ok := f.Users[handle]; ok {
			delete(f.Users, handle)
			found = true
		}
		for id, inv := range f.Invites {
			if inv.Handle == handle {
				delete(f.Invites, id)
				found = true
			}
		}
		if !found {
			return fmt.Errorf("^%s is %w; list who is: wormhole serve users", handle, ErrNotFound)
		}
		return nil
	})
}

// ListUsers returns every user, sorted by handle.
func ListUsers(stateDir string) ([]UserInfo, error) {
	f, err := loadUsers(stateDir)
	if err != nil {
		return nil, err
	}
	out := make([]UserInfo, 0, len(f.Users))
	for h, u := range f.Users {
		out = append(out, UserInfo{Handle: h, Created: u.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

// ListInvites returns the invites that have not expired, soonest first.
func ListInvites(stateDir string) ([]InviteInfo, error) {
	f, err := loadUsers(stateDir)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]InviteInfo, 0, len(f.Invites))
	for id, inv := range f.Invites {
		if now.Before(inv.Expires) {
			out = append(out, InviteInfo{ID: id, Handle: inv.Handle, By: inv.By, Expires: inv.Expires, Rotate: inv.Rotate})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Expires.Equal(out[j].Expires) {
			return out[i].Expires.Before(out[j].Expires)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ReadAdvertised returns the client URLs the last serve run advertised.
func ReadAdvertised(stateDir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, advertiseFile))
	if err != nil {
		return nil, fmt.Errorf("%w: no bus address known (%s is missing); run: wormhole serve", ErrNotServing, filepath.Join(stateDir, advertiseFile))
	}
	var urls []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			urls = append(urls, l)
		}
	}
	return urls, nil
}

// compareHash reports whether secret matches a bcrypt hash.
func compareHash(h, secret string) bool {
	return bcrypt.CompareHashAndPassword([]byte(h), []byte(secret)) == nil
}
