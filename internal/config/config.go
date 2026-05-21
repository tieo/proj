// Package config loads the optional ~/.config/proj/config.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	BaseDir     string        `toml:"base_dir"`
	DefaultLang string        `toml:"default_lang"`
	Claude      ClaudeConfig  `toml:"claude"`
	Unreset     UnresetConfig `toml:"unreset"`
}

type ClaudeConfig struct {
	Command    string `toml:"command"`
	ResumeFlag string `toml:"resume_flag"`
}

type UnresetConfig struct {
	PollInterval string `toml:"poll_interval"`
	MaxWait      string `toml:"max_wait"`
	Jitter       string `toml:"jitter"`
	ResumeText   string `toml:"resume_text"`
	CaptureLines int    `toml:"capture_lines"`
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		BaseDir:     filepath.Join(home, "projects", "code"),
		DefaultLang: "polyglot",
		Claude: ClaudeConfig{
			Command:    "claude --dangerously-skip-permissions --remote-control --remote-control-session-name-prefix {name} -n {name}",
			ResumeFlag: "-c",
		},
		Unreset: UnresetConfig{
			PollInterval: "60s",
			MaxWait:      "5h",
			Jitter:       "30s",
			ResumeText:   "continue",
			CaptureLines: 300,
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
