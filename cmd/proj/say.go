package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/paseo"
	"github.com/tieo/proj/internal/projects"
)

var sayCmd = &cobra.Command{
	Use:   "say <project> [text]",
	Short: "send text to a project's session as a turn of its own",
	Long: `Send text into a project's running session, as though it had been typed there.

The session is the one place a conversation happens, so anything that wants to
tell the agent something sends it here rather than keeping a channel of its own:
a script, a webhook, a file watcher, a page someone is editing in a browser.

With no text argument the message is read from stdin, which is how a producer
pipes into it:

    echo "the price band should be a filter" | proj say Arbay
    proj say Arbay "restart the crawler and report"

The project must have a Paseo agent. Paseo holds a message sent to a busy agent
until its turn ends, unless --now, which interrupts the turn where it stands and
loses that work.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runSay,
}

var sayNow bool

func init() {
	sayCmd.Flags().BoolVar(&sayNow, "now", false, "interrupt whatever the session is doing, losing that turn's work")
	rootCmd.AddCommand(sayCmd)
}

// runSay delivers a message to the project's agent.
//
// Sessions live in Paseo, so an agent is what there is to talk to. It is found
// by its working directory, because that is the one thing both sides agree on:
// a project name is proj's, an agent id is Paseo's, and neither knows the
// other's.
func runSay(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p, err := projects.Resolve(cfg.BaseDir, args[0])
	if err != nil {
		return err
	}

	text := strings.Join(args[1:], " ")
	if len(args) == 1 {
		piped, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		text = string(piped)
	}
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("nothing to say: no text argument and nothing on stdin")
	}

	if !paseo.Available() {
		return fmt.Errorf("paseo is not installed, so there is no session to say this to")
	}
	agent := paseo.AgentFor(p.Dir)
	if agent == nil {
		return fmt.Errorf("%s has no Paseo agent; create one in %s first", p.Name, p.Dir)
	}
	if sayNow && agent.Running() {
		if err := paseo.Stop(agent.ID); err != nil {
			return err
		}
	}
	if err := paseo.Send(agent.ID, text); err != nil {
		return err
	}
	queued := ""
	if agent.Running() && !sayNow {
		queued = ", queued behind the turn it is working on"
	}
	fmt.Printf("said to %s (%d chars)%s\n", p.Name, len([]rune(text)), queued)
	return nil
}
