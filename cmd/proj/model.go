package main

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

// `proj model` sets the Claude model for the whole fleet by writing Claude
// Code's own default, the same key /model saves. It deliberately does not type
// into panes: a running session holds its model in memory, and switching it by
// keystroke means driving a slash command past an autocomplete that fuzzy-
// matches and a "switch model?" confirmation, which is how a fleet-wide switch
// left four sessions sitting on an unanswered picker.
//
// So the split is: the default changes here, and a running session keeps what
// it started with until it is restarted or the user runs /model in it. The
// sessions that need that are named rather than silently left behind.
//
// Listing models is `proj list`, which already has a model column.

var modelCmd = &cobra.Command{
	Use:   "model <model>",
	Short: "set the Claude model new sessions start with",
	Long: `Set the model Claude Code starts sessions with, for every project at once.

The model is an alias for the latest of a family ("opus", "sonnet") or a full
id ("claude-opus-5"). It is written as Claude Code's default, so every session
started afterwards runs it.

A session that is already running keeps the model it started with; the ones
that differ are listed, and pick the new model up when restarted
(` + "`proj close <name> && proj <name>`" + `, which resumes the conversation) or
when /model is run inside them. Current models are shown by ` + "`proj list`" + `.`,
	Args: cobra.ExactArgs(1),
	RunE: runModel,
}

func init() {
	rootCmd.AddCommand(modelCmd)
}

func runModel(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	model := args[0]
	if err := setDefaultModel(cfg, model); err != nil {
		return err
	}
	fmt.Printf("model: %s (new sessions)\n", model)

	if stale := sessionsOnOtherModel(cfg, model); len(stale) > 0 {
		fmt.Printf("still running their old model: %s\n", joinSorted(stale))
		fmt.Println("restart one with `proj close <name> && proj <name>`, or run /model inside it")
	}
	return nil
}

// setDefaultModel pins model as Claude Code's default by writing it into the
// settings.json it reads, leaving every other setting (the doner hook included)
// in place.
func setDefaultModel(cfg config.Config, model string) error {
	path := donerSettingsPath(cfg)
	root, err := readSettings(path)
	if err != nil {
		return err
	}
	root["model"] = model
	return writeSettings(path, root)
}

// sessionsOnOtherModel names the live Claude sessions whose recorded model is
// not the one just set. A session with no recorded model is left out: nothing
// says it differs, and naming it would only add noise.
func sessionsOnOtherModel(cfg config.Config, model string) []string {
	reg, _ := projects.LoadRegistry()
	seen := map[string]bool{}
	var out []string
	for _, pane := range tmux.ListPanes() {
		if seen[pane.Session] {
			continue
		}
		dir := tmux.PaneCurrentPath(pane.ID)
		if daemon.ToolName(reg.Tool(filepath.Base(dir))) != config.DefaultTool {
			continue
		}
		seen[pane.Session] = true
		if got := daemon.ModelFromDir(cfg.Claude.Home, dir); got != "" && got != model {
			out = append(out, pane.Session)
		}
	}
	return out
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
