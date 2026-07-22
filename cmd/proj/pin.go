package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

var pinCmd = &cobra.Command{
	Use:   "pin [project...]",
	Short: "pin projects so the daemon always recreates their sessions",
	Args:  cobra.ArbitraryArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return setPinnedAll(args, true) },
}

var unpinCmd = &cobra.Command{
	Use:   "unpin [project...]",
	Short: "remove the pinned flag from projects",
	Args:  cobra.ArbitraryArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return setPinnedAll(args, false) },
}

// setPinnedAll applies the flag to every named project, or to the current tmux
// session when no name is given.
func setPinnedAll(args []string, pinned bool) error {
	if len(args) <= 1 {
		return setPinned(args, pinned)
	}
	return eachTarget(args, func(name string) error { return setPinned([]string{name}, pinned) })
}

func init() {
	rootCmd.AddCommand(pinCmd, unpinCmd)
}

// setPinned resolves a project (by name or unique prefix, or the current tmux
// session when no argument is given) and sets or clears its pinned flag in the
// daemon's managed state. The session name and the project directory are
// recorded so the daemon can recreate the session even when it isn't running.
func setPinned(args []string, pinned bool) error {
	session, dir, label, err := resolvePinTarget(args)
	if err != nil {
		return err
	}
	cfg := daemonConfig()
	if err := daemon.UpdateManagedState(cfg.StatePath, func(managed daemon.ManagedState) error {
		ms := managed[session]
		ms.Name = session
		ms.Pinned = pinned
		if pinned && dir != "" {
			ms.Dir = dir
		}
		managed[session] = ms
		return nil
	}); err != nil {
		return err
	}
	verb := "pinned"
	if !pinned {
		verb = "unpinned"
	}
	fmt.Printf("%s %s\n", verb, label)
	return nil
}

// resolvePinTarget returns the tmux session name, project directory, and a
// display label for the pin target: a named project (name or unique prefix), or
// the current tmux session when no argument is given.
func resolvePinTarget(args []string) (session, dir, label string, err error) {
	if len(args) > 0 && args[0] != "" {
		cfg, e := config.Load()
		if e != nil {
			return "", "", "", e
		}
		p, e := projects.Resolve(cfg.BaseDir, args[0])
		if e != nil {
			return "", "", "", e
		}
		session = tmux.SessionForPath(p.Dir)
		if session == "" {
			session = projects.SessionName(p.Name, p.Tags)
		}
		return session, p.Dir, p.Name, nil
	}
	if os.Getenv("TMUX") == "" {
		return "", "", "", fmt.Errorf("no project given and not inside a tmux session")
	}
	session = currentSessionName()
	if session == "" {
		return "", "", "", fmt.Errorf("could not determine the current tmux session")
	}
	out, _ := exec.Command("tmux", "display-message", "-p", "#{session_path}").Output()
	return session, strings.TrimSpace(string(out)), session, nil
}
