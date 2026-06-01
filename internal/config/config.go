// Package config loads the optional ~/.config/proj/config.toml.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	BaseDir string       `toml:"base_dir"`
	Claude  ClaudeConfig `toml:"claude"`
	Daemon  DaemonConfig `toml:"daemon"`
	List    ListConfig   `toml:"list"`
}

type ClaudeConfig struct {
	Command    string `toml:"command"`
	ResumeFlag string `toml:"resume_flag"`
	Home       string `toml:"home"` // Claude home override; default ~/.claude, or the Windows one when running under WSL
}

type DaemonConfig struct {
	PollInterval string `toml:"poll_interval"`
	MaxWait      string `toml:"max_wait"`
	Jitter       string `toml:"jitter"`
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
			Command:    "claude --dangerously-skip-permissions --remote-control --remote-control-session-name-prefix {name} -n {name}",
			ResumeFlag: "-c",
		},
		Daemon: DaemonConfig{
			PollInterval: "60s",
			MaxWait:      "5h",
			Jitter:       "1s",
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
