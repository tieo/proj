package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions [project]",
	Short: "list existing Claude sessions; resume one with `proj sessions resume <id>`",
	Long: `List the Claude Code sessions on disk, grouped by project and newest first.
proj only indexes them; viewing is handed off to Claude itself. With a project
name or prefix, only that project's sessions are shown.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessions,
}

var sessionsResumeCmd = &cobra.Command{
	Use:   "resume <id>",
	Short: "open a Claude session by id or prefix (hands off to `claude --resume`)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsResume,
}

func init() {
	sessionsCmd.AddCommand(sessionsResumeCmd)
	rootCmd.AddCommand(sessionsCmd)
}

func runSessions(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	home := sessions.Home(cfg.Claude.Home)
	all, err := sessions.List(home)
	if err != nil {
		return err
	}

	// Map each proj project's Claude cwd to its name, to label "mine" vs loose.
	nameByCwd := map[string]string{}
	for _, p := range projects.All(cfg.BaseDir) {
		nameByCwd[sessions.CwdForDir(p.Dir, all)] = p.Name
	}

	var filterCwd string
	if len(args) == 1 {
		p, err := projects.Resolve(cfg.BaseDir, args[0])
		if err != nil {
			return err
		}
		filterCwd = sessions.CwdForDir(p.Dir, all)
	}

	// Hide sessions older than the cutoff unless --all (same knob as `proj list`).
	var cutoff time.Time
	if !listAll && cfg.List.MaxAgeDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -cfg.List.MaxAgeDays)
	}

	now := time.Now()
	hidden := 0
	type srow struct {
		id, age, project, title string
		msgs                    int
		managed                 bool
	}
	var rows []srow
	for _, s := range all {
		if filterCwd != "" && s.Cwd != filterCwd {
			continue
		}
		if !cutoff.IsZero() && s.Modified.Before(cutoff) {
			hidden++
			continue
		}
		name, managed := nameByCwd[s.Cwd]
		if !managed {
			name = dirBase(s.Cwd)
		}
		rows = append(rows, srow{s.ID[:8], formatAgo(now.Sub(s.Modified)), name, s.Title, s.Messages, managed})
	}

	if len(rows) == 0 {
		if hidden > 0 {
			fmt.Printf("no recent Claude sessions (%d older; --all to show)\n", hidden)
		} else {
			fmt.Println("no Claude sessions found")
		}
		return nil
	}

	fmt.Printf("  \033[90m%-8s %9s %6s  %-16s %s\033[0m\n", "ID", "AGE", "MSGS", "PROJECT", "TITLE")
	for _, r := range rows {
		cell := truncPad(r.project, 16)
		if r.managed {
			cell = "\033[32m" + cell + "\033[0m"
		} else {
			cell = "\033[90m" + cell + "\033[0m"
		}
		fmt.Printf("  %-8s %9s %6d  %s %s\n", r.id, r.age, r.msgs, cell, r.title)
	}
	if hidden > 0 {
		fmt.Printf("\n  \033[90m%d older session(s) hidden; --all to show\033[0m\n", hidden)
	}
	return nil
}

// dirBase is filepath.Base for both / and \ separated paths (the cwd may be a
// Windows or UNC path even though proj runs on Linux).
func dirBase(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// truncPad fits s to exactly w runes, padding with spaces or truncating with an
// ellipsis, so a following colored column still lines up.
func truncPad(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		return string(r[:w-1]) + "…"
	}
	return s + strings.Repeat(" ", w-len(r))
}

func runSessionsResume(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	home := sessions.Home(cfg.Claude.Home)
	s, err := sessions.Find(home, args[0])
	if err != nil {
		return err
	}
	dir := sessions.UNCToWSL(s.Cwd)
	if dir == "" {
		dir = s.Cwd
	}
	c := exec.Command("claude", "--resume", s.ID, "--dangerously-skip-permissions")
	c.Dir = dir
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("could not launch claude (run manually: cd %q && claude --resume %s): %w", dir, s.ID, err)
	}
	return nil
}
