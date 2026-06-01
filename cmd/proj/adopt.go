package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/sessions"
)

var adoptCmd = &cobra.Command{
	Use:   "adopt <session-id> <project>",
	Short: "copy an existing Claude session into a proj project so the project resumes it",
	Long: `Copy a Claude session transcript into a proj project's history (the original is
left in place), rewriting its working directory to the project's so Claude
treats it as belonging there. This also handles moving a Windows-path session
onto its WSL project path. The project's continue pointer is updated, so ` +
		"`proj <project>`" + ` resumes the adopted session.`,
	Args: cobra.ExactArgs(2),
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
	p, err := projects.Resolve(cfg.BaseDir, args[1])
	if err != nil {
		return err
	}
	home := sessions.Home(cfg.Claude.Home)
	all, err := sessions.List(home)
	if err != nil {
		return err
	}
	s, err := sessions.FindIn(all, args[0])
	if err != nil {
		return err
	}
	targetCwd := sessions.CwdForDir(p.Dir, all)
	if s.Cwd == targetCwd {
		return fmt.Errorf("session %s already belongs to %s", s.ID[:8], p.Name)
	}
	if _, err := sessions.Adopt(home, s, targetCwd); err != nil {
		return err
	}
	fmt.Printf("adopted %s into %s\n", s.ID[:8], p.Name)
	fmt.Printf("  from: %s\n", s.Cwd)
	fmt.Printf("  to:   %s\n", targetCwd)
	fmt.Printf("  open with: proj %s\n", p.Name)
	return nil
}
