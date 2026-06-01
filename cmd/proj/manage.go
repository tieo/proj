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
		_ = tmux.KillSession(projects.SessionName(p.Name, p.Tags))
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
		newDir := filepath.Join(cfg.BaseDir, args[1])
		if _, err := os.Stat(newDir); err == nil {
			return fmt.Errorf("%s already exists", newDir)
		}
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
		// Best-effort: move Claude's history folder so the renamed project keeps
		// its conversation. The folder is keyed on the project path; this works
		// when proj and Claude resolve that path the same way (native setups).
		// On a WSL setup that launches claude.exe via interop, Claude keys on the
		// Windows UNC path instead, so this is a harmless no-op there.
		migrateClaudeHistory(p.Dir, newDir)
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

// migrateClaudeHistory moves Claude Code's transcript folder for oldDir to the
// one for newDir, when the source exists and the target doesn't.
func migrateClaudeHistory(oldDir, newDir string) {
	oldHist := claudeProjectDir(oldDir)
	newHist := claudeProjectDir(newDir)
	if oldHist == "" || newHist == "" {
		return
	}
	if _, err := os.Stat(oldHist); err != nil {
		return // nothing to migrate
	}
	if _, err := os.Stat(newHist); err == nil {
		return // target already present; leave both alone
	}
	_ = os.Rename(oldHist, newHist)
}

// claudeProjectDir returns ~/.claude/projects/<encoded(workDir)>, where the
// encoding replaces every non-alphanumeric rune with '-' (matching Claude Code).
func claudeProjectDir(workDir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	encoded := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, workDir)
	return filepath.Join(home, ".claude", "projects", encoded)
}

func init() {
	cleanCmd.Flags().IntVar(&cleanDays, "days", 7, "kill sessions idle longer than this")
	rootCmd.AddCommand(rmCmd, cdCmd, pathCmd, renameCmd, cleanCmd, versionCmd)
}
