package daemon

import "testing"

// Pane captures from live sessions. The interop build renders the prompt as
// ">" plus a non-breaking space, the Linux one as "❯"; both occur in one fleet.
const (
	emptyBox = "  ⎿  done\n" +
		"────────────────────────────────\n" +
		"> \n" +
		"────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"

	draftBox = "────────────────────────────────\n" +
		"> restore all 79\n" +
		"────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on\n"

	multiLineBox = "────────────────────────────────\n" +
		"❯ first line of the draft\n" +
		"  second line\n" +
		"  third line\n" +
		"────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on\n"

	pastedBox = "────────────────────────────────\n" +
		"> [Pasted text #1][Pasted text #2]\n" +
		"────────────────────────────────\n" +
		"  paste again to expand\n"

	noBox = "Welcome to Claude Code\n\n  Do you trust the files in this folder?\n"
)

func TestComposerBox(t *testing.T) {
	cases := []struct {
		name        string
		capture     string
		want        string
		placeholder bool
		present     bool
	}{
		{"empty", emptyBox, "", false, true},
		{"draft", draftBox, "restore all 79", false, true},
		{"multi-line", multiLineBox, "first line of the draft\nsecond line\nthird line", false, true},
		{"pasted", pastedBox, "[Pasted text #1][Pasted text #2]", true, true},
		{"no input box", noBox, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ph, present := ComposerBox(c.capture)
			if got != c.want || ph != c.placeholder || present != c.present {
				t.Errorf("ComposerBox = (%q, %v, %v), want (%q, %v, %v)", got, ph, present, c.want, c.placeholder, c.present)
			}
		})
	}
}

// A conversation line quoting a prompt marker must not be mistaken for the box:
// only the LAST marker on screen is the live one.
func TestComposerBoxTakesTheLastPrompt(t *testing.T) {
	capture := "❯ this is quoted output from an earlier turn\n" +
		"────────────────────────────────\n" +
		"> the real draft\n" +
		"────────────────────────────────\n"
	if got, _, _ := ComposerBox(capture); got != "the real draft" {
		t.Errorf("ComposerBox = %q, want the live box", got)
	}
}

func TestComposerEndsWith(t *testing.T) {
	// The TUI wraps a long line and indents the continuation, so the rendered
	// text differs from the input in whitespace alone.
	typed := "one two three four five six seven"
	rendered := "one two three four\nfive six seven"
	if !composerEndsWith(rendered, typed) {
		t.Error("wrapped text should compare equal to what was typed")
	}
	if composerEndsWith("one two three", typed) {
		t.Error("a short fill must not pass as the full text")
	}
	if !composerEndsWith("", "") {
		t.Error("empty should equal empty")
	}
}

// The prompt is appended to whatever stands in the box, so the check is a
// suffix one: a leftover draft in front of it is fine, a cut-off fill is not.
func TestTypeVerifiedAcceptsTextAfterALeftoverDraft(t *testing.T) {
	io := paneIO{
		send: func(string, string) error { return nil },
		read: func(string) (string, bool, bool) { return "leftover draft the whole prompt", false, true },
	}
	ok, err := typeVerifiedVia(io, "s", "the whole prompt")
	if err != nil || !ok {
		t.Fatalf("typeVerified = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestTypeVerifiedRejectsATruncatedFill(t *testing.T) {
	io := paneIO{
		send: func(string, string) error { return nil },
		read: func(string) (string, bool, bool) { return "the whole pro", false, true },
	}
	if ok, _ := typeVerifiedVia(io, "s", "the whole prompt"); ok {
		t.Error("a fill missing its tail was reported as delivered")
	}
}

func TestTypeVerifiedRejectsAPastePlaceholder(t *testing.T) {
	io := paneIO{
		send: func(string, string) error { return nil },
		read: func(string) (string, bool, bool) { return "[Pasted text #1]", true, true },
	}
	if ok, _ := typeVerifiedVia(io, "s", "the whole prompt"); ok {
		t.Error("a paste placeholder was reported as delivered")
	}
}
