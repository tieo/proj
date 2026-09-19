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

// DonerConfig is doner's global switch and its one number. A project opts in
// by carrying the "doner" tag; Enabled turns the whole mechanism off without
// touching the tags.
type DonerConfig struct {
	Enabled bool `toml:"enabled"`
	// Wait is how long a session that stopped without reporting done is left
	// alone before the sweep asks again. It is the number the nudge quotes
	// back to a waiting session, so changing it here changes what sessions are
	// promised.
	Wait string `toml:"wait"`
}

// WaitDefault is how long a stopped session is left alone when no wait is
// configured: long enough that a job worth waiting for has a chance to finish,
// short enough that one which never returns does not park the session for the
// rest of the day.
const WaitDefault = 30 * time.Minute

// Active reports whether doner runs.
func (d DonerConfig) Active() bool { return d.Enabled }

// WaitDuration is Wait parsed, falling back to the default when unset or
// unreadable.
func (d DonerConfig) WaitDuration() time.Duration {
	if d.Wait == "" {
		return WaitDefault
	}
	if v, err := time.ParseDuration(d.Wait); err == nil && v > 0 {
		return v
	}
	return WaitDefault
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		BaseDir: filepath.Join(home, "projects", "code"),
		Daemon:  DaemonConfig{Doner: DonerConfig{Enabled: true, Wait: WaitDefault.String()}},
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
