package unreset

import (
	"strings"
	"testing"
	"time"
)

// Captured 2026-05-21 from a real tmux pane (proj user's session that hit
// the Pro plan extra-usage exhaustion).
const realBannerInline = `  ⎿  You're out of extra usage · resets 3am (Europe/Berlin)`

// Same banner, but with the timezone wrapped onto the next line by tmux
// reflow. Indentation comes from the ⎿ continuation prefix.
const realBannerWrapped = `  ⎿  You're out of extra usage · resets 3am
     (Europe/Berlin)`

// Real selector shown after /rate-limit-options (or mid-call when limits hit).
const realSelector = `──────────
  What do you want to do?

  ❯ 1. Stop and wait for limit to reset
    2. Upgrade your plan
    3. Upgrade to Team plan

  Enter to confirm · Esc to cancel`

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// ---------- Detect ----------

func TestDetect_RealBannerInline(t *testing.T) {
	if Detect(realBannerInline, time.Now()) == nil {
		t.Fatal("expected detection of inline real banner")
	}
}

func TestDetect_RealBannerWrapped(t *testing.T) {
	if Detect(realBannerWrapped, time.Now()) == nil {
		t.Fatal("expected detection of wrapped real banner")
	}
}

func TestDetect_NoTimezone(t *testing.T) {
	s := "  ⎿  You're out of extra usage · resets 3am"
	if Detect(s, time.Now()) == nil {
		t.Fatal("expected detection without timezone")
	}
}

func TestDetect_StillUsingCreditsIgnored(t *testing.T) {
	// Banner with the prefix but credits still active.
	s := "  ⎿  out of extra usage · resets 3pm — continuing with extra usage"
	if Detect(s, time.Now()) != nil {
		t.Error("must not track a session still proceeding via credits")
	}
}

func TestDetect_NoMatch(t *testing.T) {
	for _, s := range []string{
		"",
		"just some terminal output",
		"resets 3am",
		"You're out of extra usage",
		"$ ls -la /tmp/foo\nfile.txt",
	} {
		if got := Detect(s, time.Now()); got != nil {
			t.Errorf("Detect(%q) = %+v, want nil", s, got)
		}
	}
}

// ---------- false-positive rejection (no ⎿ prefix) ----------

func TestDetect_QuotedInProseRejected(t *testing.T) {
	s := `Earlier the banner said: You're out of extra usage · resets 3am (Europe/Berlin)`
	if Detect(s, time.Now()) != nil {
		t.Error("prose mentioning the banner phrase must not match without the ⎿ prefix")
	}
}

func TestDetect_UserTypedRejected(t *testing.T) {
	// The Claude TUI prompt prefix is "❯ "; user-typed text never has ⎿.
	s := `❯ what does "out of extra usage · resets 3am" mean?`
	if Detect(s, time.Now()) != nil {
		t.Error("user-typed text containing the phrase must not match")
	}
}

func TestDetect_CodeFenceRejected(t *testing.T) {
	s := "```\nYou're out of extra usage · resets 3am (Europe/Berlin)\n```"
	if Detect(s, time.Now()) != nil {
		t.Error("banner inside a markdown code fence must not match")
	}
}

func TestDetect_BuriedInScrollbackIgnored(t *testing.T) {
	padding := strings.Repeat("x ", recentWindow)
	content := realBannerInline + "\n" + padding
	if got := Detect(content, time.Now()); got != nil {
		t.Errorf("banner buried in scrollback must not match, got %+v", got)
	}
}

func TestDetect_MostRecentValidMatchWins(t *testing.T) {
	// Two prefixed banners — last one wins.
	old := "  ⎿  You're out of extra usage · resets 6am (Europe/Berlin)\n"
	current := "  ⎿  You're out of extra usage · resets 3am (Europe/Berlin)"
	content := old + strings.Repeat("normal output\n", 5) + current
	b := Detect(content, time.Now())
	if b == nil {
		t.Fatal("nil")
	}
	if !strings.Contains(b.Text, "3am") {
		t.Errorf("most recent valid match should win; got %q", b.Text)
	}
}

