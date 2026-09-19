package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultTurnsDonerOn(t *testing.T) {
	if !Default().Daemon.Doner.Active() {
		t.Error("doner is on unless a config turns it off; the tag is the per-project switch")
	}
}

func TestLoadIgnoresRetiredKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A config written when proj was a session manager. Everything here but
	// the doner switch names something that no longer exists.
	old := `base_dir = "/tmp/code"

[claude]
command = "claude --dangerously-skip-permissions"
resume_flag = "-c"

[tools.codex]
command = "codex"

[daemon]
poll_interval = "60s"
capture_lines = 300

[daemon.doner]
enabled = false
grace = "5m"

[list]
max_age_days = 14
`
	if err := os.WriteFile(filepath.Join(dir, "proj", "config.toml"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a config naming retired keys must still load: %v", err)
	}
	if cfg.BaseDir != "/tmp/code" {
		t.Errorf("BaseDir = %q, want /tmp/code", cfg.BaseDir)
	}
	if cfg.Daemon.Doner.Active() {
		t.Error("the file turns doner off and that has to survive the trim")
	}
}

func TestLoadWithoutFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a missing config is not an error: %v", err)
	}
	if cfg.BaseDir == "" {
		t.Error("a missing config still has to name a base directory")
	}
}

func TestWriteThenLoad(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := Default()
	cfg.Daemon.Doner.Enabled = false
	if err := Write(cfg); err != nil {
		t.Fatal(err)
	}
	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if back.Daemon.Doner.Active() {
		t.Error("turning doner off has to survive a round trip through the file")
	}
}
