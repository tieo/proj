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
	switch len(args) {
	case 0:
		return runList(cmd, args)
	case 1:
		return openExisting(cfg, args[0])
	case 2:
		return openOrCreate(cfg, args[0], args[1])
	}
	return nil
}

func openExisting(cfg config.Config, name string) error {
	dir, err := resolveProjectDir(cfg.BaseDir, name)
	if err != nil {
		return err
	}
	return openInTmux(cfg, name, dir)
}

func openOrCreate(cfg config.Config, lang, name string) error {
	dir := filepath.Join(cfg.BaseDir, lang, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return openInTmux(cfg, name, dir)
}

func openInTmux(cfg config.Config, name, dir string) error {
	session := projects.SessionNameForDir(dir)
	if !tmux.HasSession(session) {
		pane, err := tmux.NewSession(session, dir)
		if err != nil {
			return fmt.Errorf("create tmux session: %w", err)
		}
		cmdLine := strings.NewReplacer("{name}", name, "{dir}", dir).Replace(cfg.Claude.Command)
		if cfg.Claude.ResumeFlag != "" && projects.HasHistory(dir) {
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
