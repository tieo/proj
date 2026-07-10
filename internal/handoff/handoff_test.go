package handoff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadClaude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"fix the bug"}}`,
		`{"type":"user","message":{"role":"user","content":"<system-reminder>noise</system-reminder>"}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta noise"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"opaque"},{"type":"text","text":"On it."},{"type":"tool_use","name":"Bash","input":{"command":"go test"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"file-history-snapshot","snapshot":{}}`,
	}, "\n"))
	tr, err := ReadClaude(path, "/p/api")
	if err != nil {
		t.Fatal(err)
	}
	want := []Turn{
		{Role: "user", Text: "fix the bug"},
		{Role: "assistant", Text: "On it."},
		{Role: "tool", Name: "Bash", Text: `{"command":"go test"}`},
	}
	if len(tr.Turns) != len(want) {
		t.Fatalf("turns = %+v, want %+v", tr.Turns, want)
	}
	for i := range want {
		if tr.Turns[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, tr.Turns[i], want[i])
		}
	}
	if tr.SourceTool != "claude" || tr.Cwd != "/p/api" {
		t.Errorf("meta = %+v", tr)
	}
}

func TestReadCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"x","cwd":"/p/api"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>noise</environment_context>"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"fix the bug"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":{"command":["go","test"]}}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"done"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{}}}`,
	}, "\n"))
	tr, err := ReadCodex(path, "/p/api")
	if err != nil {
		t.Fatal(err)
	}
	want := []Turn{
		{Role: "user", Text: "fix the bug"},
		{Role: "tool", Name: "shell", Text: `{"command":["go","test"]}`},
		{Role: "assistant", Text: "done"},
	}
	if len(tr.Turns) != len(want) {
		t.Fatalf("turns = %+v, want %+v", tr.Turns, want)
	}
	for i := range want {
		if tr.Turns[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, tr.Turns[i], want[i])
		}
	}
}

func TestReadAgy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"display":"fix the bug","timestamp":1,"workspace":"/p/api"}`,
		`{"display":"unrelated","timestamp":2,"workspace":"/p/other"}`,
	}, "\n"))
	tr, err := ReadAgy(path, "/p/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Turns) != 1 || tr.Turns[0].Text != "fix the bug" {
		t.Fatalf("turns = %+v", tr.Turns)
	}
	// A missing history file is an empty transcript, not an error.
	if tr, err := ReadAgy(filepath.Join(t.TempDir(), "none.jsonl"), "/p/api"); err != nil || !tr.Empty() {
		t.Errorf("missing file: tr=%+v err=%v", tr, err)
	}
}

