package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/agents"
	"github.com/yjmrobert/wormhole/skills"
)

// skillInstall is one agent's skill status, as `skill status` and `skill
// install` report it.
type skillInstall struct {
	Agent     string `json:"agent"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	SkillPath string `json:"skill_path"`
	Installed bool   `json:"installed"`
	Current   bool   `json:"current"`
	// Changed is true when install wrote the file.
	Changed bool `json:"changed,omitempty"`
}

// lookPath resolves a binary on PATH, through the App's hook in tests.
func (a *App) lookPath(name string) (string, error) {
	if a.LookPath != nil {
		return a.LookPath(name)
	}
	return exec.LookPath(name)
}

// detectAgents lists the installed agents, or the one named.
func (a *App) detectAgents(ctx context.Context, name string) ([]agents.Detected, error) {
	found := agents.Detect(ctx, a.lookPath)
	if name == "" {
		return found, nil
	}
	if _, ok := agents.Lookup(name); !ok {
		return nil, fail(ExitUsage, "unknown agent %q; known: %s", name, strings.Join(agents.Names(), ", "))
	}
	for _, d := range found {
		if d.Name == name {
			return []agents.Detected{d}, nil
		}
	}
	return nil, nil
}

// skillStatus reports where the skill stands for each detected agent.
func (a *App) skillStatus(ctx context.Context, name string) ([]skillInstall, error) {
	found, err := a.detectAgents(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]skillInstall, 0, len(found))
	for _, d := range found {
		p := d.SkillPath()
		installed, current := agents.SkillInstalled(p, skills.Wormhole)
		out = append(out, skillInstall{Agent: d.Name, Path: d.Path, Version: d.Version, SkillPath: p, Installed: installed, Current: current})
	}
	return out, nil
}

// installSkills writes the embedded skill into every detected agent (or
// the named one, or the directory given) and says what it did. join runs
// it on its way through.
func (a *App) installSkills(ctx context.Context, name, dir string, w io.Writer) ([]skillInstall, error) {
	var targets []skillInstall
	if dir != "" {
		targets = append(targets, skillInstall{Agent: "custom", SkillPath: agents.SkillPathIn(dir)})
	} else {
		found, err := a.detectAgents(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, d := range found {
			targets = append(targets, skillInstall{Agent: d.Name, Path: d.Path, Version: d.Version, SkillPath: d.SkillPath()})
		}
	}
	if len(targets) == 0 {
		if name != "" {
			return nil, fail(ExitNotFound, "%s is not on PATH; install it, or install the skill somewhere by hand with: wormhole skill install --dir <skills directory>", name)
		}
		return nil, fail(ExitNotFound, "%v (looked for: %s); install the Copilot CLI, or pass the directory: wormhole skill install --dir <skills directory>", agents.ErrNoAgent, strings.Join(agents.Names(), ", "))
	}
	for i := range targets {
		changed, err := agents.InstallSkill(targets[i].SkillPath, skills.Wormhole)
		if err != nil {
			return targets, fmt.Errorf("install skill for %s: %w", targets[i].Agent, err)
		}
		targets[i].Installed, targets[i].Current, targets[i].Changed = true, true, changed
		if w != nil {
			if changed {
				fmt.Fprintf(w, "installed skill %s for %s at %s\n", skills.Version(), targets[i].Agent, targets[i].SkillPath)
			} else {
				fmt.Fprintf(w, "skill %s for %s is already current at %s\n", skills.Version(), targets[i].Agent, targets[i].SkillPath)
			}
		}
	}
	return targets, nil
}

// newSkillCommand installs the skill into agents or reports where it is.
func (a *App) newSkillCommand(c *common) *cobra.Command {
	cmd := command("skill", "Install the agent skill or show where it is")
	cmd.Long = "The skill teaches an agent the ^handle syntax and the inbox flow. install writes the version\nembedded in this binary into every detected agent's skills directory; join runs it for you."
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }

	var agent, dir string
	install := command("install", "Write the skill into every detected agent (or --agent, or --dir)")
	install.Flags().StringVar(&agent, "agent", "", "only this agent ("+strings.Join(agents.Names(), ", ")+")")
	install.Flags().StringVar(&dir, "dir", "", "a skills directory to install into instead of detecting agents")
	install.Args = noArgs("wormhole skill install [--agent <name>] [--dir <path>]")
	install.RunE = func(cmd *cobra.Command, _ []string) error {
		var w io.Writer
		if !c.json {
			w = a.Stdout
		}
		list, err := a.installSkills(cmd.Context(), agent, dir, w)
		if err != nil {
			return err
		}
		a.emit(c, map[string]any{"ok": true, "version": skills.Version(), "agents": list}, func(io.Writer) {})
		return nil
	}

	status := command("status", "Show detected agents and whether the skill is installed")
	status.Flags().StringVar(&agent, "agent", "", "only this agent")
	status.Args = noArgs("wormhole skill status [--agent <name>]")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		list, err := a.skillStatus(cmd.Context(), agent)
		if err != nil {
			return err
		}
		a.emit(c, map[string]any{"ok": true, "version": skills.Version(), "agents": list}, func(w io.Writer) {
			if len(list) == 0 {
				fmt.Fprintf(w, "no supported agent on PATH (looked for: %s); install one, or: wormhole skill install --dir <skills directory>\n", strings.Join(agents.Names(), ", "))
				return
			}
			rows := make([][]string, len(list))
			for i, s := range list {
				state := "missing"
				switch {
				case s.Current:
					state = "current"
				case s.Installed:
					state = "outdated"
				}
				rows[i] = []string{s.Agent, s.Version, state, s.SkillPath}
			}
			table(w, []string{"AGENT", "VERSION", "SKILL", "PATH"}, rows)
			for _, s := range list {
				if !s.Current {
					fmt.Fprintf(w, "install it with: wormhole skill install --agent %s\n", s.Agent)
					break
				}
			}
		})
		return nil
	}
	cmd.AddCommand(install, status)
	return cmd
}
