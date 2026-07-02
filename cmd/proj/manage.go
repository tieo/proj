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
		// Drop the daemon's tracking first, so it cannot recreate the session
		// between the kill and the directory removal. Without this the daemon
		// keeps the managed entry and, with keep-alive on, respawns the removed
		// project's session every poll. Best effort: the daemon also drops a
		// tracked session whose directory is gone, so a lost race still resolves.
		sessName := projects.SessionName(p.Name, p.Tags)
		ucfg := daemonConfig()
		managed := daemon.LoadManagedState(ucfg.StatePath)
		if _, tracked := managed[sessName]; tracked {
			delete(managed, sessName)
			if err := daemon.SaveManagedState(ucfg.StatePath, managed); err != nil {
				return fmt.Errorf("clear daemon tracking for %q: %w", sessName, err)
			}
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
	},
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
		if err := projects.ValidateName(args[1]); err != nil {
			return err
		}
		exists, err := projects.CheckNewName(cfg.BaseDir, args[1])
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%q already exists", args[1])
		}
		newDir := filepath.Join(cfg.BaseDir, args[1])
		oldSession := projects.SessionName(p.Name, p.Tags)
		if err := os.Rename(p.Dir, newDir); err != nil {
			return err
		}
		reg, err := projects.LoadRegistry()
		if err != nil {
			return err
		}
		if err := reg.Rename(p.Name, args[1]); err != nil {
			return err
		}
		newSession := projects.SessionName(args[1], p.Tags)
		_ = tmux.RenameSession(oldSession, newSession)
		// Move Claude's history folder so the renamed project keeps its
		// conversation, resolving the cwd Claude actually uses (the \\wsl.localhost
		// UNC form when claude.exe is launched via interop).
		sessions.MigrateHistory(sessions.Home(cfg.Claude.Home), p.Dir, newDir)
		fmt.Printf("renamed %s -> %s\n", p.Dir, newDir)
		return nil
	},
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
