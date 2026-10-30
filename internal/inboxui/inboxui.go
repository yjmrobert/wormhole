// Package inboxui serves the inbox page: one HTML page on a loopback
// address that lists the messages waiting in the inbox, shows the selected
// one in full and offers the inbox verbs as buttons. It is the glanceable
// surface next to the Copilot app; the tab title carries the waiting count.
//
// Every request must carry the per-start token (the `t` query parameter or
// the X-Wormhole-Token header) and come from a loopback address, so another
// local page cannot drive the inbox from the browser. The verbs are the
// same functions `wormhole open`, `snooze` and `decline` run.
package inboxui

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

//go:embed assets/*
var assets embed.FS

// Options are the server's collaborators.
type Options struct {
	Config *config.Config
	Store  *threads.Store
	Bus    bus.Bus // decline receipts
	App    ghapp.App
	Me     envelope.Peer
	Log    *slog.Logger
	Now    func() time.Time
	// Token guards every request; empty generates one.
	Token string
	// Poll is how often the store is checked for changes to push to open
	// pages (default 1s).
	Poll time.Duration
}

// Server is the inbox page.
type Server struct {
	o   Options
	mux *http.ServeMux

	mu   sync.Mutex
	subs map[chan struct{}]struct{}
	// opening guards against two Open buttons racing on one thread.
	opening map[string]bool
}

// New builds a server. Serve runs it.
func New(o Options) (*Server, error) {
	if o.Config == nil || o.Store == nil || o.Bus == nil || o.App == nil {
		return nil, errors.New("inboxui: config, store, bus and app are required")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Poll == 0 {
		o.Poll = time.Second
	}
	if o.Token == "" {
		o.Token = NewToken()
	}
	s := &Server{o: o, subs: map[chan struct{}]struct{}{}, opening: map[string]bool{}}
	s.routes()
	return s, nil
}

// NewToken returns 32 hex characters of randomness.
func NewToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Token is the secret every request carries.
func (s *Server) Token() string { return s.o.Token }

// Listen binds addr, which must be a loopback host:port.
func Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("inbox_ui %q: %w", addr, err)
	}
	if host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("inbox_ui %q: only loopback addresses are served", addr)
		}
	}
	return net.Listen("tcp", addr)
}

// URL is the page address with the token, for ln's bound address.
func (s *Server) URL(ln net.Listener) string {
	host := ln.Addr().String()
	if h, p, err := net.SplitHostPort(host); err == nil && (h == "::1" || h == "127.0.0.1" || h == "::") {
		host = net.JoinHostPort("127.0.0.1", p)
		if h == "::1" {
			host = "[::1]:" + p
		}
	}
	return "http://" + host + "/?t=" + s.o.Token
}