func TestDetect_MixedValidAndInvalidPicksValid(t *testing.T) {
	// User typed a question after the real banner appeared. Real banner
	// (with ⎿) should still win even though the user's text is more recent.
	real := "  ⎿  You're out of extra usage · resets 3am (Europe/Berlin)\n"
	userTyped := `❯ what does "out of extra usage · resets 3am" mean?`
	content := real + strings.Repeat("\n", 3) + userTyped
	b := Detect(content, time.Now())
	if b == nil {
		t.Fatal("expected the real ⎿-prefixed banner to be detected even with a later unprefixed mention")
	}
}

func TestDetect_FixtureSanity(t *testing.T) {
	for _, s := range []string{realBannerInline, realBannerWrapped} {
		if !strings.ContainsRune(s, toolPrefix) {
			t.Fatalf("fixture lost its ⎿ prefix:\n%s", s)
		}
	}
	if !strings.Contains(realSelector, "Stop and wait for limit to reset") {
		t.Fatal("selector fixture lost its key phrase")
	}
}

// ---------- HasSelector ----------

func TestHasSelector_Real(t *testing.T) {
	if !HasSelector(realSelector) {
		t.Error("real /rate-limit-options selector should be detected")
	}
}

func TestHasSelector_BannerOnly(t *testing.T) {
	if HasSelector(realBannerInline) {
		t.Error("a bare banner without the selector menu must not match")
	}
}

func TestHasSelector_EmptyPrompt(t *testing.T) {
	if HasSelector("❯ \n") {
		t.Error("an empty Claude prompt is not a selector")
	}
}

// ---------- Decide ----------

func TestDecide_NoBanner(t *testing.T) {
	if got := Decide("just shell output\n$ ls\n", Tracked{}, time.Now()); got != ActNone {
		t.Errorf("got %v, want ActNone", got)
	}
}

func TestDecide_FirstAttemptImmediate(t *testing.T) {
	// No prior state → first attempt fires immediately (no waiting).
	if got := Decide(realBannerInline, Tracked{}, time.Now()); got != ActResume {
		t.Errorf("got %v, want ActResume", got)
	}
}

func TestDecide_BannerPlusSelectorIsResume(t *testing.T) {
	// Selector handling is independent of Decide; banner-present is the
	// only thing that matters for the Resume/Wait classification.
	if got := Decide(realBannerInline+"\n"+realSelector, Tracked{}, time.Now()); got != ActResume {
		t.Errorf("got %v, want ActResume (selector dismissal happens in Tick, outside Decide)", got)
	}
}

// Real capture from a resume-old-session prompt — the daemon should
// recognize this as a dismissable picker even with no usage-limit banner.
const realResumePicker = `  This session is 2d 21h old and 126.3k tokens.

  Resuming the full session will consume a substantial portion of your usage limits. We recommend resuming from a summary.

  ❯ 1. Resume from summary (recommended)
    2. Resume full session as-is
    3. Don't ask me again

  Enter to confirm · Esc to cancel`

func TestHasSelector_ResumePicker(t *testing.T) {
	if !HasSelector(realResumePicker) {
		t.Error("resume-from-summary picker must be recognized as a dismissable selector")
	}
}

func TestHasSelector_QuotedPickerInScrollbackRejected(t *testing.T) {
	// Picker text verbatim near the top, then plenty of newer chat content
	// pushes it well past the recent-window threshold.
	padding := strings.Repeat("more chat output\n", recentWindow/16)
	content := realSelector + "\n\n" + padding
	if HasSelector(content) {
		t.Error("picker quoted in deep scrollback must not be flagged as live")
	}
}

