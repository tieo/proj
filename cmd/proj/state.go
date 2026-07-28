package main

import (
	"os"
	"path/filepath"
)

// projStateDir is where proj keeps the state that outlives a command: handoff
// artifacts, the doner nudge stamps, the viewbook key. Every caller resolved
// XDG_STATE_HOME itself before this existed, so the same four lines sat in as
// many files as had state to keep.
func projStateDir(parts ...string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(append([]string{base, "proj"}, parts...)...)
}
