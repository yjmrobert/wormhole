package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// Thread ids for the reference tests. aliceNew and aliceOld share 25
// characters, so only the whole id tells them apart.
const (
	aliceNew  = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	aliceOld  = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	aliceDone = "01BX5ZZKBKACTAV9WEVGEMMVRZ"
	carolOpen = "01C8XV5KJ7QH0ZB4Y3N2W1T9RS"
)

// seedRefThreads stores two open threads and one closed thread with alice
// and one open thread with carol, newest first in that order.
func seedRefThreads(t *testing.T, store *threads.Store) {
	t.Helper()
	rows := []threads.Thread{
		{ID: aliceNew, Peer: "alice", State: threads.StatePending, Direction: threads.DirectionIn, Title: "newest", StagedKickoff: "Please review", LastMessageAt: testNow.Add(-time.Minute)},
		{ID: aliceOld, Peer: "alice", State: threads.StateDelivered, Direction: threads.DirectionOut, Title: "older", LastMessageAt: testNow.Add(-5 * time.Minute)},
		{ID: aliceDone, Peer: "alice", State: threads.StateClosed, Direction: threads.DirectionOut, Title: "done", LastMessageAt: testNow.Add(-10 * time.Minute)},
		{ID: carolOpen, Peer: "carol", State: threads.StateSent, Direction: threads.DirectionOut, Title: "carol's", LastMessageAt: testNow.Add(-20 * time.Minute)},
	}
	for _, r := range rows {
		r.CreatedAt, r.ExpiresAt = r.LastMessageAt, testNow.Add(time.Hour)
		if err := store.Put(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}

// TestResolveThread covers every <thread> form and its failures.
func TestResolveThread(t *testing.T) {
	f := newCLIFixture(t)
	store := f.openFixtureStore(t)
	seedRefThreads(t, store)
	cases := []struct {
		ref      string
		want     string
		code     int
		contains []string
	}{
		{ref: aliceOld, want: aliceOld},
		{ref: strings.ToLower(aliceNew), want: aliceNew},
		{ref: "01bx", want: aliceDone},
		{ref: "01C8XV", want: carolOpen},
		{ref: "^alice", want: aliceNew},
		{ref: "^alice/1", want: aliceNew},
		{ref: "^alice/2", want: aliceOld},
		{ref: "^carol", want: carolOpen},
		{ref: "01ARZ", code: ExitUsage, contains: []string{`"01ARZ" matches 2 threads`, aliceNew + " ^alice pending", aliceOld + " ^alice delivered", "add characters"}},
		{ref: "^alice/9", code: ExitNotFound, contains: []string{"only 2 open threads with ^alice", "wormhole threads --peer alice"}},
		{ref: "^dave", code: ExitNotFound, contains: []string{"no open thread with ^dave; see: wormhole threads --peer dave"}},
		{ref: "^Bad!", code: ExitNotFound, contains: []string{`"Bad!" is not a valid handle`}},
		{ref: "^alice/0", code: ExitUsage, contains: []string{"^alice/2"}},
		{ref: "^alice/x", code: ExitUsage},
		{ref: "zz!!", code: ExitNotFound, contains: []string{"list yours with: wormhole threads"}},
		{ref: "", code: ExitNotFound},
		{ref: "01Z", code: ExitNotFound, contains: []string{`no thread on this machine starts with "01Z"`}},
		{ref: "01ARZ3NDEKTSV4RRFFQ69G5FAX", code: ExitNotFound, contains: []string{"thread 01ARZ3NDEKTSV4RRFFQ69G5FAX is not on this machine; list yours with: wormhole threads"}},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			got, err := f.app.resolveThread(context.Background(), store, tc.ref)
			if tc.code == 0 {
				if err != nil || got.ID != tc.want {
					t.Fatalf("got %s, %v; want %s", got.ID, err, tc.want)
				}
				return
			}
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != tc.code {
				t.Fatalf("err = %v, want exit %d", err, tc.code)
			}
			for _, s := range tc.contains {
				if !strings.Contains(err.Error(), s) {
					t.Fatalf("err %q does not contain %q", err, s)
				}
			}
		})
	}
}

// TestShortIDs checks the minimum length, uniqueness and prefix property.
func TestShortIDs(t *testing.T) {
	ids := []string{aliceNew, aliceOld, aliceDone, carolOpen, "01BX5ZQ0000000000000000000"}
	short := shortIDs(ids)
	if len(short) != len(ids) {
		t.Fatalf("short = %v", short)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		s := short[id]
		if len(s) < minShortID || !strings.HasPrefix(id, s) || seen[s] {
			t.Fatalf("short[%s] = %q", id, s)
		}
		seen[s] = true
		for _, other := range ids {
			if other != id && strings.HasPrefix(other, s) {
				t.Fatalf("%q is a prefix of both %s and %s", s, id, other)
			}
		}
	}
	if short[carolOpen] != "01C8XV" {
		t.Fatalf("lone id short = %q, want the 6-character minimum", short[carolOpen])
	}
	if short[aliceDone] != "01BX5ZZ" || short[aliceNew] != aliceNew {
		t.Fatalf("short = %v", short)
	}
}

