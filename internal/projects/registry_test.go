package projects

import "testing"

func TestSetAgent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetAgent("api", "codex"); err != nil {
		t.Fatal(err)
	}
	reg, _ = LoadRegistry()
	if got := reg.Agent("api"); got != "codex" {
		t.Errorf("Agent = %q, want codex", got)
	}
	// Setting claude clears the field; with nothing else set the entry goes away.
	if err := reg.SetAgent("api", "claude"); err != nil {
		t.Fatal(err)
	}
	reg, _ = LoadRegistry()
	if got := reg.Agent("api"); got != "" {
		t.Errorf("Agent after reset = %q, want empty", got)
	}
	if _, ok := reg.Projects["api"]; ok {
		t.Error("empty entry should be dropped")
	}
}

func TestSetAgentKeepsTags(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg, _ := LoadRegistry()
	if err := reg.SetTags("api", []string{"work"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetAgent("api", "codex"); err != nil {
		t.Fatal(err)
	}
	// Clearing tags must not drop the entry while an agent is set.
	if err := reg.SetTags("api", nil); err != nil {
		t.Fatal(err)
	}
	reg, _ = LoadRegistry()
	if got := reg.Agent("api"); got != "codex" {
		t.Errorf("Agent = %q, want codex", got)
	}
}
