// Package projstate names the one directory proj keeps its state in: the
// daemon's tracking file, handoff artifacts, doner's nudge stamps, the viewbook
// key. Every caller with something to keep used to resolve XDG_STATE_HOME
// itself, so the same four lines sat in as many files as had state, in two
// packages, and each new feature added another copy.
package projstate

import (
	"os"
	"path/filepath"
)

// Dir returns proj's state directory, with parts joined onto it. With no parts
// it is the directory itself.
func Dir(parts ...string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(append([]string{base, "proj"}, parts...)...)
}
