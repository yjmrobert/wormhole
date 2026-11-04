package cli

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/threads"
)

const completionTimeout = 2 * time.Second

// snoozeLengths are the durations offered after `wormhole snooze <thread>`.
var snoozeLengths = []string{"45m\t45 minutes", "2h\t2 hours", "1d\ttomorrow", "off\tback in the inbox now"}

// completeThreadIDs completes <thread> arguments: ^peer references and full
// ids of open threads (staged ones only when stagedOnly), with titles.
func (a *App) completeThreadIDs(c *common, stagedOnly bool) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return a.threadCandidates(cmd.Context(), "", stagedOnly, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeThreadFlag completes `send <user> --thread`: the open threads
// with the user already typed, or every open thread.
func (a *App) completeThreadFlag() cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		peer := ""
		if len(args) > 0 {
			peer = strings.TrimPrefix(args[0], "^")
		}
		return a.threadCandidates(cmd.Context(), peer, false, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeSnooze completes the thread, then the duration.
func (a *App) completeSnooze(c *common) cobra.CompletionFunc {
	threadsFn := a.completeThreadIDs(c, true)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 1 {
			var out []string
			for _, v := range snoozeLengths {
				if strings.HasPrefix(v, toComplete) {
					out = append(out, v)
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		}
		return threadsFn(cmd, args, toComplete)
	}
}

// threadCandidates lists the references and ids of open threads that start
// with toComplete. It fails silently: completion must never stall a shell.
func (a *App) threadCandidates(ctx context.Context, peer string, stagedOnly bool, toComplete string) []string {
	if a.OpenStore == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()
	store, err := a.OpenStore()
	if err != nil {
		return nil
	}
	defer store.Close()
	list, err := store.List(ctx, threads.Filter{Open: true})
	if err != nil {
		return nil
	}
	refs := threadRefs(list)
	upper := strings.ToUpper(toComplete)
	var values []string
	for _, t := range list {
		if (stagedOnly && t.StagedKickoff == "") || (peer != "" && t.Peer != peer) {
			continue
		}
		if r := refs[t.ID]; strings.HasPrefix(r, toComplete) {
			values = append(values, r+"\t"+t.Title)
		}
		if strings.HasPrefix(t.ID, upper) {
			values = append(values, t.ID+"\t"+t.Title)
		}
	}
	return values
}

// registerCompletions attaches dynamic argument and flag completion handlers.
func (a *App) registerCompletions(root *cobra.Command, c *common) {
	threadsByName := map[string]bool{"open": true, "reply": false, "decline": true}
	for name, staged := range threadsByName {
		cmd, _, err := root.Find([]string{name})
		if err == nil {
			cmd.ValidArgsFunction = a.completeThreadIDs(c, staged)
		}
	}
	if cmd, _, err := root.Find([]string{"snooze"}); err == nil {
		cmd.ValidArgsFunction = a.completeSnooze(c)
	}
	for _, name := range []string{"ask", "send"} {
		cmd, _, err := root.Find([]string{name})
		if err == nil {
			cmd.ValidArgsFunction = a.completeHandles(c)
		}
	}
	if cmd, _, err := root.Find([]string{"send"}); err == nil {
		_ = cmd.RegisterFlagCompletionFunc("thread", a.completeThreadFlag())
	}
	if cmd, _, err := root.Find([]string{"threads"}); err == nil {
		_ = cmd.RegisterFlagCompletionFunc("peer", a.completeHandles(c))
	}
}

// completeHandles completes handles from the registry, or, when the config
// or the bus is unavailable, from the peers in the local thread map.
func (a *App) completeHandles(c *common) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
		defer cancel()
		toComplete = strings.TrimPrefix(toComplete, "^")
		if values, ok := a.registryHandles(ctx, c, toComplete); ok {
			return values, cobra.ShellCompDirectiveNoFileComp
		}
		return a.threadPeers(ctx, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// registryHandles lists registered handles with their hosts; ok is false
// when the registry could not be read.
func (a *App) registryHandles(ctx context.Context, c *common, prefix string) ([]string, bool) {
	if a.NewBus == nil {
		return nil, false
	}
	cfg, err := a.loadConfig(c)
	if err != nil {
		return nil, false
	}
	b, err := a.NewBus(ctx, cfg)
	if err != nil {
		return nil, false
	}
	defer b.Close()
	list, err := b.Registry().List(ctx)
	if err != nil {
		return nil, false
	}
	var values []string
	for _, presence := range list {
		if strings.HasPrefix(presence.Handle, prefix) {
			values = append(values, presence.Handle+"\t"+presence.Host)
		}
	}
	return values, true
}

// threadPeers lists the distinct peers of local threads.
func (a *App) threadPeers(ctx context.Context, prefix string) []string {
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
	for _, t := range list {
		if strings.HasPrefix(t.Peer, prefix) {
			seen[t.Peer] = true
		}
	}
	peers := sortedKeys(seen)
	values := make([]string, 0, len(peers))
	for _, p := range peers {
		values = append(values, p+"\tfrom your threads")
	}
	if len(values) == 0 {
		return nil
	}
	return values
}
