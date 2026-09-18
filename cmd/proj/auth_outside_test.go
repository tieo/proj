package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProc lays out one process in a fake /proc: its stat line carries the
// name and parent, cmdline the argv separated by NULs as the kernel writes it.
func writeProc(t *testing.T, root string, pid int, name string, parent int, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (%s) S %d 0 0 0 -1 0 0 0 0 0 0 0", pid, name, parent)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdline := strings.Join(argv, "\x00")
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A Claude that a supervisor other than tmux started is the case this exists
// for: proj cannot stop it, and it writes its old login back when it ends.
func TestClaudeOutsideTmuxFindsAgentsOfOtherSupervisors(t *testing.T) {
	root := t.TempDir()
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })

	writeProc(t, root, 100, "tmux: server", 1, "tmux")
	writeProc(t, root, 200, "zsh", 100, "zsh")
	writeProc(t, root, 210, "claude", 200, "claude", "--resume")
	writeProc(t, root, 300, "node", 1, "node", "paseo-daemon")
	writeProc(t, root, 310, "claude", 300, "claude", "--print")

	found := claudeOutsideTmux("claude")

	if len(found) != 1 {
		t.Fatalf("want exactly the supervised-elsewhere claude, got %d: %v", len(found), found)
	}
	if !strings.Contains(found[0], "310") {
		t.Errorf("want the claude under the other supervisor (310), got %q", found[0])
	}
}

// A Claude nested several shells deep inside tmux is still inside tmux, and
// stopping it is claudeSessions' job rather than something to warn about.
func TestClaudeOutsideTmuxIgnoresDeeplyNestedPanes(t *testing.T) {
	root := t.TempDir()
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })

	writeProc(t, root, 100, "tmux: server", 1, "tmux")
	writeProc(t, root, 200, "zsh", 100, "zsh")
	writeProc(t, root, 205, "sh", 200, "sh", "-c")
	writeProc(t, root, 210, "claude", 205, "claude")

	if found := claudeOutsideTmux("claude"); len(found) != 0 {
		t.Errorf("want nothing for a pane-hosted claude, got %v", found)
	}
}

// The tool may be configured as a path or with arguments, and a wrapper script
// around it must not be mistaken for a second copy.
func TestClaudeOutsideTmuxMatchesTheBinaryNotTheWholeCommand(t *testing.T) {
	root := t.TempDir()
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })

	writeProc(t, root, 400, "claude", 1, "/nix/store/abc-claude/bin/claude", "--resume")

	found := claudeOutsideTmux("/etc/profiles/per-user/marius/bin/claude --dangerously-skip-permissions")
	if len(found) != 1 {
		t.Fatalf("want the claude matched by binary name, got %d: %v", len(found), found)
	}
}

// A parent chain that points at itself must not spin.
func TestClaudeOutsideTmuxSurvivesACycle(t *testing.T) {
	root := t.TempDir()
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })

	writeProc(t, root, 500, "claude", 501, "claude")
	writeProc(t, root, 501, "zsh", 500, "zsh")

	if found := claudeOutsideTmux("claude"); len(found) != 1 {
		t.Errorf("want the claude reported once, got %v", found)
	}
}
