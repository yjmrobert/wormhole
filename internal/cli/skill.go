package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/agents"
	"github.com/yjmrobert/wormhole/skills"
)

// noAgentFix is the fix named when no agent is detected.
const noAgentFix = "install the Copilot CLI, or pass --dir <skills directory>"

// skillInstall is one skill written (or found current) for one agent.
type skillInstall struct {
	Agent   string `json:"agent,omitempty"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Changed bool   `json:"changed"`
}

// skillStatus is one detected agent and its copy of the skill.
type skillStatus struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Version   string `json:"version"`
	SkillPath string `json:"skill_path"`
	Installed bool   `json:"installed"`
	Current   bool   `json:"current"`
}

// lookPath is the App's LookPath, defaulting to exec.LookPath.
func (a *App) lookPath() func(string) (string, error) {
	if a.LookPath != nil {
		return a.LookPath
	}
	return exec.LookPath
}

// newSkillCommand installs and reports the agent skill.
func (a *App) newSkillCommand(c *common) *cobra.Command {
	cmd := command("skill", "Install the wormhole skill into your coding agents")
	cmd.Args = noArgs("wormhole skill install|status")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		return fail(ExitUsage, "usage: wormhole skill install [--agent <name>] [--dir <path>] | wormhole skill status")
	}

	var agentName, dir string
	install := command("install", "Install the skill into every detected agent")
	install.Flags().StringVar(&agentName, "agent", "", "only this agent ("+strings.Join(agents.Names(), ", ")+")")
	install.Flags().StringVar(&dir, "dir", "", "install into this skills directory, detected or not (writes <dir>/wormhole/SKILL.md)")
	install.Args = noArgs("wormhole skill install [--agent <name>] [--dir <path>]")
	install.RunE = func(cmd *cobra.Command, _ []string) error {
		var human strings.Builder
		res, err := a.installSkillsFor(cmd.Context(), agentName, dir, &human)
		if err != nil {
			return err
		}
		a.emit(c, map[string]any{"ok": true, "installed": res}, func(w io.Writer) { fmt.Fprint(w, human.String()) })
		return nil
	}

	status := command("status", "Show detected agents and whether the skill is installed and current")
	status.Args = noArgs("wormhole skill status")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		list := a.skillStatuses(cmd.Context())
		a.emit(c, map[string]any{"ok": true, "agents": list}, func(w io.Writer) {
			if len(list) == 0 {
				fmt.Fprintf(w, "no agent detected; %s\n", noAgentFix)
				return
			}
			rows := make([][]string, 0, len(list))
			stale := false
			for _, s := range list {
				state := "not installed"
				switch {
				case s.Current:
					state = "current (" + skills.Version() + ")"
				case s.Installed:
					state = "out of date"
				}
				if !s.Current {
					stale = true
				}
				version := s.Version
				if version == "" {
					version = "-"
				}
				rows = append(rows, []string{s.Name, version, s.Path, state, s.SkillPath})
			}
			table(w, []string{"AGENT", "VERSION", "BINARY", "SKILL", "SKILL PATH"}, rows)
			if stale {
				fmt.Fprintln(w, "\ninstall or update it with: wormhole skill install")
			}
		})
		return nil
	}
	cmd.AddCommand(install, status)
	return cmd
}

// installSkills installs the embedded skill into every detected agent and
// writes one line per agent to w (nil: silent). `wormhole join` calls it.
// It fails with ExitNotFound when no agent is detected.
func (a *App) installSkills(ctx context.Context, _ *common, w io.Writer) ([]skillInstall, error) {
	return a.installSkillsFor(ctx, "", "", w)
}

// installSkillsFor installs into the named agent (all detected when
// empty), or into dir when it is set.
func (a *App) installSkillsFor(ctx context.Context, agentName, dir string, w io.Writer) ([]skillInstall, error) {
	if w == nil {
		w = io.Discard
	}
	version := skills.Version()
	if agentName != "" {
		if _, ok := agents.Lookup(agentName); !ok {
			return nil, fail(ExitUsage, "unknown agent %q; want one of: %s (or pass --dir <skills directory>)", agentName, strings.Join(agents.Names(), ", "))
		}
	}
	if dir != "" {
		path, changed, err := agents.InstallSkillAt(filepath.Join(dir, agents.SkillName, "SKILL.md"), skills.Wormhole)
		if err != nil {
			return nil, fail(ExitError, "install skill: %v; check that %s is writable", err, dir)
		}
		res := skillInstall{Agent: agentName, Path: path, Version: version, Changed: changed}
		printSkillInstall(w, res)
		return []skillInstall{res}, nil
	}
	var out []skillInstall
	for _, d := range agents.Detect(ctx, a.lookPath()) {
		if agentName != "" && d.Name != agentName {
			continue
		}
		path, changed, err := agents.InstallSkill(d.Agent, skills.Wormhole)
		if err != nil {
			return out, fail(ExitError, "install skill for %s: %v; check that %s is writable, or pass --dir <skills directory>", d.Name, err, d.SkillDir())
		}
		res := skillInstall{Agent: d.Name, Path: path, Version: version, Changed: changed}
		printSkillInstall(w, res)
		out = append(out, res)
	}
	if len(out) == 0 {
		if agentName != "" {
			return nil, fail(ExitNotFound, "%s is not on PATH; install it, or pass --dir <skills directory>", agentName)
		}
		return nil, fail(ExitNotFound, "no agent detected; %s", noAgentFix)
	}
	return out, nil
}

// printSkillInstall writes the one line an install reports.
func printSkillInstall(w io.Writer, r skillInstall) {
	who := ""
	if r.Agent != "" {
		who = " for " + r.Agent
	}
	if r.Changed {
		fmt.Fprintf(w, "installed skill %s%s at %s\n", r.Version, who, r.Path)
	} else {
		fmt.Fprintf(w, "skill %s%s already current at %s\n", r.Version, who, r.Path)
	}
}

// skillStatuses reports every detected agent's copy of the skill.
func (a *App) skillStatuses(ctx context.Context) []skillStatus {
	list := []skillStatus{}
	for _, d := range agents.Detect(ctx, a.lookPath()) {
		installed, current := agents.SkillInstalled(d.Agent, skills.Wormhole)
		list = append(list, skillStatus{Name: d.Name, Path: d.Path, Version: d.Version, SkillPath: d.SkillPath(), Installed: installed, Current: current})
	}
	return list
}
