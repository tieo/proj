package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

var toolCmd = &cobra.Command{
	Use:   "tool <project> [tool]",
	Short: "show or set the coding tool a project's session launches",
	Long: `Show or set which coding tool a project's session runs. Built-in tools:
claude (the default), codex, agy. Additional tools can be defined in
config.toml under [tools.<name>] with a command and an optional
resume_command used when the project has prior history.

The setting applies on the next session launch; a running session keeps its
current tool until it exits (or is closed with ` + "`proj close`" + `).`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runTool,
}

func init() {
	rootCmd.AddCommand(toolCmd)
}

func runTool(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p, err := projects.Resolve(cfg.BaseDir, args[0])
	if err != nil {
		return err
	}
	if len(args) == 1 {
		fmt.Printf("%s: %s\n", p.Name, daemon.ToolName(p.Tool))
		return nil
	}
	name := strings.TrimSpace(args[1])
	if _, err := cfg.Tool(name); err != nil {
		return err
	}
	reg, err := projects.LoadRegistry()
	if err != nil {
		return err
	}
	if err := reg.SetTool(p.Name, name); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", p.Name, daemon.ToolName(reg.Tool(p.Name)))
	if tmux.SessionForPath(p.Dir) != "" {
		fmt.Println("session is running; the new tool applies on the next launch")
	}
	return nil
}
