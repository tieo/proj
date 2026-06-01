// Package tmux is a thin wrapper around the tmux command-line for the
// operations proj needs (list panes/sessions, capture, attach, send-keys).
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/tieo/proj/internal/shellout"
)

type Pane struct {
	Session string
	ID      string
}

type Session struct {
	Name     string
	Path     string
	Activity int64
}

// SessionForPath returns the name of the session whose working directory is
// dir, or "" if none. A project's identity is its directory, not its session
// name: the name carries tags and can drift (see `proj tag`), but the dir is
// fixed, and is also what Claude keys its history on. Callers that look up by
// dir therefore find the project's session even after a tag rename, and never
// spawn a duplicate session for the same dir.
func SessionForPath(dir string) string {
	for _, s := range ListSessions() {
		if s.Path == dir {
			return s.Name
		}
	}
	return ""
}

func ListPanes() []Pane {
	out := shellout.Run("tmux", "list-panes", "-a", "-F", "#{session_name}\t#{pane_id}")
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		panes = append(panes, Pane{Session: parts[0], ID: parts[1]})
	}
	return panes
}

func ListSessions() []Session {
	out := shellout.Run("tmux", "list-sessions", "-F", "#S\t#{session_path}\t#{session_activity}")
	var sessions []Session
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		act, _ := strconv.ParseInt(parts[2], 10, 64)
		sessions = append(sessions, Session{Name: parts[0], Path: parts[1], Activity: act})
	}
	return sessions
}

// PaneCurrentPath returns the working directory of `target`.
func PaneCurrentPath(target string) string {
	return strings.TrimSpace(shellout.Run("tmux", "display-message", "-p", "-t", target, "#{pane_current_path}"))
}

// CapturePane returns the visible content of `target`. If lines > 0, that many
// lines of scrollback are included; 0 means the current visible screen only.
func CapturePane(target string, lines int) string {
	args := []string{"capture-pane", "-p", "-t", target}
	if lines > 0 {
		args = append(args, "-S", fmt.Sprintf("-%d", lines))
	}
	return shellout.Run("tmux", args...)
}

// NewSession creates a detached session in `dir` and returns the new pane id.
// When command is non-empty it becomes the pane's program (run via the
// default shell), so nothing (no shell prompt, no echoed command line) is
// printed above it; an empty command starts a plain interactive shell.
//
// Mouse mode is enabled on the session so the scroll wheel scrolls pane
// content (and enters copy-mode) instead of sending arrow keys to the
// program. Scoped with -t to this session so the user's global tmux config
// is left untouched. The wheel scroll step is also set (see scrollStep);
// that one is a global key binding because tmux has no per-session key
// tables, but it is re-asserted idempotently on every session create.
func NewSession(name, dir, command string) (string, error) {
	args := []string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", name, "-c", dir}
	if command != "" {
		args = append(args, command)
	}
	pane, err := shellout.RunErr("tmux", args...)
	if err != nil {
		return "", err
	}
	// Best effort: failures here shouldn't prevent the session from opening.
	// Note: set-option does not accept the "=" exact-match target prefix that
	// has-session/kill-session use, so the bare name is passed here.
	_, _ = shellout.RunErr("tmux", "set-option", "-t", name, "mouse", "on")
	// Tear the pane (and thus the one-window session) down when its program
	// exits, regardless of the user's global remain-on-exit. proj treats a
	// finished claude as a finished session: the dir is left clean and the next
	// open starts fresh rather than re-attaching a dead pane.
	_, _ = shellout.RunErr("tmux", "set-option", "-t", name, "remain-on-exit", "off")
	setScrollStep()
	return pane, nil
}

// scrollStep is how many lines a single mouse-wheel notch scrolls in
// copy-mode (tmux's default is 5).
const scrollStep = 2

// setScrollStep rebinds the mouse wheel in both copy-mode key tables to
// scroll scrollStep lines per notch. Key bindings are server-global (tmux has
// no per-session key tables), so this affects all sessions; it is idempotent.
func setScrollStep() {
	n := strconv.Itoa(scrollStep)
	// The command after select-pane must be joined with an escaped `\;`; a
	// bare `;` is consumed by tmux as a top-level command separator and the
	// binding would collapse to just select-pane.
	for _, table := range []string{"copy-mode", "copy-mode-vi"} {
		_, _ = shellout.RunErr("tmux", "bind-key", "-T", table, "WheelUpPane",
			"select-pane", `\;`, "send-keys", "-X", "-N", n, "scroll-up")
		_, _ = shellout.RunErr("tmux", "bind-key", "-T", table, "WheelDownPane",
			"select-pane", `\;`, "send-keys", "-X", "-N", n, "scroll-down")
	}
}

// SendKeys sends `cmd` followed by Enter to `target`.
func SendKeys(target, cmd string) error {
	_, err := shellout.RunErr("tmux", "send-keys", "-t", target, cmd, "Enter")
	return err
}

// SendLiteral sends `text` to `target` in literal mode (no Enter, no key
// name expansion). Use for long messages where a trailing Enter must arrive
// only after the target has had time to process all buffered input.
func SendLiteral(target, text string) error {
	_, err := shellout.RunErr("tmux", "send-keys", "-t", target, "-l", text)
	return err
}

// SendKey sends a single named tmux key (e.g. "Escape", "Up") with no Enter.
func SendKey(target, key string) error {
	_, err := shellout.RunErr("tmux", "send-keys", "-t", target, key)
	return err
}

func KillSession(name string) error {
	_, err := shellout.RunErr("tmux", "kill-session", "-t", "="+name)
	return err
}

func RenameSession(oldName, newName string) error {
	_, err := shellout.RunErr("tmux", "rename-session", "-t", "="+oldName, newName)
	return err
}

// Attach switches the current tmux client (if inside tmux) or execs `tmux
// attach`, replacing this process so the terminal hands off cleanly.
func Attach(name string) error {
	if os.Getenv("TMUX") != "" {
		_, err := shellout.RunErr("tmux", "switch-client", "-t", "="+name)
		return err
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	return syscall.Exec(bin, []string{"tmux", "attach", "-t", "=" + name}, os.Environ())
}
