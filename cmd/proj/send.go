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

var sendForce bool

var sendCmd = &cobra.Command{
	Use:   "send <session|project> <text...>",
	Short: "type a prompt into another session and submit it (delegate a task)",
	Long: `Type text into another session's input box and submit it, without attaching.

The target is a live session name (as shown in ` + "`proj list`" + `) or a project
name, which is resolved to its session. This is how the manager delegates work:
it sends a task into a session and lets goal-nudge watch it. By default it
refuses to type over an unsent draft in the target; pass --force to override.`,
	Args: cobra.MinimumNArgs(2),
	RunE: runSend,
}

func init() {
	sendCmd.Flags().BoolVar(&sendForce, "force", false, "send even if the target has an unsent draft")
	rootCmd.AddCommand(sendCmd)
}

func runSend(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	sess, err := resolveSendTarget(cfg, args[0])
	if err != nil {
		return err
	}
	text := strings.Join(args[1:], " ")
	if !sendForce && daemon.ComposerHasDraft(tmux.CapturePaneEsc(sess)) {
		return fmt.Errorf("%s has an unsent draft; not overwriting (use --force)", sess)
	}
	if err := daemon.SendPrompt(daemonConfig(), sess, text); err != nil {
		return fmt.Errorf("send to %s: %w", sess, err)
	}
	fmt.Printf("sent to %s\n", sess)
	return nil
}

// resolveSendTarget maps a target string to a live tmux session name: an exact
// live-session match wins (so session names from `proj list`, including the
// manager, work directly); otherwise it is treated as a project name and
// resolved to that project's session, which must be running.
func resolveSendTarget(cfg config.Config, target string) (string, error) {
	live := map[string]bool{}
	for _, s := range tmux.ListSessions() {
		if s.Name == target {
			return target, nil
		}
		live[s.Name] = true
	}
	p, err := projects.Resolve(cfg.BaseDir, target)
	if err != nil {
		return "", fmt.Errorf("no live session or project matching %q", target)
	}
	sess := projects.SessionName(p.Name, p.Tags)
	if !live[sess] {
		return "", fmt.Errorf("project %q has no running session (open it first with `proj %s`)", p.Name, p.Name)
	}
	return sess, nil
}
