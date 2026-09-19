package doner

import "testing"

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
