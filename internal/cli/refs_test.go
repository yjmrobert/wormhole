package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yjmrobert/wormhole/internal/threads"
)

// seedThreads stores a few threads with alice and bob at distinct times,
// newest last in the returned slice.
func seedThreads(t *testing.T, f *cliFixture) []threads.Thread {
	t.Helper()
	store := f.openFixtureStore(t)
	mk := func(id, peer string, state threads.State, age time.Duration) threads.Thread {
		th := threads.Thread{ID: id, Peer: peer, Direction: threads.DirectionIn, State: state, Title: "t " + id, LastMessageAt: testNow.Add(-age), CreatedAt: testNow.Add(-age), ExpiresAt: testNow.Add(time.Hour)}
		if state == threads.StatePending {
			th.StagedKickoff = "staged " + id
		}
		if err := store.Put(context.Background(), th); err != nil {
			t.Fatal(err)
		}
		return th
	}
	return []threads.Thread{
		mk("01ARZ3NDEKTSV4RRFFQ69G5FAA", "alice", threads.StateClosed, 4*time.Hour),
		mk("01ARZ3NDEKTSV4RRFFQ69G5FAB", "alice", threads.StatePending, 3*time.Hour),
		mk("01ARZ3NDEKTSV4RRFFQ69G5FAC", "bob", threads.StateSent, 2*time.Hour),
		mk("01ARZ3NDEKTSV4RRFFQ69G5FAD", "alice", threads.StatePending, time.Hour),
		mk("01BBBBBBBBBBBBBBBBBBBBBBBB", "bob", threads.StateReplied, 0),
	}
}

func TestResolveThread(t *testing.T) {
	f := newCLIFixture(t)
	seeded := seedThreads(t, f)
	store := f.openFixtureStore(t)
	cases := []struct {
		ref  string
		want string
		code int
		msg  string
	}{
		{ref: "01ARZ3NDEKTSV4RRFFQ69G5FAC", want: "01ARZ3NDEKTSV4RRFFQ69G5FAC"},
		{ref: "01arz3ndektsv4rrffq69g5fac", want: "01ARZ3NDEKTSV4RRFFQ69G5FAC"},
		{ref: "01B", want: "01BBBBBBBBBBBBBBBBBBBBBBBB"},
		{ref: "01ARZ3NDEKTSV4RRFFQ69G5FAD", want: "01ARZ3NDEKTSV4RRFFQ69G5FAD"},
		{ref: "01ARZ", code: ExitUsage, msg: "matches 4 threads"},
		{ref: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", code: ExitNotFound, msg: "wormhole threads"},
		{ref: "01C", code: ExitNotFound, msg: "starts with"},
		{ref: "not-a-thread!", code: ExitNotFound, msg: "is not a thread"},
		{ref: "^alice", want: "01ARZ3NDEKTSV4RRFFQ69G5FAD"},
		{ref: "^alice/2", want: "01ARZ3NDEKTSV4RRFFQ69G5FAB"},
		{ref: "^alice/3", code: ExitNotFound, msg: "only 2 open thread(s)"},
		{ref: "^bob", want: "01BBBBBBBBBBBBBBBBBBBBBBBB"},
		{ref: "^carol", code: ExitNotFound, msg: "no open thread with ^carol"},
		{ref: "^alice/0", code: ExitUsage},
		{ref: "^Not/Valid", code: ExitUsage},
		{ref: "^Not", code: ExitNotFound},
		{ref: "", code: ExitUsage},
	}
	for _, c := range cases {
		got, err := f.app.resolveThread(context.Background(), store, c.ref)
		if c.code == 0 {
			if err != nil || got.ID != c.want {
				t.Errorf("resolve(%q) = %s, %v; want %s", c.ref, got.ID, err, c.want)
			}
			continue
		}
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("resolve(%q) = %v; want code %d containing %q", c.ref, err, c.code, c.msg)
		}
	}
	_ = seeded
}

