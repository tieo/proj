package daemon

import (
	"path/filepath"
	"testing"
)

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
