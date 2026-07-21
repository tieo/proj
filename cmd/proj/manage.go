package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
	"github.com/tieo/proj/internal/tmux"
)

var rmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "delete a project directory and kill its session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		p, err := projects.Resolve(cfg.BaseDir, args[0])
		if err != nil {
			return err
		}
		fmt.Printf("delete %s and kill its tmux session? [y/N] ", p.Dir)
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return fmt.Errorf("aborted")
		}
		return removeProject(p)
	},
}

// removeProject deletes a project completely: it drops the daemon's managed
// entry first (so keep-alive cannot recreate the session between the kill and
// the directory removal), kills the tmux session, removes the directory, and
// deletes the registry entry. Shared by `proj rm` and the interactive sessions
// list's remove action.
func removeProject(p projects.Project) error {
	sessName := projects.SessionName(p.Name, p.Tags)
	ucfg := daemonConfig()
	if err := daemon.UpdateManagedState(ucfg.StatePath, func(managed daemon.ManagedState) error {
		delete(managed, sessName)
		return nil
	}); err != nil {
		return fmt.Errorf("clear daemon tracking for %q: %w", sessName, err)
	}
	_ = tmux.KillSession(sessName)
	if err := os.RemoveAll(p.Dir); err != nil {
		return err
	}
	reg, err := projects.LoadRegistry()
	if err != nil {
		return err
	}
	return reg.Delete(p.Name)
}

// printPathRunE is shared by `cd` and `path`. The shell shim wraps `proj cd`
// to capture this stdout and run `builtin cd`.
func printPathRunE(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p, err := projects.Resolve(cfg.BaseDir, args[0])
	if err != nil {
		return err
	}
	fmt.Println(p.Dir)
	return nil
}

var cdCmd = &cobra.Command{
	Use:   "cd <name>",
	Short: "print a project's path (shell shim wraps this to cd)",
	Args:  cobra.ExactArgs(1),
	RunE:  printPathRunE,
}

var pathCmd = &cobra.Command{
	Use:   "path <name>",
	Short: "print a project's path",
	Args:  cobra.ExactArgs(1),
	RunE:  printPathRunE,
}

var renameCmd = &cobra.Command{
	Use:   "rename <old> <new>",
	Short: "rename a project directory and its tmux session",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		p, err := projects.Resolve(cfg.BaseDir, args[0])
		if err != nil {
			return err
		}
		return renameProject(cfg, p, args[1], tmuxSessionOps{})
	},
}

// sessionOps is the tmux side of a rename. Renaming has to interleave session
// work with moving the directory and its history, so it is injected: a test
// can then assert the order without a tmux server.
type sessionOps interface {
	SessionForPath(dir string) string
	RenameSession(oldName, newName string) error
	RespawnShell(name, dir string) error
	RespawnSession(name, dir, command string) error
}

type tmuxSessionOps struct{}

func (tmuxSessionOps) SessionForPath(dir string) string { return tmux.SessionForPath(dir) }
func (tmuxSessionOps) RenameSession(oldName, newName string) error {
	return tmux.RenameSession(oldName, newName)
}
func (tmuxSessionOps) RespawnShell(name, dir string) error { return tmux.RespawnShell(name, dir) }
func (tmuxSessionOps) RespawnSession(name, dir, command string) error {
	return tmux.RespawnSession(name, dir, command)
}

// renameProject moves a project to newName and carries its running
// conversation across. A live session is parked on a shell before anything
// moves, because the tool holds the old directory as its working directory and
// keeps appending to the transcript filed under it: left running, it recreates
// the history being migrated out from under it and the conversation continues
// into a file the renamed project no longer reads. Once the directory and the
// history have moved, the same session (same name-independent id, so attached
// clients stay attached) relaunches the tool in the new directory, where its
// resume command finds the migrated conversation.
func renameProject(cfg config.Config, p projects.Project, newName string, ops sessionOps) error {
	if err := projects.ValidateName(newName); err != nil {
		return err
	}
	exists, err := projects.CheckNewName(cfg.BaseDir, newName)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%q already exists", newName)
	}
	spec, err := cfg.Tool(p.Tool)
	if err != nil {
		return err
	}
	newDir := filepath.Join(cfg.BaseDir, newName)
	oldSession := projects.SessionName(p.Name, p.Tags)
	newSession := projects.SessionName(newName, p.Tags)

	live := ops.SessionForPath(p.Dir)
	if live != "" {
		if err := ops.RespawnShell(live, p.Dir); err != nil {
			return fmt.Errorf("park session %q: %w", live, err)
		}
	}
	if err := os.Rename(p.Dir, newDir); err != nil {
		return err
	}
	reg, err := projects.LoadRegistry()
	if err != nil {
		return err
	}
	if err := reg.Rename(p.Name, newName); err != nil {
		return err
	}
	// Move Claude's history folder so the renamed project keeps its
	// conversation, resolving the cwd Claude actually uses (the \\wsl.localhost
	// UNC form when claude.exe is launched via interop). This runs before the
	// tool is relaunched: its resume command reads the history at startup, so a
	// later migration would hand it an empty project and start a new
	// conversation next to the one it was supposed to continue.
	sessions.MigrateHistory(sessions.Home(cfg.Claude.Home), p.Dir, newDir)
	renameManagedSession(oldSession, newSession, newDir)

	if live == "" {
		// Nothing is running under the project's directory, but a session may
		// still carry its old name (e.g. it was created and the project renamed
		// while the tool was down).
		_ = ops.RenameSession(oldSession, newSession)
	} else {
		if live != newSession {
			if err := ops.RenameSession(live, newSession); err != nil {
				return fmt.Errorf("rename session %q: %w", live, err)
			}
		}
		cmdLine := daemon.LaunchCommand(spec, cfg.Claude.Home, newName, newSession, newDir)
		if err := ops.RespawnSession(newSession, newDir, cmdLine); err != nil {
			return fmt.Errorf("relaunch session %q: %w", newSession, err)
		}
	}
	fmt.Printf("renamed %s -> %s\n", p.Dir, newDir)
	return nil
}

// renameManagedSession carries the daemon's bookkeeping (pin, keep-alive) to
// the renamed session and its new directory. Without it the old entry names a
// directory that no longer exists, which the daemon reads as a removed project
// and drops, taking the pin with it. Best effort: the rename itself has
// already happened and must not fail over bookkeeping.
func renameManagedSession(oldSession, newSession, newDir string) {
	_ = daemon.UpdateManagedState(daemonConfig().StatePath, func(managed daemon.ManagedState) error {
		ms, ok := managed[oldSession]
		if !ok {
			return nil
		}
		delete(managed, oldSession)
		ms.Name = newSession
		ms.Dir = newDir
		managed[newSession] = ms
		return nil
	})
}

var cleanDays int

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "kill tmux sessions idle longer than --days days",
	RunE: func(cmd *cobra.Command, args []string) error {
		cutoff := time.Now().Add(-time.Duration(cleanDays) * 24 * time.Hour).Unix()
		for _, s := range tmux.ListSessions() {
			if s.Activity < cutoff {
				if err := tmux.KillSession(s.Name); err == nil {
					fmt.Printf("killed %s\n", s.Name)
				}
			}
		}
		return nil
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "print proj version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("proj %s\n", Version)
	},
}

func init() {
	cleanCmd.Flags().IntVar(&cleanDays, "days", 7, "kill sessions idle longer than this")
	rootCmd.AddCommand(rmCmd, cdCmd, pathCmd, renameCmd, cleanCmd, versionCmd)
}
