// proj close [project]; mark a project's session as intentionally closed and
// kill it.
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

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
	"github.com/tieo/proj/internal/unreset"
)

var closeCmd = &cobra.Command{
	Use:   "close [project]",
	Short: "close a project's session: mark it intentionally closed and kill it",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runClose,
}

func init() {
	rootCmd.AddCommand(closeCmd)
}

func runClose(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	name, err := resolveCloseSession(cfg.BaseDir, args)
	if err != nil {
		return err
	}
	ucfg := unresetConfig()
	managed := unreset.LoadManagedState(ucfg.StatePath)
	ms := managed[name]
	ms.Name = name
	ms.ExitedCleanly = true
	managed[name] = ms
	if err := unreset.SaveManagedState(ucfg.StatePath, managed); err != nil {
		return fmt.Errorf("save managed state: %w", err)
	}
	if err := tmux.KillSession(name); err != nil {
		return fmt.Errorf("kill session %q: %w", name, err)
	}
	fmt.Printf("closed %s\n", name)
	return nil
}

// resolveCloseSession returns the tmux session name to close. With an argument
// it is treated as a project name or unique prefix (like `proj` open) and
// resolved to that project's open session; with no argument it is the current
// tmux session.
func resolveCloseSession(baseDir string, args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		p, err := projects.Resolve(baseDir, args[0])
		if err != nil {
			return "", err
		}
		session := tmux.SessionForPath(p.Dir)
		if session == "" {
			return "", fmt.Errorf("%q has no open session", p.Name)
		}
		return session, nil
	}
	if os.Getenv("TMUX") == "" {
		return "", fmt.Errorf("no project name given and not inside a tmux session")
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return "", fmt.Errorf("get current tmux session: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
