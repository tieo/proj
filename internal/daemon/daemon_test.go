package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tieo/proj/internal/config"
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

func TestDetect_SessionLimit(t *testing.T) {
	s := `  ⎿  You've hit your session limit · resets 7:10pm (Europe/Berlin)`
	if Detect(s, time.Now()) == nil {
		t.Fatal("session-limit banner not detected")
	}
}

// TestDetect_LimitBehindFeedbackPrompt guards the case that showed a stuck
// session as healthy: Claude hit its session limit, then rendered its
// post-session feedback prompt directly below the banner. The prompt's ● header
// counted as newer output, so the stale-banner guard discarded a live limit
// stall - the session read green and never auto-resumed. The banner must be
// detected despite the prompt, and the prompt must be recognised as dismissable.
func TestDetect_LimitBehindFeedbackPrompt(t *testing.T) {
	content := "  ⎿  You've hit your session limit · resets 5pm (Europe/Berlin)\n" +
		"     /upgrade to increase your usage limit.\n" +
		"● How is Claude doing this session? (optional)\n" +
		"  1: Bad    2: Fine   3: Good   0: Dismiss\n" +
		"──── devtools @lwenb4004 [vscode] ──\n" +
		"❯ \n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents\n"
	if Detect(content, time.Now()) == nil {
		t.Error("usage-limit banner must be detected even with the feedback prompt below it")
	}
	if !feedbackPromptVisible(content) {
		t.Error("feedback prompt must be recognised so the daemon can dismiss it")
	}
}

func TestDetect_StillUsingCreditsIgnored(t *testing.T) {
	// Banner with the prefix but credits still active.
	s := "  ⎿  out of extra usage · resets 3pm; continuing with extra usage"
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
	// Two prefixed banners; last one wins.
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
	if got := Decide(Detect("just shell output\n$ ls\n", time.Now()), Tracked{}, time.Now()); got != ActNone {
		t.Errorf("got %v, want ActNone", got)
	}
}

func TestDecide_FutureResetWaits(t *testing.T) {
	// Reset known and still ahead: wait for it rather than burning a continue
	// that only earns another limit message.
	now := time.Now()
	if got := Decide(&Banner{Reset: now.Add(time.Hour)}, Tracked{}, now); got != ActWait {
		t.Errorf("got %v, want ActWait (reset 1h ahead)", got)
	}
}

func TestDecide_PastResetResumesImmediately(t *testing.T) {
	// Limit already expired: resume now.
	now := time.Now()
	if got := Decide(&Banner{Reset: now.Add(-time.Hour)}, Tracked{}, now); got != ActResume {
		t.Errorf("got %v, want ActResume (reset already passed)", got)
	}
}

func TestDecide_UnknownResetResumesImmediately(t *testing.T) {
	// No parseable reset time: fall back to an immediate resume, the hedge
	// against a limit whose reset the detector could not read.
	now := time.Now()
	if got := Decide(&Banner{}, Tracked{}, now); got != ActResume {
		t.Errorf("got %v, want ActResume (unknown reset)", got)
	}
}

func TestDecide_SelectorDoesNotChangeClassification(t *testing.T) {
	// Selector dismissal happens in Tick, outside Decide; the banner's reset
	// time alone drives Resume/Wait, with or without a selector present.
	now := time.Now()
	base := Decide(Detect(realBannerInline, now), Tracked{}, now)
	withSel := Decide(Detect(realBannerInline+"\n"+realSelector, now), Tracked{}, now)
	if base != withSel {
		t.Errorf("selector changed classification: %v vs %v", base, withSel)
	}
}

// Real capture from a resume-old-session prompt. The daemon must NOT touch
// this: Escape cancels the resume, and choosing summary vs full is the user's
// call. Acting on it interrupted the user and cancelled their resume.
const realResumePicker = `  This session is 2d 21h old and 126.3k tokens.

  Resuming the full session will consume a substantial portion of your usage limits. We recommend resuming from a summary.

  ❯ 1. Resume from summary (recommended)
    2. Resume full session as-is
    3. Don't ask me again

  Enter to confirm · Esc to cancel`

func TestHasSelector_ResumePickerIgnored(t *testing.T) {
	if HasSelector(realResumePicker) {
		t.Error("resume-from-summary dialog must not be treated as a dismissable selector")
	}
}

