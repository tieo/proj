package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	} else if s, err = pickSession(cfg, all); err != nil {
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

// pickSession shows the same table as `proj sessions`, with a moving cursor.
func pickSession(cfg config.Config, all []sessions.Session) (sessions.Session, error) {
	header, lines, shown, _ := sessionLines(cfg, all, "")
	if len(shown) == 0 {
		return sessions.Session{}, fmt.Errorf("no recent sessions (use --all to include older ones)")
	}
	i := selectFromList(header, lines)
	if i < 0 {
		return sessions.Session{}, fmt.Errorf("cancelled")
	}
	return shown[i], nil
}

// pickProject defaults to creating a new project (type a name) and lets the
// user arrow down to adopt into an existing one instead.
func pickProject(cfg config.Config) (projects.Project, error) {
	all := projects.All(cfg.BaseDir)
	lines := make([]string, len(all))
	for i, p := range all {
		lines[i] = p.Name
		if len(p.Tags) > 0 {
			lines[i] += "  \033[90m" + strings.Join(p.Tags, " ") + "\033[0m"
		}
	}
	text, idx, ok := selectOrCreate("adopt into project (type a new name, ↓ to pick existing):", lines)
	if !ok {
		return projects.Project{}, fmt.Errorf("cancelled")
	}
	if idx >= 0 {
		return projects.FindByName(cfg.BaseDir, all[idx].Name)
	}
	if err := projects.ValidateName(text); err != nil {
		return projects.Project{}, err
	}
	dir := filepath.Join(cfg.BaseDir, text)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return projects.Project{}, err
		}
	}
	return projects.FindByName(cfg.BaseDir, text)
}
