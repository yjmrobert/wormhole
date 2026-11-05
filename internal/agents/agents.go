// Package agents knows which coding agents are installed on this machine
// and where each one reads its skills, so `wormhole skill install` and
// `wormhole join` put the skill where the agent will find it. It is a
// table with one row per agent; M4's drivers add rows, and a wrong path
// is a one-line fix.
package agents

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SkillName is the directory the skill is installed under.
const SkillName = "wormhole"

// Agent is one row of the table.
type Agent struct {
	// Name is what --agent takes: copilot.
	Name string
	// Binary is looked up on PATH to detect the agent.
	Binary string
	// SkillDir is the user-level skills directory.
	SkillDir func() string
	// Version is best effort: the first line of `<binary> --version`.
	Version func(ctx context.Context, path string) string
}

// Detected is an installed agent.
type Detected struct {
	Agent
	// Path is where the binary was found.
	Path string
	// Version is the reported version, "" when unknown.
	Version string
}

// Copilot is the GitHub Copilot CLI. Its user-level skills live under
// ~/.copilot/skills (COPILOT_HOME overrides the directory).
var Copilot = Agent{
	Name:   "copilot",
	Binary: "copilot",
	SkillDir: func() string {
		if h := os.Getenv("COPILOT_HOME"); h != "" {
			return filepath.Join(h, "skills")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".copilot", "skills")
	},
	Version: versionFlag,
}

// Table lists every agent Wormhole can install its skill into.
var Table = []Agent{Copilot}

// Lookup returns the agent with that name.
func Lookup(name string) (Agent, bool) {
	for _, a := range Table {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// Names lists the agent names, for help text and errors.
func Names() []string {
	out := make([]string, len(Table))
	for i, a := range Table {
		out[i] = a.Name
	}
	return out
}

// Detect finds the agents whose binary lookPath resolves (exec.LookPath
// when nil).
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

// versionFlag runs `<path> --version` with a short timeout and returns its
// first line.
func versionFlag(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := bytes.Cut(bytes.TrimSpace(out), []byte("\n"))
	return strings.TrimSpace(string(line))
}

// SkillPath is where the skill file goes for this agent.
func (a Agent) SkillPath() string {
	return SkillPathIn(a.SkillDir())
}

// SkillPathIn is the skill file under an arbitrary skills directory.
func SkillPathIn(dir string) string {
	return filepath.Join(dir, SkillName, "SKILL.md")
}

// SkillInstalled reports whether the skill exists at path and whether it
// equals content.
func SkillInstalled(path string, content []byte) (installed, current bool) {
	have, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	return true, bytes.Equal(have, content)
}

// InstallSkill writes content to path (mode 644, directory 755) and
// reports whether the file changed.
func InstallSkill(path string, content []byte) (changed bool, err error) {
	if _, current := SkillInstalled(path, content); current {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".SKILL.md.*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}

// ErrNoAgent is returned when nothing was detected.
var ErrNoAgent = errors.New("no supported agent found on PATH")