func TestHasSelector_PhraseWithoutOptionLineRejected(t *testing.T) {
	// Just the phrase mentioned in prose — no "❯ 1." option line.
	s := "Earlier I saw the prompt that says 'Stop and wait for limit to reset' and dismissed it."
	if HasSelector(s) {
		t.Error("phrase-only mention must not be flagged without the option-line structure")
	}
}

func TestDecide_WaitWhenRetryScheduled(t *testing.T) {
	now := time.Now()
	prev := Tracked{NextAttempt: now.Add(2 * time.Hour)}
	if got := Decide(realBannerInline, prev, now); got != ActWait {
		t.Errorf("got %v, want ActWait (retry scheduled 2h out)", got)
	}
}

func TestDecide_RetriesOnceScheduledTimeArrives(t *testing.T) {
	now := time.Now()
	prev := Tracked{NextAttempt: now.Add(-time.Second)} // just past
	if got := Decide(realBannerInline, prev, now); got != ActResume {
		t.Errorf("got %v, want ActResume (NextAttempt already past)", got)
	}
}

// ---------- nextAttemptAfter ----------

func TestNextAttemptAfter_UsesParsedFutureOccurrence(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 11, 0, 0, 0, berlin)
	// Banner says "3am"; nearest-to-now is today's 3am which is in the past.
	// nextAttemptAfter must advance to tomorrow's 3am.
	reset := time.Date(2026, 5, 21, 3, 0, 0, 0, berlin)
	cfg := Config{MaxWait: 24 * time.Hour, Jitter: 30 * time.Second}
	got := nextAttemptAfter(&Banner{Reset: reset}, now, cfg)
	want := time.Date(2026, 5, 22, 3, 0, 30, 0, berlin)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNextAttemptAfter_CapsAtMaxWait(t *testing.T) {
	now := time.Date(2026, 5, 21, 11, 0, 0, 0, time.UTC)
	reset := now.Add(20 * time.Hour) // way past MaxWait
	cfg := Config{MaxWait: 5 * time.Hour, Jitter: 30 * time.Second}
	got := nextAttemptAfter(&Banner{Reset: reset}, now, cfg)
	wantCap := now.Add(5 * time.Hour)
	if !got.Equal(wantCap) {
		t.Errorf("got %v, want %v (capped at now+MaxWait)", got, wantCap)
	}
}

func TestNextAttemptAfter_FallbackWhenUnparseable(t *testing.T) {
	now := time.Now()
	cfg := Config{MaxWait: 5 * time.Hour, Jitter: 30 * time.Second}
	got := nextAttemptAfter(&Banner{}, now, cfg) // Reset is zero
	want := now.Add(5 * time.Hour)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v (fallback to now+MaxWait when no parsed time)", got, want)
	}
}

// ---------- parseReset ----------

func TestParseReset_NearestOccurrencePicked(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	// 11am Berlin; "3am" should resolve to TODAY's 3am (8h ago), not tomorrow's.
	now := time.Date(2026, 5, 21, 11, 0, 0, 0, berlin)
	got, explicit, err := parseReset("", "3am", "Europe/Berlin", now)
	if err != nil {
		t.Fatal(err)
	}
	if explicit {
		t.Error("clock-only banner must report explicit=false")
	}
	want := time.Date(2026, 5, 21, 3, 0, 0, 0, berlin)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v (3am at 11am should mean TODAY)", got, want)
	}
}

