package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
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
		// Already running under the right name; re-apply skills below.
	case "":
		spec, err := cfg.Tool(p.Tool)
		if err != nil {
			return err
		}
		// Run the tool as the pane's program with no trailing shell. When it
		// exits, the pane's program ends and tmux (with remain-on-exit off, set
		// in NewSession) tears the single-window session down, so a finished
		// project leaves nothing behind, and the next `proj <name>` launches a
		// fresh resume instead of re-attaching a leftover shell. Surviving a
		// closed terminal is handled at the server level (see tmux.NewSession /
		// ensureServer), not by keeping a shell in the pane.
		//
		// LaunchCommand gates the resume command on real prior history (resume
		// on an empty history is an error that tears the fresh pane down) and
		// appends the clean-close mark that keeps keep-alive from resurrecting
		// a deliberately closed session; see its doc for the && semantics.
		cmdLine := daemon.LaunchCommand(spec, cfg.Claude.Home, p.Name, session, p.Dir)
		if _, err := tmux.NewSession(session, p.Dir, cmdLine); err != nil {
			return fmt.Errorf("create tmux session: %w", err)
		}
	default:
		// A session for this dir exists under a stale (old-tag) name; bring its
		// name in line with the current tags before attaching.
		_ = tmux.RenameSession(existing, session)
	}
	// Per-project skills (e.g. "caveman") fire on every open, including
	// re-attach to a live session - that way "this project always runs in
	// caveman mode" stays consistent without the user typing it each time.
	// Waits for claude's input box to settle so commands don't land mid-init
	// (or on the trust-folder prompt for a brand-new dir). Skills are Claude
	// Code slash commands; other tools don't get them.
	if len(p.Skills) > 0 && daemon.ToolName(p.Tool) == config.DefaultTool {
		tmux.ApplySlashCommands(session, p.Skills, 30*time.Second)
	}
	if headless {
		// Nothing is attached, so the session itself is the only evidence the
		// open worked. Name it, so a caller driving proj from a script reads a
		// result rather than silence.
		fmt.Printf("opened %s in session %s\n", p.Name, session)
		return nil
	}
	return tmux.Attach(session)
}
