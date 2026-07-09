// Package config loads the optional ~/.config/proj/config.toml.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	BaseDir string                 `toml:"base_dir"`
	Claude  ClaudeConfig           `toml:"claude"`
	Agents  map[string]AgentConfig `toml:"agents"`
	Daemon  DaemonConfig           `toml:"daemon"`
	List    ListConfig             `toml:"list"`
}

type ClaudeConfig struct {
	Command    string `toml:"command"`
	ResumeFlag string `toml:"resume_flag"`
	Home       string `toml:"home"` // Claude home override; default ~/.claude, or the Windows one when running under WSL
}

// AgentConfig is the launch recipe for a non-Claude coding agent, configured
// under [agents.<name>]. Claude keeps its own [claude] section (it carries the
// extra home override) and is exposed through the same AgentSpec resolution.
type AgentConfig struct {
	Command       string `toml:"command"`
	ResumeCommand string `toml:"resume_command"` // full command used instead of command when the project has prior history
}

// AgentSpec is a resolved launch recipe: the command templates a session is
// started with. Both commands support the {name}, {dir}, {host} and {rc}
// placeholders.
type AgentSpec struct {
	Name          string
	Command       string
	ResumeCommand string // empty: always launch fresh
}

// DefaultAgent is the agent used by projects with no agent set.
const DefaultAgent = "claude"

// defaultAgents holds the built-in recipes for the supported non-Claude
// agents. A [agents.<name>] entry in config.toml overrides the whole recipe
// for that name.
var defaultAgents = map[string]AgentConfig{
	"codex": {
		Command:       "codex --dangerously-bypass-approvals-and-sandbox",
		ResumeCommand: "codex resume --last --dangerously-bypass-approvals-and-sandbox",
	},
	"agy": {
		Command: "agy",
	},
}

// Agent resolves an agent name to its launch spec. "" means claude. Unknown
// names error with a hint at where to define them.
func (c Config) Agent(name string) (AgentSpec, error) {
	if name == "" || name == DefaultAgent {
		spec := AgentSpec{Name: DefaultAgent, Command: c.Claude.Command}
		if c.Claude.ResumeFlag != "" {
			spec.ResumeCommand = c.Claude.Command + " " + c.Claude.ResumeFlag
		}
		return spec, nil
	}
	a, ok := c.Agents[name]
	if !ok {
		a, ok = defaultAgents[name]
	}
	if !ok || a.Command == "" {
		return AgentSpec{}, fmt.Errorf("unknown agent %q; known: %s (add [agents.%s] to %s)",
			name, strings.Join(c.AgentNames(), ", "), name, Path())
	}
	return AgentSpec{Name: name, Command: a.Command, ResumeCommand: a.ResumeCommand}, nil
}

// AgentNames returns every resolvable agent name, claude first, the rest sorted.
func (c Config) AgentNames() []string {
	seen := map[string]bool{}
	var rest []string
	for name := range defaultAgents {
		if !seen[name] {
			seen[name] = true
			rest = append(rest, name)
		}
	}
	for name, a := range c.Agents {
		if !seen[name] && a.Command != "" {
			seen[name] = true
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append([]string{DefaultAgent}, rest...)
}

type DaemonConfig struct {
	PollInterval string `toml:"poll_interval"`
	MaxWait      string `toml:"max_wait"`
	ResumeText   string `toml:"resume_text"`
	CaptureLines int    `toml:"capture_lines"`
	KeepAlive    bool   `toml:"keep_alive"`
}

type ListConfig struct {
	MaxAgeDays int `toml:"max_age_days"` // hide inactive projects older than this; 0 = show all
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		BaseDir: filepath.Join(home, "projects", "code"),
		Claude: ClaudeConfig{
			Command:    "claude --dangerously-skip-permissions --remote-control {rc} -n {rc}",
			ResumeFlag: "-c",
		},
		Daemon: DaemonConfig{
			PollInterval: "60s",
			MaxWait:      "5h",
			ResumeText:   "continue",
			CaptureLines: 300,
		},
		List: ListConfig{
			MaxAgeDays: 14,
		},
	}
}

func Path() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "proj", "config.toml")
}

func Load() (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", Path(), err)
	}
	return cfg, nil
}

// Write marshals cfg back to Path(), creating the parent directory if needed.
func Write(cfg Config) error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Duration parses a Go duration string or returns the fallback if empty/invalid.
func Duration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return fallback
}
