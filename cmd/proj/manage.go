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
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

var killCmd = &cobra.Command{
	Use:   "kill <name>",
	Short: "kill a project's tmux session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return tmux.KillSession(projects.SessionName(args[0]))
	},
}

var rmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "delete a project directory and kill its session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		dir := projects.Find(cfg.BaseDir, args[0])
		if dir == "" {
			return fmt.Errorf("%q not found", args[0])
		}
		fmt.Printf("delete %s and kill its tmux session? [y/N] ", dir)
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return fmt.Errorf("aborted")
		}
		_ = tmux.KillSession(projects.SessionName(args[0]))
		return os.RemoveAll(dir)
	},
}

// printPathRunE is shared by `cd` and `path`. The shell shim wraps `proj cd`
// to capture this stdout and run `builtin cd`.
func printPathRunE(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir := projects.Find(cfg.BaseDir, args[0])
	if dir == "" {
		return fmt.Errorf("%q not found", args[0])
	}
	fmt.Println(dir)
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
		old := projects.Find(cfg.BaseDir, args[0])
		if old == "" {
			return fmt.Errorf("%q not found", args[0])
		}
		newDir := filepath.Join(filepath.Dir(old), args[1])
		if _, err := os.Stat(newDir); err == nil {
			return fmt.Errorf("%s already exists", newDir)
		}
		if err := os.Rename(old, newDir); err != nil {
			return err
		}
		_ = tmux.RenameSession(projects.SessionName(args[0]), projects.SessionName(args[1]))
		fmt.Printf("renamed %s → %s\n", old, newDir)
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
	rootCmd.AddCommand(killCmd, rmCmd, cdCmd, pathCmd, renameCmd, cleanCmd, versionCmd)
}