func TestParseReset_FuturePicked(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 1, 0, 0, 0, berlin)
	got, _, _ := parseReset("", "3am", "Europe/Berlin", now)
	want := time.Date(2026, 5, 21, 3, 0, 0, 0, berlin)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseReset_ExplicitDate(t *testing.T) {
	// Real banner: "out of extra usage · resets May 24, 2am (Europe/Berlin)"
	// captured on May 21. Must resolve to May 24, 02:00 in Europe/Berlin —
	// NOT the nearest-occurrence 2am.
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 23, 20, 0, 0, berlin)
	got, explicit, err := parseReset("May 24", "2am", "Europe/Berlin", now)
	if err != nil {
		t.Fatal(err)
	}
	if !explicit {
		t.Error("date-qualified banner must report explicit=true")
	}
	want := time.Date(2026, 5, 24, 2, 0, 0, 0, berlin)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseReset_Formats(t *testing.T) {
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	cases := map[string]int{"3am": 3, "3pm": 15, "12am": 0, "12pm": 12, "3:30 pm": 15}
	for in, wantHour := range cases {
		t.Run(in, func(t *testing.T) {
			got, _, err := parseReset("", in, "", now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Hour() != wantHour {
				t.Errorf("hour = %d, want %d", got.Hour(), wantHour)
			}
		})
	}
}

func TestDetect_DatedBanner(t *testing.T) {
	// End-to-end: real captured banner with explicit date.
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 23, 20, 0, 0, berlin)
	s := "  ⎿  You're out of extra usage · resets May 24, 2am (Europe/Berlin)"
	b := Detect(s, now)
	if b == nil {
		t.Fatal("expected detection of dated banner")
	}
	if !b.ResetExplicit {
		t.Error("expected ResetExplicit=true for dated banner")
	}
	want := time.Date(2026, 5, 24, 2, 0, 0, 0, berlin)
	if !b.Reset.Equal(want) {
		t.Errorf("Reset = %v, want %v", b.Reset, want)
	}
}

func TestNextAttemptAfter_TrustsExplicitFutureDate(t *testing.T) {
	// When the banner says "May 24, 2am" three days from now, schedule
	// for exactly that time — don't apply the MaxWait cap.
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 23, 20, 0, 0, berlin)
	reset := time.Date(2026, 5, 24, 2, 0, 0, 0, berlin)
	cfg := Config{MaxWait: 5 * time.Hour, Jitter: time.Second}
	got := nextAttemptAfter(&Banner{Reset: reset, ResetExplicit: true}, now, cfg)
	want := reset.Add(time.Second)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v (explicit future date must bypass MaxWait cap)", got, want)
	}
}

// ---------- hasToolPrefix ----------

func TestHasToolPrefix(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"prefix present", "  ⎿  You're out of extra usage", true},
		{"no prefix, plain text", "You're out of extra usage", false},
		{"prefix on previous line", "  ⎿  Reason: x\n  You're out of extra usage", false},
		{"empty line before", "\nYou're out of extra usage", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			idx := strings.Index(c.content, "You're")
			if idx < 0 {
				t.Fatal("fixture missing anchor")
			}
			if got := hasToolPrefix(c.content, idx); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// ---------- DetectAPIError ----------

// Real tmux capture from a Claude Code session that hit a 400 "Could not
// process image" error (the PNG at /tmp/screen_now.png was 22 bytes / corrupt).
// The ❯ at the end shows Claude returned control to the input prompt.
const realAPIErrorCapture = `  ⎿  Read ../../../../../../tmp/screen_now.png (22 bytes)
  ⎿  API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"Could not process image"},"request_id":"req_011CbPrGcwvFBwN2wXSA8gfu"}

✻ Cogitated for 3h 2m 39s · 3 shells still running

  3 tasks (1 done, 1 in progress, 1 open)
  ◼ Test _handle_icloud_password_prompt fix on live VM

──── nix-airtag-tracker ────
❯ `

func TestDetectAPIError_Real(t *testing.T) {
	got := DetectAPIError(realAPIErrorCapture)
	if got == nil {
		t.Fatal("expected non-nil for real API error capture")
	}
	if got.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", got.StatusCode)
	}
	if got.Message != "Could not process image" {
		t.Errorf("Message = %q, want %q", got.Message, "Could not process image")
	}
	if got.RequestID != "req_011CbPrGcwvFBwN2wXSA8gfu" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
}