// Serve runs the page on ln until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second}
	go s.poll(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

// ServeHTTP checks the caller before any route runs.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; form-action 'none'; frame-ancestors 'none'")
	if !fromLoopback(r.RemoteAddr) {
		http.Error(w, "the inbox page is served to this machine only", http.StatusForbidden)
		return
	}
	// The stylesheet and script hold nothing private and the browser
	// requests them without the token; everything else needs it.
	if r.Method == http.MethodGet && (r.URL.Path == "/app.css" || r.URL.Path == "/app.js") {
		s.mux.ServeHTTP(w, r)
		return
	}
	token := r.Header.Get("X-Wormhole-Token")
	if token == "" {
		token = r.URL.Query().Get("t")
	}
	if token != s.o.Token {
		http.Error(w, "missing or wrong token; open the URL from `wormhole inbox --ui`", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func fromLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", s.page)
	m.HandleFunc("GET /app.css", s.asset("assets/app.css", "text/css; charset=utf-8"))
	m.HandleFunc("GET /app.js", s.asset("assets/app.js", "text/javascript; charset=utf-8"))
	m.HandleFunc("GET /api/health", s.health)
	m.HandleFunc("GET /api/inbox", s.inbox)
	m.HandleFunc("GET /api/events", s.events)
	m.HandleFunc("POST /api/threads/{id}/open", s.open)
	m.HandleFunc("POST /api/threads/{id}/snooze", s.snooze)
	m.HandleFunc("POST /api/threads/{id}/decline", s.decline)
	s.mux = m
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	b, err := assets.ReadFile("assets/page.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) asset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := assets.ReadFile(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ctype)
		_, _ = w.Write(b)
	}
}

// item is one staged message as the page sees it: the listing fields plus
// the full text, since reading it here is how approval is earned.
type item struct {
	ThreadID      string `json:"thread_id"`
	Peer          string `json:"peer"`
	Kind          string `json:"kind"`
	Title         string `json:"title"`
	Preview       string `json:"preview"`
	Received      string `json:"received"`
	Repo          string `json:"repo,omitempty"`
	Ref           string `json:"ref,omitempty"`
	SnoozedUntil  string `json:"snoozed_until,omitempty"`
	ExpiresAt     string `json:"expires_at"`
	Hop           int    `json:"hop"`
	HumanReviewed bool   `json:"human_reviewed"`
	Text          string `json:"text"`
}

type inboxView struct {
	OK             bool   `json:"ok"`
	Handle         string `json:"handle"`
	Now            string `json:"now"`
	Waiting        int    `json:"waiting"`
	Snoozed        int    `json:"snoozed"`
	AgentAvailable bool   `json:"agent_available"`
	SnoozeDefault  string `json:"snooze_default"`
	Paused         bool   `json:"paused"`
	Items          []item `json:"items"`
}

const stamp = "2006-01-02T15:04:05Z"

func (s *Server) view(ctx context.Context) (inboxView, error) {
	list, err := s.o.Store.List(ctx, threads.Filter{Open: true, Staged: true})
	if err != nil {
		return inboxView{}, err
	}
	now := s.o.Now()
	v := inboxView{OK: true, Handle: s.o.Config.Handle, Now: now.UTC().Format(stamp), AgentAvailable: len(s.o.Config.Copilot.SafeTools) > 0, SnoozeDefault: daemon.ShortDuration(s.o.Config.Daemon.SnoozeDefault), Paused: daemon.IsPaused(), Items: []item{}}
	for _, t := range list {
		it := item{ThreadID: t.ID, Peer: t.Peer, Kind: "ask", Title: t.Title, Preview: t.LastPreview, Received: t.LastMessageAt.UTC().Format(stamp), Repo: t.Repo, ExpiresAt: t.ExpiresAt.UTC().Format(stamp), Text: t.StagedKickoff}
		if t.Direction == threads.DirectionOut {
			it.Kind = "reply"
		}
		if t.Snoozed(now) {
			it.SnoozedUntil = t.SnoozedUntil.UTC().Format(stamp)
			v.Snoozed++
		} else {
			v.Waiting++
		}
		if last, err := s.o.Store.LastMessage(ctx, t.ID); err == nil {
			if e, err := last.Envelope(); err == nil {
				it.Hop, it.HumanReviewed, it.Ref = e.Hop, e.HumanReviewed, e.Context.Ref
			}
		}
		v.Items = append(v.Items, it)
	}
	return v, nil
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "handle": v.Handle, "waiting": v.Waiting, "snoozed": v.Snoozed})
}

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// events streams the inbox as server-sent events: the whole view on
// connect and on every change, plus a comment every 20 s to keep the
// connection alive.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	ch := s.subscribe()
	defer s.unsubscribe(ch)
	send := func() bool {
		v, err := s.view(r.Context())
		if err != nil {
			return false
		}
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: inbox\ndata: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *Server) unsubscribe(ch chan struct{}) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
}

// Changed wakes every open page to reload the inbox.
func (s *Server) Changed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// poll watches the store for changes made elsewhere (the daemon, the CLI,
// the notification buttons) and pushes them to open pages.
func (s *Server) poll(ctx context.Context) {
	last := ""
	t := time.NewTicker(s.o.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fp, err := s.fingerprint(ctx)
			if err != nil {
				continue
			}
			if fp != last {
				last = fp
				s.Changed()
			}
		}
	}
}

