package cli

import (
	"fmt"
	"io"
	"strings"

	mango "github.com/muesli/mango-cobra"
	"github.com/muesli/roff"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func (a *App) newVersionCommand(c *common) *cobra.Command {
	cmd := command("version", "Show version and build metadata")
	cmd.Args = noArgs("wormhole version")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		a.emit(c, map[string]string{
			"version":    a.Version,
			"commit":     a.commit(),
			"build_date": a.buildDate(),
		}, a.printVersion)
		return nil
	}
	return cmd
}

func (a *App) newCompletionCommand() *cobra.Command {
	cmd := command("completion bash|zsh|fish", "Generate a shell completion script")
	cmd.ValidArgs = []string{"bash", "zsh", "fish"}
	cmd.Args = func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return fail(ExitUsage, "usage: wormhole completion bash|zsh|fish")
		}
		switch args[0] {
		case "bash", "zsh", "fish":
			return nil
		default:
			return fail(ExitUsage, "unsupported shell %q; use bash, zsh, or fish", args[0])
		}
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
		case "zsh":
			return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
		default:
			return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
		}
	}
	return cmd
}

type commandDocument struct {
	SchemaVersion int         `json:"schemaVersion"`
	Command       commandInfo `json:"command"`
}

type commandInfo struct {
	Name        string        `json:"name"`
	Path        string        `json:"path"`
	Usage       string        `json:"usage"`
	Description string        `json:"description"`
	Flags       []flagInfo    `json:"flags,omitempty"`
	Commands    []commandInfo `json:"commands,omitempty"`
}

type flagInfo struct {
	Name       string `json:"name"`
	Shorthand  string `json:"shorthand,omitempty"`
	Type       string `json:"type"`
	Default    string `json:"default"`
	Usage      string `json:"usage"`
	Required   bool   `json:"required"`
	Persistent bool   `json:"persistent"`
}

func (a *App) newCommandsCommand(c *common) *cobra.Command {
	var withFlags bool
	cmd := command("commands [path...]", "Show the command tree and its flags")
	cmd.Flags().BoolVar(&withFlags, "flags", false, "include command flag metadata")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		selected := cmd.Root()
		for _, name := range args {
			var next *cobra.Command
			for _, child := range selected.Commands() {
				if child.Name() == name && !child.Hidden {
					next = child
					break
				}
			}
			if next == nil {
				return fail(ExitNotFound, "command path %q not found", strings.Join(args, " "))
			}
			selected = next
		}
		info := describeCommand(selected, selected == cmd.Root(), withFlags)
		a.emit(c, commandDocument{SchemaVersion: 1, Command: info}, func(w io.Writer) {
			printCommandInfo(w, info, withFlags, 0)
		})
		return nil
	}
	return cmd
}

func describeCommand(cmd *cobra.Command, root, withFlags bool) commandInfo {
	info := commandInfo{
		Name:        cmd.Name(),
		Path:        cmd.CommandPath(),
		Usage:       cmd.UseLine(),
		Description: cmd.Short,
	}
	if withFlags {
		cmd.InitDefaultHelpFlag()
		if root {
			cmd.InitDefaultVersionFlag()
		}
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
			info.Flags = append(info.Flags, flagInfo{
				Name:       f.Name,
				Shorthand:  f.Shorthand,
				Type:       f.Value.Type(),
				Default:    f.DefValue,
				Usage:      f.Usage,
				Required:   required,
				Persistent: cmd.PersistentFlags().Lookup(f.Name) != nil,
			})
		})
		if info.Flags == nil {
			info.Flags = []flagInfo{}
		}
	}
	for _, child := range cmd.Commands() {
		if child.Hidden || child.Name() == "__complete" {
			continue
		}
		info.Commands = append(info.Commands, describeCommand(child, false, withFlags))
	}
	return info
}

func printCommandInfo(w io.Writer, info commandInfo, withFlags bool, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(w, "%s%s\t%s\n", indent, info.Path, info.Description)
	if withFlags {
		for _, f := range info.Flags {
			name := "--" + f.Name
			if f.Shorthand != "" {
				name = "-" + f.Shorthand + ", " + name
			}
			fmt.Fprintf(w, "%s  %s\t%s\n", indent, name, f.Usage)
		}
	}
	for _, child := range info.Commands {
		printCommandInfo(w, child, withFlags, depth+1)
	}
}

func (a *App) newManCommand() *cobra.Command {
	cmd := command("man", "Generate a man page")
	cmd.Hidden = true
	cmd.Args = noArgs("wormhole man")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		page, err := mango.NewManPage(1, cmd.Root())
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), page.Build(roff.NewDocument()))
		return err
	}
	return cmd
}
