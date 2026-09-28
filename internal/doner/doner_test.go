package doner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsWaiting(t *testing.T) {
	waiting := []string{
		"Waiting on agent-7",
		"waiting on the deploy job",
		"Handed the crawl to a subagent.\n\nWaiting on crawl-42",
		"WAITING ON build-3",
	}
	for _, s := range waiting {
		if !IsWaiting(s) {
			t.Errorf("not read as waiting: %q", s)
		}
	}
	notWaiting := []string{
		"Yes.",
		"waiting on",                          // names nothing, so it buys nothing
		"I am waiting on the build to finish", // a sentence, not the answer
		"",
	}
	for _, s := range notWaiting {
		if IsWaiting(s) {
			t.Errorf("read as waiting: %q", s)
		}
	}
	// The two answers are distinct: a wait is not a finish.
	if IsDone("Waiting on agent-7") {
		t.Error("a wait must not read as done")
	}
}

// A nudge that is answered in seconds and followed by silence achieved
// nothing. Counting those is what stops a loop whose cause proj has no phrase
// for yet, which is how one session was nudged 260 times over a weekly limit.

func TestIsAPIError(t *testing.T) {
	poisoned := "API Error: an image in the conversation could not be processed and was removed. Re-read the file with a different approach if you still need it."
	if !IsAPIError(poisoned) {
		t.Error("the refusal that looped a session for an hour must stand doner down")
	}
	if !IsAPIError("  api error: 529 overloaded") {
		t.Error("leading space and lower case are still the same failure")
	}
	// A session talking about an error has work left; only a turn that never
	// reached the model is a dead end.
	for _, reply := range []string{
		"The API error in auth.go is handled now.",
		"Fixed: the endpoint returned an API Error on empty bodies.",
		"Tests fail with a 500.",
	} {
		if IsAPIError(reply) {
			t.Errorf("%q is a session reporting on work, not a refused turn", reply)
		}
	}
}

func TestIsDone(t *testing.T) {
	// The nudge asks for a reason line and then the word, so the last line
	// answers it.
	for _, reply := range []string{"Yes", "yes.", "**Yes**", "Nothing left to do.\n\nYes"} {
		if !IsDone(reply) {
			t.Errorf("%q answers the nudge and should be let go", reply)
		}
	}
	for _, reply := range []string{
		"Yes, that part is done, moving to the tests now.",
		"Done with the migration. Next: the deploy.",
		"finished",
	} {
		if IsDone(reply) {
			t.Errorf("%q ends a subtask, not the session", reply)
		}
	}
}

func TestTranscriptPath(t *testing.T) {
	got := TranscriptPath("/home/u/.claude", "/home/u/projects/code/phonetix", "abc-123")
	want := "/home/u/.claude/projects/-home-u-projects-code-phonetix/abc-123.jsonl"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A directory with a dot in it encodes the dot as a dash too, which is how
	// .dotfiles.nix is found at all.
	got = TranscriptPath("/r", "/home/u/.dotfiles.nix", "s")
	if want = "/r/projects/-home-u--dotfiles-nix/s.jsonl"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLastReplyTakesTheLastAssistantText(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"assistant","timestamp":"2026-09-19T20:00:00.000Z","message":{"content":[{"type":"text","text":"first"}]}}`,
		`{"type":"assistant","timestamp":"2026-09-19T22:48:37.000Z","message":{"content":[{"type":"text","text":"Waiting on ba9uzs1ng"}]}}`,
		`{"type":"user","timestamp":"2026-09-19T22:49:00.000Z","message":{"content":"ignored"}}`,
	)
	text, at, err := LastReply(p)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Waiting on ba9uzs1ng" {
		t.Errorf("text = %q", text)
	}
	if !IsWaiting(text) {
		t.Error("that reply is what the nudge asks a waiting session to say")
	}
	if at.UTC().Format("15:04:05") != "22:48:37" {
		t.Errorf("at = %v", at)
	}
}

func TestLastReplySkipsToolOnlyTurns(t *testing.T) {
	// A turn that only ran a tool says nothing, and the decision is about what
	// the session last said.
	p := writeTranscript(t,
		`{"type":"assistant","timestamp":"2026-09-19T20:00:00.000Z","message":{"content":[{"type":"text","text":"Yes"}]}}`,
		`{"type":"assistant","timestamp":"2026-09-19T20:01:00.000Z","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`,
	)
	text, _, err := LastReply(p)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Yes" {
		t.Errorf("text = %q, want the last thing actually said", text)
	}
}

func TestLastReplyOnEmptyTranscript(t *testing.T) {
	text, at, err := LastReply(writeTranscript(t))
	if err != nil {
		t.Fatal(err)
	}
	if text != "" || !at.IsZero() {
		t.Errorf("got %q at %v, want nothing", text, at)
	}
}

func TestLastReplyMissingFile(t *testing.T) {
	if _, _, err := LastReply(filepath.Join(t.TempDir(), "absent.jsonl")); err == nil {
		t.Error("a missing transcript is an error the caller has to see, not an empty reply")
	}
}

func TestTranscriptPathFindsAWindowsNamedFolder(t *testing.T) {
	root := t.TempDir()
	unc := filepath.Join(root, "projects", "--wsl-localhost-Ubuntu-24-04-home-u-projects-code-p")
	if err := os.MkdirAll(unc, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(unc, "sid.jsonl")
	if err := os.WriteFile(want, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TranscriptPath(root, "/home/u/projects/code/p", "sid"); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
