package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestLaunchCommandResumeGating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	spec := config.AgentSpec{Name: "codex", Command: "codex", ResumeCommand: "codex resume --last"}
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
	spec := config.AgentSpec{Name: "claude", Command: "claude -n {rc} --dir {dir} --name {name}"}
	cmd := LaunchCommand(spec, t.TempDir(), "api", "api@work", "/tmp/api")
	host, _ := os.Hostname()
	if !strings.Contains(cmd, "'api @"+host+" [work]'") {
		t.Errorf("{rc} not filled: %q", cmd)
	}
	if !strings.Contains(cmd, "--dir '/tmp/api'") || !strings.Contains(cmd, "--name 'api'") {
		t.Errorf("placeholders not filled: %q", cmd)
	}
}
