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
	"time"

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

// claudeReadyMarkers are substrings the Claude Code TUI renders only once the
// input box is live and accepting keystrokes (not during startup, not behind
// the trust-folder prompt). Matching either is sufficient.
var claudeReadyMarkers = []string{"bypass permissions", "? for shortcuts"}

// WaitForClaudeReady polls the pane until one of the live-input markers is
// visible, or timeout elapses. Returns whether the pane became ready.
func WaitForClaudeReady(target string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c := CapturePane(target, 0)
		for _, m := range claudeReadyMarkers {
			if strings.Contains(c, m) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ApplySlashCommands fires "/cmd"+Enter into the pane for each entry, after
// waiting for Claude's input box to settle. No-op when slashes is empty or
// the pane never reaches readiness (e.g. claude is sitting on the trust-folder
// prompt for a brand-new dir - we don't want slash commands typed during that).
func ApplySlashCommands(target string, slashes []string, timeout time.Duration) {
	if len(slashes) == 0 {
		return
	}
	if !WaitForClaudeReady(target, timeout) {
		return
	}
	for _, s := range slashes {
		_ = SendKeys(target, "/"+s)
		time.Sleep(200 * time.Millisecond)
	}
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
//
// Under WSL the server must be made to outlive the terminal first; see
// ensureServer. The session itself is always created with a plain new-session
// (attaching to that server), so the pane id is captured normally via -P.
func NewSession(name, dir, command string) (string, error) {
	ensureServer()
	pane, err := shellout.RunErr("tmux", newSessionArgs(name, dir, command, true)...)
	if err != nil {
		return "", err
	}
	applySessionOptions(name)
	return pane, nil
}

// bootstrapSession is the throwaway session used only to bring the tmux server
// up; ensureServer kills it immediately, leaving an empty (but persistent)
// server. It is never observed by callers, so it needs no filtering.
const bootstrapSession = "_proj_bootstrap"

// ensureServer guarantees a tmux server exists that will outlive the terminal
// proj happens to run in. It is a no-op when a server is already running or off
// WSL, where tmux servers already survive terminal exit (they reparent to PID 1
// and no console subreaper kills them).
//
// Under WSL two things conspire to kill a normally-spawned server when its
// terminal window closes:
//  1. WSL turns each Windows console into an /init "relay" that acts as a
//     subreaper, so a tmux server started from a terminal is adopted by that
//     relay and torn down with its whole subtree when the window closes.
//  2. Even a relay-immune server exits once its last session ends, and closing
//     the terminal can transiently drop the lone session, taking the server.
//
// ensureServer defeats both: it starts the server via the systemd user manager
// (systemd-run, Type=forking), which reparents it to the manager (the
// user@.service) instead of the console relay, and sets the server option
// exit-empty off so the server persists even with zero sessions. The bootstrap
// session exists only to start the server and is killed once exit-empty is set;
// no holder session is left behind (which would otherwise clutter tmux's own
// session switcher). Each later `proj open` then attaches to a server that
// simply never dies, and claude (claude.exe via interop) survives a console
// close on its own, so once it rides on this server it persists across closing
// the window. tmux command separators are passed as bare ";" arguments: no
// shell is involved, so they reach tmux literally.
//
// If systemd-run is unusable the function is a best-effort no-op: NewSession
// then falls back to a plain (terminal-owned) server, which still works but
// dies on window close.
func ensureServer() {
	if serverRunning() || !useSystemdServer() {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/"
	}
	boot := []string{
		"new-session", "-d", "-s", bootstrapSession, "-c", home,
		";", "set-option", "-s", "exit-empty", "off",
		";", "kill-session", "-t", bootstrapSession,
	}
	_, _ = shellout.RunErr("systemd-run", systemdRunArgs(boot, os.Environ())...)
}

// newSessionArgs builds the tmux argv for creating a detached session. When
// capturePane is set, -P/-F make tmux print the new pane id on stdout.
func newSessionArgs(name, dir, command string, capturePane bool) []string {
	args := []string{"new-session", "-d"}
	if capturePane {
		args = append(args, "-P", "-F", "#{pane_id}")
	}
	args = append(args, "-s", name, "-c", dir)
	if command != "" {
		args = append(args, command)
	}
	return args
}

// systemdRunArgs wraps a tmux invocation so it runs as a transient systemd
// user service. Type=forking lets systemd track the daemonized tmux server as
// the unit's main process; --collect removes the unit once the server exits.
//
// The caller's environment is forwarded with --setenv: systemd-run otherwise
// starts the service with a minimal environment, and since the tmux server (and
// every pane spawned on it) inherits that environment, a stripped PATH would
// leave the pane's program (e.g. claude in ~/.local/bin) not found, so it would
// exit immediately and tear the session down before it could be attached.
func systemdRunArgs(tmuxArgs, env []string) []string {
	args := []string{"--user", "--quiet", "--collect", "-p", "Type=forking"}
	for _, kv := range env {
		args = append(args, "--setenv="+kv)
	}
	args = append(args, "--", "tmux")
	return append(args, tmuxArgs...)
}

// applySessionOptions sets the per-session tmux options proj relies on and the
// global scroll-step binding. Best effort: failures here must not prevent the
// session from opening.
func applySessionOptions(name string) {
	// set-option does not accept the "=" exact-match target prefix that
	// has-session/kill-session use, so the bare name is passed here.
	_, _ = shellout.RunErr("tmux", "set-option", "-t", name, "mouse", "on")
	// Tear the pane (and thus the one-window session) down when its program
	// exits, regardless of the user's global remain-on-exit. proj treats a
	// finished claude as a finished session: the dir is left clean and the next
	// open starts fresh rather than re-attaching a dead pane.
	_, _ = shellout.RunErr("tmux", "set-option", "-t", name, "remain-on-exit", "off")
	// Stop the status bar from truncating the session name. tmux's default
	// status-left-length is 10, but proj's names carry tags ("name@tag+tag")
	// and routinely exceed that, so they'd render clipped ("[proj@go+t"). The
	// status-left format is "[#{session_name}] ", so size the field to the name
	// plus that "[ ] " decoration; the value only caps the field, never pads it.
	_, _ = shellout.RunErr("tmux", "set-option", "-t", name, "status-left-length", strconv.Itoa(len(name)+4))
	setScrollStep()
}

// serverRunning reports whether a tmux server is up on the default socket.
func serverRunning() bool {
	_, err := shellout.RunErr("tmux", "list-sessions")
	return err == nil
}

// useSystemdServer reports whether new tmux servers should be started through
// the systemd user manager. This matters only under WSL (see NewSession) and
// only when systemd-run is available to do it.
func useSystemdServer() bool {
	if !IsWSL() {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// IsWSL reports whether proj is running under the Windows Subsystem for Linux.
func IsWSL() bool {
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return detectWSL(string(b))
}

// detectWSL reports whether a kernel osrelease string identifies a WSL kernel.
func detectWSL(osRelease string) bool {
	return strings.Contains(strings.ToLower(osRelease), "microsoft")
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

// RespawnSession replaces the program running in a session's only pane,
// killing the old one and its children. The session and pane keep their ids,
// so clients stay attached across the swap; killing the session instead would
// detach every client watching it. The pane's scrollback does not survive.
//
// The pane is addressed by id: "=name" is an exact-match session target and
// respawn-pane rejects it, while the bare name would match by prefix.
func RespawnSession(name, dir, command string) error {
	pane := firstLine(shellout.Run("tmux", "list-panes", "-t", "="+name, "-F", "#{pane_id}"))
	if pane == "" {
		return fmt.Errorf("session %q has no pane", name)
	}
	_, err := shellout.RunErr("tmux", "respawn-pane", "-k", "-t", pane, "-c", dir, command)
	return err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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
