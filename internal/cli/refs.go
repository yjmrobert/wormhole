package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// Nobody types a thread id. Every command that takes a <thread> resolves
// it here: a full id, a unique prefix of one, or ^peer for the newest open
// thread with that peer (^peer/2 for the one before). inbox and threads
// print the same forms beside the full id.

const (
	// shortIDMin is the shortest prefix shortIDs will print.
	shortIDMin = 6
	threadHelp = "a thread id, a unique prefix of one, ^bob (the newest open thread with bob) or ^bob/2"
)

// resolveThread turns ref into the thread it names.
func (a *App) resolveThread(ctx context.Context, store *threads.Store, ref string) (threads.Thread, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return threads.Thread{}, fail(ExitUsage, "missing thread: %s", threadHelp)
	}
	if strings.HasPrefix(ref, "^") {
		return a.resolvePeerRef(ctx, store, ref)
	}
	id := strings.ToUpper(ref)
	if envelope.ValidID(id) {
		t, err := store.Get(ctx, id)
		if err != nil {
			return threads.Thread{}, fail(ExitNotFound, threadNotFoundFormat, id)
		}
		return t, nil
	}
	list, err := store.List(ctx, threads.Filter{})
	if err != nil {
		return threads.Thread{}, err
	}
	var matches []threads.Thread
	for _, t := range list {
		if strings.HasPrefix(t.ID, id) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if !isULIDPrefix(id) {
			return threads.Thread{}, fail(ExitNotFound, "%q is not a thread: give %s; list yours with: wormhole threads", ref, threadHelp)
		}
		return threads.Thread{}, fail(ExitNotFound, "no thread on this machine starts with %q; list yours with: wormhole threads", ref)
	}
	short := shortIDs(idsOf(list))
	var cands []string
	for _, t := range matches {
		cands = append(cands, fmt.Sprintf("%s ^%s %s", short[t.ID], t.Peer, t.State))
	}
	return threads.Thread{}, fail(ExitUsage, "%q matches %d threads: %s; add characters, or use ^%s", ref, len(matches), strings.Join(cands, ", "), matches[0].Peer)
}

// resolvePeerRef handles ^peer and ^peer/n.
func (a *App) resolvePeerRef(ctx context.Context, store *threads.Store, ref string) (threads.Thread, error) {
	peer, nth := strings.TrimPrefix(ref, "^"), 1
	if i := strings.IndexByte(peer, '/'); i >= 0 {
		n, err := strconv.Atoi(peer[i+1:])
		if err != nil || n < 1 {
			return threads.Thread{}, fail(ExitUsage, "%q: want ^%s or ^%s/<n> with n from 1 (newest)", ref, peer[:i], peer[:i])
		}
		peer, nth = peer[:i], n
	}
	if !envelope.ValidHandle(peer) {
		return threads.Thread{}, fail(ExitNotFound, invalidHandleFormat, peer)
	}
	list, err := store.List(ctx, threads.Filter{Open: true, Peer: peer})
	if err != nil {
		return threads.Thread{}, err
	}
	switch {
	case len(list) == 0:
		return threads.Thread{}, fail(ExitNotFound, "no open thread with ^%s; see: wormhole threads --peer %s", peer, peer)
	case nth > len(list):
		return threads.Thread{}, fail(ExitNotFound, "only %d open thread(s) with ^%s; see: wormhole threads --peer %s", len(list), peer, peer)
	}
	return list[nth-1], nil
}

// isULIDPrefix reports whether s could begin a ULID.
func isULIDPrefix(s string) bool {
	if s == "" || len(s) > 26 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			return false
		}
	}
	return true
}

// idsOf lists the ids of threads.
func idsOf(list []threads.Thread) []string {
	ids := make([]string, len(list))
	for i, t := range list {
		ids[i] = t.ID
	}
	return ids
}

// shortIDs gives each id its shortest unique prefix, at least shortIDMin
// characters.
func shortIDs(ids []string) map[string]string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	out := make(map[string]string, len(ids))
	for i, id := range sorted {
		n := shortIDMin
		for _, other := range []int{i - 1, i + 1} {
			if other < 0 || other >= len(sorted) || sorted[other] == id {
				continue
			}
			common := 0
			for common < len(id) && common < len(sorted[other]) && id[common] == sorted[other][common] {
				common++
			}
			if common+1 > n {
				n = common + 1
			}
		}
		if n > len(id) {
			n = len(id)
		}
		out[id] = id[:n]
	}
	return out
}

// threadRefs gives each open thread its ^peer/n reference, counting
// newest first per peer; the newest is plain ^peer. list must be in the
// order List returns (newest first).
func threadRefs(list []threads.Thread) map[string]string {
	open := map[threads.State]bool{}
	for _, st := range threads.OpenStates {
		open[st] = true
	}
	count := map[string]int{}
	out := map[string]string{}
	for _, t := range list {
		if !open[t.State] {
			continue
		}
		count[t.Peer]++
		if count[t.Peer] == 1 {
			out[t.ID] = "^" + t.Peer
		} else {
			out[t.ID] = fmt.Sprintf("^%s/%d", t.Peer, count[t.Peer])
		}
	}
	return out
}

// refsFor loads every thread and returns the short id and reference for
// each, for listings.
func (a *App) refsFor(ctx context.Context, store *threads.Store) (short, refs map[string]string) {
	all, err := store.List(ctx, threads.Filter{})
	if err != nil {
		return map[string]string{}, map[string]string{}
	}
	return shortIDs(idsOf(all)), threadRefs(all)
}

// refOrShort is what a listing prints in its REF column.
func refOrShort(id string, short, refs map[string]string) string {
	if r, ok := refs[id]; ok {
		return r
	}
	if s, ok := short[id]; ok {
		return s
	}
	return id
}