// fingerprint summarizes what the page shows, so a change anywhere in it
// is noticed without diffing.
func (s *Server) fingerprint(ctx context.Context) (string, error) {
	list, err := s.o.Store.List(ctx, threads.Filter{Open: true, Staged: true})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	now := s.o.Now()
	for _, t := range list {
		fmt.Fprintf(&b, "%s|%v|%s;", t.ID, t.Snoozed(now), t.UpdatedAt.Format(time.RFC3339Nano))
	}
	fmt.Fprintf(&b, "paused=%v", daemon.IsPaused())
	return b.String(), nil
}

// thread loads the thread a verb acts on.
func (s *Server) thread(w http.ResponseWriter, r *http.Request) (threads.Thread, bool) {
	id := r.PathValue("id")
	if !envelope.ValidID(id) {
		s.fail(w, http.StatusNotFound, fmt.Errorf("%q is not a thread id", id))
		return threads.Thread{}, false
	}
	t, err := s.o.Store.Get(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusNotFound, fmt.Errorf("thread %s is not on this machine", id))
		return threads.Thread{}, false
	}
	if t.StagedKickoff == "" {
		s.fail(w, http.StatusConflict, fmt.Errorf("nothing is waiting on thread %s (state %s)", id, t.State))
		return threads.Thread{}, false
	}
	return t, true
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// open runs the staged message with the person's tools or, with agent,
// under the auto-mode profile. The page showed the full text and asked;
// this is the press of the button.
func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	t, ok := s.thread(w, r)
	if !ok {
		return
	}
	var req struct {
		Agent bool `json:"agent"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	if s.opening[t.ID] {
		s.mu.Unlock()
		s.fail(w, http.StatusConflict, fmt.Errorf("thread %s is already being opened", t.ID))
		return
	}
	s.opening[t.ID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.opening, t.ID)
		s.mu.Unlock()
		s.Changed()
	}()
	// The run outlives a closed tab: a kickoff that is already running
	// must finish and be recorded, so it is not tied to the request.
	res, _, err := daemon.OpenStaged(context.WithoutCancel(r.Context()), s.o.Config, s.o.Store, s.o.App, t, req.Agent, nil)
	if err != nil {
		switch {
		case errors.Is(err, daemon.ErrNoAgentProfile):
			s.fail(w, http.StatusForbidden, err)
		case errors.Is(err, daemon.ErrStartSession):
			s.fail(w, http.StatusBadGateway, err)
		default:
			s.fail(w, http.StatusInternalServerError, err)
		}
		return
	}
	s.o.Log.Info("opened from the inbox page", "thread", t.ID, "agent", req.Agent, "session", res.SessionID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) snooze(w http.ResponseWriter, r *http.Request) {
	t, ok := s.thread(w, r)
	if !ok {
		return
	}
	var req struct {
		Duration string `json:"duration"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	d, err := daemon.ParseSnooze(req.Duration, s.o.Config.Daemon.SnoozeDefault)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	until, err := daemon.Snooze(r.Context(), s.o.Store, t, s.o.Now(), d)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.Changed()
	out := map[string]any{"ok": true, "thread_id": t.ID}
	if !until.IsZero() {
		out["snoozed_until"] = until.UTC().Format(stamp)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) decline(w http.ResponseWriter, r *http.Request) {
	t, ok := s.thread(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if hits := envelope.ScanSecrets(req.Reason); len(hits) > 0 {
		s.fail(w, http.StatusForbidden, errors.New("the reason looks like it contains a secret; it is sent to the asker"))
		return
	}
	ack, err := daemon.Decline(r.Context(), s.o.Bus, s.o.Store, s.o.Me, t, req.Reason, s.o.Now())
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	s.Changed()
	s.o.Log.Info("declined from the inbox page", "thread", t.ID, "peer", t.Peer)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "thread_id": t.ID, "peer": t.Peer, "status": ack.Body})
}

func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
