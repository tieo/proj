package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/shellout"
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
		// Open by name, with prefix matching as a typing shortcut. No tags are
		// accepted here: project names are unique, so the name alone identifies
		// the project. Tags only make sense at creation (`proj new`) or via
		// `proj tag add/rm`.
		p, err := projects.Resolve(cfg.BaseDir, args[0])
		if err != nil {
			return err
		}
		return openInTmux(cfg, p)
	default:
		return fmt.Errorf("open takes a single project name (got %d args); names are unique, so tags aren't used here. Set them with `proj new` or `proj tag`", len(args))
	}
}

func openInTmux(cfg config.Config, p projects.Project) error {
	session := projects.SessionName(p.Name, p.Tags)
	// Find the project's session by directory, not by the computed name: the
	// name carries tags and can drift, but the dir is the project's true
	// identity (and what Claude keys history on). This way a tag change, or a
	// rename that never happened, can't strand the session or spawn a second
	// one for the same dir.
	switch existing := tmux.SessionForPath(p.Dir); existing {
	case session:
		// Already running under the right name; fall through to attach.
	case "":
		cmdLine := strings.NewReplacer("{name}", shellout.Quote(p.Name), "{dir}", shellout.Quote(p.Dir)).Replace(cfg.Claude.Command)
		// Append the resume flag only when there's a transcript to resume.
		// Claude's --continue is NOT a no-op on an empty history: it exits
		// with "No deferred tool marker found in the resumed session", which
		// tears the brand-new pane down before anyone can attach. So gate it
		// on HasHistory. The WSL probe was unreliable for interop launches,
		// but Linux native (the common case) is correct, and a missed
		// resume there is recoverable; a launch failure here is not.
		if cfg.Claude.ResumeFlag != "" && daemon.HasHistory(p.Dir) {
			cmdLine += " " + cfg.Claude.ResumeFlag
		}
		// Run claude as the pane's program with no trailing shell. When claude
		// exits, the pane's program ends and tmux (with remain-on-exit off, set
		// in NewSession) tears the single-window session down, so a finished
		// project leaves nothing behind, and the next `proj <name>` launches a
		// fresh `claude -c` instead of re-attaching a leftover shell. Surviving a
		// closed terminal is handled at the server level (see tmux.NewSession /
		// ensureServer), not by keeping a shell in the pane.
		if _, err := tmux.NewSession(session, p.Dir, cmdLine); err != nil {
			return fmt.Errorf("create tmux session: %w", err)
		}
	default:
		// A session for this dir exists under a stale (old-tag) name; bring its
		// name in line with the current tags before attaching.
		_ = tmux.RenameSession(existing, session)
	}
	if headless {
		return nil
	}
	return tmux.Attach(session)
}
