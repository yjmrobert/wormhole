package inboxui

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/bus/memory"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/daemon"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/ghapp"
	"github.com/yjmrobert/wormhole/internal/threads"
)

type fixture struct {
	srv   *Server
	store *threads.Store
	app   *ghapp.Recorder
	alice chan bus.Delivery
	asks  []envelope.Envelope
}

// newFixture builds a server for bob with two staged asks from alice, and
// alice's bus connection collecting what bob publishes.
func newFixture(t *testing.T, yaml string) *fixture {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	broker := memory.NewBroker()
	aliceBus, _ := broker.Connect("alice")
	bobBus, _ := broker.Connect("bob")
	store, err := threads.Open(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, app: &ghapp.Recorder{}, alice: make(chan bus.Delivery, 8)}
	sub, err := aliceBus.Subscribe(context.Background(), func(ctx context.Context, d bus.Delivery) {
		_ = aliceBus.Ack(ctx, d.ID)
		f.alice <- d
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sub.Stop(); store.Close(); broker.Close() })
	ctx := context.Background()
	for i, body := range []string{"Is the retry policy in PR #123 sane?", "Can you look at the flaky e2e on main?"} {
		ask := envelope.NewAsk(envelope.Peer{User: "alice", Host: "alice-laptop"}, "bob", body, time.Now().Add(-time.Duration(i+1)*time.Minute))
		ask.HumanReviewed = true
		ask.Context = envelope.Context{Repo: "owner/repo", Ref: "PR #123"}
		th := threads.Thread{ID: ask.ID, Peer: "alice", Direction: threads.DirectionIn, Mode: threads.ModeInbox, State: threads.StatePending, Title: threads.Preview(body, 60), Repo: "owner/repo", StagedKickoff: daemon.AskKickoff("bob", ask), ExpiresAt: ask.ExpiresAt, CreatedAt: ask.CreatedAt}
		if err := store.Put(ctx, th); err != nil {
			t.Fatal(err)
		}
		if err := store.AddMessage(ctx, ask, threads.DirectionIn); err != nil {
			t.Fatal(err)
		}
		f.asks = append(f.asks, ask)
	}
	f.srv, err = New(Options{Config: cfg, Store: store, Bus: bobBus, App: f.app, Me: envelope.Peer{User: "bob", Host: "bob-laptop"}, Log: slog.New(slog.NewTextHandler(testWriter{t}, nil)), Token: "secret", Poll: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

const bobCfg = "handle: bob\nbus: {kind: memory}\nsenders: [alice]\ncopilot: {workdir: /tmp/wh-ui-test}\n"
const bobAgentCfg = "handle: bob\nbus: {kind: memory}\nsenders: [alice]\ncopilot: {workdir: /tmp/wh-ui-test, safe_tools: [view, grep]}\n"

// call sends a request from loopback with the token in the header.
func (f *fixture) call(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Wormhole-Token", "secret")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	var out map[string]any
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: bad json %q", method, path, rec.Body.String())
		}
	}
	return rec.Code, out
}

func TestGuards(t *testing.T) {
	f := newFixture(t, bobCfg)
	cases := []struct {
		name   string
		remote string
		token  string
		query  string
		want   int
	}{
		{"no token", "127.0.0.1:1", "", "", http.StatusForbidden},
		{"wrong token", "127.0.0.1:1", "nope", "", http.StatusForbidden},
		{"query token", "127.0.0.1:1", "", "?t=secret", http.StatusOK},
		{"header token", "[::1]:1", "secret", "", http.StatusOK},
		{"lan caller", "192.168.1.9:1", "secret", "", http.StatusForbidden},
		{"public caller", "203.0.113.5:1", "", "?t=secret", http.StatusForbidden},
	}
	// The browser loads the stylesheet and script without the token.
	for _, path := range []string{"/app.css", "/app.js"} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s without token: %d", path, rec.Code)
		}
		req.RemoteAddr = "192.168.1.9:1"
		rec = httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from the lan: %d", path, rec.Code)
		}
	}
	// The page itself does need it.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("page without token: %d", rec.Code)
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/api/health"+c.query, nil)
		req.RemoteAddr = c.remote
		if c.token != "" {
			req.Header.Set("X-Wormhole-Token", c.token)
		}
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: csp %q", c.name, csp)
		}
	}
}

