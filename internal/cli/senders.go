package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
)

// senderRow is one sender as `wormhole senders --json` prints it.
type senderRow struct {
	Handle string `json:"handle"`
	Mode   string `json:"mode"`
}

// noSendersMessage is what an empty allowlist means and how to fill it.
const noSendersMessage = "nobody may ask you yet; add someone with: wormhole senders add <handle>"

// newSendersCommand lists and edits who may ask this user.
func (a *App) newSendersCommand(c *common) *cobra.Command {
	cmd := command("senders", "List or edit who may ask you")
	cmd.Long = "List who may ask you (the senders: list in your config), or edit it with add and remove. Comments in the file survive the edit. The daemon reads the list when it starts."
	cmd.Args = noArgs("wormhole senders [add|remove <handle> [--auto]]")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		a.printSenders(c, cfg, "")
		return nil
	}

	var auto bool
	add := command("add <handle>", "Let a handle ask you (inbox mode unless --auto)")
	add.Flags().BoolVar(&auto, "auto", false, "auto mode: run their asks headless under copilot.safe_tools")
	add.Args = exactArgs(1, "wormhole senders add <handle> [--auto]")
	add.RunE = func(_ *cobra.Command, args []string) error {
		handle, err := senderHandle(args[0])
		if err != nil {
			return err
		}
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		if handle == cfg.Handle {
			return fail(ExitRefused, "%s is your own handle; you cannot add yourself to senders:, add someone else: wormhole senders add <handle>", handle)
		}
		mode := config.ModeInbox
		if auto {
			if len(cfg.Copilot.SafeTools) == 0 {
				return fail(ExitRefused, "auto mode needs copilot.safe_tools; set copilot.safe_tools in %s first, or add ^%s in inbox mode: wormhole senders add %s", cfg.Path, handle, handle)
			}
			mode = config.ModeAuto
		}
		if err := config.AddSender(cfg.Path, handle, mode); err != nil {
			return wrap(ExitError, err)
		}
		cfg, err = a.loadConfig(c)
		if err != nil {
			return err
		}
		a.printSenders(c, cfg, fmt.Sprintf("^%s may ask you (%s mode)", handle, mode))
		return nil
	}

	remove := command("remove <handle>", "Stop a handle from asking you")
	remove.Args = exactArgs(1, "wormhole senders remove <handle>")
	remove.RunE = func(_ *cobra.Command, args []string) error {
		handle, err := senderHandle(args[0])
		if err != nil {
			return err
		}
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		if err := config.RemoveSender(cfg.Path, handle); err != nil {
			if errors.Is(err, config.ErrNoSender) {
				return fail(ExitNotFound, "^%s is not under senders: in %s; see who is: wormhole senders", handle, cfg.Path)
			}
			return wrap(ExitError, err)
		}
		cfg, err = a.loadConfig(c)
		if err != nil {
			return err
		}
		a.printSenders(c, cfg, fmt.Sprintf("^%s may no longer ask you", handle))
		return nil
	}

	cmd.AddCommand(add, remove)
	return cmd
}

// senderHandle accepts bob or ^bob.
func senderHandle(arg string) (string, error) {
	handle := strings.TrimPrefix(arg, "^")
	if !envelope.ValidHandle(handle) {
		return "", fail(ExitNotFound, invalidHandleFormat+"; list handles with: wormhole users", arg)
	}
	return handle, nil
}

// printSenders emits the allowlist, after done when an edit happened.
func (a *App) printSenders(c *common, cfg *config.Config, done string) {
	rows := make([]senderRow, len(cfg.Senders))
	for i, s := range cfg.Senders {
		rows[i] = senderRow{Handle: s.Handle, Mode: s.Mode}
	}
	a.emit(c, map[string]any{"ok": true, "senders": rows}, func(w io.Writer) {
		if done != "" {
			fmt.Fprintf(w, "%s; restart the daemon to apply: systemctl --user restart wormhole, or rerun: wormhole daemon\n\n", done)
		}
		if len(rows) == 0 {
			fmt.Fprintln(w, noSendersMessage)
			return
		}
		out := make([][]string, len(rows))
		for i, r := range rows {
			out[i] = []string{"^" + r.Handle, r.Mode}
		}
		table(w, []string{"SENDER", "MODE"}, out)
	})
}