// A rate-limit picker quoted inside the user's input box: the live input box's
// hint line sits below it, so it must not be dismissed.
const pastedPickerInInput = `❯ here is the thing it kept showing me:
  What do you want to do?
  ❯ 1. Stop and wait for limit to reset
    2. Upgrade your plan
  Enter to confirm · Esc to cancel
  and that is the bug
──────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

func TestHasSelector_PastedIntoInputRejected(t *testing.T) {
	if HasSelector(pastedPickerInInput) {
		t.Error("picker text pasted into the input box must not be treated as a live selector")
	}
	// The same phrase as a genuine live overlay (no input box below) still counts.
	if !HasSelector(realSelector) {
		t.Error("a real picker overlay must still be recognized")
	}
}

// The exact class of capture the user reported: a message being composed that
// quotes Claude TUI output (API errors, the resume dialog), with the live input
// box and its hint at the very bottom. The daemon must take no action on any of it.
const composedMessageQuotingTUI = "● Background command \"Scan PCO bus\" completed (exit code 0)\n" +
	"  ⎿  API Error: 400 messages.41.content.7: `thinking` or `redacted_thinking` blocks in the latest assistant message cannot be modified.\n" +
	"✻ Churned for 0s\n" +
	"❯ hello?\n" +
	"  ⎿  API Error: 400 messages.41.content.7: `thinking` blocks cannot be modified.\n" +
	"❯ /compact\n" +
	"  ⎿  Error: Error during compaction: API Error: 400 messages.41.content.7: thinking blocks cannot be modified.\n" +
	"  This session is 4d 16h old and 376.2k tokens.\n" +
	"  Resuming the full session will consume a substantial portion of your usage limits. We recommend resuming from a summary.\n" +
	"  ❯ 1. Resume from summary (recommended)\n" +
	"    2. Resume full session as-is\n" +
	"    3. Don't ask me again\n" +
	"  Enter to confirm · Esc to cancel\n" +
	"──────── proj ──❯ also right now i am not able to send this previous message because the daemon constantly presses esc\n" +
	"  a string\n" +
	"────────\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle)"

func TestNoActionOnComposedMessage(t *testing.T) {
	if HasSelector(composedMessageQuotingTUI) {
		t.Error("picker text quoted in the input box must not be flagged as a selector")
	}
	if b := Detect(composedMessageQuotingTUI, time.Now()); b != nil {
		t.Errorf("quoted TUI output must not be flagged as a banner, got %+v", b)
	}
	if e := DetectAPIError(composedMessageQuotingTUI); e != nil {
		t.Errorf("quoted TUI output must not be flagged as an API error, got %+v", e)
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
	// Just the phrase mentioned in prose; no "❯ 1." option line.
	s := "Earlier I saw the prompt that says 'Stop and wait for limit to reset' and dismissed it."
	if HasSelector(s) {
		t.Error("phrase-only mention must not be flagged without the option-line structure")
	}
}

func TestDecide_WaitWhenRetryScheduled(t *testing.T) {
	now := time.Now()
	prev := Tracked{NextAttempt: now.Add(2 * time.Hour)}
	if got := Decide(Detect(realBannerInline, now), prev, now); got != ActWait {
		t.Errorf("got %v, want ActWait (retry scheduled 2h out)", got)
	}
}

func TestDecide_RetriesOnceScheduledTimeArrives(t *testing.T) {
	now := time.Now()
	// Reset reached: banner reset and the scheduled retry both just past.
	prev := Tracked{NextAttempt: now.Add(-time.Second)}
	if got := Decide(&Banner{Reset: now.Add(-time.Second)}, prev, now); got != ActResume {
		t.Errorf("got %v, want ActResume (reset reached)", got)
	}
}

// ---------- nextAttemptAfter ----------

func TestNextAttemptAfter_UsesParsedFutureOccurrence(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 11, 0, 0, 0, berlin)
	// Banner says "3am"; nearest-to-now is today's 3am which is in the past.
	// nextAttemptAfter must advance to tomorrow's 3am, plus a random offset
	// derived from the wait so deferred clients don't stack on the reset
	// minute. The jitter ceiling is wait/jitterFraction, capped at jitterMax.
	reset := time.Date(2026, 5, 21, 3, 0, 0, 0, berlin)
	cfg := Config{MaxWait: 24 * time.Hour}
	wantBase := time.Date(2026, 5, 22, 3, 0, 0, 0, berlin)
	got := nextAttemptAfter(&Banner{Reset: reset}, now, cfg)
	maxJ := jitterMax
	if got.Before(wantBase) || got.After(wantBase.Add(maxJ)) {
		t.Errorf("got %v, want in [%v, %v]", got, wantBase, wantBase.Add(maxJ))
	}
}

func TestNextAttemptAfter_FallbackWhenUnparseable(t *testing.T) {
	now := time.Now()
	cfg := Config{MaxWait: 5 * time.Hour}
	got := nextAttemptAfter(&Banner{}, now, cfg) // Reset is zero
	wantBase := now.Add(cfg.MaxWait)
	maxJ := jitterMax
	if got.Before(wantBase) || got.After(wantBase.Add(maxJ)) {
		t.Errorf("got %v, want in [%v, %v] (fallback to now+MaxWait+jitter)", got, wantBase, wantBase.Add(maxJ))
	}
}

// ---------- parseReset ----------

func TestParseReset_NearestOccurrencePicked(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	// 11am Berlin; "3am" should resolve to TODAY's 3am (8h ago), not tomorrow's.
	now := time.Date(2026, 5, 21, 11, 0, 0, 0, berlin)
	got, err := parseReset("", "3am", "Europe/Berlin", now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 5, 21, 3, 0, 0, 0, berlin)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v (3am at 11am should mean TODAY)", got, want)
	}
}

func TestParseReset_FuturePicked(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 1, 0, 0, 0, berlin)
	got, _ := parseReset("", "3am", "Europe/Berlin", now)
	want := time.Date(2026, 5, 21, 3, 0, 0, 0, berlin)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseReset_ExplicitDate(t *testing.T) {
	// Real banner: "out of extra usage · resets May 24, 2am (Europe/Berlin)"
	// captured on May 21. Must resolve to May 24, 02:00 in Europe/Berlin -
	// NOT the nearest-occurrence 2am.
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 23, 20, 0, 0, berlin)
	got, err := parseReset("May 24", "2am", "Europe/Berlin", now)
	if err != nil {
		t.Fatal(err)
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
			got, err := parseReset("", in, "", now)
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
	want := time.Date(2026, 5, 24, 2, 0, 0, 0, berlin)
	if !b.Reset.Equal(want) {
		t.Errorf("Reset = %v, want %v", b.Reset, want)
	}
}

func TestNextAttemptAfter_TrustsFutureDate(t *testing.T) {
	// When the banner says "May 24, 2am" three days from now, schedule
	// for that reset plus jitter derived from the wait; trust the parsed
	// reset, no cap.
	berlin := mustLoad(t, "Europe/Berlin")
	now := time.Date(2026, 5, 21, 23, 20, 0, 0, berlin)
	reset := time.Date(2026, 5, 24, 2, 0, 0, 0, berlin)
	cfg := Config{MaxWait: 5 * time.Hour}
	got := nextAttemptAfter(&Banner{Reset: reset}, now, cfg)
	maxJ := jitterMax
	if got.Before(reset) || got.After(reset.Add(maxJ)) {
		t.Errorf("got %v, want in [%v, %v] (future date must be trusted, not capped)", got, reset, reset.Add(maxJ))
	}
}

func TestNextAttemptAfter_BackoffOverridesReset(t *testing.T) {
	// Transient banners (Backoff > 0) ignore Reset and schedule a short retry
	// from now + Backoff + jitter. They override the long usage-reset defer.
	now := time.Date(2026, 5, 21, 11, 0, 0, 0, time.UTC)
	cfg := Config{MaxWait: 24 * time.Hour}
	backoff := 60 * time.Second
	got := nextAttemptAfter(&Banner{Backoff: backoff}, now, cfg)
	wantBase := now.Add(backoff)
	maxJ := jitterMax
	if got.Before(wantBase) || got.After(wantBase.Add(maxJ)) {
		t.Errorf("got %v, want in [%v, %v]", got, wantBase, wantBase.Add(maxJ))
	}
}

func TestTransientBackoff_Escalates(t *testing.T) {
	base := 60 * time.Second
	max := 5 * time.Hour
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 60 * time.Second},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
	}
	for _, c := range cases {
		if got := transientBackoff(base, c.attempts, max); got != c.want {
			t.Errorf("attempts=%d: got %v, want %v", c.attempts, got, c.want)
		}
	}
	// Never exceeds the cap however many attempts pile up.
	if got := transientBackoff(base, 100, max); got != max {
		t.Errorf("saturated: got %v, want cap %v", got, max)
	}
}

func TestJitter_InRange(t *testing.T) {
	for i := 0; i < 100; i++ {
		if got := jitter(); got < 0 || got >= jitterMax {
			t.Fatalf("got %v, want in [0, %v)", got, jitterMax)
		}
	}
}

func TestDetect_TransientPattern(t *testing.T) {
	content := "  ⎿  API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited"
	b := Detect(content, time.Now())
	if b == nil {
		t.Fatal("expected transient banner, got nil")
	}
	if b.Backoff <= 0 {
		t.Errorf("transient banner must carry a Backoff, got %v", b.Backoff)
	}
}

// prompt is Claude Code's live input line: "> " where the space is U+00A0.
const prompt = "> \n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"

func TestDetect_ConnDropStalledResumes(t *testing.T) {
	content := "● Now rewrite cli.py: run, ps, down. Writing locally.\n\n" +
		"● API Error: Connection closed mid-response. The response above may be incomplete.\n\n" +
		"✻ Cogitated for 2m 27s\n\n" + prompt
	b := Detect(content, time.Now())
	if b == nil {
		t.Fatal("stalled conn-drop should be detected")
	}
	if b.Backoff <= 0 {
		t.Errorf("conn-drop must carry a Backoff, got %v", b.Backoff)
	}
}

func TestDetect_BulletRateLimitStalledResumes(t *testing.T) {
	// The transient gateway rate limit, rendered as a ● assistant bullet (not ⎿).
	// transientPattern requires ⎿ and misses this; bulletErrorRE must catch it.
	// This is the real virtmc stall.
	content := "● API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited\n\n" +
		"✻ Brewed for 30s · 5 shells still running\n\n" + prompt
	b := Detect(content, time.Now())
	if b == nil {
		t.Fatal("● -rendered transient rate limit should be detected")
	}
	if b.Backoff <= 0 {
		t.Errorf("must carry a Backoff, got %v", b.Backoff)
	}
}

func TestDetect_BulletOverloadedStalledResumes(t *testing.T) {
	// 529 server overload, ● bullet, no JSON → apiErrorRE/transientPattern miss
	// it. The real virtmc stall. bulletErrorRE must catch it.
	content := "● API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment.\n\n" + prompt
	b := Detect(content, time.Now())
	if b == nil {
		t.Fatal("● -rendered 529 Overloaded should be detected")
	}
	if b.Backoff <= 0 {
		t.Errorf("must carry a Backoff, got %v", b.Backoff)
	}
}

func TestDetect_ConnDropAlreadyResumedIgnored(t *testing.T) {
	// Newer ● output below the error → Claude resumed; must not re-fire.
	content := "● API Error: Connection closed mid-response.\n\n" +
		"● Bringing up BTG router + appl stack on virtmc.\n\n" + prompt
	if b := Detect(content, time.Now()); b != nil {
		t.Errorf("resumed session must not match, got %+v", b)
	}
}

func TestDetect_ConnDropBusyIgnored(t *testing.T) {
	// Active spinner ("… (timer") after the error → still generating.
	content := "● API Error: Connection closed mid-response.\n\n" +
		"· Whisking… (2m 42s · ↓ 9.5k tokens)\n\n" + prompt
	if b := Detect(content, time.Now()); b != nil {
		t.Errorf("busy session must not match, got %+v", b)
	}
}

func TestDetect_ConnDropNoPromptIgnored(t *testing.T) {
	// No live input prompt → cannot confirm idle.
	content := "● API Error: Connection closed mid-response.\n✻ Cogitated for 1m\n"
	if b := Detect(content, time.Now()); b != nil {
		t.Errorf("without a prompt must not match, got %+v", b)
	}
}

func TestDetect_ConnDropBelowPromptIgnored(t *testing.T) {
	// Error pasted into the input buffer (below the live prompt).
	content := prompt + "● API Error: Connection closed mid-response.\n"
	if b := Detect(content, time.Now()); b != nil {
		t.Errorf("error below the prompt must not match, got %+v", b)
	}
}

func TestDetect_ConnDropProseIgnored(t *testing.T) {
	// Phrase without the ● assistant bullet (prose / tool output / source).
	content := "the API Error: Connection closed mid-response happened earlier\n" + prompt
	if b := Detect(content, time.Now()); b != nil {
		t.Errorf("prose mention must not match, got %+v", b)
	}
}

func TestDetect_BannerWithNewerOutputIgnored(t *testing.T) {
	// Real ⎿ banner, but newer ● assistant output follows it: scrollback / a
	// quoted fixture (how the proj pane self-flagged "out of tokens"), not a
	// live stall. Must not match.
	content := realBannerInline + "\n\n" +
		"● Got it, continuing the refactor.\n\n" + prompt
	if b := Detect(content, time.Now()); b != nil {
		t.Errorf("banner with newer output must not match, got %+v", b)
	}
}

func TestDetect_BannerAsLatestStillMatches(t *testing.T) {
	// Same banner, nothing newer after it → still a live stall → detected.
	content := "● Working on it.\n\n" + realBannerInline + "\n\n" + prompt
	if Detect(content, time.Now()) == nil {
		t.Fatal("banner as the latest output must still be detected")
	}
}

func TestDecide_TransientCooldown(t *testing.T) {
	content := "  ⎿  API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited"
	now := time.Now()
	// Just acted: should wait through the backoff before retrying.
	prev := Tracked{LastActed: now.Add(-10 * time.Second)}
	if got := Decide(Detect(content, now), prev, now); got != ActWait {
		t.Errorf("within backoff: got %v, want ActWait", got)
	}
	// Acted longer ago than the backoff window: should retry.
	prev = Tracked{LastActed: now.Add(-2 * time.Minute)}
	if got := Decide(Detect(content, now), prev, now); got != ActResume {
		t.Errorf("past backoff: got %v, want ActResume", got)
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
	// Claude talking about an API error in prose; no ⎿ prefix.
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
	// DetectAPIError should still fire IF the session is at the prompt; the
	// two structural guards (⎿ prefix + lone ❯) are what matter, not position.
	// Actual protection against stale scrollback is provided by the daemon's
	// cfg.Capture limit (10 lines), not by an offset threshold.
	s := "  ⎿  API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"old\"}}\n  ⎿  subsequent tool output\n❯ "
	// This CAN match; both error and prompt are visible, which is the signal.
	// The two-tick persistence in Tick() prevents acting on transient errors.
	_ = DetectAPIError(s) // just ensure it doesn't panic
}

func TestDetectAPIError_EmptyPromptNoError(t *testing.T) {
	// Normal idle session; input prompt is visible but no error.
	s := "$ ls\nfile.txt\n❯ "
	if got := DetectAPIError(s); got != nil {
		t.Errorf("idle session at prompt must not match, got %+v", got)
	}
}

func TestDetectAPIError_MostRecentErrorWins(t *testing.T) {
	// Two API errors in the recent window; the most recent one is returned.
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

func TestDetectAPIError_PastedBelowPromptRejected(t *testing.T) {
	// The user pastes an API error into the input box; it renders as indented
	// continuation lines below the live ❯ prompt. It must not be treated as a
	// real, current error. Arbitrary unrelated lines sit between, so position
	// (not distance) is what we check.
	s := "❯ look at what it kept showing me earlier:\n" +
		strings.Repeat("  some other quoted line\n", 40) +
		"  ⎿  API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"Could not process image\"}}\n" +
		"  and that is the whole problem"
	if got := DetectAPIError(s); got != nil {
		t.Errorf("an API error quoted in the input buffer must not match, got %+v", got)
	}
}

func TestDetectAPIError_LargeGapAbovePromptStillDetected(t *testing.T) {
	// A real error far above the prompt (many lines of retry output between)
	// must still be detected: the boundary is the prompt's position, not a
	// fixed byte distance.
	s := "  ⎿  API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"Could not process image\"}}\n" +
		strings.Repeat("  ⎿  retrying tool output line\n", 60) +
		"──── proj ────\n❯ "
	if got := DetectAPIError(s); got == nil {
		t.Error("a real error far above the prompt must still be detected")
	}
}

func TestDetectAPIError_RealAboveBeatsPastedBelow(t *testing.T) {
	// A genuine error above the prompt plus a quoted one in the input below:
	// the real (above-prompt) error must win, not the pasted one.
	s := "  ⎿  API Error: 500 {\"type\":\"error\",\"error\":{\"message\":\"real one\"}}\n" +
		"❯ here is what i saw and pasted:\n" +
		"  ⎿  API Error: 400 {\"type\":\"error\",\"error\":{\"message\":\"pasted\"}}\n"
	got := DetectAPIError(s)
	if got == nil || got.StatusCode != 500 {
		t.Errorf("must detect the real error above the prompt, not the pasted one below; got %+v", got)
	}
}

func TestDetectAPIError_EscapedJSONFixtureRejected(t *testing.T) {
	// The actual incident this guard fixes: the daemon's own test file was on
	// screen while being edited. Its fixtures embed `⎿  API Error:` lines as Go
	// string literals, so the rendered JSON is backslash-escaped and does not
	// parse. Such a line above the live prompt must not trigger a recovery.
	s := "  ⎿  API Error: 400 {\\\"type\\\":\\\"error\\\",\\\"error\\\":{\\\"message\\\":\\\"old\\\"}}\n" +
		"──── proj ────\n❯ "
	if got := DetectAPIError(s); got != nil {
		t.Errorf("a backslash-escaped JSON literal (source on screen) must not match, got %+v", got)
	}
}

func TestDetectAPIError_SkipsMalformedKeepsRealAbove(t *testing.T) {
	// A malformed match (escaped source literal) nearer the prompt must not mask
	// a genuine, parseable error further up: the loop keeps scanning past it.
	s := "  ⎿  API Error: 500 {\"type\":\"error\",\"error\":{\"message\":\"real\"}}\n" +
		strings.Repeat("  some line\n", 5) +
		"  ⎿  API Error: 400 {\\\"escaped\\\":\\\"junk\\\"}\n" +
		"❯ "
	got := DetectAPIError(s)
	if got == nil || got.StatusCode != 500 || got.Message != "real" {
		t.Errorf("malformed match nearer the prompt must not mask the real error above it; got %+v", got)
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
		"❯ ", // NBSP; actual Claude Code TUI output
		"line before\n❯ \nline after",
		"line before\n❯ \nline after", // NBSP variant
		"❯ commit this",               // text in input buffer; session still idle
		"❯ some pending text",
	}
	for _, s := range cases {
		if !inputPromptRE.MatchString(s) {
			t.Errorf("inputPromptRE should match %q", s)
		}
	}
}

func TestInputPromptRE_Rejects(t *testing.T) {
	cases := []string{
		"  ❯ 1. Stop and wait", // indented picker option (TUI always indents these)
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

func TestDetectAPIError_CurrentPromptGlyph(t *testing.T) {
	// The live input prompt is "> " with a non-breaking space (U+00A0), not the
	// old "❯". The idle gate must accept it, or DetectAPIError returns nil in
	// production and the compact/clear recovery path never fires.
	s := "⎿ API Error: 500 {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"overloaded\"}}\n> \n"
	if got := DetectAPIError(s); got == nil {
		t.Error("must detect API error with the current \"> \" (NBSP) prompt glyph")
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

func TestSaveLoadManagedState_RCEverActivePersists(t *testing.T) {
	// The RC watchdog's in-memory rcEverActive is wiped on restart; the
	// persisted mirror must survive so a session that bound before a restart
	// is still recoverable after it.
	path := t.TempDir() + "/daemon.json"
	in := ManagedState{
		"virtmc@big_projects+qemu": {Name: "virtmc@big_projects+qemu", RCEverActive: true},
		"proj@go+tools":            {Name: "proj@go+tools", RCEverActive: false},
	}
	if err := SaveManagedState(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadManagedState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !out["virtmc@big_projects+qemu"].RCEverActive {
		t.Error("RCEverActive=true must survive the roundtrip")
	}
	if out["proj@go+tools"].RCEverActive {
		t.Error("RCEverActive=false must stay false")
	}
}

func TestRCName(t *testing.T) {
	cases := map[string]string{
		"proj@go+tools":            "proj @myhost [go,tools]",
		"virtmc@big_projects+qemu": "virtmc @myhost [big_projects,qemu]",
		"TagHistory@Android":       "TagHistory @myhost [android]",
		"proj@Go":                  "proj @myhost [go]",
		"solo":                     "solo @myhost",
	}
	for in, want := range cases {
		if got := RCName(in, "myhost"); got != want {
			t.Errorf("RCName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRCWatchdog_Detection(t *testing.T) {
	// wouldNudge replays the watchdog gate using the TUI zone (⏵⏵ line + context
	// line above it): fire only when the zone is present and lacks an RC marker.
	wouldNudge := func(content string) bool {
		zone, ok := rcTUIZone(content)
		return ok && !rcActiveRE.MatchString(zone)
	}

	// Fully bound: "Remote Control active" on the ⏵⏵ status line.
	live := "❯ \n  [CAVEMAN]\n  ⏵⏵ bypass permissions on (shift+tab to cycle)          Remote Control active"
	zone, ok := rcTUIZone(live)
	if !ok {
		t.Fatal("TUI zone not found on a live pane")
	}
	if !rcActiveRE.MatchString(zone) {
		t.Error("should detect active RC marker in the TUI zone")
	}
	if wouldNudge(live) {
		t.Error("watchdog must NOT nudge a pane that already has RC")
	}

	// Connecting state: "/rc active" on the session-context (CAVEMAN) line above
	// ⏵⏵. This is the intermediate state during --remote-control auto-bind.
	// The watchdog must treat it as "RC in progress" and not nudge.
	connecting := "❯ \n  [CAVEMAN]                                    /rc active\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	if wouldNudge(connecting) {
		t.Error("watchdog must NOT nudge a pane with /rc active on the context line (connecting state)")
	}

	// RC dropped: status line present, no marker anywhere in the TUI zone.
	dropped := "❯ \n  [CAVEMAN]\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	if _, ok := rcTUIZone(dropped); !ok {
		t.Error("dropped pane is still a live claude TUI (status line present)")
	}
	if !wouldNudge(dropped) {
		t.Error("watchdog should nudge a live pane missing RC")
	}

	// Spoof guard (false negative): user typed "/rc active?" into the input box
	// but RC is genuinely dropped. ❯ line is above the TUI zone, so the phrase
	// must not suppress the nudge.
	spoofInput := "❯ is /rc active?\n  [CAVEMAN]\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	if !wouldNudge(spoofInput) {
		t.Error("phrase in the input box must not suppress a needed nudge")
	}

	// Spoof guard (false negative): tool output mentions the marker while RC is
	// dropped. Output is above the zone and must not suppress the nudge.
	spoofOutput := "⎿ logs: Remote Control active earlier\n  [CAVEMAN]\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	if !wouldNudge(spoofOutput) {
		t.Error("phrase in tool output must not suppress a needed nudge")
	}

	// Spoof guard (false positive): "/rc failed" in prose above the zone while
	// RC is actually bound. rcFailedRE must only fire from the ⏵⏵ status line.
	failedInProse := "⎿ note: /rc failed last week\n  [CAVEMAN]\n  ⏵⏵ bypass permissions on (shift+tab to cycle)          Remote Control active"
	st, _ := rcStatusLine(failedInProse)
	if rcFailedRE.MatchString(st) {
		t.Error("'/rc failed' in prose must not be read off the status line")
	}
	if wouldNudge(failedInProse) {
		t.Error("watchdog must NOT nudge a bound pane despite '/rc failed' in output")
	}

	// Real /rc-failed indicator on the status line is still detected.
	if !rcFailedRE.MatchString("  ⏵⏵ bypass permissions on (shift+tab to cycle)   /rc failed") {
		t.Error("should detect a genuine /rc failed marker on the status line")
	}

	// Default-mode pane (no ⏵⏵, "? for shortcuts" instead) is a live TUI too.
	def := "❯ \n  ? for shortcuts"
	if _, ok := rcTUIZone(def); !ok {
		t.Error("default-mode status line should be recognized")
	}

	// A plain shell (no status line) is not a live claude pane: never nudge.
	if _, ok := rcTUIZone("user@host:~$ ls\nfile.txt"); ok {
		t.Error("a plain shell must not be treated as a live claude pane")
	}
}

func TestRCPicker(t *testing.T) {
	// The /rc binding dialog, as captured from a real pane.
	picker := "  Remote Control\n\n  This session is available via Remote Control at\n" +
		"  https://claude.ai/code/session_01P5\n\n    Disconnect this session\n" +
		"    Show QR code\n  ❯ Continue\n\n  Enter to select · Esc to continue"

	if !rcPickerRE.MatchString(picker) {
		t.Error("should recognize the /rc binding dialog")
	}
	// HasSelector cannot see it (no "❯ <digit>." option), which is exactly why
	// it needs its own Enter-sending branch (not the Escape branch).
	if HasSelector(picker) {
		t.Error("HasSelector must not match the RC dialog (it has no numbered option)")
	}
	// Watchdog must NOT re-send /rc while the dialog is already open - the
	// 1b-pre Enter branch is handling it. Replays the full gate.
	status, ok := rcStatusLine(picker)
	wouldNudge := ok && !rcActiveRE.MatchString(status) &&
		!HasSelector(picker) && !rcPickerRE.MatchString(picker)
	if wouldNudge {
		t.Error("watchdog must not re-send /rc while the RC dialog is open")
	}
	// Prose mentioning one label alone must not trip the matcher.
	if rcPickerRE.MatchString("you can Disconnect this session whenever you like") {
		t.Error("a single label in prose must not match the RC dialog")
	}
}

// ---------- managed state: corruption and concurrent writes ----------

// TestLoadManagedState_MissingVsCorrupt separates the two cases the old code
// collapsed into "empty state": a missing file is a clean first run, a corrupt
// one is an error. Collapsing them let a single bad file erase every pin, since
// the daemon promptly saved the empty map back over it.
func TestLoadManagedState_MissingVsCorrupt(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "daemon.json")
	got, err := LoadManagedState(missing)
	if err != nil || len(got) != 0 {
		t.Errorf("missing file: want empty state and no error, got %v, %v", got, err)
	}

	corrupt := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(managedStatePath(corrupt), []byte(`{"a": {trunc`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManagedState(corrupt); err == nil {
		t.Error("corrupt file must be an error, not an empty state")
	}
}

// TestMergeManaged_ConcurrentPinSurvivesTick is the lost update that unpinned a
// session: the daemon reads state, spends a tick launching sessions, and writes
// its stale map back. A `proj pin` landing inside that window must survive.
func TestMergeManaged_ConcurrentPinSurvivesTick(t *testing.T) {
	base := ManagedState{"tldr@Python": {Name: "tldr@Python"}}
	// The daemon's copy: it saw the session alive and stamped it, no pin.
	ours := ManagedState{"tldr@Python": {Name: "tldr@Python", SeenAt: time.Unix(100, 0)}}
	// Meanwhile `proj pin tldr` wrote the pin to disk.
	theirs := ManagedState{"tldr@Python": {Name: "tldr@Python", Pinned: true}}

	out := mergeManaged(base, ours, theirs)
	if !out["tldr@Python"].Pinned {
		t.Error("a pin written during a tick must survive the daemon's write-back")
	}
	if !out["tldr@Python"].SeenAt.Equal(time.Unix(100, 0)) {
		t.Error("the daemon's own bookkeeping (SeenAt) must still be applied")
	}
}

// TestMergeManaged_ConcurrentPinSurvivesDelete covers the same race against the
// daemon's delete path: it decided the dead session was unpinned and dropped it,
// while a pin landed on disk. The pin wins; dropping it would lose the session.
func TestMergeManaged_ConcurrentPinSurvivesDelete(t *testing.T) {
	base := ManagedState{"tldr@Python": {Name: "tldr@Python"}}
	ours := ManagedState{} // daemon dropped it
	theirs := ManagedState{"tldr@Python": {Name: "tldr@Python", Pinned: true}}

	if out := mergeManaged(base, ours, theirs); !out["tldr@Python"].Pinned {
		t.Error("a pin racing the daemon's delete must survive")
	}
}

// TestMergeManaged_UncontestedDeleteApplies keeps the delete path working: when
// nobody else touched the entry, the daemon's drop must actually take effect.
func TestMergeManaged_UncontestedDeleteApplies(t *testing.T) {
	entry := ManagedSession{Name: "gone@x"}
	base := ManagedState{"gone@x": entry}
	ours := ManagedState{}
	theirs := ManagedState{"gone@x": entry}

	if out := mergeManaged(base, ours, theirs); len(out) != 0 {
		t.Errorf("uncontested delete must apply, got %v", out)
	}
}

// TestMergeManaged_DaemonAddSurvives: a session the daemon newly observed is not
// on disk yet and must be added rather than mistaken for someone else's delete.
func TestMergeManaged_DaemonAddSurvives(t *testing.T) {
	out := mergeManaged(ManagedState{}, ManagedState{"new@x": {Name: "new@x"}}, ManagedState{})
	if _, ok := out["new@x"]; !ok {
		t.Error("a session first seen this tick must be added")
	}
}

// TestUpdateManagedState_RefusesCorrupt: a CLI mutation must not write over a
// state file it could not parse, or `proj pin` becomes a pin-wipe.
func TestUpdateManagedState_RefusesCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	bad := []byte(`{"a": {trunc`)
	if err := os.WriteFile(managedStatePath(path), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	err := UpdateManagedState(path, func(ManagedState) error { called = true; return nil })
	if err == nil {
		t.Error("UpdateManagedState must fail on a corrupt state file")
	}
	if called {
		t.Error("the mutation must not run against a corrupt state")
	}
	after, _ := os.ReadFile(managedStatePath(path))
	if string(after) != string(bad) {
		t.Error("a corrupt state file must be left untouched for inspection")
	}
}

// TestUpdateManagedState_RoundTrip is the ordinary path: mutate under the lock,
// persist, read back.
func TestUpdateManagedState_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := UpdateManagedState(path, func(m ManagedState) error {
		m["tldr@Python"] = ManagedSession{Name: "tldr@Python", Pinned: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := LoadManagedState(path)
	if err != nil || !out["tldr@Python"].Pinned {
		t.Errorf("pin must persist, got %v, %v", out, err)
	}
}

// writeTranscript writes lines as a jsonl file and returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

const (
	limitErrRecord = `{"type":"user","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":"You've hit your session limit · resets 4:40pm (Europe/Berlin)"}}`
	// A background command finishing injects a role-"user" line that the user
	// never typed; it must not read as a resume.
	taskNotifyRecord = `{"type":"user","message":{"content":"<task-notification> <task-id>abc</task-id> done"}}`
	assistantRecord  = `{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"back to work"}]}}`
)

// TestDetectFromTranscript_InjectedUserAfterLimit guards the stall where a
// usage limit was followed only by a machine-injected role-"user" record (a
// background <task-notification>, a queued command, a tool result). The session
// is still stalled, so the banner must survive: dropping it here abandons the
// session and no continue is ever sent once the limit resets.
func TestDetectFromTranscript_InjectedUserAfterLimit(t *testing.T) {
	now := time.Now()
	if b := DetectFromTranscript(writeTranscript(t, limitErrRecord, taskNotifyRecord), now); b == nil {
		t.Error("injected user record after limit must not read as a resume; banner expected")
	}
	// A bare user turn (whatever its content) is not proof either.
	userTyped := `{"type":"user","message":{"content":"continue"}}`
	if b := DetectFromTranscript(writeTranscript(t, limitErrRecord, userTyped), now); b == nil {
		t.Error("a user turn alone must not read as a resume; typing does not clear a limit")
	}
}

// TestDetectFromTranscript_AssistantResumes confirms the one signal that does
// prove a resume: a real-model assistant reply after the error clears the banner.
func TestDetectFromTranscript_AssistantResumes(t *testing.T) {
	if b := DetectFromTranscript(writeTranscript(t, limitErrRecord, assistantRecord), time.Now()); b != nil {
		t.Errorf("assistant turn after limit means resumed; want nil, got %+v", b)
	}
}

// TestDetectFromTranscript_LimitLast keeps the base case: a limit error at the
// tail with nothing after it surfaces a banner.
func TestDetectFromTranscript_LimitLast(t *testing.T) {
	if b := DetectFromTranscript(writeTranscript(t, assistantRecord, limitErrRecord), time.Now()); b == nil {
		t.Error("trailing limit error must surface a banner")
	}
}

func TestHasTrustPrompt(t *testing.T) {
	content := `
 Accessing workspace:

 /tmp/claude-1000/-home-user-projects-code-proj/session/scratchpad/agytest

 Quick safety check: Is this a project you created or one you trust?

 ❯ 1. Yes, I trust this folder
   2. No, exit
