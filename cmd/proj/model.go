package main

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

// `proj model` sets the Claude model across the fleet. Claude Code's /model
// switches a running session live and saves the choice as the default for new
// sessions, so this drives /model into the panes it targets: one command puts
// the whole fleet (or the sessions matching a pattern) on a model at once.

var modelCmd = &cobra.Command{
	Use:   "model [<pattern>] <model>",
	Short: "switch the Claude model across running sessions",
	Long: `Switch the Claude model of running sessions by driving /model into them.

  proj model                     show each live session's model
  proj model claude-opus-5       switch every live Claude session, and set it
                                 as the default for new sessions too
  proj model '*opus*' claude-opus-5
                                 switch only the sessions whose current model
                                 matches the glob (here, any opus)

The model is an alias for the latest of a family ("opus", "sonnet") or a full
id ("claude-opus-5"). /model saves the choice as the default, so new sessions
follow without a restart; the running sessions are switched in place.`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runModel,
}

func init() {
	rootCmd.AddCommand(modelCmd)
}

// paneModel is one live Claude session and the model it currently runs.
type paneModel struct {
	session string
	id      string
	dir     string
	model   string
}

func runModel(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	panes := liveClaudeModels(cfg)

	if len(args) == 0 {
		if len(panes) == 0 {
			fmt.Println("no live Claude sessions")
			return nil
		}
		for _, p := range panes {
			m := p.model
			if m == "" {
				m = "default"
			}
			fmt.Printf("  %-24s %s\n", p.session, m)
		}
		return nil
	}

	glob, model := "*", args[0]
	if len(args) == 2 {
		glob, model = args[0], args[1]
	}
	if _, err := path.Match(glob, ""); err != nil {
		return fmt.Errorf("bad pattern %q: %w", glob, err)
	}

	var switched, skipped []string
	for _, p := range panes {
		ok, err := path.Match(glob, p.model)
		if err != nil {
			return err
		}
		if !ok {
			skipped = append(skipped, p.session)
			continue
		}
		if err := setPaneModel(cfg, p.id, model); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", p.session, err)
			continue
		}
		switched = append(switched, p.session)
	}

	if len(switched) == 0 {
		fmt.Printf("no live session matched %q\n", glob)
	} else {
		fmt.Printf("switched to %s: %s\n", model, joinSorted(switched))
	}
	// One arg is the blanket form, so it also pins the default for sessions that
	// are not running yet. /model already saved it from the panes above, but do
	// it directly too so the default is set even when nothing was live to carry
	// it.
	if len(args) == 1 {
		if err := setDefaultModel(cfg, model); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: set default model: %v\n", err)
		} else {
			fmt.Printf("default for new sessions: %s\n", model)
		}
	}
	return nil
}

// liveClaudeModels returns the running Claude sessions with the model each one
// currently runs, one per session (the first pane wins).
func liveClaudeModels(cfg config.Config) []paneModel {
	reg, _ := projects.LoadRegistry()
	seen := map[string]bool{}
	var out []paneModel
	for _, pane := range tmux.ListPanes() {
		if seen[pane.Session] {
			continue
		}
		dir := tmux.PaneCurrentPath(pane.ID)
		if daemon.ToolName(reg.Tool(filepath.Base(dir))) != config.DefaultTool {
			continue
		}
		seen[pane.Session] = true
		model := daemon.ConfiguredClaudeModel(cfg.Claude.Home, dir, cfg.Claude.Command)
		if model == "" {
			model = daemon.ModelFromDir(cfg.Claude.Home, dir)
		}
		out = append(out, paneModel{session: pane.Session, id: pane.ID, dir: dir, model: model})
	}
	return out
}

// setPaneModel runs /model in a pane to switch its session live. The command
// and its Enter go as separate keystrokes with a settle gap, so the slash
// command runs rather than merging with the autocomplete expansion (the same
// care the daemon takes with /rc). It refuses to type over a user's draft.
func setPaneModel(cfg config.Config, paneID, model string) error {
	if daemon.ComposerHasDraft(tmux.CapturePaneEsc(paneID)) {
		return fmt.Errorf("has an unsent draft; left alone")
	}
	if err := tmux.SendLiteral(paneID, "/model "+model); err != nil {
		return err
	}
	time.Sleep(daemonConfig().DismissGap)
	return tmux.SendKey(paneID, "Enter")
}

// setDefaultModel pins model as the default for new sessions by writing it into
// the settings.json Claude Code reads, leaving every other setting in place.
func setDefaultModel(cfg config.Config, model string) error {
	path := donerSettingsPath(cfg)
	root, err := readSettings(path)
	if err != nil {
		return err
	}
	root["model"] = model
	return writeSettings(path, root)
}

func joinSorted(names []string) string {
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
