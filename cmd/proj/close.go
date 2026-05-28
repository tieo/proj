// proj close [session] — mark a session as intentionally closed and kill it.
//
// Use this instead of `tmux kill-session` when you want proj unreset to know
// the close was deliberate. Without this (or the shell exit trap in proj.zsh /
// proj.bash / proj.fish), a vanished keep-alive or pinned session will be
// automatically recreated by the daemon.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/tmux"
	"github.com/tieo/proj/internal/unreset"
)

var closeCmd = &cobra.Command{
	Use:   "close [session]",
	Short: "mark a session as intentionally closed and kill it",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runClose,
}

func init() {
	rootCmd.AddCommand(closeCmd)
}

func runClose(cmd *cobra.Command, args []string) error {
	name, err := resolveCloseSession(args)
	if err != nil {
		return err
	}
	cfg := unresetConfig()
	managed := unreset.LoadManagedState(cfg.StatePath)
	ms := managed[name]
	ms.Name = name
	ms.Closed = true
	managed[name] = ms
	if err := unreset.SaveManagedState(cfg.StatePath, managed); err != nil {
		return fmt.Errorf("save managed state: %w", err)
	}
	if err := tmux.KillSession(name); err != nil {
		return fmt.Errorf("kill session %q: %w", name, err)
	}
	fmt.Printf("closed %s\n", name)
	return nil
}

// resolveCloseSession returns the session name from args, or from the current
// tmux session if no arg is given.
func resolveCloseSession(args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return args[0], nil
	}
	if os.Getenv("TMUX") == "" {
		return "", fmt.Errorf("no session name given and not inside a tmux session")
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return "", fmt.Errorf("get current tmux session: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