`
	if !HasTrustPrompt(content) {
		t.Fatal("workspace trust prompt was not detected")
	}
	if HasTrustPrompt("❯ 1. Yes, I trust this folder\nplain prose") {
		t.Fatal("option text alone must not count")
	}
}

func TestAutoTrustPath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "code")
	project := filepath.Join(base, "proj")
	scratch := "/tmp/claude-1000/-home-user-projects-code-proj/session/scratchpad/agytest"
	otherTmp := "/tmp/other/scratchpad/agytest"
	if !autoTrustPath(base, project) {
		t.Fatal("project dir under base must be trusted")
	}
	if !autoTrustPath(base, scratch) {
		t.Fatal("proj scratchpad dir must be trusted")
	}
	if autoTrustPath(base, otherTmp) {
		t.Fatal("unrelated scratchpad dir must be left alone")
	}
	if autoTrustPath(base, filepath.Dir(base)) {
		t.Fatal("parent of base must be left alone")
	}
}

func TestRCEnabled(t *testing.T) {
	on := Config{Tools: map[string]config.ToolSpec{
		"claude": {Name: "claude", Command: "claude --dangerously-skip-permissions --remote-control --remote-control-session-name-prefix {name} -n {name}"},
	}}
	if !rcEnabled(on) {
		t.Error("rcEnabled should be true when --remote-control present")
	}
	off := Config{Tools: map[string]config.ToolSpec{
		"claude": {Name: "claude", Command: "claude --dangerously-skip-permissions -n {name}"},
	}}
	if rcEnabled(off) {
		t.Error("rcEnabled should be false without --remote-control")
	}
}
