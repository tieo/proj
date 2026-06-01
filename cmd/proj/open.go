package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
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
		return createWithTags(cfg, args[0], args[1:])
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
	dir := filepath.Join(cfg.BaseDir, name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%q already exists; use `proj tag add %s ...` to add tags", name, name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := projects.SaveTags(dir, tags); err != nil {
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
		pane, err := tmux.NewSession(session, p.Dir)
		if err != nil {
			return fmt.Errorf("create tmux session: %w", err)
		}
		cmdLine := strings.NewReplacer("{name}", p.Name, "{dir}", p.Dir).Replace(cfg.Claude.Command)
		if cfg.Claude.ResumeFlag != "" && projects.HasHistory(p.Dir) {
			cmdLine += " " + cfg.Claude.ResumeFlag
		}
		if err := tmux.SendKeys(pane, cmdLine); err != nil {
			return fmt.Errorf("send-keys: %w", err)
		}
	}
	if headless {
		return nil
	}
	return tmux.Attach(session)
}