func TestShortIDsAndRefs(t *testing.T) {
	ids := []string{"01ARZ3NDEKTSV4RRFFQ69G5FAA", "01ARZ3NDEKTSV4RRFFQ69G5FAB", "01BBBBBBBBBBBBBBBBBBBBBBBB"}
	short := shortIDs(ids)
	if short[ids[0]] != "01ARZ3NDEKTSV4RRFFQ69G5FAA" || short[ids[1]] != "01ARZ3NDEKTSV4RRFFQ69G5FAB" || short[ids[2]] != "01BBBB" {
		t.Fatalf("short = %v", short)
	}
	if got := shortIDs([]string{"01ARZ3NDEKTSV4RRFFQ69G5FAA"}); got["01ARZ3NDEKTSV4RRFFQ69G5FAA"] != "01ARZ3" {
		t.Fatalf("single = %v", got)
	}
	f := newCLIFixture(t)
	seedThreads(t, f)
	store := f.openFixtureStore(t)
	all, _ := store.List(context.Background(), threads.Filter{})
	refs := threadRefs(all)
	want := map[string]string{"01BBBBBBBBBBBBBBBBBBBBBBBB": "^bob", "01ARZ3NDEKTSV4RRFFQ69G5FAD": "^alice", "01ARZ3NDEKTSV4RRFFQ69G5FAC": "^bob/2", "01ARZ3NDEKTSV4RRFFQ69G5FAB": "^alice/2"}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v", refs)
	}
	for id, r := range want {
		if refs[id] != r {
			t.Errorf("ref[%s] = %q, want %q", id, refs[id], r)
		}
	}
}

func TestCommandsAcceptThreadRefs(t *testing.T) {
	f := newCLIFixture(t)
	seedThreads(t, f)
	if code := f.run("open", "^alice", "--no-start"); code != ExitOK || !strings.Contains(f.out.String(), "staged 01ARZ3NDEKTSV4RRFFQ69G5FAD") {
		t.Fatalf("open ^alice: %d %s %s", code, f.out.String(), f.errOut.String())
	}
	f.out.Reset()
	if code := f.run("snooze", "^alice/2", "2h", "--json"); code != ExitOK {
		t.Fatalf("snooze ^alice/2: %d %s", code, f.errOut.String())
	}
	var res map[string]any
	_ = json.Unmarshal(f.out.Bytes(), &res)
	if res["thread_id"] != "01ARZ3NDEKTSV4RRFFQ69G5FAB" {
		t.Fatalf("snooze result = %v", res)
	}
	f.out.Reset()
	if code := f.run("inbox", "--json", "--all"); code != ExitOK {
		t.Fatalf("inbox: %d %s", code, f.errOut.String())
	}
	var inbox struct {
		Inbox []map[string]any `json:"inbox"`
	}
	_ = json.Unmarshal(f.out.Bytes(), &inbox)
	if len(inbox.Inbox) != 2 || inbox.Inbox[0]["ref"] != "^alice" || inbox.Inbox[0]["short"] == "" || inbox.Inbox[1]["ref"] != "^alice/2" {
		t.Fatalf("inbox = %+v", inbox.Inbox)
	}
	f.out.Reset()
	if code := f.run("threads", "--json"); code != ExitOK {
		t.Fatalf("threads: %d", code)
	}
	var list struct {
		Threads []map[string]any `json:"threads"`
	}
	_ = json.Unmarshal(f.out.Bytes(), &list)
	if len(list.Threads) != 5 || list.Threads[0]["ref"] != "^bob" || list.Threads[4]["ref"] != nil && list.Threads[4]["ref"] != "" {
		t.Fatalf("threads = %+v", list.Threads)
	}
	f.out.Reset()
	if code := f.run("threads"); code != ExitOK || !strings.Contains(f.out.String(), "REF") || !strings.Contains(f.out.String(), "^bob/2") {
		t.Fatalf("threads table: %d %s", code, f.out.String())
	}
	if code := f.run("open", "01ARZ", "--no-start"); code != ExitUsage {
		t.Fatalf("ambiguous prefix: %d", code)
	}
}
