package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// The backstop reads the last assistant turn out of the transcript and judges
// it the way the Stop hook judges last_assistant_message, so a session that
// answered is not nudged again. This is the reply that was: a reason line and
// then the word, which a whole-message match would have missed.
func TestLastAssistantTextAndDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := `{"type":"user","message":{"content":"go on"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}
{"type":"user","message":{"content":"[hook] nudge"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Erteiltes fertig, Rest wartet auf deine Freigabe.\n\nYes"}]}}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	got := lastAssistantText(path)
	if got == "" {
		t.Fatal("no assistant turn read back")
	}
	if !IsDone(got) {
		t.Errorf("last turn %q should read as done", got)
	}

	// An unfinished last turn must still be nudgeable.
	more := lines + `{"type":"assistant","message":{"content":[{"type":"text","text":"still three findings open"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(more), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsDone(lastAssistantText(path)) {
		t.Error("a status line should not read as done")
	}

	// A message whose content is a bare string, and a missing file.
	plain := filepath.Join(t.TempDir(), "p.jsonl")
	if err := os.WriteFile(plain, []byte(`{"type":"assistant","message":{"content":"Yes"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsDone(lastAssistantText(plain)) {
		t.Error("a string-content turn should be read too")
	}
	if lastAssistantText(filepath.Join(t.TempDir(), "gone.jsonl")) != "" {
		t.Error("a missing transcript should read empty")
	}
}
