package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// minShortID is the shortest prefix shortIDs hands out, so that a short id
// stays unique for a while as the store grows.
const minShortID = 6

// threadArgHelp explains the three forms every <thread> argument accepts.
const threadArgHelp = "<thread> is a thread id, a unique prefix of one (01JB), or ^bob / ^bob/2 for the newest / second-newest open thread with bob."

// ulidAlphabet is Crockford's base32 as ULIDs spell it.
const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// threadRef is a parsed <thread> argument.
type threadRef struct {
	id     string // a full thread id
	prefix string // an upper-cased id prefix
	peer   string // ^peer
	n      int    // ^peer/n, 1-based
}

// parseThreadRef checks a <thread> argument's syntax without the store, so
// commands can refuse a malformed one before loading anything.
func parseThreadRef(ref string) (threadRef, error) {
	ref = strings.TrimSpace(ref)
	if rest, ok := strings.CutPrefix(ref, "^"); ok {
		peer, nth, hasN := strings.Cut(rest, "/")
		if !envelope.ValidHandle(peer) {
			return threadRef{}, fail(ExitNotFound, invalidHandleFormat+"; a reference looks like ^bob or ^bob/2", peer)
		}
		n := 1
		if hasN {
			v, err := strconv.Atoi(nth)
			if err != nil || v < 1 {
				return threadRef{}, fail(ExitUsage, "%q: the number after / counts open threads with ^%s from 1 (newest); try ^%s/2", ref, peer, peer)
			}
			n = v
		}
		return threadRef{peer: peer, n: n}, nil
	}
	up := strings.ToUpper(ref)
	if envelope.ValidID(up) {
		return threadRef{id: up}, nil
	}
	if up == "" || len(up) > 26 || strings.Trim(up, ulidAlphabet) != "" {
		return threadRef{}, fail(ExitNotFound, invalidThreadIDFormat, ref)
	}
	return threadRef{prefix: up}, nil
}

// resolveThread turns a <thread> argument into the thread it names: a full
// id, ^peer or ^peer/n (the n-th newest open thread with peer), or a unique
// case-insensitive id prefix.
func (a *App) resolveThread(ctx context.Context, store *threads.Store, ref string) (threads.Thread, error) {
	r, err := parseThreadRef(ref)
	if err != nil {
		return threads.Thread{}, err
	}
	switch {
	case r.id != "":
		t, err := store.Get(ctx, r.id)
		if err != nil {
			if errors.Is(err, threads.ErrNotFound) {
				return threads.Thread{}, fail(ExitNotFound, threadNotFoundFormat, r.id)
			}
			return threads.Thread{}, err
		}
		return t, nil
	case r.peer != "":
		open, err := store.List(ctx, threads.Filter{Open: true, Peer: r.peer})
		if err != nil {
			return threads.Thread{}, err
		}
		if len(open) == 0 {
			return threads.Thread{}, fail(ExitNotFound, "no open thread with ^%s; see: wormhole threads --peer %s", r.peer, r.peer)
		}
		if r.n > len(open) {
			return threads.Thread{}, fail(ExitNotFound, "^%s/%d: there %s only %d open %s with ^%s; see: wormhole threads --peer %s",
				r.peer, r.n, plural(len(open), "is", "are"), len(open), plural(len(open), "thread", "threads"), r.peer, r.peer)
		}
		return open[r.n-1], nil
	}
	all, err := store.List(ctx, threads.Filter{})
	if err != nil {
		return threads.Thread{}, err
	}
	var matches []threads.Thread
	for _, t := range all {
		if strings.HasPrefix(t.ID, r.prefix) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return threads.Thread{}, fail(ExitNotFound, "no thread on this machine starts with %q; list yours with: wormhole threads", ref)
	case 1:
		return matches[0], nil
	}
	short := shortIDs(threadIDs(all))
	candidates := make([]string, len(matches))
	for i, t := range matches {
		candidates[i] = fmt.Sprintf("%s ^%s %s", short[t.ID], t.Peer, t.State)
	}
	return threads.Thread{}, fail(ExitUsage, "%q matches %d threads: %s; add characters", ref, len(matches), strings.Join(candidates, ", "))
}

// plural picks the singular or plural word for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// threadIDs lists the ids of list.
func threadIDs(list []threads.Thread) []string {
	ids := make([]string, len(list))
	for i, t := range list {
		ids[i] = t.ID
	}
	return ids
}

// shortIDs maps each id to its shortest unique prefix, at least
// minShortID characters long (or the whole id when it is shorter).
func shortIDs(ids []string) map[string]string {
	sorted := slices.Clone(ids)
	sort.Strings(sorted)
	sorted = slices.Compact(sorted)
	out := make(map[string]string, len(sorted))
	for i, id := range sorted {
		n := minShortID
		if i > 0 {
			n = max(n, commonPrefix(id, sorted[i-1])+1)
		}
		if i+1 < len(sorted) {
			n = max(n, commonPrefix(id, sorted[i+1])+1)
		}
		out[id] = id[:min(n, len(id))]
	}
	return out
}

// commonPrefix is the length of the longest common prefix of a and b.
func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// isOpen reports whether t still expects traffic.
func isOpen(t threads.Thread) bool {
	return slices.Contains(threads.OpenStates, t.State)
}

// threadRefs maps each open thread's id to its ^peer/n reference. list is
// newest first, as threads.Store.List returns it; n counts each peer's open
// threads from the newest, which is plain ^peer.
func threadRefs(list []threads.Thread) map[string]string {
	out := map[string]string{}
	count := map[string]int{}
	for _, t := range list {
		if !isOpen(t) {
			continue
		}
		count[t.Peer]++
		if n := count[t.Peer]; n == 1 {
			out[t.ID] = "^" + t.Peer
		} else {
			out[t.ID] = fmt.Sprintf("^%s/%d", t.Peer, n)
		}
	}
	return out
}

// threadLabels is the short id and reference of every thread in the store.
type threadLabels struct {
	short map[string]string
	refs  map[string]string
}

// labelThreads computes short ids and references over the whole store, so
// that a filtered listing prints the same labels an unfiltered one does.
func labelThreads(ctx context.Context, store *threads.Store) (threadLabels, error) {
	all, err := store.List(ctx, threads.Filter{})
	if err != nil {
		return threadLabels{}, err
	}
	return threadLabels{short: shortIDs(threadIDs(all)), refs: threadRefs(all)}, nil
}

// column is what the REF column prints for id: its reference, or its short
// id when the thread is not open.
func (l threadLabels) column(id string) string {
	if r := l.refs[id]; r != "" {
		return r
	}
	if s := l.short[id]; s != "" {
		return s
	}
	return id
}