func TestRecentCodexRollout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	old := filepath.Join(home, "sessions", "2026", "07", "08", "rollout-a.jsonl")
	newer := filepath.Join(home, "sessions", "2026", "07", "09", "rollout-b.jsonl")
	other := filepath.Join(home, "sessions", "2026", "07", "09", "rollout-c.jsonl")
	writeFile(t, old, `{"type":"session_meta","payload":{"cwd":"/p/api"}}`)
	writeFile(t, newer, `{"type":"session_meta","payload":{"cwd":"/p/api"}}`)
	writeFile(t, other, `{"type":"session_meta","payload":{"cwd":"/p/other"}}`)
	if got := RecentCodexRollout("/p/api"); got != newer {
		t.Errorf("got %q, want %q", got, newer)
	}
	if got := RecentCodexRollout("/p/none"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func testTranscript() *Transcript {
	return &Transcript{
		Version: 1, SourceTool: "claude", Cwd: "/p/api", ExtractedAt: "2026-07-09T18:00:00Z",
		Turns: []Turn{
			{Role: "user", Text: "fix the bug"},
			{Role: "tool", Name: "Bash", Text: `{"command":"go test"}`},
			{Role: "assistant", Text: "done"},
		},
	}
}

func TestWriteCodex(t *testing.T) {
	home := t.TempDir()
	id, err := WriteCodex(testTranscript(), home, "/p/api", "/tmp/handoff.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	path := RecentCodexRollout("/p/api")
	if path == "" || !strings.Contains(path, id) {
		t.Fatalf("rollout for cwd not found (id %s)", id)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// session_meta + (handoff + 3 turns) * 2 (response_item and event_msg per turn)
	if len(lines) != 9 {
		t.Fatalf("got %d lines", len(lines))
	}
	for _, ln := range lines {
		var v map[string]any
		if json.Unmarshal([]byte(ln), &v) != nil {
			t.Fatalf("unparseable line %q", ln)
		}
	}
	if !strings.Contains(string(data), "[ran Bash:") {
		t.Errorf("tool turn not flattened in data: %s", string(data))
	}
	if !strings.Contains(string(data), "/tmp/handoff.json") {
		t.Errorf("handoff artifact path missing: %s", string(data))
	}
	idx, err := os.ReadFile(filepath.Join(home, "session_index.jsonl"))
	if err != nil || !strings.Contains(string(idx), id) {
		t.Errorf("index missing session: %v %s", err, idx)
	}
}

func TestWriteClaude(t *testing.T) {
	root := t.TempDir()
	claudeHome := filepath.Join(root, ".claude")
	writeFile(t, filepath.Join(root, ".claude.json"), `{"projects":{}}`)
	id, err := WriteClaude(testTranscript(), claudeHome, "/p/api", "/tmp/handoff.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeHome, "projects", "-p-api", id+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// handoff note + 3 turns
	if len(lines) != 4 {
		t.Fatalf("got %d lines", len(lines))
	}
	prev := ""
	for i, ln := range lines {
		var rec struct {
			UUID       string  `json:"uuid"`
			ParentUUID *string `json:"parentUuid"`
			SessionID  string  `json:"sessionId"`
			Cwd        string  `json:"cwd"`
		}
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if rec.SessionID != id || rec.Cwd != "/p/api" {
			t.Errorf("line %d meta: %+v", i, rec)
		}
		if i == 0 && rec.ParentUUID != nil {
			t.Error("first record must have null parentUuid")
		}
		if i > 0 && (rec.ParentUUID == nil || *rec.ParentUUID != prev) {
			t.Errorf("line %d parent chain broken", i)
		}
		prev = rec.UUID
	}
	cj, _ := os.ReadFile(filepath.Join(root, ".claude.json"))
	if !strings.Contains(string(cj), id) {
		t.Errorf("lastSessionId not set: %s", cj)
	}
	if !strings.Contains(string(data), "/tmp/handoff.json") {
		t.Errorf("handoff artifact path missing: %s", data)
	}
}

func TestPromptAndCaps(t *testing.T) {
	p := testTranscript().PromptWithArtifact("")
	for _, want := range []string{"taking over", "[User] fix the bug", "[claude ran Bash]", "[claude] done"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// Short turns are never dropped just for being numerous.
	manyShort := make([]Turn, 5000)
	for i := range manyShort {
		manyShort[i] = Turn{Role: "tool", Text: "x"}
	}
	if capped := capTurns(manyShort); len(capped) != len(manyShort) {
		t.Errorf("count alone must not evict: got %d, want %d", len(capped), len(manyShort))
	}

	// Over budget, tool calls go before assistant replies and user turns stay.
	var mixed []Turn
	for i := 0; i < 200; i++ {
		mixed = append(mixed,
			Turn{Role: "user", Text: strings.Repeat("u", 100)},
			Turn{Role: "assistant", Text: strings.Repeat("a", 1000)},
			Turn{Role: "tool", Text: strings.Repeat("t", 1000)},
		)
	}
	capped := capTurns(mixed)
	got := map[string]int{}
	chars := 0
	for _, turn := range capped {
		got[turn.Role]++
		chars += len(turn.Text)
	}
	if got["user"] != 200 {
		t.Errorf("user turns evicted: kept %d of 200", got["user"])
	}
	if chars > maxTranscriptChars {
		t.Errorf("over budget: %d chars", chars)
	}
	if got["tool"] >= got["assistant"] {
		t.Errorf("tool calls must be evicted first: tool=%d assistant=%d", got["tool"], got["assistant"])
	}
	if got := capText(strings.Repeat("x", 5000)); len(got) > maxTurnText+20 {
		t.Errorf("text cap: %d", len(got))
	}
}

func TestTargetContextCutoffKeepsSavedTurns(t *testing.T) {
	turns := make([]Turn, 500)
	for i := range turns {
		turns[i] = Turn{Role: "tool", Text: strings.Repeat("x", 1000)}
	}
	tr := newTranscript("claude", "/p/api", turns)
	if len(tr.Turns) != 500 {
		t.Fatalf("saved turns = %d, want 500", len(tr.Turns))
	}
	if got := len(tr.TargetTurns()); got >= 500 || got == 0 {
		t.Fatalf("target turns = %d, want a cut below 500", got)
	}
	note := tr.HandoffNote("/tmp/full.json")
	for _, want := range []string{"older extracted turns", "/tmp/full.json"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %s", want, note)
		}
	}
	prompt := tr.PromptWithArtifact("/tmp/full.json")
	if !strings.Contains(prompt, "bounded recent-history cutoff") || !strings.Contains(prompt, "/tmp/full.json") {
		t.Errorf("prompt missing cutoff/path:\n%s", prompt)
	}
}

// A cutoff notice that names no artifact leaves the target model told that
// turns are missing with no way to reach them.
func TestCutoffNoticeAlwaysResolves(t *testing.T) {
	long := &Transcript{SourceTool: "codex", ExtractedAt: "now"}
	for i := 0; i < 500; i++ {
		long.Turns = append(long.Turns, Turn{Role: "tool", Text: strings.Repeat("x", 1000)})
	}
	if long.omittedTurns() == 0 {
		t.Fatal("fixture must exceed the char budget")
	}
	cases := []struct {
		name     string
		render   func(string) string
		artifact string
		want     string
	}{
		{"prompt with artifact", long.PromptWithArtifact, "/tmp/h.json", "/tmp/h.json"},
		{"prompt without artifact", long.PromptWithArtifact, "", "cannot be recovered"},
		{"note with artifact", long.HandoffNote, "/tmp/h.json", "/tmp/h.json"},
		{"note without artifact", long.HandoffNote, "", "cannot be recovered"},
	}
	for _, tc := range cases {
		out := tc.render(tc.artifact)
		if !strings.Contains(out, "omitted") {
			t.Errorf("%s: no cutoff notice:\n%s", tc.name, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: missing %q:\n%s", tc.name, tc.want, out)
		}
	}
}

// Notes left by earlier switches must not ride along into the next target, or
// each hop would carry one more of them forever.
func TestHandoffNotesDoNotAccumulate(t *testing.T) {
	prior := &Transcript{SourceTool: "claude", ExtractedAt: "then"}
	note := prior.HandoffNote("/tmp/hop1.json")

	tr := newTranscript("codex", "/p/api", []Turn{
		{Role: "user", Text: note},
		{Role: "user", Text: "real instruction"},
		{Role: "assistant", Text: "done"},
	})
	if len(tr.Turns) != 3 {
		t.Fatalf("saved IR must keep the note for audit: %d turns", len(tr.Turns))
	}
	target := tr.TargetTurns()
	if len(target) != 2 {
		t.Fatalf("target turns = %d, want 2", len(target))
	}
	for _, turn := range target {
		if strings.Contains(turn.Text, "translated from") {
			t.Errorf("prior handoff note survived into target: %q", turn.Text)
		}
	}
	if tr.omittedTurns() != 0 {
		t.Errorf("stripping a note is not an omission, got %d", tr.omittedTurns())
	}
	if !strings.Contains(tr.PromptWithArtifact(""), "real instruction") {
		t.Error("real user turn lost")
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for _, stamp := range []string{"100", "200", "300", "400"} {
		writeFile(t, filepath.Join(dir, "api-"+stamp+".json"), "{}")
	}
	writeFile(t, filepath.Join(dir, "other-100.json"), "{}")

	if err := Prune(dir, "api", 2); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "api-*.json"))
	if len(left) != 2 {
		t.Fatalf("kept %d, want 2: %v", len(left), left)
	}
	for _, want := range []string{"api-300.json", "api-400.json"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("newest artifact %s pruned", want)
		}
	}
	// Another project's artifacts are untouched.
	if _, err := os.Stat(filepath.Join(dir, "other-100.json")); err != nil {
		t.Error("pruned a different project")
	}
	if err := Prune(dir, "api", 0); err == nil {
		t.Error("keep=0 would delete every artifact; want error")
	}
}
