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

func TestDecide_BannerPlusSelector(t *testing.T) {
	if got := Decide(realBannerInline+"\n"+realSelector, Tracked{}, time.Now()); got != ActDismiss {
		t.Errorf("got %v, want ActDismiss", got)
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
