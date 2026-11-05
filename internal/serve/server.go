// Package serve is the embedded bus: a nats-server with JetStream started
// from programmatic options, its own TLS material, its own users, and the
// one-time invite exchange that `wormhole join` redeems. It is the bus for
// a small team on a developer machine; a team that wants a VM runs
// nats-server from deploy/nats instead, with the same stream, bucket,
// subjects and per-user permissions (internal/bus/nats.UserPermissions).
package serve

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

const (
	// DefaultListen is where the server accepts clients.
	DefaultListen = ":4222"
	// joinSubjectPrefix is the request subject prefix join users may
	// publish on. It is outside the wh.> stream on purpose.
	joinSubjectPrefix  = "$WH.join."
	defaultPoll        = 2 * time.Second
	readyTimeout       = 10 * time.Second
	jetStreamDir       = "jetstream"
	serverName         = "wormhole-bus"
	maxPayload         = 64 * 1024
	jetStreamMaxMemory = 256 << 20
	jetStreamMaxStore  = 4 << 30
)

// Options configure Start.
type Options struct {
	// StateDir holds the TLS material, users.json and the JetStream store.
	StateDir string
	// Listen is host:port; port 0 picks a free one (tests). Default :4222.
	Listen string
	// Advertise are extra hosts (name or address, optional port) to put in
	// invites and in the server certificate.
	Advertise []string
	// Hostname overrides os.Hostname for the certificate and the invites.
	Hostname string
	Log      *slog.Logger
	Now      func() time.Time
	// PollInterval is how often users.json is checked for edits made by
	// another process (default 2s).
	PollInterval time.Duration
	// BcryptCost is for tests; 0 means the default.
	BcryptCost int
}

// Server is a running embedded bus.
type Server struct {
	o     Options
	srv   *server.Server
	opts  *server.Options
	admin *natsgo.Conn
	ca    *ca
	urls  []string
	now   func() time.Time
	log   *slog.Logger

	mu      sync.Mutex // serializes reloads and users.json writes
	lastMod time.Time
	// watching is set once the users.json watcher runs, so Close knows
	// whether to wait for it.
	watching bool
	stop     chan struct{}
	done     chan struct{}
}

// Start brings the bus up: TLS material, users, the stream and the
// registry bucket, the join responder and the users.json watcher.
func Start(ctx context.Context, o Options) (*Server, error) {
	if o.StateDir == "" {
		return nil, errors.New("serve: StateDir is required")
	}
	if o.Listen == "" {
		o.Listen = DefaultListen
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPoll
	}
	if o.Hostname == "" {
		o.Hostname, _ = os.Hostname()
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	now := o.Now()
	authority, err := loadOrCreateCA(o.StateDir, now)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	dns, ips := localNames(o.Hostname, o.Advertise)
	cert, err := authority.serverCert(o.StateDir, dns, ips, now)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	adminPW, err := ensureAdmin(o.StateDir, o.BcryptCost)
	if err != nil {
		return nil, err
	}
	// The modification time is taken before the read: a write that lands
	// during the read then shows up as a change at the next poll.
	usersMod := modTime(usersPath(o.StateDir))
	doc, err := loadUsers(o.StateDir)
	if err != nil {
		return nil, err
	}
	host, port, err := splitListen(o.Listen)
	if err != nil {
		return nil, err
	}
	opts := &server.Options{
		ServerName:         serverName,
		Host:               host,
		Port:               port,
		JetStream:          true,
		StoreDir:           filepath.Join(o.StateDir, jetStreamDir),
		JetStreamMaxMemory: jetStreamMaxMemory,
		JetStreamMaxStore:  jetStreamMaxStore,
		MaxPayload:         maxPayload,
		NoLog:              true,
		NoSigs:             true,
		TLS:                true,
		TLSTimeout:         5,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
		Users: buildUsers(doc, now),
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("nats: %w", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(readyTimeout) {
		srv.Shutdown()
		return nil, fmt.Errorf("nats did not start listening on %s; is the port in use? try: wormhole serve --listen :4223", o.Listen)
	}
	s := &Server{o: o, srv: srv, opts: opts, ca: authority, now: o.Now, log: o.Log, stop: make(chan struct{}), done: make(chan struct{}), lastMod: usersMod}
	actual := port
	if a, ok := srv.Addr().(*net.TCPAddr); ok {
		actual = a.Port
	}
	s.urls = clientURLs(o.Hostname, dns, ips, actual)
	if err := config.WritePrivate(filepath.Join(o.StateDir, advertiseFile), []byte(strings.Join(s.urls, "\n")+"\n")); err != nil {
		s.Close()
		return nil, err
	}
	nc, err := natsgo.Connect("", natsgo.InProcessServer(srv), natsgo.UserInfo(adminUser, adminPW), natsgo.Name("wormhole/serve"))
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("admin connection: %w", err)
	}
	s.admin = nc
	if err := natsbus.EnsureInfrastructure(ctx, nc); err != nil {
		s.Close()
		return nil, err
	}
	if _, err := nc.Subscribe(joinSubjectPrefix+">", func(m *natsgo.Msg) { go s.handleJoin(m) }); err != nil {
		s.Close()
		return nil, fmt.Errorf("join responder: %w", err)
	}
	s.watching = true
	go s.watch()
	s.log.Info("bus listening", "urls", s.urls, "state", o.StateDir, "ca", s.ca.Fingerprint()[:16], "users", len(doc.Users), "invites", len(doc.Invites))
	return s, nil
}

// splitListen parses host:port; port 0 asks the server for a free port.
func splitListen(listen string) (string, int, error) {
	host, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", 0, fmt.Errorf("--listen %q: want host:port such as :4222", listen)
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("--listen %q: bad port", listen)
	}
	if port == 0 {
		port = -1
	}
	return host, port, nil
}

// clientURLs builds the advertised list: the hostname and the machine's
// addresses first (what other machines use), loopback last (what the
// serving machine's own join uses).
func clientURLs(hostname string, dns []string, ips []net.IP, port int) []string {
	var out []string
	add := func(host string) {
		u := "tls://" + net.JoinHostPort(host, strconv.Itoa(port))
		for _, have := range out {
			if have == u {
				return
			}
		}
		out = append(out, u)
	}
	if hostname != "" {
		add(hostname)
	}
	for _, n := range dns {
		if n != "localhost" {
			add(n)
		}
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			add(ip.String())
		}
	}
	add("127.0.0.1")
	return out
}

