package cli

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/threads"
)

const completionTimeout = 2 * time.Second

func (a *App) completeThreadIDs(c *common, stagedOnly bool) cobra.CompletionFunc {
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
		list, err := store.List(ctx, threads.Filter{Open: true, Staged: stagedOnly})
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var values []string
		for _, thread := range list {
			if strings.HasPrefix(thread.ID, toComplete) {
				values = append(values, thread.ID+"\t"+thread.Title)
			}
		}
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

func (a *App) registerCompletions(root *cobra.Command, c *common) {
	threadsByName := map[string]bool{"open": true, "reply": false, "snooze": true, "decline": true}
	for name, staged := range threadsByName {
		cmd, _, err := root.Find([]string{name})
		if err == nil {
			cmd.ValidArgsFunction = a.completeThreadIDs(c, staged)
		}
	}
	for _, name := range []string{"ask", "send"} {
		cmd, _, err := root.Find([]string{name})
		if err == nil {
			cmd.ValidArgsFunction = a.completeHandles(c)
		}
	}
	if cmd, _, err := root.Find([]string{"send"}); err == nil {
		_ = cmd.RegisterFlagCompletionFunc("thread", a.completeThreadIDs(c, false))
	}
	if cmd, _, err := root.Find([]string{"threads"}); err == nil {
		_ = cmd.RegisterFlagCompletionFunc("peer", a.completeHandles(c))
	}
}

func (a *App) completeHandles(c *common) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 0 || a.NewBus == nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
		defer cancel()
		cfg, err := a.loadConfig(c)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		b, err := a.NewBus(ctx, cfg)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		defer b.Close()
		list, err := b.Registry().List(ctx)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var values []string
		for _, presence := range list {
			if strings.HasPrefix(presence.Handle, toComplete) {
				values = append(values, presence.Handle+"\t"+presence.Host)
			}
		}
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}
