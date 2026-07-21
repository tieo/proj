package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/tieo/proj/internal/sessions"
)

// Claude Code writes one file per running session under <claude root>/sessions,
// named after the process id. It carries the conversation id, the working
// directory, the display name and - once Remote Control has bound - the bridge
// session id, which is what claude.ai/code lists.

type sessionFile struct {
	PID              int    `json:"pid"`
	SessionID        string `json:"sessionId"`
	Cwd              string `json:"cwd"`
	Name             string `json:"name"`
	BridgeSessionID  string `json:"bridgeSessionId"`
	UpdatedAt        int64  `json:"updatedAt"`
	StartedAt        int64  `json:"startedAt"`
	ConnectionStatus string `json:"status"`
}

// BridgeSessionID returns the Remote Control session id bound to the Claude
// session running in dir, or "" when none is. The newest entry wins: exited
// processes leave their file behind, and a project that has been reopened has
// several.
//
// The stored cwd is the path as Claude Code sees it, which under WSL is the
// \\wsl.localhost UNC form of dir, so both spellings are compared.
func BridgeSessionID(claudeHome, dir string) string {
	entries, err := filepath.Glob(filepath.Join(claudeRoot(claudeHome), "sessions", "*.json"))
	if err != nil {
		return ""
	}
	want := []string{dir, sessions.WSLToUNC(dir)}
	var best sessionFile
	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s sessionFile
		if json.Unmarshal(raw, &s) != nil || s.BridgeSessionID == "" {
			continue
		}
		if !matchesDir(s.Cwd, want) {
			continue
		}
		if s.UpdatedAt >= best.UpdatedAt {
			best = s
		}
	}
	return best.BridgeSessionID
}

func matchesDir(cwd string, want []string) bool {
	for _, w := range want {
		if w == "" {
			continue
		}
		if strings.EqualFold(cwd, w) || sessions.LocalDir(cwd) == w {
			return true
		}
	}
	return false
}
