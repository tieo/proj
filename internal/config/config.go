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
	Doner       DonerConfig       `toml:"doner"`
	IdleCompact IdleCompactConfig `toml:"idle_compact"`
}

// IdleCompactConfig is when an idle session is compacted before its prompt
// cache expires: once it has been idle for After, if its context is above
// Above tokens.
type IdleCompactConfig struct {
	Enabled bool   `toml:"enabled"`
	Above   int    `toml:"above"`
	After   string `toml:"after"`
}

// The defaults come from replaying the last four months of both machines'
// transcripts against the current weekly limit: compacting above 250k after 55
// idle minutes, together with Claude Code's own compaction at 700k, kept every
// week but the heaviest under the limit. 55 minutes leaves the minute timer
// five chances before the one-hour cache is gone.
const (
	IdleCompactAboveDefault = 250_000
	IdleCompactAfterDefault = 55 * time.Minute
)

// AboveTokens is Above, falling back to the default when unset.
func (c IdleCompactConfig) AboveTokens() int {
	if c.Above > 0 {
		return c.Above
	}
	return IdleCompactAboveDefault
}

// AfterDuration is After parsed, falling back to the default when unset or
// unreadable.
func (c IdleCompactConfig) AfterDuration() time.Duration {
	if v, err := time.ParseDuration(c.After); err == nil && v > 0 {
		return v
	}
	return IdleCompactAfterDefault
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
		Daemon: DaemonConfig{
			Doner:       DonerConfig{Enabled: true, Wait: WaitDefault.String()},
			IdleCompact: IdleCompactConfig{Enabled: true, Above: IdleCompactAboveDefault, After: IdleCompactAfterDefault.String()},
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