// ensureAdmin creates the admin credential on first run and returns its
// plaintext, which only the in-process connection ever sees.
func ensureAdmin(stateDir string, cost int) (string, error) {
	doc, err := loadUsers(stateDir)
	if err != nil {
		return "", err
	}
	secretPath := filepath.Join(stateDir, adminSecretFile)
	if doc.Admin.Hash != "" {
		if raw, err := os.ReadFile(secretPath); err == nil {
			pw := strings.TrimSpace(string(raw))
			if pw != "" {
				return pw, nil
			}
		}
	}
	pw, err := newSecret()
	if err != nil {
		return "", err
	}
	h, err := hash(pw, cost)
	if err != nil {
		return "", err
	}
	doc.Admin.Hash = h
	if err := config.WritePrivate(secretPath, []byte(pw+"\n")); err != nil {
		return "", err
	}
	return pw, saveUsers(stateDir, doc)
}

// buildUsers turns the database into the server's user list: the admin,
// every user with the Wormhole ACL, and one join user per live invite.
func buildUsers(doc *usersDoc, now time.Time) []*server.User {
	users := []*server.User{{Username: adminUser, Password: doc.Admin.Hash}}
	for h, u := range doc.Users {
		users = append(users, &server.User{Username: h, Password: u.Hash, Permissions: natsbus.UserPermissions(h)})
	}
	for id, inv := range doc.Invites {
		if !now.Before(inv.Expires) {
			continue
		}
		users = append(users, &server.User{
			Username: joinUserPrefix + id,
			Password: inv.Hash,
			Permissions: &server.Permissions{
				Publish:   &server.SubjectPermission{Allow: []string{joinSubject(id)}},
				Subscribe: &server.SubjectPermission{Allow: []string{natsbus.InboxPrefix(joinUserPrefix+id) + ".>"}},
			},
		})
	}
	return users
}

// reload re-reads users.json and applies it to the running server. The
// caller holds s.mu.
func (s *Server) reload() error {
	mod := modTime(usersPath(s.o.StateDir))
	doc, err := loadUsers(s.o.StateDir)
	if err != nil {
		return err
	}
	opts := *s.opts
	opts.Users = buildUsers(doc, s.now())
	if err := s.srv.ReloadOptions(&opts); err != nil {
		return err
	}
	s.lastMod = mod
	return nil
}

// modTime is a file's modification time, zero when it does not exist.
func modTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// watch reloads the user list when users.json changes and prunes expired
// invites, so `wormhole serve invite` and `revoke` in another process take
// effect without a restart.
func (s *Server) watch() {
	defer close(s.done)
	t := time.NewTicker(s.o.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			s.pruneExpired()
			if st, err := os.Stat(usersPath(s.o.StateDir)); err == nil && !st.ModTime().Equal(s.lastMod) {
				if err := s.reload(); err != nil {
					s.log.Warn("reload users", "err", err)
				} else {
					s.log.Info("reloaded users")
				}
			}
			s.mu.Unlock()
		}
	}
}

