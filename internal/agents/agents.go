// Package agents is the table of coding agents Wormhole knows how to set up
// on this machine: where each one's binary is called, where it reads
// user-level skills from and how to ask its version. Detection is
// best-effort and never fails a command; a row that is wrong is a one-line
// fix here.
package agents

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SkillName is the directory the Wormhole skill is installed under.
const SkillName = "wormhole"

// Agent is one row of the detection table.
type Agent struct {
	// Name is what --agent takes ("copilot").
	Name string
	// Binary is the executable looked up on PATH.
	Binary string
	// SkillDir is where the agent reads user-level skills from.
	SkillDir func() string
	// Version asks the binary at path for its version, best effort: ""
	// when it cannot tell.
	Version func(ctx context.Context, path string) string
}

// Detected is an agent found on this machine.
type Detected struct {
	Agent
	Path    string
	Version string
}

// copilot is the GitHub Copilot CLI. It reads user-level skills from
// ~/.copilot/skills, or $COPILOT_HOME/skills when COPILOT_HOME moves its
// state directory.
var copilot = Agent{
	Name:   "copilot",
	Binary: "copilot",
	SkillDir: func() string {
		if h := os.Getenv("COPILOT_HOME"); h != "" {
			return filepath.Join(h, "skills")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".copilot", "skills")
	},
	Version: firstLineOf("--version"),
}

// Table lists every agent Wormhole can detect. M4's drivers add rows.
var Table = []Agent{copilot}

// Lookup returns the table row named name.
func Lookup(name string) (Agent, bool) {
	for _, a := range Table {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// Names lists the table's agent names.
func Names() []string {
	out := make([]string, len(Table))
	for i, a := range Table {
		out[i] = a.Name
	}
	return out
}

// Detect returns the agents whose binary lookPath finds, in table order.
func Detect(ctx context.Context, lookPath func(string) (string, error)) []Detected {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	var out []Detected
	for _, a := range Table {
		p, err := lookPath(a.Binary)
		if err != nil {
			continue
		}
		d := Detected{Agent: a, Path: p}
		if a.Version != nil {
			d.Version = a.Version(ctx, p)
		}
		out = append(out, d)
	}
	return out
}

// SkillPath is where the Wormhole skill lives for this agent.
func (a Agent) SkillPath() string {
	return filepath.Join(a.SkillDir(), SkillName, "SKILL.md")
}

// InstallSkill writes content to a's skill path (file 0644, directories
// 0755) and reports whether the file changed.
func InstallSkill(a Agent, content []byte) (path string, changed bool, err error) {
	return InstallSkillAt(a.SkillPath(), content)
}

// InstallSkillAt writes content to path, creating its directory, and
// reports whether the file changed. --dir uses it directly.
func InstallSkillAt(path string, content []byte) (string, bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return path, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".SKILL.md.*")
	if err != nil {
		return path, false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return path, false, err
	}
	if err := tmp.Close(); err != nil {
		return path, false, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return path, false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// SkillInstalled reports whether a's skill file exists and whether it
// equals content.
func SkillInstalled(a Agent, content []byte) (installed, current bool) {
	return SkillInstalledAt(a.SkillPath(), content)
}

// SkillInstalledAt is SkillInstalled for an explicit path.
func SkillInstalledAt(path string, content []byte) (installed, current bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	return true, bytes.Equal(b, content)
}

// firstLineOf runs `<binary> <args>` with a short timeout and returns the
// first line it printed.
func firstLineOf(args ...string) func(ctx context.Context, path string) string {
	return func(ctx context.Context, path string) string {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, path, args...).Output()
		if err != nil {
			return ""
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return strings.TrimSpace(line)
	}
}
