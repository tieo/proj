// proj close [project]; mark a project's session as intentionally closed and
// kill it.
//
// Use this instead of `tmux kill-session` when you want proj daemon to know
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
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

var closeForce bool

var closeCmd = &cobra.Command{
	Use:   "close [project...]",
	Short: "close a project's session: mark it intentionally closed and kill it",
	Long: `Mark a project's session as intentionally closed and kill it, so the daemon
does not treat it as a vanished keep-alive session and recreate it. With no
argument, closes the current tmux session; with several, closes each of them
and reports the ones it could not.

With --force, also unpin the project, so even a pinned session stays closed
instead of being recreated on the next daemon tick.`,
	Args: cobra.ArbitraryArgs,
	RunE: runClose,
}

func init() {
	closeCmd.Flags().BoolVarP(&closeForce, "force", "f", false, "also unpin, so a pinned project stays closed")
	rootCmd.AddCommand(closeCmd)
}

func runClose(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		name, err := resolveCloseSession(cfg.BaseDir, nil)
		if err != nil {
			return err
		}
		if err := closeSession(name, closeForce); err != nil {
			return err
		}
		fmt.Printf("closed %s\n", name)
		return nil
	}
	return eachTarget(args, func(arg string) error {
		name, err := resolveCloseSession(cfg.BaseDir, []string{arg})
		if err != nil {
			return err
		}
		if err := closeSession(name, closeForce); err != nil {
			return err
		}
		fmt.Printf("closed %s\n", name)
		return nil
	})
}

// closeSession marks a session cleanly exited in the daemon's managed state
// (so keep-alive does not recreate it) and kills its tmux session. unpin also
// clears the pinned flag, so a pinned project stays closed. Shared by
// `proj close` and the interactive sessions list's stop action.
func closeSession(name string, unpin bool) error {
	ucfg := daemonConfig()
	if err := daemon.UpdateManagedState(ucfg.StatePath, func(managed daemon.ManagedState) error {
		ms := managed[name]
		ms.Name = name
		ms.ExitedCleanly = true
		if unpin {
			ms.Pinned = false
		}
		managed[name] = ms
		return nil
	}); err != nil {
		return fmt.Errorf("save managed state: %w", err)
	}
	if err := tmux.KillSession(name); err != nil {
		return fmt.Errorf("kill session %q: %w", name, err)
	}
	return nil
}

// resolveCloseSession returns the tmux session name to close. With an argument
// it is a live session name, or else a project name or unique prefix (like
// `proj` open) resolved to that project's open session; with no argument it is
// the current tmux session. A session outside base_dir belongs to no project,
// so matching the live name first is what makes it closable at all: without it
// keep-alive recreates the session every tick and nothing can retire it.
func resolveCloseSession(baseDir string, args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		for _, s := range tmux.ListSessions() {
			if s.Name == args[0] {
				return s.Name, nil
			}
		}
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
