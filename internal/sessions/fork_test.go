package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forkTranscript is three user turns, each followed by an assistant reply.
func forkTranscript(cwd, id string) string {
	u := func(text string) string {
		return `{"type":"user","cwd":"` + cwd + `","sessionId":"` + id + `","message":{"role":"user","content":"` + text + `"}}`
	}
	a := func(text string) string {
		return `{"type":"assistant","cwd":"` + cwd + `","sessionId":"` + id + `","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`
	}
	lines := []string{u("one"), a("reply-one"), u("two"), a("reply-two"), u("three"), a("reply-three")}
	return strings.Join(lines, "\n") + "\n"
}

func TestPrompts(t *testing.T) {
	dir := t.TempDir()
	body := forkTranscript("/x", "abc123")
	path := filepath.Join(dir, "abc123.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	prompts, err := Prompts(path)
	if err != nil {
		t.Fatalf("Prompts: %v", err)
	}
	if len(prompts) != 3 {
		t.Fatalf("got %d prompts, want 3", len(prompts))
	}
	for i, want := range []string{"one", "two", "three"} {
		if prompts[i].Text != want {
			t.Errorf("prompt %d text = %q, want %q", i, prompts[i].Text, want)
		}
	}
	// Cutting after prompt 0 keeps its turn and reply, and nothing of prompt 1.
	kept := body[:prompts[0].CutAt]
	if !strings.Contains(kept, "reply-one") {
		t.Error("cut after prompt 1 should keep its reply")
	}
	if strings.Contains(kept, "two") {
		t.Error("cut after prompt 1 must not include the second prompt")
	}
	// The last prompt keeps the whole file.
	if prompts[2].CutAt != len(body) {
		t.Errorf("last prompt CutAt = %d, want %d (whole file)", prompts[2].CutAt, len(body))
	}
}

func TestFork(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, ".claude")
	oldCwd := `C:\Users\u\scratch`
	newCwd := `\\wsl.localhost\Ubuntu-24.04\home\u\projects\code\sorden`

	srcDir := filepath.Join(home, "projects", EncodeCwd(oldCwd))
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := forkTranscript(`C:\\Users\\u\\scratch`, "abc123")
	src := filepath.Join(srcDir, "abc123.jsonl")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".claude.json"), []byte(`{"projects":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	prompts, err := Prompts(src)
	if err != nil {
		t.Fatal(err)
	}
	// Fork after the second prompt: keep "two"/"reply-two", drop "three".
	newID, _, err := Fork(home, Session{ID: "abc123", Cwd: oldCwd, Path: src}, newCwd, prompts[1].CutAt)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if newID == "" || newID == "abc123" {
		t.Errorf("expected a fresh session id, got %q", newID)
	}
	// Source left intact (fork is always a copy).
	if _, err := os.Stat(src); err != nil {
		t.Error("fork must leave the original transcript in place")
	}
	dst := filepath.Join(home, "projects", EncodeCwd(newCwd), newID+".jsonl")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("forked transcript missing at %s: %v", dst, err)
	}
	got := string(data)
	if !strings.Contains(got, "reply-two") {
		t.Error("forked transcript should keep the chosen turn's reply")
	}
	if strings.Contains(got, "three") {
		t.Error("forked transcript must be truncated before the later prompt")
	}
	if strings.Contains(got, `Users\\u\\scratch`) {
		t.Error("old cwd still present in forked transcript")
	}
	if !strings.Contains(got, jsonInner(newCwd)) {
		t.Error("new cwd not written into forked transcript")
	}
	if strings.Contains(got, `"sessionId":"abc123"`) {
		t.Error("old session id still present in forked transcript")
	}
}
