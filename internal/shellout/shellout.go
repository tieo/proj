// Package shellout wraps os/exec for the common "run a command, get its
// trimmed stdout" pattern used throughout proj.
package shellout

import (
	"os/exec"
	"strings"
)

// Run executes cmd with args and returns trimmed stdout, or "" on error.
func Run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// RunErr is like Run but surfaces the error.
func RunErr(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// Quote wraps s in single quotes so it survives as a single word when a
// command line is typed into a shell (e.g. via tmux send-keys). Embedded
// single quotes are escaped using the standard '\'' idiom. This is only
// context-free-correct when the placeholder sits in an unquoted position in
// the surrounding command template.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
