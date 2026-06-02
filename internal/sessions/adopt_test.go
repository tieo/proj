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

// TestAdoptCarriesMemory checks that the source project's memory is merged into
// the target's (copied, never moved) so an adopted session keeps what the
// project taught it, the source keeps its own copy, and the target's existing
// memory is preserved.
func TestAdoptCarriesMemory(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, ".claude")
	oldCwd := `C:\Users\u\scratch`
	newCwd := `\\wsl.localhost\Ubuntu-24.04\home\u\projects\code\proj`

	srcProj := filepath.Join(home, "projects", EncodeCwd(oldCwd))
	srcMem := filepath.Join(srcProj, "memory")
	if err := os.MkdirAll(srcMem, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcProj, "abc123.jsonl")
	if err := os.WriteFile(src, []byte(`{"sessionId":"abc123"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".claude.json"), []byte(`{"projects":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(srcMem, "MEMORY.md"), "- [Fact A](fact-a.md) — hook A\n")
	mustWrite(t, filepath.Join(srcMem, "fact-a.md"), "fact a body")

	// The target project already has its own memory; it must survive the merge.
	dstMem := filepath.Join(home, "projects", EncodeCwd(newCwd), "memory")
	if err := os.MkdirAll(dstMem, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dstMem, "MEMORY.md"), "- [Fact B](fact-b.md) — hook B\n")
	mustWrite(t, filepath.Join(dstMem, "fact-b.md"), "fact b body")

	if _, err := Adopt(home, Session{ID: "abc123", Cwd: oldCwd, Path: src}, newCwd, true); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	// Source fact copied in; target fact untouched.
	if b, err := os.ReadFile(filepath.Join(dstMem, "fact-a.md")); err != nil || string(b) != "fact a body" {
		t.Errorf("source fact not carried over: %v %q", err, b)
	}
	if b, _ := os.ReadFile(filepath.Join(dstMem, "fact-b.md")); string(b) != "fact b body" {
		t.Errorf("target fact must be preserved, got %q", b)
	}
	// Index merged: both pointers present.
	idx, _ := os.ReadFile(filepath.Join(dstMem, "MEMORY.md"))
	if !strings.Contains(string(idx), "Fact A") || !strings.Contains(string(idx), "Fact B") {
		t.Errorf("MEMORY.md should hold both entries, got:\n%s", idx)
	}
	// Source memory preserved (copy, not move).
	if _, err := os.Stat(filepath.Join(srcMem, "fact-a.md")); err != nil {
		t.Error("source memory must remain in place (copied, not moved)")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
