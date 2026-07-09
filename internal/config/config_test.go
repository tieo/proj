package config

import (
	"strings"
	"testing"
)

func TestAgentClaudeDefault(t *testing.T) {
	cfg := Default()
	for _, name := range []string{"", "claude"} {
		spec, err := cfg.Agent(name)
		if err != nil {
			t.Fatalf("Agent(%q): %v", name, err)
		}
		if spec.Name != "claude" || spec.Command != cfg.Claude.Command {
			t.Errorf("Agent(%q) = %+v; want claude with [claude] command", name, spec)
		}
		if want := cfg.Claude.Command + " " + cfg.Claude.ResumeFlag; spec.ResumeCommand != want {
			t.Errorf("resume command %q; want %q", spec.ResumeCommand, want)
		}
	}
}

func TestAgentBuiltins(t *testing.T) {
	cfg := Default()
	codex, err := cfg.Agent("codex")
	if err != nil {
		t.Fatalf("Agent(codex): %v", err)
	}
	if !strings.HasPrefix(codex.Command, "codex") || !strings.Contains(codex.ResumeCommand, "resume --last") {
		t.Errorf("codex spec %+v", codex)
	}
	agy, err := cfg.Agent("agy")
	if err != nil {
		t.Fatalf("Agent(agy): %v", err)
	}
	if agy.Command != "agy" || agy.ResumeCommand != "" {
		t.Errorf("agy spec %+v", agy)
	}
}

func TestAgentUserOverrideAndUnknown(t *testing.T) {
	cfg := Default()
	cfg.Agents = map[string]AgentConfig{
		"codex": {Command: "codex --full-auto"},
		"aider": {Command: "aider"},
	}
	codex, err := cfg.Agent("codex")
	if err != nil {
		t.Fatalf("Agent(codex): %v", err)
	}
	// A [agents.codex] entry replaces the whole built-in recipe, including
	// the default resume command.
	if codex.Command != "codex --full-auto" || codex.ResumeCommand != "" {
		t.Errorf("override spec %+v", codex)
	}
	if _, err := cfg.Agent("aider"); err != nil {
		t.Errorf("user-defined agent should resolve: %v", err)
	}
	if _, err := cfg.Agent("nope"); err == nil {
		t.Error("unknown agent must error")
	}
}

func TestAgentNames(t *testing.T) {
	cfg := Default()
	names := cfg.AgentNames()
	if len(names) == 0 || names[0] != "claude" {
		t.Fatalf("claude must come first: %v", names)
	}
	got := strings.Join(names, ",")
	if !strings.Contains(got, "codex") || !strings.Contains(got, "agy") {
		t.Errorf("builtins missing from %v", names)
	}
}
