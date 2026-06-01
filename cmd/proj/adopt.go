package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
)

var adoptCmd = &cobra.Command{
	Use:   "adopt [session-id] [project]",
	Short: "copy an existing Claude session into a proj project; interactive when args are omitted",
	Long: `Copy a Claude session transcript into a proj project's history (the original is
left in place), rewriting its working directory to the project's so Claude
treats it as belonging there. This also handles moving a Windows-path session
onto its WSL project path, and pulling a stranded session back onto a renamed
project. The project's continue pointer is updated, so ` + "`proj <project>`" + ` resumes it.

With no arguments, pick a session and a target project interactively. Pass a
session id (or prefix) to skip the session picker, and a project to skip both.`,
	Args: cobra.MaximumNArgs(2),
	RunE: runAdopt,
}

func init() {
	rootCmd.AddCommand(adoptCmd)
}

func runAdopt(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	home := sessions.Home(cfg.Claude.Home)
	all, err := sessions.List(home)
	if err != nil {
		return err
	}

	var s sessions.Session
	if len(args) >= 1 {
		if s, err = sessions.FindIn(all, args[0]); err != nil {
			return err
		}
	} else if s, err = pickSession(all, cfg); err != nil {
		return err
	}

	var p projects.Project
	if len(args) >= 2 {
		if p, err = projects.Resolve(cfg.BaseDir, args[1]); err != nil {
			return err
		}
	} else if p, err = pickProject(cfg); err != nil {
		return err
	}

	targetCwd := sessions.CwdForDir(p.Dir, all)
	if s.Cwd == targetCwd {
		return fmt.Errorf("session %s already belongs to %s", s.ID[:8], p.Name)
	}
	if _, err := sessions.Adopt(home, s, targetCwd); err != nil {
		return err
	}
	fmt.Printf("adopted %s into %s\n  open with: proj %s\n", s.ID[:8], p.Name, p.Name)
	return nil
}

// pickSession prints a numbered, recency-sorted session list and reads a choice.
func pickSession(all []sessions.Session, cfg config.Config) (sessions.Session, error) {
	var cutoff time.Time
	if !listAll && cfg.List.MaxAgeDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -cfg.List.MaxAgeDays)
	}
	nameByCwd := map[string]string{}
	for _, pr := range projects.All(cfg.BaseDir) {
		nameByCwd[sessions.CwdForDir(pr.Dir, all)] = pr.Name
	}
	now := time.Now()
	var shown []sessions.Session
	for _, s := range all {
		if !cutoff.IsZero() && s.Modified.Before(cutoff) {
			continue
		}
		shown = append(shown, s)
	}
	if len(shown) == 0 {
		return sessions.Session{}, fmt.Errorf("no recent sessions (use --all to include older ones)")
	}
	for i, s := range shown {
		name, managed := nameByCwd[s.Cwd]
		if !managed {
			name = dirBase(s.Cwd)
		}
		fmt.Printf("  %2d  %-8s %9s %5d  %-16s %s\n",
			i+1, s.ID[:8], formatAgo(now.Sub(s.Modified)), s.Messages, truncPad(name, 16), s.Title)
	}
	i, err := promptIndex("adopt which session", len(shown))
	if err != nil {
		return sessions.Session{}, err
	}
	return shown[i], nil
}

// pickProject prints a numbered project list and reads a choice.
func pickProject(cfg config.Config) (projects.Project, error) {
	all := projects.All(cfg.BaseDir)
	if len(all) == 0 {
		return projects.Project{}, fmt.Errorf("no projects under %s", cfg.BaseDir)
	}
	for i, p := range all {
		fmt.Printf("  %2d  %s\n", i+1, p.Name)
	}
	i, err := promptIndex("into which project", len(all))
	if err != nil {
		return projects.Project{}, err
	}
	return projects.FindByName(cfg.BaseDir, all[i].Name)
}

func promptIndex(prompt string, n int) (int, error) {
	fmt.Printf("%s? [1-%d, q to cancel]: ", prompt, n)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return 0, fmt.Errorf("cancelled")
	}
	in := strings.TrimSpace(sc.Text())
	if in == "" || in == "q" {
		return 0, fmt.Errorf("cancelled")
	}
	i, err := strconv.Atoi(in)
	if err != nil || i < 1 || i > n {
		return 0, fmt.Errorf("invalid selection %q", in)
	}
	return i - 1, nil
}
