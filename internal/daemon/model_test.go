package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredClaudeModelPrecedence(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	writeSettings(t, filepath.Join(home, "settings.json"), `{"model":"claude-opus-5"}`)

	if got := ConfiguredClaudeModel(home, proj, ""); got != "claude-opus-5" {
		t.Fatalf("user settings model = %q", got)
	}

	writeSettings(t, filepath.Join(proj, ".claude", "settings.json"), `{"model":"sonnet"}`)
	if got := ConfiguredClaudeModel(home, proj, ""); got != "sonnet" {
		t.Fatalf("project settings model = %q", got)
	}

	writeSettings(t, filepath.Join(proj, ".claude", "settings.local.json"), `{"model":"haiku"}`)
	if got := ConfiguredClaudeModel(home, proj, ""); got != "haiku" {
		t.Fatalf("project local settings model = %q", got)
	}

	cmd := "claude --dangerously-skip-permissions --model claude-fable-5 -n {rc}"
	if got := ConfiguredClaudeModel(home, proj, cmd); got != "claude-fable-5" {
		t.Fatalf("launch command model = %q", got)
	}
}

func TestConfiguredClaudeModelUnset(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	writeSettings(t, filepath.Join(home, "settings.json"), `{"permissions":{"allow":[]}}`)
	writeSettings(t, filepath.Join(proj, ".claude", "settings.json"), `not json`)

	if got := ConfiguredClaudeModel(home, proj, "claude -c"); got != "" {
		t.Fatalf("model = %q, want empty", got)
	}
}

func TestModelFlagForms(t *testing.T) {
	cases := map[string]string{
		`claude --model=claude-opus-5`:   "claude-opus-5",
		`claude --model "claude-opus-5"`: "claude-opus-5",
		`claude --modelfoo bar`:          "",
		`claude --model`:                 "",
	}
	for cmd, want := range cases {
		if got := modelFlag(cmd); got != want {
			t.Errorf("modelFlag(%q) = %q, want %q", cmd, got, want)
		}
	}
}
