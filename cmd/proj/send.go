package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
)

var (
	sendForce bool
	sendWait  time.Duration
)

// sendPoll is how often a --wait send re-checks the target. A turn runs for
// minutes, so checking more often only costs pane captures.
var sendPoll = 5 * time.Second

var sendCmd = &cobra.Command{
	Use:   "send <session|project> <text...>",
	Short: "type a prompt into another session and submit it (delegate a task)",
	Long: `Type text into another session's input box and submit it, without attaching.

The target is a live session name (as shown in ` + "`proj list`" + `) or a project
name, which is resolved to its session. This is how the manager delegates work:
it sends a task into a session and lets goal-nudge watch it.

The text is typed into the target's input box, so the target has to be ready
for it: a session that is mid-turn swallows what is typed, and one with an
unsent draft would have it overwritten. Both are refused. --wait holds until
the target goes idle instead of refusing, --force sends regardless.`,
	Args: cobra.MinimumNArgs(2),
	RunE: runSend,
}

func init() {
	sendCmd.Flags().BoolVar(&sendForce, "force", false, "send even if the target is busy or has an unsent draft")
	sendCmd.Flags().DurationVar(&sendWait, "wait", 0, "wait up to this long for a busy target to go idle (e.g. 10m)")
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
	if !sendForce {
		if err := awaitSendable(sess, sendWait, capturePaneBoth); err != nil {
			return err
		}
	}
	if err := daemon.SendPrompt(daemonConfig(), sess, text); err != nil {
		return fmt.Errorf("send to %s: %w", sess, err)
	}
	fmt.Printf("sent to %s\n", sess)
	return nil
}

func capturePaneBoth(sess string) (plain, esc string) {
	return tmux.CapturePane(sess, 0), tmux.CapturePaneEsc(sess)
}

// awaitSendable blocks until the target can actually receive a prompt, or
// returns why it cannot. A session mid-turn swallows typed text (it lands in
// no input box, or arrives truncated once one appears), and a session holding
// an unsent draft would have that draft overwritten, so neither is sent into.
// With wait == 0 the state is checked once and a busy target is refused;
// otherwise the target is polled until it goes idle or wait elapses.
//
// capture returns the pane both ways because the two checks need different
// forms: the draft check reads the escape sequences that tell a real draft from
// the dim ghost placeholder, while the input-box check anchors on a prompt at
// the start of a line, which only the plain capture has.
func awaitSendable(sess string, wait time.Duration, capture func(string) (plain, esc string)) error {
	deadline := time.Now().Add(wait)
	for {
		plain, esc := capture(sess)
		switch {
		case daemon.ComposerHasDraft(esc):
			// A draft is the user's, not a phase the target grows out of, so
			// waiting for it would be waiting for a person: refuse either way.
			return fmt.Errorf("%s has an unsent draft; not overwriting (use --force)", sess)
		case !daemon.SessionBusy(plain):
			return nil
		case time.Now().After(deadline):
			if wait == 0 {
				return fmt.Errorf("%s is mid-turn; typing now would be swallowed (use --wait to queue, or --force)", sess)
			}
			return fmt.Errorf("%s was still mid-turn after %s", sess, wait)
		}
		time.Sleep(sendPoll)
	}
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
