// Package config loads the optional ~/.config/proj/config.toml.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	BaseDir string       `toml:"base_dir"`
	Claude  ClaudeConfig `toml:"claude"`
	Daemon  DaemonConfig `toml:"daemon"`
}

type ClaudeConfig struct {
	// Home overrides where Claude Code keeps its settings. Default ~/.claude,
	// or the Windows one when running under WSL, where claude.exe reads the
	// settings of the Windows user rather than the distro's.
	Home string `toml:"home"`
}

// DaemonConfig survives as the section the doner switch lives under, because
// that is where it has always been written and an existing config still says
// so.
type DaemonConfig struct {
	Doner DonerConfig `toml:"doner"`
}

// DonerConfig is doner's global switch. A project opts in by carrying the
// "doner" tag; this turns the whole mechanism off without touching the tags.
type DonerConfig struct {
	Enabled bool `toml:"enabled"`
}

// Active reports whether doner runs.
func (d DonerConfig) Active() bool { return d.Enabled }

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		BaseDir: filepath.Join(home, "projects", "code"),
		Daemon:  DaemonConfig{Doner: DonerConfig{Enabled: true}},
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

// Load reads the config file. Keys it no longer knows are ignored, so a config
// written when proj was a session manager still loads.
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
