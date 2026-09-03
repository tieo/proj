package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

// A rename must carry everything a project's Claude folder holds, not just
// its top-level .jsonl transcripts: the memory/ directory the auto-memory
// system keeps, a bridge-pointer.json, and a transcript's own subagents/
// subfolder. Reproduces the bug where memory/ was left behind under the old
// slug because the original loop skipped every directory entry.
func TestMigrateHistoryCarriesMemoryAndOtherFiles(t *testing.T) {
	home := t.TempDir()
	oldDir := filepath.Join(t.TempDir(), "wordtap")
	newDir := filepath.Join(t.TempDir(), "taptitude")

	oldFolder := filepath.Join(home, "projects", EncodeCwd(oldDir))
	mustMkdir(t, filepath.Join(oldFolder, "memory"))
	mustWrite(t, filepath.Join(oldFolder, "memory", "MEMORY.md"), "- [x](x.md)")
	mustWrite(t, filepath.Join(oldFolder, "memory", "user-languages.md"), "spanish")
	mustWrite(t, filepath.Join(oldFolder, "bridge-pointer.json"), `{"sessionId":"abc"}`)
	mustWrite(t, filepath.Join(oldFolder, "abc123.jsonl"), `{"cwd":"`+oldDir+`"}`)
	mustMkdir(t, filepath.Join(oldFolder, "abc123", "subagents"))
	mustWrite(t, filepath.Join(oldFolder, "abc123", "subagents", "agent-1.jsonl"), "sub-agent data")

	MigrateHistory(home, oldDir, newDir)

	newFolder := filepath.Join(home, "projects", EncodeCwd(newDir))
	mustExist(t, filepath.Join(newFolder, "memory", "MEMORY.md"), "- [x](x.md)")
	mustExist(t, filepath.Join(newFolder, "memory", "user-languages.md"), "spanish")
	mustExist(t, filepath.Join(newFolder, "bridge-pointer.json"), `{"sessionId":"abc"}`)
	mustExist(t, filepath.Join(newFolder, "abc123", "subagents", "agent-1.jsonl"), "sub-agent data")
	mustExist(t, filepath.Join(newFolder, "abc123.jsonl"), `{"cwd":"`+newDir+`"}`)

	if _, err := os.Stat(oldFolder); !os.IsNotExist(err) {
		t.Errorf("old folder %s should be gone once everything moved out, stat err = %v", oldFolder, err)
	}
}

// The new project's folder can already exist - a session already started
// under the new name before the rename's migration runs. Migrating must
// merge into it (filling in what the target is missing) rather than
// refusing, and must not clobber a same-named file already standing there.
func TestMigrateHistoryMergesIntoAnExistingTarget(t *testing.T) {
	home := t.TempDir()
	oldDir := filepath.Join(t.TempDir(), "wordtap")
	newDir := filepath.Join(t.TempDir(), "taptitude")

	oldFolder := filepath.Join(home, "projects", EncodeCwd(oldDir))
	mustMkdir(t, filepath.Join(oldFolder, "memory"))
	mustWrite(t, filepath.Join(oldFolder, "memory", "MEMORY.md"), "old content")
	mustWrite(t, filepath.Join(oldFolder, "old-only.jsonl"), "old transcript")

	newFolder := filepath.Join(home, "projects", EncodeCwd(newDir))
	mustMkdir(t, filepath.Join(newFolder, "memory"))
	mustWrite(t, filepath.Join(newFolder, "memory", "MEMORY.md"), "") // empty, as a fresh session leaves it
	mustWrite(t, filepath.Join(newFolder, "new-only.jsonl"), "new transcript")

	MigrateHistory(home, oldDir, newDir)

	// The target's own file is not overwritten by the source's.
	mustExist(t, filepath.Join(newFolder, "memory", "MEMORY.md"), "")
	// Both sides' other content survives the merge.
	mustExist(t, filepath.Join(newFolder, "new-only.jsonl"), "new transcript")
	mustExist(t, filepath.Join(oldFolder, "memory", "MEMORY.md"), "old content")
	mustExist(t, filepath.Join(newFolder, "old-only.jsonl"), "old transcript")
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustExist(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}
