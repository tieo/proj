package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tieo/proj/internal/config"
)

// setDefaultModel writes the model Claude Code reads as its new-session default
// while leaving every other setting - including the doner hook - in place.
func TestSetDefaultModelPreservesSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config")) // keep config.Load off the real one

	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claude, "settings.json")
	original := map[string]any{
		"model": "claude-opus-4-8",
		"hooks": map[string]any{"Stop": []any{"keep me"}},
	}
	data, _ := json.MarshalIndent(original, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{}
	cfg.Claude.Home = claude
	if err := setDefaultModel(cfg, "claude-opus-5"); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	raw, _ := io.ReadAll(mustOpen(t, path))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "claude-opus-5" {
		t.Errorf("model = %v, want claude-opus-5", got["model"])
	}
	if got["hooks"] == nil {
		t.Error("setDefaultModel dropped the hooks block")
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestJoinSorted(t *testing.T) {
	if got := joinSorted([]string{"c", "a", "b"}); got != "a, b, c" {
		t.Errorf("joinSorted = %q, want \"a, b, c\"", got)
	}
	if got := joinSorted(nil); got != "" {
		t.Errorf("joinSorted(nil) = %q, want empty", got)
	}
}
