package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdopt(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, ".claude")
	oldCwd := `C:\Users\u\scratch`
	newCwd := `\\wsl.localhost\Ubuntu-24.04\home\u\projects\code\proj`

	srcDir := filepath.Join(home, "projects", EncodeCwd(oldCwd))
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "abc123.jsonl")
	line := `{"type":"user","cwd":"C:\\Users\\u\\scratch","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(src, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".claude.json"), []byte(`{"projects":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	dst, err := Adopt(home, Session{ID: "abc123", Cwd: oldCwd, Path: src}, newCwd)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	// Landed in the new project's encoded folder.
	wantDir := filepath.Join(home, "projects", EncodeCwd(newCwd))
	if filepath.Dir(dst) != wantDir {
		t.Errorf("dst dir = %s, want %s", filepath.Dir(dst), wantDir)
	}
	data, _ := os.ReadFile(dst)
	if strings.Contains(string(data), `Users\\u\\scratch`) {
		t.Error("old cwd still present in adopted transcript")
	}
	if !strings.Contains(string(data), jsonInner(newCwd)) {
		t.Error("new cwd not written into adopted transcript")
	}
	// Copy, not move: the original stays.
	if _, err := os.Stat(src); err != nil {
		t.Error("original transcript was removed; adopt should copy")
	}
	// Continue pointer updated.
	cj, _ := os.ReadFile(filepath.Join(base, ".claude.json"))
	if !strings.Contains(string(cj), `"abc123"`) {
		t.Error("lastSessionId not set in .claude.json")
	}
}
