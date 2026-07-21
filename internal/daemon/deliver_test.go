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
	}{
		{"empty", emptyBox, "", false},
		{"draft", draftBox, "restore all 79", false},
		{"multi-line", multiLineBox, "first line of the draft\nsecond line\nthird line", false},
		{"pasted", pastedBox, "[Pasted text #1][Pasted text #2]", true},
		{"no input box", noBox, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ph := ComposerBox(c.capture)
			if got != c.want || ph != c.placeholder {
				t.Errorf("ComposerBox = (%q, %v), want (%q, %v)", got, ph, c.want, c.placeholder)
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
	if got, _ := ComposerBox(capture); got != "the real draft" {
		t.Errorf("ComposerBox = %q, want the live box", got)
	}
}

func TestSameComposerText(t *testing.T) {
	// The TUI wraps a long line and indents the continuation, so the rendered
	// text differs from the input in whitespace alone.
	typed := "one two three four five six seven"
	rendered := "one two three four\nfive six seven"
	if !sameComposerText(rendered, typed) {
		t.Error("wrapped text should compare equal to what was typed")
	}
	if sameComposerText("one two three", typed) {
		t.Error("a short fill must not pass as the full text")
	}
	if !sameComposerText("", "") {
		t.Error("empty should equal empty")
	}
}

// A mangled fill must be retyped, not submitted: the TUI drops input often
// enough that one clean attempt cannot be assumed.
func TestTypeVerifiedRetriesAMangledFill(t *testing.T) {
	want := "the whole prompt"
	boxes := []string{"the whole", "[Pasted text #1]", want}
	var sends, clears int
	io := paneIO{
		send:  func(string, string) error { sends++; return nil },
		read:  func(string) (string, bool) { b := boxes[0]; boxes = boxes[1:]; return b, b == "[Pasted text #1]" },
		clear: func(string) error { clears++; return nil },
	}
	ok, err := typeVerifiedVia(io, "s", want)
	if err != nil || !ok {
		t.Fatalf("typeVerified = (%v, %v), want (true, nil)", ok, err)
	}
	if sends != 3 || clears != 2 {
		t.Errorf("sends=%d clears=%d, want 3 and 2", sends, clears)
	}
}

func TestTypeVerifiedGivesUp(t *testing.T) {
	io := paneIO{
		send:  func(string, string) error { return nil },
		read:  func(string) (string, bool) { return "half of it", false },
		clear: func(string) error { return nil },
	}
	ok, err := typeVerifiedVia(io, "s", "the whole prompt")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a fill that never matched was reported as delivered")
	}
}