func TestDetectAPIError_DifferentStatus(t *testing.T) {
	// 500-range errors should also be detected; it's the structural markers
	// (⎿ prefix + input prompt) that matter, not the specific HTTP code.
	s := "  ⎿  API Error: 500 {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Internal server error\"}}\n❯ "
	got := DetectAPIError(s)
	if got == nil {
		t.Fatal("expected detection for 500 error")
	}
	if got.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", got.StatusCode)
	}
}

// --- false-positive rejection ---

func TestDetectAPIError_NoPrefixRejected(t *testing.T) {
	// Claude talking about an API error in prose — no ⎿ prefix.
	s := "I received an API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"Could not process image\"}}\n❯ "
	if got := DetectAPIError(s); got != nil {
		t.Errorf("prose mention without ⎿ must not match, got %+v", got)
	}
}

func TestDetectAPIError_NoInputPromptRejected(t *testing.T) {
	// Error line present but session is still running (no lone ❯ line).
	s := "  ⎿  API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"Could not process image\"}}\n✻ Thinking..."
	if got := DetectAPIError(s); got != nil {
		t.Errorf("error without input prompt must not match, got %+v", got)
	}
}

func TestDetectAPIError_PickerPromptRejected(t *testing.T) {
	// ❯ followed by a digit+period is a picker option, not the input prompt.
	s := "  ⎿  API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"Could not process image\"}}\n  ❯ 1. Stop and wait\n  ❯ 2. Retry"
	if got := DetectAPIError(s); got != nil {
		t.Errorf("picker ❯ line must not satisfy input-prompt check, got %+v", got)
	}
}

func TestDetectAPIError_RecoveryContentAfterError(t *testing.T) {
	// An error followed by successful tool output means Claude recovered.
	// DetectAPIError should still fire IF the session is at the prompt — the
	// two structural guards (⎿ prefix + lone ❯) are what matter, not position.
	// Actual protection against stale scrollback is provided by the daemon's
	// cfg.Capture limit (10 lines), not by an offset threshold.
	s := "  ⎿  API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"old\"}}\n  ⎿  subsequent tool output\n❯ "
	// This CAN match — both error and prompt are visible, which is the signal.
	// The two-tick persistence in Tick() prevents acting on transient errors.
	_ = DetectAPIError(s) // just ensure it doesn't panic
}

func TestDetectAPIError_EmptyPromptNoError(t *testing.T) {
	// Normal idle session — input prompt is visible but no error.
	s := "$ ls\nfile.txt\n❯ "
	if got := DetectAPIError(s); got != nil {
		t.Errorf("idle session at prompt must not match, got %+v", got)
	}
}


func TestDetectAPIError_MostRecentErrorWins(t *testing.T) {
	// Two API errors in the recent window — the most recent one is returned.
	first := "  ⎿  API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"first\"}}\n"
	second := "  ⎿  API Error: 422 {\"type\":\"error\",\"error\":{\"message\":\"second\"}}\n"
	s := first + second + "❯ "
	got := DetectAPIError(s)
	if got == nil {
		t.Fatal("expected detection with two errors")
	}
	if got.StatusCode != 422 {
		t.Errorf("StatusCode = %d, want 422 (most recent)", got.StatusCode)
	}
}

func TestDetectAPIError_BannerAndErrorCoexist(t *testing.T) {
	// Both a usage-limit banner and an API error are visible.
	// DetectAPIError must still find the error; callers can decide priority.
	content := realBannerInline + "\n" + realAPIErrorCapture
	if got := DetectAPIError(content); got == nil {
		t.Error("expected detection even when banner is also present")
	}
}

// ---------- DecideCompact ----------

func TestDecideCompact_FirstSeen_NoAct(t *testing.T) {
	apiErr := &APIError{StatusCode: 400}
	prev := ErrorTracked{} // zero FirstSeen
	if DecideCompact(apiErr, prev, time.Now(), time.Minute) {
		t.Error("must not compact on first sighting (no persistence confirmation)")
	}
}

