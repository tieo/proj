package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tieo/proj/internal/paseo"
)

// sweepWorld stands up a machine: a Paseo CLI that lists the agents a test
// wants and records what was sent to them, a Claude transcript per agent, a
// project registry, and a config.
type sweepWorld struct {
	dir     string
	sent    string // file the stub CLI appends its sends to
	claude  string
	paseoHm string
}

func newSweepWorld(t *testing.T, agents []map[string]string, tagged []string) *sweepWorld {
	t.Helper()
	root := t.TempDir()
	w := &sweepWorld{
		dir:     root,
		sent:    filepath.Join(root, "sent.log"),
		claude:  filepath.Join(root, "claude"),
		paseoHm: filepath.Join(root, "paseo"),
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("PASEO_HOME", w.paseoHm)

	list, err := json.Marshal(agents)
	if err != nil {
		t.Fatal(err)
	}
	// The stub answers `agent ls` with the fixture and logs `agent send`.
	stub := filepath.Join(root, "paseo-stub")
	script := "#!/bin/sh\nif [ \"$2\" = ls ]; then cat <<'JSON'\n" + string(list) +
		"\nJSON\nelse echo \"$5\" >> " + w.sent + "\nfi\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := paseo.Bin
	paseo.Bin = stub
	t.Cleanup(func() { paseo.Bin = old })

	reg := "[projects]\n"
	for _, p := range tagged {
		reg += "  [projects." + p + "]\n    tags = [\"doner\"]\n"
	}
	cfgDir := filepath.Join(root, "config", "proj")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	if err := os.WriteFile(filepath.Join(cfgDir, "projects.toml"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	conf := "[claude]\nhome = \"" + w.claude + "\"\n\n[daemon.doner]\nenabled = true\nwait = \"30m\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	return w
}

// say writes a transcript whose last assistant message is text, spoken ago
// before now, and the agent record that points at it.
func (w *sweepWorld) say(t *testing.T, agentID, dir, text string, ago time.Duration) {
	t.Helper()
	sid := "sess-" + agentID
	slug := strings.ReplaceAll(strings.TrimPrefix(dir, "/"), "/", "-")
	recDir := filepath.Join(w.paseoHm, "agents", slug)
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec, _ := json.Marshal(map[string]any{"runtimeInfo": map[string]string{"sessionId": sid}})
	if err := os.WriteFile(filepath.Join(recDir, agentID+".json"), rec, 0o644); err != nil {
		t.Fatal(err)
	}
	enc := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(dir)
	tdir := filepath.Join(w.claude, "projects", enc)
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"type":      "assistant",
		"timestamp": time.Now().Add(-ago).UTC().Format(time.RFC3339),
		"message":   map[string]any{"content": []map[string]string{{"type": "text", "text": text}}},
	})
	if err := os.WriteFile(filepath.Join(tdir, sid+".jsonl"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stamp records that agentID was nudged ago before now.
func (w *sweepWorld) stamp(t *testing.T, agentID string, ago time.Duration) {
	t.Helper()
	p := sweepStampPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{agentID: time.Now().Add(-ago).Format(time.RFC3339)})
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *sweepWorld) nudges(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(w.sent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func agent(id, name, status, cwd string) map[string]string {
	return map[string]string{"id": id, "name": name, "status": status, "cwd": cwd}
}

func TestSweepNudgesAWaitingSessionPastTheWindow(t *testing.T) {
	sweepAfter, sweepDryRun = 0, false
	w := newSweepWorld(t,
		[]map[string]string{agent("a1", "phonetix", "idle", "/p/phonetix")},
		[]string{"phonetix"})
	w.say(t, "a1", "/p/phonetix", "Waiting on ba9uzs1ng (the suite run)", 74*time.Minute)

	if err := runSweep(nil, nil); err != nil {
		t.Fatal(err)
	}
	got := w.nudges(t)
	if len(got) != 1 {
		t.Fatalf("nudges = %v, want exactly one: this is the 74-minute stall", got)
	}
	if !strings.Contains(got[0], "Waiting on <id>") {
		t.Errorf("the nudge should be the same text the Stop hook sends, got %q", got[0])
	}
}

func TestSweepLeavesAlone(t *testing.T) {
	cases := []struct {
		name, status, reply string
		ago                 time.Duration
		tagged              bool
	}{
		{"still inside the window", "idle", "Waiting on b1", 5 * time.Minute, true},
		{"reported done", "idle", "Yes", 90 * time.Minute, true},
		{"turn the API refused", "idle", "API Error: an image could not be processed", 90 * time.Minute, true},
		{"still working", "running", "Waiting on b1", 90 * time.Minute, true},
		{"not doner-tagged", "idle", "Waiting on b1", 90 * time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sweepAfter, sweepDryRun = 0, false
			var tagged []string
			if c.tagged {
				tagged = []string{"x"}
			}
			w := newSweepWorld(t, []map[string]string{agent("a1", "x", c.status, "/p/x")}, tagged)
			w.say(t, "a1", "/p/x", c.reply, c.ago)
			if err := runSweep(nil, nil); err != nil {
				t.Fatal(err)
			}
			if got := w.nudges(t); len(got) != 0 {
				t.Errorf("nudged %v, want left alone", got)
			}
		})
	}
}

func TestSweepDoesNotNudgeTwiceForTheSameSilence(t *testing.T) {
	sweepAfter, sweepDryRun = 0, false
	w := newSweepWorld(t, []map[string]string{agent("a1", "x", "idle", "/p/x")}, []string{"x"})
	w.say(t, "a1", "/p/x", "Waiting on b1", 90*time.Minute)

	for i := 0; i < 3; i++ {
		if err := runSweep(nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.nudges(t); len(got) != 1 {
		t.Fatalf("nudges = %d, want 1: a session that never answers must not be nudged every tick", len(got))
	}
}

func TestSweepNudgesAgainAfterTheSessionSpokeAndWentQuietAgain(t *testing.T) {
	sweepAfter, sweepDryRun = 0, false
	w := newSweepWorld(t, []map[string]string{agent("a1", "x", "idle", "/p/x")}, []string{"x"})
	// Nudged two hours ago, answered an hour ago, quiet since: that is a new
	// silence, not the one already answered for.
	w.stamp(t, "a1", 120*time.Minute)
	w.say(t, "a1", "/p/x", "Waiting on b2", 60*time.Minute)
	if err := runSweep(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := w.nudges(t); len(got) != 1 {
		t.Fatalf("nudges = %d, want 1: the session spoke after the last nudge", len(got))
	}
}

func TestSweepDryRunSendsNothing(t *testing.T) {
	sweepAfter, sweepDryRun = 0, true
	defer func() { sweepDryRun = false }()
	w := newSweepWorld(t, []map[string]string{agent("a1", "x", "idle", "/p/x")}, []string{"x"})
	w.say(t, "a1", "/p/x", "Waiting on b1", 90*time.Minute)
	if err := runSweep(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := w.nudges(t); len(got) != 0 {
		t.Errorf("dry run sent %v", got)
	}
}

func TestSweepWindowComesFromConfig(t *testing.T) {
	sweepAfter, sweepDryRun = 0, false
	w := newSweepWorld(t, []map[string]string{agent("a1", "x", "idle", "/p/x")}, []string{"x"})
	w.say(t, "a1", "/p/x", "Waiting on b1", 40*time.Minute)
	// 40 minutes is past the configured 30, so this one is due.
	if err := runSweep(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := w.nudges(t); len(got) != 1 {
		t.Fatalf("nudges = %v, want one at the configured 30m window", got)
	}
}
