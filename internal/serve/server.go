// Package serve is the embedded bus behind `wormhole serve`: a nats-server
// with JetStream and TLS, started from programmatic options (no config file
// is ever written), whose users live in one users.json that other processes
// edit (`serve invite`, `serve revoke`) and the running server polls.
//
// Invites are exchanged for credentials over the bus itself: every pending
// invite is a NATS user join-<id> that may only publish a request on
// $WH.join.<id>; the server answers it over its in-process admin
// connection with the user's real password, once.
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	natsbus "github.com/yjmrobert/wormhole/internal/bus/nats"
)

// JoinPrefix is the subject prefix of join requests; the last token is the
// invite id.
const JoinPrefix = "$WH.join."

// DefaultListen is where serve listens when Options.Listen is empty.
const DefaultListen = ":4222"

// DefaultPort is the NATS client port.
const DefaultPort = 4222

// DefaultPollInterval is how often users.json is checked for changes.
const DefaultPollInterval = 2 * time.Second

// joinGrace keeps a spent invite's join user alive long enough for the
// response to reach it; the invite itself is already gone.
const joinGrace = 10 * time.Second

// JoinSubject is the subject a join for invite id is requested on.
func JoinSubject(id string) string { return JoinPrefix + id }

// JoinUser is the NATS user name of invite id.
func JoinUser(id string) string { return "join-" + id }

// JoinRequest is the body join sends.
type JoinRequest struct {
	Handle string `json:"handle"`
	Host   string `json:"host"`
}