func TestDecideCompact_TooSoon_NoAct(t *testing.T) {
	now := time.Now()
	apiErr := &APIError{StatusCode: 400}
	prev := ErrorTracked{FirstSeen: now.Add(-30 * time.Second)} // only 30s ago
	if DecideCompact(apiErr, prev, now, time.Minute) {
		t.Error("must not compact if error seen for less than minAge")
	}
}

func TestDecideCompact_MinAgeMet_Act(t *testing.T) {
	now := time.Now()
	apiErr := &APIError{StatusCode: 400}
	prev := ErrorTracked{FirstSeen: now.Add(-2 * time.Minute)} // 2min ago
	if !DecideCompact(apiErr, prev, now, time.Minute) {
		t.Error("must compact once error has persisted for minAge")
	}
}

func TestDecideCompact_AlreadyActed_NoAct(t *testing.T) {
	now := time.Now()
	apiErr := &APIError{StatusCode: 400}
	prev := ErrorTracked{FirstSeen: now.Add(-5 * time.Minute), Acted: true}
	if DecideCompact(apiErr, prev, now, time.Minute) {
		t.Error("must not compact again if /compact already sent")
	}
}

func TestDecideCompact_NilError_NoAct(t *testing.T) {
	now := time.Now()
	prev := ErrorTracked{FirstSeen: now.Add(-5 * time.Minute)}
	if DecideCompact(nil, prev, now, time.Minute) {
		t.Error("nil APIError must never trigger compact")
	}
}

// ---------- inputPromptRE sanity ----------

func TestInputPromptRE_Matches(t *testing.T) {
	cases := []string{
		"❯ ",
		"❯",
		"❯  ",
		"❯ ",               // NBSP — actual Claude Code TUI output
		"line before\n❯ \nline after",
		"line before\n❯ \nline after", // NBSP variant
	}
	for _, s := range cases {
		if !inputPromptRE.MatchString(s) {
			t.Errorf("inputPromptRE should match %q", s)
		}
	}
}

func TestInputPromptRE_Rejects(t *testing.T) {
	cases := []string{
		"  ❯ 1. Stop and wait",   // indented picker option
		"❯ 1. option",            // picker option at line start
		"❯ something typed here", // user input in progress
	}
	for _, s := range cases {
		if inputPromptRE.MatchString(s) {
			t.Errorf("inputPromptRE must not match %q", s)
		}
	}
}

func TestDetectAPIError_NonBreakingSpaces(t *testing.T) {
	// Claude Code TUI uses NBSP (U+00A0) as padding between its markers
	// (⎿, ❯) and adjacent text. Go's \s is ASCII-only and misses NBSP.
	// This test guards against that regression.
	s := "⎿ API Error: 400 {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Could not process image\"}}\n❯ "
	if got := DetectAPIError(s); got == nil {
		t.Error("must detect API error when NBSP is used between TUI markers and text")
	}
}

func TestDetectAPIError_RealCapture(t *testing.T) {
	// realAPIErrorCapture uses regular spaces (test fixture); the NBSP test
	// above covers the real tmux encoding. Both must pass.
	if got := DetectAPIError(realAPIErrorCapture); got == nil {
		t.Error("must detect real API error capture (regular-space fixture)")
	}
}

// ---------- state persistence ----------

func TestSaveLoadState_Roundtrip(t *testing.T) {
	path := t.TempDir() + "/state.json"
	now := time.Now().UTC().Truncate(time.Second)
	in := State{
		"%5": {Session: "foo", Pane: "%5",
			Banner: "test banner", Reset: now.Add(time.Hour),
			FirstSeen: now, LastSeen: now, LastActed: now},
	}
	if err := SaveState(path, in); err != nil {
		t.Fatal(err)
	}
	out := LoadState(path)
	got, ok := out["%5"]
	if !ok {
		t.Fatal("entry missing after roundtrip")
	}
	if got.Session != "foo" || !got.LastActed.Equal(in["%5"].LastActed) {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}
