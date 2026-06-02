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
	line := `{"type":"user","cwd":"C:\\Users\\u\\scratch","sessionId":"abc123","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(src, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".claude.json"), []byte(`{"projects":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	newID, err := Adopt(home, Session{ID: "abc123", Cwd: oldCwd, Path: src}, newCwd, true)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	// Fresh id, so the copy does not collide with the original.
	if newID == "" || newID == "abc123" {
		t.Errorf("expected a fresh session id, got %q", newID)
	}

	// Landed in the new project's encoded folder under the new id.
	dst := filepath.Join(home, "projects", EncodeCwd(newCwd), newID+".jsonl")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("adopted transcript missing at %s: %v", dst, err)
	}
	data, _ := os.ReadFile(dst)
	if strings.Contains(string(data), `Users\\u\\scratch`) {
		t.Error("old cwd still present in adopted transcript")
	}
	if !strings.Contains(string(data), jsonInner(newCwd)) {
		t.Error("new cwd not written into adopted transcript")
	}
	// The internal session id was rewritten to match the new filename.
	if strings.Contains(string(data), `"sessionId":"abc123"`) {
		t.Error("old session id still present in adopted transcript")
	}
	if !strings.Contains(string(data), `"sessionId":"`+newID+`"`) {
		t.Error("new session id not written into adopted transcript")
	}
	// Move (default): the original is gone now that the copy is verified.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("move should have removed the original transcript")
	}
	// Continue pointer updated to the new id.
	cj, _ := os.ReadFile(filepath.Join(base, ".claude.json"))
	if !strings.Contains(string(cj), newID) {
		t.Error("lastSessionId not set to the new id in .claude.json")
	}
}

// TestAdoptCopyFile checks that move=false keeps the original in place.
func TestAdoptCopyFile(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, ".claude")
	oldCwd := `C:\Users\u\scratch`
	newCwd := `\\wsl.localhost\Ubuntu-24.04\home\u\projects\code\proj`

	srcDir := filepath.Join(home, "projects", EncodeCwd(oldCwd))
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "abc123.jsonl")
	line := `{"type":"user","cwd":"C:\\Users\\u\\scratch","sessionId":"abc123","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(src, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".claude.json"), []byte(`{"projects":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	newID, err := Adopt(home, Session{ID: "abc123", Cwd: oldCwd, Path: src}, newCwd, false)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("--copy-file should leave the original transcript in place")
	}
	dst := filepath.Join(home, "projects", EncodeCwd(newCwd), newID+".jsonl")
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("copied transcript missing at %s: %v", dst, err)
	}
}

// TestAdoptCarriesSidecars checks that the task list and file-history folders,
// keyed by session id, follow the session to its new id (moved on a move,
// copied with --copy-file), so an adopted session keeps its tasks.
func TestAdoptCarriesSidecars(t *testing.T) {
	setup := func(t *testing.T) (home, src, oldCwd, newCwd string) {
		base := t.TempDir()
		home = filepath.Join(base, ".claude")
		oldCwd = `C:\Users\u\scratch`
		newCwd = `\\wsl.localhost\Ubuntu-24.04\home\u\projects\code\proj`
		srcDir := filepath.Join(home, "projects", EncodeCwd(oldCwd))
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		src = filepath.Join(srcDir, "abc123.jsonl")
		if err := os.WriteFile(src, []byte(`{"sessionId":"abc123"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, ".claude.json"), []byte(`{"projects":{}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"tasks", "file-history"} {
			d := filepath.Join(home, kind, "abc123")
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "1.json"), []byte(`{"id":"1"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	t.Run("move", func(t *testing.T) {
		home, src, oldCwd, newCwd := setup(t)
		newID, err := Adopt(home, Session{ID: "abc123", Cwd: oldCwd, Path: src}, newCwd, true)
		if err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		for _, kind := range []string{"tasks", "file-history"} {
			if _, err := os.Stat(filepath.Join(home, kind, newID, "1.json")); err != nil {
				t.Errorf("%s not carried to new id: %v", kind, err)
			}
			if _, err := os.Stat(filepath.Join(home, kind, "abc123")); !os.IsNotExist(err) {
				t.Errorf("%s for old id should be gone after a move", kind)
			}
		}
	})

	t.Run("copy-file keeps originals", func(t *testing.T) {
		home, src, oldCwd, newCwd := setup(t)
		newID, err := Adopt(home, Session{ID: "abc123", Cwd: oldCwd, Path: src}, newCwd, false)
		if err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		for _, kind := range []string{"tasks", "file-history"} {
			if _, err := os.Stat(filepath.Join(home, kind, newID, "1.json")); err != nil {
				t.Errorf("%s not copied to new id: %v", kind, err)
			}
			if _, err := os.Stat(filepath.Join(home, kind, "abc123", "1.json")); err != nil {
				t.Errorf("%s for old id should remain with --copy-file: %v", kind, err)
			}
		}
	})
}