func TestPageAndAssets(t *testing.T) {
	f := newFixture(t, bobCfg)
	for path, want := range map[string]string{"/": "<title>Wormhole inbox</title>", "/app.js": "EventSource", "/app.css": "--blue"} {
		req := httptest.NewRequest("GET", path+"?t=secret", nil)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d, lacks %q", path, rec.Code, want)
		}
	}
	code, _ := f.call(t, "GET", "/nothing", "")
	if code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", code)
	}
}

func TestInboxListsFullText(t *testing.T) {
	f := newFixture(t, bobCfg)
	code, out := f.call(t, "GET", "/api/inbox", "")
	if code != 200 || out["handle"] != "bob" || out["waiting"] != float64(2) || out["agent_available"] != false {
		t.Fatalf("%d %v", code, out)
	}
	items := out["items"].([]any)
	first := items[0].(map[string]any)
	if first["thread_id"] != f.asks[0].ID || first["kind"] != "ask" || first["ref"] != "PR #123" || first["human_reviewed"] != true || first["hop"] != float64(1) {
		t.Fatalf("item = %v", first)
	}
	if text, _ := first["text"].(string); !strings.HasPrefix(text, "[wormhole] Message from ^alice") || !strings.HasSuffix(text, "---\nIs the retry policy in PR #123 sane?") {
		t.Fatalf("text = %q", text)
	}
}

func TestSnoozeAndBack(t *testing.T) {
	f := newFixture(t, bobCfg)
	id := f.asks[0].ID
	code, out := f.call(t, "POST", "/api/threads/"+id+"/snooze", `{"duration":"2h"}`)
	if code != 200 || out["snoozed_until"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	_, view := f.call(t, "GET", "/api/inbox", "")
	if view["waiting"] != float64(1) || view["snoozed"] != float64(1) {
		t.Fatalf("view = %v", view)
	}
	if code, out := f.call(t, "POST", "/api/threads/"+id+"/snooze", `{"duration":"soon"}`); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "not a duration") {
		t.Fatalf("bad duration: %d %v", code, out)
	}
	if code, out := f.call(t, "POST", "/api/threads/"+id+"/snooze", `{"duration":"off"}`); code != 200 || out["snoozed_until"] != nil {
		t.Fatalf("off: %d %v", code, out)
	}
	if code, _ := f.call(t, "POST", "/api/threads/01NOTATHREADXXXXXXXXXXXXXX/snooze", `{}`); code != http.StatusNotFound {
		t.Fatalf("unknown thread: %d", code)
	}
}

