package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tieo/proj/internal/config"
)

// writeCodexRollout lays out a rollout transcript the way codex does:
// <home>/sessions/YYYY/MM/DD/rollout-<ts>-<id>.jsonl with a session_meta
// first line carrying the cwd.
func writeCodexRollout(t *testing.T, home, cwd string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "07", "09")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-07-09T10:00:00.000Z","type":"session_meta","payload":{"id":"0000","cwd":"` + cwd + `"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-07-09T10-00-00-0000.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCodexRolloutWithModel(t *testing.T, home, cwd, name, model string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "07", "09")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"timestamp":"2026-07-09T10:00:00.000Z","type":"session_meta","payload":{"id":"` + name + `","cwd":"` + cwd + `"}}` + "\n" +
		`{"timestamp":"2026-07-09T10:00:01.000Z","type":"turn_context","payload":{"model":"` + model + `"}}` + "\n"
	path := filepath.Join(dir, "rollout-2026-07-09T10-00-00-"+name+".jsonl")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexHasHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	proj := "/home/u/projects/code/api"
	if CodexHasHistory(proj) {
		t.Error("empty codex home must report no history")
	}
	writeCodexRollout(t, home, proj)
	if !CodexHasHistory(proj) {
		t.Error("rollout with matching cwd must report history")
	}
	if CodexHasHistory("/home/u/projects/code/other") {
		t.Error("rollout for another cwd must not count")
	}
}

func TestCodexModelFromDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	proj := "/home/u/projects/code/api"
	older := writeCodexRolloutWithModel(t, home, proj, "older", "gpt-5.4-mini")
	newer := writeCodexRolloutWithModel(t, home, proj, "newer", "gpt-5.5")
	if err := os.Chtimes(older, time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC), time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC), time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := CodexModelFromDir(proj); got != "gpt-5.5" {
		t.Errorf("model = %q, want gpt-5.5", got)
	}
	if got := CodexModelFromDir("/home/u/projects/code/other"); got != "" {
		t.Errorf("other model = %q, want empty", got)
	}
}

func TestDetectCodexFromRolloutPremiumLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	reset := time.Date(2026, 7, 10, 0, 23, 34, 0, time.Local).Unix()
	lines := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"go"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":79,"resets_at":` + strconv.FormatInt(reset, 10) + `},"rate_limit_reached_type":null}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"premium","primary":null,"credits":{"has_credits":false,"unlimited":false},"rate_limit_reached_type":null}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":null}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	b := DetectCodexFromRollout(path, time.Unix(reset-3600, 0))
	if b == nil {
		t.Fatal("expected codex premium limit")
	}
	if b.Reset.Unix() != reset {
		t.Fatalf("reset = %v, want unix %d", b.Reset, reset)
	}
	if !strings.Contains(b.Text, "premium") {
		t.Fatalf("text = %q, want premium", b.Text)
	}
}

func TestDetectCodexFromRolloutPastResetUsesBackoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	reset := time.Date(2026, 7, 10, 0, 23, 34, 0, time.Local).Unix()
	lines := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"go"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":100,"resets_at":` + strconv.FormatInt(reset, 10) + `},"rate_limit_reached_type":null}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":null}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	b := DetectCodexFromRollout(path, time.Unix(reset+3600, 0))
	if b == nil {
		t.Fatal("expected codex limit")
	}
	if b.Backoff != transientShortBackoff {
		t.Fatalf("backoff = %v, want %v", b.Backoff, transientShortBackoff)
	}
	if !b.Reset.IsZero() {
		t.Fatalf("reset = %v, want zero", b.Reset)
	}
}

func TestDetectCodexFromRolloutClearedByAgentTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"go"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":100,"resets_at":1783635814}}}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"done"}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if b := DetectCodexFromRollout(path, time.Now()); b != nil {
		t.Fatalf("got %+v, want nil", b)
	}
}

func TestLaunchCommandResumeGating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	spec := config.ToolSpec{Name: "codex", Command: "codex", ResumeCommand: "codex resume --last"}
	proj := "/home/u/projects/code/api"

	cmd := LaunchCommand(spec, "", "api", "api@work", proj)
	if !strings.HasPrefix(cmd, "codex &&") {
		t.Errorf("no history: want fresh launch, got %q", cmd)
	}
	if !strings.Contains(cmd, "proj daemon mark-closed 'api@work'") &&
		!strings.Contains(cmd, "proj daemon mark-closed api@work") {
		t.Errorf("clean-close mark missing: %q", cmd)
	}

	writeCodexRollout(t, home, proj)
	cmd = LaunchCommand(spec, "", "api", "api@work", proj)
	if !strings.HasPrefix(cmd, "codex resume --last") {
		t.Errorf("with history: want resume command, got %q", cmd)
	}
}

func TestLaunchCommandPlaceholders(t *testing.T) {
	spec := config.ToolSpec{Name: "claude", Command: "claude -n {rc} --dir {dir} --name {name}"}
	cmd := LaunchCommand(spec, t.TempDir(), "api", "api@work", "/tmp/api")
	host, _ := os.Hostname()
	if !strings.Contains(cmd, "'api @"+host+" [work]'") {
		t.Errorf("{rc} not filled: %q", cmd)
	}
	if !strings.Contains(cmd, "--dir '/tmp/api'") || !strings.Contains(cmd, "--name 'api'") {
		t.Errorf("placeholders not filled: %q", cmd)
	}
}

func TestAgyHasHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := "/home/u/projects/code/api"
	if AgyHasHistory(proj) {
		t.Error("missing history file must report no history")
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"display":"hi","timestamp":1,"workspace":"/home/u/projects/code/api"}` + "\n" +
		`{"display":"yo","timestamp":2,"workspace":"/home/u/projects/code/other"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if !AgyHasHistory(proj) {
		t.Error("workspace match must report history")
	}
	if AgyHasHistory("/home/u/projects/code/third") {
		t.Error("unrecorded workspace must not count")
	}
}

func TestAgyModelFromDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := "/home/u/projects/code/api"
	app := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(filepath.Join(app, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app, "log"), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := `{"` + proj + `":"conv-api","/other":"conv-other"}`
	if err := os.WriteFile(filepath.Join(app, "cache", "last_conversations.json"), []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	log := strings.Join([]string{
		`I creating conversation conv-api`,
		`I model_config_manager.go:157] Propagating selected model override to backend: label="Gemini 3.5 Flash (Medium)"`,
		`I model_config_manager.go:157] Propagating selected model override to backend: label="Gemini 3.5 Pro (High)"`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(app, "log", "cli-20260709_181747.log"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := AgyModelFromDir(proj); got != "Gemini 3.5 Pro (High)" {
		t.Errorf("model = %q, want latest label", got)
	}
	if got := AgyModelFromDir("/missing"); got != "" {
		t.Errorf("missing model = %q, want empty", got)
	}
}
