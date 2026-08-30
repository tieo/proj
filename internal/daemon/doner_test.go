package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tieo/proj/internal/config"
)

func TestHasTag(t *testing.T) {
	if !hasTag([]string{"go", "doner"}, DonerTag) {
		t.Error("a tagged project should read as tagged")
	}
	if hasTag([]string{"go", "tools"}, DonerTag) {
		t.Error("an untagged project should not")
	}
	if hasTag(nil, DonerTag) {
		t.Error("no tags is not tagged")
	}
}

// The grace measures silence, so a transcript written recently (a reply from
// the user, a job finishing) keeps the session out of reach of a nudge.
func TestTranscriptMTimeGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	grace := 5 * time.Minute
	now := time.Now()

	if now.Sub(transcriptMTime(path)) >= grace {
		t.Error("a transcript just written should be inside the grace")
	}
	old := now.Add(-2 * grace)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if now.Sub(transcriptMTime(path)) < grace {
		t.Error("a transcript quiet past the grace should be outside it")
	}
	// Unreadable reads as long ago, which the caller's other checks then gate.
	if !transcriptMTime(filepath.Join(t.TempDir(), "gone.jsonl")).IsZero() {
		t.Error("a missing transcript should report the zero time")
	}
}

func TestDonerGraceDefault(t *testing.T) {
	if got := (config.DonerConfig{}).GraceDuration(); got != config.DonerGraceDefault {
		t.Errorf("unset grace = %v, want the default %v", got, config.DonerGraceDefault)
	}
	if got := (config.DonerConfig{Grace: "90s"}).GraceDuration(); got != 90*time.Second {
		t.Errorf("grace = %v, want 90s", got)
	}
	if got := (config.DonerConfig{Grace: "nonsense"}).GraceDuration(); got != config.DonerGraceDefault {
		t.Errorf("unreadable grace = %v, want the default", got)
	}
}

func TestPruneDonerNudges(t *testing.T) {
	donerNudgedAt = map[string]time.Time{"alive": time.Now(), "gone": time.Now()}
	pruneDonerNudges(map[string]bool{"alive": true})
	if _, ok := donerNudgedAt["gone"]; ok {
		t.Error("a session that is gone should not keep its stamp")
	}
	if _, ok := donerNudgedAt["alive"]; !ok {
		t.Error("a live session should keep its stamp")
	}
	donerNudgedAt = map[string]time.Time{}
}

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
func TestBounced(t *testing.T) {
	now := time.Now()
	if !bounced(now.Add(-2*time.Minute), now.Add(-2*time.Minute+3*time.Second)) {
		t.Error("an instant reply is a bounce")
	}
	if bounced(now.Add(-2*time.Hour), now.Add(-10*time.Minute)) {
		t.Error("a session that went away and worked is not a bounce")
	}
	// Never nudged, or a transcript last written before the nudge landed: no
	// evidence either way, so nothing is counted against the session.
	if bounced(time.Time{}, now) {
		t.Error("a session that was never nudged cannot bounce")
	}
	if bounced(now.Add(-time.Minute), now.Add(-5*time.Minute)) {
		t.Error("a write older than the nudge is not an answer to it")
	}
}