// pruneExpired drops invites past their expiry. The caller holds s.mu.
func (s *Server) pruneExpired() {
	doc, err := loadUsers(s.o.StateDir)
	if err != nil {
		return
	}
	now := s.now()
	changed := false
	for id, inv := range doc.Invites {
		if !now.Before(inv.Expires) {
			delete(doc.Invites, id)
			changed = true
		}
	}
	if changed {
		if err := saveUsers(s.o.StateDir, doc); err != nil {
			s.log.Warn("prune invites", "err", err)
		}
	}
}

// handleJoin redeems an invite: it creates the user, deletes the invite,
// answers with the password once, and only then reloads, because the
// reload disconnects the join user the answer is travelling to.
func (s *Server) handleJoin(m *natsgo.Msg) {
	id := strings.TrimPrefix(m.Subject, joinSubjectPrefix)
	s.mu.Lock()
	defer s.mu.Unlock()
	resp, who := s.redeem(id, m.Data)
	raw, _ := json.Marshal(resp)
	if err := m.Respond(raw); err != nil {
		s.log.Warn("join response", "err", err)
	}
	if err := s.admin.Flush(); err != nil {
		s.log.Warn("join response flush", "err", err)
	}
	if resp.OK {
		if err := s.reload(); err != nil {
			s.log.Warn("reload after join", "err", err)
		}
		s.log.Info("joined", "handle", who.Handle, "host", who.Host)
	} else {
		s.log.Warn("join refused", "invite", id, "reason", resp.Error)
	}
}

// redeem is handleJoin's decision: it updates users.json but does not
// reload. The caller holds s.mu.
func (s *Server) redeem(id string, body []byte) (JoinResponse, JoinRequest) {
	var req JoinRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return JoinResponse{Error: "malformed join request; update wormhole on both machines"}, req
	}
	doc, err := loadUsers(s.o.StateDir)
	if err != nil {
		return JoinResponse{Error: "the bus could not read its user list: " + err.Error()}, req
	}
	inv, ok := doc.Invites[id]
	switch {
	case !ok:
		return JoinResponse{Error: "this invite was already used or revoked; ask for a new one: wormhole serve invite " + req.Handle}, req
	case !s.now().Before(inv.Expires):
		return JoinResponse{Error: "this invite expired; ask for a new one: wormhole serve invite " + inv.Handle}, req
	case req.Handle != inv.Handle || !envelope.ValidHandle(req.Handle):
		return JoinResponse{Error: fmt.Sprintf("this invite is for ^%s, not ^%s", inv.Handle, req.Handle)}, req
	}
	if _, exists := doc.Users[inv.Handle]; exists && !inv.Rotate {
		return JoinResponse{Error: fmt.Sprintf("^%s is already on this bus; to replace their credential the inviter runs: wormhole serve invite %s --rotate", inv.Handle, inv.Handle)}, req
	}
	pw, err := newSecret()
	if err != nil {
		return JoinResponse{Error: err.Error()}, req
	}
	h, err := hash(pw, s.o.BcryptCost)
	if err != nil {
		return JoinResponse{Error: err.Error()}, req
	}
	doc.Users[inv.Handle] = userEntry{Hash: h, Created: s.now().UTC()}
	delete(doc.Invites, id)
	if err := saveUsers(s.o.StateDir, doc); err != nil {
		return JoinResponse{Error: "the bus could not save its user list: " + err.Error()}, req
	}
	return JoinResponse{OK: true, Password: pw}, req
}

// ClientURLs is the advertised address list, tls://host:port, the ones
// other machines use first and loopback last.
func (s *Server) ClientURLs() []string { return append([]string(nil), s.urls...) }

// StateDir is where the bus keeps its material.
func (s *Server) StateDir() string { return s.o.StateDir }

// CAFingerprint identifies the CA invites carry.
func (s *Server) CAFingerprint() string { return s.ca.Fingerprint() }

// Reload applies users.json now instead of at the next poll.
func (s *Server) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reload()
}

// Close stops the watcher, the admin connection and the server.
func (s *Server) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
		if s.watching {
			<-s.done
		}
	}
	if s.admin != nil {
		s.admin.Close()
		s.admin = nil
	}
	s.srv.Shutdown()
	s.srv.WaitForShutdown()
}

// Wait blocks until ctx is done, then closes the server.
func (s *Server) Wait(ctx context.Context) {
	<-ctx.Done()
	s.Close()
}

