package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A directory can have two transcripts written to at once: Claude Code runs the
// conversation in a background host while the terminal process keeps the
// session it started with, so "newest file" is whichever of the two wrote last.
// The session records say which one is actually in use.
func TestRecentSessionFilePrefersTheRunningSession(t *testing.T) {
	home := t.TempDir()
	work := "/home/u/projects/api"
	projectDir := filepath.Join(home, "projects", encodeClaudePath(work))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(projectDir, "stale-id.jsonl")
	live := filepath.Join(projectDir, "live-id.jsonl")
	for _, p := range []string{live, stale} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The abandoned session's transcript is the newer file, which is exactly
	// the case that used to be read.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(live, old, old); err != nil {
		t.Fatal(err)
	}

	writeRecord := func(name, id string, updated int64) {
		dir := filepath.Join(home, "sessions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"sessionId": id, "cwd": work, "updatedAt": updated})
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeRecord("1.json", "stale-id", 1000)
	writeRecord("2.json", "live-id", 2000)

	if got := CurrentSessionID(home, work); got != "live-id" {
		t.Fatalf("CurrentSessionID = %q, want live-id", got)
	}
	if got := recentSessionFile(home, work); got != live {
		t.Fatalf("recentSessionFile = %q, want %q", got, live)
	}

	// With no session records - every session there has exited - the newest
	// transcript is all there is to go on.
	if err := os.RemoveAll(filepath.Join(home, "sessions")); err != nil {
		t.Fatal(err)
	}
	if got := recentSessionFile(home, work); got != stale {
		t.Fatalf("fallback picked %q, want %q", got, stale)
	}
}
