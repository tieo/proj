package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsDone(t *testing.T) {
	done := []string{
		"Yes", "yes", "  yes.  ", "Yes!", "**Yes**", "yes\n", "Done", "done.",
		"All done", "all done!", "finished", "Completed.", "Yes, done.", "yes done",
	}
	notDone := []string{
		"", "Working on it", "Not yet", "no", "the answer is yes",
		"I said yes to the first option but there is more to do",
		"Here is the result: yes and no", "Let me continue",
	}
	for _, s := range done {
		if !isDone(s) {
			t.Errorf("isDone(%q) = false, want true", s)
		}
	}
	for _, s := range notDone {
		if isDone(s) {
			t.Errorf("isDone(%q) = true, want false", s)
		}
	}
}

func TestLastPathSegment(t *testing.T) {
	cases := map[string]string{
		`\\wsl.localhost\Ubuntu-24.04\home\u\projects\code\29`: "29",
		"/home/u/projects/code/29":                             "29",
		`C:\Users\u\projects\code\29`:                          "29",
		"/home/u/projects/code/29/":                            "29",
		"single":                                               "single",
		"":                                                     "",
	}
	for in, want := range cases {
		if got := lastPathSegment(in); got != want {
			t.Errorf("lastPathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

// Installing merges into settings.json without disturbing other keys or hooks,
// installing twice is idempotent, and uninstalling leaves the file as it was.
func TestDonerHookMergeRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo other"}}},
			},
			"PreToolUse": []any{map[string]any{"matcher": "Bash"}},
		},
	}
	writeMust(t, path, original)

	root, _ := readSettings(path)
	// Add our entry twice; the second must be a no-op.
	for i := 0; i < 2; i++ {
		matchers := stopMatchers(root)
		kept := matchers[:0:0]
		for _, e := range matchers {
			if !isDonerHookEntry(e) {
				kept = append(kept, e)
			}
		}
		kept = append(kept, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "proj doner-hook"}}})
		setStopMatchers(root, kept)
	}
	if got := len(stopMatchers(root)); got != 2 {
		t.Fatalf("after two installs, Stop has %d matchers, want 2 (other + doner)", got)
	}
	if _, ok := root["model"]; !ok {
		t.Error("install dropped an unrelated setting")
	}
	if h, _ := root["hooks"].(map[string]any); h["PreToolUse"] == nil {
		t.Error("install dropped an unrelated hook")
	}

	// Uninstall: our entry goes, the other stays.
	matchers := stopMatchers(root)
	kept := matchers[:0:0]
	for _, e := range matchers {
		if !isDonerHookEntry(e) {
			kept = append(kept, e)
		}
	}
	setStopMatchers(root, kept)
	left := stopMatchers(root)
	if len(left) != 1 || isDonerHookEntry(left[0]) {
		t.Fatalf("uninstall left the wrong matchers: %v", left)
	}
}

func writeMust(t *testing.T, path string, v map[string]any) {
	t.Helper()
	data, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A session waiting on a background shell has nothing to continue, so doner
// must let it stop rather than spend a turn re-reporting the wait.
func TestHasRunningTask(t *testing.T) {
	type task = backgroundTask
	if hasRunningTask(nil) {
		t.Error("no tasks should not read as running")
	}
	if !hasRunningTask([]task{{Status: "running"}}) {
		t.Error("a running task should hold the nudge")
	}
	if hasRunningTask([]task{{Status: "completed"}, {Status: "failed"}}) {
		t.Error("finished tasks should not hold the nudge")
	}
	if !hasRunningTask([]task{{Status: "completed"}, {Status: "running"}}) {
		t.Error("one live task among finished ones should hold the nudge")
	}
}
