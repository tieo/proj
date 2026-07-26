package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ConfiguredClaudeModel returns the model a Claude session started in dir runs:
// a --model flag on the launch command, else the first settings file that names
// one, in Claude Code's own precedence (project local, project shared, user
// local, user shared). It is empty when nothing pins a model, which leaves the
// session on whatever Claude Code defaults to. A /model choice made inside a
// running session is held by that process alone and is invisible here.
func ConfiguredClaudeModel(homeOverride, workDir, launchCommand string) string {
	if m := modelFlag(launchCommand); m != "" {
		return m
	}
	root := claudeRoot(homeOverride)
	for _, path := range []string{
		filepath.Join(workDir, ".claude", "settings.local.json"),
		filepath.Join(workDir, ".claude", "settings.json"),
		filepath.Join(root, "settings.local.json"),
		filepath.Join(root, "settings.json"),
	} {
		if m := modelFromSettings(path); m != "" {
			return m
		}
	}
	return ""
}

// modelFlag reads the model out of a launch command template. The value may
// still carry the quotes the shell would strip, so they come off here.
func modelFlag(command string) string {
	fields := strings.Fields(command)
	for i, f := range fields {
		if v, ok := strings.CutPrefix(f, "--model="); ok {
			return strings.Trim(v, `"'`)
		}
		if f == "--model" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], `"'`)
		}
	}
	return ""
}

func modelFromSettings(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s.Model
}
