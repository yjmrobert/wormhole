package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// The most common config edit, who may ask you, is a command. It edits
// the senders: list in config.yaml in place, comments included.

type senderRow struct {
	Handle string `json:"handle"`
	Mode   string `json:"mode"`
}

// newSendersCommand lists and edits the sender allowlist.
func (a *App) newSendersCommand(c *common) *cobra.Command {
	cmd := command("senders", "Who may ask you: list, add or remove handles")
	cmd.Long = "Who may open a wormhole to you. A handle in inbox mode waits in your inbox until you read it;\nauto mode runs headless under copilot.safe_tools and needs that set.\nEdits config.yaml in place and keep its comments."
	cmd.Args = noArgs("wormhole senders [add|remove <handle>]")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		return a.printSenders(c, cfg)
	}

	var auto bool
	add := command("add <handle>", "Let a handle ask you (inbox mode unless --auto)")
	add.Flags().BoolVar(&auto, "auto", false, "auto mode: their asks run headless under copilot.safe_tools")
	add.Args = exactArgs(1, "wormhole senders add <handle> [--auto]")
	add.RunE = func(cmd *cobra.Command, args []string) error {
		handle := args[0]
		if !envelope.ValidHandle(handle) {
			return fail(ExitNotFound, invalidHandleFormat, handle)
		}
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		if handle == cfg.Handle {
			return fail(ExitUsage, "^%s is you; senders are the other people. add one with: wormhole senders add <handle>", handle)
		}
		mode := config.ModeInbox
		if auto {
			if len(cfg.Copilot.SafeTools) == 0 {
				return fail(ExitRefused, "auto mode needs copilot.safe_tools in %s (empty today); set the profile from docs/design.md, M0 results, or add ^%s in inbox mode: wormhole senders add %s", cfg.Path, handle, handle)
			}
			mode = config.ModeAuto
		}
		if err := config.AddSender(cfg.Path, handle, mode); err != nil {
			if errors.Is(err, config.ErrExists) {
				return fail(ExitRefused, "%v; to change their mode remove them first: wormhole senders remove %s", err, handle)
			}
			return wrap(ExitError, err)
		}
		cfg, err = a.loadConfig(c)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stderr, "^%s may now ask you (%s mode); restart the daemon to apply: systemctl --user restart wormhole\n", handle, mode)
		return a.printSenders(c, cfg)
	}

	remove := command("remove <handle>", "Stop a handle from asking you")
	remove.Args = exactArgs(1, "wormhole senders remove <handle>")
	remove.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		if err := config.RemoveSender(cfg.Path, args[0]); err != nil {
			if errors.Is(err, config.ErrNotListed) {
				return fail(ExitNotFound, "%v; see who is: wormhole senders", err)
			}
			return wrap(ExitError, err)
		}
		cfg, err = a.loadConfig(c)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stderr, "^%s can no longer ask you; restart the daemon to apply: systemctl --user restart wormhole\n", args[0])
		return a.printSenders(c, cfg)
	}
	cmd.AddCommand(add, remove)
	return cmd
}

// printSenders emits the current list.
func (a *App) printSenders(c *common, cfg *config.Config) error {
	rows := make([]senderRow, 0, len(cfg.Senders))
	for _, s := range cfg.Senders {
		rows = append(rows, senderRow{Handle: s.Handle, Mode: s.Mode})
	}
	a.emit(c, map[string]any{"ok": true, "senders": rows, "config": cfg.Path}, func(w io.Writer) {
		if len(rows) == 0 {
			fmt.Fprintf(w, "nobody may ask you yet; add someone with: wormhole senders add <handle>\n")
			return
		}
		out := make([][]string, len(rows))
		for i, r := range rows {
			out[i] = []string{"^" + r.Handle, r.Mode}
		}
		table(w, []string{"HANDLE", "MODE"}, out)
	})
	return nil
}
