package codex

import (
	"strings"
	"testing"

	"github.com/tieo/proj/internal/handoff"
)

func TestItems(t *testing.T) {
	transcript := &handoff.Transcript{SourceTool: "claude", Turns: []handoff.Turn{
		{Role: "user", Text: "fix it"},
		{Role: "assistant", Text: "working"},
		{Role: "tool", Name: "Bash", Text: "go test"},
	}}
	items := items(transcript, "/tmp/handoff.json")
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4", len(items))
	}
	if items[1]["role"] != "user" || items[2]["phase"] != "final_answer" || !strings.Contains(items[3]["content"].([]map[string]string)[0]["text"], "ran Bash") {
		t.Fatalf("unexpected items: %#v", items)
	}
}