// Redeem is the client side of the exchange: connect as the invite's join
// user over TLS, trusting the CA the token carries, and ask for the
// credential. It is what `wormhole join` runs; it is here so tests and the
// serving machine's own join share it. caFile is a PEM file holding the CA.
func Redeem(ctx context.Context, inv Invite, caFile, host string, timeout time.Duration) (Redeemed, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	// A credential the server rejects usually means a spent invite, but
	// for a moment after `serve invite` the running server has not loaded
	// the join user yet: keep trying for a few seconds before saying so.
	authGrace := authRetryGrace
	if authGrace > timeout {
		authGrace = timeout
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	var authSince time.Time
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Redeemed{}, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		r, retry, err := redeemOnce(ctx, inv, caFile, host)
		if err == nil {
			return r, nil
		}
		lastErr = err
		if errors.Is(err, natsgo.ErrAuthorization) {
			if authSince.IsZero() {
				authSince = time.Now()
			}
			if time.Since(authSince) >= authGrace {
				return Redeemed{}, fmt.Errorf("%w: the bus does not know this invite any more (used, revoked or expired); ask for a new one: wormhole serve invite %s", ErrRefused, inv.Handle)
			}
		}
		if !retry || time.Now().After(deadline) {
			return Redeemed{}, lastErr
		}
	}
}

// authRetryGrace is how long a rejected join credential is retried before
// the invite is reported as spent.
const authRetryGrace = 5 * time.Second

// Redeemed is what a successful exchange yields.
type Redeemed struct {
	Password string
	// URL is the address that answered, which join puts first in bus.url.
	URL string
}

// ErrRefused marks a join the server answered with a reason, as opposed to
// one that could not reach it.
var ErrRefused = errors.New("join refused")

// redeemOnce is one attempt; retry says whether waiting may help.
func redeemOnce(ctx context.Context, inv Invite, caFile, host string) (Redeemed, bool, error) {
	nc, err := natsgo.Connect(strings.Join(inv.URLs, ","),
		natsgo.Name("wormhole/join"),
		natsgo.UserInfo(inv.JoinUser(), inv.Secret),
		natsgo.CustomInboxPrefix(natsbus.InboxPrefix(inv.JoinUser())),
		natsgo.RootCAs(caFile),
		natsgo.Timeout(5*time.Second),
		natsgo.NoReconnect(),
		natsgo.DontRandomize(),
	)
	if err != nil {
		if errors.Is(err, natsgo.ErrAuthorization) {
			// The join user is not there (yet): an invite minted a moment
			// ago, or one already spent.
			return Redeemed{}, true, fmt.Errorf("the bus rejected the invite's credential: %w", err)
		}
		return Redeemed{}, true, err
	}
	defer nc.Close()
	connected := nc.ConnectedUrl()
	body, _ := json.Marshal(JoinRequest{Handle: inv.Handle, Host: host})
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	msg, err := nc.RequestWithContext(reqCtx, inv.JoinSubject(), body)
	if err != nil {
		return Redeemed{}, true, fmt.Errorf("join request: %w", err)
	}
	var resp JoinResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return Redeemed{}, false, fmt.Errorf("join response: %w", err)
	}
	if !resp.OK {
		return Redeemed{}, false, fmt.Errorf("%w: %s", ErrRefused, resp.Error)
	}
	return Redeemed{Password: resp.Password, URL: connected}, false, nil
}

// Prepare creates the state directory, the CA and the admin credential
// and records the addresses invites will carry, without starting the
// server. `wormhole serve install` uses it so invites can be minted before
// the service is up; Start does the same work when it runs first.
func Prepare(o Options) ([]string, error) {
	if o.StateDir == "" {
		return nil, errors.New("serve: StateDir is required")
	}
	if o.Listen == "" {
		o.Listen = DefaultListen
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Hostname == "" {
		o.Hostname, _ = os.Hostname()
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	if _, err := loadOrCreateCA(o.StateDir, o.Now()); err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	if _, err := ensureAdmin(o.StateDir, o.BcryptCost); err != nil {
		return nil, err
	}
	_, port, err := splitListen(o.Listen)
	if err != nil {
		return nil, err
	}
	if port < 0 {
		return nil, errors.New("serve: a fixed port is needed to prepare invites")
	}
	dns, ips := localNames(o.Hostname, o.Advertise)
	urls := clientURLs(o.Hostname, dns, ips, port)
	if err := config.WritePrivate(filepath.Join(o.StateDir, advertiseFile), []byte(strings.Join(urls, "\n")+"\n")); err != nil {
		return nil, err
	}
	return urls, nil
}
