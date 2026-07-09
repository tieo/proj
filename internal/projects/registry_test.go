package projects

import "testing"

func TestSetTool(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetTool("api", "codex"); err != nil {
		t.Fatal(err)
	}
	reg, _ = LoadRegistry()
	if got := reg.Tool("api"); got != "codex" {
		t.Errorf("Tool = %q, want codex", got)
	}
	// Setting claude clears the field; with nothing else set the entry goes away.
	if err := reg.SetTool("api", "claude"); err != nil {
		t.Fatal(err)
	}
	reg, _ = LoadRegistry()
	if got := reg.Tool("api"); got != "" {
		t.Errorf("Tool after reset = %q, want empty", got)
	}
	if _, ok := reg.Projects["api"]; ok {
		t.Error("empty entry should be dropped")
	}
}

func TestSetToolKeepsTags(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg, _ := LoadRegistry()
	if err := reg.SetTags("api", []string{"work"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetTool("api", "codex"); err != nil {
		t.Fatal(err)
	}
	// Clearing tags must not drop the entry while an tool is set.
	if err := reg.SetTags("api", nil); err != nil {
		t.Fatal(err)
	}
	reg, _ = LoadRegistry()
	if got := reg.Tool("api"); got != "codex" {
		t.Errorf("Tool = %q, want codex", got)
	}
}
