package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// request writes a transcript whose last request ran ago before now with
// context tokens, in place of the plain reply sweepWorld.say writes.
func (w *sweepWorld) request(t *testing.T, agentID, dir string, context int, ago time.Duration) {
	t.Helper()
	w.say(t, agentID, dir, "ok", ago)
	enc := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(dir)
	path := filepath.Join(w.claude, "projects", enc, "sess-"+agentID+".jsonl")
	line, _ := json.Marshal(map[string]any{
		"type":      "assistant",
		"timestamp": time.Now().Add(-ago).UTC().Format(time.RFC3339),
		"message": map[string]any{
			"model":   "claude-opus-5-5",
			"content": []map[string]string{{"type": "text", "text": "ok"}},
			"usage":   map[string]int{"input_tokens": 2, "cache_read_input_tokens": context},
		},
	})
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runIdleCompactOnce(t *testing.T) {
	t.Helper()
	idleCompactDryRun, idleCompactReport = false, false
	if err := runIdleCompact(idleCompactCmd, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIdleCompactSendsCompactOnceToALargeSessionAboutToLoseItsCache(t *testing.T) {
	w := newSweepWorld(t, []map[string]string{
		agent("big", "big", "idle", "/p/big"),
		agent("small", "small", "idle", "/p/small"),
		agent("fresh", "fresh", "idle", "/p/fresh"),
		agent("busy", "busy", "running", "/p/busy"),
		agent("cold", "cold", "idle", "/p/cold"),
	}, nil)
	w.request(t, "big", "/p/big", 400000, 56*time.Minute)
	w.request(t, "small", "/p/small", 100000, 56*time.Minute)
	w.request(t, "fresh", "/p/fresh", 400000, 20*time.Minute)
	w.request(t, "busy", "/p/busy", 400000, 56*time.Minute)
	w.request(t, "cold", "/p/cold", 400000, 90*time.Minute)

	runIdleCompactOnce(t)
	if got := w.nudges(t); len(got) != 1 || got[0] != "/compact" {
		t.Fatalf("sent %v, want one /compact", got)
	}
	log := loadCompactions()
	if len(log) != 1 || log[0].Project != "big" || log[0].Context != 400002 {
		t.Fatalf("logged %+v", log)
	}

	// The next tick sees the same idle stretch and leaves it alone.
	runIdleCompactOnce(t)
	if got := w.nudges(t); len(got) != 1 {
		t.Fatalf("compacted twice: %v", got)
	}
}
