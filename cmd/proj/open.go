package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/shellout"
	"github.com/tieo/proj/internal/tmux"
)

func runOpen(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch {
	case len(args) == 0:
		return runList(cmd, args)
	case len(args) == 1:
		return openExisting(cfg, args[0])
	default:
		// Last argument is the name, the rest are tags (matches `proj new`).
		return createWithTags(cfg, args[len(args)-1], args[:len(args)-1])
	}
}

func openExisting(cfg config.Config, name string) error {
	p, err := projects.FindByName(cfg.BaseDir, name)
	if err != nil {
		return err
	}
	return openInTmux(cfg, p)
}

func createWithTags(cfg config.Config, name string, tags []string) error {
	if err := projects.ValidateName(name); err != nil {
		return err
	}
	dir := filepath.Join(cfg.BaseDir, name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%q already exists; use `proj tag add %s ...` to add tags", name, name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	reg, err := projects.LoadRegistry()
	if err != nil {
		return err
	}
	if err := reg.SetTags(name, tags); err != nil {
		return err
	}
	p, err := projects.FindByName(cfg.BaseDir, name)
	if err != nil {
		return err
	}
	return openInTmux(cfg, p)
}

func openInTmux(cfg config.Config, p projects.Project) error {
	session := projects.SessionName(p.Name, p.Tags)
	if !tmux.HasSession(session) {
		cmdLine := strings.NewReplacer("{name}", shellout.Quote(p.Name), "{dir}", shellout.Quote(p.Dir)).Replace(cfg.Claude.Command)
		if cfg.Claude.ResumeFlag != "" && projects.HasHistory(p.Dir) {
			cmdLine += " " + cfg.Claude.ResumeFlag
		}
		// Run claude as the pane's program (then drop to a shell in the
		// project dir on exit) rather than typing it into an interactive
		// shell, so no prompt or echoed command is left above its UI.
		launch := cmdLine + `; exec "${SHELL:-bash}"`
		if _, err := tmux.NewSession(session, p.Dir, launch); err != nil {
			return fmt.Errorf("create tmux session: %w", err)
		}
	}
	if headless {
		return nil
	}
	return tmux.Attach(session)
}
