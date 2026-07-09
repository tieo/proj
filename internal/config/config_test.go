package config

import (
	"strings"
	"testing"
)

func TestToolClaudeDefault(t *testing.T) {
	cfg := Default()
	for _, name := range []string{"", "claude"} {
		spec, err := cfg.Tool(name)
		if err != nil {
			t.Fatalf("Tool(%q): %v", name, err)
		}
		if spec.Name != "claude" || spec.Command != cfg.Claude.Command {
			t.Errorf("Tool(%q) = %+v; want claude with [claude] command", name, spec)
		}
		if want := cfg.Claude.Command + " " + cfg.Claude.ResumeFlag; spec.ResumeCommand != want {
			t.Errorf("resume command %q; want %q", spec.ResumeCommand, want)
		}
	}
}

func TestToolBuiltins(t *testing.T) {
	cfg := Default()
	codex, err := cfg.Tool("codex")
	if err != nil {
		t.Fatalf("Tool(codex): %v", err)
	}
	if !strings.HasPrefix(codex.Command, "codex") || !strings.Contains(codex.ResumeCommand, "resume --last") {
		t.Errorf("codex spec %+v", codex)
	}
	agy, err := cfg.Tool("agy")
	if err != nil {
		t.Fatalf("Tool(agy): %v", err)
	}
	if !strings.HasPrefix(agy.Command, "agy") || !strings.Contains(agy.ResumeCommand, "--continue") {
		t.Errorf("agy spec %+v", agy)
	}
}

func TestToolUserOverrideAndUnknown(t *testing.T) {
	cfg := Default()
	cfg.Tools = map[string]ToolConfig{
		"codex": {Command: "codex --full-auto"},
		"aider": {Command: "aider"},
	}
	codex, err := cfg.Tool("codex")
	if err != nil {
		t.Fatalf("Tool(codex): %v", err)
	}
	// A [tools.codex] entry replaces the whole built-in recipe, including
	// the default resume command.
	if codex.Command != "codex --full-auto" || codex.ResumeCommand != "" {
		t.Errorf("override spec %+v", codex)
	}
	if _, err := cfg.Tool("aider"); err != nil {
		t.Errorf("user-defined tool should resolve: %v", err)
	}
	if _, err := cfg.Tool("nope"); err == nil {
		t.Error("unknown tool must error")
	}
}

func TestToolNames(t *testing.T) {
	cfg := Default()
	names := cfg.ToolNames()
	if len(names) == 0 || names[0] != "claude" {
		t.Fatalf("claude must come first: %v", names)
	}
	got := strings.Join(names, ",")
	if !strings.Contains(got, "codex") || !strings.Contains(got, "agy") {
		t.Errorf("builtins missing from %v", names)
	}
}
