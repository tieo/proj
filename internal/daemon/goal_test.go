package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tieo/proj/internal/goalnudge"
)

func TestEnforceOpenGoal(t *testing.T) {
	const cond = "ship the feature"
	cases := []struct {
		name        string
		v           goalnudge.Verdict
		cond        string
		wantState   string
		wantCallout string
	}{
		{"no goal keeps verdict", goalnudge.Verdict{State: "done", Callout: ""}, "", "done", ""},
		{"open goal + done -> stopped_short", goalnudge.Verdict{State: "done"}, cond, "stopped_short", ""},
		{"open goal + working -> stopped_short", goalnudge.Verdict{State: "working"}, cond, "stopped_short", ""},
		{"keeps judge callout", goalnudge.Verdict{State: "working", Callout: "resume the crawler"}, cond, "stopped_short", "resume the crawler"},
		{"needs_user is left to notify", goalnudge.Verdict{State: "blocked", NeedsUser: true}, cond, "blocked", ""},
		{"already stopped_short stays", goalnudge.Verdict{State: "stopped_short", Callout: "go"}, cond, "stopped_short", "go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, callout := enforceOpenGoal(c.v, c.cond)
			if state != c.wantState {
				t.Errorf("state = %q, want %q", state, c.wantState)
			}
			// A forced stopped_short with no judge callout must still carry one so
			// Decide does not drop the nudge.
			if c.wantCallout != "" && callout != c.wantCallout {
				t.Errorf("callout = %q, want %q", callout, c.wantCallout)
			}
			if state == "stopped_short" && callout == "" {
				t.Error("stopped_short must have a non-empty callout")
			}
			if c.cond != "" && !c.v.NeedsUser && callout == "" && !strings.Contains(callout, c.cond) {
				t.Error("synthesized callout should reference the goal")
			}
		})
	}
}

// goal_status attachment records as Claude Code's /goal writes them.
const (
	goalArmed     = `{"type":"attachment","uuid":"a","attachment":{"type":"goal_status","met":false,"sentinel":true,"condition":"ship it"}}`
	goalIterating = `{"type":"attachment","uuid":"b","attachment":{"type":"goal_status","met":false,"condition":"ship it","reason":"still working"}}`
	goalMet       = `{"type":"attachment","uuid":"c","attachment":{"type":"goal_status","met":true,"condition":"ship it"}}`
	goalFailed    = `{"type":"attachment","uuid":"d","attachment":{"type":"goal_status","met":false,"failed":true,"condition":"ship it"}}`
	// An assistant line that merely mentions the marker in prose or a tool call
	// must not read as a goal.
	goalProse = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"grep for \"type\":\"goal_status\" in the log"}]}}`
)

func TestSessionHasActiveGoal(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"no goal", []string{goalProse}, false},
		{"empty", nil, false},
		{"armed", []string{goalArmed}, true},
		{"armed then iterating", []string{goalArmed, goalIterating}, true},
		{"met closes it", []string{goalArmed, goalIterating, goalMet}, false},
		{"failed closes it", []string{goalArmed, goalFailed}, false},
		{"prose does not arm", []string{goalProse, goalProse}, false},
		{"re-armed after met", []string{goalMet, goalArmed}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sessionHasActiveGoal(writeTranscript(t, c.lines...)); got != c.want {
				t.Fatalf("sessionHasActiveGoal = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSessionHasActiveGoalMissingFile(t *testing.T) {
	if sessionHasActiveGoal(filepath.Join(t.TempDir(), "nope.jsonl")) {
		t.Fatal("missing transcript must read as no goal")
	}
}

func TestGoalBackstopGate(t *testing.T) {
	now := time.Now()
	// No open goal -> not goal-nudge's concern, regardless of age.
	noGoal := writeTranscript(t, goalProse)
	if g := goalBackstopGate(noGoal, now); g != goalGateNone {
		t.Errorf("no goal: got %v, want goalGateNone", g)
	}
	// Open goal, transcript just written (within grace) -> the goal may still
	// fire, so wait.
	armed := writeTranscript(t, goalArmed)
	touch(t, armed, now.Add(-goalFireGrace/2))
	if g := goalBackstopGate(armed, now); g != goalGateWait {
		t.Errorf("armed + recent write: got %v, want goalGateWait", g)
	}
	// Open goal, transcript quiet past the grace -> the goal did not fire, judge.
	touch(t, armed, now.Add(-2*goalFireGrace))
	if g := goalBackstopGate(armed, now); g != goalGateJudge {
		t.Errorf("armed + quiet: got %v, want goalGateJudge", g)
	}
	// A met goal is closed -> no concern even if recently written.
	met := writeTranscript(t, goalArmed, goalMet)
	touch(t, met, now.Add(-2*goalFireGrace))
	if g := goalBackstopGate(met, now); g != goalGateNone {
		t.Errorf("met goal: got %v, want goalGateNone", g)
	}
}

func touch(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}