// TestThreadRefs checks per-peer numbering newest first and that closed
// threads get no reference.
func TestThreadRefs(t *testing.T) {
	f := newCLIFixture(t)
	store := f.openFixtureStore(t)
	seedRefThreads(t, store)
	list, err := store.List(context.Background(), threads.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	refs := threadRefs(list)
	want := map[string]string{aliceNew: "^alice", aliceOld: "^alice/2", carolOpen: "^carol"}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v", refs)
	}
	for id, r := range want {
		if refs[id] != r {
			t.Fatalf("refs[%s] = %q, want %q", id, refs[id], r)
		}
	}
}

// TestCommandsAcceptReferences drives open and snooze with ^peer and checks
// that inbox and threads print and emit references.
func TestCommandsAcceptReferences(t *testing.T) {
	f := newCLIFixture(t)
	f.seedThread(t, nil)
	if code := f.run("open", "^alice", "--no-start"); code != ExitOK || !strings.Contains(f.out.String(), "Please review this change") {
		t.Fatalf("open code %d out %q err %q", code, f.out, f.errOut)
	}
	f.out.Reset()
	if code := f.run("snooze", "^alice", "2h", "--json"); code != ExitOK {
		t.Fatalf("snooze code %d out %q", code, f.out)
	}
	var snoozed snoozeResult
	if err := json.Unmarshal(f.out.Bytes(), &snoozed); err != nil || snoozed.ThreadID != testThreadID || snoozed.SnoozedUntil == "" {
		t.Fatalf("snooze = %+v, %v", snoozed, err)
	}
	f.out.Reset()
	if code := f.run("snooze", "01arz3", "off"); code != ExitOK {
		t.Fatalf("snooze by prefix code %d: %s", code, f.out)
	}

	f.out.Reset()
	if code := f.run("inbox", "--json"); code != ExitOK {
		t.Fatalf("inbox code %d", code)
	}
	var inbox struct {
		Inbox []inboxItem `json:"inbox"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &inbox); err != nil || len(inbox.Inbox) != 1 || inbox.Inbox[0].Ref != "^alice" || inbox.Inbox[0].Short != "01ARZ3" {
		t.Fatalf("inbox = %+v, %v", inbox, err)
	}
	f.out.Reset()
	if code := f.run("inbox"); code != ExitOK || !strings.HasPrefix(f.out.String(), "REF") || !strings.Contains(f.out.String(), "^alice  "+testThreadID) {
		t.Fatalf("inbox table:\n%s", f.out)
	}

	store := f.openFixtureStore(t)
	if _, err := store.Update(context.Background(), testThreadID, func(th *threads.Thread) { th.State = threads.StateClosed }); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if code := f.run("threads", "--json"); code != ExitOK {
		t.Fatalf("threads code %d", code)
	}
	var list struct {
		Threads []map[string]any `json:"threads"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &list); err != nil || len(list.Threads) != 1 {
		t.Fatalf("threads = %s, %v", f.out, err)
	}
	if row := list.Threads[0]; row["thread_id"] != testThreadID || row["short"] != "01ARZ3" || row["ref"] != "" || row["state"] != "closed" {
		t.Fatalf("thread row = %v", row)
	}
	f.out.Reset()
	if code := f.run("threads", "--peer", "^alice"); code != ExitOK || !strings.Contains(f.out.String(), "01ARZ3  "+testThreadID) {
		t.Fatalf("threads table:\n%s", f.out)
	}
	if code := f.run("open", "^alice", "--no-start"); code != ExitNotFound {
		t.Fatalf("open on a closed peer code %d", code)
	}
}

// TestCompletionOffersReferences checks thread completion offers ^peer
// references and ids, and handle completion falls back to the thread map
// when the bus is unreachable.
func TestCompletionOffersReferences(t *testing.T) {
	f := newCLIFixture(t)
	seedRefThreads(t, f.openFixtureStore(t))
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	values, _ := f.app.completeThreadIDs(&common{}, false)(cmd, nil, "^al")
	if strings.Join(values, ",") != "^alice\tnewest,^alice/2\tolder" {
		t.Fatalf("ref completion = %q", values)
	}
	values, _ = f.app.completeThreadIDs(&common{}, true)(cmd, nil, "")
	if strings.Join(values, ",") != "^alice\tnewest,"+aliceNew+"\tnewest" {
		t.Fatalf("staged completion = %q", values)
	}
	values, _ = f.app.completeThreadIDs(&common{}, false)(cmd, nil, "01c")
	if len(values) != 1 || values[0] != carolOpen+"\tcarol's" {
		t.Fatalf("id completion = %q", values)
	}
	values, _ = f.app.completeThreadFlag()(cmd, []string{"carol"}, "")
	if strings.Join(values, ",") != "^carol\tcarol's,"+carolOpen+"\tcarol's" {
		t.Fatalf("send --thread completion = %q", values)
	}
	values, _ = f.app.completeSnooze(&common{})(cmd, []string{"^alice"}, "2")
	if len(values) != 1 || !strings.HasPrefix(values[0], "2h\t") {
		t.Fatalf("snooze duration completion = %q", values)
	}

	f.app.NewBus = func(context.Context, *config.Config) (bus.Bus, error) { return nil, errors.New("offline") }
	values, _ = f.app.completeHandles(&common{config: f.cfgPath})(cmd, nil, "")
	if strings.Join(values, ",") != "alice\tfrom your threads,carol\tfrom your threads" {
		t.Fatalf("fallback handles = %q", values)
	}
	values, _ = f.app.completeHandles(&common{config: "/nonexistent/config.yaml"})(cmd, nil, "^c")
	if len(values) != 1 || !strings.HasPrefix(values[0], "carol\t") {
		t.Fatalf("fallback without config = %q", values)
	}
}
