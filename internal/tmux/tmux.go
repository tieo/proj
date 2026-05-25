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

func HasSession(name string) bool {
	_, err := shellout.RunErr("tmux", "has-session", "-t", "="+name)
	return err == nil
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
func NewSession(name, dir string) (string, error) {
	return shellout.RunErr("tmux", "new-session", "-d", "-P", "-F", "#{pane_id}", "-s", name, "-c", dir)
}

// SendKeys sends `cmd` followed by Enter to `target`.
func SendKeys(target, cmd string) error {
	_, err := shellout.RunErr("tmux", "send-keys", "-t", target, cmd, "Enter")
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
