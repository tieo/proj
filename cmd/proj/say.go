package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
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

The project must have a session running with a coding tool at its input box;
without one there is nothing to say it to, and the command says so rather than
dropping the message.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runSay,
}

func init() {
	rootCmd.AddCommand(sayCmd)
}

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

	pane := paneForSession(projects.SessionName(p.Name, p.Tags))
	if pane == "" {
		return fmt.Errorf("%s has no running session; start one with `proj %s` first", p.Name, p.Name)
	}
	if err := daemon.SendPrompt(daemon.DefaultConfig(), pane, text); err != nil {
		return err
	}
	fmt.Printf("said to %s (%d chars)\n", p.Name, len([]rune(text)))
	return nil
}

// paneForSession returns the pane holding a session's program, or "" when the
// session is not running. The message goes to the pane rather than the session
// name so it lands in the program's input box even when the session has more
// than one window.
func paneForSession(session string) string {
	for _, pane := range tmux.ListPanes() {
		if pane.Session == session {
			return pane.ID
		}
	}
	return ""
}