func TestDeclineTellsAlice(t *testing.T) {
	f := newFixture(t, bobCfg)
	id := f.asks[1].ID
	if code, out := f.call(t, "POST", "/api/threads/"+id+"/decline", `{"reason":"token ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`); code != http.StatusForbidden || !strings.Contains(out["error"].(string), "secret") {
		t.Fatalf("secret reason: %d %v", code, out)
	}
	code, out := f.call(t, "POST", "/api/threads/"+id+"/decline", `{"reason":"taking it to standup"}`)
	if code != 200 || out["status"] != "declined: taking it to standup" || out["peer"] != "alice" {
		t.Fatalf("%d %v", code, out)
	}
	select {
	case d := <-f.alice:
		if reason, ok := d.Env.Declined(); !ok || reason != "taking it to standup" || d.Env.ThreadID != id || d.Sender != "bob" {
			t.Fatalf("alice got %+v", d.Env)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("alice never got the receipt")
	}
	th, _ := f.store.Get(context.Background(), id)
	if th.State != threads.StateDeclined || th.StagedKickoff != "" {
		t.Fatalf("thread = %+v", th)
	}
	if code, _ := f.call(t, "POST", "/api/threads/"+id+"/decline", `{}`); code != http.StatusConflict {
		t.Fatalf("second decline: %d", code)
	}
	_, view := f.call(t, "GET", "/api/inbox", "")
	if view["waiting"] != float64(1) {
		t.Fatalf("view = %v", view)
	}
}

func TestOpenRunsTheKickoff(t *testing.T) {
	f := newFixture(t, bobCfg)
	id := f.asks[0].ID
	// No profile: the agent button is refused, nothing runs.
	if code, out := f.call(t, "POST", "/api/threads/"+id+"/open", `{"agent":true}`); code != http.StatusForbidden || !strings.Contains(out["error"].(string), "safe_tools") || len(f.app.Ops()) != 0 {
		t.Fatalf("agent without profile: %d %v %v", code, out, f.app.Ops())
	}
	code, out := f.call(t, "POST", "/api/threads/"+id+"/open", `{}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	res := out["result"].(map[string]any)
	calls := f.app.Snapshot()
	if len(calls) != 2 || calls[0].Op != "create" || calls[1].Op != "open" || calls[1].SessionID != res["session_id"] || res["opened"] != true || res["started"] != true {
		t.Fatalf("calls = %+v result %v", calls, res)
	}
	if len(calls[0].Session.AvailableTools) != 0 || !strings.HasSuffix(calls[0].Session.Kickoff, "---\nIs the retry policy in PR #123 sane?") || calls[0].Session.Workdir != "/tmp/wh-ui-test" {
		t.Fatalf("session = %+v", calls[0].Session)
	}
	th, _ := f.store.Get(context.Background(), id)
	if th.StagedKickoff != "" || th.LocalSessionID != res["session_id"] || th.State != threads.StateDelivered || !th.Opened {
		t.Fatalf("thread = %+v", th)
	}
	if code, _ := f.call(t, "POST", "/api/threads/"+id+"/open", `{}`); code != http.StatusConflict {
		t.Fatalf("open twice: %d", code)
	}
}

func TestOpenWithAgentProfile(t *testing.T) {
	f := newFixture(t, bobAgentCfg)
	_, view := f.call(t, "GET", "/api/inbox", "")
	if view["agent_available"] != true {
		t.Fatal(view)
	}
	code, out := f.call(t, "POST", "/api/threads/"+f.asks[1].ID+"/open", `{"agent":true}`)
	if code != 200 || out["result"].(map[string]any)["agent"] != true {
		t.Fatalf("%d %v", code, out)
	}
	calls := f.app.Snapshot()
	if strings.Join(calls[0].Session.AvailableTools, ",") != "view,grep" {
		t.Fatalf("session = %+v", calls[0].Session)
	}
	th, _ := f.store.Get(context.Background(), f.asks[1].ID)
	if th.Mode != threads.ModeAuto {
		t.Fatalf("mode = %s", th.Mode)
	}
}

// TestEventsPushChanges runs the server for real and reads the stream: the
// view arrives on connect and again when the store changes under it.
func TestEventsPushChanges(t *testing.T) {
	f := newFixture(t, bobCfg)
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.srv.Serve(ctx, ln) }()
	url := f.srv.URL(ln)
	if !strings.HasPrefix(url, "http://127.0.0.1:") || !strings.HasSuffix(url, "/?t=secret") {
		t.Fatalf("url = %s", url)
	}
	base := strings.TrimSuffix(url, "/?t=secret")
	resp, err := http.Get(base + "/api/events?t=secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	rd := bufio.NewReader(resp.Body)
	next := func() inboxView {
		t.Helper()
		var v inboxView
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
					t.Fatal(err)
				}
				return v
			}
		}
		t.Fatal("no event")
		return v
	}
	if v := next(); v.Waiting != 2 {
		t.Fatalf("initial view = %+v", v)
	}
	// A change made elsewhere (the CLI, say) is noticed by the poller.
	if _, err := daemon.Snooze(ctx, f.store, mustGet(t, f.store, f.asks[0].ID), time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if v := next(); v.Waiting != 1 || v.Snoozed != 1 {
		t.Fatalf("after snooze = %+v", v)
	}
	// Health answers too.
	hr, err := http.Get(base + "/api/health?t=secret")
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	_ = json.NewDecoder(hr.Body).Decode(&h)
	hr.Body.Close()
	if h["ok"] != true || h["waiting"] != float64(1) {
		t.Fatalf("health = %v", h)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func mustGet(t *testing.T, s *threads.Store, id string) threads.Thread {
	t.Helper()
	th, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func TestListenRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.168.1.5:0", "bus.example.internal:0", "nonsense"} {
		if ln, err := Listen(addr); err == nil {
			ln.Close()
			t.Errorf("%s accepted", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		ln, err := Listen(addr)
		if err != nil {
			t.Errorf("%s refused: %v", addr, err)
			continue
		}
		ln.Close()
	}
}

func TestNewToken(t *testing.T) {
	a, b := NewToken(), NewToken()
	if len(a) != 32 || a == b {
		t.Fatal(a, b)
	}
}