// JoinResponse is the body the server answers with.
type JoinResponse struct {
	OK       bool   `json:"ok"`
	Password string `json:"password,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Options configure Start.
type Options struct {
	// StateDir holds the CA, certificates, users and the JetStream store
	// (DefaultStateDir when empty).
	StateDir string
	// Listen is host:port (DefaultListen when empty). Port 0 picks a free one.
	Listen string
	// Advertise names extra hosts (or host:port) for invites and the
	// server certificate: a DNS name, a VPN address.
	Advertise []string
	Log       *slog.Logger
	Now       func() time.Time
	// BcryptCost hashes the passwords the join responder generates.
	BcryptCost int
	// PollInterval is how often users.json is checked (DefaultPollInterval).
	PollInterval time.Duration
	// Hostname overrides os.Hostname (tests).
	Hostname string
}

// Server is a running embedded bus.
type Server struct {
	o       Options
	srv     *server.Server
	tlsConf *tls.Config
	host    string
	port    int
	urls    []string
	nc      *natsgo.Conn
	sub     *natsgo.Subscription

	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex // reload state
	file    *usersFile
	stamp   fileStamp
	applied string

	graceMu sync.Mutex
	grace   map[string]graceUser
}

type fileStamp struct {
	mod  time.Time
	size int64
}

type graceUser struct {
	hash  string
	until time.Time
}

// Start brings the bus up and returns once it accepts connections, the
// stream and bucket exist, and the join responder is listening.
func Start(ctx context.Context, o Options) (*Server, error) {
	if o.StateDir == "" {
		o.StateDir = DefaultStateDir()
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
	if o.BcryptCost == 0 {
		o.BcryptCost = DefaultBcryptCost
	}
	if o.PollInterval == 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.Hostname == "" {
		o.Hostname, _ = os.Hostname()
	}
	host, portStr, err := net.SplitHostPort(o.Listen)
	if err != nil {
		return nil, fmt.Errorf("--listen %q: want host:port, such as :4222 or 10.0.0.5:4222", o.Listen)
	}
	port := DefaultPort
	if portStr != "" {
		if port, err = strconv.Atoi(portStr); err != nil || port < 0 || port > 65535 {
			return nil, fmt.Errorf("--listen %q: bad port", o.Listen)
		}
	}
	if port == 0 {
		port = server.RANDOM_PORT
	}
	if err := ensureDir(o.StateDir); err != nil {
		return nil, err
	}
	now := o.Now()
	ca, err := loadOrCreateCA(o.StateDir, now)
	if err != nil {
		return nil, err
	}
	dns, ips := certNames(o.Hostname, host, interfaceIPs(), o.Advertise)
	cert, err := ensureServerCert(o.StateDir, ca, dns, ips, now)
	if err != nil {
		return nil, err
	}
	adminPW, err := ensureAdmin(o.StateDir, o.BcryptCost)
	if err != nil {
		return nil, err
	}
	s := &Server{o: o, host: host, port: port, grace: map[string]graceUser{},
		tlsConf: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}

	f, stamp, err := s.readFile()
	if err != nil {
		return nil, err
	}
	s.file, s.stamp = f, stamp
	users := s.users(now)
	srv, err := server.NewServer(s.options(users))
	if err != nil {
		return nil, fmt.Errorf("nats server: %w", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(15 * time.Second) {
		srv.Shutdown()
		return nil, fmt.Errorf("the bus did not start listening on %s; is another process using the port? pick another with --listen", o.Listen)
	}
	s.srv = srv
	s.applied = fingerprint(users)
	if a, ok := srv.Addr().(*net.TCPAddr); ok {
		s.port = a.Port
	}
	s.urls = clientURLs(o.Hostname, host, s.port, interfaceIPs(), o.Advertise)
	if err := writeFile(filepath.Join(o.StateDir, advertiseFile), []byte(strings.Join(s.urls, "\n")+"\n")); err != nil {
		s.shutdown()
		return nil, err
	}
	nc, err := natsgo.Connect("", natsgo.InProcessServer(srv), natsgo.UserInfo(AdminUser, adminPW), natsgo.Name("wormhole/serve"))
	if err != nil {
		s.shutdown()
		return nil, fmt.Errorf("in-process admin connection: %w", err)
	}
	s.nc = nc
	if err := natsbus.EnsureInfrastructure(ctx, nc); err != nil {
		s.shutdown()
		return nil, err
	}
	if s.sub, err = nc.Subscribe(JoinPrefix+">", s.handleJoin); err != nil {
		s.shutdown()
		return nil, err
	}
	if err := nc.Flush(); err != nil {
		s.shutdown()
		return nil, err
	}
	pollCtx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.poll(pollCtx)
	o.Log.Info("bus serving", "urls", strings.Join(s.urls, ","), "state_dir", o.StateDir)
	return s, nil
}

// ClientURLs are the advertised tls:// addresses, local ones first.
func (s *Server) ClientURLs() []string { return slices.Clone(s.urls) }

// StateDir is the directory the server keeps its state in.
func (s *Server) StateDir() string { return s.o.StateDir }

// Close stops the poller, the responder and the server.
func (s *Server) Close() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	s.shutdown()
}

// shutdown releases the connection and the server.
func (s *Server) shutdown() {
	if s.nc != nil {
		s.nc.Close()
	}
	if s.srv != nil {
		s.srv.Shutdown()
		s.srv.WaitForShutdown()
	}
}

// options builds the complete server options for users. They are rebuilt
// from scratch for every reload so nothing the server mutates leaks back.
func (s *Server) options(users []*server.User) *server.Options {
	return &server.Options{
		ServerName:         "wormhole-serve",
		Host:               listenHost(s.host),
		Port:               s.port,
		MaxPayload:         natsbus.MaxMsgSize,
		JetStream:          true,
		StoreDir:           filepath.Join(s.o.StateDir, jetStreamDir),
		JetStreamMaxMemory: 256 << 20,
		JetStreamMaxStore:  4 << 30,
		NoLog:              true,
		NoSigs:             true,
		TLS:                true,
		TLSConfig:          s.tlsConf,
		TLSTimeout:         5,
		Users:              users,
	}
}

// listenHost maps an empty listen host to every interface.
func listenHost(h string) string {
	if h == "" {
		return "0.0.0.0"
	}
	return h
}

// readFile loads users.json with its stamp.
func (s *Server) readFile() (*usersFile, fileStamp, error) {
	path := filepath.Join(s.o.StateDir, usersFileName)
	var st fileStamp
	if info, err := os.Stat(path); err == nil {
		st = fileStamp{mod: info.ModTime(), size: info.Size()}
	}
	f, err := loadUsers(s.o.StateDir)
	return f, st, err
}

// users builds the NATS user list: the admin, every user with the
// template permissions, a join user per live invite, and spent join users
// still in their grace period.
func (s *Server) users(now time.Time) []*server.User {
	f := s.file
	out := []*server.User{{Username: AdminUser, Password: f.Admin.Hash}}
	for _, h := range sortedKeys(f.Users) {
		out = append(out, &server.User{Username: h, Password: f.Users[h].Hash, Permissions: natsbus.UserPermissions(h)})
	}
	joins := map[string]string{}
	for id, inv := range f.Invites {
		if now.Before(inv.Expires) {
			joins[id] = inv.Hash
		}
	}
	s.graceMu.Lock()
	for id, g := range s.grace {
		if now.Before(g.until) {
			if _, ok := joins[id]; !ok {
				joins[id] = g.hash
			}
		} else {
			delete(s.grace, id)
		}
	}
	s.graceMu.Unlock()
	for _, id := range sortedKeys(joins) {
		out = append(out, &server.User{Username: JoinUser(id), Password: joins[id], Permissions: &server.Permissions{
			Publish:   &server.SubjectPermission{Allow: []string{JoinSubject(id)}},
			Subscribe: &server.SubjectPermission{Allow: []string{"_INBOX.>"}},
		}})
	}
	return out
}

// fingerprint identifies a user list, hashes included.
func fingerprint(users []*server.User) string {
	var b strings.Builder
	for _, u := range users {
		b.WriteString(u.Username)
		b.WriteByte(0)
		b.WriteString(u.Password)
		b.WriteByte(0)
	}
	return b.String()
}

// Reload re-reads users.json now and applies any change.
func (s *Server) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(true)
}

// reloadLocked applies users.json to the running server when it or the
// set of live invites changed. Expired invites are pruned from the file.
func (s *Server) reloadLocked(force bool) error {
	path := filepath.Join(s.o.StateDir, usersFileName)
	info, err := os.Stat(path)
	var st fileStamp
	if err == nil {
		st = fileStamp{mod: info.ModTime(), size: info.Size()}
	}
	if force || st != s.stamp {
		f, st2, err := s.readFile()
		if err != nil {
			return err
		}
		s.file, s.stamp = f, st2
	}
	now := s.o.Now()
	expired := false
	for _, inv := range s.file.Invites {
		if !now.Before(inv.Expires) {
			expired = true
		}
	}
	if expired {
		err := update(s.o.StateDir, func(f *usersFile) error {
			for id, inv := range f.Invites {
				if !now.Before(inv.Expires) {
					delete(f.Invites, id)
				}
			}
			return nil
		})
		if err != nil {
			s.o.Log.Warn("could not prune expired invites", "err", err)
		} else if f, st2, err := s.readFile(); err == nil {
			s.file, s.stamp = f, st2
		}
	}
	users := s.users(now)
	fp := fingerprint(users)
	if fp == s.applied {
		return nil
	}
	if err := s.srv.ReloadOptions(s.options(users)); err != nil {
		return fmt.Errorf("reload users: %w", err)
	}
	s.applied = fp
	s.o.Log.Debug("users reloaded", "users", len(s.file.Users), "invites", len(s.file.Invites))
	return nil
}

// poll applies users.json edits made by other processes.
func (s *Server) poll(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.o.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			err := s.reloadLocked(false)
			s.mu.Unlock()
			if err != nil {
				s.o.Log.Warn("users.json", "err", err)
			}
		}
	}
}

// handleJoin answers one join request.
func (s *Server) handleJoin(m *natsgo.Msg) {
	id := strings.TrimPrefix(m.Subject, JoinPrefix)
	var req JoinRequest
	resp := JoinResponse{}
	if err := json.Unmarshal(m.Data, &req); err != nil {
		resp.Error = "malformed join request; update wormhole and run: wormhole join <invite>"
	} else if pw, err := s.join(id, req); err != nil {
		resp.Error = err.Error()
		s.o.Log.Warn("join refused", "handle", req.Handle, "host", req.Host, "invite", id, "err", err)
	} else {
		resp.OK, resp.Password = true, pw
		s.o.Log.Info("joined", "handle", req.Handle, "host", req.Host)
	}
	data, _ := json.Marshal(resp)
	if err := m.Respond(data); err != nil {
		s.o.Log.Warn("join response", "err", err)
	}
}

// join spends invite id for req and returns the new password.
func (s *Server) join(id string, req JoinRequest) (string, error) {
	now := s.o.Now()
	pw := newPassword()
	h, err := hash([]byte(pw), s.o.BcryptCost)
	if err != nil {
		return "", err
	}
	err = update(s.o.StateDir, func(f *usersFile) error {
		inv, ok := f.Invites[id]
		switch {
		case !ok:
			return errors.New("this invite is unknown to the bus: it was already used, revoked or replaced; ask the person running wormhole serve for a new one (wormhole serve invite <handle>)")
		case !now.Before(inv.Expires):
			return fmt.Errorf("this invite expired at %s; ask %s for a new one: wormhole serve invite %s", inv.Expires.Format(time.RFC3339), who(inv.By), inv.Handle)
		case req.Handle != inv.Handle:
			return fmt.Errorf("this invite is for ^%s, not ^%s; ask %s for an invite for %s", inv.Handle, req.Handle, who(inv.By), req.Handle)
		}
		if _, exists := f.Users[inv.Handle]; exists && !inv.Rotate {
			return fmt.Errorf("^%s already joined this bus; ask %s to run: wormhole serve invite %s --rotate", inv.Handle, who(inv.By), inv.Handle)
		}
		f.Users[inv.Handle] = userEntry{Hash: h, Created: now.UTC().Truncate(time.Second)}
		delete(f.Invites, id)
		s.graceMu.Lock()
		s.grace[id] = graceUser{hash: inv.Hash, until: now.Add(joinGrace)}
		s.graceMu.Unlock()
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := s.Reload(); err != nil {
		return "", fmt.Errorf("the bus could not add the user: %v; check the wormhole serve log", err)
	}
	return pw, nil
}

// who names the person to ask for a new invite.
func who(by string) string {
	if by == "" {
		return "the person running wormhole serve"
	}
	return "^" + by
}

// ensureAdmin returns the admin password, generating it on first run, and
// keeps the hash in users.json in step with it.
func ensureAdmin(dir string, cost int) (string, error) {
	path := filepath.Join(dir, adminSecretFile)
	raw, err := os.ReadFile(path)
	pw := strings.TrimSpace(string(raw))
	if errors.Is(err, os.ErrNotExist) || pw == "" {
		pw = newPassword()
		if err := writeFile(path, []byte(pw+"\n")); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	err = update(dir, func(f *usersFile) error {
		if f.Admin.Hash != "" && compareHash(f.Admin.Hash, pw) {
			return nil
		}
		h, err := hash([]byte(pw), cost)
		if err != nil {
			return err
		}
		f.Admin.Hash = h
		return nil
	})
	return pw, err
}

// certNames is the server certificate's SAN set.
func certNames(hostname, listenHost string, ifaceIPs []net.IP, advertise []string) ([]string, []net.IP) {
	dns := map[string]bool{"localhost": true}
	ips := map[string]net.IP{}
	add := func(h string) {
		if h == "" {
			return
		}
		if ip := net.ParseIP(h); ip != nil {
			if !ip.IsUnspecified() {
				ips[ip.String()] = ip
			}
			return
		}
		dns[strings.ToLower(h)] = true
	}
	add("127.0.0.1")
	add("::1")
	add(hostname)
	add(listenHost)
	for _, ip := range ifaceIPs {
		ips[ip.String()] = ip
	}
	for _, a := range advertise {
		h, _ := splitAdvertise(a)
		add(h)
	}
	dnsOut := sortedKeys(dns)
	ipOut := make([]net.IP, 0, len(ips))
	for _, k := range sortedKeys(ips) {
		ipOut = append(ipOut, ips[k])
	}
	return dnsOut, ipOut
}

// clientURLs is what invites carry: the hostname and every interface
// address (or the one listened on), then the advertised extras.
func clientURLs(hostname, listenHost string, port int, ifaceIPs []net.IP, advertise []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(h string, p int) {
		u := "tls://" + net.JoinHostPort(h, strconv.Itoa(p))
		if h != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	if ip := net.ParseIP(listenHost); listenHost != "" && (ip == nil || !ip.IsUnspecified()) {
		add(listenHost, port)
	} else {
		add(hostname, port)
		for _, ip := range ifaceIPs {
			add(ip.String(), port)
		}
	}
	for _, a := range advertise {
		h, p := splitAdvertise(a)
		if p == 0 {
			p = port
		}
		add(h, p)
	}
	return out
}

// splitAdvertise reads host or host:port; port 0 means the server's.
func splitAdvertise(a string) (string, int) {
	a = strings.TrimSpace(a)
	a = strings.TrimPrefix(strings.TrimPrefix(a, "tls://"), "nats://")
	if h, p, err := net.SplitHostPort(a); err == nil {
		n, _ := strconv.Atoi(p)
		return h, n
	}
	return strings.Trim(a, "[]"), 0
}

// interfaceIPs is every usable unicast address on an up interface.
func interfaceIPs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := n.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			out = append(out, ip)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].To4() != nil && out[j].To4() == nil })
	return out
}

// sortedKeys returns map keys in lexical order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
