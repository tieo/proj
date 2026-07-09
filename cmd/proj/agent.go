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

var agentCmd = &cobra.Command{
	Use:   "agent <project> [agent]",
	Short: "show or set the coding agent a project's session launches",
	Long: `Show or set which coding agent a project's session runs. Built-in agents:
claude (the default), codex, agy. Additional agents can be defined in
config.toml under [agents.<name>] with a command and an optional
resume_command used when the project has prior history.

The setting applies on the next session launch; a running session keeps its
current agent until it exits (or is closed with ` + "`proj close`" + `).`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runAgent,
}

func init() {
	rootCmd.AddCommand(agentCmd)
}

func runAgent(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p, err := projects.Resolve(cfg.BaseDir, args[0])
	if err != nil {
		return err
	}
	if len(args) == 1 {
		fmt.Printf("%s: %s\n", p.Name, daemon.AgentName(p.Agent))
		return nil
	}
	name := strings.TrimSpace(args[1])
	if _, err := cfg.Agent(name); err != nil {
		return err
	}
	reg, err := projects.LoadRegistry()
	if err != nil {
		return err
	}
	if err := reg.SetAgent(p.Name, name); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", p.Name, daemon.AgentName(reg.Agent(p.Name)))
	if tmux.SessionForPath(p.Dir) != "" {
		fmt.Println("session is running; the new agent applies on the next launch")
	}
	return nil
}
