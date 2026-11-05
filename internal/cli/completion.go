package cli

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/threads"
)

const completionTimeout = 2 * time.Second

// completeThreads completes thread references (^bob, ^bob/2), short ids
// and full ids with their titles, from the local store.
func (a *App) completeThreads(c *common, stagedOnly bool) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 0 || a.OpenStore == nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
		defer cancel()
		store, err := a.OpenStore()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		defer store.Close()
		// Only threads that still expect traffic: a closed or declined
		// one would be refused by the command anyway.
		open, err := store.List(ctx, threads.Filter{Open: true})
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		refs := threadRefs(open)
		var values []string
		for _, thread := range open {
			if stagedOnly && thread.StagedKickoff == "" {
				continue
			}
			if ref, ok := refs[thread.ID]; ok && strings.HasPrefix(ref, toComplete) {
				values = append(values, ref+"\t"+thread.Title)
			}
			if strings.HasPrefix(thread.ID, strings.ToUpper(toComplete)) && (toComplete == "" || !strings.HasPrefix(toComplete, "^")) {
				values = append(values, thread.ID+"\t"+thread.Title)
			}
		}
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

// registerCompletions attaches dynamic argument and flag completion handlers.
func (a *App) registerCompletions(root *cobra.Command, c *common) {
	threadsByName := map[string]bool{"open": true, "reply": false, "snooze": true, "decline": true}
	for name, staged := range threadsByName {
		cmd, _, err := root.Find([]string{name})
		if err == nil {
			cmd.ValidArgsFunction = a.completeThreads(c, staged)
		}
	}
	for _, name := range []string{"ask", "send"} {
		cmd, _, err := root.Find([]string{name})
		if err == nil {
			cmd.ValidArgsFunction = a.completeHandles(c)
		}
	}
	if cmd, _, err := root.Find([]string{"send"}); err == nil {
		_ = cmd.RegisterFlagCompletionFunc("thread", a.completeThreads(c, false))
	}
	if cmd, _, err := root.Find([]string{"threads"}); err == nil {
		_ = cmd.RegisterFlagCompletionFunc("peer", a.completeHandles(c))
	}
	for _, path := range [][]string{{"senders", "add"}, {"senders", "remove"}, {"serve", "invite"}, {"serve", "revoke"}} {
		if cmd, _, err := root.Find(path); err == nil && cmd.Name() == path[1] {
			cmd.ValidArgsFunction = a.completeHandles(c)
		}
	}
}

// completeHandles completes handles from the registry when the bus answers
// in time, else from the peers in the local thread map.
func (a *App) completeHandles(c *common) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
		defer cancel()
		values := a.registryHandles(ctx, c, toComplete)
		if values == nil {
			values = a.storedPeers(ctx, toComplete)
		}
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

// registryHandles lists handles from the bus registry, or nil when the
// config or the bus is unavailable.
func (a *App) registryHandles(ctx context.Context, c *common, prefix string) []string {
	if a.NewBus == nil {
		return nil
	}
	cfg, err := a.loadConfig(c)
	if err != nil {
		return nil
	}
	b, err := a.NewBus(ctx, cfg)
	if err != nil {
		return nil
	}
	defer b.Close()
	list, err := b.Registry().List(ctx)
	if err != nil {
		return nil
	}
	values := []string{}
	for _, presence := range list {
		if strings.HasPrefix(presence.Handle, prefix) {
			values = append(values, presence.Handle+"\t"+presence.Host)
		}
	}
	return values
}

// storedPeers lists the distinct peers in the thread map.
func (a *App) storedPeers(ctx context.Context, prefix string) []string {
	if a.OpenStore == nil {
		return nil
	}
	store, err := a.OpenStore()
	if err != nil {
		return nil
	}
	defer store.Close()
	list, err := store.List(ctx, threads.Filter{})
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var values []string
	for _, t := range list {
		if seen[t.Peer] || !strings.HasPrefix(t.Peer, prefix) {
			continue
		}
		seen[t.Peer] = true
		values = append(values, t.Peer)
	}
	sort.Strings(values)
	return values
}
